package shield

// Mode is the per-site protection level chosen in the panel.
type Mode string

const (
	ModeOff         Mode = "off"          // shield not in the request path at all
	ModeStandard    Mode = "standard"     // default: stop bad bots, throttle abuse
	ModeUnderAttack Mode = "under_attack" // DDoS / scraping incident: challenge everyone
)

func (m Mode) Valid() bool {
	return m == ModeOff || m == ModeStandard || m == ModeUnderAttack
}

type Verdict int

const (
	Allow     Verdict = iota // pass the request to WordPress
	Challenge                // serve the proof-of-work interstitial
	Block                    // 403
	Throttle                 // 429 with Retry-After
)

func (v Verdict) String() string {
	return [...]string{"allow", "challenge", "block", "throttle"}[v]
}

// Signals is everything the shield knows about a request when deciding.
type Signals struct {
	Mode        Mode
	BlockAIBots bool  // site setting: refuse AI crawlers/fetchers
	Class       Class // see classify.go
	HasPass     bool  // holds a valid, unexpired challenge-pass cookie
	LoginPath   bool  // POST to wp-login.php / xmlrpc.php: brute-force target
	// RateExceeded is true when the client is over its budget. On LoginPath
	// requests the (much stricter) login budget is used.
	RateExceeded bool
}

// Decide turns signals into a verdict. It is pure — no I/O, no clock — so
// the whole security policy is unit-testable in one table.
func Decide(s Signals) Verdict {
	if s.Mode == ModeOff {
		return Allow
	}

	// TODO(policy): implement the protection policy. Until then the shield
	// only observes (every request is allowed).
	//
	// Decisions to make:
	//   - Which classes are blocked outright vs. challenged? (A ClassScript
	//     could be an uptime monitor or a payment webhook!)
	//   - Does a valid pass let a client skip rate limits, or only the
	//     challenge? What about on LoginPath?
	//   - How should ModeUnderAttack differ from ModeStandard?
	//   - Should a verified search crawler ever be challenged? (It can't
	//     solve one — challenging Googlebot silently de-indexes the site.)
	return Allow
}
