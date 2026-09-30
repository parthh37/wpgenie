// Package wplogin signs panel users in to a site's wp-admin without its
// WordPress password.
//
// Asking for it from the panel makes a real WordPress session for one of
// the site's administrators (site.Service.AdminLogin: WP-CLI with plugins
// skipped, the cookies wp_set_auth_cookie() would set) and returns a
// one-time link on the site's own domain: https://<site>/_wpgenie/login?
// wpgenie_token=…, valid two minutes. Caddy routes /_wpgenie/* to the
// daemon, naming the site; the token is exchanged for WordPress's cookies
// and the browser lands in wp-admin. The cookies only ever exist in this
// process's memory until then, and the session is an ordinary WordPress
// session: it shows in the user's sessions, expires like one, and ends
// with "log out everywhere" or a password change.
//
// Why a link rather than setting cookies from the panel: they belong to
// the site's domain, which only a response from that domain can set.
package wplogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/phpmyadmin"
	"github.com/parthh37/wpgenie/internal/site"
)

const (
	// Path is where sign-in links point on sites' domains.
	Path       = "/_wpgenie/login"
	tokenParam = "wpgenie_token"
	TokenTTL   = 2 * time.Minute
	// maxPending bounds links nobody used yet.
	maxPending = 10000
)

var ErrTooMany = errors.New("too many unused sign-in links; try again in a few minutes")

// Minter makes WordPress sessions (site.Service).
type Minter interface {
	AdminLogin(ctx context.Context, id string, userID int, ip, ua string) (*site.AdminSession, error)
}

type pending struct {
	siteID  string
	sess    *site.AdminSession
	created time.Time
	actor   string
}

type Service struct {
	Sites Minter
	Log   *slog.Logger
	Now   func() time.Time // for tests

	mu      sync.Mutex
	pending map[string]*pending // sha256(token) -> not yet used
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// Link is a one-time sign-in link.
type Link struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	User      string    `json:"user"`
	UserID    int       `json:"user_id"`
}

// Open makes a session for an administrator of a site (userID 0: the
// oldest) and returns the link that signs the browser in with it. ip and
// ua are the requester's, recorded with the WordPress session.
func (s *Service) Open(ctx context.Context, siteID string, userID int, actor, ip, ua string) (*Link, error) {
	s.mu.Lock()
	s.expireLocked()
	full := len(s.pending) >= maxPending
	s.mu.Unlock()
	if full {
		return nil, ErrTooMany
	}
	sess, err := s.Sites.AdminLogin(ctx, siteID, userID, ip, ua)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	now := s.now()
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]*pending{}
	}
	s.pending[hash(token)] = &pending{siteID: siteID, sess: sess, created: now, actor: actor}
	s.mu.Unlock()
	s.Log.Info("wp-admin sign-in link", "site", siteID, "actor", actor, "wp_user", sess.User)
	u := url.URL{Scheme: "https", Host: sess.Host, Path: Path, RawQuery: tokenParam + "=" + token}
	return &Link{URL: u.String(), ExpiresAt: now.Add(TokenTTL), User: sess.User, UserID: sess.UserID}, nil
}

func (s *Service) expireLocked() {
	now := s.now()
	for k, p := range s.pending {
		if now.Sub(p.created) > TokenTTL {
			delete(s.pending, k)
		}
	}
}

// ServeHTTP handles Path on sites' domains (via Caddy).
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Cache-Control", "no-store")
	siteID := r.Header.Get(phpmyadmin.SiteHeader)
	tok := r.URL.Query().Get(tokenParam)
	if siteID == "" || r.URL.Path != Path {
		http.NotFound(w, r)
		return
	}
	if tok == "" {
		denied(w, "Sign in to wp-admin from the WPGenie panel.")
		return
	}
	s.mu.Lock()
	key := hash(tok)
	p := s.pending[key]
	if p != nil && p.siteID == siteID {
		delete(s.pending, key) // single use, whatever happens next
	}
	s.mu.Unlock()
	if p == nil || p.siteID != siteID || s.now().Sub(p.created) > TokenTTL {
		denied(w, "This sign-in link has expired or was already used. Open wp-admin again from the WPGenie panel.")
		return
	}
	host := strings.ToLower(r.Host)
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	if host != p.sess.Host {
		denied(w, "This sign-in link belongs to another address of the site. Open wp-admin again from the WPGenie panel.")
		return
	}
	domain := cookieDomain(p.sess.Domain, host)
	for _, c := range p.sess.Cookies {
		// Session cookies, as WordPress sets them without "remember me";
		// the WordPress session behind them expires on its own.
		http.SetCookie(w, &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: domain,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	s.Log.Info("wp-admin sign-in", "site", siteID, "actor", p.actor, "wp_user", p.sess.User)
	http.Redirect(w, r, p.sess.AdminPath, http.StatusSeeOther)
}

// cookieDomain is the site's COOKIE_DOMAIN if it covers host (a site may
// share its cookies with subdomains), else "" (host-only, WordPress's
// default).
func cookieDomain(d, host string) string {
	d = strings.ToLower(strings.TrimPrefix(d, "."))
	if d == "" || !strings.Contains(d, ".") || (host != d && !strings.HasSuffix(host, "."+d)) {
		return ""
	}
	return d
}

func denied(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Sign in</title>`+
		`<p style="font:16px system-ui;margin:3rem auto;max-width:36rem">%s</p>`, html.EscapeString(msg))
}

// SiteRemoved forgets a deleted or suspended site's unused links.
func (s *Service) SiteRemoved(_ context.Context, siteID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.pending {
		if p.siteID == siteID {
			delete(s.pending, k)
		}
	}
}
