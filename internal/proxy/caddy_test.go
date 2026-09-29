package proxy

import (
	"github.com/parthh37/wpgenie/internal/shield"
	"net/netip"
	"net/textproto"
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
		CloudflareRanges: func() []netip.Prefix {
			return []netip.Prefix{netip.MustParsePrefix("173.245.48.0/20"), netip.MustParsePrefix("2606:4700::/32")}
		},
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

// Behind Cloudflare every connection comes from its edge. The shield must
// see the visitor (CF-Connecting-IP), but only when the peer really is
// Cloudflare, or anyone could pick the address that gets banned.
func TestRenderClientIP(t *testing.T) {
	out, err := testCaddy().Render(testSites)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	global := s[:strings.Index(s, "(wpgenie_hardening)")]
	if !strings.Contains(global, "trusted_proxies static 173.245.48.0/20 2606:4700::/32") ||
		!strings.Contains(global, "client_ip_headers CF-Connecting-IP") {
		t.Errorf("global options must trust Cloudflare's ranges for CF-Connecting-IP:\n%s", global)
	}
	// Every call to the shield (checks and challenge verification, which
	// must agree on the address) carries the resolved client IP, and so
	// does the panel (its sign-in limits and audit log key on it).
	calls := strings.Count(s, "header_up X-WPGenie-Site ") + strings.Count(s, "header_up -X-WPGenie-Site")
	if n := strings.Count(s, "header_up X-Forwarded-For {client_ip}"); n != calls || n == 0 {
		t.Errorf("%d shield and panel proxies but %d pass {client_ip}", calls, n)
	}

	none, err := NewCaddy(Config{AdminURL: "http://127.0.0.1:2019", ShieldUpstream: "127.0.0.1:8088"}).Render(testSites)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(none), "trusted_proxies") {
		t.Error("no ranges configured: nothing may be trusted")
	}
}

// Static files get cache headers (browsers and the CDN keep them), but only
// files that exist: a missing /x.css is WordPress's 404 page.
func TestRenderStaticCaching(t *testing.T) {
	out, _ := testCaddy().Render(testSites)
	s := string(out)
	blog := s[strings.Index(s, "example.com, www.example.com {"):strings.Index(s, "shop.test {")]
	for _, want := range []string{
		`header @wpg_static_long Cache-Control "public, max-age=2592000"`,
		`header @wpg_static_short Cache-Control "public, max-age=604800"`,
	} {
		if !strings.Contains(blog, want) {
			t.Errorf("missing %q", want)
		}
	}
	long := blog[strings.Index(blog, "@wpg_static_long {"):]
	long = long[:strings.Index(long, "}")]
	if !strings.Contains(long, "file") || !strings.Contains(long, `not path_regexp (?i)\.php`) {
		t.Errorf("static cache matcher must require an existing, non-PHP file:\n%s", long)
	}
	if strings.Contains(s[strings.Index(s, "mail.example.com {"):], "Cache-Control") {
		t.Error("webmail is proxied as-is")
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
// The access log hashes the health token's header: the exact canonical name
// is what Caddy's log filter matches.
func TestRenderHashesHealthToken(t *testing.T) {
	out, err := testCaddy().Render(testSites)
	if err != nil {
		t.Fatal(err)
	}
	want := "request>headers>" + textproto.CanonicalMIMEHeaderKey(shield.HealthHeader) + " hash"
	if strings.Count(string(out), want) != 2 { // both WordPress sites
		t.Errorf("health token not hashed in the access log (%q)", want)
	}
}

func TestRenderIsValidCaddyfile(t *testing.T) {
	out, err := testCaddy().Render(testSites)
	if err != nil {
		t.Fatal(err)
	}
	adapt(t, out)
}

func adapt(t *testing.T, out []byte) {
	t.Helper()
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

var phase2Sites = []Site{
	{ID: "sabc1234", Name: "Blog", Domains: []string{"example.com", "blog.example.com"},
		Redirects: []string{"www.example.com", "old-name.test"}, CustomCert: true,
		Root: "/var/lib/wpgenie/sites/sabc1234/public", Upstreams: []string{"127.0.0.1:19000"}, ShieldEnabled: true,
		BodyWAF: WAFOff},
	{ID: "sstg0001", Name: "Staging: Blog", Domains: []string{"staging.example.com"}, Staging: true,
		Root: "/var/lib/wpgenie/sites/sstg0001/public", Upstreams: []string{"127.0.0.1:19001"}, ShieldEnabled: true},
}

func TestRenderDomainsCertsStaging(t *testing.T) {
	out, err := testCaddy().Render(phase2Sites)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	blog := s[strings.Index(s, "example.com, blog.example.com {"):strings.Index(s, "www.example.com, old-name.test {")]
	redirects := s[strings.Index(s, "www.example.com, old-name.test {"):strings.Index(s, "staging.example.com {")]
	staging := s[strings.Index(s, "staging.example.com {"):]
	if !strings.Contains(blog, "tls /etc/caddy/certs/sabc1234/cert.pem /etc/caddy/certs/sabc1234/key.pem") {
		t.Error("own certificate not used")
	}
	if !strings.Contains(redirects, "redir https://example.com{uri} permanent") || strings.Contains(redirects, "php_fastcgi") {
		t.Errorf("redirect block:\n%s", redirects)
	}
	if strings.Contains(blog, "X-Robots-Tag") || !strings.Contains(staging, `X-Robots-Tag "noindex, nofollow"`) {
		t.Error("noindex must be on staging sites only")
	}
	if strings.Contains(staging, "tls /etc") {
		t.Error("a site without its own certificate must use automatic TLS")
	}
	for _, block := range []string{blog, staging} {
		tools := strings.Index(block, "reverse_proxy /_wpgenie/*")
		if tools < 0 || tools > strings.Index(block, "forward_auth") {
			t.Error("WPGenie tools must be routed before the shield and the WAF")
		}
		if !strings.Contains(block[tools:tools+200], "header_up X-WPGenie-Site") {
			t.Error("tools route must name the site (Caddy sets it, overriding the client)")
		}
	}
	bad := []Site{{ID: "x", Domains: []string{"a.test"}, Redirects: []string{"b.test {\n}"}, Upstreams: []string{"127.0.0.1:1"}}}
	if _, err := testCaddy().Render(bad); err == nil {
		t.Error("a redirect domain with Caddyfile syntax rendered")
	}
	adapt(t, out)
}
