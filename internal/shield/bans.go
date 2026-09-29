package shield

import (
	"cmp"
	"hash/maphash"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Automatic bans: fail2ban for the whole server, in the shield's hot path.
// A client that shows unambiguous attack evidence (see strikes in
// policy.go) BanThreshold times within BanWindow is blocked on every site,
// first for BanBase and then twice as long for each repeat ban within a day,
// up to BanMax. Scanners typically probe many sites on one server, so a ban
// earned on one site protects all of them.
//
// Bans live in memory: a daemon restart forgives everyone, which is the
// safe failure mode for an automatic system.
const (
	BanThreshold = 5
	BanWindow    = 10 * time.Minute
	BanBase      = time.Hour
	BanMax       = 24 * time.Hour
	// MaxManualBan bounds bans set by hand from the panel.
	MaxManualBan = 30 * 24 * time.Hour
)

// Ban is one banned address (an IPv4 address or an IPv6 /64).
type Ban struct {
	Addr   string    `json:"addr"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
	Manual bool      `json:"manual"`
}

type offender struct {
	strikes     int
	windowStart time.Time
	until       time.Time // banned while now < until
	bans        int       // automatic bans so far, for escalation
	reason      string
	manual      bool
}

type banList struct {
	seed   maphash.Seed
	shards [32]banShard
}

type banShard struct {
	mu sync.Mutex
	m  map[string]*offender
}

func newBanList() *banList {
	b := &banList{seed: maphash.MakeSeed()}
	for i := range b.shards {
		b.shards[i].m = map[string]*offender{}
	}
	return b
}

// BanKey is the unit a ban applies to. IPv6 clients get a whole /64 (one
// subscriber's allocation, which an attacker can rotate through freely);
// IPv4 bans stay exact so one abuser doesn't lock out a NAT'd office.
func BanKey(ip string) (string, bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", false
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String(), true
	}
	p, _ := a.Prefix(64)
	return p.String(), true
}

func (b *banList) shard(key string) *banShard {
	return &b.shards[maphash.String(b.seed, key)%uint64(len(b.shards))]
}

func (b *banList) banned(key string, now time.Time) bool {
	sh := b.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	o, ok := sh.m[key]
	return ok && now.Before(o.until)
}

// strike records n strikes and reports whether this pushed the client into
// a ban.
func (b *banList) strike(key string, n int, reason string, now time.Time) (bool, time.Time) {
	sh := b.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	o, ok := sh.m[key]
	if !ok {
		o = &offender{}
		sh.m[key] = o
	}
	if now.Before(o.until) {
		return false, o.until // already banned
	}
	if now.Sub(o.windowStart) > BanWindow {
		o.strikes, o.windowStart = 0, now
	}
	if !o.until.IsZero() && now.Sub(o.until) > BanMax {
		o.bans = 0 // a day of good behaviour resets the escalation
	}
	o.strikes += n
	if o.strikes < BanThreshold {
		return false, time.Time{}
	}
	d := min(BanBase<<min(o.bans, 16), BanMax)
	o.bans++
	o.strikes, o.until, o.reason, o.manual = 0, now.Add(d), reason, false
	return true, o.until
}

func (b *banList) ban(key string, d time.Duration, reason string, now time.Time) {
	sh := b.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.m[key] = &offender{until: now.Add(d), reason: reason, manual: true}
}

func (b *banList) unban(key string) bool {
	sh := b.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	o, ok := sh.m[key]
	if !ok || o.until.IsZero() {
		return false
	}
	delete(sh.m, key) // forgiveness is complete: strikes and escalation too
	return true
}

func (b *banList) list(now time.Time) []Ban {
	var out []Ban
	for i := range b.shards {
		sh := &b.shards[i]
		sh.mu.Lock()
		for k, o := range sh.m {
			if now.Before(o.until) {
				out = append(out, Ban{Addr: k, Until: o.until, Reason: o.reason, Manual: o.manual})
			}
		}
		sh.mu.Unlock()
	}
	slices.SortFunc(out, func(a, b Ban) int { return cmp.Compare(b.Until.Unix(), a.Until.Unix()) })
	return out
}

// sweep forgets offenders with nothing left to remember: no active ban, no
// strikes in the current window and no escalation still pending.
func (b *banList) sweep(now time.Time) {
	for i := range b.shards {
		sh := &b.shards[i]
		sh.mu.Lock()
		for k, o := range sh.m {
			if now.Sub(o.windowStart) > BanWindow && now.Sub(o.until) > BanMax {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
}
