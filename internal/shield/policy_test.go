package shield

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	std := func(mut func(*Signals)) Signals {
		s := Signals{Mode: ModeStandard, BlockAIBots: true, Class: ClassHuman}
		if mut != nil {
			mut(&s)
		}
		return s
	}
	cases := []struct {
		name string
		sig  Signals
		want Verdict
	}{
		{"human", std(nil), Allow},
		{"shield off allows attack tools", Signals{Mode: ModeOff, Class: ClassAttackTool}, Allow},
		{"trusted skips everything", std(func(s *Signals) { s.Trusted, s.Banned, s.Threat, s.RateExceeded = true, true, ThreatSQLi, true }), Allow},

		{"attack tool", std(func(s *Signals) { s.Class = ClassAttackTool }), Block},
		{"spoofed googlebot", std(func(s *Signals) { s.Class = ClassSpoofedCrawler }), Block},
		{"ai bot blocked", std(func(s *Signals) { s.Class = ClassAIBot }), Block},
		{"ai bot allowed by site", std(func(s *Signals) { s.Class, s.BlockAIBots = ClassAIBot, false }), Allow},
		{"script (monitor, webhook) allowed", std(func(s *Signals) { s.Class = ClassScript }), Allow},
		{"verified crawler", std(func(s *Signals) { s.Class = ClassVerifiedCrawler }), Allow},

		{"banned", std(func(s *Signals) { s.Banned = true }), Block},
		{"banned even with a pass", std(func(s *Signals) { s.Banned, s.HasPass = true, true }), Block},
		{"sql injection", std(func(s *Signals) { s.Threat = ThreatSQLi }), Block},
		{"a pass never excuses an attack", std(func(s *Signals) { s.Threat, s.HasPass = ThreatXSS, true }), Block},
		{"admin allowlist", std(func(s *Signals) { s.AdminDenied = true }), Block},

		{"human over budget is challenged", std(func(s *Signals) { s.RateExceeded = true }), Challenge},
		{"a pass doesn't lift rate limits", std(func(s *Signals) { s.RateExceeded, s.HasPass = true, true }), Throttle},
		{"login brute force", std(func(s *Signals) { s.RateExceeded, s.LoginPath = true, true }), Throttle},
		{"crawler over budget slows down, never challenged", std(func(s *Signals) { s.RateExceeded, s.Class = true, ClassVerifiedCrawler }), Throttle},
		{"script over budget", std(func(s *Signals) { s.RateExceeded, s.Class = true, ClassScript }), Throttle},

		{"under attack: human challenged", Signals{Mode: ModeUnderAttack, Class: ClassHuman}, Challenge},
		{"under attack: pass holder allowed", Signals{Mode: ModeUnderAttack, Class: ClassHuman, HasPass: true}, Allow},
		{"under attack: googlebot allowed", Signals{Mode: ModeUnderAttack, Class: ClassVerifiedCrawler}, Allow},
		{"under attack: scripts challenged", Signals{Mode: ModeUnderAttack, Class: ClassScript}, Challenge},
		{"under attack: attack tool still blocked", Signals{Mode: ModeUnderAttack, Class: ClassAttackTool, HasPass: true}, Block},
	}
	for _, c := range cases {
		if got := Decide(c.sig); got != c.want {
			t.Errorf("%s: Decide = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStrikes(t *testing.T) {
	sqli := Signals{Mode: ModeStandard, Class: ClassScript, Threat: ThreatSQLi}
	if strikes(sqli, Decide(sqli)) != 1 {
		t.Error("an SQL injection must count towards a ban")
	}
	crawler := Signals{Mode: ModeStandard, Class: ClassVerifiedCrawler, Threat: ThreatSQLi}
	if strikes(crawler, Decide(crawler)) != 0 {
		t.Error("Googlebot crawling a spam link must never be banned")
	}
	for name, s := range map[string]Signals{
		"rate limit":      {Mode: ModeStandard, RateExceeded: true},
		"admin allowlist": {Mode: ModeStandard, AdminDenied: true},
		"ai bot":          {Mode: ModeStandard, Class: ClassAIBot, BlockAIBots: true},
	} {
		if n := strikes(s, Decide(s)); n != 0 {
			t.Errorf("%s: %d strikes; an honest client can trigger it, so it must not ban", name, n)
		}
	}
	login := Signals{Mode: ModeStandard, LoginPath: true, RateExceeded: true}
	if strikes(login, Decide(login)) != 1 {
		t.Error("login brute force beyond the budget must count")
	}
}

func TestBanEscalation(t *testing.T) {
	b := newBanList()
	now := time.Unix(1_000_000, 0)
	for i := range BanThreshold - 1 {
		if banned, _ := b.strike("1.2.3.4", 1, "sql_injection", now); banned {
			t.Fatalf("banned after %d strikes", i+1)
		}
	}
	banned, until := b.strike("1.2.3.4", 1, "sql_injection", now)
	if !banned || until.Sub(now) != BanBase {
		t.Fatalf("5th strike: banned=%v for %v, want %v", banned, until.Sub(now), BanBase)
	}
	if !b.banned("1.2.3.4", now.Add(BanBase-time.Second)) || b.banned("1.2.3.4", now.Add(BanBase)) {
		t.Fatal("ban must last exactly BanBase")
	}
	if b.banned("1.2.3.5", now) {
		t.Fatal("IPv4 bans must be exact")
	}

	// Coming back after the ban: the next ban lasts twice as long.
	later := now.Add(BanBase + time.Minute)
	for range BanThreshold {
		banned, until = b.strike("1.2.3.4", 1, "probe", later)
	}
	if !banned || until.Sub(later) != 2*BanBase {
		t.Fatalf("repeat ban lasts %v, want %v", until.Sub(later), 2*BanBase)
	}

	// Strikes spread wider than the window never add up to a ban.
	for i := range 2 * BanThreshold {
		if banned, _ := b.strike("5.6.7.8", 1, "probe", now.Add(time.Duration(i)*(BanWindow+time.Second))); banned {
			t.Fatal("strikes outside the window accumulated")
		}
	}

	if !b.unban("1.2.3.4") || b.banned("1.2.3.4", later) {
		t.Fatal("unban failed")
	}
	b.sweep(later.Add(48 * time.Hour))
	for i := range b.shards {
		if n := len(b.shards[i].m); n != 0 {
			t.Fatalf("sweep kept %d stale offenders", n)
		}
	}
}

func TestBanKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":           "203.0.113.9",
		"::ffff:203.0.113.9":    "203.0.113.9",
		"2001:db8:1:2:aaaa::1":  "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::99": "2001:db8:1:2::/64",
	} {
		if got, ok := BanKey(in); !ok || got != want {
			t.Errorf("BanKey(%q) = %q, want %q", in, got, want)
		}
	}
	if _, ok := BanKey("not-an-ip"); ok {
		t.Error("garbage accepted")
	}
}

// checkRequest sends one forward_auth check the way Caddy does.
func checkRequest(s *Shield, ip, method, uri, ua string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/_shield/check", nil)
	req.Header.Set(SiteHeader, "s1")
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("X-Forwarded-Method", method)
	req.Header.Set("X-Forwarded-Uri", uri)
	req.Header.Set("User-Agent", ua)
	rec := httptest.NewRecorder()
	s.CheckHandler().ServeHTTP(rec, req)
	return rec
}

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Safari/605.1.15"

func TestCheckBansScannerOnEverySite(t *testing.T) {
	settings := map[string]SiteSettings{
		"s1": {ID: "s1", Mode: ModeStandard, Inspect: true},
		"s2": {ID: "s2", Mode: ModeStandard, Inspect: true},
	}
	s := New(Options{Secret: []byte("0123456789abcdef0123456789abcdef"),
		Sites: func(id string) (SiteSettings, bool) { st, ok := settings[id]; return st, ok }})
	const ip = "198.51.100.7"
	for range BanThreshold {
		if rec := checkRequest(s, ip, "GET", "/?id=1%27%20UNION%20SELECT%20user_pass%20FROM%20wp_users--", "python-requests/2.31"); rec.Code != http.StatusForbidden {
			t.Fatalf("sqli: %d", rec.Code)
		}
	}
	// The same client now gets nothing, not even a harmless page on another site.
	req := httptest.NewRequest("GET", "/_shield/check", nil)
	req.Header.Set(SiteHeader, "s2")
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("X-Forwarded-Uri", "/about/")
	req.Header.Set("User-Agent", browserUA)
	rec := httptest.NewRecorder()
	s.CheckHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("banned client reached another site: %d", rec.Code)
	}
	if bans := s.Bans(); len(bans) != 1 || bans[0].Addr != ip || bans[0].Reason != "sql_injection" {
		t.Fatalf("bans = %+v", bans)
	}
	if ev := s.Events("s2", 10); len(ev) != 1 || ev[0].Reason != "banned" {
		t.Fatalf("s2 events = %+v", ev)
	}
	if ok, err := s.Unban(ip); !ok || err != nil {
		t.Fatalf("unban: %v %v", ok, err)
	}
	if rec := checkRequest(s, ip, "GET", "/about/", browserUA); rec.Code != http.StatusOK {
		t.Fatalf("after unban: %d", rec.Code)
	}
}

func TestCheckTrustedAndAdminAllowlist(t *testing.T) {
	office := netip.MustParsePrefix("203.0.113.0/24")
	monitor := netip.MustParsePrefix("192.0.2.10/32")
	s := New(Options{Secret: []byte("0123456789abcdef0123456789abcdef"), HealthToken: "health-secret",
		Sites: func(string) (SiteSettings, bool) {
			return SiteSettings{ID: "s1", Mode: ModeUnderAttack, Inspect: true,
				AdminAllow: []netip.Prefix{office}, Trusted: []netip.Prefix{monitor}}, true
		}})

	// Trusted monitors skip even under attack mode...
	if rec := checkRequest(s, "192.0.2.10", "GET", "/", "Go-http-client/1.1"); rec.Code != http.StatusOK {
		t.Errorf("trusted monitor: %d", rec.Code)
	}
	// ...but loopback alone proves nothing: a local tunnel forwards the
	// whole internet from 127.0.0.1. The daemon's checks carry the token.
	if rec := checkRequest(s, "127.0.0.1", "GET", "/", browserUA); rec.Code == http.StatusOK {
		t.Error("loopback trusted without the health token")
	}
	for tok, want := range map[string]int{"health-secret": http.StatusOK, "wrong": http.StatusForbidden} {
		req := httptest.NewRequest("GET", "/_shield/check", nil)
		req.Header.Set(SiteHeader, "s1")
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		req.Header.Set("X-Forwarded-Uri", "/")
		req.Header.Set(HealthHeader, tok)
		rec := httptest.NewRecorder()
		s.CheckHandler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("health token %q: %d, want %d", tok, rec.Code, want)
		}
	}
	// Application passwords reach the admin through the REST API.
	req := httptest.NewRequest("GET", "/_shield/check", nil)
	req.Header.Set(SiteHeader, "s1")
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.Header.Set("X-Forwarded-Uri", "/wp-json/wp/v2/posts")
	req.Header.Set("X-Forwarded-Method", "POST")
	req.Header.Set("Authorization", "Basic YWRtaW46eHh4")
	rec := httptest.NewRecorder()
	s.CheckHandler().ServeHTTP(rec, req)
	if rec.Header().Get(VerdictHeader) != "block" {
		t.Error("authenticated REST request from outside the admin allowlist allowed")
	}
	for uri, want := range map[string]int{
		"/wp-login.php":                   http.StatusForbidden,
		"/wp-login.php/x":                 http.StatusForbidden, // PATH_INFO still runs wp-login.php
		"//wp-admin/./options.php":        http.StatusForbidden,
		"/wp-admin/":                      http.StatusForbidden,
		"/wp-admin/admin-ajax.php?a=cart": http.StatusForbidden, // public, but under attack mode: challenge (403 page)
		"/%77p-login.php?redirect_to=%2F": http.StatusForbidden,
	} {
		rec := checkRequest(s, "198.51.100.1", "GET", uri, browserUA)
		if rec.Code != want {
			t.Errorf("%s from outside the allowlist: %d, want %d", uri, rec.Code, want)
		}
		if isAdminPath(ScriptPath(uri)) && rec.Header().Get(VerdictHeader) != "block" {
			t.Errorf("%s: verdict %q, want block", uri, rec.Header().Get(VerdictHeader))
		}
	}
	if rec := checkRequest(s, "203.0.113.50", "GET", "/wp-admin/", browserUA); rec.Header().Get(VerdictHeader) == "block" {
		t.Error("office IP blocked from wp-admin")
	}
}

func TestNobodyCanGetSomeoneElseBanned(t *testing.T) {
	sqli := Signals{Mode: ModeStandard, Class: ClassHuman, Threat: ThreatSQLi}
	if Decide(sqli) != Block || strikes(sqli, Block) != 0 {
		t.Error("a browser search that looks like SQL: block, but never count towards a ban")
	}
	embedded := Signals{Mode: ModeStandard, Class: ClassHuman, Threat: ThreatTraversal, CrossSite: true}
	if strikes(embedded, Decide(embedded)) != 0 {
		t.Error("an attack URL embedded in another site's page must not ban its visitors")
	}
	tool := Signals{Mode: ModeStandard, Class: ClassScript, Threat: ThreatSQLi}
	if strikes(tool, Decide(tool)) != 1 {
		t.Error("scripts sending SQL injection must still be banned")
	}
}

func TestIPv6RateLimitSharedPer64(t *testing.T) {
	s := New(Options{Secret: []byte("0123456789abcdef0123456789abcdef"), LoginPerMinute: 1, LoginBurst: 2,
		Sites: func(string) (SiteSettings, bool) { return SiteSettings{ID: "s1", Mode: ModeStandard}, true }})
	var codes []int
	for _, ip := range []string{"2001:db8:1:2::1", "2001:db8:1:2::2", "2001:db8:1:2::3"} {
		codes = append(codes, checkRequest(s, ip, "POST", "/wp-login.php", browserUA).Code)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Fatalf("rotating addresses in one /64: %v", codes)
	}
}

func TestLoginLimitCannotBeBypassedWithPathInfo(t *testing.T) {
	s := New(Options{Secret: []byte("0123456789abcdef0123456789abcdef"), LoginPerMinute: 1, LoginBurst: 2,
		Sites: func(string) (SiteSettings, bool) { return SiteSettings{ID: "s1", Mode: ModeStandard}, true }})
	codes := []int{}
	for _, uri := range []string{"/wp-login.php", "/wp-login.php/a", "/wp-login.php/b", "/./wp-login.php"} {
		codes = append(codes, checkRequest(s, "198.51.100.2", "POST", uri, browserUA).Code)
	}
	if codes[2] != http.StatusTooManyRequests || codes[3] != http.StatusTooManyRequests {
		t.Fatalf("login attempts via path variants = %v, want the 3rd and 4th throttled", codes)
	}
}
