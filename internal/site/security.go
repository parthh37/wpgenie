package site

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/store"
)

// ShieldInput changes a site's shield settings. Nil fields keep their
// current value, so older clients that only send mode and block_ai_bots
// don't switch the WAF off by omission.
type ShieldInput struct {
	Mode shield.Mode `json:"mode"`
	// Level applies a protection level (basic, recommended, strict) to
	// the settings this input doesn't set itself.
	Level       string    `json:"level,omitempty"`
	BlockAIBots bool      `json:"block_ai_bots"`
	WAF         *bool     `json:"waf,omitempty"`
	AdminAllow  *[]string `json:"admin_allow,omitempty"`
	TrustedIPs  *[]string `json:"trusted_ips,omitempty"`
	DenyIPs     *[]string `json:"deny_ips,omitempty"`
	XMLRPC      *bool     `json:"xmlrpc,omitempty"`
	// Rate limits and challenge difficulty; 0 restores the server default.
	RateRPS       *float64  `json:"rate_rps,omitempty"`
	RateBurst     *int      `json:"rate_burst,omitempty"`
	LoginPerMin   *float64  `json:"login_per_min,omitempty"`
	ChallengeBits *int      `json:"challenge_bits,omitempty"`
	Reputation    *string   `json:"reputation,omitempty"`     // off | challenge | block
	CountryMode   *string   `json:"country_mode,omitempty"`   // off | block | allow
	Countries     *[]string `json:"countries,omitempty"`      // ISO 3166-1 alpha-2
	CountryAction *string   `json:"country_action,omitempty"` // block | challenge
	BodyWAF       *string   `json:"body_waf,omitempty"`       // off | detect | block
}

// maxNets bounds each IP list; the shield checks them on every request.
const maxNets = 64

// Values of the enumerated shield settings.
const (
	ReputationOff       = "off"
	ReputationChallenge = "challenge"
	ReputationBlock     = "block"

	CountryOff   = "off"
	CountryBlock = "block" // the listed countries get the action
	CountryAllow = "allow" // everyone but the listed countries gets it

	BodyWAFOff    = "off"
	BodyWAFDetect = "detect" // log what would be blocked
	BodyWAFBlock  = "block"
)

// Bounds of the tunable limits. The difficulty range keeps a challenge
// solvable on an old phone (22 bits: ~4M hashes, several seconds) and
// meaningful (10 bits is ~1000 hashes).
const (
	maxRateRPS     = 1000
	maxRateBurst   = 10000
	maxLoginPerMin = 600
	minChallenge   = 10
	maxChallenge   = 22
)

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

func (s *Service) SetShield(ctx context.Context, id string, in ShieldInput) (*store.Site, error) {
	if !in.Mode.Valid() {
		return nil, fmt.Errorf("%w: shield mode", ErrInvalidInput)
	}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Level != "" {
		if err := applyLevel(&in, in.Level); err != nil {
			return nil, err
		}
	}
	c := st.ShieldSettings()
	c.Mode, c.BlockAIBots = string(in.Mode), in.BlockAIBots
	if err := applyShieldInput(&c, in); err != nil {
		return nil, err
	}
	if err := s.Store.SetShield(ctx, id, c); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	return s.Store.GetSite(ctx, id)
}

// applyShieldInput validates the fields that were sent and applies them.
func applyShieldInput(c *store.ShieldSettings, in ShieldInput) error {
	var err error
	if in.WAF != nil {
		c.WAF = *in.WAF
	}
	if in.XMLRPC != nil {
		c.XMLRPC = *in.XMLRPC
	}
	for _, l := range []struct {
		field string
		in    *[]string
		out   *[]string
	}{{"admin_allow", in.AdminAllow, &c.AdminAllow}, {"trusted_ips", in.TrustedIPs, &c.TrustedIPs},
		{"deny_ips", in.DenyIPs, &c.DenyIPs}} {
		if l.in != nil {
			if *l.out, err = normalizeNets(l.field, *l.in); err != nil {
				return err
			}
		}
	}
	if in.RateRPS != nil {
		if v := *in.RateRPS; v != 0 && (v < 0.1 || v > maxRateRPS) {
			return fmt.Errorf("%w: rate_rps must be 0 (default) or 0.1-%d requests per second", ErrInvalidInput, maxRateRPS)
		}
		c.RateRPS = *in.RateRPS
	}
	if in.RateBurst != nil {
		if v := *in.RateBurst; v < 0 || v > maxRateBurst {
			return fmt.Errorf("%w: rate_burst must be 0 (default) or 1-%d", ErrInvalidInput, maxRateBurst)
		}
		c.RateBurst = *in.RateBurst
	}
	if in.LoginPerMin != nil {
		if v := *in.LoginPerMin; v != 0 && (v < 0.5 || v > maxLoginPerMin) {
			return fmt.Errorf("%w: login_per_min must be 0 (default) or 0.5-%d attempts per minute", ErrInvalidInput, maxLoginPerMin)
		}
		c.LoginPerMin = *in.LoginPerMin
	}
	if in.ChallengeBits != nil {
		if v := *in.ChallengeBits; v != 0 && (v < minChallenge || v > maxChallenge) {
			return fmt.Errorf("%w: challenge_bits must be 0 (default) or %d-%d", ErrInvalidInput, minChallenge, maxChallenge)
		}
		c.ChallengeBits = *in.ChallengeBits
	}
	if in.Reputation != nil {
		if !slices.Contains([]string{ReputationOff, ReputationChallenge, ReputationBlock}, *in.Reputation) {
			return fmt.Errorf("%w: reputation must be off, challenge or block", ErrInvalidInput)
		}
		c.Reputation = *in.Reputation
	}
	if in.BodyWAF != nil {
		if !slices.Contains([]string{BodyWAFOff, BodyWAFDetect, BodyWAFBlock}, *in.BodyWAF) {
			return fmt.Errorf("%w: body_waf must be off, detect or block", ErrInvalidInput)
		}
		c.BodyWAF = *in.BodyWAF
	}
	if in.CountryMode != nil {
		if !slices.Contains([]string{CountryOff, CountryBlock, CountryAllow}, *in.CountryMode) {
			return fmt.Errorf("%w: country_mode must be off, block or allow", ErrInvalidInput)
		}
		c.CountryMode = *in.CountryMode
	}
	if in.CountryAction != nil {
		if *in.CountryAction != ReputationBlock && *in.CountryAction != ReputationChallenge {
			return fmt.Errorf("%w: country_action must be block or challenge", ErrInvalidInput)
		}
		c.CountryAction = *in.CountryAction
	}
	if in.Countries != nil {
		if c.Countries, err = normalizeCountries(*in.Countries); err != nil {
			return err
		}
	}
	if c.CountryMode != CountryOff && len(c.Countries) == 0 {
		return fmt.Errorf("%w: country rules need at least one country", ErrInvalidInput)
	}
	return nil
}

func normalizeCountries(in []string) ([]string, error) {
	if len(in) > 250 {
		return nil, fmt.Errorf("%w: countries: too many", ErrInvalidInput)
	}
	out := []string{}
	for _, v := range in {
		v = strings.ToUpper(strings.TrimSpace(v))
		if !countryRe.MatchString(v) {
			return nil, fmt.Errorf("%w: countries: %q is not a two-letter country code (ISO 3166)", ErrInvalidInput, v)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out, nil
}

// shieldSettings turns a stored site into what the shield checks per
// request.
func shieldSettings(st *store.Site) shield.SiteSettings {
	ss := shield.SiteSettings{ID: st.ID, Mode: shield.Mode(st.ShieldMode), BlockAIBots: st.BlockAIBots, Inspect: st.WAF,
		AdminAllow: mustPrefixes(st.AdminAllow), Trusted: mustPrefixes(st.TrustedIPs), Deny: mustPrefixes(st.DenyIPs),
		RequestsPerSecond: st.RateRPS, Burst: float64(st.RateBurst), LoginPerMinute: st.LoginPerMin,
		Difficulty: st.ChallengeBits, Reputation: verdictFor(st.Reputation)}
	if st.CountryMode != CountryOff && len(st.Countries) > 0 {
		ss.CountryAction = verdictFor(st.CountryAction)
		ss.CountryAllow = st.CountryMode == CountryAllow
		ss.Countries = make(map[string]bool, len(st.Countries))
		for _, cc := range st.Countries {
			ss.Countries[cc] = true
		}
	}
	return ss
}

func verdictFor(action string) shield.Verdict {
	switch action {
	case ReputationChallenge:
		return shield.Challenge
	case ReputationBlock:
		return shield.Block
	}
	return shield.Allow
}

// CountryRulesInUse reports whether any active site has country rules, so
// the country database is only downloaded when something needs it.
func (s *Service) CountryRulesInUse() bool {
	m := s.shieldSt.Load()
	if m == nil {
		return false
	}
	for _, st := range *m {
		if st.CountryAction != shield.Allow {
			return true
		}
	}
	return false
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

// Server-wide shield lists, kept in panel settings.
const (
	settingGlobalAllow = "shield_global_allow"
	settingGlobalDeny  = "shield_global_deny"
)

// GlobalLists are the server-wide allow and deny lists.
type GlobalLists struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Shield converts the lists for the shield.
func (g GlobalLists) Shield() shield.Global {
	return shield.Global{Allow: mustPrefixes(g.Allow), Deny: mustPrefixes(g.Deny)}
}

func (s *Service) GlobalLists(ctx context.Context) (GlobalLists, error) {
	allow, err := s.Store.Setting(ctx, settingGlobalAllow)
	if err != nil {
		return GlobalLists{}, err
	}
	deny, err := s.Store.Setting(ctx, settingGlobalDeny)
	return GlobalLists{Allow: splitNonEmpty(allow), Deny: splitNonEmpty(deny)}, err
}

// SetGlobalLists validates and stores the lists; the caller hands the
// result to the shield.
func (s *Service) SetGlobalLists(ctx context.Context, in GlobalLists) (GlobalLists, error) {
	var out GlobalLists
	var err error
	if out.Allow, err = normalizeNets("allow", in.Allow); err != nil {
		return out, err
	}
	if out.Deny, err = normalizeNets("deny", in.Deny); err != nil {
		return out, err
	}
	if err := s.Store.SetSetting(ctx, settingGlobalAllow, strings.Join(out.Allow, ",")); err != nil {
		return out, err
	}
	return out, s.Store.SetSetting(ctx, settingGlobalDeny, strings.Join(out.Deny, ","))
}

func splitNonEmpty(v string) []string {
	if v == "" {
		return []string{}
	}
	return strings.Split(v, ",")
}
