package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	mountUnit   = "../../deploy/var-lib-wpgenie-sites.nosymfollow.mount"
	composeFile = "../../deploy/docker-compose.yml"
)

// mountOptions returns Options= from the systemd unit that gives Caddy its
// view of the site files, so the Docker test mounts exactly what production does.
func mountOptions(t *testing.T) string {
	t.Helper()
	f, err := os.Open(mountUnit)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Options="); ok {
			return v
		}
	}
	t.Fatalf("%s has no Options= line", mountUnit)
	return ""
}

// TestCaddySeesSitesWithoutSymlinks guards the deployment wiring the Docker
// test below can't see: Caddy must get the nosymfollow mount, never the raw
// sites directory, and must not run as root.
func TestCaddySeesSitesWithoutSymlinks(t *testing.T) {
	if opts := mountOptions(t); !strings.Contains(","+opts+",", ",nosymfollow,") || !strings.Contains(","+opts+",", ",ro,") {
		t.Errorf("%s: Options=%s must include ro and nosymfollow", mountUnit, opts)
	}
	b, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	compose := string(b)
	if !strings.Contains(compose, "- /var/lib/wpgenie/sites.nosymfollow:/var/lib/wpgenie/sites:ro") {
		t.Error("caddy must mount /var/lib/wpgenie/sites.nosymfollow, the symlink-free view of site files")
	}
	if strings.Contains(compose, "- /var/lib/wpgenie/sites:") {
		t.Error("caddy must not mount the raw sites directory: file_server follows symlinks planted by sites")
	}
	if !strings.Contains(compose, `user: "${CADDY_UID`) {
		t.Error("caddy must run as the unprivileged wpgenie-caddy user")
	}
}

// fixture builds two sites laid out like site.prepareFiles, plants symlinks
// in site A's uploads, mounts the tree as $MOUNT_OPTS and runs Caddy as
// $CADDY_UID. It runs in a privileged container because it calls mount(2);
// util-linux mount is what systemd uses (busybox silently drops ro and
// nosymfollow on bind mounts).
const fixture = `set -eu
S=/srv/sites
for id in sa sb; do
	mkdir -p $S/$id/public/wp-content/uploads $S/$id/public/wp-content/themes/t
	echo "<?php define('DB_PASSWORD', 'db-secret-$id');" >$S/$id/wp-config.php
	echo "static-$id" >$S/$id/public/wp-content/uploads/ok.txt
	echo "theme-$id" >$S/$id/public/wp-content/themes/t/style.css
	echo "backup-secret-$id" >$S/$id/public/wp-content/uploads/db-backup.sql
	chown -R 82:82 $S/$id/public
	chown 0:82 $S/$id $S/$id/wp-config.php
	chmod 0751 $S/$id
	chmod 0640 $S/$id/wp-config.php
done
# Caddy's certificate store (/data in the compose stack), owned by Caddy.
mkdir -p /data/caddy
echo tls-secret-key >/data/caddy/site.key
chown -R "$CADDY_UID" /data
chmod 0600 /data/caddy/site.key

U=$S/sa/public/wp-content/uploads
ln -s /var/lib/wpgenie/sites/sb/wp-config.php $U/config.txt
ln -s /var/lib/wpgenie/sites/sb/public/wp-content/uploads/db-backup.sql $U/peer.txt
ln -s /data/caddy/site.key $U/tls.txt

mkdir -p /var/lib/wpgenie/sites /etc/caddy
mount -o "$MOUNT_OPTS" $S /var/lib/wpgenie/sites
install -m 0755 /mnt/caddy /usr/local/bin/caddy
install -m 0644 /mnt/Caddyfile /etc/caddy/Caddyfile
export XDG_DATA_HOME=/data XDG_CONFIG_HOME=/tmp/config
exec setpriv --reuid="$CADDY_UID" --regid="$CADDY_UID" --clear-groups \
	caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
`

// TestStaticFilesDoNotFollowSymlinks is the regression test for cross-site
// file disclosure: a site that can create a symlink in its docroot (archive
// extraction, WP-CLI, ...) must not be able to make Caddy serve another
// site's wp-config.php, another site's private files, or Caddy's TLS keys.
// Needs Docker with privileged containers: WPGENIE_TEST_DOCKER=1.
func TestStaticFilesDoNotFollowSymlinks(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run against real Caddy via Docker")
	}
	dir := t.TempDir()
	copyCaddyBinary(t, dir)
	out, err := NewCaddy(Config{
		AdminURL: "http://127.0.0.1:2019", ShieldUpstream: "127.0.0.1:1", AccessLog: "/tmp/access.log",
	}).Render([]Site{
		// Plain HTTP so the test needs no certificates. PHP is unreachable
		// on purpose: static files must never depend on it.
		{ID: "sa", Name: "A", Domains: []string{"http://a.test:8080"}, Root: "/var/lib/wpgenie/sites/sa/public", FPMPort: 1},
		{ID: "sb", Name: "B", Domains: []string{"http://b.test:8080"}, Root: "/var/lib/wpgenie/sites/sb/public", FPMPort: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	leaks := map[string]string{
		"/wp-content/uploads/config.txt": "db-secret-sb",
		"/wp-content/uploads/peer.txt":   "backup-secret-sb",
		"/wp-content/uploads/tls.txt":    "tls-secret-key",
	}
	const caddyUID = "2019" // not root, not in www-data's group (82)

	t.Run("baseline: root Caddy on a plain read-only mount leaks", func(t *testing.T) {
		t.Parallel()
		base := startCaddy(t, dir, "bind,ro", "0")
		// Proves the fixture exercises the bug, so the passes below mean something.
		if code, body := get(t, base, "a.test", "/wp-content/uploads/config.txt"); code != 200 || !strings.Contains(body, "db-secret-sb") {
			t.Fatalf("fixture did not reproduce the leak: %d %q", code, body)
		}
	})

	t.Run("production: nosymfollow mount and unprivileged Caddy", func(t *testing.T) {
		t.Parallel()
		base := startCaddy(t, dir, mountOptions(t), caddyUID)
		for path, secret := range leaks {
			if code, body := get(t, base, "a.test", path); code == 200 || strings.Contains(body, secret) {
				t.Errorf("GET a.test%s = %d, leaked %q", path, code, body)
			}
		}
		// Regular files are still served, as the unprivileged user.
		for _, c := range []struct{ host, path, want string }{
			{"a.test", "/wp-content/uploads/ok.txt", "static-sa"},
			{"b.test", "/wp-content/uploads/ok.txt", "static-sb"},
			{"a.test", "/wp-content/themes/t/style.css", "theme-sa"},
		} {
			if code, body := get(t, base, c.host, c.path); code != 200 || strings.TrimSpace(body) != c.want {
				t.Errorf("GET %s%s = %d %q, want 200 %q", c.host, c.path, code, body, c.want)
			}
		}
	})

	t.Run("unprivileged Caddy alone still protects wp-config.php", func(t *testing.T) {
		t.Parallel()
		base := startCaddy(t, dir, "bind,ro", caddyUID)
		if code, body := get(t, base, "a.test", "/wp-content/uploads/config.txt"); code == 200 || strings.Contains(body, "db-secret-sb") {
			t.Errorf("wp-config.php readable by unprivileged Caddy: %d %q", code, body)
		}
	})
}

// copyCaddyBinary extracts the static caddy binary from the image the
// compose stack runs, so the fixture can use util-linux from Debian.
func copyCaddyBinary(t *testing.T, dir string) {
	t.Helper()
	id, err := exec.Command("docker", "create", "caddy:2-alpine").Output()
	if err != nil {
		t.Fatalf("docker create caddy:2-alpine: %v", err)
	}
	cid := strings.TrimSpace(string(id))
	defer exec.Command("docker", "rm", cid).Run()
	if b, err := exec.Command("docker", "cp", cid+":/usr/bin/caddy", filepath.Join(dir, "caddy")).CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, b)
	}
}

// startCaddy runs the fixture and returns Caddy's base URL once it serves.
func startCaddy(t *testing.T, dir, mountOpts, uid string) string {
	t.Helper()
	b, err := exec.Command("docker", "run", "-d", "--privileged",
		"-p", "127.0.0.1::8080", "-v", dir+":/mnt:ro",
		"-e", "MOUNT_OPTS="+mountOpts, "-e", "CADDY_UID="+uid,
		"debian:12-slim", "bash", "-c", fixture).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, b)
	}
	cid := strings.TrimSpace(string(b))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })

	var addr string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if a, err := exec.Command("docker", "port", cid, "8080/tcp").Output(); err == nil && len(a) > 0 {
			addr = strings.TrimSpace(strings.SplitN(string(a), "\n", 2)[0])
			if code, _ := tryGet("http://"+addr, "a.test", "/wp-content/uploads/ok.txt"); code == 200 {
				return "http://" + addr
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", cid).CombinedOutput()
	t.Fatalf("caddy did not come up (mount %q, uid %s):\n%s", mountOpts, uid, logs)
	return ""
}

func tryGet(base, host, path string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, err.Error()
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(body)
}

func get(t *testing.T, base, host, path string) (int, string) {
	t.Helper()
	code, body := tryGet(base, host, path)
	if code == 0 {
		t.Fatalf("GET %s%s: %s", host, path, body)
	}
	return code, body
}
