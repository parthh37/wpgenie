package shield

import (
	"hash/maphash"
	"sync"
	"time"
)

// rateLimiter is a sharded token-bucket limiter. Sharding keeps lock
// contention low when thousands of IPs hit the shield concurrently.
type rateLimiter struct {
	rate   float64 // tokens per second
	burst  float64
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
}

func newRateLimiter(perSecond, burst float64) *rateLimiter {
	rl := &rateLimiter{rate: perSecond, burst: burst, seed: maphash.MakeSeed()}
	for i := range rl.shards {
		rl.shards[i].buckets = map[string]*bucket{}
	}
	return rl
}

// allow consumes one token for key and reports whether it was available.
func (rl *rateLimiter) allow(key string, now time.Time) bool {
	sh := &rl.shards[maphash.String(rl.seed, key)%uint64(len(rl.shards))]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b, ok := sh.buckets[key]
	if !ok {
		sh.buckets[key] = &bucket{tokens: rl.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have been idle long enough to be full again, so
// memory tracks active clients rather than every IP ever seen.
func (rl *rateLimiter) sweep(now time.Time) {
	idle := time.Duration(rl.burst/rl.rate*float64(time.Second)) + time.Minute
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.Lock()
		for k, b := range sh.buckets {
			if now.Sub(b.last) > idle {
				delete(sh.buckets, k)
			}
		}
		sh.mu.Unlock()
	}
}
