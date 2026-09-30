package shield

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// Automatic Under attack mode. A site owner shouldn't have to notice a
// flood and flip a switch: sites in ModeAuto are switched to Under attack
// by the shield while it sees one, and back when it's over.
//
// Each site's dynamic requests are counted in 10-second buckets forming a
// sliding one-minute window. When a bucket rolls over, the minute that just
// completed is judged against two triggers:
//
//   - volume: far more requests than the site normally gets (AttackFactor
//     times its baseline), and at least AttackFloor;
//   - spread: AttackClients different addresses stopped for abuse, a
//     botnet in which every client stays near its own limit so that no
//     single one looks like much.
//
// Detection happens on the request path, when the first request of a new
// bucket arrives, so it needs no ticker to react; Run only notices the end
// of an attack when traffic stops altogether, and forgets idle sites.
//
// Like bans, all of this lives in memory: a daemon restart starts every
// site over in Standard, which is the safe failure mode.
const (
	// AttackFloor is the fewest requests a minute that can count as a flood
	// (20 a second of dynamic, uncached requests). Below it a small server
	// copes on its own, however quiet the site normally is: a post shared
	// on social media must not put a small blog's visitors behind a
	// challenge.
	AttackFloor = 1200
	// AttackFactor: a minute with this many times the site's normal traffic
	// is a flood. Legitimate peaks (a newsletter, a sale) rarely reach 5x
	// within one minute; floods start at 10-100x.
	AttackFactor = 5
	// AttackClients different addresses stopped for abuse within a minute
	// is a distributed flood. Honest visitors almost never hit a limit, so
	// 30 at once is a botnet, not a crowd.
	AttackClients = 30
	// AttackHold is how long a site stays in Under attack after the last
	// minute that looked like a flood. Floods come in waves and often pause
	// to see whether the site gave in; switching back after every lull
	// would let each wave through.
	AttackHold = 15 * time.Minute
	// AttackWarmup is how many minutes of history a site needs before its
	// baseline counts. Until then only the floor applies.
	AttackWarmup = 10
)

const (
	attackBucket  = 10 * time.Second
	attackBuckets = 6 // buckets per minute: the window
	// attackMaxClients bounds the refused-client set of each site. Past it
	// the spread trigger has long fired, so counting further is pointless.
	attackMaxClients = 256
	// baselineMinutes is the baseline's memory: an exponentially weighted
	// average over roughly the last hour of normal traffic, slow enough
	// that a flood ramping up over a few minutes doesn't raise it much and
	// fast enough to follow a site's day.
	baselineMinutes = 60
	// attackIdle: sites without a request for this long are forgotten.
	attackIdle = time.Hour
)

// AttackState describes an automatic Under attack period of a site.
type AttackState struct {
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`  // when it ends if the flood doesn't return
	Reason string    `json:"reason"` // what started it, in words
	Peak   int       `json:"peak_per_minute"`
}

// attackChange is a site entering or leaving Under attack, reported to the
// Shield outside the site's lock.
type attackChange struct {
	site   string
	active bool
	state  AttackState
	at     time.Time
	why    string // why it ended
}

// attackDetector keeps the traffic of every site. Sites are looked up
// lock-free (sync.Map is built for keys written once and read constantly);
// each site then has its own small lock, so a flood on one site never slows
// down another.
type attackDetector struct {
	sites  sync.Map // site ID -> *siteTraffic
	notify func(attackChange)
}

func newAttackDetector(notify func(attackChange)) *attackDetector {
	return &attackDetector{notify: notify}
}

type siteTraffic struct {
	mu       sync.Mutex
	dead     bool  // swept: callers holding a pointer must look again
	slot     int64 // current bucket (time / attackBucket)
	born     int64 // minute the site was first seen; partial, not learned
	buckets  [attackBuckets]trafficBucket
	refused  map[string]int64 // client -> last bucket it was refused in
	lastSeen time.Time
	auto     bool // the site is in ModeAuto

	baseline float64 // normal requests per minute
	minutes  int     // minutes of history in the baseline

	lastMet     time.Time // last time a minute looked like a flood
	since       time.Time // start of the current attack; zero if none
	reason      string
	peak        int
	provisional bool // started on the floor alone, before a baseline existed
}

type trafficBucket struct {
	slot     int64
	requests int
}

func slotOf(t time.Time) int64      { return t.UnixNano() / int64(attackBucket) }
func slotTime(slot int64) time.Time { return time.Unix(0, slot*int64(attackBucket)) }
func (t *siteTraffic) active() bool { return !t.since.IsZero() }
func (t *siteTraffic) warm() bool   { return t.minutes >= AttackWarmup }
func (t *siteTraffic) at(slot int64) *trafficBucket {
	return &t.buckets[slot%attackBuckets]
}

// site returns a site's traffic, creating it on first sight.
func (d *attackDetector) site(id string, now time.Time) *siteTraffic {
	if v, ok := d.sites.Load(id); ok {
		return v.(*siteTraffic)
	}
	slot := slotOf(now)
	t := &siteTraffic{slot: slot, born: slot / attackBuckets, refused: map[string]int64{}, lastSeen: now}
	t.at(slot).slot = slot
	v, _ := d.sites.LoadOrStore(id, t)
	return v.(*siteTraffic)
}

// record counts one request for a site and reports whether the site is
// under an automatic attack, i.e. its visitors should be challenged. auto
// is whether the site is in ModeAuto; other sites are counted (so their
// baseline is ready when they switch) but never escalated.
func (d *attackDetector) record(id string, auto bool, now time.Time) (*siteTraffic, bool) {
	for {
		t := d.site(id, now)
		t.mu.Lock()
		if t.dead {
			t.mu.Unlock()
			continue
		}
		changes := t.setAuto(id, auto, now)
		changes = append(changes, t.advance(id, now)...)
		t.at(t.slot).requests++
		t.lastSeen = now
		active := t.active()
		t.mu.Unlock()
		d.report(changes)
		return t, active
	}
}

// refuse notes a client the shield refused for abuse, for the spread
// trigger. key is the client's ban key (an IPv6 /64 counts once).
func (t *siteTraffic) refuse(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.refused[key]; ok || len(t.refused) < attackMaxClients {
		t.refused[key] = t.slot
	}
}

// tick evaluates a site without a request, so an attack ends even when the
// traffic stops altogether, and forgets the site once it has been idle for
// attackIdle. auto is the site's current setting (false if it's gone).
func (d *attackDetector) tick(now time.Time, auto func(id string) bool) {
	d.sites.Range(func(k, v any) bool {
		id, t := k.(string), v.(*siteTraffic)
		a := auto(id)
		t.mu.Lock()
		changes := t.setAuto(id, a, now)
		changes = append(changes, t.advance(id, now)...)
		if !t.active() && now.Sub(t.lastSeen) >= attackIdle {
			t.dead = true
			d.sites.CompareAndDelete(id, t)
		}
		t.mu.Unlock()
		d.report(changes)
		return true
	})
}

func (d *attackDetector) report(changes []attackChange) {
	for _, c := range changes {
		d.notify(c)
	}
}

// setAuto follows the site's mode. Leaving ModeAuto ends an automatic
// attack: whatever the owner picked instead is now in charge.
func (t *siteTraffic) setAuto(id string, auto bool, now time.Time) []attackChange {
	t.auto = auto
	if auto || !t.active() {
		return nil
	}
	return []attackChange{t.end(id, now, "the site's protection mode was changed")}
}

// advance moves the window to now's bucket, judging the minutes completed
// on the way and learning the baseline from them.
func (t *siteTraffic) advance(id string, now time.Time) []attackChange {
	slot := slotOf(now)
	if slot <= t.slot {
		return nil
	}
	old := t.slot
	var changes []attackChange
	// The window ending with the last bucket that saw traffic holds more
	// than any later one, so it's judged first: a burst followed by silence
	// must not slip through between two ticks. The current window may then
	// end an attack.
	if c, ok := t.evaluate(id, old+1); ok {
		changes = append(changes, c)
	}
	if slot > old+1 {
		if c, ok := t.evaluate(id, slot); ok {
			changes = append(changes, c)
		}
	}
	t.learn(old, slot)
	t.slot = slot
	*t.at(slot) = trafficBucket{slot: slot}
	return changes
}

// window sums the minute before bucket end: requests, and different
// clients refused. Refusals older than the window are forgotten.
func (t *siteTraffic) window(end int64) (requests, clients int) {
	for _, b := range t.buckets {
		if b.slot >= end-attackBuckets && b.slot < end {
			requests += b.requests
		}
	}
	for k, s := range t.refused {
		switch {
		case s < end-attackBuckets:
			delete(t.refused, k)
		case s < end:
			clients++
		}
	}
	return requests, clients
}

// evaluate judges the minute before bucket end and starts or ends an
// attack.
func (t *siteTraffic) evaluate(id string, end int64) (attackChange, bool) {
	at := slotTime(end)
	requests, clients := t.window(end)
	threshold := AttackFloor
	if t.warm() {
		threshold = max(threshold, int(math.Ceil(AttackFactor*t.baseline)))
	}
	byVolume, bySpread := requests >= threshold, clients >= AttackClients
	met := byVolume || bySpread
	if met {
		t.lastMet = at
	}
	if !t.active() {
		if !met || !t.auto {
			return attackChange{}, false
		}
		t.since, t.peak = at, requests
		t.provisional = !t.warm() && !bySpread
		if byVolume {
			t.reason = commas(requests) + " requests in the last minute"
			if t.warm() && t.baseline >= 1 {
				t.reason += " (normally about " + commas(int(math.Round(t.baseline))) + ")"
			}
		} else {
			n := strconv.Itoa(clients)
			if clients >= attackMaxClients {
				n = "at least " + n
			}
			t.reason = n + " different addresses were throttled or blocked in the last minute"
		}
		return attackChange{site: id, active: true, state: t.state(), at: at}, true
	}
	t.peak = max(t.peak, requests)
	switch {
	case met && (t.warm() || bySpread):
		t.provisional = false // confirmed now that the site's normal is known
	case met:
	case t.provisional && t.warm() && requests >= AttackFloor:
		// The floor was all there was to go on; the baseline now says this
		// much traffic is normal for the site (a busy site after a restart).
		// A flood that merely paused below the floor waits out the hold.
		t.lastMet = time.Time{}
		return t.end(id, at, "the traffic turned out to be normal for this site"), true
	case !at.Before(t.lastMet.Add(AttackHold)):
		return t.end(id, at, fmt.Sprintf("no flood for %d minutes", int(AttackHold.Minutes()))), true
	}
	return attackChange{}, false
}

// learn folds the minutes completed between buckets old and now into the
// baseline. Minutes of a flood are left out, so an attack never becomes the
// new normal, except during warm-up: with no normal to protect yet, a busy
// site would otherwise look flooded forever.
func (t *siteTraffic) learn(old, now int64) {
	from, to := old/attackBuckets, now/attackBuckets
	if from == to {
		return
	}
	flooding := func(minute int64) bool {
		end := slotTime((minute + 1) * attackBuckets)
		return t.warm() && !t.lastMet.IsZero() && end.Before(t.lastMet.Add(AttackHold))
	}
	if from != t.born && !flooding(from) {
		n := 0
		for _, b := range t.buckets {
			if b.slot/attackBuckets == from {
				n += b.requests
			}
		}
		t.fold(float64(n))
	}
	// Minutes without a single request. Sites idle for attackIdle are
	// swept, so this stays short.
	for m := from + 1; m < to && m < from+24*60; m++ {
		if !flooding(m) {
			t.fold(0)
		}
	}
}

// fold adds a minute to the baseline: a plain average of the first
// baselineMinutes minutes, then an exponentially weighted one.
func (t *siteTraffic) fold(requests float64) {
	t.minutes++
	t.baseline += (requests - t.baseline) / float64(min(t.minutes, baselineMinutes))
}

func (t *siteTraffic) state() AttackState {
	return AttackState{Since: t.since, Until: t.lastMet.Add(AttackHold), Reason: t.reason, Peak: t.peak}
}

func (t *siteTraffic) end(id string, at time.Time, why string) attackChange {
	st := t.state()
	st.Until = at
	t.since, t.reason, t.peak, t.provisional = time.Time{}, "", 0, false
	return attackChange{site: id, active: false, state: st, at: at, why: why}
}

// endNow ends an attack by hand. The window starts over, so the flood has
// to show itself again rather than the minute that caused the attack
// restarting it seconds later.
func (d *attackDetector) endNow(id string, now time.Time) bool {
	v, ok := d.sites.Load(id)
	if !ok {
		return false
	}
	t := v.(*siteTraffic)
	t.mu.Lock()
	if !t.active() {
		t.mu.Unlock()
		return false
	}
	c := t.end(id, now, "ended by hand")
	for i := range t.buckets {
		t.buckets[i].requests = 0
	}
	clear(t.refused)
	t.mu.Unlock()
	d.report([]attackChange{c})
	return true
}

func (d *attackDetector) attack(id string) (AttackState, bool) {
	v, ok := d.sites.Load(id)
	if !ok {
		return AttackState{}, false
	}
	t := v.(*siteTraffic)
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active() {
		return AttackState{}, false
	}
	return t.state(), true
}

func (d *attackDetector) attacks() map[string]AttackState {
	out := map[string]AttackState{}
	d.sites.Range(func(k, _ any) bool {
		if st, ok := d.attack(k.(string)); ok {
			out[k.(string)] = st
		}
		return true
	})
	return out
}

// floodRefusal reports whether a refusal counts towards the spread trigger:
// the client was over its budget, is banned, or showed attack evidence.
// Like strikes, where a client is (deny lists, country rules, blocklists)
// and site policy (AI crawlers, the admin allowlist) don't count: a site
// blocking a country or GPTBot refuses dozens of addresses a minute without
// being attacked.
func floodRefusal(s Signals, v Verdict) bool {
	return v != Allow && (s.RateExceeded || s.Banned || strikes(s, v) > 0)
}

// commas formats n with thousands separators, for people.
func commas(n int) string {
	if n < 0 {
		return "-" + commas(-n)
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Attack returns a site's automatic Under attack period, if it's in one.
func (s *Shield) Attack(siteID string) (AttackState, bool) { return s.attacks.attack(siteID) }

// Attacks lists the sites in an automatic Under attack period.
func (s *Shield) Attacks() map[string]AttackState { return s.attacks.attacks() }

// EndAttack ends a site's automatic Under attack period now (the panel's
// "It's over" button); the detector may start it again if the flood
// continues.
func (s *Shield) EndAttack(siteID string) bool { return s.attacks.endNow(siteID, s.now()) }

// checkAttacks is Run's share of the detector: ending attacks on sites that
// went silent, following mode changes and forgetting idle sites.
func (s *Shield) checkAttacks(now time.Time) {
	s.attacks.tick(now, func(id string) bool {
		if s.o.Sites == nil {
			return false
		}
		site, ok := s.o.Sites(id)
		return ok && site.Mode == ModeAuto
	})
}

// attackChanged logs a transition and tells Options.OnAttack. Callbacks
// run on their own goroutine, never the request's, but in order: a start
// and an end delivered the wrong way round would leave the panel showing a
// site under attack forever.
func (s *Shield) attackChanged(c attackChange) {
	if c.active {
		s.o.Logger.Warn("shield: flood detected, challenging all visitors", "site", c.site, "reason", c.state.Reason)
		s.events.add(Event{Time: c.at, Site: c.site, Verdict: "attack", Reason: c.state.Reason})
	} else {
		reason := c.why + " (peak " + commas(c.state.Peak) + " requests a minute)"
		s.o.Logger.Info("shield: attack over", "site", c.site, "reason", reason)
		s.events.add(Event{Time: c.at, Site: c.site, Verdict: "attack_end", Reason: reason})
	}
	if s.o.OnAttack == nil {
		return
	}
	s.cbMu.Lock()
	s.cbQueue = append(s.cbQueue, c)
	if s.cbRunning {
		s.cbMu.Unlock()
		return
	}
	s.cbRunning = true
	s.cbMu.Unlock()
	go func() {
		for {
			s.cbMu.Lock()
			if len(s.cbQueue) == 0 {
				s.cbRunning = false
				s.cbMu.Unlock()
				return
			}
			c := s.cbQueue[0]
			s.cbQueue = s.cbQueue[1:]
			s.cbMu.Unlock()
			s.o.OnAttack(c.site, c.active, c.state)
		}
	}()
}
