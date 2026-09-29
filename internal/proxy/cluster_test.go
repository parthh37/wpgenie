package proxy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var clusterSites = []Site{
	// Spread over two servers: 19000 local, 19100 a tunnel to a replica
	// elsewhere.
	{ID: "sspread1", Name: "Busy", Domains: []string{"busy.test"}, Root: "/var/lib/wpgenie/sites/sspread1/public",
		Upstreams: []string{"127.0.0.1:19000", "127.0.0.1:19100"}, HomeUpstreams: []string{"127.0.0.1:19000"},
		ShieldEnabled: true, PageCache: true},
	// Moved here; its old server still passes visitors on.
	{ID: "sarrive1", Name: "Arrived", Domains: []string{"arrived.test", "www.arrived.test"}, CustomCert: true,
		Root: "/var/lib/wpgenie/sites/sarrive1/public", Upstreams: []string{"127.0.0.1:19001"}, ShieldEnabled: true,
		Forwarded: true},
	// Moved away from here.
	{ID: "sgone001", Name: "Gone", Domains: []string{"gone.test", "www.gone.test"},
		Forward: "127.0.0.1:19200", ForwardHTTP: "127.0.0.1:19201"},
}

func clusterCaddy() *Caddy {
	c := testCaddy()
	c.cfg.IngressListen = "127.0.0.1:8444"
	return c
}

func TestRenderCluster(t *testing.T) {
	out, err := clusterCaddy().Render(clusterSites)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		// Spread: writes pinned to the home replica, reads to all.
		"@wpg_home expression",
		"php_fastcgi @wpg_home 127.0.0.1:19000 {",
		"php_fastcgi 127.0.0.1:19000 127.0.0.1:19100 {",
		// The arriving site's loopback copy.
		"servers 127.0.0.1:8444 {",
		"allow 127.0.0.1/32",
		"http://arrived.test:8444, http://www.arrived.test:8444 {",
		"bind 127.0.0.1",
		"env HTTPS on",
		// The departed site: forwarded with the visitor's address, and ACME
		// challenges passed on.
		"gone.test, www.gone.test {\n\treverse_proxy 127.0.0.1:19200 {",
		"proxy_protocol v2",
		"http://gone.test, http://www.gone.test {",
		"reverse_proxy 127.0.0.1:19201",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The loopback copy never carries the uploaded certificate (it's plain
	// HTTP inside the tunnel), the public block does.
	if n := strings.Count(s, "tls /etc/caddy/certs/sarrive1/cert.pem"); n != 1 {
		t.Errorf("custom cert rendered %d times", n)
	}
	// Without forwarded sites there's no ingress server at all.
	plain, err := clusterCaddy().Render(clusterSites[:1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "8444") {
		t.Error("ingress listener rendered with no forwarded site")
	}
	// A non-loopback ingress listener is refused: it would take PROXY
	// headers (any claimed address) from the network.
	c := clusterCaddy()
	c.cfg.IngressListen = "0.0.0.0:8444"
	if _, err := c.Render(clusterSites); err == nil {
		t.Error("public ingress listener accepted")
	}
	bad := []Site{{ID: "sx", Name: "x", Domains: []string{"x.test"}, Forward: "127.0.0.1:1 {"}}
	if _, err := clusterCaddy().Render(bad); err == nil {
		t.Error("unsafe forward accepted")
	}
}

// TestRenderClusterAdapts has a real Caddy adapt the cluster config and
// checks where the PROXY protocol listener ended up.
func TestRenderClusterAdapts(t *testing.T) {
	out, err := clusterCaddy().Render(clusterSites)
	if err != nil {
		t.Fatal(err)
	}
	cfg := adaptJSON(t, out)
	var parsed struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen           []string          `json:"listen"`
					ListenerWrappers []json.RawMessage `json:"listener_wrappers"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	found := false
	for name, srv := range parsed.Apps.HTTP.Servers {
		wrapped := false
		for _, w := range srv.ListenerWrappers {
			if strings.Contains(string(w), "proxy_protocol") {
				wrapped = true
			}
		}
		for _, l := range srv.Listen {
			if strings.HasSuffix(l, ":8444") {
				found = true
				if l != "127.0.0.1:8444" && l != "tcp/127.0.0.1:8444" {
					t.Errorf("server %s listens on %s, want loopback only", name, l)
				}
				if !wrapped {
					t.Errorf("server %s (ingress) has no proxy_protocol wrapper", name)
				}
			} else if wrapped {
				t.Errorf("server %s on %v takes PROXY headers", name, srv.Listen)
			}
		}
	}
	if !found {
		t.Fatalf("no ingress server in adapted config:\n%s", cfg)
	}
}

func adaptJSON(t *testing.T, out []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Caddyfile"), out, 0o644)
	var cmd *exec.Cmd
	if _, err := exec.LookPath("caddy"); err == nil {
		cmd = exec.Command("caddy", "adapt", "--config", filepath.Join(dir, "Caddyfile"))
	} else if os.Getenv("WPGENIE_TEST_DOCKER") == "1" {
		cmd = exec.Command("docker", "run", "--rm", "-v", dir+":/cfg:ro", "caddy:2-alpine",
			"caddy", "adapt", "--config", "/cfg/Caddyfile")
	} else {
		t.Skip("no caddy binary; set WPGENIE_TEST_DOCKER=1 to validate via Docker")
	}
	cmd.Stderr = nil
	b, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("caddy rejected rendered config: %v\n%s\n--- Caddyfile ---\n%s", err, ee.Stderr, out)
		}
		t.Fatal(err)
	}
	return b
}
