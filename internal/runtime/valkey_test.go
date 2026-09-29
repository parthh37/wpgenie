package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderACL(t *testing.T) {
	b, err := RenderACL("adminpw", []ACLUser{{Name: "wpg_sabc1234", Password: "p1", Prefix: "sabc1234:"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"user default off resetkeys resetchannels -@all\n",
		"user wpgenie on #" + hashPassword("adminpw") + " ~* &* +@all\n",
		"user wpg_sabc1234 on #" + hashPassword("p1") + " resetkeys ~sabc1234:* resetchannels +@all -@dangerous -@admin +info -function -script|flush -script|kill\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "p1") || strings.Contains(s, "adminpw") {
		t.Error("a password in clear")
	}
	for _, bad := range []ACLUser{
		{Name: "wpg_x", Password: "p", Prefix: "*"},          // every key
		{Name: "wpg_x", Password: "p", Prefix: "a:* ~b:"},    // a second pattern
		{Name: "wpg x", Password: "p", Prefix: "a:"},         // a second word
		{Name: "default", Password: "p", Prefix: "a:"},       // the default user
		{Name: "wpg_x", Password: "", Prefix: "a:"},          // no password
		{Name: "wpg_x\nuser y", Password: "p", Prefix: "a:"}, // a second line
	} {
		if _, err := RenderACL("adminpw", []ACLUser{bad}); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// TestValkeyACLIsolatesSites runs Valkey as deploy/docker-compose.yml does
// and checks what a site's user can and can't do.
func TestValkeyACLIsolatesSites(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	d := &Docker{}
	name := "wpgt-valkey-acl"
	d.Run(ctx, nil, "rm", "-f", name)
	// The compose command: without users.acl Valkey starts open.
	if _, err := d.Run(ctx, nil, "run", "-d", "--name", name, "-v", dir+":/etc/valkey:ro", "valkey/valkey:8-alpine",
		"sh", "-c", `exec valkey-server --save "" --appendonly no $([ -f /etc/valkey/users.acl ] && echo --aclfile /etc/valkey/users.acl)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Run(context.Background(), nil, "rm", "-f", name) })
	admin := "admin-secret"
	v := &Valkey{Docker: *d, Container: name}
	waitUp := func() {
		for i := 0; ; i++ {
			out, _ := d.Run(ctx, nil, "exec", name, "valkey-cli", "PING")
			if strings.Contains(string(out), "PONG") || strings.Contains(string(out), "NOAUTH") {
				return
			}
			if i > 50 {
				t.Fatal("valkey didn't start")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	waitUp()
	if err := v.LoadACL(ctx); err != ErrNoACLFile {
		t.Fatalf("LoadACL without a file: %v", err)
	}

	b, err := RenderACL(admin, []ACLUser{
		{Name: "wpg_sa", Password: "pa", Prefix: "sa:"},
		{Name: "wpg_sb", Password: "pb", Prefix: "sb:"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.acl"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	waitUp()
	v.AdminPassword = func() string { return admin }
	if err := v.LoadACL(ctx); err != nil {
		t.Fatalf("LoadACL: %v", err)
	}
	as := func(user, pass string, args ...string) string {
		full := append([]string{"exec", "-e", "REDISCLI_AUTH=" + pass, name, "valkey-cli", "--user", user}, args...)
		out, _ := d.Run(ctx, nil, full...)
		return strings.TrimSpace(string(out))
	}
	// Anonymous access is off.
	if out, _ := d.Run(ctx, nil, "exec", name, "valkey-cli", "GET", "sa:x"); !strings.Contains(string(out), "NOAUTH") {
		t.Fatalf("anonymous GET: %s", out)
	}
	if out := as("wpg_sa", "wrong", "PING"); strings.Contains(out, "PONG") {
		t.Fatal("wrong password accepted")
	}
	// Own keys: everything the object cache does.
	if out := as("wpg_sa", "pa", "SET", "sa:options:alloptions", "x"); out != "OK" {
		t.Fatalf("own SET: %s", out)
	}
	if out := as("wpg_sb", "pb", "SET", "sb:options:alloptions", "y"); out != "OK" {
		t.Fatalf("own SET: %s", out)
	}
	if out := as("wpg_sa", "pa", "GET", "sa:options:alloptions"); out != "x" {
		t.Fatalf("own GET: %s", out)
	}
	if out := as("wpg_sa", "pa", "INFO", "server"); !strings.Contains(out, "valkey_version") && !strings.Contains(out, "redis_version") {
		t.Fatalf("INFO (the drop-in reads the version): %.100s", out)
	}
	// Another site's keys, and server-wide commands: refused.
	for _, c := range [][]string{
		{"GET", "sb:options:alloptions"}, {"SET", "sb:options:alloptions", "pwned"}, {"DEL", "sb:options:alloptions"},
		{"FLUSHALL"}, {"FLUSHDB"}, {"KEYS", "*"}, {"CONFIG", "GET", "*"}, {"CLIENT", "LIST"}, {"ACL", "LIST"},
		{"MONITOR"}, {"DEBUG", "SLEEP", "0"}, {"FUNCTION", "FLUSH"}, {"SCRIPT", "FLUSH"}, {"PUBLISH", "c", "m"},
		{"EVAL", "return redis.call('GET', 'sb:options:alloptions')", "0"},
	} {
		// Refused (NOPERM, or a script's ACL failure, or DEBUG being off
		// server-wide), and never site B's value.
		out := as("wpg_sa", "pa", c...)
		if !strings.Contains(out, "NOPERM") && !strings.Contains(out, "No permissions") && !strings.Contains(out, "not allowed") ||
			strings.Contains(out, "\"y\"") || out == "y" {
			t.Errorf("%v as another site: %s", c, out)
		}
	}
	if out := as("wpg_sb", "pb", "GET", "sb:options:alloptions"); out != "y" {
		t.Fatalf("site B's key after site A's attempts: %s", out)
	}
	// The drop-in's selective flush (Lua: SCAN MATCH own prefix, DEL) works.
	flush := `local cur = 0 local i = 0 local tmp repeat tmp = redis.call('SCAN', cur, 'MATCH', 'sa:*') cur = tonumber(tmp[1]) if tmp[2] then for _, v in pairs(tmp[2]) do redis.call('del', v) i = i + 1 end end until 0 == cur return i`
	if out := as("wpg_sa", "pa", "EVAL", flush, "0"); out != "1" {
		t.Fatalf("the drop-in's flush: %s", out)
	}
	// The daemon's own flush, as the admin user.
	if err := v.FlushPrefix(ctx, "sb:"); err != nil {
		t.Fatal(err)
	}
	if out := as("wpg_sb", "pb", "EXISTS", "sb:options:alloptions"); out != "0" {
		t.Fatalf("after the daemon's flush: %s", out)
	}
}
