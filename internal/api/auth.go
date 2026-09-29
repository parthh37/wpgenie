package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/store"
)

// Panel sign-in. Browsers get a server-side session in an HttpOnly,
// SameSite=Strict cookie; scripts and the CLI keep using the API token
// (full access, as it is readable only by root on the server). Every
// change made through the API is recorded in the audit log.

const (
	sessionCookie = "wpgenie_session"
	// A session ends after sessionIdle without requests, and sessionMax
	// after sign-in regardless.
	sessionIdle = 12 * time.Hour
	sessionMax  = 7 * 24 * time.Hour
	// csrfHeader must accompany every state-changing request made with a
	// session cookie. Browsers only send custom headers cross-origin after a
	// CORS preflight, which this API never grants.
	csrfHeader = "X-Requested-With"
	csrfValue  = "wpgenie"

	settingRequire2FA = "auth_require_2fa"
	totpIssuer        = "WPGenie"
)

// Principal is who is making a request.
type Principal struct {
	UserID    int64
	Name      string
	Role      string
	SessionID string // "" for the API token
	TOTP      bool   // the user has two-factor authentication on
}

type ctxKey struct{}

func principalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

var (
	errUnauthorized = errors.New("unauthorized")
	errForbidden    = errors.New("forbidden: your role doesn't allow this")
	errCSRF         = errors.New("missing " + csrfHeader + " header")
	err2FARequired  = errors.New("two-factor authentication is required on this panel: enable it under Account first")
	errTooMany      = errors.New("too many failed sign-in attempts; try again later")
	errBadLogin     = errors.New("invalid username, password or code")
	errLastAdmin    = errors.New("that would leave the panel without an active administrator")
	errConflict     = errors.New("conflict")
)

// authenticate resolves the API token or a session cookie.
func (s *Server) authenticate(r *http.Request) (*Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		if subtle.ConstantTimeCompare([]byte(h), []byte("Bearer "+s.Token)) == 1 {
			return &Principal{Name: "api-token", Role: auth.RoleAdmin}, nil
		}
		return nil, errUnauthorized
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, errUnauthorized
	}
	ctx := r.Context()
	sess, err := s.Store.SessionByToken(ctx, auth.HashToken(c.Value))
	if err != nil {
		return nil, errUnauthorized
	}
	now := s.now()
	if now.After(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) > sessionIdle {
		s.Store.DeleteSession(ctx, sess.ID, 0)
		return nil, errUnauthorized
	}
	u, err := s.Store.GetUser(ctx, sess.UserID)
	if err != nil || u.Disabled {
		return nil, errUnauthorized
	}
	if now.Sub(sess.LastSeenAt) > time.Minute { // don't write on every request
		s.Store.TouchSession(ctx, sess.ID, now)
	}
	return &Principal{UserID: u.ID, Name: u.Username, Role: u.Role, SessionID: sess.ID, TOTP: u.TOTPEnabled}, nil
}

// route registers an authenticated handler that needs at least role. It
// also enforces the CSRF header on cookie requests, the panel's 2FA
// requirement, and audits every request that changes something.
func (s *Server) route(mux *http.ServeMux, pattern, role string, h handlerFunc) {
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
		// Refused changes are audited too: someone trying beyond their role
		// is what an audit log is for.
		deny := func(err error, extra ...string) {
			body := map[string]string{"error": err.Error()}
			if len(extra) == 2 {
				body[extra[0]] = extra[1]
			}
			writeJSON(w, http.StatusForbidden, body)
			if mutating {
				s.audit(r.Context(), store.AuditEntry{Actor: p.Name, IP: clientIP(r), Action: auditAction(r),
					Target: auditTarget(r), Status: http.StatusForbidden, Detail: err.Error()})
			}
		}
		if p.SessionID != "" && mutating && r.Header.Get(csrfHeader) != csrfValue {
			deny(errCSRF)
			return
		}
		accountRoute := strings.Contains(pattern, "/api/v1/account")
		if p.SessionID != "" && !p.TOTP && !accountRoute && s.require2FA(r.Context()) {
			deny(err2FARequired, "code", "totp_required")
			return
		}
		if auth.Level(p.Role) < auth.Level(role) {
			deny(errForbidden)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		err = h(rec, r)
		if err != nil {
			s.writeError(rec, r, err)
		}
		if mutating {
			entry := store.AuditEntry{Actor: p.Name, IP: clientIP(r), Action: auditAction(r), Target: auditTarget(r),
				Status: rec.status}
			if err != nil {
				entry.Detail = err.Error()
			}
			s.audit(r.Context(), entry)
		}
	}))
}

func (s *Server) audit(ctx context.Context, e store.AuditEntry) {
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	// Some of it comes from unauthenticated clients: bounded, so nobody
	// fills the disk through the audit log.
	e.Actor, e.Detail, e.Target = truncate(e.Actor, 64), truncate(e.Detail, 500), truncate(e.Target, 200)
	// Recorded even if the client went away mid-request.
	if err := s.Store.AddAudit(context.WithoutCancel(ctx), e); err != nil {
		s.Log.Error("writing the audit log", "err", err)
	}
}

// auditAction is the request as "METHOD /path?query", relative to the API.
func auditAction(r *http.Request) string {
	a := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/v1")
	if r.URL.RawQuery != "" {
		a += "?" + r.URL.RawQuery
	}
	return truncate(a, 300)
}

// auditTarget is the object a request acts on, for filtering.
func auditTarget(r *http.Request) string {
	for _, k := range []string{"id", "domain", "address"} {
		if v := r.PathValue(k); v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) require2FA(ctx context.Context) bool {
	v, _ := s.Store.Setting(ctx, settingRequire2FA)
	return v == "1"
}

// clientIP is the address the request came from. The API listens on
// loopback behind Caddy, which sets X-Forwarded-For to the client's
// address (resolved through Cloudflare when it is in front); anything
// else is a local connection.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}

// loginGuard limits failed sign-ins per client address and per account.
// The per-account limit stops a botnet guessing one password from many
// addresses; it can lock the account's owner out for a while too, which is
// why it is looser and the CLI can always get in with the API token.
type loginGuard struct {
	mu        sync.Mutex
	failures  map[string]*failures
	lastPrune time.Time
}

type failures struct {
	n        int
	first    time.Time
	reported bool // the lockout was audited (once per window, not per request)
}

const (
	guardWindow  = 15 * time.Minute
	guardPerIP   = 20
	guardPerUser = 10
)

type limitKey struct {
	key   string
	limit int
}

// ipKey counts IPv6 clients per /64, like the shield's bans: one
// subscriber can rotate through a whole /64.
func ipKey(ip string) limitKey {
	k, ok := shield.BanKey(ip)
	if !ok {
		k = ip
	}
	return limitKey{"ip:" + k, guardPerIP}
}

func userKey(username string) limitKey {
	return limitKey{"user:" + strings.ToLower(username), guardPerUser}
}

// attempt counts an attempt against every key before the (slow) password
// check, so a burst of parallel requests can't all get past the limit. A
// successful sign-in forgives them; an attempt that turns out not to be a
// failure (the second factor is asked for) is given back with undo. Over a
// limit, it returns how long to wait, and whether this is the first refusal
// of the window.
func (g *loginGuard) attempt(now time.Time, keys ...limitKey) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failures == nil {
		g.failures = map[string]*failures{}
	}
	if now.Sub(g.lastPrune) > time.Minute { // bounded memory: forget old windows
		g.lastPrune = now
		for k, f := range g.failures {
			if now.Sub(f.first) > guardWindow {
				delete(g.failures, k)
			}
		}
	}
	var wait time.Duration
	first := false
	for _, k := range keys {
		f := g.failures[k.key]
		if f != nil && now.Sub(f.first) > guardWindow {
			delete(g.failures, k.key)
			f = nil
		}
		if f != nil && f.n >= k.limit {
			wait = max(wait, guardWindow-now.Sub(f.first))
			first = first || !f.reported
			f.reported = true
		}
	}
	if wait > 0 {
		return wait, first
	}
	for _, k := range keys {
		f := g.failures[k.key]
		if f == nil {
			f = &failures{first: now}
			g.failures[k.key] = f
		}
		f.n++
	}
	return 0, false
}

func (g *loginGuard) undo(keys ...limitKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		if f := g.failures[k.key]; f != nil {
			if f.n--; f.n <= 0 {
				delete(g.failures, k.key)
			}
		}
	}
}

func (g *loginGuard) forgive(keys ...limitKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		delete(g.failures, k.key)
	}
}

// passwordSlots bounds concurrent password checks (each is ~100 ms of CPU):
// a flood of sign-ins from many addresses queues instead of starving the
// server.
var passwordSlots = make(chan struct{}, max(2, runtime.NumCPU()/2))

func checkPassword(hash, pw string) bool {
	passwordSlots <- struct{}{}
	defer func() { <-passwordSlots }()
	if hash == "" {
		auth.CheckNoUser(pw)
		return false
	}
	return auth.CheckPassword(hash, pw)
}

// publicAuth wraps the endpoints that work without a session (state,
// setup, login, logout): uniform errors and the CSRF header on POSTs, so
// another site can't sign a visitor in or out behind their back.
func (s *Server) publicAuth(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != csrfValue {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": errCSRF.Error()})
			return
		}
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	})
}

// authState tells the dashboard what to show: first-run setup, sign-in,
// or who is signed in.
func (s *Server) authState(w http.ResponseWriter, r *http.Request) error {
	n, err := s.Store.CountUsers(r.Context())
	if err != nil {
		return err
	}
	out := map[string]any{"setup": n == 0, "user": nil, "require_2fa": s.require2FA(r.Context())}
	if p, err := s.authenticate(r); err == nil && p.SessionID != "" {
		if u, err := s.Store.GetUser(r.Context(), p.UserID); err == nil {
			out["user"] = u
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@+-]{1,63}$`)

func validUsername(u string) error {
	if !usernameRe.MatchString(u) {
		return fmt.Errorf("%w: username must be 2-64 letters, digits or . _ @ + -", errBadRequest)
	}
	if _, err := strconv.ParseInt(u, 10, 64); err == nil {
		return fmt.Errorf("%w: username can't be only digits", errBadRequest)
	}
	return nil
}

// setup creates the first administrator. Whoever can read the API token
// (root on the server) owns the panel, so the token proves it; after the
// first account exists, this endpoint is closed.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	// Closed once an account exists, before anything else: nobody can keep
	// the throttle below busy for a panel that is already set up.
	if n, err := s.Store.CountUsers(r.Context()); err != nil {
		return err
	} else if n > 0 {
		return fmt.Errorf("%w: the panel already has accounts; sign in", errConflict)
	}
	ip := clientIP(r)
	// Per address only: a shared key would let anyone block the owner's setup.
	key := ipKey(ip)
	key.key = "setup-" + key.key
	if d, _ := s.guard.attempt(s.now(), key); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		return errTooMany
	}
	if subtle.ConstantTimeCompare([]byte(in.Token), []byte(s.Token)) != 1 {
		s.audit(r.Context(), store.AuditEntry{Actor: truncate(in.Username, 64), IP: ip, Action: "setup_failed",
			Detail: "wrong API token", Status: http.StatusUnauthorized})
		return fmt.Errorf("%w: wrong API token", errUnauthorized)
	}
	s.guard.forgive(key)
	if err := validUsername(in.Username); err != nil {
		return err
	}
	if err := auth.ValidatePassword(in.Password); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return err
	}
	u, err := s.Store.CreateFirstUser(r.Context(), in.Username, hash, auth.RoleAdmin)
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("%w: the panel already has accounts; sign in", errConflict)
	}
	if err != nil {
		return err
	}
	s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: ip, Action: "setup", Target: u.Username, Status: http.StatusOK})
	return s.startSession(w, r, u)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if len(in.Username) > 64 || len(in.Password) > auth.MaxPasswordLen || len(in.Code) > 64 {
		return errBadLogin // no account has such a name: not worth a hash or an audit row
	}
	ctx, ip, now := r.Context(), clientIP(r), s.now()
	keys := []limitKey{ipKey(ip), userKey(in.Username)}
	if d, first := s.guard.attempt(now, keys...); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		if first {
			s.audit(ctx, store.AuditEntry{Actor: in.Username, IP: ip, Action: "login_throttled", Status: http.StatusTooManyRequests})
		}
		return errTooMany
	}
	fail := func(reason string) error {
		s.audit(ctx, store.AuditEntry{Actor: in.Username, IP: ip, Action: "login_failed", Detail: reason,
			Status: http.StatusUnauthorized})
		return errBadLogin
	}
	u, err := s.Store.UserByName(ctx, in.Username)
	if errors.Is(err, store.ErrNotFound) {
		checkPassword("", in.Password) // same timing as a wrong password
		return fail("unknown user")
	}
	if err != nil {
		return err
	}
	if !checkPassword(u.PasswordHash, in.Password) {
		return fail("wrong password")
	}
	if u.Disabled {
		return fail("account disabled")
	}
	if u.TOTPEnabled {
		code := strings.TrimSpace(in.Code)
		if code == "" {
			// The password was right: ask for the second factor. Not a failure.
			s.guard.undo(keys...)
			return writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "enter the code from your authenticator app", "need_code": true})
		}
		if step, err := auth.CheckTOTP(u.TOTPSecret, code, now, u.TOTPLastStep); err == nil {
			if ok, err := s.Store.UseTOTPStep(ctx, u.ID, step); err != nil {
				return err
			} else if !ok {
				return fail("two-factor code already used")
			}
		} else if ok, err := s.Store.UseRecoveryCode(ctx, u.ID, auth.HashRecoveryCode(code)); err != nil {
			return err
		} else if ok {
			s.audit(ctx, store.AuditEntry{Actor: u.Username, IP: ip, Action: "recovery_code_used", Target: u.Username,
				Detail: fmt.Sprintf("%d left", u.RecoveryLeft-1), Status: http.StatusOK})
		} else {
			return fail("wrong two-factor code")
		}
	}
	s.guard.forgive(keys...)
	s.Store.PruneSessions(ctx, now, sessionIdle)
	s.Store.TouchLogin(ctx, u.ID, now)
	s.audit(ctx, store.AuditEntry{Actor: u.Username, IP: ip, Action: "login", Target: u.Username, Status: http.StatusOK})
	return s.startSession(w, r, u)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token := auth.RandomToken(32)
	now := s.now()
	sess := &store.Session{ID: auth.RandomToken(9), UserID: u.ID, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(sessionMax), IP: clientIP(r), UserAgent: truncate(r.UserAgent(), 200)}
	if err := s.Store.CreateSession(r.Context(), sess, auth.HashToken(token)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionMax.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		// Behind Caddy with TLS; a local SSH tunnel is plain HTTP on loopback.
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	u, err := s.Store.GetUser(r.Context(), u.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	if p, err := s.authenticate(r); err == nil && p.SessionID != "" {
		s.Store.DeleteSession(r.Context(), p.SessionID, 0)
		s.audit(r.Context(), store.AuditEntry{Actor: p.Name, IP: clientIP(r), Action: "logout", Status: http.StatusOK})
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
	return nil
}
