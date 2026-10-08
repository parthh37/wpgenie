package api

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/runtime"
)

// execOnly is a runtime whose replicas only run commands (other calls
// would panic: hardening never makes them).
type execOnly struct {
	runtime.Runtime
	calls []string
}

func (e *execOnly) Exec(_ context.Context, _ string, _ io.Reader, _ io.Writer, args ...string) error {
	e.calls = append(e.calls, strings.Join(args, " "))
	return nil
}

// TestHardeningNeedsManagerAccess: someone a site is shared with can read
// the catalogue, but changing the site's hardening or signing everyone
// out is a protection change: managers only.
func TestHardeningNeedsManagerAccess(t *testing.T) {
	e := newTenancyEnv(t)
	e.share("bob", "sb", "alice", auth.AccessViewer)
	set := func(level string) { e.as("bob", "PUT", e.grantPath("sb", "alice"), `{"access":"`+level+`"}`, nil) }

	var catalogue []map[string]any
	if c := e.as("alice", "GET", "/api/v1/hardening", "", &catalogue); c != 200 || len(catalogue) != 8 {
		t.Fatalf("catalogue: %d %v", c, catalogue)
	}
	put := "/api/v1/sites/sb/hardening"
	signOut := "/api/v1/sites/sb/hardening/sign-out"
	for _, level := range []string{auth.AccessViewer, auth.AccessDeveloper} {
		set(level)
		for _, p := range []string{put, signOut} {
			method := "POST"
			if p == put {
				method = "PUT"
			}
			var out map[string]string
			if c := e.as("alice", method, p, `{"hardening":["user_enum"]}`, &out); c != 403 || !strings.Contains(out["error"], "access to the site") {
				t.Errorf("%s: %s %s = %d %v", level, method, p, c, out)
			}
		}
	}

	set(auth.AccessManager)
	// Past the access check, the body is read: a bad one is the caller's.
	if c := e.as("alice", "PUT", put, "not json", nil); c != 400 {
		t.Errorf("manager, malformed body: %d", c)
	}
	if c := e.as("alice", "PUT", put, `{"hardening":["no_such_thing"]}`, nil); c != 400 {
		t.Errorf("manager, unknown setting: %d", c)
	}

	// And signing everyone out works: new salts in wp-config.php, sessions
	// forgotten through WP-CLI.
	svc := e.api.Sites
	svc.Cfg.DataDir = t.TempDir()
	rt := &execOnly{}
	svc.Runtime = rt
	dir := svc.Cfg.SiteDir("sb")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := "<?php\n" +
		"define( 'AUTH_KEY', 'a' );\ndefine( 'SECURE_AUTH_KEY', 'a' );\ndefine( 'LOGGED_IN_KEY', 'a' );\ndefine( 'NONCE_KEY', 'a' );\n" +
		"define( 'AUTH_SALT', 'a' );\ndefine( 'SECURE_AUTH_SALT', 'a' );\ndefine( 'LOGGED_IN_SALT', 'a' );\ndefine( 'NONCE_SALT', 'a' );\n" +
		"// --- end WPGenie ---\nrequire_once ABSPATH . 'wp-settings.php';\n"
	path := filepath.Join(dir, "wp-config.php")
	if err := os.WriteFile(path, []byte(cfg), 0o640); err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if c := e.as("alice", "POST", signOut, "", &out); c != 200 || out["status"] != "signed_out" {
		t.Fatalf("manager signing everyone out: %d %v", c, out)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "'AUTH_KEY', 'a'") || !strings.HasSuffix(string(got), "// --- end WPGenie ---\nrequire_once ABSPATH . 'wp-settings.php';\n") {
		t.Errorf("wp-config.php after signing out:\n%s", got)
	}
	if len(rt.calls) != 1 || !strings.Contains(rt.calls[0], "destroy_all_for_all_users") {
		t.Errorf("WP-CLI calls %v", rt.calls)
	}
}
