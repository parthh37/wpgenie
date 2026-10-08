package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// lockHash is bcrypt("open sesame") at the lowest cost (tests only).
var lockHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte("open sesame"), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}()

var edgeRedirects = []PathRedirect{
	{From: "/old/*", To: "https://example.org/archive", Code: 301},
	{From: "/old/keep", To: "/kept", Code: 302, KeepQuery: true},
	{From: "/old/deeper/*", To: "/deeper", Code: 308},
	{From: "/", To: "/home", Code: 307},
	{From: "/café", To: "/caf%C3%A9-new", Code: 301},
}

func edgeSite() Site {
	return Site{ID: "slock001", Name: "Locked", Domains: []string{"locked.test", "www.locked.test"},
		Root: "/var/lib/wpgenie/sites/slock001/public", Upstreams: []string{"127.0.0.1:19010"},
		ShieldEnabled: true, PageCache: true, EdgeHTML: true, BodyWAF: WAFBlock,
		Lock:          &Lock{User: "team", Hash: lockHash, Allow: []string{"203.0.113.7/32", "2001:db8::/48"}},
		PathRedirects: edgeRedirects}
}

// TestRenderLockAndRedirects: the lock comes after the shield and before
// everything that serves the site (the WAF, cached pages, files, PHP), the
// panel's own paths before it; redirects before cached pages and PHP, the
// most specific first.
func TestRenderLockAndRedirects(t *testing.T) {
	open := edgeSite()
	open.ID, open.Domains, open.Lock, open.PathRedirects = "sopen001", []string{"open.test"}, nil, nil
	out, err := testCaddy().Render([]Site{edgeSite(), open})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	locked := s[strings.Index(s, "locked.test, www.locked.test {"):strings.Index(s, "open.test {")]
	openBlock := s[strings.Index(s, "open.test {"):]
	route := locked[strings.Index(locked, "\troute {"):]
	order := []string{
		"reverse_proxy /_shield/*",
		"reverse_proxy /_wpgenie/*",
		"forward_auth @wpg_dynamic",
		"basic_auth @wpg_locked bcrypt \"Private site\" {\n\t\t\tteam " + lockHash + "\n\t\t}",
		"import wpgenie_waf",
		"respond @wpg_forbidden 404",
		"redir @wpg_redirect_0 ",
		"route @wpg_cached {",
		"php_fastcgi",
		"file_server\n",
	}
	last := -1
	for _, want := range order {
		i := strings.Index(route, want)
		if i < 0 {
			t.Fatalf("route lacks %q:\n%s", want, route)
		}
		if i < last {
			t.Errorf("%q is out of order in the route:\n%s", want, route)
		}
		last = i
	}
	for _, want := range []string{
		"@wpg_locked {\n\t\tnot path /.well-known/acme-challenge/*\n\t\tnot client_ip 203.0.113.7/32 2001:db8::/48\n\t}",
		// Exact paths first (in the order made), then prefixes, longest first.
		"@wpg_redirect_0 path /old/keep /old/keep/\n",
		"@wpg_redirect_1 path / \n",
		"@wpg_redirect_2 path /café /café/\n",
		"@wpg_redirect_3 path /old/deeper /old/deeper/*\n",
		"@wpg_redirect_4 path /old /old/*\n",
		"redir @wpg_redirect_0 /kept{?query} 302\n",
		"redir @wpg_redirect_1 /home 307\n",
		"redir @wpg_redirect_2 /caf%C3%A9-new 301\n",
		"redir @wpg_redirect_3 /deeper 308\n",
		"redir @wpg_redirect_4 https://example.org/archive 301\n",
		// Never public, nor on the lock's 401; never offered to a CDN.
		"header @wpg_static_long {\n\t\tCache-Control \"private, max-age=2592000\"\n\t\tdefer\n\t}",
		"header @wpg_static_short {\n\t\tCache-Control \"private, max-age=604800\"\n\t\tdefer\n\t}",
	} {
		if !strings.Contains(locked, strings.ReplaceAll(want, "/ \n", "/\n")) {
			t.Errorf("locked block lacks %q:\n%s", want, locked)
		}
	}
	if strings.Contains(locked, "public, max-age") || strings.Contains(locked, "s-maxage") {
		t.Error("a locked site's files or pages are marked for shared caches")
	}
	if strings.Contains(openBlock, "basic_auth") || strings.Contains(openBlock, "wpg_redirect") ||
		!strings.Contains(openBlock, `header @wpg_static_long Cache-Control "public, max-age=2592000"`) ||
		!strings.Contains(openBlock, "s-maxage=3600") {
		t.Errorf("an open site changed:\n%s", openBlock)
	}
	// Stock Caddy has no WAF module.
	plain := edgeSite()
	plain.BodyWAF, open.BodyWAF = WAFOff, WAFOff
	if out, err = testCaddy().Render([]Site{plain, open}); err != nil {
		t.Fatal(err)
	}
	adapt(t, out)
}

// The copy served to visitors another server passes on is locked and
// redirected alike.
func TestRenderLockOnIngress(t *testing.T) {
	c := testCaddy()
	c.cfg.IngressListen = "127.0.0.1:8443"
	st := edgeSite()
	st.Forwarded = true
	out, err := c.Render([]Site{st})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	ingress := s[strings.Index(s, "http://locked.test:8443"):]
	if !strings.Contains(ingress, "basic_auth @wpg_locked") || !strings.Contains(ingress, "redir @wpg_redirect_4 ") {
		t.Errorf("ingress copy:\n%s", ingress)
	}
	if strings.Count(s, "basic_auth @wpg_locked") != 2 {
		t.Error("the lock must be on both copies of the site")
	}
}

// TestRenderRejectsHostileEdgeRules: whatever reaches Render, nothing in a
// lock or a redirect can add to the Caddyfile or change its structure.
func TestRenderRejectsHostileEdgeRules(t *testing.T) {
	base := edgeSite()
	try := func(name string, mod func(*Site)) {
		t.Helper()
		st := base
		l := *base.Lock
		st.Lock = &l
		st.PathRedirects = []PathRedirect{{From: "/a", To: "/b", Code: 301}}
		mod(&st)
		if out, err := testCaddy().Render([]Site{st}); err == nil {
			t.Errorf("%s rendered:\n%s", name, out)
		}
	}
	for _, u := range []string{"", "team leader", "a\nb", "{env.USER}", "#x", "@m", "a:b", `a"b`, "a}", "-a", ".a",
		strings.Repeat("a", 65), "é"} {
		try(fmt.Sprintf("lock user %q", u), func(s *Site) { s.Lock.User = u })
	}
	for _, h := range []string{"", "open sesame", "$2a$10$short", lockHash + "\n\trespond 200", "{env.HASH}",
		lockHash + " x", "$1$abc$" + strings.Repeat("a", 53), lockHash[:len(lockHash)-1] + "{"} {
		try(fmt.Sprintf("lock hash %q", h), func(s *Site) { s.Lock.Hash = h })
	}
	for _, a := range []string{"1.2.3.4/32 }", "evil", "1.2.3.4", "1.2.3.0/24\n", "{remote_host}", "1.2.3.5/24"} {
		try(fmt.Sprintf("lock network %q", a), func(s *Site) { s.Lock.Allow = []string{a} })
	}
	for _, f := range []string{"", "old", "/old page", "/old\n", "/old\r\n\trespond 200", "/{path}", "/a}", "/a{",
		`/a"b`, "/a#b", "/a*b", "/a/*/b", "/*/a", "/a?b", "/a[1]", `/a\b`, "/a%2fb", "//x", "/a/../b", "/a/./b",
		"/a/", "//*", "/a`b", "/\u2028", "/a\tb", "/a\x00", "/<x>", "/a|b", "/a^b", "/" + strings.Repeat("a", 1100)} {
		try(fmt.Sprintf("redirect from %q", f), func(s *Site) { s.PathRedirects[0].From = f })
	}
	for _, to := range []string{"", "x", "http://e.com", "https://e.com/ a", "https://e.com/{uri}", "https://e.com/\nrespond",
		"//evil.com", `/\evil`, "https://e.com@evil.com", "https://E.com/", "https://e.com/#x", `https://e.com/"`,
		"javascript:alert(1)", "/a`b", "https://e.com/<x>", "https://e.com/é", "/x y", "https://e.com:99999x/",
		"https://user:pw@e.com/", "/{?query}", "https://e.com/}", "///evil.com"} {
		try(fmt.Sprintf("redirect to %q", to), func(s *Site) { s.PathRedirects[0].To = to })
	}
	for _, c := range []int{0, 200, 300, 304, 399, 404} {
		try(fmt.Sprintf("redirect code %d", c), func(s *Site) { s.PathRedirects[0].Code = c })
	}
}

// FuzzRedirectRender: a redirect Render accepts never changes the
// Caddyfile's structure: the same lines and braces as a plain rule, its
// From and To each one token on their own line.
func FuzzRedirectRender(f *testing.F) {
	for _, s := range []struct{ from, to string }{
		{"/a", "/b"}, {"/old/*", "https://example.com/x?y=1"}, {"/a b", "/c"}, {"/a{x}", "/b"},
		{"/a", "https://e.com/{uri}"}, {"/a\n}", "/b"}, {"/é", "/%C3%A9"}, {"/*", "https://e.com"},
		{"/x", "/y\n\trespond 200"}, {"/x", "https://e.com/`"}, {"/a\"", "/b"},
	} {
		f.Add(s.from, s.to, true)
	}
	render := func(r PathRedirect) (string, error) {
		st := Site{ID: "s1", Name: "S", Domains: []string{"a.test"}, Root: "/srv", Upstreams: []string{"127.0.0.1:1"},
			PathRedirects: []PathRedirect{r}}
		out, err := testCaddy().Render([]Site{st})
		return string(out), err
	}
	baseline, err := render(PathRedirect{From: "/a", To: "/b", Code: 301})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, from, to string, keep bool) {
		out, err := render(PathRedirect{From: from, To: to, Code: 301, KeepQuery: keep})
		if err != nil {
			return
		}
		if strings.ContainsAny(from+to, "{}\"`\\# \t\r\n") {
			t.Fatalf("accepted %q -> %q", from, to)
		}
		if strings.Count(out, "\n") != strings.Count(baseline, "\n") ||
			strings.Count(out, "{")-strings.Count(baseline, "{") != boolInt(keep) ||
			strings.Count(out, "}")-strings.Count(baseline, "}") != boolInt(keep) {
			t.Fatalf("%q -> %q changed the Caddyfile's structure:\n%s", from, to, out)
		}
		loc := to
		if keep {
			loc += "{?query}"
		}
		if !strings.Contains(out, "\t\tredir @wpg_redirect_0 "+loc+" 301\n") ||
			!strings.Contains(out, "\t@wpg_redirect_0 path "+strings.Join(redirectPatterns(from), " ")+"\n") {
			t.Fatalf("%q -> %q not rendered as one token each:\n%s", from, to, out)
		}
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestMatchRedirect: exact paths match with or without a trailing slash,
// prefixes match their folder and everything under it, case doesn't
// matter, paths are cleaned first, and the most specific rule wins.
func TestMatchRedirect(t *testing.T) {
	for _, c := range []struct {
		path string
		want int
	}{
		{"/", 3},
		{"", 3},
		{"/old/keep", 1},
		{"/old/keep/", 1},
		{"/OLD/Keep", 1},
		{"/old//keep", 1},
		{"/old/x/../keep", 1},
		{"/old/keep/more", 0},
		{"/old/keeper", 0},
		{"/old", 0},
		{"/old/", 0},
		{"/old/anything/at/all", 0},
		{"/old/deeper", 2},
		{"/old/deeper/x", 2},
		{"/old/deeperx", 0},
		{"/older", -1},
		{"/café", 4},
		{"/CAFÉ/", 4},
		{"/caf", -1},
		{"/new", -1},
		{"/index.php", -1},
	} {
		if got := MatchRedirect(edgeRedirects, c.path); got != c.want {
			t.Errorf("MatchRedirect(%q) = %d, want %d", c.path, got, c.want)
		}
	}
	all := []PathRedirect{{From: "/*", To: "https://new.example.com", Code: 301}, {From: "/keep", To: "/kept", Code: 301}}
	if MatchRedirect(all, "/anything") != 0 || MatchRedirect(all, "/keep/") != 1 {
		t.Error("an exact path must win over a catch-all prefix")
	}
	if got := RedirectLocation(edgeRedirects[1], "a=1&b=2"); got != "/kept?a=1&b=2" {
		t.Errorf("kept query: %q", got)
	}
	if got := RedirectLocation(edgeRedirects[0], "a=1"); got != "https://example.org/archive" {
		t.Errorf("dropped query: %q", got)
	}
}

// TestLockAndRedirectsInRealCaddy runs the rendered config in a real Caddy
// (a caddy binary on PATH, or Docker with WPGENIE_TEST_DOCKER=1): nothing
// of a locked site (pages, cached pages, files) without the password but
// the panel's paths and Let's Encrypt's; and every redirect answers as
// MatchRedirect says.
func TestLockAndRedirectsInRealCaddy(t *testing.T) {
	_, local := exec.LookPath("caddy")
	docker := os.Getenv("WPGENIE_TEST_DOCKER") == "1"
	if local != nil && !docker {
		t.Skip("no caddy binary; set WPGENIE_TEST_DOCKER=1 to run via Docker")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"public/wp-content/uploads/pic.jpg":            "jpeg",
		"public/wp-content/cache/wpgenie/index.html":   "cached home page",
		"public/wp-content/cache/wpgenie/x/index.html": "cached x page",
		"public/.well-known/acme-challenge/tok":        "acme",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Ports and paths as Caddy sees them: its own (Docker), or free ones.
	port, admin, tools, root := "8080", "2019", "8088", "/srv/public"
	if local == nil {
		port, admin, tools, root = freePort(t), freePort(t), freePort(t), filepath.Join(dir, "public")
	}
	everyone := []string{"0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"}
	mk := func(id, host string, allow []string) Site {
		return Site{ID: id, Name: id, Domains: []string{"http://" + host + ":" + port}, Root: root,
			Upstreams: []string{"127.0.0.1:1"}, PageCache: true,
			Lock:          &Lock{User: "team", Hash: lockHash, Allow: allow},
			PathRedirects: edgeRedirects}
	}
	out, err := NewCaddy(Config{AdminURL: "http://127.0.0.1:" + admin, ShieldUpstream: "127.0.0.1:" + tools,
		AccessLog: filepath.Join(root, "..", "access.log")}).Render([]Site{mk("sa", "a.test", nil), mk("sb", "b.test", everyone)})
	if err != nil {
		t.Fatal(err)
	}
	// A stand-in for the daemon's tools (/_wpgenie/*).
	out = append(out, "\n:"+tools+" {\n\trespond \"tools {path}\" 200\n}\n"...)
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	var base string
	if local == nil {
		cmd := exec.Command("caddy", "run", "--config", filepath.Join(dir, "Caddyfile"), "--adapter", "caddyfile")
		var logs strings.Builder
		cmd.Stdout, cmd.Stderr = &logs, &logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		base = "http://127.0.0.1:" + port
		waitUp(t, base, func() string { return logs.String() })
	} else {
		b, err := exec.Command("docker", "run", "-d", "-p", "127.0.0.1::8080", "-v", dir+":/srv",
			"caddy:2-alpine", "caddy", "run", "--config", "/srv/Caddyfile", "--adapter", "caddyfile").CombinedOutput()
		if err != nil {
			t.Fatalf("docker run: %v\n%s", err, b)
		}
		cid := strings.TrimSpace(string(b))
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
		for deadline := time.Now().Add(30 * time.Second); base == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if a, err := exec.Command("docker", "port", cid, "8080/tcp").Output(); err == nil && len(a) > 0 {
				base = "http://" + strings.TrimSpace(strings.SplitN(string(a), "\n", 2)[0])
			}
		}
		waitUp(t, base, func() string { l, _ := exec.Command("docker", "logs", cid).CombinedOutput(); return string(l) })
	}

	type answer struct {
		code             int
		body, loc, cache string
		prompt           bool
	}
	req := func(host, rawPath string, auth bool) answer {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		r.URL.Opaque = rawPath // sent exactly as written
		r.Host = host
		if auth {
			r.SetBasicAuth("team", "open sesame")
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := make([]byte, 256)
		n, _ := resp.Body.Read(b)
		return answer{resp.StatusCode, string(b[:n]), resp.Header.Get("Location"), resp.Header.Get("Cache-Control"),
			strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ")}
	}

	// Locked: the password, and nothing without it.
	for _, p := range []string{"/", "/x/", "/wp-content/uploads/pic.jpg", "/old-page", "/old/keep", "/wp-login.php"} {
		if a := req("a.test", p, false); a.code != 401 || !a.prompt || a.cache != "" || strings.Contains(a.body, "cached") {
			t.Errorf("GET %s without the password: %+v", p, a)
		}
		if a := req("a.test", p, true); a.code == 401 {
			t.Errorf("GET %s with the password: %+v", p, a)
		}
		if a := req("a.test", p, false); a.code != 401 {
			t.Errorf("GET %s after someone else signed in: %+v", p, a)
		}
	}
	if a := req("a.test", "/wp-content/uploads/pic.jpg", true); a.code != 200 || a.body != "jpeg" || a.cache != "private, max-age=2592000" {
		t.Errorf("an upload with the password: %+v", a)
	}
	// The panel's paths and Let's Encrypt's challenges stay open.
	if a := req("a.test", "/_wpgenie/login?t=x", false); a.code != 200 || a.body != "tools /_wpgenie/login" {
		t.Errorf("panel path: %+v", a)
	}
	if a := req("a.test", "/.well-known/acme-challenge/tok", false); a.code == 401 {
		t.Errorf("ACME challenge: %+v", a)
	}
	// Allowed networks skip the lock: cached pages too.
	if a := req("b.test", "/x/", false); a.code != 200 || a.body != "cached x page" {
		t.Errorf("an allowed network: %+v", a)
	}

	// Every redirect answers as MatchRedirect says it does (b.test: no
	// password needed).
	for _, p := range []string{"/", "/old/keep", "/old/keep/", "/OLD/KEEP", "/old//keep", "/old/x/../keep", "/old/keep/more",
		"/old", "/old/", "/old/a/b", "/old/deeper", "/old/Deeper/x", "/old/deeperx", "/older", "/caf%C3%A9", "/CAF%C3%89/",
		"/caf%c3%a9?q=1", "/old/keep?a=1&b=%20", "/x/", "/wp-content/uploads/pic.jpg"} {
		u, _ := url.Parse(p)
		a := req("b.test", p, false)
		i := MatchRedirect(edgeRedirects, u.Path)
		if i < 0 {
			if a.code >= 300 && a.code < 400 {
				t.Errorf("GET %s: redirected (%+v), MatchRedirect says no rule", p, a)
			}
			continue
		}
		if want := RedirectLocation(edgeRedirects[i], u.RawQuery); a.code != edgeRedirects[i].Code || a.loc != want {
			t.Errorf("GET %s: %d %q, MatchRedirect says rule %d: %d %q", p, a.code, a.loc, i, edgeRedirects[i].Code, want)
		}
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

func waitUp(t *testing.T, base string, logs func() string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if base != "" {
			if code, _ := tryGet(base, "b.test", "/wp-content/uploads/pic.jpg"); code == 200 {
				return
			}
		}
	}
	t.Fatalf("caddy did not come up:\n%s", logs())
}
