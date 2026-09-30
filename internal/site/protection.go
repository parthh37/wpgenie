package site

import (
	"fmt"

	"github.com/parthh37/wpgenie/internal/store"
)

// Protection levels are named sets of the shield's technical settings
// (the firewalls, blocklists, rate limits and the challenge's difficulty),
// so a site owner picks how strict to be rather than tuning each one.
// Recommended is what new sites get. A site whose settings match no level
// (someone tuned them under Advanced) is "custom".
//
// Levels leave alone what isn't about strictness: who is trusted or
// blocked by address or country, XML-RPC, AI crawlers, and when visitors
// are checked (the shield mode).
const (
	LevelBasic       = "basic"
	LevelRecommended = "recommended"
	LevelStrict      = "strict"
	LevelCustom      = "custom"
)

// ProtectionLevel is one level's settings.
type ProtectionLevel struct {
	ID          string  `json:"id"`
	WAF         bool    `json:"waf"`
	BodyWAF     string  `json:"body_waf"`
	Reputation  string  `json:"reputation"`
	RateRPS     float64 `json:"rate_rps"`
	RateBurst   int     `json:"rate_burst"`
	LoginPerMin float64 `json:"login_per_min"`
	// ChallengeBits: 0 is the server default (16).
	ChallengeBits int `json:"challenge_bits"`
}

// ProtectionLevels lists the levels, least strict first.
var ProtectionLevels = []ProtectionLevel{
	// Basic: known attacks are still blocked, but nothing that could stop
	// an honest visitor: request bodies are only logged, blocklisted
	// addresses get in, and the limits are generous (busy APIs, shops
	// behind shared office addresses).
	{ID: LevelBasic, WAF: true, BodyWAF: BodyWAFDetect, Reputation: ReputationOff, RateRPS: 20, RateBurst: 120, LoginPerMin: 10},
	// Recommended: new sites' defaults.
	{ID: LevelRecommended, WAF: true, BodyWAF: BodyWAFBlock, Reputation: ReputationChallenge},
	// Strict: for sites attacked often: blocklisted addresses are refused,
	// the limits are halved and a challenge takes about four times longer.
	{ID: LevelStrict, WAF: true, BodyWAF: BodyWAFBlock, Reputation: ReputationBlock, RateRPS: 5, RateBurst: 30, LoginPerMin: 3,
		ChallengeBits: 18},
}

// LevelOf returns the level a site's settings match, or LevelCustom.
func LevelOf(st *store.Site) string {
	for _, l := range ProtectionLevels {
		if st.WAF == l.WAF && st.BodyWAF == l.BodyWAF && st.Reputation == l.Reputation && st.RateRPS == l.RateRPS &&
			st.RateBurst == l.RateBurst && st.LoginPerMin == l.LoginPerMin && st.ChallengeBits == l.ChallengeBits {
			return l.ID
		}
	}
	return LevelCustom
}

// applyLevel sets a level's settings in a shield input where the input
// has none of its own.
func applyLevel(in *ShieldInput, id string) error {
	for _, l := range ProtectionLevels {
		if l.ID == id {
			orDefault(&in.WAF, l.WAF)
			orDefault(&in.BodyWAF, l.BodyWAF)
			orDefault(&in.Reputation, l.Reputation)
			orDefault(&in.RateRPS, l.RateRPS)
			orDefault(&in.RateBurst, l.RateBurst)
			orDefault(&in.LoginPerMin, l.LoginPerMin)
			orDefault(&in.ChallengeBits, l.ChallengeBits)
			return nil
		}
	}
	return fmt.Errorf("%w: level must be basic, recommended or strict", ErrInvalidInput)
}

func orDefault[T any](p **T, v T) {
	if *p == nil {
		*p = &v
	}
}
