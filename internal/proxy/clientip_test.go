package proxy

import (
	"context"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestShieldSeesVisitorBehindCloudflare runs the rendered config in a real
// Caddy with a stand-in shield that echoes the X-Forwarded-For it receives.
// Behind a trusted edge, the shield must see CF-Connecting-IP and nothing a
// client forged; from anywhere else, CF-Connecting-IP must be ignored.
// Also checks static cache headers. Needs Docker: WPGENIE_TEST_DOCKER=1.
func TestShieldSeesVisitorBehindCloudflare(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run against real Caddy via Docker")
	}
	for _, c := range []struct {
		name    string
		trusted string // the test client's connections come from the Docker gateway
		edge    bool
	}{
		{"peer is the CDN edge", "0.0.0.0/0", true},
		{"peer is not the CDN edge", "203.0.113.0/24", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			base := startClientIPCaddy(t, netip.MustParsePrefix(c.trusted))
			req, _ := http.NewRequest(http.MethodGet, base+"/hello/", nil)
			req.Host = "a.test"
			req.Header.Set("CF-Connecting-IP", "198.51.100.7")
			req.Header.Set("X-Forwarded-For", "6.6.6.6")
			code, body := do(t, req)
			if code != http.StatusForbidden || strings.Contains(body, "6.6.6.6") {
				t.Fatalf("shield got %d %q: a forged X-Forwarded-For must never reach it", code, body)
			}
			if got := body == "XFF=[198.51.100.7]"; got != c.edge {
				t.Errorf("shield saw %q; CF-Connecting-IP must count exactly when the peer is the edge", body)
			}
		})
	}

	t.Run("static cache headers", func(t *testing.T) {
		t.Parallel()
		base := startClientIPCaddy(t, netip.MustParsePrefix("203.0.113.0/24"))
		for path, want := range map[string]string{
			"/wp-content/themes/t/style.css": "public, max-age=604800",
			"/wp-content/uploads/pic.png":    "public, max-age=2592000",
			"/wp-content/themes/t/gone.css":  "", // falls through to PHP: never cached
		} {
			req, _ := http.NewRequest(http.MethodGet, base+path, nil)
			req.Host = "a.test"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if got := resp.Header.Get("Cache-Control"); got != want {
				t.Errorf("GET %s: %d, Cache-Control %q, want %q", path, resp.StatusCode, got, want)
			}
		}
	})
}

func startClientIPCaddy(t *testing.T, trusted netip.Prefix) string {
	t.Helper()
	return startSiteCaddy(t, trusted, Site{ID: "sa", Name: "A", ShieldEnabled: true}, map[string]string{
		"wp-content/themes/t/style.css": "body{}",
		"wp-content/uploads/pic.png":    "png",
	}, "/wp-content/uploads/pic.png")
}

// startSiteCaddy runs site (served as http://a.test:8080 from /srv/public, with
// PHP on a dead upstream) in a real Caddy, with files under the docroot and
// a stand-in shield on :8088 that answers 403 with the X-Forwarded-For it
// got. It returns the base URL once ready (a GET of readyPath gives 200).
func startSiteCaddy(t *testing.T, trusted netip.Prefix, site Site, files map[string]string, readyPath string) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "public")
	for name, body := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	site.Domains, site.Root, site.Upstreams = []string{"http://a.test:8080"}, "/srv/public", []string{"127.0.0.1:1"}
	out, err := NewCaddy(Config{
		AdminURL: "http://127.0.0.1:2019", ShieldUpstream: "127.0.0.1:8088", AccessLog: "/tmp/access.log",
		CloudflareRanges: func() []netip.Prefix { return []netip.Prefix{trusted} },
	}).Render([]Site{site})
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in shield: forward_auth hands a non-2xx straight to the client.
	out = append(out, "\n:8088 {\n\trespond \"XFF=[{header.X-Forwarded-For}]\" 403\n}\n"...)
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd := exec.Command("docker", "run", "-d", "-p", "127.0.0.1::8080", "-v", dir+":/srv:ro",
		"caddy:2-alpine", "caddy", "run", "--config", "/srv/Caddyfile", "--adapter", "caddyfile")
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, stderr.String())
	}
	cid := strings.TrimSpace(string(b))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if a, err := exec.Command("docker", "port", cid, "8080/tcp").Output(); err == nil && len(a) > 0 {
			base := "http://" + strings.TrimSpace(strings.SplitN(string(a), "\n", 2)[0])
			if code, _ := tryGet(base, "a.test", readyPath); code == 200 {
				return base
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", cid).CombinedOutput()
	t.Fatalf("caddy did not come up:\n%s", logs)
	return ""
}

func do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	sb.Write(buf[:n])
	return resp.StatusCode, strings.TrimSpace(sb.String())
}
