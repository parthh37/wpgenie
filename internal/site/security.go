package site

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/store"
)

// ShieldInput changes a site's shield settings. Nil fields keep their
// current value, so older clients that only send mode and block_ai_bots
// don't switch the WAF off by omission.
type ShieldInput struct {
	Mode        shield.Mode `json:"mode"`
	BlockAIBots bool        `json:"block_ai_bots"`
	WAF         *bool       `json:"waf,omitempty"`
	AdminAllow  *[]string   `json:"admin_allow,omitempty"`
	TrustedIPs  *[]string   `json:"trusted_ips,omitempty"`
}

// maxNets bounds each IP list; the shield checks them on every request.
const maxNets = 64

func (s *Service) SetShield(ctx context.Context, id string, in ShieldInput) (*store.Site, error) {
	if !in.Mode.Valid() {
		return nil, fmt.Errorf("%w: shield mode", ErrInvalidInput)
	}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	c := store.ShieldSettings{Mode: string(in.Mode), BlockAIBots: in.BlockAIBots,
		WAF: st.WAF, AdminAllow: st.AdminAllow, TrustedIPs: st.TrustedIPs}
	if in.WAF != nil {
		c.WAF = *in.WAF
	}
	if in.AdminAllow != nil {
		if c.AdminAllow, err = normalizeNets("admin_allow", *in.AdminAllow); err != nil {
			return nil, err
		}
	}
	if in.TrustedIPs != nil {
		if c.TrustedIPs, err = normalizeNets("trusted_ips", *in.TrustedIPs); err != nil {
			return nil, err
		}
	}
	if err := s.Store.SetShield(ctx, id, c); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	return s.Store.GetSite(ctx, id)
}

// normalizeNets accepts IPs and CIDR prefixes and returns them in canonical
// prefix form ("203.0.113.7/32", "2001:db8::/48"), without duplicates.
func normalizeNets(field string, in []string) ([]string, error) {
	if len(in) > maxNets {
		return nil, fmt.Errorf("%w: %s: at most %d entries", ErrInvalidInput, field, maxNets)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		p, err := parseNet(v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %q is not an IP address or CIDR range", ErrInvalidInput, field, v)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("%w: %s: %q matches every address", ErrInvalidInput, field, v)
		}
		if k := p.String(); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

func parseNet(v string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(v); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(v)
	if err != nil {
		return netip.Prefix{}, err
	}
	// Clients are compared unmapped, so ::ffff:203.0.113.0/120 must become
	// 203.0.113.0/24 or it would never match.
	if a := p.Addr(); a.Is4In6() {
		if p.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("mapped prefix shorter than /96")
		}
		p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

// mustPrefixes parses stored (already normalised) prefixes; anything
// unparseable was never written by normalizeNets and is skipped.
func mustPrefixes(vs []string) []netip.Prefix {
	var out []netip.Prefix
	for _, v := range vs {
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p)
		}
	}
	return out
}
