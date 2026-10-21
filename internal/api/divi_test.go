package api

import (
	"context"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/site"
)

const testDiviKey = "0123456789abcdefABCDEF0123456789abcdef99"

// TestDiviRoutes: administrators set the license and see its username and
// the key's last characters, never the key; everyone else (staff below
// admin, tenants) only learns whether new sites get Divi and can't change
// it. Installing on a site follows the site's rules.
func TestDiviRoutes(t *testing.T) {
	e := newTenancyEnv(t)
	// The install jobs started below must never reach Elegant Themes.
	e.api.Sites.DiviAPI = "http://127.0.0.1:1/api_downloads.php"
	ctx := context.Background()
	for _, role := range []string{auth.RoleViewer, auth.RoleOperator, auth.RoleAdmin} {
		u, err := e.st.CreateUser(ctx, "staff-"+role, "x", role)
		if err != nil {
			t.Fatal(err)
		}
		e.user[u.Username] = u
		e.tokens[u.Username] = e.token(u.Username)
	}
	set := `{"username":"acme@example.com","api_key":"` + testDiviKey + `"}`

	// Only administrators change or check it.
	for _, who := range []string{"staff-viewer", "staff-operator", "alice", "rita", "session:carl"} {
		if c := e.as(who, "PUT", "/api/v1/settings/divi", set, nil); c != 403 {
			t.Errorf("%s set the license: %d", who, c)
		}
		if c := e.as(who, "POST", "/api/v1/settings/divi/check", "", nil); c != 403 {
			t.Errorf("%s checked the license: %d", who, c)
		}
	}
	var v site.DiviView
	if c := e.as("staff-admin", "PUT", "/api/v1/settings/divi", `{"username":"acme@example.com","api_key":"bad'key"}`, nil); c != 400 {
		t.Errorf("unsafe key accepted: %d", c)
	}
	if c := e.as("staff-admin", "PUT", "/api/v1/settings/divi", set, &v); c != 200 || !v.Configured || !v.NewSites ||
		v.Username != "acme@example.com" || !v.KeySet || v.KeyHint != "…ef99" {
		t.Fatalf("set: %d %+v", c, v)
	}

	// Reading: the key never; the account only for administrators.
	raw := func(who string) string {
		var m map[string]any
		if c := e.as(who, "GET", "/api/v1/settings/divi", "", &m); c != 200 {
			t.Fatalf("%s reading: %d", who, c)
		}
		var b strings.Builder
		for k, val := range m {
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(toString(val), "\n", ""), " ", "")))
			b.WriteString(";")
		}
		return b.String()
	}
	for _, who := range []string{"tok", "staff-admin", "staff-viewer", "staff-operator", "alice", "rita", "session:carl"} {
		got := raw(who)
		if strings.Contains(got, testDiviKey) {
			t.Errorf("%s sees the key: %s", who, got)
		}
		admin := who == "tok" || who == "staff-admin"
		if admin != strings.Contains(got, "acme@example.com") || admin != strings.Contains(got, "key_hint") {
			t.Errorf("%s sees %s", who, got)
		}
		if !strings.Contains(got, "configured=true") || !strings.Contains(got, "new_sites=true") {
			t.Errorf("%s doesn't learn whether new sites get Divi: %s", who, got)
		}
	}

	// Installing on a site: operators, and tenants on their own sites (no
	// Docker here: the site's PHP isn't running, so the job is queued and
	// fails later; what matters is who may start it).
	if c := e.as("staff-viewer", "POST", "/api/v1/sites/sx/divi", "", nil); c != 403 {
		t.Errorf("viewer installed Divi: %d", c)
	}
	if c := e.as("staff-operator", "POST", "/api/v1/sites/sx/divi", "", nil); c != 202 {
		t.Errorf("operator installing Divi: %d", c)
	}
	if c := e.as("alice", "POST", "/api/v1/sites/sa/divi", "", nil); c != 202 {
		t.Errorf("owner installing Divi: %d", c)
	}
	if c := e.as("alice", "POST", "/api/v1/sites/sb/divi", "", nil); c != 404 {
		t.Errorf("alice on bob's site: %d", c)
	}
	// Shared at viewer level: no; developer: yes.
	e.share("bob", "sb", "alice", auth.AccessViewer)
	if c := e.as("alice", "POST", "/api/v1/sites/sb/divi", "", nil); c != 403 {
		t.Errorf("viewer access installing Divi: %d", c)
	}

	// Removing it: everyone sees it's gone.
	if c := e.as("staff-admin", "PUT", "/api/v1/settings/divi", `{"username":"","api_key":""}`, &v); c != 200 || v.Configured || v.KeySet {
		t.Fatalf("clear: %d %+v", c, v)
	}
	if got := raw("alice"); !strings.Contains(got, "configured=false") {
		t.Errorf("after clearing: %s", got)
	}
	if c := e.as("staff-operator", "POST", "/api/v1/sites/sx/divi", "", nil); c != 400 {
		t.Errorf("installing without a license: %d", c)
	}

	// The audit log records the changes, never the key.
	entries, err := e.st.Audit(ctx, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, a := range entries {
		if strings.Contains(a.Action+a.Target+a.Detail, testDiviKey) || strings.Contains(a.Detail, "bad'key") {
			t.Errorf("audit entry shows the key: %+v", a)
		}
		seen = seen || strings.Contains(a.Action, "/settings/divi")
	}
	if !seen {
		t.Error("license changes weren't audited")
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}
