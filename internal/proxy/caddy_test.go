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
		Root: "/var/lib/wpgenie/sites/sabc1234/public", FPMPort: 19000, ShieldEnabled: true, BlockXMLRPC: true},
	{ID: "sdef5678", Name: "Shop", Domains: []string{"shop.test"},
		Root: "/var/lib/wpgenie/sites/sdef5678/public", FPMPort: 19001},
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
		"php_fastcgi 127.0.0.1:19000",
		"forward_auth @wpg_dynamic 127.0.0.1:8088",
		"header_up X-WPGenie-Site sabc1234",
		"panel.example.com {",
		"header_up -X-WPGenie-Site",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered Caddyfile missing %q", want)
		}
	}
	shop := s[strings.Index(s, "shop.test {"):]
	if strings.Contains(shop, "forward_auth") {
		t.Error("shield disabled site must not call forward_auth")
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	bad := []Site{{ID: "x", Domains: []string{"evil.com {\n respond 200 }"}}}
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
