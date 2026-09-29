package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWAFRendersOnlyWhenSupported(t *testing.T) {
	c := NewCaddy(Config{AdminURL: "http://127.0.0.1:2019", ShieldUpstream: "127.0.0.1:8088", AccessLog: "/var/log/wpgenie/access.log"})
	sites := []Site{
		{ID: "sa", Domains: []string{"a.test"}, Root: "/r", Upstreams: []string{"127.0.0.1:1"}, ShieldEnabled: true, BodyWAF: WAFBlock},
		{ID: "sb", Domains: []string{"b.test"}, Root: "/r", Upstreams: []string{"127.0.0.1:2"}, BodyWAF: WAFDetect},
		{ID: "sc", Domains: []string{"c.test"}, Root: "/r", Upstreams: []string{"127.0.0.1:3"}},
	}
	out, err := c.RenderWAF(sites, true)
	if err != nil {
		t.Fatal(err)
	}
	cf := string(out)
	for _, want := range []string{"(wpgenie_waf) {", "import wpgenie_waf On", "import wpgenie_waf DetectionOnly",
		"SecAuditLog /var/log/wpgenie/waf.log", "id:9507100", "Include @owasp_crs/*.conf"} {
		if !strings.Contains(cf, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	if n := strings.Count(cf, "handle_errors"); n != 1 {
		t.Errorf("%d handle_errors blocks, want 1 (block mode only)", n)
	}
	if sites[2].BodyWAF != "" || sites[0].BodyWAF != WAFBlock {
		t.Error("Render modified the caller's sites")
	}
	// A Caddy without Coraza gets no WAF at all rather than a config it
	// would reject.
	out, err = c.RenderWAF(sites, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "coraza") || strings.Contains(string(out), "wpgenie_waf") {
		t.Error("WAF rendered for a Caddy without the module")
	}
	if _, err := c.RenderWAF([]Site{{ID: "x", Domains: []string{"x.test"}, Upstreams: []string{"a:1"}, BodyWAF: "maybe"}}, true); err == nil {
		t.Error("unknown mode accepted")
	}
}

// TestWAFInRealCaddy runs the rendered config in WPGenie's Caddy image
// (images/caddy, built if missing): attacks in request bodies are blocked
// (or only logged in detect mode), ordinary WordPress traffic passes, and
// every match reaches the audit log the daemon reads. Needs Docker:
// WPGENIE_TEST_DOCKER=1.
func TestWAFInRealCaddy(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run against real Caddy via Docker")
	}
	const image = "wpgenie/caddy:test"
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		if out, err := exec.Command("docker", "build", "-q", "-t", image, "../../images/caddy").CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", image, err, out)
		}
	}
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	// Pretty permalinks route through index.php, which must exist.
	os.WriteFile(filepath.Join(dir, "public", "index.php"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "public", "pic.png"), []byte("png"), 0o644)
	os.MkdirAll(logs, 0o777)
	os.Chmod(logs, 0o777)
	out, err := NewCaddy(Config{AdminURL: "http://127.0.0.1:2019", ShieldUpstream: "127.0.0.1:8088", AccessLog: "/logs/access.log"}).
		RenderWAF([]Site{
			{ID: "sa", Domains: []string{"http://a.test:8080"}, Root: "/srv/public", Upstreams: []string{"127.0.0.1:1"},
				ShieldEnabled: true, BlockXMLRPC: true, BodyWAF: WAFBlock},
			{ID: "sb", Domains: []string{"http://b.test:8080"}, Root: "/srv/public", Upstreams: []string{"127.0.0.1:1"},
				ShieldEnabled: true, BodyWAF: WAFDetect},
		}, true)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, "\n:8088 {\n\trespond 200\n}\n"...) // stand-in shield: allow everything
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd := exec.Command("docker", "run", "-d", "-p", "127.0.0.1::8080", "-v", dir+":/srv:ro", "-v", logs+":/logs",
		image, "caddy", "run", "--config", "/srv/Caddyfile", "--adapter", "caddyfile")
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, stderr.String())
	}
	cid := strings.TrimSpace(string(b))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
	var base string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline) && base == ""; time.Sleep(300 * time.Millisecond) {
		if a, err := exec.Command("docker", "port", cid, "8080/tcp").Output(); err == nil && len(a) > 0 {
			u := "http://" + strings.TrimSpace(strings.SplitN(string(a), "\n", 2)[0])
			if code, _ := tryGet(u, "a.test", "/pic.png"); code == http.StatusOK {
				base = u
			}
		}
	}
	if base == "" {
		logs, _ := exec.Command("docker", "logs", cid).CombinedOutput()
		t.Fatalf("caddy did not come up:\n%s", logs)
	}

	post := func(host, path, ctype, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		req.Host = host
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Safari/605.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-Wpgenie-Shield")
	}
	form := "application/x-www-form-urlencoded"
	sqli := url.Values{"action": {"load"}, "id": {"1' UNION SELECT user_pass FROM wp_users-- -"}}.Encode()
	// Reaching PHP (none runs here) is a 502: an error, like a WAF block,
	// so these also show handle_errors leaves other errors alone. A WAF
	// block is a marked 403.
	for _, c := range []struct {
		name, host, path, ctype, body string
		code                          int
		verdict                       string
	}{
		{"sqli in a form body", "a.test", "/wp-admin/admin-ajax.php", form, sqli, 403, "waf"},
		{"sqli in JSON", "a.test", "/wp-json/shop/v1/items", "application/json", `{"id":"1' UNION SELECT user_pass FROM wp_users-- -"}`, 403, "waf"},
		{"detect mode logs only", "b.test", "/wp-admin/admin-ajax.php", form, sqli, 502, ""},
		{"login with a quote-heavy password", "a.test", "/wp-login.php", form,
			url.Values{"log": {"admin"}, "pwd": {"' OR 1=1 -- <script>"}, "redirect_to": {"https://a.test/wp-admin/"}}.Encode(), 502, ""},
		{"block editor saves HTML", "a.test", "/wp-json/wp/v2/posts/7", "application/json",
			`{"content":"<!-- wp:html --><script>track()</script><iframe src=\"https://www.youtube.com/embed/x\"></iframe><!-- /wp:html -->"}`, 502, ""},
		{"comment with code", "a.test", "/wp-comments-post.php", form,
			url.Values{"comment": {"Try SELECT * FROM t WHERE a = 'b'; it works"}, "author": {"Ann"}}.Encode(), 502, ""},
	} {
		code, verdict := post(c.host, c.path, c.ctype, c.body)
		if code != c.code || verdict != c.verdict {
			t.Errorf("%s: %d %q, want %d %q", c.name, code, verdict, c.code, c.verdict)
		}
	}

	// The audit log names the host and the rules, without request data.
	// Read through the container: Caddy creates it 0600 as its own user,
	// which the test's user may not be (the daemon reads it as root).
	var entries []WAFAuditEntry
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(entries) < 3; time.Sleep(200 * time.Millisecond) {
		entries = nil
		out, err := exec.Command("docker", "exec", cid, "cat", "/logs/waf.log").Output()
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var e WAFAuditEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) != 3 {
		t.Fatalf("%d audit log entries, want 3", len(entries))
	}
	for _, e := range entries {
		if e.Transaction.ServerID == "" || len(e.Messages) == 0 || e.Transaction.Request.Headers != nil {
			t.Errorf("audit entry %+v", e.Transaction)
		}
	}
	if !entries[0].Transaction.Interrupted || entries[2].Transaction.Interrupted || entries[2].Transaction.ServerID != "b.test" {
		t.Errorf("interrupted flags / hosts: %+v %+v", entries[0].Transaction, entries[2].Transaction)
	}
}

func TestWAFLogFollower(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waf.log")
	line := func(host, uri string, blocked bool) string {
		return `{"transaction":{"unix_timestamp":1790674267682197505,"client_ip":"198.51.100.7","server_id":"` + host +
			`","request":{"method":"POST","uri":"` + uri + `","headers":null},"is_interrupted":` + map[bool]string{true: "true", false: "false"}[blocked] +
			`},"messages":[{"error_message":"[client \"198.51.100.7\"] Coraza: Warning. SQL Injection Attack Detected via libinjection [file \"x\"] [line \"1\"] [id \"942100\"] [msg \"SQL Injection Attack Detected via libinjection\"] [data \"Matched Data: s&1 found within ARGS:id: secret-value\"]"},` +
			`{"error_message":"[id \"949110\"] [msg \"Inbound Anomaly Score Exceeded\"]"}]}` + "\n"
	}
	os.WriteFile(path, []byte(line("old.test", "/", true)), 0o600)
	var got []WAFEvent
	w := &WAFLog{Path: path, MaxSize: 1000, Handle: func(evs []WAFEvent) { got = append(got, evs...) }}
	w.poll() // starts at the end: old entries aren't replayed
	appendLine := func(s string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(s)
		f.Close()
	}
	appendLine(line("A.test:443", "/wp-admin/admin-ajax.php?action=x", true))
	appendLine(`{"transaction":` /* a partial line, still being written */)
	w.poll()
	if len(got) != 1 {
		t.Fatalf("%d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Host != "a.test" || ev.Path != "/wp-admin/admin-ajax.php" || !ev.Blocked || ev.IP != "198.51.100.7" ||
		len(ev.Rules) != 1 || ev.Rules[0].ID != 942100 || ev.Rules[0].Target != "ARGS:id" {
		t.Fatalf("event %+v", ev)
	}
	if r := ev.Reason(); r != "waf 942100: SQL Injection Attack Detected via libinjection (ARGS:id)" || strings.Contains(r, "secret") {
		t.Errorf("reason %q", r)
	}
	// Past MaxSize the file is truncated after reading; Coraza (O_APPEND)
	// keeps writing at the new end.
	os.WriteFile(path, nil, 0o600)
	w.offset = 0
	for range 3 {
		appendLine(line("b.test", "/", false))
	}
	w.poll()
	if info, _ := os.Stat(path); info.Size() != 0 || len(got) != 4 || got[3].Blocked {
		t.Fatalf("size %d, %d events", info.Size(), len(got))
	}
	appendLine(line("c.test", "/", true))
	w.poll()
	if len(got) != 5 || got[4].Host != "c.test" {
		t.Fatalf("after truncation: %d events", len(got))
	}
}

// TestWAFProbe asks real Caddies' admin APIs whether they can run the WAF:
// WPGenie's image can, the stock one can't. Needs Docker.
func TestWAFProbe(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run against real Caddy via Docker")
	}
	for image, want := range map[string]bool{"wpgenie/caddy:test": true, "caddy:2-alpine": false} {
		if image == "wpgenie/caddy:test" && exec.Command("docker", "image", "inspect", image).Run() != nil {
			if out, err := exec.Command("docker", "build", "-q", "-t", image, "../../images/caddy").CombinedOutput(); err != nil {
				t.Fatalf("building %s: %v\n%s", image, err, out)
			}
		}
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Caddyfile"), []byte("{\n\tadmin 0.0.0.0:2019\n}\n"), 0o644)
		out, err := exec.Command("docker", "run", "-d", "-p", "127.0.0.1::2019", "-v", dir+":/srv:ro",
			image, "caddy", "run", "--config", "/srv/Caddyfile", "--adapter", "caddyfile").Output()
		if err != nil {
			t.Fatalf("docker run %s: %v", image, err)
		}
		cid := strings.TrimSpace(string(out))
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
		var admin string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			a, err := exec.Command("docker", "port", cid, "2019/tcp").Output()
			if err != nil || len(a) == 0 {
				continue
			}
			admin = "http://" + strings.TrimSpace(strings.SplitN(string(a), "\n", 2)[0])
			if resp, err := http.Get(admin + "/config/"); err == nil {
				resp.Body.Close()
				break
			}
		}
		c := NewCaddy(Config{AdminURL: admin})
		if got := c.wafSupported(context.Background()); got != want || c.WAFAvailable() != want {
			t.Errorf("%s: WAF supported = %v, want %v", image, got, want)
		}
	}
}

func TestSiteNamesCantInjectConfig(t *testing.T) {
	out, err := NewCaddy(Config{AdminURL: "http://127.0.0.1:2019", ShieldUpstream: "127.0.0.1:8088"}).Render([]Site{{
		ID: "sa", Name: "x\n}\nevil.example {\n\treverse_proxy 127.0.0.1:2019\n}\n#", Domains: []string{"a.test"},
		Root: "/r", Upstreams: []string{"127.0.0.1:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "evil.example") {
			t.Fatalf("a site name injected a site block:\n%s", out)
		}
	}
}
