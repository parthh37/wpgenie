package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
	"github.com/parthh37/wpgenie/internal/updater"
)

type authEnv struct {
	t     *testing.T
	srv   *httptest.Server
	api   *Server
	store *store.Store
	now   time.Time
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	st := storetest.Open(t)
	svc := &site.Service{Cfg: config.Default(), Store: st, Log: slog.New(slog.DiscardHandler)}
	ml := &mail.Service{Cfg: mail.Config{DataDir: t.TempDir()}, Store: st, Log: slog.New(slog.DiscardHandler)}
	if err := ml.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &authEnv{t: t, store: st, now: time.Unix(1_800_000_000, 0)}
	e.api = &Server{Token: "tok", Version: "v0", Sites: svc, Store: st, Mail: ml, Log: slog.New(slog.DiscardHandler),
		Shield:  shield.New(shield.Options{Secret: []byte("k"), Sites: svc.ShieldLookup}),
		Updater: &updater.Updater{Current: "v0", Repo: "o/r", StateDir: t.TempDir(), APIBase: "http://127.0.0.1:1"},
		Now:     func() time.Time { return e.now }}
	e.srv = httptest.NewServer(e.api.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

// browser is a cookie-carrying client, like the dashboard.
type browser struct {
	e      *authEnv
	client *http.Client
}

func (e *authEnv) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, client: &http.Client{Jar: jar}}
}

// do sends a request (with the CSRF header unless csrf is false) and
// decodes a JSON reply into out if given.
func (b *browser) do(method, path, body string, csrf bool, out any) int {
	b.e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, b.e.srv.URL+path, rd)
	if csrf {
		req.Header.Set(csrfHeader, csrfValue)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (e *authEnv) token(method, path, body string) int {
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

const pw = "a long enough password"

func TestFirstRunSetupNeedsTheToken(t *testing.T) {
	e := newAuthEnv(t)
	b := e.browser()
	var state struct {
		Setup bool            `json:"setup"`
		User  json.RawMessage `json:"user"`
	}
	b.do("GET", "/api/v1/auth/state", "", false, &state)
	if !state.Setup || string(state.User) != "null" {
		t.Fatalf("state %+v", state)
	}
	if c := b.do("GET", "/api/v1/sites", "", false, nil); c != 401 {
		t.Fatalf("no session: %d", c)
	}
	if c := b.do("POST", "/api/v1/auth/setup", `{"token":"wrong","username":"ann","password":"`+pw+`"}`, true, nil); c != 401 {
		t.Fatalf("wrong token: %d", c)
	}
	if c := b.do("POST", "/api/v1/auth/setup", `{"token":"tok","username":"ann","password":"short"}`, true, nil); c != 400 {
		t.Fatalf("weak password: %d", c)
	}
	if c := b.do("POST", "/api/v1/auth/setup", `{"token":"tok","username":"ann","password":"`+pw+`"}`, false, nil); c != 403 {
		t.Fatalf("setup without the CSRF header: %d", c)
	}
	if c := b.do("POST", "/api/v1/auth/setup", `{"token":"tok","username":"ann","password":"`+pw+`"}`, true, nil); c != 200 {
		t.Fatalf("setup: %d", c)
	}
	// Signed in straight away, as an administrator.
	if c := b.do("GET", "/api/v1/users", "", false, nil); c != 200 {
		t.Fatalf("after setup: %d", c)
	}
	if c := e.browser().do("POST", "/api/v1/auth/setup", `{"token":"tok","username":"eve","password":"`+pw+`"}`, true, nil); c != 409 {
		t.Fatalf("second setup: %d", c)
	}
}

func setupAdmin(t *testing.T, e *authEnv) *browser {
	t.Helper()
	b := e.browser()
	if c := b.do("POST", "/api/v1/auth/setup", `{"token":"tok","username":"ann","password":"`+pw+`"}`, true, nil); c != 200 {
		t.Fatalf("setup: %d", c)
	}
	return b
}

func TestRolesAndCSRF(t *testing.T) {
	e := newAuthEnv(t)
	admin := setupAdmin(t, e)
	var created struct {
		Password string `json:"password"`
	}
	if c := admin.do("POST", "/api/v1/users", `{"username":"vic","role":"viewer"}`, true, &created); c != 201 || created.Password == "" {
		t.Fatalf("create viewer: %d %+v", c, created)
	}
	admin.do("POST", "/api/v1/users", `{"username":"oli","role":"operator","password":"`+pw+`"}`, true, nil)

	viewer := e.browser()
	if c := viewer.do("POST", "/api/v1/auth/login", `{"username":"vic","password":"`+created.Password+`"}`, true, nil); c != 200 {
		t.Fatalf("viewer login: %d", c)
	}
	op := e.browser()
	op.do("POST", "/api/v1/auth/login", `{"username":"OLI","password":"`+pw+`"}`, true, nil) // names are case-insensitive

	e.store.CreateSite(context.Background(), &store.Site{ID: "slive", Name: "live", PrimaryDomain: "live.test",
		PHPVersion: "8.3", FPMPort: 19000, DBName: "wp_slive", Status: store.StatusActive, MemoryMB: 512, CPUs: 1, Replicas: 1})
	for _, c := range []struct {
		who          *browser
		method, path string
		body         string
		want         int
	}{
		{viewer, "GET", "/api/v1/sites", "", 200},
		// Operators delete staging sites only; live sites need an admin.
		{op, "DELETE", "/api/v1/sites/slive", "", 403},
		{viewer, "DELETE", "/api/v1/sites/slive", "", 403},
		{op, "POST", "/api/v1/backups/repos", `{"name":"x","kind":"local","path":"/srv/b"}`, 403},
		{op, "POST", "/api/v1/backups/repos/local/password", "", 403},
		{viewer, "POST", "/api/v1/sites/slive/backups", "", 403},
		{viewer, "POST", "/api/v1/sites/slive/phpmyadmin", "", 403},
		{viewer, "GET", "/api/v1/sites/slive/backups/local/0123abcd/download", "", 403},
		{viewer, "PUT", "/api/v1/sites/x/cache", `{"page_cache":true,"object_cache":true}`, 403},
		{op, "PUT", "/api/v1/sites/x/cache", `{"page_cache":true,"object_cache":true}`, 404}, // allowed; no such site
		{op, "POST", "/api/v1/sites", `{"domain":"a.com","admin_email":"a@b.co"}`, 403},
		{op, "GET", "/api/v1/users", "", 403},
		{op, "GET", "/api/v1/audit", "", 403},
		{op, "PUT", "/api/v1/security/settings", `{"allow":[],"deny":[]}`, 403},
		{admin, "PUT", "/api/v1/security/settings", `{"allow":[],"deny":["198.51.100.0/24"]}`, 200},
	} {
		if got := c.who.do(c.method, c.path, c.body, true, nil); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	// A cookie alone can't change anything: another site could send it.
	if c := op.do("PUT", "/api/v1/sites/x/cache", `{"page_cache":true,"object_cache":true}`, false, nil); c != 403 {
		t.Errorf("cookie request without the CSRF header: %d", c)
	}
	// The API token needs no header and has full access.
	if c := e.token("GET", "/api/v1/users", ""); c != 200 {
		t.Errorf("token: %d", c)
	}

	var audit []store.AuditEntry
	admin.do("GET", "/api/v1/audit?limit=100", "", false, &audit)
	var sawDenied, sawSettings bool
	for _, a := range audit {
		sawDenied = sawDenied || a.Actor == "vic" && a.Action == "PUT /sites/x/cache" && a.Status == 403
		sawSettings = sawSettings || a.Actor == "ann" && a.Action == "PUT /security/settings" && a.Status == 200
	}
	if !sawDenied || !sawSettings {
		t.Errorf("audit log misses entries: %+v", audit)
	}
}

func TestLoginWithTOTPAndRecoveryCodes(t *testing.T) {
	e := newAuthEnv(t)
	b := setupAdmin(t, e)
	var enrol struct{ Secret, URI string }
	if c := b.do("POST", "/api/v1/account/totp", "", true, &enrol); c != 200 || enrol.Secret == "" {
		t.Fatalf("enrol: %d", c)
	}
	if c := b.do("GET", "/api/v1/account/totp/qr.svg", "", false, nil); c != 200 {
		t.Fatalf("qr: %d", c)
	}
	if c := b.do("PUT", "/api/v1/account/totp", `{"code":"000000"}`, true, nil); c != 400 {
		t.Fatalf("wrong confirmation code: %d", c)
	}
	code, _ := auth.TOTPCode(enrol.Secret, e.now)
	var rec struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if c := b.do("PUT", "/api/v1/account/totp", `{"code":"`+code+`"}`, true, &rec); c != 200 || len(rec.RecoveryCodes) != 10 {
		t.Fatalf("confirm: %d %v", c, rec)
	}

	fresh := e.browser()
	var resp map[string]any
	if c := fresh.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`"}`, true, &resp); c != 401 || resp["need_code"] != true {
		t.Fatalf("password only: %d %v", c, resp)
	}
	// The code that confirmed enrolment was used up.
	if c := fresh.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`","code":"`+code+`"}`, true, nil); c != 401 {
		t.Fatalf("replayed code: %d", c)
	}
	e.now = e.now.Add(30 * time.Second)
	next, _ := auth.TOTPCode(enrol.Secret, e.now)
	if c := fresh.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`","code":"`+next+`"}`, true, nil); c != 200 {
		t.Fatalf("login with code: %d", c)
	}
	other := e.browser()
	if c := other.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`","code":"`+strings.ToUpper(rec.RecoveryCodes[0])+`"}`, true, nil); c != 200 {
		t.Fatalf("recovery code: %d", c)
	}
	if c := e.browser().do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`","code":"`+rec.RecoveryCodes[0]+`"}`, true, nil); c != 401 {
		t.Fatalf("recovery code reused: %d", c)
	}
	var me struct {
		User store.User `json:"user"`
	}
	fresh.do("GET", "/api/v1/account", "", false, &me)
	if !me.User.TOTPEnabled || me.User.RecoveryLeft != 9 {
		t.Errorf("account %+v", me.User)
	}
}

func TestRequire2FAAndLastAdmin(t *testing.T) {
	e := newAuthEnv(t)
	admin := setupAdmin(t, e)
	var me struct {
		User store.User `json:"user"`
	}
	admin.do("GET", "/api/v1/account", "", false, &me)
	if c := admin.do("PUT", "/api/v1/settings/auth", `{"require_2fa":true}`, true, nil); c != 200 {
		t.Fatalf("require 2fa: %d", c)
	}
	var body map[string]string
	if c := admin.do("GET", "/api/v1/sites", "", false, &body); c != 403 || body["code"] != "totp_required" {
		t.Fatalf("without 2fa under the requirement: %d %v", c, body)
	}
	if c := admin.do("GET", "/api/v1/account", "", false, nil); c != 200 {
		t.Fatalf("account must stay reachable to enrol: %d", c)
	}
	if c := e.token("GET", "/api/v1/sites", ""); c != 200 {
		t.Fatalf("the API token is not a user: %d", c)
	}
	// The last active administrator can't be demoted, disabled or deleted.
	for _, body := range []string{`{"role":"viewer"}`, `{"disabled":true}`} {
		if c := e.token("PUT", "/api/v1/users/ann", body); c != 409 {
			t.Errorf("%s on the last admin: %d", body, c)
		}
	}
	if c := e.token("DELETE", "/api/v1/users/ann", ""); c != 409 {
		t.Errorf("deleting the last admin: %d", c)
	}
}

func TestSessionsExpireAndDisabledUsersAreOut(t *testing.T) {
	e := newAuthEnv(t)
	admin := setupAdmin(t, e)
	admin.do("POST", "/api/v1/users", `{"username":"oli","role":"operator","password":"`+pw+`"}`, true, nil)
	op := e.browser()
	op.do("POST", "/api/v1/auth/login", `{"username":"oli","password":"`+pw+`"}`, true, nil)
	if c := op.do("GET", "/api/v1/sites", "", false, nil); c != 200 {
		t.Fatal(c)
	}
	e.token("PUT", "/api/v1/users/oli", `{"disabled":true}`)
	if c := op.do("GET", "/api/v1/sites", "", false, nil); c != 401 {
		t.Errorf("disabled user still in: %d", c)
	}
	if c := e.browser().do("POST", "/api/v1/auth/login", `{"username":"oli","password":"`+pw+`"}`, true, nil); c != 401 {
		t.Errorf("disabled user signed in: %d", c)
	}
	e.now = e.now.Add(sessionIdle + time.Minute)
	if c := admin.do("GET", "/api/v1/sites", "", false, nil); c != 401 {
		t.Errorf("idle session still valid: %d", c)
	}
	// Signing out ends the session server-side.
	b := e.browser()
	b.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`"}`, true, nil)
	b.do("POST", "/api/v1/auth/logout", "", true, nil)
	if n, _ := e.store.Sessions(context.Background(), 0); len(n) != 0 {
		t.Errorf("%d sessions after logout", len(n))
	}
}

func TestLoginThrottling(t *testing.T) {
	e := newAuthEnv(t)
	setupAdmin(t, e)
	b := e.browser()
	codes := map[int]int{}
	for range guardPerUser + 2 {
		codes[b.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"wrong wrong wrong"}`, true, nil)]++
	}
	if codes[401] != guardPerUser || codes[429] != 2 {
		t.Fatalf("status counts %v", codes)
	}
	// Even the right password waits out the lock.
	if c := b.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`"}`, true, nil); c != 429 {
		t.Fatalf("locked account: %d", c)
	}
	e.now = e.now.Add(guardWindow + time.Second)
	if c := b.do("POST", "/api/v1/auth/login", `{"username":"ann","password":"`+pw+`"}`, true, nil); c != 200 {
		t.Fatalf("after the window: %d", c)
	}
	var audit []store.AuditEntry
	b.do("GET", "/api/v1/audit", "", false, &audit)
	fails := 0
	for _, a := range audit {
		if a.Action == "login_failed" && a.Detail == "wrong password" {
			fails++
		}
	}
	if fails != guardPerUser {
		t.Errorf("%d failed logins audited", fails)
	}
}

func TestLoginGuardAgainstBurstsAndRotation(t *testing.T) {
	var g loginGuard
	now := time.Unix(1_800_000_000, 0)
	// A parallel burst: every attempt is counted before the slow password
	// check, so only the limit's worth get through.
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, _ := g.attempt(now, ipKey("198.51.100.1"), userKey("ann")); d == 0 {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != guardPerUser {
		t.Errorf("%d attempts passed, want %d", passed, guardPerUser)
	}
	// IPv6 addresses in one /64 share a budget.
	for i := range guardPerIP + 1 {
		d, _ := g.attempt(now, ipKey(fmt.Sprintf("2001:db8:1:2::%x", i+1)), userKey(fmt.Sprintf("user%d", i)))
		if (d > 0) != (i == guardPerIP) {
			t.Fatalf("attempt %d from the same /64: wait %v", i, d)
		}
	}
	// The refusal is reported once per window, not per request.
	if _, first := g.attempt(now, userKey("ann")); first {
		t.Error("lockout reported again")
	}
	// Asking for the second factor gives the attempt back.
	k := userKey("bob")
	for range guardPerUser + 5 {
		g.attempt(now, k)
		g.undo(k)
	}
	if d, _ := g.attempt(now, k); d != 0 {
		t.Error("undone attempts counted")
	}
}

func TestSetupClosedBeforeThrottle(t *testing.T) {
	e := newAuthEnv(t)
	setupAdmin(t, e)
	b := e.browser()
	for range 30 {
		if c := b.do("POST", "/api/v1/auth/setup", `{"token":"wrong","username":"x","password":"`+pw+`"}`, true, nil); c != 409 {
			t.Fatalf("setup on a set-up panel: %d", c)
		}
	}
	audit, _ := e.store.Audit(context.Background(), "", 100)
	for _, a := range audit {
		if a.Action == "setup_failed" {
			t.Fatal("a closed setup endpoint still audits (and throttles) attempts")
		}
	}
	// Oversized input never reaches hashing or the audit log.
	long := strings.Repeat("a", 10000)
	if c := b.do("POST", "/api/v1/auth/login", `{"username":"`+long+`","password":"x"}`, true, nil); c != 401 {
		t.Fatalf("long username: %d", c)
	}
	audit, _ = e.store.Audit(context.Background(), "", 100)
	for _, a := range audit {
		if len(a.Actor) > 64 {
			t.Fatalf("audit actor of %d bytes", len(a.Actor))
		}
	}
}
