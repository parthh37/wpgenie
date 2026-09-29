package proxy

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/offload/offloadtest"
)

var offloadSite = Site{ID: "soff0001", Name: "Offloaded", Domains: []string{"media.test"},
	Root: "/var/lib/wpgenie/sites/soff0001/public", Upstreams: []string{"127.0.0.1:19000"},
	ShieldEnabled: true, PageCache: true, Images: []string{"avif"},
	Offload: "https://bucket.s3.eu-central-1.amazonaws.com/soff0001/uploads"}

func TestRenderOffload(t *testing.T) {
	plain := perfSite
	plain.ID, plain.Domains = "splain01", []string{"plain.test"}
	out, err := testCaddy().Render([]Site{offloadSite, plain})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	site := s[strings.Index(s, "media.test {"):strings.Index(s, "plain.test {")]
	for _, want := range []string{
		"reverse_proxy https://bucket.s3.eu-central-1.amazonaws.com:443 {",
		"header_up Host bucket.s3.eu-central-1.amazonaws.com",
		"tls_server_name bucket.s3.eu-central-1.amazonaws.com",
		"rewrite * /soff0001/uploads{path}?",
		"header_up -Cookie",
		"header_up -Authorization",
		"header_down -Set-Cookie",
		"method GET HEAD",
		"not file",
	} {
		if !strings.Contains(site, want) {
			t.Errorf("missing %q", want)
		}
	}
	matcher := site[strings.Index(site, "@wpg_offload {"):]
	matcher = matcher[:strings.Index(matcher, "}")]
	if !strings.Contains(matcher, "path /wp-content/uploads/*") || !strings.Contains(matcher, `not path_regexp (?i)(?:\.php|/\.)`) {
		t.Errorf("the fallback must be limited to uploads, never PHP or dot paths:\n%s", matcher)
	}
	// Local files win: the fallback comes after the shield and the hardening
	// rules and the format negotiation, just before PHP.
	at := strings.Index(site, "route @wpg_offload {")
	for _, before := range []string{"forward_auth @wpg_dynamic", "respond @wpg_forbidden 404", "rewrite @wpg_img_avif"} {
		if i := strings.Index(site, before); i < 0 || i > at {
			t.Errorf("%q must come before the offload fallback", before)
		}
	}
	if at > strings.Index(site, "php_fastcgi") {
		t.Error("the offload fallback must come before PHP")
	}
	if strings.Contains(s[strings.Index(s, "plain.test {"):], "wpg_offload") {
		t.Error("offload fallback on a site without offload")
	}
	adapt(t, out)

	for _, bad := range []string{
		"ftp://bucket.example/x", "https://bucket.example/x?acl", "https://user:pw@bucket.example/x",
		"https://bucket.example/x y", "https://bucket.example/a/../b", "https://bucket.example/a%2Fb",
		"https://bucket.example/x\n}", "https://{bucket}.example/x", "https://bucket.example/x#f", "//bucket.example/x",
	} {
		o := offloadSite
		o.Offload = bad
		if _, err := testCaddy().Render([]Site{o}); err == nil {
			t.Errorf("offload URL %q rendered", bad)
		}
	}
	for raw, want := range map[string]offloadTarget{
		"https://cdn.example.com":         {Origin: "https://cdn.example.com:443", Host: "cdn.example.com", TLS: true, ServerName: "cdn.example.com"},
		"https://S3.example.com:8443/b/p": {Origin: "https://s3.example.com:8443", Host: "s3.example.com:8443", Path: "/b/p", TLS: true, ServerName: "s3.example.com"},
		"http://minio:9000/media/s1":      {Origin: "http://minio:9000", Host: "minio:9000", Path: "/media/s1", ServerName: "minio"},
	} {
		if got, err := parseOffload(raw); err != nil || got != want {
			t.Errorf("parseOffload(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
}

// TestOffloadFallbackInRealCaddy serves uploads missing on disk from a real
// MinIO through a real Caddy, and checks what reaches the storage with a
// stand-in that echoes the request. Needs Docker: WPGENIE_TEST_DOCKER=1.
func TestOffloadFallbackInRealCaddy(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run against real Caddy and MinIO via Docker")
	}
	m := offloadtest.Start(t, "s1/uploads/")
	m.Put(t, "s1/uploads/2024/01/remote.jpg", "remote-jpg")
	m.Put(t, "s1/uploads/2024/01/sp ace é.png", "spaced")
	m.Put(t, "s1/uploads/2024/01/local.jpg", "stale-remote-copy")
	m.Put(t, "s1/uploads/shell.php", "<?php remote")

	dir := t.TempDir()
	for name, body := range map[string]string{
		"public/wp-content/uploads/2024/01/local.jpg": "local-jpg",
		"public/index.php":                            "<?php",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	site := Site{ID: "s1", Name: "S1", Domains: []string{"http://a.test:8080"}, Root: "/srv/public",
		Upstreams: []string{"127.0.0.1:1"}, Offload: m.Endpoint + "/" + m.Bucket + "/s1/uploads"}
	echo := Site{ID: "s2", Name: "S2", Domains: []string{"http://b.test:8080"}, Root: "/srv/public",
		Upstreams: []string{"127.0.0.1:1"}, Offload: "http://127.0.0.1:9090/store/s2"}
	out, err := NewCaddy(Config{AdminURL: "http://127.0.0.1:2019", ShieldUpstream: "127.0.0.1:8088",
		AccessLog: "/tmp/access.log"}).Render([]Site{site, echo})
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in storage answers with what it received.
	out = append(out, "\n:9090 {\n\theader Set-Cookie \"storage=1\"\n\theader X-Amz-Request-Id abc\n"+
		"\trespond \"{method} {http.request.hostport} {uri} cookie=[{header.Cookie}] auth=[{header.Authorization}] xff=[{header.X-Forwarded-For}] ref=[{header.Referer}]\"\n}\n"...)
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command("docker", "run", "-d", "--network", m.Network, "-p", "127.0.0.1::8080", "-v", dir+":/srv:ro",
		"caddy:2-alpine", "caddy", "run", "--config", "/srv/Caddyfile", "--adapter", "caddyfile").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run caddy: %v\n%s", err, b)
	}
	cid := strings.TrimSpace(string(b))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
	var base string
	for deadline := time.Now().Add(30 * time.Second); ; {
		if a, err := exec.Command("docker", "port", cid, "8080/tcp").Output(); err == nil && len(a) > 0 {
			base = "http://" + strings.TrimSpace(strings.SplitN(string(a), "\n", 2)[0])
			if code, _ := tryGet(base, "a.test", "/wp-content/uploads/2024/01/local.jpg"); code == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", cid).CombinedOutput()
			t.Fatalf("caddy did not come up:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}

	req := func(method, host, path string, hdr ...string) (*http.Response, string) {
		t.Helper()
		r, _ := http.NewRequest(method, base+"/", nil)
		r.URL.Opaque = path // sent exactly as written
		r.Host = host
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	for _, c := range []struct {
		name, method, path string
		code               int
		body               string
	}{
		{"local copy first", "GET", "/wp-content/uploads/2024/01/local.jpg", 200, "local-jpg"},
		{"missing locally: from the bucket", "GET", "/wp-content/uploads/2024/01/remote.jpg", 200, "remote-jpg"},
		{"HEAD", "HEAD", "/wp-content/uploads/2024/01/remote.jpg", 200, ""},
		{"query string dropped", "GET", "/wp-content/uploads/2024/01/remote.jpg?acl", 200, "remote-jpg"},
		{"escaped names", "GET", "/wp-content/uploads/2024/01/sp%20ace%20%C3%A9.png", 200, "spaced"},
		{"not in the bucket either", "GET", "/wp-content/uploads/2024/01/gone.jpg", 404, ""},
	} {
		resp, body := req(c.method, "a.test", c.path)
		if resp.StatusCode != c.code || (c.body != "" && body != c.body) {
			t.Errorf("%s: %d %q, want %d %q", c.name, resp.StatusCode, body, c.code, c.body)
		}
		if strings.Contains(body, "NoSuchKey") || strings.Contains(body, m.Bucket) {
			t.Errorf("%s: the storage's error reached the visitor: %q", c.name, body)
		}
		for k := range resp.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
				t.Errorf("%s: storage header %s passed on", c.name, k)
			}
		}
		if c.code == 200 && c.path != "/wp-content/uploads/2024/01/local.jpg" &&
			resp.Header.Get("Cache-Control") != "public, max-age=2592000" {
			t.Errorf("%s: Cache-Control %q", c.name, resp.Header.Get("Cache-Control"))
		}
	}
	// Never proxied: PHP, dot paths, traversal, other methods, other paths.
	for _, c := range []struct{ method, path string }{
		{"GET", "/wp-content/uploads/shell.php"},
		{"GET", "/wp-content/uploads/SHELL.PHP"},
		{"GET", "/wp-content/uploads/shell.php/x.jpg"},
		{"POST", "/wp-content/uploads/2024/01/remote.jpg"},
		{"GET", "/wp-content/themes/t/2024/01/remote.jpg"},
		{"GET", "/wp-content/uploads/%2e%2e/%2e%2e/index.php"},
	} {
		resp, body := req(c.method, "a.test", c.path)
		if resp.StatusCode == 200 || strings.Contains(body, "remote") {
			t.Errorf("%s %s: %d %q: must not reach the storage", c.method, c.path, resp.StatusCode, body)
		}
	}

	// What the storage receives: its own Host, the path under the prefix,
	// nothing of the visitor's; and nothing of it but the file comes back.
	resp, body := req("GET", "b.test", "/wp-content/uploads/2024/a%20b.jpg?x=1",
		"Cookie", "wordpress_logged_in_x=secret", "Authorization", "Basic c2VjcmV0", "Referer", "https://b.test/private-page")
	if want := "GET 127.0.0.1:9090 /store/s2/2024/a%20b.jpg cookie=[] auth=[] xff=[] ref=[]"; body != want {
		t.Errorf("storage saw %q\nwant        %q", body, want)
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Amz-Request-Id") != "" {
		t.Errorf("storage headers reached the visitor: %v", resp.Header)
	}
}
