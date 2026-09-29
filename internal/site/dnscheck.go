package site

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/store"
)

// DNS checks: before a domain is attached (a new site, an alias), the panel
// looks up where it points. A domain with no records, or records pointing
// at another server, gets no certificate from Let's Encrypt, and a site
// redirecting to it sends every visitor to a TLS error.

// DNS check results.
const (
	DNSOK        = "ok"        // every address is this server (or the chosen one)
	DNSMissing   = "missing"   // no A/AAAA records
	DNSElsewhere = "elsewhere" // no address is ours
	DNSPartly    = "partly"    // some addresses are ours, some aren't
	DNSProxied   = "proxied"   // behind Cloudflare: the origin can't be seen
	DNSUnknown   = "unknown"   // the lookup failed, or our addresses aren't known
)

// DNSCheck is where a domain points, compared with where it should.
type DNSCheck struct {
	Domain string `json:"domain"`
	Status string `json:"status"`
	// Addresses are what the domain resolves to; Expected, the addresses
	// of the server(s) that may serve it.
	Addresses []string `json:"addresses"`
	Expected  []string `json:"expected"`
	// Server is the server the domain points to, when that's one of the
	// cluster's and none was asked for (automatic placement).
	Server  string `json:"server,omitempty"`
	Message string `json:"message"`
}

// dnsTimeout bounds one check: the wizard re-checks while DNS propagates.
const dnsTimeout = 5 * time.Second

// CheckDNS looks up a domain and compares it with the addresses of node
// (cluster.LocalNode: this server), or of any server of the cluster when
// node is "" (the site isn't placed yet).
func (s *Service) CheckDNS(ctx context.Context, name, node string) (*DNSCheck, error) {
	domain, err := NormalizeDomain(name)
	if err != nil {
		return nil, err
	}
	targets, err := s.dnsTargets(ctx, node)
	if err != nil {
		return nil, err
	}
	res := &DNSCheck{Domain: domain, Addresses: []string{}, Expected: []string{}}
	for a := range targets {
		res.Expected = append(res.Expected, a.String())
	}
	slices.Sort(res.Expected)

	var r Resolver = net.DefaultResolver
	if s.DNS != nil {
		r = s.DNS
	}
	lctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	addrs, err := r.LookupNetIP(lctx, "ip", domain)
	var dnsErr *net.DNSError
	switch {
	case err != nil && errors.As(err, &dnsErr) && dnsErr.IsNotFound, err == nil && len(addrs) == 0:
		res.Status = DNSMissing
		res.Message = fmt.Sprintf("%s has no A or AAAA record yet.", domain)
		return res, nil
	case err != nil:
		res.Status = DNSUnknown
		res.Message = fmt.Sprintf("Couldn't look up %s: %v", domain, err)
		return res, nil
	}

	var ours, cf, other []netip.Addr
	servers := map[string]bool{}
	for _, a := range addrs {
		a = a.Unmap()
		res.Addresses = append(res.Addresses, a.String())
		if srv, ok := targets[a]; ok {
			ours = append(ours, a)
			servers[srv] = true
		} else if s.CDNRanges != nil && s.CDNRanges.Contains(a) {
			cf = append(cf, a)
		} else {
			other = append(other, a)
		}
	}
	slices.Sort(res.Addresses)
	res.Addresses = slices.Compact(res.Addresses)
	if node == "" && len(servers) == 1 {
		for srv := range servers {
			res.Server = srv
		}
	}
	res.Status, res.Message = classifyDNS(domain, ours, cf, other, len(targets) > 0)
	return res, nil
}

// classifyDNS turns a domain's addresses, sorted into this server's
// (ours), Cloudflare's edge (cf) and anything else (other), into a status
// and what to tell the user. known: this server's addresses are known.
func classifyDNS(domain string, ours, cf, other []netip.Addr, known bool) (string, string) {
	switch {
	case len(cf) > 0 && len(ours) == 0 && len(other) == 0:
		return DNSProxied, fmt.Sprintf("%s is behind Cloudflare (proxied), so where it really points can't be checked. "+
			"Make sure its origin record is this server and SSL/TLS is set to Full (strict).", domain)
	case !known:
		return DNSUnknown, fmt.Sprintf("%s resolves to %s, but this server's public address isn't known: "+
			"check it's the right one.", domain, joinAddrs(slices.Concat(ours, cf, other)))
	case len(ours) > 0 && len(other) == 0 && len(cf) == 0:
		return DNSOK, fmt.Sprintf("%s points to this server.", domain)
	case len(ours) > 0:
		// Let's Encrypt may validate over any of them (IPv6 first): one
		// stale record is enough to fail the certificate.
		return DNSPartly, fmt.Sprintf("%s also points to %s, which isn't this server: remove those records, "+
			"or some visitors (and Let's Encrypt) end up elsewhere.", domain, joinAddrs(slices.Concat(cf, other)))
	}
	return DNSElsewhere, fmt.Sprintf("%s points to %s, not to this server.", domain, joinAddrs(slices.Concat(cf, other)))
}

func joinAddrs(as []netip.Addr) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// dnsTargets maps the public addresses a domain may point to onto the
// server each belongs to: node's, or every server's when node is "".
func (s *Service) dnsTargets(ctx context.Context, node string) (map[netip.Addr]string, error) {
	out := map[netip.Addr]string{}
	if node == "" || node == cluster.LocalNode {
		for _, a := range s.localPublicAddrs(ctx) {
			out[a] = cluster.LocalNode
		}
	}
	if node == cluster.LocalNode {
		return out, nil
	}
	var nodes []*store.Node
	if node == "" {
		all, err := s.Store.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		nodes = all
	} else {
		n, err := s.Store.GetNode(ctx, node)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: no server %q", ErrInvalidInput, node)
		} else if err != nil {
			return nil, err
		}
		nodes = []*store.Node{n}
	}
	for _, n := range nodes {
		if a, err := netip.ParseAddr(n.PublicIP); err == nil {
			out[a.Unmap()] = n.ID
		}
	}
	return out, nil
}

// localPublicAddrs are this server's public addresses: those on its
// network interfaces, plus what the panel's own domain resolves to (it
// points here by definition, and covers servers behind NAT whose public
// address isn't on any interface).
func (s *Service) localPublicAddrs(ctx context.Context) []netip.Addr {
	var out []netip.Addr
	list := net.InterfaceAddrs
	if s.interfaceAddrs != nil {
		list = s.interfaceAddrs
	}
	if ifAddrs, err := list(); err == nil {
		for _, ia := range ifAddrs {
			if pfx, err := netip.ParsePrefix(ia.String()); err == nil && publicAddr(pfx.Addr()) {
				out = append(out, pfx.Addr().Unmap())
			}
		}
	}
	if s.Cfg != nil && s.Cfg.PanelDomain != "" {
		var r Resolver = net.DefaultResolver
		if s.DNS != nil {
			r = s.DNS
		}
		lctx, cancel := context.WithTimeout(ctx, dnsTimeout)
		defer cancel()
		if addrs, err := r.LookupNetIP(lctx, "ip", s.Cfg.PanelDomain); err == nil {
			for _, a := range addrs {
				// A proxied panel domain resolves to Cloudflare, not here.
				if a = a.Unmap(); publicAddr(a) && (s.CDNRanges == nil || !s.CDNRanges.Contains(a)) {
					out = append(out, a)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}

func publicAddr(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast()
}
