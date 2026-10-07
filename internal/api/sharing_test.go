package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/sftp"
	"github.com/parthh37/wpgenie/internal/store"
)

// share has an owner share a site through the API.
func (e *tenancyEnv) share(owner, site, user, access string) {
	e.t.Helper()
	var out map[string]any
	if c := e.as(owner, "POST", "/api/v1/sites/"+site+"/access",
		fmt.Sprintf(`{"username":%q,"access":%q}`, user, access), &out); c != http.StatusCreated {
		e.t.Fatalf("%s sharing %s with %s: %d %v", owner, site, user, c, out)
	}
}

func (e *tenancyEnv) grantPath(site, user string) string {
	return "/api/v1/sites/" + site + "/access/" + strconv.FormatInt(e.user[user].ID, 10)
}

func siteIn(list []siteView, id string) *siteView {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

// TestSharedSitesAreReachable: a site shared with a user of another account
// is listed for them with its level, and its jobs and events are theirs to
// see; the owner's account isn't.
func TestSharedSitesAreReachable(t *testing.T) {
	e := newTenancyEnv(t)
	e.share("bob", "sb", "alice", auth.AccessViewer)
	for _, who := range []string{"alice", "session:alice"} {
		var sites []siteView
		if c := e.as(who, "GET", "/api/v1/sites", "", &sites); c != 200 || fmt.Sprint(ids(sites)) != "[sa sb]" {
			t.Fatalf("%s's sites: %d %v", who, c, ids(sites))
		}
		if sb, sa := siteIn(sites, "sb"), siteIn(sites, "sa"); sb.Access != "viewer" || sb.AccountID != e.acct["B"].ID || sa.Access != "" {
			t.Fatalf("levels: sa %+v, sb %+v", sa, sb)
		}
	}
	var one siteView
	if c := e.as("alice", "GET", "/api/v1/sites/sb", "", &one); c != 200 || one.Access != "viewer" {
		t.Fatalf("the shared site: %d %+v", c, one)
	}
	var owners siteView
	if c := e.as("bob", "GET", "/api/v1/sites/sb", "", &owners); c != 200 || owners.Access != "" {
		t.Fatalf("its owner: %d %+v", c, owners)
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{fmt.Sprintf("/api/v1/jobs/%d", e.jobs["sb"]), 200},
		{"/api/v1/jobs?site=sb", 200},
		{"/api/v1/security/events?site=sb", 200},
		{"/api/v1/sites/sb/events", 200},
		{fmt.Sprintf("/api/v1/accounts/%d", e.acct["B"].ID), 404},
		{fmt.Sprintf("/api/v1/accounts/%d/usage", e.acct["B"].ID), 404},
		{"/api/v1/sites/sc", 404}, // only what was shared
	} {
		if got := e.as("alice", "GET", c.path, "", nil); got != c.want {
			t.Errorf("alice GET %s = %d, want %d", c.path, got, c.want)
		}
	}
	var jobsList []store.Job
	if e.as("alice", "GET", "/api/v1/jobs", "", &jobsList); len(jobsList) != 2 {
		t.Fatalf("alice's jobs (hers and the shared site's): %+v", jobsList)
	}
	// The other party sees nothing new.
	var sites []siteView
	if e.as("bob", "GET", "/api/v1/sites", "", &sites); fmt.Sprint(ids(sites)) != "[sb]" {
		t.Fatalf("the owner's sites: %v", ids(sites))
	}
}

// TestEveryLevelIsHeldToItsRoutes walks every tenant route on a site at
// each level: whatever needs more than the level is refused, by the access
// check (not the plan: the owner's plan has every feature).
func TestEveryLevelIsHeldToItsRoutes(t *testing.T) {
	e := newTenancyEnv(t)
	e.share("bob", "sb", "alice", auth.AccessViewer)
	refusedAt := map[string]int{}
	for _, level := range []string{auth.AccessViewer, auth.AccessDeveloper, auth.AccessManager} {
		if c := e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"`+level+`"}`, nil); c != 200 {
			t.Fatalf("setting %s: %d", level, c)
		}
		for _, rt := range e.api.routes {
			if _, open := tenantRoutes[rt.Pattern]; !open || routeScope(rt.Pattern) != scopeSite {
				continue
			}
			if accessAllows(level, requiredAccess(rt.Pattern)) {
				continue
			}
			method, path := fill(rt.Pattern, "sb", "", "")
			var out map[string]string
			got := e.as("alice", method, path, "{}", &out)
			if got != 403 || !(strings.Contains(out["error"], "access to the site") || strings.Contains(out["error"], "site's owner")) {
				t.Errorf("%s: %s %s = %d %v, want refused for the level", level, method, path, got, out)
			}
			refusedAt[level]++
		}
	}
	// Manager is refused only what owners alone do: sharing it (deleting a
	// live site is the handler's: TestSharedLevelsInPractice).
	if refusedAt["manager"] != 4 || refusedAt["developer"] <= refusedAt["manager"] || refusedAt["viewer"] <= refusedAt["developer"] {
		t.Fatalf("routes refused per level: %v", refusedAt)
	}
}

// TestSharedLevelsInPractice: authorisation runs before the handler reads
// the body, so a malformed one answers 400 once the level is enough and
// 403 before.
func TestSharedLevelsInPractice(t *testing.T) {
	e := newTenancyEnv(t)
	e.share("bob", "sb", "alice", auth.AccessViewer)
	probe := func(method, path string) int { return e.as("alice", method, path, "not json", nil) }
	set := func(level string) { e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"`+level+`"}`, nil) }

	cache, resources := "/api/v1/sites/sb/cache", "/api/v1/sites/sb/resources"
	if c := probe("PUT", cache); c != 403 {
		t.Fatalf("viewer changing the cache: %d", c)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sb/wp-admin/users", "", nil); c != 403 {
		t.Fatalf("viewer reading WordPress users: %d", c)
	}
	set(auth.AccessDeveloper)
	if c := probe("PUT", cache); c != 400 {
		t.Fatalf("developer changing the cache: %d", c)
	}
	if c := probe("PUT", resources); c != 403 {
		t.Fatalf("developer scaling: %d", c)
	}
	set(auth.AccessManager)
	if c := probe("PUT", resources); c != 400 {
		t.Fatalf("manager scaling: %d", c)
	}
	for _, rt := range []struct{ method, path string }{
		{"DELETE", "/api/v1/sites/sb"},
		{"GET", "/api/v1/sites/sb/access"},
		{"POST", "/api/v1/sites/sb/access"},
	} {
		if c := probe(rt.method, rt.path); c != 403 {
			t.Errorf("manager: %s %s = %d, want 403", rt.method, rt.path, c)
		}
	}

	var del map[string]string
	if c := e.as("alice", "DELETE", "/api/v1/sites/sb", "", &del); c != 403 || !strings.Contains(del["error"], "live site") {
		t.Fatalf("manager deleting the live site: %d %v", c, del)
	}

	// The owner's suspension freezes the site for them too; reading goes on.
	ctx := context.Background()
	e.st.SetAccountStatus(ctx, e.acct["B"].ID, store.AccountSuspended, billing.ReasonBilling, e.now)
	var out map[string]string
	if c := e.as("alice", "PUT", cache, "not json", &out); c != 403 || !strings.Contains(out["error"], "suspended") {
		t.Fatalf("owner suspended: %d %v", c, out)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sb", "", nil); c != 200 {
		t.Fatalf("owner suspended, reading: %d", c)
	}
	e.st.SetAccountStatus(ctx, e.acct["B"].ID, store.AccountActive, "", e.now)

	// Given to another account, the site stops being shared.
	if c := e.as("tok", "PUT", "/api/v1/sites/sb/account", fmt.Sprintf(`{"account_id":%d}`, e.acct["C"].ID), nil); c != 200 {
		t.Fatalf("moving the site: %d", c)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sb", "", nil); c != 404 {
		t.Fatalf("a moved site is still shared: %d", c)
	}
}

func TestSharingRules(t *testing.T) {
	e := newTenancyEnv(t)
	ctx := context.Background()
	if _, err := e.st.CreateUser(ctx, "sam", "x", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who, site, user, access string
		want                    int
	}{
		{"alice", "sa", "nobody", "viewer", 400},
		{"alice", "sa", "sam", "viewer", 400}, // staff: as if nobody, and they reach every site anyway
		{"alice", "sa", "bob", "owner", 400},
		{"alice", "sa", "bob", "", 400},
		{"alice", "sa", "alice", "viewer", 409}, // their own
		{"carl", "sc", "rita", "viewer", 409},   // the reseller has it already
		{"rita", "sc", "carl", "viewer", 409},
		{"alice", "sb", "carl", "viewer", 404}, // not hers to share
		{"tok", "sx", "alice", "viewer", 400},  // staff-only sites aren't anyone's to share
		{"rita", "sc", "alice", "developer", 201},
		{"tok", "sb", "alice", "viewer", 201},
		{"bob", "sb", "ALICE", "manager", 409}, // already shared (names are case-insensitive)
	} {
		var out map[string]any
		body := fmt.Sprintf(`{"username":%q,"access":%q}`, c.user, c.access)
		if got := e.as(c.who, "POST", "/api/v1/sites/"+c.site+"/access", body, &out); got != c.want {
			t.Errorf("%s sharing %s with %s as %q: %d %v, want %d", c.who, c.site, c.user, c.access, got, out, c.want)
		}
	}
	var nobody, staff map[string]string
	e.as("alice", "POST", "/api/v1/sites/sa/access", `{"username":"sam","access":"viewer"}`, &staff)
	e.as("alice", "POST", "/api/v1/sites/sa/access", `{"username":"sam2","access":"viewer"}`, &nobody)
	if strings.Replace(staff["error"], "sam", "sam2", 1) != nobody["error"] {
		t.Fatalf("staff names tell themselves apart: %q vs %q", staff["error"], nobody["error"])
	}

	// Who has it: the owner sees and changes it; whoever it was shared with
	// can't.
	var list []store.SiteGrant
	if c := e.as("bob", "GET", "/api/v1/sites/sb/access", "", &list); c != 200 || len(list) != 1 ||
		list[0].Username != "alice" || list[0].Access != "viewer" || list[0].GrantedBy != "api-token" {
		t.Fatalf("sb's grants: %d %+v", c, list)
	}
	if c := e.as("alice", "POST", "/api/v1/sites/sb/access", `{"username":"carl","access":"viewer"}`, nil); c != 403 {
		t.Fatalf("re-shared by whoever it was shared with: %d", c)
	}
	var g store.SiteGrant
	if c := e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"developer"}`, &g); c != 200 || g.Access != "developer" {
		t.Fatalf("changing the level: %d %+v", c, g)
	}
	if c := e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"root"}`, nil); c != 400 {
		t.Fatalf("a made-up level: %d", c)
	}
	if c := e.as("bob", "PUT", "/api/v1/sites/sb/access/99999", `{"access":"viewer"}`, nil); c != 404 {
		t.Fatalf("changing a grant that doesn't exist: %d", c)
	}
	// Another site's grant isn't reachable through this one.
	if c := e.as("bob", "DELETE", e.grantPath("sb", "carl"), "", nil); c != 404 {
		t.Fatalf("revoking what wasn't shared: %d", c)
	}
	if c := e.as("bob", "DELETE", e.grantPath("sb", "alice"), "", nil); c != 204 {
		t.Fatalf("revoking: %d", c)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sb", "", nil); c != 404 {
		t.Fatalf("revoked, still reachable: %d", c)
	}
	// The reseller's share of their customer's site still stands.
	if c := e.as("alice", "GET", "/api/v1/sites/sc", "", nil); c != 200 {
		t.Fatalf("shared by the reseller: %d", c)
	}
}

// TestSharedStagingCopies: a live site's staging copies are shared with it,
// at its level, and stop being when it does; pushing into the live site
// needs the push's level.
func TestSharedStagingCopies(t *testing.T) {
	e := newTenancyEnv(t)
	ctx := context.Background()
	nextPort++
	if err := e.st.CreateSite(ctx, &store.Site{ID: "sbstg", Name: "sbstg", PrimaryDomain: "sbstg.test", PHPVersion: "8.3",
		FPMPort: nextPort, DBName: "wp_sbstg", Status: store.StatusActive, ShieldMode: "standard", ParentID: "sb"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AssignSite(ctx, "sbstg", e.acct["B"].ID); err != nil {
		t.Fatal(err)
	}
	e.share("bob", "sb", "alice", auth.AccessViewer)
	var sites []siteView
	if e.as("alice", "GET", "/api/v1/sites", "", &sites); fmt.Sprint(ids(sites)) != "[sa sb sbstg]" || siteIn(sites, "sbstg").Access != "viewer" {
		t.Fatalf("alice's sites: %v", ids(sites))
	}
	var out map[string]string
	if c := e.as("alice", "POST", "/api/v1/sites/sbstg/push", `{}`, &out); c != 403 || !strings.Contains(out["error"], "developer") {
		t.Fatalf("a viewer pushing: %d %v", c, out)
	}
	// Its own owner's sharing only: given to another account, it isn't.
	if err := e.st.AssignSite(ctx, "sbstg", e.acct["C"].ID); err != nil {
		t.Fatal(err)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sbstg", "", nil); c != 404 {
		t.Fatalf("a copy another account owns: %d", c)
	}
	if err := e.st.AssignSite(ctx, "sbstg", e.acct["B"].ID); err != nil {
		t.Fatal(err)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sbstg", "", nil); c != 200 {
		t.Fatalf("back with its live site's account: %d", c)
	}
	// Revoked on the live site: the copy goes too.
	if c := e.as("bob", "DELETE", e.grantPath("sb", "alice"), "", nil); c != 204 {
		t.Fatalf("revoking: %d", c)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sbstg", "", nil); c != 404 {
		t.Fatalf("the copy of a revoked site: %d", c)
	}
}

// TestSharedFixesNeedTheirRoutesLevel: an analyser fix needs the level of
// the route it does the work of.
func TestSharedFixesNeedTheirRoutesLevel(t *testing.T) {
	e := newTenancyEnv(t)
	e.share("bob", "sb", "alice", auth.AccessDeveloper)
	for _, fix := range []string{"shield", "waf"} {
		var out map[string]string
		if c := e.as("alice", "POST", "/api/v1/sites/sb/analysis/fix", `{"fix":"`+fix+`"}`, &out); c != 403 || !strings.Contains(out["error"], "manager") {
			t.Errorf("developer applying %s: %d %v", fix, c, out)
		}
	}
	registered := map[string]bool{}
	for _, rt := range e.api.routes {
		registered[rt.Pattern] = true
	}
	for fix, route := range fixRoutes {
		if !registered[route] {
			t.Errorf("fix %s stands in for %s, which isn't a route", fix, route)
		}
	}
}

// fakeDocker lets the SFTP service render logins without a container.
type fakeDocker struct{}

func (fakeDocker) Run(context.Context, io.Reader, ...string) ([]byte, error) { return nil, nil }
func (fakeDocker) EnsureBuilt(context.Context, string, string) (string, error) {
	return "img", nil
}

// TestSFTPLoginsGoWithAccess: the SFTP logins someone added to a shared
// site are deleted when they drop to viewer, lose access or are deleted;
// the owner's stay.
func TestSFTPLoginsGoWithAccess(t *testing.T) {
	e := newTenancyEnv(t)
	e.api.SFTP = &sftp.Service{Cfg: sftp.Config{DataDir: t.TempDir(), SitesDir: t.TempDir()}, Store: e.st,
		Docker: fakeDocker{}, Log: e.api.Log}
	logins := func() map[string]string {
		var info struct {
			Users []store.SFTPUser `json:"users"`
		}
		if c := e.as("bob", "GET", "/api/v1/sites/sb/sftp", "", &info); c != 200 {
			t.Fatalf("listing logins: %d", c)
		}
		out := map[string]string{}
		for _, u := range info.Users {
			out[u.Username] = u.AddedByName
		}
		return out
	}
	add := func(who, suffix string) {
		var out map[string]any
		if c := e.as(who, "POST", "/api/v1/sites/sb/sftp", `{"suffix":"`+suffix+`","password":true}`, &out); c != 201 {
			t.Fatalf("%s adding a login: %d %v", who, c, out)
		}
	}
	e.share("bob", "sb", "alice", auth.AccessDeveloper)
	add("bob", "own")
	add("alice", "dev")
	if l := logins(); len(l) != 2 || l["sb-own"] != "bob" || l["sb-dev"] != "alice" {
		t.Fatalf("logins %v", l)
	}
	e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"viewer"}`, nil)
	if l := logins(); len(l) != 1 || l["sb-own"] == "" {
		t.Fatalf("after dropping to viewer: %v", l)
	}
	e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"developer"}`, nil)
	add("alice", "dev")
	if c := e.as("bob", "DELETE", e.grantPath("sb", "alice"), "", nil); c != 204 {
		t.Fatalf("revoking: %d", c)
	}
	if l := logins(); len(l) != 1 {
		t.Fatalf("after revoking: %v", l)
	}
	e.share("bob", "sb", "alice", auth.AccessDeveloper)
	add("alice", "dev")
	if c := e.as("tok", "DELETE", fmt.Sprintf("/api/v1/accounts/%d/users/alice", e.acct["A"].ID), "", nil); c != 204 {
		t.Fatalf("deleting the user: %d", c)
	}
	if l := logins(); len(l) != 1 {
		t.Fatalf("after deleting the user: %v", l)
	}
}

// TestSharedBurstShowsOnlyThisSite: the owner's burst balance comes with
// the site, without naming the account's other sites.
func TestSharedBurstShowsOnlyThisSite(t *testing.T) {
	e := newTenancyEnv(t)
	ctx := context.Background()
	e.site("sb2")
	if err := e.st.AssignSite(ctx, "sb2", e.acct["B"].ID); err != nil {
		t.Fatal(err)
	}
	month := billing.MonthStart(e.now)
	e.st.AddBurstCharge(ctx, e.acct["B"].ID, "sb", month, 5)
	e.st.AddBurstCharge(ctx, e.acct["B"].ID, "sb2", month, 7)
	e.share("bob", "sb", "alice", auth.AccessViewer)
	var mine, theirs siteBurstView
	if c := e.as("alice", "GET", "/api/v1/sites/sb/burst", "", &mine); c != 200 || fmt.Sprint(mine.Account.PerSite) != "map[sb:5]" {
		t.Fatalf("shared: %d %+v", c, mine.Account)
	}
	if c := e.as("bob", "GET", "/api/v1/sites/sb/burst", "", &theirs); c != 200 || len(theirs.Account.PerSite) != 2 {
		t.Fatalf("owner: %d %+v", c, theirs.Account)
	}
}

// Every level rule names a tenant route on a site.
func TestSiteAccessRulesAreTenantSiteRoutes(t *testing.T) {
	e := newTenancyEnv(t)
	registered := map[string]bool{}
	for _, rt := range e.api.routes {
		registered[rt.Pattern] = true
	}
	for _, p := range sortedKeys(siteAccessRules) {
		if _, open := tenantRoutes[p]; !open || !registered[p] || routeScope(p) != scopeSite {
			t.Errorf("site access rule for %s, which isn't a registered tenant route on a site", p)
		}
	}
}
