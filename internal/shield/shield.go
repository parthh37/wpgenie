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
	_ "embed"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	SiteHeader = "X-WPGenie-Site" // set by Caddy via header_up; never trusted from clients
	// VerdictHeader marks responses produced by the shield so access-log
	// analytics can tell a shield block apart from WordPress's own 403s.
	VerdictHeader = "X-WPGenie-Shield"
	passCookie    = "wpg_pass"
)

type SiteSettings struct {
	ID          string
	Mode        Mode
	BlockAIBots bool
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

	Logger *slog.Logger
}

type Shield struct {
	o        Options
	limiter  *rateLimiter
	login    *rateLimiter
	verifier *crawlerVerifier
	now      func() time.Time
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
	return &Shield{
		o:        o,
		limiter:  newRateLimiter(o.RequestsPerSecond, o.Burst),
		login:    newRateLimiter(o.LoginPerMinute/60, o.LoginBurst),
		verifier: newCrawlerVerifier(o.Resolver),
		now:      time.Now,
	}
}

// Run sweeps idle rate-limit buckets until ctx is cancelled.
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
		}
	}
}

// Classify combines UA matching with DNS verification of search engines.
func (s *Shield) Classify(ctx context.Context, ip, ua string) Class {
	c, suffixes := classifyUA(ua)
	if c == ClassVerifiedCrawler && !s.verifier.verify(ctx, ip, suffixes) {
		return ClassSpoofedCrawler
	}
	return c
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
		path, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Uri"), "?")
		method := r.Header.Get("X-Forwarded-Method")
		now := s.now()

		sig := Signals{
			Mode:        site.Mode,
			BlockAIBots: site.BlockAIBots,
			Class:       s.Classify(r.Context(), ip, ua),
			HasPass:     s.validPass(r, site.ID, ip, ua, now),
			LoginPath:   isLoginPath(method, path),
		}
		key := site.ID + "|" + ip
		if sig.LoginPath {
			sig.RateExceeded = !s.login.allow(key, now)
		} else {
			sig.RateExceeded = !s.limiter.allow(key, now)
		}

		v := Decide(sig)
		if v != Allow {
			s.o.Logger.Debug("shield", "site", site.ID, "ip", ip, "class", sig.Class, "verdict", v, "path", path)
		}
		if v != Allow {
			w.Header().Set(VerdictHeader, v.String())
		}
		switch v {
		case Allow:
			w.WriteHeader(http.StatusOK)
		case Challenge:
			s.serveChallenge(w, site, ip, r.Header.Get("X-Forwarded-Uri"), now)
		case Throttle:
			w.Header().Set("Retry-After", "30")
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
		default:
			http.Error(w, "Access denied", http.StatusForbidden)
		}
	})
}

//go:embed challenge.html
var challengeHTML string

var challengeTmpl = template.Must(template.New("challenge").Parse(challengeHTML))

func (s *Shield) serveChallenge(w http.ResponseWriter, site SiteSettings, ip, returnTo string, now time.Time) {
	difficulty := s.o.Difficulty
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

func isLoginPath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	return path == "/wp-login.php" || path == "/xmlrpc.php" || strings.HasSuffix(path, "/wp-login.php")
}

// clientIP trusts X-Forwarded-For because the shield only listens on
// loopback behind Caddy, which replaces (not appends to) the header for
// untrusted clients. The last hop is the one Caddy observed.
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
