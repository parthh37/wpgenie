package proxy

import (
	"strings"
	"testing"
)

// The page is a Caddyfile backtick string: a backtick would end it, and
// braces are Caddy placeholders.
func TestSuspendedPageIsCaddySafe(t *testing.T) {
	if strings.ContainsAny(SuspendedPage, "`{}") {
		t.Fatal("SuspendedPage contains Caddyfile syntax")
	}
	if !strings.Contains(SuspendedPage, "This site is temporarily unavailable") {
		t.Fatal("wording")
	}
}

func TestRenderSuspendedSite(t *testing.T) {
	sites := []Site{
		{ID: "sabc1234", Name: "Blog", Domains: []string{"example.com", "www.example.com"},
			Root: "/var/lib/wpgenie/sites/sabc1234/public", Upstreams: []string{"127.0.0.1:19000"}, ShieldEnabled: true},
		// Suspended: no upstreams (its replicas are stopped), its own
		// certificate still served.
		{ID: "ssus0001", Name: "Unpaid", Domains: []string{"unpaid.test", "old.unpaid.test"}, Suspended: true,
			CustomCert: true, BodyWAF: WAFBlock, PageCache: true},
	}
	out, err := testCaddy().Render(sites)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	i := strings.Index(s, "unpaid.test, old.unpaid.test {")
	if i < 0 {
		t.Fatalf("suspended site not rendered:\n%s", s)
	}
	block := s[i:]
	for _, want := range []string{"respond `<!doctype html>", "This site is temporarily unavailable", "` 503",
		"tls /etc/caddy/certs/ssus0001/cert.pem", "import wpgenie_hardening", `header Retry-After "3600"`} {
		if !strings.Contains(block, want) {
			t.Errorf("suspended block missing %q:\n%s", want, block)
		}
	}
	// Nothing of the live site: no PHP, no shield call, no log, no cache.
	for _, bad := range []string{"php_fastcgi", "forward_auth", "coraza", "wpgenie_waf", "output file", "root *", "file_server"} {
		if strings.Contains(block, bad) {
			t.Errorf("suspended block contains %q", bad)
		}
	}
	adapt(t, out)
}
