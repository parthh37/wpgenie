package shield

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

type Resolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// crawlerVerifier performs forward-confirmed reverse DNS (FCrDNS): the IP's
// PTR record must end in an operator-owned suffix AND that hostname must
// resolve back to the same IP. Anyone can set a PTR record; only the operator
// controls the forward zone. Results are cached because DNS is slow and bots
// come back often.
type crawlerVerifier struct {
	resolver Resolver
	ttl      time.Duration
	mu       sync.Mutex
	cache    map[string]verifyResult
}

type verifyResult struct {
	ok      bool
	expires time.Time
}

func newCrawlerVerifier(r Resolver) *crawlerVerifier {
	if r == nil {
		r = net.DefaultResolver
	}
	return &crawlerVerifier{resolver: r, ttl: 6 * time.Hour, cache: map[string]verifyResult{}}
}

func (v *crawlerVerifier) verify(ctx context.Context, ip string, suffixes []string) bool {
	key := ip + "|" + strings.Join(suffixes, ",")
	now := time.Now()
	v.mu.Lock()
	if r, ok := v.cache[key]; ok && now.Before(r.expires) {
		v.mu.Unlock()
		return r.ok
	}
	v.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ok := v.lookup(ctx, ip, suffixes)

	v.mu.Lock()
	if len(v.cache) > 100_000 { // crude bound; verified crawlers are a small set of IPs
		v.cache = map[string]verifyResult{}
	}
	v.cache[key] = verifyResult{ok: ok, expires: now.Add(v.ttl)}
	v.mu.Unlock()
	return ok
}

func (v *crawlerVerifier) lookup(ctx context.Context, ip string, suffixes []string) bool {
	names, err := v.resolver.LookupAddr(ctx, ip)
	if err != nil {
		return false
	}
	for _, name := range names {
		name = strings.TrimSuffix(strings.ToLower(name), ".")
		if !hasAnySuffix(name, suffixes) {
			continue
		}
		addrs, err := v.resolver.LookupHost(ctx, name)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if net.ParseIP(a).Equal(net.ParseIP(ip)) {
				return true
			}
		}
	}
	return false
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
