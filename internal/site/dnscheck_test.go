package site

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/store"
)

// dnsZone answers like a real resolver: NXDOMAIN for names it doesn't have.
type dnsZone map[string][]string

func (z dnsZone) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	as, ok := z[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []netip.Addr
	for _, a := range as {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

func ifAddrs(cidrs ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, err
			}
			n.IP = ip
			out = append(out, n)
		}
		return out, nil
	}
}

func TestCheckDNS(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ranges := cdn.NewRanges()
	ranges.Set([]netip.Prefix{netip.MustParsePrefix("104.16.0.0/13")})
	h.svc.CDNRanges = ranges
	// This server: a public IPv4 and IPv6 on its interfaces (the private
	// and loopback ones never count).
	h.svc.interfaceAddrs = ifAddrs("127.0.0.1/8", "10.0.0.5/24", "198.51.100.7/24", "2001:db8:1::7/64", "fe80::1/64")
	h.svc.DNS = dnsZone{
		"ok.test":     {"198.51.100.7", "2001:db8:1::7"},
		"v4.test":     {"198.51.100.7"},
		"stale6.test": {"198.51.100.7", "2001:db8:9::1"},
		"other.test":  {"203.0.113.9"},
		"cf.test":     {"104.16.1.1"},
		"node.test":   {"192.0.2.44"},
		"www.cf.test": {"104.16.1.1", "203.0.113.9"},
		"empty.test":  {},
	}
	if err := h.svc.Store.CreateNode(ctx, &store.Node{ID: "web-2", Num: 2, Name: "web-2", Address: "10.0.0.6:7443",
		PublicIP: "192.0.2.44"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		domain, node, status, server string
	}{
		{"ok.test", cluster.LocalNode, DNSOK, ""},
		{"OK.test.", cluster.LocalNode, DNSOK, ""}, // normalised
		{"v4.test", cluster.LocalNode, DNSOK, ""},
		{"stale6.test", cluster.LocalNode, DNSPartly, ""},
		{"other.test", cluster.LocalNode, DNSElsewhere, ""},
		{"cf.test", cluster.LocalNode, DNSProxied, ""},
		{"www.cf.test", cluster.LocalNode, DNSElsewhere, ""},
		{"missing.test", cluster.LocalNode, DNSMissing, ""},
		{"empty.test", cluster.LocalNode, DNSMissing, ""},
		// A site on web-2: only its address counts.
		{"node.test", "web-2", DNSOK, ""},
		{"ok.test", "web-2", DNSElsewhere, ""},
		// Not placed yet: any server, and which one it is.
		{"node.test", "", DNSOK, "web-2"},
		{"ok.test", "", DNSOK, cluster.LocalNode},
	} {
		res, err := h.svc.CheckDNS(ctx, c.domain, c.node)
		if err != nil {
			t.Fatalf("%s on %q: %v", c.domain, c.node, err)
		}
		if res.Status != c.status || res.Server != c.server {
			t.Errorf("%s on %q: %s (server %q), want %s (server %q): %s", c.domain, c.node, res.Status, res.Server,
				c.status, c.server, res.Message)
		}
		if res.Message == "" {
			t.Errorf("%s: no message", c.domain)
		}
	}

	res, _ := h.svc.CheckDNS(ctx, "ok.test", cluster.LocalNode)
	if len(res.Expected) != 2 || res.Expected[0] != "198.51.100.7" || res.Expected[1] != "2001:db8:1::7" {
		t.Errorf("expected addresses %v", res.Expected)
	}
	if _, err := h.svc.CheckDNS(ctx, "ok.test", "nope"); err == nil {
		t.Error("an unknown server was accepted")
	}
	if _, err := h.svc.CheckDNS(ctx, "not a domain", cluster.LocalNode); err == nil {
		t.Error("an invalid domain was accepted")
	}

	// Behind NAT: no public address on any interface, but the panel's
	// domain points here.
	h.svc.interfaceAddrs = ifAddrs("10.0.0.5/24")
	if res, _ := h.svc.CheckDNS(ctx, "v4.test", cluster.LocalNode); res.Status != DNSUnknown {
		t.Errorf("unknown own address: %s", res.Status)
	}
	h.svc.Cfg.PanelDomain = "v4.test"
	if res, _ := h.svc.CheckDNS(ctx, "v4.test", cluster.LocalNode); res.Status != DNSOK {
		t.Errorf("via the panel domain: %s: %s", res.Status, res.Message)
	}
}
