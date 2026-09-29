package shield

import (
	"hash/maphash"
	"sync"
	"time"
)

// rateLimiter is a sharded token-bucket limiter. Sharding keeps lock
// contention low when thousands of IPs hit the shield concurrently. The
// rate is passed on every call, so each site can have its own limits and a
// changed limit applies at once.
type rateLimiter struct {
	seed   maphash.Seed
	shards [64]limiterShard
}

type limiterShard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	idle   time.Duration // time after which the bucket is full again
}

func newRateLimiter() *rateLimiter {
	rl := &rateLimiter{seed: maphash.MakeSeed()}
	for i := range rl.shards {
		rl.shards[i].buckets = map[string]*bucket{}
	}
	return rl
}

// allow consumes one token for key from a bucket refilling at perSecond up
// to burst, and reports whether it was available.
func (rl *rateLimiter) allow(key string, now time.Time, perSecond, burst float64) bool {
	sh := &rl.shards[maphash.String(rl.seed, key)%uint64(len(rl.shards))]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	idle := time.Duration(burst/perSecond*float64(time.Second)) + time.Minute
	b, ok := sh.buckets[key]
	if !ok {
		sh.buckets[key] = &bucket{tokens: burst - 1, last: now, idle: idle}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * perSecond
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last, b.idle = now, idle
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have been idle long enough to be full again, so
// memory tracks active clients rather than every IP ever seen.
func (rl *rateLimiter) sweep(now time.Time) {
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.Lock()
		for k, b := range sh.buckets {
			if now.Sub(b.last) > b.idle {
				delete(sh.buckets, k)
			}
		}
		sh.mu.Unlock()
	}
}
