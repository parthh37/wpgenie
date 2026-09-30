package shield

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Unix(60*20_000_000, 0) // on a minute boundary

func newTestDetector() (*attackDetector, *[]attackChange) {
	var got []attackChange
	return newAttackDetector(func(c attackChange) { got = append(got, c) }), &got
}

// traffic sends perMinute requests a minute, spread evenly over the
// buckets, for the given minutes, and returns the time after the last one.
func traffic(d *attackDetector, id string, auto bool, from time.Time, perMinute, minutes int) time.Time {
	for m := range minutes {
		for b := range attackBuckets {
			at := from.Add(time.Duration(m)*time.Minute + time.Duration(b)*attackBucket)
			n := perMinute / attackBuckets
			if b < perMinute%attackBuckets {
				n++
			}
			for range n {
				d.record(id, auto, at)
			}
		}
	}
	return from.Add(time.Duration(minutes) * time.Minute)
}

func allAuto(string) bool { return true }

func active(d *attackDetector, id string) bool {
	_, ok := d.attack(id)
	return ok
}

func TestAttackFloor(t *testing.T) {
	d, got := newTestDetector()
	now := traffic(d, "quiet", true, t0, AttackFloor-6, 1)
	traffic(d, "busy", true, t0, AttackFloor, 1)
	d.tick(now, allAuto) // no request needed to judge the minute that just ended
	if active(d, "quiet") {
		t.Fatal("under the floor: attack started")
	}
	st, ok := d.attack("busy")
	if !ok {
		t.Fatal("at the floor: no attack")
	}
	if !st.Since.Equal(now) || !st.Until.Equal(now.Add(AttackHold)) || st.Peak != AttackFloor ||
		st.Reason != "1,200 requests in the last minute" {
		t.Fatalf("state %+v", st)
	}
	if len(*got) != 1 || !(*got)[0].active || (*got)[0].site != "busy" {
		t.Fatalf("changes %+v", *got)
	}
	if a := d.attacks(); len(a) != 1 || a["busy"] != st {
		t.Fatalf("attacks %+v", a)
	}
}

func TestAttackBaseline(t *testing.T) {
	d, _ := newTestDetector()
	// After warm-up, a site that normally gets 300 a minute: 1,400 is over
	// the floor but under 5x normal...
	now := traffic(d, "a", true, t0, 300, AttackWarmup+2)
	traffic(d, "b", true, t0, 300, AttackWarmup+2)
	traffic(d, "a", true, now, 1400, 1)
	end := traffic(d, "b", true, now, 1600, 1)
	d.tick(end, allAuto)
	if active(d, "a") {
		t.Error("1,400 a minute on a site normally at 300 started an attack")
	}
	// ...1,600 isn't.
	if st, ok := d.attack("b"); !ok || st.Reason != "1,600 requests in the last minute (normally about 300)" {
		t.Errorf("1,600 a minute on a site normally at 300: %v %+v", ok, st)
	}
}

func TestAttackBusySite(t *testing.T) {
	d, got := newTestDetector()
	// A busy site right after a restart: over the floor, with nothing to
	// compare to yet. It's challenged until the baseline says this is
	// normal, then released at once rather than after AttackHold.
	now := traffic(d, "s", true, t0, 2000, AttackWarmup+3)
	if active(d, "s") || len(*got) != 2 || !(*got)[0].active || (*got)[1].why != "the traffic turned out to be normal for this site" {
		t.Fatalf("warm-up on a busy site: changes %+v", *got)
	}
	if end := (*got)[1].at; end.Sub(t0) > time.Duration(AttackWarmup+2)*time.Minute {
		t.Errorf("released %v after start", end.Sub(t0))
	}
	// Steady traffic well above the floor never triggers: the threshold
	// is max(1,200, 5x2,000) = 10,000.
	now = traffic(d, "s", true, now, 2000, 30)
	now = traffic(d, "s", true, now, 9000, 1)
	d.tick(now, allAuto)
	if active(d, "s") || len(*got) != 2 {
		t.Fatalf("steady busy site: changes %+v", (*got)[2:])
	}
	now = traffic(d, "s", true, now, 15000, 1)
	d.tick(now, allAuto)
	if st, ok := d.attack("s"); !ok || !strings.Contains(st.Reason, " requests in the last minute (normally about 2,") {
		t.Fatalf("15,000 on a site normally at 2,000: %v %+v", ok, st)
	}
}

func TestAttackSpread(t *testing.T) {
	d, _ := newTestDetector()
	for i := range AttackClients {
		if i < AttackClients-1 {
			tr, _ := d.record("few", true, t0)
			tr.refuse(fmt.Sprintf("198.51.100.%d", i))
			tr.refuse(fmt.Sprintf("198.51.100.%d", i)) // the same client twice counts once
		}
		tr, _ := d.record("many", true, t0)
		tr.refuse(fmt.Sprintf("198.51.100.%d", i))
	}
	d.tick(t0.Add(time.Minute), allAuto)
	if active(d, "few") {
		t.Error("29 refused clients started an attack")
	}
	if st, ok := d.attack("many"); !ok || st.Reason != "30 different addresses were throttled or blocked in the last minute" {
		t.Errorf("30 refused clients: %v %+v", ok, st)
	}
}

func TestAttackRefusedClientsBounded(t *testing.T) {
	d, _ := newTestDetector()
	tr, _ := d.record("s", true, t0)
	for i := range 5000 {
		tr.refuse(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	if n := len(tr.refused); n != attackMaxClients {
		t.Fatalf("%d refused clients kept, want %d", n, attackMaxClients)
	}
	d.tick(t0.Add(time.Minute), allAuto)
	if st, ok := d.attack("s"); !ok || !strings.HasPrefix(st.Reason, "at least 256 different") {
		t.Fatalf("%v %+v", ok, st)
	}
	// Refusals out of the window are forgotten.
	d.tick(t0.Add(2*time.Minute), allAuto)
	if n := len(tr.refused); n != 0 {
		t.Fatalf("%d stale refusals kept", n)
	}
}

func TestAttackHoldAndEnd(t *testing.T) {
	d, got := newTestDetector()
	now := traffic(d, "s", true, t0, AttackFloor, 1)
	d.tick(now, allAuto)
	start := now
	// A lull shorter than AttackHold doesn't end it...
	now = traffic(d, "s", true, now, 10, 10)
	d.tick(now, allAuto)
	if !active(d, "s") {
		t.Fatal("ended during a lull")
	}
	// ...and a second wave starts the hold over.
	now = traffic(d, "s", true, now, 1500, 1)
	d.tick(now, allAuto)
	st, ok := d.attack("s")
	if !ok || !st.Since.Equal(start) || !st.Until.Equal(now.Add(AttackHold)) || st.Peak != 1500 {
		t.Fatalf("after the second wave: %v %+v", ok, st)
	}
	lastWave := now
	now = traffic(d, "s", true, now, 10, int(AttackHold/time.Minute)-1)
	d.tick(now, allAuto)
	// The sliding window still saw most of the wave a bucket later.
	st, ok = d.attack("s")
	if !ok || st.Until.Before(lastWave.Add(AttackHold)) || st.Until.After(lastWave.Add(AttackHold+attackBucket)) {
		t.Fatalf("ended before AttackHold, or held too long: %v %+v", ok, st)
	}
	// The traffic stops altogether: Run's tick ends it on time.
	d.tick(st.Until.Add(-time.Second), allAuto)
	if !active(d, "s") {
		t.Fatal("ended early")
	}
	d.tick(st.Until, allAuto)
	if active(d, "s") {
		t.Fatal("still under attack after AttackHold without a flood")
	}
	if len(*got) != 2 {
		t.Fatalf("changes %+v", *got)
	}
	end := (*got)[1]
	if end.active || end.why != "no flood for 15 minutes" || !end.state.Since.Equal(start) ||
		!end.state.Until.Equal(st.Until) || end.state.Peak != 1500 {
		t.Fatalf("end %+v", end)
	}
}

func TestAttackBurstThenSilence(t *testing.T) {
	d, _ := newTestDetector()
	// All in the last bucket before the traffic stops; the next look is a
	// minute later.
	traffic(d, "s", true, t0, 0, 1)
	for range AttackFloor {
		d.record("s", true, t0.Add(50*time.Second))
	}
	d.tick(t0.Add(2*time.Minute), allAuto)
	if !active(d, "s") {
		t.Fatal("a burst followed by silence slipped through")
	}
}

func TestEndAttack(t *testing.T) {
	d, got := newTestDetector()
	now := traffic(d, "s", true, t0, AttackFloor, 1)
	d.tick(now, allAuto)
	if !d.endNow("s", now) || active(d, "s") {
		t.Fatal("EndAttack didn't end it")
	}
	if d.endNow("s", now) || d.endNow("nope", now) {
		t.Error("ended an attack that wasn't running")
	}
	if len(*got) != 2 || (*got)[1].active || (*got)[1].why != "ended by hand" || !(*got)[1].state.Until.Equal(now) {
		t.Fatalf("changes %+v", *got)
	}
	// The minute that started it doesn't start it again...
	d.record("s", true, now)
	d.tick(now.Add(attackBucket), allAuto)
	if active(d, "s") {
		t.Fatal("restarted by the minute before EndAttack")
	}
	// ...but a flood that goes on does.
	now = traffic(d, "s", true, now.Add(attackBucket), AttackFloor, 1)
	d.tick(now, allAuto)
	if !active(d, "s") {
		t.Fatal("a continuing flood didn't restart it")
	}
}

func TestAttackOnlyInAutoMode(t *testing.T) {
	d, got := newTestDetector()
	now := traffic(d, "s", false, t0, 5000, 1)
	d.tick(now, func(string) bool { return false })
	if active(d, "s") || len(*got) != 0 {
		t.Fatal("a Standard site was escalated")
	}
	// Switching to Auto during a flood escalates at the next minute that
	// qualifies; switching away ends it.
	now = traffic(d, "s", true, now, 5000, 1)
	d.tick(now, allAuto)
	if !active(d, "s") {
		t.Fatal("not escalated after switching to Auto")
	}
	d.record("s", false, now)
	if active(d, "s") || (*got)[len(*got)-1].why != "the site's protection mode was changed" {
		t.Fatalf("switching away from Auto: %+v", *got)
	}
}

func TestAttackSweep(t *testing.T) {
	d, _ := newTestDetector()
	idle, _ := d.record("idle", true, t0)
	d.record("recent", true, t0.Add(30*time.Minute))
	d.tick(t0.Add(attackIdle), allAuto)
	if _, ok := d.sites.Load("idle"); ok {
		t.Error("idle site kept")
	}
	if _, ok := d.sites.Load("recent"); !ok {
		t.Error("recently seen site swept")
	}
	// A request holding the swept state starts over rather than counting
	// into a site nobody can see.
	if tr, _ := d.record("idle", true, t0.Add(attackIdle)); tr == idle || !idle.dead {
		t.Error("swept state reused")
	}
}

func TestAttackConcurrent(t *testing.T) {
	d, _ := newTestDetector()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				at := t0.Add(time.Duration(i) * 50 * time.Millisecond)
				tr, _ := d.record(fmt.Sprint("s", g%2), true, at)
				tr.refuse(fmt.Sprint("10.0.0.", i%50))
				if i%100 == 0 {
					d.tick(at, allAuto)
					d.attacks()
				}
			}
		})
	}
	wg.Wait()
}

func TestModeAuto(t *testing.T) {
	if !ModeAuto.Valid() || Mode("bogus").Valid() {
		t.Error("Valid")
	}
	for _, sig := range []Signals{
		{Class: ClassHuman}, {Class: ClassScript}, {Class: ClassAIBot, BlockAIBots: true},
		{Class: ClassHuman, RateExceeded: true}, {Class: ClassHuman, Country: Challenge},
	} {
		auto, std := sig, sig
		auto.Mode, std.Mode = ModeAuto, ModeStandard
		if Decide(auto) != Decide(std) {
			t.Errorf("%+v: auto %v, standard %v", sig, Decide(auto), Decide(std))
		}
	}
	sig := Signals{Mode: ModeUnderAttack, Class: ClassHuman}
	if reasonFor(sig, Challenge) != "under_attack" {
		t.Error("manual under attack reason")
	}
	sig.AutoAttack = true
	if reasonFor(sig, Challenge) != "under_attack_auto" {
		t.Error("automatic under attack reason")
	}
}

func TestFloodRefusal(t *testing.T) {
	for name, c := range map[string]struct {
		sig  Signals
		want bool
	}{
		"rate limit":       {Signals{Mode: ModeStandard, Class: ClassHuman, RateExceeded: true}, true},
		"login brute":      {Signals{Mode: ModeStandard, Class: ClassScript, RateExceeded: true, LoginPath: true}, true},
		"banned":           {Signals{Mode: ModeStandard, Class: ClassHuman, Banned: true}, true},
		"attack tool":      {Signals{Mode: ModeStandard, Class: ClassAttackTool}, true},
		"under attack":     {Signals{Mode: ModeUnderAttack, Class: ClassHuman}, false},
		"country":          {Signals{Mode: ModeStandard, Class: ClassHuman, Country: Block}, false},
		"ai bot policy":    {Signals{Mode: ModeStandard, Class: ClassAIBot, BlockAIBots: true}, false},
		"browser sql-ish":  {Signals{Mode: ModeStandard, Class: ClassHuman, Threat: ThreatSQLi}, false},
		"allowed":          {Signals{Mode: ModeStandard, Class: ClassHuman}, false},
		"admin allowlist":  {Signals{Mode: ModeStandard, Class: ClassHuman, AdminDenied: true}, false},
		"deny list script": {Signals{Mode: ModeStandard, Class: ClassScript, Denied: true}, false},
	} {
		if got := floodRefusal(c.sig, Decide(c.sig)); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestCommas(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4210: "-4,210"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q", n, got)
		}
	}
}

// floodShield is a shield for site s1 in the given mode, on a clock the
// test moves by hand.
func floodShield(mode Mode, onAttack func(string, bool, AttackState)) (*Shield, *time.Time, *Mode) {
	clock := t0
	r := fakeResolver{
		ptr: map[string][]string{"66.249.66.1": {"crawl-66-249-66-1.googlebot.com."}},
		a:   map[string][]string{"crawl-66-249-66-1.googlebot.com": {"66.249.66.1"}},
	}
	s := New(Options{Secret: []byte("0123456789abcdef0123456789abcdef"), Resolver: r, OnAttack: onAttack,
		Sites: func(id string) (SiteSettings, bool) { return SiteSettings{ID: "s1", Mode: mode}, id == "s1" }})
	s.now = func() time.Time { return clock }
	return s, &clock, &mode
}

// flood sends AttackFloor browser requests from different addresses over a
// minute, none of them over its own limit, and moves the clock to the end.
func flood(s *Shield, clock *time.Time) {
	start := *clock
	for b := range attackBuckets {
		*clock = start.Add(time.Duration(b) * attackBucket)
		for i := range AttackFloor / attackBuckets {
			checkRequest(s, fmt.Sprintf("10.%d.%d.%d", b, i/256, i%256), "GET", "/", browserUA)
		}
	}
	*clock = start.Add(time.Minute)
}

func TestCheckAutoAttack(t *testing.T) {
	type call struct {
		active bool
		st     AttackState
	}
	calls := make(chan call, 4)
	s, clock, _ := floodShield(ModeAuto, func(site string, active bool, st AttackState) {
		if site == "s1" {
			calls <- call{active, st}
		}
	})
	flood(s, clock)

	// A visitor without a pass is challenged, at Under attack's difficulty.
	rec := checkRequest(s, "198.51.100.10", "GET", "/", browserUA)
	if rec.Code != http.StatusForbidden || rec.Header().Get(VerdictHeader) != "challenge" {
		t.Fatalf("browser during an automatic attack: %d %q", rec.Code, rec.Header().Get(VerdictHeader))
	}
	token := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(rec.Body.String())[1]
	var p challengePayload
	if err := open(s.o.Secret, token, &p); err != nil || p.Difficulty != s.o.Difficulty+2 {
		t.Errorf("challenge difficulty %d, want %d", p.Difficulty, s.o.Difficulty+2)
	}
	// Verified search engines and pass holders get through.
	if rec := checkRequest(s, "66.249.66.1", "GET", "/", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"); rec.Code != http.StatusOK {
		t.Errorf("googlebot during an automatic attack: %d", rec.Code)
	}
	const ip = "198.51.100.11"
	pass, _ := sign(s.o.Secret, passPayload{Site: "s1", Net: ipBucket(ip), UA: uaHash(browserUA), Expires: clock.Add(time.Hour).Unix()})
	req := httptest.NewRequest("GET", "/_shield/check", nil)
	req.Header.Set(SiteHeader, "s1")
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("X-Forwarded-Uri", "/")
	req.Header.Set("User-Agent", browserUA)
	req.AddCookie(&http.Cookie{Name: passCookie, Value: pass})
	rec = httptest.NewRecorder()
	s.CheckHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("pass holder during an automatic attack: %d", rec.Code)
	}

	st, ok := s.Attack("s1")
	if !ok || st.Reason != "1,200 requests in the last minute" || len(s.Attacks()) != 1 {
		t.Fatalf("Attack = %v %+v", ok, st)
	}
	if c := <-calls; !c.active || c.st != st {
		t.Errorf("OnAttack start: %+v", c)
	}
	if ev := s.Events("s1", 10); len(ev) != 1 || ev[0].Verdict != "attack" || ev[0].Reason != st.Reason || ev[0].IP != "" {
		t.Errorf("events %+v", ev)
	}

	if !s.EndAttack("s1") {
		t.Fatal("EndAttack")
	}
	if c := <-calls; c.active || !c.st.Until.Equal(*clock) || c.st.Peak != AttackFloor {
		t.Errorf("OnAttack end: %+v", c)
	}
	if ev := s.Events("s1", 1); len(ev) != 1 || ev[0].Verdict != "attack_end" || ev[0].Reason != "ended by hand (peak 1,200 requests a minute)" {
		t.Errorf("events %+v", ev)
	}
	if rec := checkRequest(s, "198.51.100.10", "GET", "/", browserUA); rec.Code != http.StatusOK {
		t.Errorf("browser after EndAttack: %d", rec.Code)
	}
}

func TestCheckAutoAttackFollowsMode(t *testing.T) {
	s, clock, mode := floodShield(ModeAuto, nil)
	flood(s, clock)
	checkRequest(s, "198.51.100.10", "GET", "/", browserUA)
	if _, ok := s.Attack("s1"); !ok {
		t.Fatal("no attack")
	}
	// The owner picks Standard while the flood has stopped: Run's tick
	// ends the automatic attack.
	*mode = ModeStandard
	s.checkAttacks(clock.Add(time.Minute))
	if _, ok := s.Attack("s1"); ok {
		t.Fatal("still under attack after leaving Auto")
	}
	if ev := s.Events("s1", 1); len(ev) != 1 || ev[0].Verdict != "attack_end" {
		t.Errorf("events %+v", ev)
	}
}

func TestCheckStandardNeverEscalated(t *testing.T) {
	s, clock, _ := floodShield(ModeStandard, func(string, bool, AttackState) { t.Error("OnAttack called for a Standard site") })
	flood(s, clock)
	if rec := checkRequest(s, "198.51.100.10", "GET", "/", browserUA); rec.Code != http.StatusOK {
		t.Fatalf("browser on a flooded Standard site: %d", rec.Code)
	}
	s.checkAttacks(clock.Add(time.Minute))
	if len(s.Attacks()) != 0 || len(s.Events("", 10)) != 0 {
		t.Fatalf("attacks %+v events %+v", s.Attacks(), s.Events("", 10))
	}
}
