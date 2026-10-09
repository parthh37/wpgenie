package api

import (
	"context"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/auth"
)

// TestToolsNeedTheirLevels: a site's Tools follow the sharing levels (the
// PHP error log needs developer access, like the site's files; every change
// needs developer) and staff roles (viewers read, operators change).
// Authorisation runs before the handler reads the body, so a malformed or
// invalid one answers 400 once the level is enough and 403 before.
func TestToolsNeedTheirLevels(t *testing.T) {
	e := newTenancyEnv(t)
	e.share("bob", "sb", "alice", auth.AccessViewer)
	set := func(level string) {
		if c := e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"`+level+`"}`, nil); c != 200 {
			t.Fatalf("setting %s: %d", level, c)
		}
	}
	base := "/api/v1/sites/sb/tools"
	changes := []struct{ method, path, body string }{
		{"PUT", base + "/maintenance", `{"on":true,"message":"` + strings.Repeat("x", 400) + `"}`},
		{"PUT", base + "/debug", "not json"},
		{"POST", base + "/search-replace", `{"search":"ab","replace":"cd"}`},
		{"POST", base + "/search-replace", `{"search":"--dry-run","replace":"x","dry_run":true}`},
		{"POST", base + "/cron/run", `{"hook":"x","time":1,"sig":"nope"}`},
		{"POST", base + "/themes", `{"slug":"--activate"}`},
		{"POST", base + "/themes", `{"slug":"../../x"}`},
		{"PUT", base + "/settings", `{"timezone":"Not/A Zone"}`},
		{"PUT", base + "/settings", `{"title":"line\nbreak"}`},
		{"PUT", base + "/settings", `{"admin_email":"x@y.test"}`}, // not an allowed setting
	}
	for _, c := range changes {
		if got := e.as("alice", c.method, c.path, c.body, nil); got != 403 {
			t.Errorf("viewer: %s %s = %d, want 403", c.method, c.path, got)
		}
	}
	if got := e.as("alice", "GET", base+"/debug/log", "", nil); got != 403 {
		t.Errorf("viewer reading the PHP error log: %d", got)
	}
	for _, p := range []string{"DELETE " + base + "/debug/log", "DELETE " + base + "/themes/old", "POST " + base + "/themes/old/activate"} {
		method, path, _ := strings.Cut(p, " ")
		if got := e.as("alice", method, path, "", nil); got != 403 {
			t.Errorf("viewer: %s = %d, want 403", p, got)
		}
	}

	set(auth.AccessDeveloper)
	for _, c := range changes {
		var out map[string]string
		if got := e.as("alice", c.method, c.path, c.body, &out); got != 400 {
			t.Errorf("developer: %s %s %s = %d %v, want 400", c.method, c.path, c.body, got, out)
		}
	}
	// No log on this (Docker-less) site yet: empty, not an error.
	var log struct {
		Lines []string `json:"lines"`
	}
	if got := e.as("alice", "GET", base+"/debug/log", "", &log); got != 200 || log.Lines == nil || len(log.Lines) != 0 {
		t.Errorf("developer reading the PHP error log: %d %+v", got, log)
	}

	// Not for other tenants' sites at all.
	for _, path := range []string{"/api/v1/sites/sa/tools/maintenance", "/api/v1/sites/sa/tools/themes"} {
		if got := e.as("bob", "GET", path, "", nil); got != 404 {
			t.Errorf("bob: GET %s = %d, want 404", path, got)
		}
	}
	if got := e.as("bob", "PUT", "/api/v1/sites/sa/tools/maintenance", `{"on":true}`, nil); got != 404 {
		t.Errorf("bob changing alice's maintenance mode: %d", got)
	}
}

func TestToolsStaffRoles(t *testing.T) {
	e := newTenancyEnv(t)
	ctx := context.Background()
	for _, role := range []string{auth.RoleViewer, auth.RoleOperator} {
		u, err := e.st.CreateUser(ctx, "staff-"+role, "x", role)
		if err != nil {
			t.Fatal(err)
		}
		e.user[u.Username] = u
		e.tokens[u.Username] = e.token(u.Username)
	}
	base := "/api/v1/sites/sx/tools"
	for _, c := range []struct{ method, path, body string }{
		{"PUT", base + "/maintenance", "not json"},
		{"PUT", base + "/debug", "not json"},
		{"POST", base + "/search-replace", "not json"},
		{"POST", base + "/themes", "not json"},
		{"PUT", base + "/settings", "not json"},
		{"GET", base + "/debug/log", ""},
	} {
		if got := e.as("staff-viewer", c.method, c.path, c.body, nil); got != 403 {
			t.Errorf("staff viewer: %s %s = %d, want 403", c.method, c.path, got)
		}
		want := 400
		if c.method == "GET" {
			want = 200
		}
		if got := e.as("staff-operator", c.method, c.path, c.body, nil); got != want {
			t.Errorf("staff operator: %s %s = %d, want %d", c.method, c.path, got, want)
		}
	}
}
