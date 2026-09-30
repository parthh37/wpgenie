// Package shield is WPGenie's edge protection: bot classification, AI
// crawler blocking, per-IP rate limiting and a self-hosted proof-of-work
// challenge (no third-party CAPTCHA, no tracking).
//
// Caddy calls CheckHandler via `forward_auth` for every dynamic request.
// A 2xx lets the request through; any other response is sent to the client
// as-is, which is how the challenge page is delivered.
package shield

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	SiteHeader = "X-WPGenie-Site" // set by Caddy via header_up; never trusted from clients
	// VerdictHeader marks responses produced by the shield so access-log
	// analytics can tell a shield block apart from WordPress's own 403s.
	VerdictHeader = "X-WPGenie-Shield"
	// HealthHeader carries Options.HealthToken on the daemon's own health
	// checks, which then skip the shield.
	HealthHeader = "X-WPGenie-Health"
	passCookie   = "wpg_pass"
)

type SiteSettings struct {
	ID          string
	Mode        Mode
	BlockAIBots bool
	// Inspect enables request inspection (the WAF rules in inspect.go).
	Inspect bool
	// AdminAllow, if not empty, is the only networks allowed to reach
	// wp-login.php and wp-admin.
	AdminAllow []netip.Prefix
	// Trusted networks bypass the shield entirely (office, uptime monitor).
	Trusted []netip.Prefix
	// Deny networks are blocked outright.
	Deny []netip.Prefix
	// Webmail marks Roundcube: its login is a POST to ?_task=login.
	Webmail bool
	// Per-site rate limits and challenge difficulty; zero values use the
	// server defaults (Options).
	RequestsPerSecond float64
	Burst             float64
	LoginPerMinute    float64
	Difficulty        int
	// Reputation is what clients on an IP blocklist get (Challenge or
	// Block); Allow ignores the lists.
	Reputation Verdict
	// Country rules: CountryAction (Challenge or Block; Allow = no rule) is
	// applied to clients from Countries, or with CountryAllow, to clients
	// from anywhere else.
	Countries     map[string]bool
	CountryAllow  bool
	CountryAction Verdict
}

// Reputation knows which addresses are on IP blocklists, and where they are.
type Reputation interface {
	// Listed returns the name of a blocklist that has the address.
	Listed(a netip.Addr) (list string, ok bool)
	// Country returns the address's ISO 3166 country code ("" if unknown);
	// ok is false while no country database is loaded, which disables
	// country rules rather than blocking everyone.
	Country(a netip.Addr) (cc string, ok bool)
}

// Global is the server-wide configuration, applied on every site.
type Global struct {
	Allow []netip.Prefix // never challenged, blocked or banned
	Deny  []netip.Prefix // blocked on every site
}

// SiteLookup returns the shield settings for a site ID.
type SiteLookup func(id string) (SiteSettings, bool)

type Options struct {
	Secret     []byte
	Sites      SiteLookup
	Resolver   Resolver // nil = system DNS
	Difficulty int      // leading zero bits for the PoW; ~16 solves in <1s on a phone
	PassTTL    time.Duration

	RequestsPerSecond float64 // per IP per site, dynamic requests only
	Burst             float64
	LoginPerMinute    float64
	LoginBurst        float64
	// HealthToken, if set, is the secret the daemon's health checks send in
	// HealthHeader. Not loopback: a local tunnel (cloudflared, ssh -L)
	// makes every visitor it forwards look like loopback.
	HealthToken string
	// Reputation, if set, provides IP blocklists and country lookups.
	Reputation Reputation
	// OnAttack, if set, is told when a site in ModeAuto enters (active) or
	// leaves an automatic Under attack period, including by EndAttack; on
	// the end, st.Until is when it ended. Calls come from a goroutine of
	// their own, in order, and may be slow without holding up requests.
	OnAttack func(siteID string, active bool, st AttackState)

	Logger *slog.Logger
}

type Shield struct {
	o        Options
	limiter  *rateLimiter
	login    *rateLimiter
	verifier *crawlerVerifier
	bans     *banList
	events   *eventLog
	attacks  *attackDetector
	global   atomic.Pointer[Global]
	now      func() time.Time

	// OnAttack deliveries waiting for the callback goroutine (attack.go).
	cbMu      sync.Mutex
	cbQueue   []attackChange
	cbRunning bool

	// decisions counts verdicts since start, by Verdict (metrics).
	decisions [4]atomic.Uint64
}

func New(o Options) *Shield {
	if o.Difficulty == 0 {
		o.Difficulty = 16
	}
	if o.PassTTL == 0 {
		o.PassTTL = 12 * time.Hour
	}
	if o.RequestsPerSecond == 0 {
		o.RequestsPerSecond, o.Burst = 10, 60
	}
	if o.LoginPerMinute == 0 {
		o.LoginPerMinute, o.LoginBurst = 6, 10
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Shield{
		o:        o,
		limiter:  newRateLimiter(),
		login:    newRateLimiter(),
		verifier: newCrawlerVerifier(o.Resolver),
		bans:     newBanList(),
		events:   newEventLog(500),
		now:      time.Now,
	}
	s.attacks = newAttackDetector(s.attackChanged)
	return s
}

// SetGlobal replaces the server-wide allow and deny lists.
func (s *Shield) SetGlobal(g Global) { s.global.Store(&g) }

// Run sweeps idle rate-limit buckets and ends automatic attacks on sites
// that went quiet, until ctx is cancelled.
func (s *Shield) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.limiter.sweep(now)
			s.login.sweep(now)
			s.bans.sweep(now)
			s.checkAttacks(s.now())
		}
	}
}

// Classify combines UA matching with DNS verification of search engines.
// A claimed crawler whose DNS can't be checked right now is treated as an
// unverified script: throttled if it's too fast, never blocked or trusted.
func (s *Shield) Classify(_ context.Context, ip, ua string) Class {
	c, suffixes := classifyUA(ua)
	if c != ClassVerifiedCrawler {
		return c
	}
	switch s.verifier.verify(ip, suffixes) {
	case verified:
		return ClassVerifiedCrawler
	case spoofed:
		return ClassSpoofedCrawler
	}
	return ClassScript
}

func (s *Shield) CheckHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, ok := s.o.Sites(r.Header.Get(SiteHeader))
		if !ok {
			// Unknown site: fail open. Availability of a customer's site beats
			// protection during a config race (e.g. site created seconds ago).
			w.WriteHeader(http.StatusOK)
			return
		}
		ip := clientIP(r)
		ua := r.UserAgent()
		uri := r.Header.Get("X-Forwarded-Uri")
		script := ScriptPath(uri)
		method := r.Header.Get("X-Forwarded-Method")
		now := s.now()
		banKey, _ := BanKey(ip)

		global := s.global.Load()
		if global == nil {
			global = &Global{}
		}
		sig := Signals{
			Mode:        site.Mode,
			BlockAIBots: site.BlockAIBots,
			Trusted:     s.isHealthCheck(r) || inAny(ip, site.Trusted) || inAny(ip, global.Allow),
		}
		if sig.Trusted || sig.Mode == ModeOff {
			s.decisions[Allow].Add(1)
			w.WriteHeader(http.StatusOK) // skip the DNS lookups and rate limit accounting
			return
		}
		// Every judged request counts towards flood detection, whatever the
		// verdict: a flood the shield refuses is still a flood.
		traffic, flooded := s.attacks.record(site.ID, site.Mode == ModeAuto, now)
		if flooded && site.Mode == ModeAuto {
			// Decide sees the effective mode; the challenge gets Under
			// attack's difficulty too.
			sig.Mode, sig.AutoAttack, site.Mode = ModeUnderAttack, true, ModeUnderAttack
		}
		sig.Banned = s.bans.banned(banKey, now)
		sig.Class = s.Classify(r.Context(), ip, ua)
		sig.HasPass = s.validPass(r, site.ID, ip, ua, now)
		sig.LoginPath = isLoginPath(method, script) ||
			// Roundcube reads _task from the query, body or cookie; any POST
			// without a logged-in session is a login attempt.
			(site.Webmail && method == http.MethodPost && !hasCookie(r, "roundcube_sessauth"))
		sig.AdminDenied = len(site.AdminAllow) > 0 && !inAny(ip, site.AdminAllow) &&
			(isAdminPath(script) || isAuthenticatedAPI(r, script, uri))
		sig.CrossSite = crossSite(r.Header.Get("Sec-Fetch-Site"))
		sig.Denied = inAny(ip, site.Deny) || inAny(ip, global.Deny)
		s.checkReputation(&sig, site, ip)
		if site.Inspect {
			sig.Threat = Inspect(Request{Method: method, URI: uri, UA: ua, Referer: r.Header.Get("Referer"),
				Cookie: r.Header.Get("Cookie"), Browser: sig.Class == ClassHuman})
		}
		// IPv6 clients share a budget per /64, like bans: otherwise one
		// subscriber rotates through addresses for fresh bursts forever.
		key := site.ID + "|" + banKey
		if banKey == "" {
			key = site.ID + "|" + ip
		}
		if sig.LoginPath {
			perMin, burst := s.loginLimit(site)
			sig.RateExceeded = !s.login.allow(key, now, perMin/60, burst)
		} else {
			rps, burst := s.rateLimit(site)
			sig.RateExceeded = !s.limiter.allow(key, now, rps, burst)
		}

		v := Decide(sig)
		s.decisions[v].Add(1)
		if floodRefusal(sig, v) {
			client := banKey
			if client == "" {
				client = ip
			}
			traffic.refuse(client)
		}
		if v != Allow {
			w.Header().Set(VerdictHeader, v.String())
			path, _, _ := strings.Cut(uri, "?")
			reason := reasonFor(sig, v)
			s.o.Logger.Debug("shield", "site", site.ID, "ip", ip, "class", sig.Class, "verdict", v, "reason", reason, "path", path)
			if v != Challenge {
				s.events.add(Event{Time: now, Site: site.ID, IP: ip, Verdict: v.String(), Reason: reason, Path: path})
			}
			if n := strikes(sig, v); n > 0 && banKey != "" {
				if banned, until := s.bans.strike(banKey, n, reason, now); banned {
					s.o.Logger.Warn("shield: client banned", "addr", banKey, "until", until, "reason", reason, "site", site.ID)
					s.events.add(Event{Time: now, Site: site.ID, IP: ip, Verdict: "ban", Reason: reason + " (until " + until.UTC().Format(time.RFC3339) + ")", Path: path})
				}
			}
		}
		switch v {
		case Allow:
			w.WriteHeader(http.StatusOK)
		case Challenge:
			s.serveChallenge(w, site, ip, uri, now)
		case Throttle:
			w.Header().Set("Retry-After", "30")
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
		default:
			http.Error(w, "Access denied", http.StatusForbidden)
		}
	})
}

// checkReputation fills in the country and blocklist signals. Only public
// addresses are looked up: a private one (a local tunnel, the Docker
// network) has no country and is on no list.
func (s *Shield) checkReputation(sig *Signals, site SiteSettings, ip string) {
	if s.o.Reputation == nil || (site.Reputation == Allow && site.CountryAction == Allow) {
		return
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return
	}
	a = a.Unmap()
	if !isPublic(a) {
		return
	}
	if site.Reputation != Allow {
		if list, ok := s.o.Reputation.Listed(a); ok {
			sig.Reputation, sig.ReputationList = site.Reputation, list
		}
	}
	if site.CountryAction != Allow {
		if cc, ok := s.o.Reputation.Country(a); ok && site.Countries[cc] != site.CountryAllow {
			if cc == "" {
				cc = "unknown"
			}
			sig.Country, sig.CountryCode = site.CountryAction, cc
		}
	}
}

func isPublic(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// rateLimit is a site's request budget per client: its own, or the
// server's. A site that only sets a rate gets the default burst-to-rate
// ratio (60 for 10/s).
func (s *Shield) rateLimit(site SiteSettings) (perSecond, burst float64) {
	perSecond, burst = s.o.RequestsPerSecond, s.o.Burst
	if site.RequestsPerSecond > 0 {
		perSecond, burst = site.RequestsPerSecond, max(1, site.RequestsPerSecond*s.o.Burst/s.o.RequestsPerSecond)
	}
	if site.Burst > 0 {
		burst = site.Burst
	}
	return perSecond, burst
}

// loginLimit is the login budget: attempts per minute and a burst that
// scales with it (10 for the default 6/min).
func (s *Shield) loginLimit(site SiteSettings) (perMinute, burst float64) {
	if site.LoginPerMinute <= 0 {
		return s.o.LoginPerMinute, s.o.LoginBurst
	}
	return site.LoginPerMinute, min(max(2, math.Round(site.LoginPerMinute*s.o.LoginBurst/s.o.LoginPerMinute)), 100)
}

// reasonFor names the signal that produced a verdict, for the security log.
func reasonFor(s Signals, v Verdict) string {
	switch {
	case s.Banned:
		return "banned"
	case s.Threat != ThreatNone:
		return s.Threat.String()
	case s.AdminDenied:
		return "admin_allowlist"
	case s.Denied:
		return "deny_list"
	case v == Block && (s.Class == ClassAttackTool || s.Class == ClassSpoofedCrawler || s.Class == ClassAIBot):
		return s.Class.String()
	case v == Block && s.Country == Block:
		return "country:" + s.CountryCode
	case v == Block && s.Reputation == Block:
		return "reputation:" + s.ReputationList
	case s.RateExceeded && s.LoginPath:
		return "login_rate_limit"
	case s.RateExceeded:
		return "rate_limit"
	case s.Mode == ModeUnderAttack && s.AutoAttack:
		return "under_attack_auto"
	case s.Mode == ModeUnderAttack:
		return "under_attack"
	case s.Country != Allow:
		return "country:" + s.CountryCode
	case s.Reputation != Allow:
		return "reputation:" + s.ReputationList
	}
	return "under_attack"
}

// Record adds an event decided elsewhere (request-body inspection in Caddy)
// to the security log.
func (s *Shield) Record(e Event) {
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	s.events.add(e)
}

// TeeEvents hands every event of the security log (blocks, bans,
// attacks, recorded WAF matches) to fn as well, nil to stop: log shipping
// keeps them after the in-memory ring forgets them. fn is called on the
// request path and must not block.
func (s *Shield) TeeEvents(fn func(Event)) {
	if fn == nil {
		s.events.tee.Store(nil)
		return
	}
	s.events.tee.Store(&fn)
}

// Bans lists active bans, longest-lasting first.
func (s *Shield) Bans() []Ban { return s.bans.list(s.now()) }

// Decisions counts the verdicts given since the daemon started, by name
// (allow, challenge, block, throttle).
func (s *Shield) Decisions() map[string]uint64 {
	out := make(map[string]uint64, len(s.decisions))
	for v := range s.decisions {
		out[Verdict(v).String()] = s.decisions[v].Load()
	}
	return out
}

// Events returns recent blocks, throttles and bans, newest first.
func (s *Shield) Events(site string, limit int) []Event { return s.events.recent(site, limit) }

var ErrBadAddr = errors.New("not an IP address or ban entry")

// BanAddr bans an address by hand on every site.
func (s *Shield) BanAddr(addr string, d time.Duration, reason string) (string, error) {
	key, err := normalizeBanAddr(addr)
	if err != nil {
		return "", err
	}
	if d <= 0 || d > MaxManualBan {
		d = MaxManualBan
	}
	if reason == "" {
		reason = "manual"
	}
	s.bans.ban(key, d, reason, s.now())
	return key, nil
}

// Unban lifts a ban; addr is an IP or an entry as listed by Bans.
func (s *Shield) Unban(addr string) (bool, error) {
	key, err := normalizeBanAddr(addr)
	if err != nil {
		return false, err
	}
	return s.bans.unban(key), nil
}

func normalizeBanAddr(addr string) (string, error) {
	if key, ok := BanKey(addr); ok {
		return key, nil
	}
	if p, err := netip.ParsePrefix(addr); err == nil && p.Addr().Is6() && p.Bits() == 64 {
		return p.Masked().String(), nil
	}
	return "", ErrBadAddr
}

func (s *Shield) isHealthCheck(r *http.Request) bool {
	got := r.Header.Get(HealthHeader)
	return s.o.HealthToken != "" && got != "" &&
		subtle.ConstantTimeCompare([]byte(got), []byte(s.o.HealthToken)) == 1
}

func hasCookie(r *http.Request, name string) bool {
	c, err := r.Cookie(name)
	return err == nil && c.Value != ""
}

// crossSite: the browser says another site initiated the request. Tools
// don't send Sec-Fetch-* at all.
func crossSite(secFetchSite string) bool {
	return secFetchSite == "cross-site" || secFetchSite == "same-site"
}

// isAuthenticatedAPI: a REST or XML-RPC request carrying credentials
// (application passwords), which reaches the admin without wp-admin.
func isAuthenticatedAPI(r *http.Request, script, uri string) bool {
	if r.Header.Get("Authorization") == "" {
		return false
	}
	return strings.HasSuffix(script, "/xmlrpc.php") || strings.HasPrefix(script, "/wp-json") ||
		strings.Contains(uri, "rest_route=")
}

func inAny(ip string, nets []netip.Prefix) bool {
	if len(nets) == 0 {
		return false
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, n := range nets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

//go:embed challenge.html
var challengeHTML string

var challengeTmpl = template.Must(template.New("challenge").Parse(challengeHTML))

func (s *Shield) serveChallenge(w http.ResponseWriter, site SiteSettings, ip, returnTo string, now time.Time) {
	difficulty := s.o.Difficulty
	if site.Difficulty > 0 {
		difficulty = site.Difficulty
	}
	if site.Mode == ModeUnderAttack {
		difficulty += 2 // 4x the work per solve during an incident
	}
	token, err := sign(s.o.Secret, challengePayload{
		Site: site.ID, Net: ipBucket(ip), Seed: randomHex(12),
		Difficulty: difficulty, Expires: now.Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(http.StatusForbidden)
	challengeTmpl.Execute(w, map[string]any{
		"Token": token, "Difficulty": difficulty, "Return": safeReturn(returnTo),
	})
}

// VerifyHandler checks a solved challenge and issues the pass cookie.
func (s *Shield) VerifyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, ok := s.o.Sites(r.Header.Get(SiteHeader))
		if !ok {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		token, nonce := r.PostFormValue("token"), r.PostFormValue("nonce")
		ip, now := clientIP(r), s.now()

		var p challengePayload
		if len(nonce) > 32 || open(s.o.Secret, token, &p) != nil ||
			p.Site != site.ID || p.Net != ipBucket(ip) || expired(p.Expires, now) ||
			leadingZeroBits(token, nonce) < p.Difficulty {
			http.Error(w, "Verification failed, please reload the page.", http.StatusForbidden)
			return
		}

		pass, err := sign(s.o.Secret, passPayload{
			Site: site.ID, Net: p.Net, UA: uaHash(r.UserAgent()), Expires: now.Add(s.o.PassTTL).Unix(),
		})
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: passCookie, Value: pass, Path: "/", MaxAge: int(s.o.PassTTL.Seconds()),
			HttpOnly: true, Secure: r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, safeReturn(r.PostFormValue("return")), http.StatusSeeOther)
	})
}

func (s *Shield) validPass(r *http.Request, siteID, ip, ua string, now time.Time) bool {
	c, err := r.Cookie(passCookie)
	if err != nil {
		return false
	}
	var p passPayload
	if open(s.o.Secret, c.Value, &p) != nil {
		return false
	}
	return p.Site == siteID && p.Net == ipBucket(ip) && p.UA == uaHash(ua) && !expired(p.Expires, now)
}

// isLoginPath takes the script (ScriptPath), so /wp-login.php/x can't
// escape the login budget.
func isLoginPath(method, script string) bool {
	if method != http.MethodPost {
		return false
	}
	return strings.HasSuffix(script, "/wp-login.php") || strings.HasSuffix(script, "/xmlrpc.php")
}

// clientIP trusts X-Forwarded-For because the shield only listens on
// loopback behind Caddy, which sets it to {client_ip} on every shield call:
// the peer's address, or CF-Connecting-IP when the peer is Cloudflare's
// edge. Anything a client sent is replaced. The last hop is taken anyway.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// safeReturn only allows same-origin relative paths, preventing the verify
// endpoint from becoming an open redirect.
func safeReturn(u string) string {
	if u == "" || u[0] != '/' || strings.HasPrefix(u, "//") || strings.HasPrefix(u, "/\\") || strings.ContainsAny(u, "\r\n") {
		return "/"
	}
	return u
}
