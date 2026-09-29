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
	// Trusted clients skip the shield: the server's own health checks
	// (proven with a per-process secret header) and the site's trusted IPs.
	Trusted bool
	// Banned is set while the client's address is serving an automatic ban
	// for repeated attacks (see bans.go), on any site.
	Banned bool
	// Threat is what request inspection found (see inspect.go).
	Threat Threat
	// AdminDenied: the site restricts wp-admin/wp-login.php to an IP
	// allowlist and the client isn't on it.
	AdminDenied bool
	// CrossSite: a browser sent this because a page on another site told it
	// to (Sec-Fetch-Site), e.g. an <img> pointing at an attack URL.
	CrossSite bool
	// Denied: the site's or the server's deny list covers the client.
	Denied bool
	// Country is what the site's country rules do with the client's
	// country: Allow (no rule applies), Challenge or Block.
	Country     Verdict
	CountryCode string // for the security log
	// Reputation is what the site does with a client on an IP blocklist:
	// Allow (not listed, or the site ignores lists), Challenge or Block.
	Reputation     Verdict
	ReputationList string // which list, for the security log
}

// Decide turns signals into a verdict. It is pure — no I/O, no clock — so
// the whole security policy is unit-testable in one table.
//
// The order encodes three rules:
//  1. Evidence of bad intent is blocked outright. Challenging it would be
//     pointless: an attacker's script solves a proof-of-work as easily as a
//     browser, so a pass must never outweigh an attack signature.
//  2. A pass proves a browser did some work once, not that it is honest, so
//     it skips challenges but never rate limits.
//  3. Clients that can't run JavaScript (search crawlers, uptime monitors,
//     payment webhooks) are throttled rather than challenged when a limit is
//     hit: a challenge they can't solve would silently turn into a block,
//     and challenging Googlebot de-indexes the site. For the same reason,
//     verified search engines are exempt from country and reputation rules.
func Decide(s Signals) Verdict {
	if s.Mode == ModeOff || s.Trusted {
		return Allow
	}
	if s.Banned || s.Threat != ThreatNone || s.AdminDenied || s.Denied {
		return Block
	}
	switch s.Class {
	case ClassAttackTool, ClassSpoofedCrawler:
		return Block
	case ClassAIBot:
		if s.BlockAIBots {
			return Block
		}
	}
	crawler := s.Class == ClassVerifiedCrawler
	if !crawler && (s.Country == Block || s.Reputation == Block) {
		return Block
	}
	if s.RateExceeded {
		// Brute force gets a 429 even from a browser: a login form that
		// accepted a challenge would let a solver keep guessing.
		if s.LoginPath || s.HasPass || s.Class != ClassHuman {
			return Throttle
		}
		return Challenge
	}
	if !s.HasPass && !crawler &&
		(s.Mode == ModeUnderAttack || s.Country == Challenge || s.Reputation == Challenge) {
		// Deliberately includes scripts and AI bots the site allows: during
		// an incident, only proven browsers and verified search engines get
		// through. Webhooks fail until the mode is switched back.
		return Challenge
	}
	return Allow
}

// strikes is how much a verdict counts towards an automatic ban. Only
// unambiguous attack evidence counts: an honest visitor can hit a rate
// limit or the admin allowlist, but never sends an SQL injection.
//
// Nobody must be able to get someone else banned: requests a browser makes
// on another site's behalf (an attack URL in an <img> tag, an auto-submitted
// login form) are blocked but never counted, and a browser's search that
// merely looks like SQL or HTML is blocked without a strike. Deny lists,
// country rules and blocklists describe where a client is, not what it
// did, so they never count either.
func strikes(s Signals, v Verdict) int {
	if v == Allow || s.Trusted || s.Banned || s.CrossSite || s.Class == ClassVerifiedCrawler {
		// Search engines sometimes crawl spam links that carry attack
		// strings; block the URL but never ban Googlebot.
		return 0
	}
	switch {
	case s.Class == ClassAttackTool, s.Class == ClassSpoofedCrawler:
		return 1
	case s.Threat == ThreatSQLi, s.Threat == ThreatXSS, s.Threat == ThreatEnumeration:
		if s.Class == ClassHuman {
			return 0
		}
		return 1
	case s.Threat != ThreatNone:
		return 1 // traversal, code injection, probes: no reader does these
	case s.LoginPath && v == Throttle:
		return 1 // brute force beyond the login budget
	}
	return 0
}
