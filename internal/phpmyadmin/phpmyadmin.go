// Package phpmyadmin opens phpMyAdmin on a site's database, on demand.
//
// Opening it from the panel creates a temporary MariaDB account with
// rights on that one database and a one-time token (valid for two
// minutes), and returns a link on the site's own domain:
// https://<site>/_wpgenie/phpmyadmin/?wpgenie_token=…. Caddy routes /_wpgenie/*
// to the daemon; the token is exchanged for a session cookie scoped to that
// path, and requests are proxied to the phpMyAdmin container with the
// session's account in headers. Sessions end after 15 minutes idle or an
// hour, when the account is dropped (open connections killed); the
// container stops once no session is left.
//
// Why the site's own domain and not the panel's: phpMyAdmin renders whatever
// the database holds, and a compromised plugin controls that. If an XSS in
// phpMyAdmin ran on the panel's origin, it could act with the operator's
// panel session. On the site's origin it reaches nothing the site's own
// code couldn't already.
package phpmyadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

var ErrInvalid = errors.New("invalid input")

// Docker runs docker (runtime.Docker).
type Docker interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	EnsureBuilt(ctx context.Context, tag, dir string) (string, error)
}

// Accounts creates and drops temporary database accounts (dbprov.MariaDB).
type Accounts interface {
	CreateTempUser(ctx context.Context, db, user, password string) error
	DropTempUser(ctx context.Context, user string) error
	DropTempUsers(ctx context.Context) error
}

type Config struct {
	Image    string
	ImageDir string // images/phpmyadmin
	Port     int    // phpMyAdmin on 127.0.0.1
	Network  string // where MariaDB is
	DBHost   string // MariaDB's name on that network
	// Upstream is where the daemon reaches phpMyAdmin (default
	// 127.0.0.1:<Port>; tests use the container's network address).
	Upstream string
}

// SiteHeader names the site of a request (set by Caddy, see proxy).
const SiteHeader = "X-WPGenie-Site"

const (
	container = "wpgenie-phpmyadmin"
	// Adminer's container, which this replaced: removed if an upgrade
	// left it running.
	legacyContainer = "wpgenie-adminer"
	Path            = "/_wpgenie/phpmyadmin/"
	cookieName      = "wpgenie_pma"
	tokenParam      = "wpgenie_token"
	tokenTTL        = 2 * time.Minute
	sessionIdle     = 15 * time.Minute
	sessionMax      = time.Hour
	idleStop        = 5 * time.Minute
)

type session struct {
	siteID, db       string
	user, pass       string
	created, touched time.Time
	redeemed         bool // the token was exchanged: now keyed by cookie
	actor            string
}

type Service struct {
	Cfg      Config
	Docker   Docker
	Accounts Accounts
	Store    *store.Store
	Log      *slog.Logger
	Now      func() time.Time // for tests

	startMu  sync.Mutex // one ensureRunning at a time (container name, secret)
	mu       sync.Mutex
	pending  map[string]*session // sha256(token) -> not yet redeemed
	sessions map[string]*session // sha256(cookie) -> session
	secret   string              // proves the daemon to the container
	lastUsed time.Time
	proxy    *httputil.ReverseProxy
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// Open starts a session on a site's database and returns the link to it.
func (s *Service) Open(ctx context.Context, siteID, actor string) (string, time.Time, error) {
	st, err := s.Store.GetSite(ctx, siteID)
	if err != nil {
		return "", time.Time{}, err
	}
	if st.Status != store.StatusActive {
		return "", time.Time{}, fmt.Errorf("%w: site is %s", ErrInvalid, st.Status)
	}
	if err := s.ensureRunning(ctx); err != nil {
		return "", time.Time{}, err
	}
	sess := &session{siteID: siteID, db: st.DBName, user: "wpga_" + randHex(6), pass: randHex(16),
		created: s.now(), touched: s.now(), actor: actor}
	if err := s.Accounts.CreateTempUser(ctx, st.DBName, sess.user, sess.pass); err != nil {
		return "", time.Time{}, fmt.Errorf("creating a database account: %w", err)
	}
	token := randHex(32)
	s.mu.Lock()
	if s.pending == nil {
		s.pending, s.sessions = map[string]*session{}, map[string]*session{}
	}
	s.pending[hash(token)] = sess
	s.lastUsed = s.now()
	s.mu.Unlock()
	s.Log.Info("phpmyadmin session opened", "site", siteID, "actor", actor, "db_user", sess.user)
	u := url.URL{Scheme: "https", Host: st.PrimaryDomain, Path: Path, RawQuery: tokenParam + "=" + token}
	return u.String(), sess.created.Add(tokenTTL), nil
}

// ServeHTTP handles /_wpgenie/phpmyadmin/ on sites' domains (via Caddy).
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	siteID := r.Header.Get(SiteHeader)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store")
	if siteID == "" || !strings.HasPrefix(r.URL.Path, Path) {
		http.NotFound(w, r)
		return
	}
	if tok := r.URL.Query().Get(tokenParam); tok != "" {
		s.redeem(w, r, siteID, tok)
		return
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		denied(w, "Open phpMyAdmin from the WPGenie panel (Site → SFTP & database).")
		return
	}
	s.mu.Lock()
	sess := s.sessions[hash(c.Value)]
	now := s.now()
	if sess != nil && (sess.siteID != siteID || now.Sub(sess.touched) > sessionIdle || now.Sub(sess.created) > sessionMax) {
		if sess.siteID == siteID {
			delete(s.sessions, hash(c.Value))
			go s.drop(sess)
		}
		sess = nil
	}
	if sess != nil {
		sess.touched, s.lastUsed = now, now
	}
	proxy := s.proxy
	s.mu.Unlock()
	if sess == nil || proxy == nil {
		denied(w, "This database session has ended. Open phpMyAdmin again from the WPGenie panel.")
		return
	}
	r = r.Clone(r.Context())
	for k := range r.Header {
		// PHP turns "_" into "-" too (HTTP_X_WPGENIE_…): X_WPGenie-DB-Host
		// must not reach phpMyAdmin as the database host.
		if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(k, "_", "-")), "x-wpgenie-") {
			delete(r.Header, k)
		}
	}
	r.Header.Set("Cookie", withoutCookie(r.Header.Get("Cookie"), cookieName))
	r.Header.Set("X-WPGenie-Secret", s.secret)
	r.Header.Set("X-WPGenie-DB-Host", s.Cfg.DBHost)
	r.Header.Set("X-WPGenie-DB-User", sess.user)
	r.Header.Set("X-WPGenie-DB-Pass", sess.pass)
	r.Header.Set("X-WPGenie-DB-Name", sess.db)
	proxy.ServeHTTP(w, r)
}

// redeem exchanges a one-time token for a session cookie and redirects to
// a URL without the token (so it isn't left in history or Referer).
func (s *Service) redeem(w http.ResponseWriter, r *http.Request, siteID, tok string) {
	s.mu.Lock()
	key := hash(tok)
	sess := s.pending[key]
	if sess != nil {
		delete(s.pending, key) // single use, whatever happens next
	}
	var cookie, db string
	ok := sess != nil && sess.siteID == siteID && s.now().Sub(sess.created) <= tokenTTL
	if ok {
		cookie = randHex(32)
		sess.redeemed, sess.touched, db = true, s.now(), sess.db
		s.sessions[hash(cookie)] = sess
	}
	s.mu.Unlock()
	if !ok {
		if sess != nil {
			go s.drop(sess)
		}
		denied(w, "This link has expired or was already used. Open phpMyAdmin again from the WPGenie panel.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: cookie, Path: Path, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionMax / time.Second)})
	// Straight to the site's tables (phpMyAdmin's home page is the server's).
	http.Redirect(w, r, Path+"index.php?route=/database/structure&db="+url.QueryEscape(db), http.StatusSeeOther)
}

func denied(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Database session</title>`+
		`<p style="font:16px system-ui;margin:3rem auto;max-width:36rem">%s</p>`, html.EscapeString(msg))
}

// withoutCookie removes one cookie from a Cookie header: phpMyAdmin never
// sees WPGenie's session cookie.
func withoutCookie(header, name string) string {
	var keep []string
	for _, part := range strings.Split(header, ";") {
		if n, _, _ := strings.Cut(strings.TrimSpace(part), "="); n != name && strings.TrimSpace(part) != "" {
			keep = append(keep, strings.TrimSpace(part))
		}
	}
	return strings.Join(keep, "; ")
}

func (s *Service) drop(sess *session) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Accounts.DropTempUser(ctx, sess.user); err != nil {
		s.Log.Warn("phpmyadmin: dropping a temporary database account", "user", sess.user, "err", err)
	}
}

// SiteRemoved ends a deleted site's sessions.
func (s *Service) SiteRemoved(_ context.Context, siteID string) {
	s.mu.Lock()
	var gone []*session
	for _, m := range []map[string]*session{s.pending, s.sessions} {
		for k, sess := range m {
			if sess.siteID == siteID {
				delete(m, k)
				gone = append(gone, sess)
			}
		}
	}
	s.mu.Unlock()
	for _, sess := range gone {
		s.drop(sess)
	}
}

// Sessions counts open sessions per site (for the panel).
func (s *Service) Sessions(siteID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sess := range s.sessions {
		if sess.siteID == siteID {
			n++
		}
	}
	return n
}

// CloseSite ends every session on a site's database now.
func (s *Service) CloseSite(ctx context.Context, siteID string) { s.SiteRemoved(ctx, siteID) }

// Run expires sessions and stops phpMyAdmin when nobody uses it. At start it
// cleans up after a previous run (sessions live in memory only).
func (s *Service) Run(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, time.Minute)
	s.Docker.Run(c, nil, "rm", "-f", container, legacyContainer)
	if err := s.Accounts.DropTempUsers(c); err != nil {
		s.Log.Warn("phpmyadmin: dropping leftover temporary database accounts", "err", err)
	}
	cancel()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sweep(ctx)
	}
}

func (s *Service) sweep(ctx context.Context) {
	now := s.now()
	// Not while a start is in progress (it would stop what it starts).
	if !s.startMu.TryLock() {
		return
	}
	defer s.startMu.Unlock()
	s.mu.Lock()
	var gone []*session
	for k, sess := range s.pending {
		if now.Sub(sess.created) > tokenTTL {
			delete(s.pending, k)
			gone = append(gone, sess)
		}
	}
	for k, sess := range s.sessions {
		if now.Sub(sess.touched) > sessionIdle || now.Sub(sess.created) > sessionMax {
			delete(s.sessions, k)
			gone = append(gone, sess)
		}
	}
	stop := s.proxy != nil && len(s.pending) == 0 && len(s.sessions) == 0 && now.Sub(s.lastUsed) > idleStop
	if stop {
		s.proxy, s.secret = nil, ""
	}
	s.mu.Unlock()
	for _, sess := range gone {
		s.drop(sess)
	}
	if stop {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		s.Docker.Run(c, nil, "rm", "-f", container)
		s.Log.Info("phpmyadmin stopped (no sessions)")
	}
}

// ensureRunning starts the phpMyAdmin container (and gives it a new secret)
// unless it runs.
func (s *Service) ensureRunning(ctx context.Context) error {
	// Two Opens at once must not both start the container: the secret one
	// of them wrote would not be the one the proxy sends.
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	running := s.proxy != nil
	// Counts as use: the sweeper must not stop phpMyAdmin between here and
	// the session being recorded.
	s.lastUsed = s.now()
	s.mu.Unlock()
	if running {
		if out, err := s.Docker.Run(ctx, nil, "inspect", "-f", "{{.State.Running}}", container); err == nil &&
			strings.TrimSpace(string(out)) == "true" {
			return nil
		}
	}
	if _, err := s.Docker.EnsureBuilt(ctx, s.Cfg.Image, s.Cfg.ImageDir); err != nil {
		return fmt.Errorf("building the phpMyAdmin image: %w", err)
	}
	s.Docker.Run(ctx, nil, "rm", "-f", container)
	if _, err := s.Docker.Run(ctx, nil, "run", "-d", "--name", container, "--label", "wpgenie.phpmyadmin=1",
		"--network", s.Cfg.Network, "-p", "127.0.0.1:"+strconv.Itoa(s.Cfg.Port)+":8080",
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=128m",
		"--tmpfs", "/run/wpg:rw,noexec,nosuid,size=64k,uid=82,gid=82,mode=0700",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "512m", "--pids-limit", "64",
		s.Cfg.Image); err != nil {
		return fmt.Errorf("starting phpMyAdmin: %w", err)
	}
	secret := randHex(32)
	if _, err := s.Docker.Run(ctx, strings.NewReader(secret), "exec", "-i", container, "sh", "-c",
		"umask 077; cat > /run/wpg/secret"); err != nil {
		return fmt.Errorf("starting phpMyAdmin: %w", err)
	}
	upstream := s.Cfg.Upstream
	if upstream == "" {
		upstream = "127.0.0.1:" + strconv.Itoa(s.Cfg.Port)
	}
	target := &url.URL{Scheme: "http", Host: upstream}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.Log.Warn("phpmyadmin proxy", "err", err)
		denied(w, "phpMyAdmin is not answering. Open it again from the WPGenie panel.")
	}
	// Wait until the PHP server listens.
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", target.Host, time.Second)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			return errors.New("phpMyAdmin did not start")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	s.mu.Lock()
	s.proxy, s.secret, s.lastUsed = proxy, secret, s.now()
	s.mu.Unlock()
	return nil
}
