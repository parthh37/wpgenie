package shield

import (
	"context"
	"errors"
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
	inflight map[string]chan struct{} // one lookup per key at a time
}

// verdict is the outcome of FCrDNS. unknown (a timeout, SERVFAIL) must never
// count as verified: an attacker controls their own reverse zone and could
// make lookups fail on purpose. Nor as spoofed: a DNS hiccup must not get
// the real Googlebot blocked.
type verdict int

const (
	unknown verdict = iota
	verified
	spoofed
)

type verifyResult struct {
	v       verdict
	expires time.Time
}

const (
	unknownTTL    = time.Minute
	lookupTimeout = 2 * time.Second
)

func newCrawlerVerifier(r Resolver) *crawlerVerifier {
	if r == nil {
		r = net.DefaultResolver
	}
	return &crawlerVerifier{resolver: r, ttl: 6 * time.Hour, cache: map[string]verifyResult{}, inflight: map[string]chan struct{}{}}
}

func (v *crawlerVerifier) verify(ip string, suffixes []string) verdict {
	key := ip + "|" + strings.Join(suffixes, ",")
	for {
		now := time.Now()
		v.mu.Lock()
		if r, ok := v.cache[key]; ok && now.Before(r.expires) {
			v.mu.Unlock()
			return r.v
		}
		if wait, busy := v.inflight[key]; busy {
			v.mu.Unlock()
			<-wait // someone is already asking; use their answer
			continue
		}
		done := make(chan struct{})
		v.inflight[key] = done
		v.mu.Unlock()

		// Detached from the request: a client hanging up mid-lookup must not
		// be cached as a failed verification.
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		res := v.lookup(ctx, ip, suffixes)
		cancel()

		ttl := v.ttl
		if res == unknown {
			ttl = unknownTTL
		}
		v.mu.Lock()
		if len(v.cache) > 100_000 { // crude bound; verified crawlers are a small set of IPs
			for k, r := range v.cache {
				if now.After(r.expires) || r.v != verified {
					delete(v.cache, k) // keep what real crawlers depend on
				}
			}
		}
		v.cache[key] = verifyResult{v: res, expires: now.Add(ttl)}
		delete(v.inflight, key)
		v.mu.Unlock()
		close(done)
		return res
	}
}

func (v *crawlerVerifier) lookup(ctx context.Context, ip string, suffixes []string) verdict {
	names, err := v.resolver.LookupAddr(ctx, ip)
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return spoofed // no PTR at all: definitely not the operator's
		}
		return unknown
	}
	indeterminate := false
	for _, name := range names {
		name = strings.TrimSuffix(strings.ToLower(name), ".")
		if !hasAnySuffix(name, suffixes) {
			continue
		}
		addrs, err := v.resolver.LookupHost(ctx, name)
		if err != nil {
			var de *net.DNSError
			if !errors.As(err, &de) || !de.IsNotFound {
				indeterminate = true
			}
			continue
		}
		for _, a := range addrs {
			if net.ParseIP(a).Equal(net.ParseIP(ip)) {
				return verified
			}
		}
	}
	if indeterminate {
		return unknown
	}
	return spoofed
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
