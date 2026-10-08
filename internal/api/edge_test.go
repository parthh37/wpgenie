package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/site"
)

// edgeProxy records what reaches Caddy.
type edgeProxy struct{ last []proxy.Site }

func (p *edgeProxy) Apply(_ context.Context, sites []proxy.Site) error {
	p.last = sites
	return nil
}

func (p *edgeProxy) site(id string) *proxy.Site {
	for i := range p.last {
		if p.last[i].ID == id {
			return &p.last[i]
		}
	}
	return nil
}

// raw sends a request and returns the status and body as text.
func (e *tenancyEnv) raw(who, method, path, body string) (int, string) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if who == "tok" {
		req.Header.Set("Authorization", "Bearer tok")
	} else {
		req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestSiteLockAndRedirectsAccess: users a site is shared with change its
// lock and redirects only as managers; viewers read the redirects; nobody,
// at any level, ever sees the lock's password or its hash.
func TestSiteLockAndRedirectsAccess(t *testing.T) {
	e := newTenancyEnv(t)
	px := &edgeProxy{}
	e.api.Sites.Proxy = px
	ctx := context.Background()
	vic, err := e.st.CreateUser(ctx, "vic", "x", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	e.user["vic"] = vic
	e.tokens["vic"] = e.token("vic")
	e.share("bob", "sb", "alice", auth.AccessViewer)
	const lock = `{"enabled":true,"username":"team","password":"open sesame","allow":["203.0.113.7"]}`
	const rules = `{"rules":[{"from":"/old","to":"/new"},{"from":"/blog/*","to":"https://example.com/","code":308}]}`

	// The owner locks the site and adds redirects.
	if c, body := e.raw("bob", "PUT", "/api/v1/sites/sb/lock", lock); c != 200 || !strings.Contains(body, `"site_lock":true`) ||
		!strings.Contains(body, `"site_lock_user":"team"`) || strings.Contains(body, "$2a$") || strings.Contains(body, "sesame") {
		t.Fatalf("owner locking: %d %s", c, body)
	}
	hash := px.site("sb").Lock.Hash
	if c, body := e.raw("bob", "PUT", "/api/v1/sites/sb/redirects", rules); c != 200 || !strings.Contains(body, `"code":301`) {
		t.Fatalf("owner's redirects: %d %s", c, body)
	}
	if got := px.site("sb").PathRedirects; len(got) != 2 {
		t.Fatalf("redirects reaching Caddy: %+v", got)
	}

	// Every way of reading the site, at every level: no hash, no password.
	for _, level := range []string{auth.AccessViewer, auth.AccessDeveloper, auth.AccessManager} {
		e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"`+level+`"}`, nil)
		for _, path := range []string{"/api/v1/sites/sb", "/api/v1/sites", "/api/v1/sites/sb/redirects",
			"/api/v1/sites/sb/events", "/api/v1/sites/sb/redirects/test?path=/old"} {
			c, body := e.raw("alice", "GET", path, "")
			if c != 200 || strings.Contains(body, hash) || strings.Contains(body, "$2a$") || strings.Contains(body, "sesame") {
				t.Errorf("%s GET %s: %d %s", level, path, c, body)
			}
		}
	}
	for _, who := range []string{"tok", "vic", "bob"} {
		if c, body := e.raw(who, "GET", "/api/v1/sites/sb", ""); c != 200 || strings.Contains(body, "$2a$") ||
			!strings.Contains(body, `"site_lock":true`) {
			t.Errorf("%s reading the site: %d %s", who, c, body)
		}
	}

	// Viewers (and developers) read redirects and test them, change nothing.
	for _, level := range []string{auth.AccessViewer, auth.AccessDeveloper} {
		e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"`+level+`"}`, nil)
		var list struct {
			Rules []struct{ From, To string }
			Max   int
		}
		if c := e.as("alice", "GET", "/api/v1/sites/sb/redirects", "", &list); c != 200 || len(list.Rules) != 2 || list.Max != site.MaxRedirects {
			t.Errorf("%s reading redirects: %d %+v", level, c, list)
		}
		var m site.RedirectMatch
		if c := e.as("alice", "GET", "/api/v1/sites/sb/redirects/test?path=/blog/2020/x%3Fy", "", &m); c != 200 ||
			m.Rule != 1 || m.Location != "https://example.com/" {
			t.Errorf("%s testing a path: %d %+v", level, c, m)
		}
		for _, c := range []struct{ path, body string }{
			{"/api/v1/sites/sb/lock", `{"enabled":false}`},
			{"/api/v1/sites/sb/redirects", `{"rules":[]}`},
		} {
			if got, body := e.raw("alice", "PUT", c.path, c.body); got != 403 || !strings.Contains(body, "manager access") {
				t.Errorf("%s PUT %s: %d %s", level, c.path, got, body)
			}
		}
	}
	if st, _ := e.st.GetSite(ctx, "sb"); !st.Lock || st.LockHash != hash {
		t.Fatal("a refused change went through")
	}
	// Managers may.
	e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"manager"}`, nil)
	if c, body := e.raw("alice", "PUT", "/api/v1/sites/sb/redirects", `{"rules":[{"from":"/a","to":"/b"}]}`); c != 200 {
		t.Fatalf("manager's redirects: %d %s", c, body)
	}
	if c, body := e.raw("alice", "PUT", "/api/v1/sites/sb/lock", `{"enabled":false}`); c != 200 || !strings.Contains(body, `"site_lock":false`) {
		t.Fatalf("manager unlocking: %d %s", c, body)
	}
	if px.site("sb").Lock != nil {
		t.Fatal("unlocked, Caddy still asks for a password")
	}

	// Staff: viewers read, operators (and the token) change.
	if c, _ := e.raw("vic", "GET", "/api/v1/sites/sb/redirects", ""); c != 200 {
		t.Errorf("staff viewer reading redirects: %d", c)
	}
	if c, _ := e.raw("vic", "PUT", "/api/v1/sites/sb/lock", lock); c != 403 {
		t.Errorf("staff viewer locking: %d", c)
	}
	if c, _ := e.raw("tok", "PUT", "/api/v1/sites/sb/lock", lock); c != 200 {
		t.Errorf("administrator locking: %d", c)
	}

	// Bad input is a 400 that says what's wrong; other people's sites 404.
	for _, c := range []struct{ path, body, msg string }{
		{"/api/v1/sites/sb/lock", `{"enabled":true,"password":"short"}`, "at least 8"},
		{"/api/v1/sites/sb/redirects", `{"rules":[{"from":"/a b","to":"/b"}]}`, "spaces"},
		{"/api/v1/sites/sb/redirects", `{"rules":[{"from":"/a","to":"https://e.com/{uri}"}]}`, "can't contain"},
		{"/api/v1/sites/sb/redirects", `{"rules":[{"from":"/a","to":"/b","code":200}]}`, "301, 302, 307 or 308"},
	} {
		if got, body := e.raw("bob", "PUT", c.path, c.body); got != 400 || !strings.Contains(body, c.msg) {
			t.Errorf("PUT %s %s: %d %s", c.path, c.body, got, body)
		}
	}
	for _, path := range []string{"/api/v1/sites/sa/redirects", "/api/v1/sites/sa/redirects/test?path=/"} {
		if c, _ := e.raw("bob", "GET", path, ""); c != 404 {
			t.Errorf("someone else's site, GET %s: %d", path, c)
		}
	}
	if c, _ := e.raw("bob", "PUT", "/api/v1/sites/sa/lock", lock); c != 404 {
		t.Errorf("someone else's site locked: %d", c)
	}
}

// Locking a staging copy as it is made is deciding who reaches it: manager
// access, like the lock itself.
func TestStagingLockNeedsManager(t *testing.T) {
	e := newTenancyEnv(t)
	e.share("bob", "sb", "alice", auth.AccessDeveloper)
	body := `{"domain":"staging.sb.test","lock":{"enabled":true,"username":"p","password":"letmesee1"}}`
	if c, out := e.raw("alice", "POST", "/api/v1/sites/sb/staging", body); c != 403 || !strings.Contains(out, "manager access") {
		t.Fatalf("developer locking a copy: %d %s", c, out)
	}
}
