// Package cdn integrates sites with a CDN in front of Caddy. Cloudflare's
// free plan is supported: real visitor IPs behind its proxy, automatic cache
// purges and configuration checks.
package cdn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"
)

// cloudflareRanges is Cloudflare's published edge list
// (https://api.cloudflare.com/client/v4/ips), used until a fresher copy is
// fetched. It changes rarely: the last change was years ago.
var cloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
	"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17",
	"162.158.0.0/15", "104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32",
	"2a06:98c0::/29", "2c0f:f248::/32",
}

// Ranges is the set of Cloudflare edge networks. Caddy believes the
// CF-Connecting-IP header only on connections from these networks, so
// nobody else can pick the address the shield bans or rate-limits.
type Ranges struct {
	p atomic.Pointer[[]netip.Prefix]
}

func NewRanges() *Ranges {
	r := &Ranges{}
	p, err := ParseRanges(cloudflareRanges)
	if err != nil {
		panic(err) // the built-in list is covered by tests
	}
	r.p.Store(&p)
	return r
}

// Get returns the current ranges (none for a zero Ranges).
func (r *Ranges) Get() []netip.Prefix {
	if p := r.p.Load(); p != nil {
		return *p
	}
	return nil
}

// Set replaces the ranges; it reports whether they changed.
func (r *Ranges) Set(p []netip.Prefix) bool {
	if slices.Equal(p, r.Get()) {
		return false
	}
	r.p.Store(&p)
	return true
}

// Contains reports whether addr is a Cloudflare edge address.
func (r *Ranges) Contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range r.Get() {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ParseRanges validates a range list before anything trusts it. A wrong
// entry here lets whoever holds those addresses claim to be any visitor, so
// the list must look like an edge network: public unicast prefixes, none
// wider than /8 (IPv4) or /24 (IPv6), and a plausible count.
func ParseRanges(list []string) ([]netip.Prefix, error) {
	if len(list) < 4 || len(list) > 200 {
		return nil, fmt.Errorf("implausible number of ranges: %d", len(list))
	}
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, err
		}
		p = p.Masked()
		a := p.Addr()
		if (a.Is4() && p.Bits() < 8) || (a.Is6() && p.Bits() < 24) {
			return nil, fmt.Errorf("range %s is too wide", s)
		}
		if !a.IsGlobalUnicast() || a.IsPrivate() {
			return nil, fmt.Errorf("range %s is not public", s)
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return slices.Compact(out), nil
}

// Strings is the inverse of ParseRanges, for persisting and rendering.
func Strings(p []netip.Prefix) []string {
	out := make([]string, len(p))
	for i, x := range p {
		out[i] = x.String()
	}
	return out
}

// RangesURL is Cloudflare's machine-readable list of edge networks.
const RangesURL = "https://api.cloudflare.com/client/v4/ips"

// FetchRanges downloads and validates the current edge list.
func FetchRanges(ctx context.Context, client *http.Client, url string) ([]netip.Prefix, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare ranges: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Success bool `json:"success"`
		Result  struct {
			IPv4 []string `json:"ipv4_cidrs"`
			IPv6 []string `json:"ipv6_cidrs"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if !body.Success || len(body.Result.IPv4) == 0 || len(body.Result.IPv6) == 0 {
		return nil, errors.New("cloudflare ranges: unexpected response")
	}
	return ParseRanges(append(body.Result.IPv4, body.Result.IPv6...))
}
