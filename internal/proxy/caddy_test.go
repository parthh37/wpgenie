package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var testSites = []Site{
	{ID: "sabc1234", Name: "Blog", Domains: []string{"example.com", "www.example.com"},
		Root: "/var/lib/wpgenie/sites/sabc1234/public", Upstreams: []string{"127.0.0.1:19000", "127.0.0.1:19007"},
		ShieldEnabled: true, BlockXMLRPC: true, PageCache: true},
	{ID: "sdef5678", Name: "Shop", Domains: []string{"shop.test"},
		Root: "/var/lib/wpgenie/sites/sdef5678/public", Upstreams: []string{"127.0.0.1:19001"}},
	{ID: "webmail", Name: "Webmail", Domains: []string{"mail.example.com"}, Proxy: "127.0.0.1:8089", ShieldEnabled: true},
}

func testCaddy() *Caddy {
	return NewCaddy(Config{
		ACMEEmail: "ops@example.com", AdminURL: "http://127.0.0.1:2019", PanelDomain: "panel.example.com",
		PanelUpstream: "127.0.0.1:8088", ShieldUpstream: "127.0.0.1:8088", AccessLog: "/var/log/wpgenie/access.log",
	})
}

func TestRender(t *testing.T) {
	out, err := testCaddy().Render(testSites)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"example.com, www.example.com {",
		"php_fastcgi 127.0.0.1:19000 127.0.0.1:19007 {",
		"lb_policy least_conn",
		"file /wp-content/cache/wpgenie{path}index.html",
		"route @wpg_cached {",
		"|edd_items_in_cart|PHPSESSID)",
		"forward_auth @wpg_dynamic 127.0.0.1:8088",
		"header_up X-WPGenie-Site sabc1234",
		"panel.example.com {",
		"header_up -X-WPGenie-Site",
		// /wp-login.php/x.css runs wp-login.php: ".php" anywhere must reach the shield.
		"not path_regexp (?i)\\.php",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered Caddyfile missing %q", want)
		}
	}
	blog := s[strings.Index(s, "example.com, www.example.com {"):strings.Index(s, "shop.test {")]
	if strings.Index(blog, "forward_auth") > strings.Index(blog, "route @wpg_cached") {
		t.Error("cached pages must be served after the shield check, not before")
	}
	if strings.Index(blog, "forward_auth") > strings.Index(blog, "respond @wpg_forbidden") {
		t.Error("the shield must see probes for forbidden files, or scanners never earn a ban")
	}
	if strings.Index(blog, "reverse_proxy /_shield/*") > strings.Index(blog, "forward_auth") {
		t.Error("the challenge endpoint must not itself be behind the challenge")
	}
	if strings.Index(blog, "route @wpg_cached") > strings.Index(blog, "php_fastcgi") {
		t.Error("cache hits must be served before falling through to PHP")
	}
	shop := s[strings.Index(s, "shop.test {"):strings.Index(s, "mail.example.com {")]
	webmail := s[strings.Index(s, "mail.example.com {"):]
	if !strings.Contains(webmail, "reverse_proxy 127.0.0.1:8089") || !strings.Contains(webmail, "forward_auth @wpg_dynamic") ||
		strings.Contains(webmail, "php_fastcgi") || strings.Contains(webmail, "root *") {
		t.Errorf("webmail block:\n%s", webmail)
	}
	if strings.Contains(shop, "forward_auth") {
		t.Error("shield disabled site must not call forward_auth")
	}
	if strings.Contains(shop, "wpg_cached") {
		t.Error("page cache disabled site must not serve cached pages")
	}
	if !strings.Contains(blog, "max_fails 3") || strings.Contains(shop, "fail_duration") {
		t.Error("passive health checks belong on multi-replica sites only: one failure must not take a single-replica site offline")
	}
}

func TestRenderRequiresUpstreams(t *testing.T) {
	if _, err := testCaddy().Render([]Site{{ID: "x", Domains: []string{"a.test"}}}); err == nil {
		t.Fatal("a site without upstreams must not render (php_fastcgi would have no backend)")
	}
	bad := []Site{{ID: "x", Domains: []string{"a.test"}, Upstreams: []string{"127.0.0.1:1 }"}}}
	if _, err := testCaddy().Render(bad); err == nil {
		t.Fatal("expected error for upstream containing Caddyfile syntax")
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	bad := []Site{{ID: "x", Domains: []string{"evil.com {\n respond 200 }"}, Upstreams: []string{"127.0.0.1:1"}}}
	if _, err := testCaddy().Render(bad); err == nil {
		t.Fatal("expected error for domain containing Caddyfile syntax")
	}
}

// TestRenderIsValidCaddyfile asks a real Caddy to adapt the output. It runs
// when a caddy binary is on PATH, or via Docker with WPGENIE_TEST_DOCKER=1.
func TestRenderIsValidCaddyfile(t *testing.T) {
	out, err := testCaddy().Render(testSites)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Caddyfile"), out, 0o644)

	var cmd *exec.Cmd
	if _, err := exec.LookPath("caddy"); err == nil {
		cmd = exec.Command("caddy", "adapt", "--config", filepath.Join(dir, "Caddyfile"), "--validate")
	} else if os.Getenv("WPGENIE_TEST_DOCKER") == "1" {
		cmd = exec.Command("docker", "run", "--rm", "-v", dir+":/cfg:ro", "caddy:2-alpine",
			"caddy", "adapt", "--config", "/cfg/Caddyfile")
	} else {
		t.Skip("no caddy binary; set WPGENIE_TEST_DOCKER=1 to validate via Docker")
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("caddy rejected rendered config: %v\n%s\n--- Caddyfile ---\n%s", err, b, out)
	}
}
