package shield

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClassifyUA(t *testing.T) {
	cases := map[string]Class{
		"":                                     ClassScript,
		"Mozilla/5.0 (Macintosh) Safari/605.1": ClassHuman,
		"Mozilla/5.0 (compatible; GPTBot/1.2; +https://openai.com/gptbot)":                                        ClassAIBot,
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)": ClassAIBot,
		"sqlmap/1.7#stable (https://sqlmap.org)":                                                                  ClassAttackTool,
		"WPScan v3.8.25 (https://wpscan.com/wordpress-security-scanner)":                                          ClassAttackTool,
		"python-requests/2.31.0": ClassScript,
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)": ClassVerifiedCrawler,
	}
	for ua, want := range cases {
		if got := ClassifyUA(ua); got != want {
			t.Errorf("ClassifyUA(%q) = %v, want %v", ua, got, want)
		}
	}
}

type fakeResolver struct {
	ptr map[string][]string
	a   map[string][]string
}

func (f fakeResolver) LookupAddr(_ context.Context, ip string) ([]string, error) {
	if v, ok := f.ptr[ip]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: ip, IsNotFound: true}
}

func (f fakeResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if v, ok := f.a[h]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: h, IsNotFound: true}
}

func TestCrawlerVerification(t *testing.T) {
	r := fakeResolver{
		ptr: map[string][]string{
			"66.249.66.1": {"crawl-66-249-66-1.googlebot.com."},
			"6.6.6.6":     {"crawl-6-6-6-6.googlebot.com."}, // attacker-controlled PTR…
		},
		a: map[string][]string{
			"crawl-66-249-66-1.googlebot.com": {"66.249.66.1"},
			"crawl-6-6-6-6.googlebot.com":     {"66.249.66.99"}, // …but Google's forward zone disagrees
		},
	}
	s := New(Options{Secret: []byte("k"), Resolver: r, Sites: nil})
	ua := "Mozilla/5.0 (compatible; Googlebot/2.1)"
	if c := s.Classify(context.Background(), "66.249.66.1", ua); c != ClassVerifiedCrawler {
		t.Errorf("real googlebot = %v", c)
	}
	if c := s.Classify(context.Background(), "6.6.6.6", ua); c != ClassSpoofedCrawler {
		t.Errorf("spoofed PTR = %v", c)
	}
	if c := s.Classify(context.Background(), "1.2.3.4", ua); c != ClassSpoofedCrawler {
		t.Errorf("no PTR = %v", c)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(1, 3) // 1/s, burst 3
	now := time.Unix(1000, 0)
	for i := range 3 {
		if !rl.allow("k", now) {
			t.Fatalf("request %d within burst denied", i)
		}
	}
	if rl.allow("k", now) {
		t.Fatal("4th request in same instant allowed")
	}
	if !rl.allow("other", now) {
		t.Fatal("keys must be independent")
	}
	if !rl.allow("k", now.Add(1100*time.Millisecond)) {
		t.Fatal("token not refilled after 1.1s")
	}
	rl.sweep(now.Add(time.Hour))
	for i := range rl.shards {
		if n := len(rl.shards[i].buckets); n != 0 {
			t.Fatalf("sweep left %d buckets", n)
		}
	}
}

func TestTokenTamper(t *testing.T) {
	secret := []byte("secret")
	tok, _ := sign(secret, passPayload{Site: "a", Expires: 99})
	var p passPayload
	if err := open(secret, tok, &p); err != nil || p.Site != "a" {
		t.Fatalf("roundtrip: %v %+v", err, p)
	}
	body, sig, _ := strings.Cut(tok, ".")
	forged, _ := sign([]byte("other"), passPayload{Site: "b"})
	fbody, _, _ := strings.Cut(forged, ".")
	for _, bad := range []string{fbody + "." + sig, body + "." + sig[:len(sig)-2], body, ""} {
		if open(secret, bad, &p) == nil {
			t.Errorf("accepted tampered token %q", bad)
		}
	}
}

func TestIPBucket(t *testing.T) {
	if ipBucket("203.0.113.7") != ipBucket("203.0.113.250") {
		t.Error("same /24 should share a bucket")
	}
	if ipBucket("203.0.113.7") == ipBucket("203.0.114.7") {
		t.Error("different /24 must differ")
	}
	if ipBucket("2001:db8:1:2:aaaa::1") != ipBucket("2001:db8:1:2:bbbb::9") {
		t.Error("same /64 should share a bucket")
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/shop?x=1": "/shop?x=1", "": "/", "https://evil.com": "/",
		"//evil.com": "/", "/\\evil.com": "/", "/a\r\nSet-Cookie: x": "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestChallengeFlow drives the full browser flow through the handlers:
// challenge page -> solve PoW -> verify -> pass cookie -> allowed.
// It uses a policy-independent path by calling serveChallenge directly.
func TestChallengeFlow(t *testing.T) {
	sites := func(id string) (SiteSettings, bool) {
		return SiteSettings{ID: "s1", Mode: ModeStandard}, id == "s1"
	}
	s := New(Options{Secret: []byte("0123456789abcdef0123456789abcdef"), Sites: sites, Difficulty: 8})
	const ip, ua = "198.51.100.23", "Mozilla/5.0 Test"

	rec := httptest.NewRecorder()
	s.serveChallenge(rec, SiteSettings{ID: "s1", Mode: ModeStandard}, ip, "/checkout?step=2", time.Now())
	if rec.Code != http.StatusForbidden || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("challenge response: %d %v", rec.Code, rec.Header())
	}
	token := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(rec.Body.String())[1]

	nonce := ""
	for n := 0; ; n++ {
		if leadingZeroBits(token, strconv.Itoa(n)) >= 8 {
			nonce = strconv.Itoa(n)
			break
		}
	}

	verify := func(ip, nonce string) *httptest.ResponseRecorder {
		form := url.Values{"token": {token}, "nonce": {nonce}, "return": {"/checkout?step=2"}}
		req := httptest.NewRequest("POST", "/_shield/verify", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set(SiteHeader, "s1")
		req.Header.Set("X-Forwarded-For", ip)
		req.Header.Set("User-Agent", ua)
		rec := httptest.NewRecorder()
		s.VerifyHandler().ServeHTTP(rec, req)
		return rec
	}

	if rec := verify("192.0.2.1", nonce); rec.Code != http.StatusForbidden {
		t.Errorf("token replayed from another network: %d", rec.Code)
	}
	wrong := nonce + "0"
	for leadingZeroBits(token, wrong) >= 8 {
		wrong += "0"
	}
	if rec := verify(ip, wrong); rec.Code != http.StatusForbidden {
		t.Errorf("wrong nonce accepted: %d", rec.Code)
	}

	rec = verify(ip, nonce)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/checkout?step=2" {
		t.Fatalf("verify: %d %v", rec.Code, rec.Header())
	}
	cookie := rec.Result().Cookies()[0]

	req := httptest.NewRequest("GET", "/_shield/check", nil)
	req.AddCookie(cookie)
	if !s.validPass(req, "s1", ip, ua, time.Now()) {
		t.Error("fresh pass rejected")
	}
	if s.validPass(req, "s1", ip, "curl/8.0", time.Now()) {
		t.Error("pass must be bound to the User-Agent")
	}
	if s.validPass(req, "s2", ip, ua, time.Now()) {
		t.Error("pass must be bound to the site")
	}
	if s.validPass(req, "s1", ip, ua, time.Now().Add(13*time.Hour)) {
		t.Error("expired pass accepted")
	}
}

func TestCheckUnknownSiteFailsOpen(t *testing.T) {
	s := New(Options{Secret: []byte("k"), Sites: func(string) (SiteSettings, bool) { return SiteSettings{}, false }})
	rec := httptest.NewRecorder()
	s.CheckHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/_shield/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown site = %d, want 200", rec.Code)
	}
}
