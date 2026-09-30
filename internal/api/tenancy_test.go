package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/phpmyadmin"
	"github.com/parthh37/wpgenie/internal/sftp"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/updater"
)

// tenancyEnv: a panel with
//
//	account A (customer)          user alice   site sa (+ job)
//	account B (customer)          user bob     site sb (+ job)
//	account R (reseller)          user rita    site sr
//	  account C (R's customer)    user carl    site sc
//	no account                                  site sx (staff-only)
//
// Handlers that would reach Docker are never meant to run in these tests:
// if the access checks let a request through, the handler panics and the
// request fails loudly.
type tenancyEnv struct {
	t          *testing.T
	api        *Server
	srv        *httptest.Server
	st         *store.Store
	now        time.Time
	acct       map[string]*store.Account
	user       map[string]*store.User
	tokens     map[string]string // user -> API token
	jobs       map[string]int64  // site -> job
	sessionFor map[string]string // user -> session cookie
}

func newTenancyEnv(t *testing.T) *tenancyEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.DiscardHandler)
	svc := &site.Service{Cfg: config.Default(), Store: st, Log: log}
	svc.Jobs = &jobs.Queue{Store: st, Log: log}
	ml := &mail.Service{Cfg: mail.Config{DataDir: t.TempDir()}, Store: st, Log: log}
	if err := ml.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &tenancyEnv{t: t, st: st, now: time.Unix(1_800_000_000, 0), acct: map[string]*store.Account{},
		user: map[string]*store.User{}, tokens: map[string]string{}, jobs: map[string]int64{}, sessionFor: map[string]string{}}
	b := &billing.Service{Store: st, Sites: noSites{}, Log: log, Hooks: &billing.Webhooks{Store: st, Log: log},
		Now: func() time.Time { return e.now }}
	e.api = &Server{Token: "tok", Version: "v0", Sites: svc, Store: st, Mail: ml, Log: log, Jobs: svc.Jobs,
		Shield:  shield.New(shield.Options{Secret: []byte("k"), Sites: svc.ShieldLookup}),
		Updater: &updater.Updater{Current: "v0", Repo: "o/r", StateDir: t.TempDir(), APIBase: "http://127.0.0.1:1"},
		SFTP:    &sftp.Service{Store: st, Log: log}, PHPMyAdmin: &phpmyadmin.Service{Store: st, Log: log},
		Billing: b, PanelURL: "https://panel.test", Now: func() time.Time { return e.now }}
	e.srv = httptest.NewServer(e.api.Handler())
	t.Cleanup(e.srv.Close)

	ctx := context.Background()
	for _, p := range []*store.Plan{
		{ID: "basic", Name: "Basic", MaxSites: 3, MaxReplicas: 2, MaxMemoryMB: 1024, MaxCPUs: 1, MaxDomains: 3,
			Features: []string{"backups"}, BackupRepos: []string{"local"}, Resellable: true},
		{ID: "full", Name: "Full", MaxSites: 10, MaxReplicas: 4, MaxMemoryMB: 4096, MaxCPUs: 4,
			Features: billing.Features, BackupRepos: []string{"local"}},
	} {
		billing.NormalizePlan(p)
		if err := st.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name, kind, plan string, parent int64, user string, sites ...string) {
		a, u, _, err := b.CreateAccount(ctx, billing.AccountInput{Name: name, Kind: kind, PlanID: plan, ParentID: parent},
			"", &store.NewUser{Username: user, PasswordHash: "x", Role: auth.TenantRole(kind)}, false)
		if err != nil {
			t.Fatal(err)
		}
		e.acct[name], e.user[user] = a, u
		for _, s := range sites {
			e.site(s)
			if err := st.AssignSite(ctx, s, a.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("A", "customer", "basic", 0, "alice", "sa")
	mk("B", "customer", "full", 0, "bob", "sb")
	mk("R", "reseller", "full", 0, "rita", "sr")
	mk("C", "customer", "basic", e.acct["R"].ID, "carl", "sc")
	e.site("sx")
	for _, u := range []string{"alice", "bob", "rita", "carl"} {
		e.tokens[u] = e.token(u)
		e.sessionFor[u] = e.session(u)
	}
	for _, s := range []string{"sa", "sb", "sc", "sx"} {
		id, err := st.CreateJob(ctx, s, "backup", "someone")
		if err != nil {
			t.Fatal(err)
		}
		e.jobs[s] = id
	}
	return e
}

// noSites stands in for the site service's suspension (no Docker here).
type noSites struct{}

func (noSites) Suspend(context.Context, string) error   { return nil }
func (noSites) Unsuspend(context.Context, string) error { return nil }
func (noSites) Delete(context.Context, string) error    { return nil }

var nextPort = 19100

func (e *tenancyEnv) site(id string) {
	e.t.Helper()
	nextPort++
	if err := e.st.CreateSite(context.Background(), &store.Site{ID: id, Name: id, PrimaryDomain: id + ".test",
		PHPVersion: "8.3", FPMPort: nextPort, DBName: "wp_" + id, Status: store.StatusActive, ShieldMode: "standard",
		MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *tenancyEnv) token(user string) string {
	e.t.Helper()
	secret := apiTokenPrefix + auth.RandomToken(32)
	if _, err := e.st.CreateAPIToken(context.Background(), &store.APIToken{UserID: e.user[user].ID, Name: "t",
		Hint: "wpg_"}, auth.HashToken(secret)); err != nil {
		e.t.Fatal(err)
	}
	return secret
}

func (e *tenancyEnv) session(user string) string {
	e.t.Helper()
	cookie := auth.RandomToken(32)
	if err := e.st.CreateSession(context.Background(), &store.Session{ID: auth.RandomToken(9), UserID: e.user[user].ID,
		CreatedAt: e.now, LastSeenAt: e.now, ExpiresAt: e.now.Add(time.Hour)}, auth.HashToken(cookie)); err != nil {
		e.t.Fatal(err)
	}
	return cookie
}

// as sends a request as a user's API token ("tok": the installer token,
// "session:<user>": their browser session with the CSRF header).
func (e *tenancyEnv) as(who, method, path, body string, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	switch {
	case who == "tok":
		req.Header.Set("Authorization", "Bearer tok")
	case strings.HasPrefix(who, "session:"):
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.sessionFor[strings.TrimPrefix(who, "session:")]})
		req.Header.Set(csrfHeader, csrfValue)
	default:
		req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s as %s: %v (a handler ran that shouldn't have?)", method, path, who, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: decoding %q: %v", method, path, b, err)
		}
	}
	return resp.StatusCode
}

// fill makes a concrete path for a pattern, pointing every {id} at the
// target's resource of that kind.
func fill(pattern string, site, account, job string) (string, string) {
	method, path, _ := strings.Cut(pattern, " ")
	id := "x"
	switch routeScope(pattern) {
	case scopeSite:
		id = site
	case scopeAccount:
		id = account
	case scopeJob:
		id = job
	case scopeSelf:
		id = "1"
	}
	r := strings.NewReplacer("{id}", id, "{user}", "bob", "{domain}", "b.test", "{repo}", "local",
		"{backup}", "abcdef12", "{address}", "a@b.test")
	return method, r.Replace(path)
}

// TestEveryRouteIsClosedToOtherTenants walks the whole route table as a
// customer and as a reseller: every staff-only route is refused (403) and
// every tenant route on someone else's site, account or job answers 404.
// A new route is covered the moment it is registered.
func TestEveryRouteIsClosedToOtherTenants(t *testing.T) {
	e := newTenancyEnv(t)
	if len(e.api.routes) < 140 {
		t.Fatalf("only %d routes registered", len(e.api.routes))
	}
	foreignSite, foreignAcct, foreignJob := "sb", strconv.FormatInt(e.acct["B"].ID, 10), strconv.FormatInt(e.jobs["sb"], 10)
	for _, who := range []string{"alice", "rita", "session:alice", "session:carl"} {
		checked, skipped := 0, 0
		for _, rt := range e.api.routes {
			rule, open := tenantRoutes[rt.Pattern]
			sc := routeScope(rt.Pattern)
			method, path := fill(rt.Pattern, foreignSite, foreignAcct, foreignJob)
			want := http.StatusForbidden
			switch {
			case !open:
			case rule.reseller && !strings.Contains(who, "rita"):
			case sc == scopeSite || sc == scopeAccount || sc == scopeJob || sc == scopeOwned:
				// Owned resources ({id} "x" here) are nobody's either.
				want = http.StatusNotFound
			default:
				skipped++ // not about someone else's resource: see the tests below
				continue
			}
			got := e.as(who, method, path, "{}", nil)
			if got != want {
				t.Errorf("%s: %s %s = %d, want %d", who, method, path, got, want)
			}
			checked++
		}
		// Only tenant routes without a site, account or job in their path
		// (listing their own, their own user) are left to the tests below
		// (and to the features' own tests: invoicing_test.go, support_test.go
		// check that their lists only hold the tenant's own).
		if skipped > 40 || checked+skipped != len(e.api.routes) {
			t.Errorf("%s: %d routes checked, %d skipped of %d", who, checked, skipped, len(e.api.routes))
		}
	}
	// Staff-only sites (no account) are nobody's.
	for _, who := range []string{"alice", "rita"} {
		if c := e.as(who, "GET", "/api/v1/sites/sx", "", nil); c != 404 {
			t.Errorf("%s reached a staff-only site: %d", who, c)
		}
	}
}

// Every rule refers to a registered route (no stale entries hiding a typo
// that leaves a route staff-only, or one that opens a different route).
func TestTenantRoutesAreRegistered(t *testing.T) {
	e := newTenancyEnv(t)
	registered := map[string]bool{}
	for _, rt := range e.api.routes {
		if registered[rt.Pattern] {
			t.Errorf("%s registered twice", rt.Pattern)
		}
		registered[rt.Pattern] = true
	}
	for _, p := range sortedKeys(tenantRoutes) {
		if !registered[p] {
			t.Errorf("tenant rule for unregistered route %s", p)
		}
	}
	// A tenant route whose path value isn't a site, account or job is
	// refused at startup.
	tenantRoutes["GET /api/v1/mail/domains/{domain}"] = anyTenant
	defer delete(tenantRoutes, "GET /api/v1/mail/domains/{domain}")
	defer func() {
		if recover() == nil {
			t.Error("unscoped tenant route accepted")
		}
	}()
	checkTenantRoute("GET /api/v1/mail/domains/{domain}")
}

func ids(list []siteView) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.ID)
	}
	slices.Sort(out)
	return out
}

func TestTenantsSeeTheirOwn(t *testing.T) {
	e := newTenancyEnv(t)
	var sites []siteView
	if c := e.as("alice", "GET", "/api/v1/sites", "", &sites); c != 200 || fmt.Sprint(ids(sites)) != "[sa]" {
		t.Fatalf("alice's sites: %d %v", c, ids(sites))
	}
	if c := e.as("rita", "GET", "/api/v1/sites", "", &sites); c != 200 || fmt.Sprint(ids(sites)) != "[sc sr]" {
		t.Fatalf("the reseller's sites: %d %v", c, ids(sites))
	}
	if c := e.as("tok", "GET", "/api/v1/sites", "", &sites); c != 200 || len(sites) != 5 {
		t.Fatalf("staff: %d %v", c, ids(sites))
	}
	for _, c := range []struct {
		who, path string
		want      int
	}{
		{"alice", "/api/v1/sites/sa", 200},
		{"rita", "/api/v1/sites/sc", 200}, // a customer's site
		{"carl", "/api/v1/sites/sr", 404}, // not the reseller's
		{"carl", "/api/v1/sites/sc", 200},
		{"alice", "/api/v1/sites/sa/events", 200},
		{"alice", fmt.Sprintf("/api/v1/accounts/%d", e.acct["A"].ID), 200},
		{"alice", fmt.Sprintf("/api/v1/accounts/%d/usage", e.acct["A"].ID), 200},
		{"rita", fmt.Sprintf("/api/v1/accounts/%d/usage", e.acct["C"].ID), 200},
		{"carl", fmt.Sprintf("/api/v1/accounts/%d", e.acct["R"].ID), 404}, // the reseller's account isn't the customer's
		{"alice", "/api/v1/accounts", 403},                                // resellers only
		{"alice", fmt.Sprintf("/api/v1/jobs/%d", e.jobs["sa"]), 200},
		{"rita", fmt.Sprintf("/api/v1/jobs/%d", e.jobs["sc"]), 200},
		{"alice", "/api/v1/users", 403},
		{"alice", "/api/v1/system", 403},
		{"alice", "/api/v1/backups/repos", 403},
	} {
		if got := e.as(c.who, "GET", c.path, "", nil); got != c.want {
			t.Errorf("%s GET %s = %d, want %d", c.who, c.path, got, c.want)
		}
	}
	var jobsList []store.Job
	e.as("alice", "GET", "/api/v1/jobs", "", &jobsList)
	if len(jobsList) != 1 || jobsList[0].SiteID != "sa" {
		t.Fatalf("alice's jobs %+v", jobsList)
	}
	if c := e.as("alice", "GET", "/api/v1/jobs?site=sb", "", nil); c != 404 {
		t.Fatalf("jobs of another site: %d", c)
	}
	if c := e.as("alice", "GET", "/api/v1/security/events?site=sb", "", nil); c != 404 {
		t.Fatalf("security events of another site: %d", c)
	}
	var accts []accountView
	e.as("rita", "GET", "/api/v1/accounts", "", &accts)
	if len(accts) != 2 {
		t.Fatalf("reseller sees %d accounts", len(accts))
	}
	var usage []billing.Usage
	e.as("alice", "GET", "/api/v1/usage", "", &usage)
	if len(usage) != 1 || usage[0].AccountID != e.acct["A"].ID {
		t.Fatalf("usage %+v", usage)
	}
	var plans []store.Plan
	e.as("alice", "GET", "/api/v1/plans", "", &plans)
	if len(plans) != 1 || plans[0].ID != "basic" {
		t.Fatalf("a customer's plans %+v", plans)
	}
	e.as("rita", "GET", "/api/v1/plans", "", &plans)
	if len(plans) != 2 { // their own, and the resellable one that fits
		t.Fatalf("a reseller's plans %+v", plans)
	}
	var me struct {
		User    store.User  `json:"user"`
		Account accountView `json:"account"`
	}
	e.as("alice", "GET", "/api/v1/account", "", &me)
	if me.User.Role != "customer" || me.Account.ID != e.acct["A"].ID || me.Account.Limits.MaxReplicas != 2 {
		t.Fatalf("account %+v", me)
	}
}

func TestTenantPlanLimitsAndFeatures(t *testing.T) {
	e := newTenancyEnv(t)
	// Basic: 2 replicas, 1 GB, 1 CPU; no SFTP, no staging.
	for _, body := range []string{`{"memory_mb":512,"cpus":1,"replicas":3}`, `{"memory_mb":2048,"cpus":1,"replicas":1}`,
		`{"memory_mb":512,"cpus":2,"replicas":1}`} {
		var out map[string]string
		if c := e.as("alice", "PUT", "/api/v1/sites/sa/resources", body, &out); c != 403 || !strings.Contains(out["error"], "plan") {
			t.Errorf("%s: %d %v", body, c, out)
		}
	}
	var out map[string]string
	if c := e.as("alice", "PUT", "/api/v1/sites/sa/autoscale", `{"enabled":true,"min_replicas":1,"max_replicas":5,"target_cpu":70}`, &out); c != 403 {
		t.Errorf("autoscale beyond the plan: %d %v", c, out)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sa/sftp", "", &out); c != 403 || !strings.Contains(out["error"], "sftp") {
		t.Errorf("sftp without the feature: %d %v", c, out)
	}
	if c := e.as("alice", "POST", "/api/v1/sites/sa/staging", `{}`, &out); c != 403 {
		t.Errorf("staging without the feature: %d", c)
	}
	if c := e.as("alice", "PUT", "/api/v1/sites/sa/backups/policy", `{"repo_id":"offsite","interval_hours":24}`, &out); c != 403 {
		t.Errorf("a destination outside the plan: %d %v", c, out)
	}
	// Staff aren't bound by plans: the same request goes on to the site
	// service (which refuses it for its own reasons, not the plan's).
	if c := e.as("tok", "PUT", "/api/v1/sites/sa/resources", `{"memory_mb":512,"cpus":1,"replicas":0}`, nil); c != 400 {
		t.Errorf("staff: %d", c)
	}
	// The site count: A has 1 of 3; the second and third reservations are
	// refused once the plan is full.
	e.st.AssignSite(context.Background(), "extra1", e.acct["A"].ID)
	e.st.AssignSite(context.Background(), "extra2", e.acct["A"].ID)
	if c := e.as("alice", "POST", "/api/v1/sites", `{"domain":"new.test","admin_email":"a@b.co"}`, &out); c != 403 || !strings.Contains(out["error"], "site") {
		t.Errorf("site beyond the plan: %d %v", c, out)
	}
	// A customer can't put a site in someone else's account.
	if c := e.as("alice", "POST", fmt.Sprintf("/api/v1/sites?account=%d", e.acct["B"].ID), `{"domain":"new.test","admin_email":"a@b.co"}`, nil); c != 404 {
		t.Errorf("site in another account: %d", c)
	}
}

func TestSuspendedAndTerminatedAccounts(t *testing.T) {
	e := newTenancyEnv(t)
	ctx := context.Background()
	// Suspend by hand (the site service here has no runtime to stop).
	e.st.SetAccountStatus(ctx, e.acct["A"].ID, store.AccountSuspended, billing.ReasonBilling, e.now)
	var out map[string]string
	if c := e.as("alice", "POST", "/api/v1/sites/sa/cache/purge", "", &out); c != 403 || !strings.Contains(out["error"], "suspended") {
		t.Fatalf("suspended purge: %d %v", c, out)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sa", "", nil); c != 200 {
		t.Fatalf("suspended read: %d", c)
	}
	// A suspended reseller's customers are suspended with it.
	e.st.SetAccountStatus(ctx, e.acct["R"].ID, store.AccountSuspended, billing.ReasonAdmin, e.now)
	if c := e.as("carl", "PUT", "/api/v1/sites/sc/cache", `{"page_cache":true,"object_cache":true}`, &out); c != 403 {
		t.Fatalf("customer of a suspended reseller: %d", c)
	}
	// Terminated: no sign-in, no token, no session.
	e.st.SetAccountStatus(ctx, e.acct["B"].ID, store.AccountTerminated, "terminated", e.now)
	for _, who := range []string{"bob", "session:bob"} {
		if c := e.as(who, "GET", "/api/v1/account", "", nil); c != 401 {
			t.Errorf("%s of a terminated account: %d", who, c)
		}
	}
}

func TestResellerManagesCustomersOnly(t *testing.T) {
	e := newTenancyEnv(t)
	r, c := e.acct["R"].ID, e.acct["C"].ID
	// Not their own account (only an administrator suspends a reseller),
	// not someone else's.
	for _, id := range []int64{r, e.acct["A"].ID} {
		if code := e.as("rita", "POST", fmt.Sprintf("/api/v1/accounts/%d/suspend", id), "", nil); code != 404 {
			t.Errorf("suspend %d: %d", id, code)
		}
	}
	var v accountView
	if code := e.as("rita", "POST", fmt.Sprintf("/api/v1/accounts/%d/suspend", c), `{"reason":"admin"}`, &v); code != 200 ||
		v.Status != "suspended" || v.SuspendReason != "reseller" {
		t.Fatalf("suspend customer: %d %+v", code, v)
	}
	// An administrator's suspension can't be lifted by the reseller.
	e.as("tok", "POST", fmt.Sprintf("/api/v1/accounts/%d/suspend", c), `{"reason":"admin"}`, nil)
	if code := e.as("rita", "POST", fmt.Sprintf("/api/v1/accounts/%d/unsuspend", c), "", nil); code != 403 {
		t.Fatalf("reseller lifted an admin suspension: %d", code)
	}
	e.as("tok", "POST", fmt.Sprintf("/api/v1/accounts/%d/unsuspend", c), "", nil)

	// Creating a customer: always theirs, a plan they may hand out,
	// idempotent per reseller.
	body := `{"name":"New Co","plan_id":"basic","idempotency_key":"k1","user":{"username":"newco"}}`
	var created struct {
		Account  accountView `json:"account"`
		Password string      `json:"password"`
		Existed  bool        `json:"existed"`
	}
	if code := e.as("rita", "POST", "/api/v1/accounts", body, &created); code != 201 || created.Account.ParentID != r ||
		created.Password == "" || created.Account.Kind != "customer" {
		t.Fatalf("create: %d %+v", code, created)
	}
	first := created.Account.ID
	created = struct {
		Account  accountView `json:"account"`
		Password string      `json:"password"`
		Existed  bool        `json:"existed"`
	}{}
	if code := e.as("rita", "POST", "/api/v1/accounts", body, &created); code != 200 || !created.Existed ||
		created.Account.ID != first || created.Password != "" {
		t.Fatalf("retry: %d %+v", code, created)
	}
	for _, bad := range []string{
		`{"name":"X","plan_id":"full"}`,                    // not resellable
		`{"name":"X","plan_id":"basic","kind":"reseller"}`, // resellers only make customers
		fmt.Sprintf(`{"name":"X","plan_id":"basic","parent_id":%d}`, e.acct["A"].ID),
		`{"name":"X","plan_id":"basic","stripe_customer_id":"cus_1"}`,
	} {
		if code := e.as("rita", "POST", "/api/v1/accounts", bad, nil); code != 403 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code := e.as("rita", "PUT", fmt.Sprintf("/api/v1/accounts/%d", c), `{"parent_id":0}`, nil); code != 403 {
		t.Errorf("reseller moved a customer out: %d", code)
	}
	// Users of their customers: yes; of others: no.
	if code := e.as("rita", "POST", fmt.Sprintf("/api/v1/accounts/%d/users/carl/password", c), `{}`, nil); code != 200 {
		t.Errorf("reset carl's password: %d", code)
	}
	if code := e.as("rita", "POST", fmt.Sprintf("/api/v1/accounts/%d/users/bob/password", c), `{}`, nil); code != 404 {
		t.Errorf("reset bob's password through C: %d", code)
	}
}

func TestAPITokensActAsTheirUser(t *testing.T) {
	e := newTenancyEnv(t)
	// A token never exceeds its user: alice's token is a customer's.
	if c := e.as("alice", "GET", "/api/v1/settings/auth", "", nil); c != 403 {
		t.Fatalf("customer token on a staff route: %d", c)
	}
	// Tokens come from sessions, not from other tokens.
	var out map[string]any
	if c := e.as("alice", "POST", "/api/v1/account/tokens", `{"name":"ci"}`, &out); c != 403 {
		t.Fatalf("token minted with a token: %d", c)
	}
	if c := e.as("session:alice", "POST", "/api/v1/account/tokens", `{"name":"ci","expires_days":1}`, &out); c != 201 {
		t.Fatalf("create: %d %v", c, out)
	}
	secret := out["token"].(string)
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/sites", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("new token: %d", resp.StatusCode)
	}
	// Expired.
	e.now = e.now.Add(25 * time.Hour)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expired token: %d", resp.StatusCode)
	}
	// A disabled user's tokens stop working; revoked tokens too.
	e.st.SetUserRole(context.Background(), e.user["carl"].ID, "customer", true)
	if c := e.as("carl", "GET", "/api/v1/sites", "", nil); c != 401 {
		t.Fatalf("disabled user's token: %d", c)
	}
	var list []store.APIToken
	e.as("tok", "GET", "/api/v1/tokens", "", &list)
	for _, tk := range list {
		if tk.Username == "rita" {
			e.as("tok", "DELETE", fmt.Sprintf("/api/v1/tokens/%d", tk.ID), "", nil)
		}
	}
	if c := e.as("rita", "GET", "/api/v1/sites", "", nil); c != 401 {
		t.Fatalf("revoked token: %d", c)
	}
	// A tenant user can never be given a staff role.
	if c := e.as("tok", "PUT", fmt.Sprintf("/api/v1/users/%d", e.user["alice"].ID), `{"role":"admin"}`, nil); c != 400 {
		t.Fatalf("tenant promoted: %d", c)
	}
}

func TestTwoFactorRequirementCoversAccountsRoutes(t *testing.T) {
	e := newTenancyEnv(t)
	e.st.SetSetting(context.Background(), settingRequire2FA, "1")
	var out map[string]string
	path := fmt.Sprintf("/api/v1/accounts/%d", e.acct["A"].ID)
	if c := e.as("session:alice", "GET", path, "", &out); c != 403 || out["code"] != "totp_required" {
		t.Fatalf("accounts route without 2FA: %d %v", c, out)
	}
	if c := e.as("session:alice", "GET", "/api/v1/account", "", nil); c != 200 {
		t.Fatalf("own account without 2FA: %d", c)
	}
}

// A password alone must not buy a credential past the two-factor
// requirement: no tokens for users without 2FA, and their existing
// tokens reach only their own user.
func TestTwoFactorRequirementCoversTokens(t *testing.T) {
	e := newTenancyEnv(t)
	e.st.SetSetting(context.Background(), settingRequire2FA, "1")
	var out map[string]string
	if c := e.as("session:alice", "POST", "/api/v1/account/tokens", `{"name":"x"}`, &out); c != 403 {
		t.Fatalf("token minted without 2FA: %d %v", c, out)
	}
	if c := e.as("alice", "GET", "/api/v1/sites", "", &out); c != 403 || out["code"] != "totp_required" {
		t.Fatalf("token of a user without 2FA: %d %v", c, out)
	}
	if c := e.as("alice", "GET", "/api/v1/account", "", nil); c != 200 {
		t.Fatalf("own account by token: %d", c)
	}
	if c := e.as("tok", "POST", fmt.Sprintf("/api/v1/users/%d/tokens", e.user["alice"].ID), `{"name":"x"}`, nil); c != 403 {
		t.Fatalf("admin minted a token for a user without 2FA: %d", c)
	}
	// Resetting a user's password revokes their tokens.
	e.st.SetSetting(context.Background(), settingRequire2FA, "0")
	if c := e.as("rita", "POST", fmt.Sprintf("/api/v1/accounts/%d/users/carl/password", e.acct["C"].ID), `{}`, nil); c != 200 {
		t.Fatalf("reset: %d", c)
	}
	if c := e.as("carl", "GET", "/api/v1/sites", "", nil); c != 401 {
		t.Fatalf("token survived a password reset: %d", c)
	}
}

// WHMCS service IDs belong to the billing system that set them: a
// reseller reusing the owner's service number never steers the owner's
// WHMCS onto their account.
func TestWHMCSServiceIDsAreNamespaced(t *testing.T) {
	e := newTenancyEnv(t)
	cID := e.acct["C"].ID
	if c := e.as("rita", "PUT", fmt.Sprintf("/api/v1/accounts/%d", cID), `{"whmcs_service_id":"123"}`, nil); c != 200 {
		t.Fatalf("reseller sets its WHMCS ID: %d", c)
	}
	if c := e.as("tok", "PUT", fmt.Sprintf("/api/v1/accounts/%d", e.acct["A"].ID), `{"whmcs_service_id":"123"}`, nil); c != 200 {
		t.Fatalf("owner sets the same number: %d", c)
	}
	// Unique within one billing system.
	if c := e.as("tok", "PUT", fmt.Sprintf("/api/v1/accounts/%d", e.acct["B"].ID), `{"whmcs_service_id":"123"}`, nil); c != 409 {
		t.Fatalf("duplicate in the owner's namespace: %d", c)
	}
	var list []accountView
	e.as("tok", "GET", "/api/v1/accounts?whmcs_service_id=123", "", &list)
	if len(list) != 1 || list[0].ID != e.acct["A"].ID {
		t.Fatalf("owner's lookup: %+v", list)
	}
	e.as("rita", "GET", "/api/v1/accounts?whmcs_service_id=123", "", &list)
	if len(list) != 1 || list[0].ID != cID {
		t.Fatalf("reseller's lookup: %+v", list)
	}
	var usage []billing.Usage
	e.as("tok", "GET", "/api/v1/usage", "", &usage)
	for _, u := range usage {
		if u.WHMCSServiceID == "123" && u.AccountID != e.acct["A"].ID {
			t.Fatalf("owner's usage report carries the reseller's service ID: %+v", u)
		}
	}
}

func TestTenantsCantTakeMailDomains(t *testing.T) {
	e := newTenancyEnv(t)
	if err := e.st.AddMailDomain(context.Background(), "owner-company.test"); err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if c := e.as("alice", "POST", "/api/v1/sites", `{"domain":"owner-company.test","admin_email":"a@b.co"}`, &out); c != 403 ||
		!strings.Contains(out["error"], "mail") {
		t.Fatalf("site on a hosted mail domain: %d %v", c, out)
	}
	if c := e.as("alice", "POST", "/api/v1/sites/sa/domains", `{"domain":"Owner-Company.test"}`, &out); c != 403 {
		t.Fatalf("alias on a hosted mail domain: %d %v", c, out)
	}
}

// A reseller's customer suspended by an administrator stays frozen for
// the reseller too.
func TestSuspendedCustomersSitesAreFrozenForTheirReseller(t *testing.T) {
	e := newTenancyEnv(t)
	e.st.SetAccountStatus(context.Background(), e.acct["C"].ID, store.AccountSuspended, billing.ReasonAdmin, e.now)
	var out map[string]string
	if c := e.as("rita", "DELETE", "/api/v1/sites/sc/domains/x.test", "", &out); c != 403 || !strings.Contains(out["error"], "suspended") {
		t.Fatalf("reseller changed a suspended customer's site: %d %v", c, out)
	}
	if c := e.as("rita", "GET", "/api/v1/sites/sc", "", nil); c != 200 {
		t.Fatalf("read: %d", c)
	}
}

func TestReservedUsernames(t *testing.T) {
	e := newTenancyEnv(t)
	for _, name := range []string{"api-token", "Scheduler", "stripe"} {
		body := fmt.Sprintf(`{"username":%q}`, name)
		if c := e.as("rita", "POST", fmt.Sprintf("/api/v1/accounts/%d/users", e.acct["C"].ID), body, nil); c != 400 {
			t.Errorf("%s: %d", name, c)
		}
	}
}

func TestSingleSignOn(t *testing.T) {
	e := newTenancyEnv(t)
	var link struct {
		URL string `json:"url"`
	}
	path := fmt.Sprintf("/api/v1/accounts/%d/sso", e.acct["C"].ID)
	if c := e.as("alice", "POST", path, `{"username":"carl"}`, nil); c != 403 {
		t.Fatalf("customer minted SSO: %d", c)
	}
	if c := e.as("rita", "POST", path, `{"username":"carl"}`, &link); c != 200 || !strings.HasPrefix(link.URL, "https://panel.test/#sso=") {
		t.Fatalf("reseller SSO: %d %+v", c, link)
	}
	tok := strings.TrimPrefix(link.URL, "https://panel.test/#sso=")
	exchange := func(csrf bool) (int, *http.Cookie) {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/auth/sso", strings.NewReader(`{"token":"`+tok+`"}`))
		if csrf {
			req.Header.Set(csrfHeader, csrfValue)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookie {
				return resp.StatusCode, c
			}
		}
		return resp.StatusCode, nil
	}
	if c, _ := exchange(false); c != 403 {
		t.Fatalf("without the CSRF header: %d", c)
	}
	c, cookie := exchange(true)
	if c != 200 || cookie == nil || !cookie.HttpOnly {
		t.Fatalf("exchange: %d %v", c, cookie)
	}
	if c, _ := exchange(true); c != 401 {
		t.Fatalf("second use: %d", c)
	}
	// Expired links don't work.
	e.as("rita", "POST", path, `{"username":"carl"}`, &link)
	tok = strings.TrimPrefix(link.URL, "https://panel.test/#sso=")
	e.now = e.now.Add(3 * time.Minute)
	if c, _ := exchange(true); c != 401 {
		t.Fatalf("expired: %d", c)
	}
	// Staff sign on with their password and second factor, never a link.
	if c := e.as("tok", "POST", "/api/v1/accounts/999/sso", `{}`, nil); c != 404 {
		t.Fatalf("no account: %d", c)
	}
}

func TestStripeWebhookEndpoint(t *testing.T) {
	e := newTenancyEnv(t)
	post := func(sig string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/billing/stripe/webhook", strings.NewReader(`{"id":"evt_1","type":"invoice.paid"}`))
		req.Header.Set("Stripe-Signature", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("t=1,v1=00"); c != 404 {
		t.Fatalf("unconfigured: %d", c)
	}
	e.as("tok", "PUT", "/api/v1/billing/settings", `{"webhook_secret":"whsec_abc"}`, nil)
	if c := post(fmt.Sprintf("t=%d,v1=%s", time.Now().Unix(), strings.Repeat("ab", 32))); c != 400 {
		t.Fatalf("bad signature: %d", c)
	}
	var settings map[string]any
	e.as("tok", "GET", "/api/v1/billing/settings", "", &settings)
	if settings["stripe_webhook_secret_set"] != true || strings.Contains(fmt.Sprint(settings), "whsec_abc") {
		t.Fatalf("settings leak the secret: %v", settings)
	}
}
