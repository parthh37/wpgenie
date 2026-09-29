package sftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
)

func TestUsername(t *testing.T) {
	if n, err := Username("sabc1234", ""); err != nil || n != "sabc1234" {
		t.Fatal(n, err)
	}
	if n, err := Username("sabc1234", "dev"); err != nil || n != "sabc1234-dev" {
		t.Fatal(n, err)
	}
	for _, bad := range [][2]string{{"root", ""}, {"sabc1234", "Dev"}, {"sabc1234", "a:b"}, {"s1", strings.Repeat("a", 17)}} {
		if _, err := Username(bad[0], bad[1]); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestNormalizeKeys(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	got, err := NormalizeKeys([]string{line + " me@laptop\n\n# comment\n" + line + " dup"})
	if err != nil || len(got) != 1 || got[0] != line+" me@laptop" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{
		`command="/bin/sh" ` + line, // options would change what the login can do
		`from="1.2.3.4" ` + line,
		"not a key",
		line + "\nssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7", // truncated
	} {
		if _, err := NormalizeKeys([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRenderAccounts(t *testing.T) {
	s := &Service{Cfg: Config{SitesDir: "/var/lib/wpgenie/sites"}}
	p, g, sh := s.render([]*store.SFTPUser{
		{Username: "s1", SiteID: "s1", Password: "$6$salt$hash"},
		{Username: "s1-dev", SiteID: "s1", PublicKeys: []string{"ssh-ed25519 AAAA"}},
	})
	if p != "s1:x:82:82:WPGenie site s1:/var/lib/wpgenie/sites/s1:/sbin/nologin\n"+
		"s1-dev:x:82:82:WPGenie site s1:/var/lib/wpgenie/sites/s1:/sbin/nologin\n" {
		t.Errorf("passwd:\n%s", p)
	}
	if g != "sftp:x:2000:s1,s1-dev\n" {
		t.Errorf("group %q", g)
	}
	if !strings.Contains(sh, "s1:$6$salt$hash:") || !strings.Contains(sh, "s1-dev:*:") {
		t.Errorf("shadow:\n%s", sh)
	}
}

// TestSFTPServer runs the real server image: password and key logins,
// the chroot, file ownership, and removed logins losing access.
func TestSFTPServer(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (site directories are root-owned)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	base := t.TempDir()
	// sshd chroots only into paths whose every component is root-owned and
	// not group/world-writable (true of /var/lib/wpgenie/sites/<id>; not of
	// a temporary directory under /tmp, mode 1777).
	for d := base; d != "/"; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && d != base &&
			(fi.Sys().(*syscall.Stat_t).Uid != 0 || fi.Mode().Perm()&0o022 != 0) {
			t.Fatalf("%s isn't root-owned or is group/world-writable: sshd refuses to chroot below it. "+
				"Point TMPDIR at a root-owned directory (make test-e2e does)", d)
		}
	}
	sites := filepath.Join(base, "sites")
	site := filepath.Join(sites, "sabc1234")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	os.Chmod(base, 0o755)
	os.Chown(site, 0, 82)
	os.Chmod(site, 0o751)
	os.Chown(filepath.Join(site, "public"), 82, 82)
	os.WriteFile(filepath.Join(site, "wp-config.php"), []byte("<?php // db password"), 0o640)
	os.Chown(filepath.Join(site, "wp-config.php"), 0, 82)

	st := storetest.Open(t)
	st.CreateSite(ctx, &store.Site{ID: "sabc1234", Name: "x", PrimaryDomain: "a.test", PHPVersion: "8.3", FPMPort: 19000,
		DBName: "wp_sabc1234", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1})
	svc := &Service{Store: st, Docker: &runtime.Docker{}, Log: slog.New(slog.DiscardHandler),
		Cfg: Config{DataDir: filepath.Join(base, "sftp"), SitesDir: sites, Image: "wpgenie/sftp:test",
			ImageDir: "../../images/sftp", Port: 22222}}
	defer exec.Command("docker", "rm", "-f", container).Run()

	_, pw, err := svc.Add(ctx, "sabc1234", UserInput{Password: true})
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	spk, _ := ssh.NewPublicKey(pub)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	keyFile := filepath.Join(base, "id")
	os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600)
	if _, _, err := svc.Add(ctx, "sabc1234", UserInput{Suffix: "dev", PublicKeys: []string{string(ssh.MarshalAuthorizedKey(spk))}}); err != nil {
		t.Fatal(err)
	}
	if info := svc.Info(ctx); !info.Running || len(info.HostKey) == 0 {
		t.Fatalf("info %+v", info)
	}

	client := func(script string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "container:"+container,
			"-v", keyFile+":/id:ro", "-e", "PW="+pw, "alpine:3.22", "sh", "-c",
			"apk add -q sshpass openssh-client >/dev/null 2>&1; cp /id /tmp/id; chmod 600 /tmp/id; export SSHPASS=\"$PW\"; "+script).CombinedOutput()
		return string(out), err
	}
	var out string
	for range 20 { // sshd starting
		if out, err = client(`printf 'ls\n' | sshpass -e sftp -o BatchMode=no -o StrictHostKeyChecking=no -P 2222 -b - sabc1234@127.0.0.1`); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
		sh, _ := exec.Command("docker", "exec", container, "sh", "-c", "tail -2 /etc/passwd /etc/group; ls -la /config /config/keys; ls -ld /var/lib/wpgenie/sites/sabc1234 "+site).CombinedOutput()
		t.Fatalf("password login: %v\n%s\n--- server ---\n%s\n%s", err, out, logs, sh)
	}
	out, err = client(`export SSHPASS="$PW"; printf 'pwd\nput /etc/hostname uploaded.txt\ncd ..\nls\nget wp-config.php /tmp/c\ncd /etc\n' | ` +
		`sshpass -e sftp -o BatchMode=no -o StrictHostKeyChecking=no -P 2222 -b - sabc1234@127.0.0.1; cat /tmp/c`)
	if !strings.Contains(out, "Remote working directory: /public") {
		t.Errorf("login doesn't start in the docroot:\n%s", out)
	}
	if !strings.Contains(out, "<?php // db password") {
		t.Errorf("the site's login can't read its own wp-config.php:\n%s", out)
	}
	if strings.Contains(out, "passwd") || !strings.Contains(out, `"/etc" not found`) && !strings.Contains(out, "No such file") {
		t.Errorf("the jail leaks the container's filesystem:\n%s", out)
	}
	fi, err := os.Stat(filepath.Join(site, "public", "uploaded.txt"))
	if err != nil {
		t.Fatalf("upload missing: %v\n%s", err, out)
	}
	if uid := fi.Sys().(*syscall.Stat_t).Uid; uid != 82 || fi.Mode().Perm() != 0o644 {
		t.Errorf("uploaded file uid %d mode %v, want 82 0644", uid, fi.Mode().Perm())
	}
	if out, err := client(`sshpass -e ssh -o StrictHostKeyChecking=no -p 2222 sabc1234@127.0.0.1 id`); err == nil ||
		strings.Contains(out, "uid=") {
		t.Errorf("got a shell:\n%s", out)
	}
	if out, err := client(`printf 'ls\n' | sftp -i /tmp/id -o StrictHostKeyChecking=no -P 2222 -b - sabc1234-dev@127.0.0.1`); err != nil {
		t.Errorf("key login: %v\n%s", err, out)
	}

	if err := svc.Delete(ctx, "sabc1234", "sabc1234"); err != nil {
		t.Fatal(err)
	}
	if out, err := client(`export SSHPASS="$PW"; printf 'ls\n' | sshpass -e sftp -o BatchMode=no -o StrictHostKeyChecking=no -P 2222 -b - sabc1234@127.0.0.1`); err == nil {
		t.Errorf("a deleted login still works:\n%s", out)
	}
	if err := svc.Delete(ctx, "sabc1234", "sabc1234-dev"); err != nil {
		t.Fatal(err)
	}
	if spec, _ := svc.containerSpec(ctx); spec != "" {
		t.Errorf("server still there without logins (%q)", spec)
	}
}
