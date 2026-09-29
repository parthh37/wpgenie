package analytics

import (
	"math"
	"slices"
	"sync"
	"time"
)

// Recent keeps the last few minutes of each site's PHP response times, for
// the autoscaler: the hourly rollups are far too coarse to act on.
type Recent struct {
	mu    sync.Mutex
	sites map[string][]timed
}

type timed struct {
	at time.Time
	ms float64
}

const (
	recentKeep = 3 * time.Minute
	// recentMax bounds memory per site; the newest are kept.
	recentMax = 5000
)

// Observe records a response time. Samples older than a few minutes (a
// backlog read after downtime) are dropped.
func (r *Recent) Observe(site string, at time.Time, ms float64) {
	if time.Since(at) > recentKeep {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sites == nil {
		r.sites = map[string][]timed{}
	}
	s := r.sites[site]
	cut := 0
	for cut < len(s) && time.Since(s[cut].at) > recentKeep {
		cut++
	}
	if len(s)-cut >= recentMax {
		cut = len(s) - recentMax + 1
	}
	r.sites[site] = append(s[cut:], timed{at, ms})
}

// Percentile returns the p-th percentile (0 < p <= 1) of a site's response
// times since the given time, and how many there were.
func (r *Recent) Percentile(site string, since time.Time, p float64) (float64, int) {
	r.mu.Lock()
	var v []float64
	for _, s := range r.sites[site] {
		if !s.at.Before(since) {
			v = append(v, s.ms)
		}
	}
	r.mu.Unlock()
	if len(v) == 0 {
		return 0, 0
	}
	slices.Sort(v)
	i := int(math.Ceil(p*float64(len(v)))) - 1 // nearest rank
	return v[min(max(i, 0), len(v)-1)], len(v)
}
