package site

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/parthh37/wpgenie/internal/domain"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/store"
)

// Rules at a site's edge, applied by Caddy before WordPress sees anything:
//
//   - the site lock asks every visitor for a username and password (the
//     browser's own prompt), for a site that isn't ready to be seen (a
//     staging copy, a site being built). Addresses can be let through
//     without it; the panel's sign-in links (/_wpgenie/*) always are.
//   - redirects send visitors from old paths of the site to new ones, or
//     to another address.
//
// Both end up in the Caddyfile (proxy/edge.go): everything here is checked
// strictly, and the proxy checks again that nothing can change its
// structure.

// Site lock.

const (
	minLockPassword = 8
	// maxLockPassword: bcrypt only uses the first 72 bytes.
	maxLockPassword = 72
	// lockCost is bcrypt's work factor. Caddy checks a password against the
	// hash once per visitor's password (it caches the result), so the
	// default is enough and keeps the first page fast.
	lockCost = bcrypt.DefaultCost
)

// LockInput changes a site's lock. Turning it off keeps the username and
// password, so turning it back on needs neither.
type LockInput struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
	// Password: "" keeps the current one (a first lock needs one).
	Password string `json:"password,omitempty"`
	// Allow are addresses and networks that skip the lock; nil keeps them.
	Allow *[]string `json:"allow,omitempty"`
}

// SetLock turns a site's lock on or off, or changes it.
func (s *Service) SetLock(ctx context.Context, id string, in LockInput) (*store.Site, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	was, wasUser, wasHash := st.Lock, st.LockUser, st.LockHash
	if err := applyLock(st, in); err != nil {
		return nil, err
	}
	if err := s.Store.SetSiteLock(ctx, id, st.Lock, st.LockUser, st.LockHash, st.LockAllow); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	if st.Lock != was {
		// Locked, static files come from the site itself (assetCDNURL); the
		// cached pages link to wherever they came from before.
		if err := s.refreshAssetLinks(ctx, id); err != nil {
			s.Log.Warn("switching the CDN's links after a lock change", "site", id, "err", err)
		}
	}
	switch {
	case st.Lock && !was:
		s.event(id, "lock", fmt.Sprintf("Site lock on: visitors need the username %s and its password", st.LockUser))
	case !st.Lock && was:
		s.event(id, "lock", "Site lock off: the site is open to everyone")
	case st.Lock && (st.LockUser != wasUser || st.LockHash != wasHash):
		s.event(id, "lock", fmt.Sprintf("Site lock changed: username %s", st.LockUser))
	}
	return s.Store.GetSite(ctx, id)
}

// refreshAssetLinks rewrites the CDN wrapper for the site as it is now and
// purges the cached pages that link to the old place, if it changed.
func (s *Service) refreshAssetLinks(ctx context.Context, id string) error {
	if _, err := s.Store.GetCDN(ctx, id); errors.Is(err, store.ErrNotFound) {
		return nil // no CDN: nothing links elsewhere
	} else if err != nil {
		return err
	}
	assetURL, err := s.assetCDNURL(ctx, id)
	if err != nil {
		return err
	}
	if err := s.writeCDNWrapper(id, assetURL); err != nil {
		return err
	}
	return s.purgePageCacheFiles(id)
}

// applyLock checks a lock change and applies it to a site's record (the
// password hashed).
func applyLock(st *store.Site, in LockInput) error {
	if u := strings.TrimSpace(in.Username); u != "" {
		if !proxy.LockUserRe.MatchString(u) {
			return fmt.Errorf("%w: the username can have letters, digits and . _ @ - (up to 64), and starts with a letter or digit", ErrInvalidInput)
		}
		st.LockUser = u
	}
	if in.Password != "" {
		if err := checkLockPassword(in.Password); err != nil {
			return err
		}
		h, err := bcrypt.GenerateFromPassword([]byte(in.Password), lockCost)
		if err != nil {
			return err
		}
		st.LockHash = string(h)
	}
	if in.Allow != nil {
		allow, err := normalizeNets("allow", *in.Allow)
		if err != nil {
			return err
		}
		st.LockAllow = allow
	}
	st.Lock = in.Enabled
	if st.Lock && st.LockUser == "" {
		return fmt.Errorf("%w: choose a username for the site lock", ErrInvalidInput)
	}
	if st.Lock && st.LockHash == "" {
		return fmt.Errorf("%w: choose a password for the site lock (at least %d characters)", ErrInvalidInput, minLockPassword)
	}
	return nil
}

func checkLockPassword(p string) error {
	if !utf8.ValidString(p) || utf8.RuneCountInString(p) < minLockPassword {
		return fmt.Errorf("%w: the password needs at least %d characters", ErrInvalidInput, minLockPassword)
	}
	if len(p) > maxLockPassword {
		return fmt.Errorf("%w: the password can be at most %d characters", ErrInvalidInput, maxLockPassword)
	}
	if strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("%w: the password can't contain control characters", ErrInvalidInput)
	}
	return nil
}

// checkImportedLock checks a lock that came with a site from another
// server, as if typed here.
func checkImportedLock(st *store.Site) error {
	allow, err := normalizeNets("allow", st.LockAllow)
	if err == nil && (st.LockUser != "" || st.LockHash != "") &&
		(!proxy.LockUserRe.MatchString(st.LockUser) || !proxy.ValidLockHash(st.LockHash)) {
		err = fmt.Errorf("%w: the site lock's username or password is not valid here", ErrInvalidInput)
	}
	if err != nil {
		if st.Lock {
			// Never open a site that was locked.
			return fmt.Errorf("the site lock didn't carry over: %w", err)
		}
		st.LockUser, st.LockHash, allow = "", "", nil
	}
	st.LockAllow = allow
	if st.Lock && st.LockHash == "" {
		return fmt.Errorf("%w: the site is locked but its password didn't carry over", ErrInvalidInput)
	}
	return nil
}

// proxyLock is a site's lock for Caddy (nil: open).
func proxyLock(st *store.Site) *proxy.Lock {
	if !st.Lock || st.LockUser == "" || st.LockHash == "" {
		return nil
	}
	return &proxy.Lock{User: st.LockUser, Hash: st.LockHash, Allow: st.LockAllow}
}

// Redirects.

const (
	// MaxRedirects bounds a site's redirects (Caddy tries them in turn).
	MaxRedirects = 100
	maxFromLen   = 512
	maxToLen     = 2048
)

// reservedPaths are WPGenie's on every site's domains (sign-in links, the
// shield's challenge) and Let's Encrypt's.
var reservedPaths = []string{"/_wpgenie", "/_shield", "/.well-known/acme-challenge"}

// Redirects returns a site's redirects, in the order they were made.
func (s *Service) Redirects(ctx context.Context, id string) ([]store.Redirect, error) {
	return s.Store.SiteRedirects(ctx, id)
}

// SetRedirects replaces a site's redirects.
func (s *Service) SetRedirects(ctx context.Context, id string, rules []store.Redirect) ([]store.Redirect, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	out, err := NormalizeRedirects(rules, siteHosts(st))
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetSiteRedirects(ctx, id, out); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	s.event(id, "redirects", fmt.Sprintf("Redirects changed (%d)", len(out)))
	return out, nil
}

// siteHosts are the domains a site answers on (served or redirecting).
func siteHosts(st *store.Site) []string {
	return slices.Concat([]string{st.PrimaryDomain}, st.Domains, st.RedirectDomains)
}

// NormalizeRedirects checks a site's redirects as typed and returns them as
// stored: From an exact path ("/old-page"; a trailing slash dropped) or a
// prefix ("/old/*"), decoded; To an https URL or a path of the site,
// %-encoded where needed; Code 301 when not given. hosts are the site's
// domains (a From may be pasted as a full address on one of them, and a
// To on one of them may come back to a redirect: no circles).
func NormalizeRedirects(in []store.Redirect, hosts []string) ([]store.Redirect, error) {
	if len(in) > MaxRedirects {
		return nil, fmt.Errorf("%w: a site can have at most %d redirects", ErrInvalidInput, MaxRedirects)
	}
	out := make([]store.Redirect, 0, len(in))
	seen := map[string]int{}
	for i, r := range in {
		bad := func(err error) error {
			return fmt.Errorf("%w: rule %d (%s): %v", ErrInvalidInput, i+1, strings.TrimSpace(r.From), err)
		}
		from, err := normalizeFrom(r.From, hosts)
		if err != nil {
			return nil, bad(err)
		}
		to, err := normalizeTo(r.To)
		if err != nil {
			return nil, bad(err)
		}
		code := r.Code
		if code == 0 {
			code = 301
		}
		if !slices.Contains(proxy.RedirectCodes, code) {
			return nil, bad(errors.New("the status must be 301, 302, 307 or 308"))
		}
		if r.KeepQuery && strings.Contains(to, "?") {
			return nil, bad(errors.New("the address already has a query string (?…): leave it out, or don't pass the visitor's on"))
		}
		key := strings.ToLower(from)
		if j, dup := seen[key]; dup {
			return nil, bad(fmt.Errorf("rule %d already redirects %s", j+1, from))
		}
		seen[key] = i
		out = append(out, store.Redirect{From: from, To: to, Code: code, KeepQuery: r.KeepQuery})
	}
	if err := checkRedirectLoops(out, hosts); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeFrom checks a path to redirect from.
func normalizeFrom(raw string, hosts []string) (string, error) {
	f := strings.TrimSpace(raw)
	if f == "" {
		return "", errors.New("the path to redirect from is empty")
	}
	if l := strings.ToLower(f); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
		// Pasted from the browser: a page of this site.
		u, err := url.Parse(f)
		if err != nil || !slices.Contains(hosts, strings.ToLower(u.Hostname())) || u.User != nil {
			return "", errors.New("redirect from a path of this site, like /old-page")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return "", errors.New("redirects match the path only: leave out the ? or # part")
		}
		if f = u.EscapedPath(); f == "" {
			f = "/"
		}
	}
	if strings.ContainsAny(f, "?#") {
		return "", errors.New("redirects match the path only: leave out the ? or # part")
	}
	if !strings.HasPrefix(f, "/") {
		return "", errors.New("the path to redirect from starts with /, like /old-page")
	}
	base, prefix := strings.CutSuffix(f, "/*")
	if strings.Contains(base, "*") {
		return "", errors.New("* only goes at the end, after a / (like /old/*)")
	}
	// As Caddy matches: the decoded path.
	p, err := url.PathUnescape(base)
	if err != nil {
		return "", errors.New("the path has a broken %-escape")
	}
	if prefix && p == "" {
		return "/*", nil // the whole site
	}
	if !prefix && len(p) > 1 {
		p = strings.TrimSuffix(p, "/") // "/old-page/" is "/old-page", which matches both
	}
	for _, r := range p {
		if !(r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/-._~!$&'()+,;=:@", r)) ||
			r >= 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r))) {
			if unicode.IsSpace(r) {
				return "", errors.New("the path can't contain spaces")
			}
			return "", fmt.Errorf("the path can't contain %q", r)
		}
	}
	if c := path.Clean(p); c != p {
		return "", fmt.Errorf("write the path as %s (no empty, . or .. parts)", c)
	}
	l := strings.ToLower(p)
	for _, r := range reservedPaths {
		if l == r || strings.HasPrefix(l, r+"/") {
			return "", fmt.Errorf("%s is WPGenie's and can't be redirected", r)
		}
	}
	if prefix {
		p += "/*"
	}
	if len(p) > maxFromLen {
		return "", fmt.Errorf("the path is longer than %d characters", maxFromLen)
	}
	if !proxy.ValidRedirectFrom(p) {
		return "", fmt.Errorf("%s can't be redirected", p)
	}
	return p, nil
}

// normalizeTo checks where a redirect sends visitors.
func normalizeTo(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", errors.New("where to send visitors is empty")
	}
	if len(t) > maxToLen {
		return "", fmt.Errorf("the address is longer than %d characters", maxToLen)
	}
	for _, r := range t {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("the address can't contain spaces")
		}
		if strings.ContainsRune("\"`\\{}<>^|#", r) {
			return "", fmt.Errorf("the address can't contain %q", r)
		}
	}
	l := strings.ToLower(t)
	var out string
	switch {
	case strings.HasPrefix(l, "https://"):
		u, err := url.Parse(t)
		if err != nil {
			return "", errors.New("that is not a web address")
		}
		if u.User != nil {
			return "", errors.New("the address can't hold a username or password")
		}
		host, err := domain.Normalize(u.Hostname())
		if err != nil {
			return "", fmt.Errorf("%q is not a domain name", u.Hostname())
		}
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
				return "", fmt.Errorf("%q is not a port", p)
			}
			host += ":" + p
		}
		out = "https://" + host + encodeExtra(u.EscapedPath()) + encodeQuery(u.RawQuery)
	case strings.HasPrefix(l, "http://"):
		return "", errors.New("use an https:// address")
	case strings.HasPrefix(t, "//"):
		return "", errors.New("start with / for a page of this site, or with https://")
	case strings.HasPrefix(t, "/"):
		u, err := url.Parse(t)
		if err != nil || u.Scheme != "" || u.Host != "" {
			return "", errors.New("that is not a path of this site")
		}
		out = encodeExtra(u.EscapedPath()) + encodeQuery(u.RawQuery)
	default:
		return "", errors.New("start with / for a page of this site, or with https://")
	}
	if !validEscapes(out) {
		return "", errors.New("the address has a broken %-escape")
	}
	if !proxy.ValidRedirectTo(out) {
		return "", errors.New("that address can't be used")
	}
	return out, nil
}

// encodeExtra %-encodes what Go leaves in an escaped path but a redirect's
// target doesn't take ([ and ]).
func encodeExtra(p string) string {
	return strings.NewReplacer("[", "%5B", "]", "%5D").Replace(p)
}

// encodeQuery is a query string for a redirect's target: "?" and the
// query, letters beyond ASCII and [ ] %-encoded ("" when empty).
func encodeQuery(q string) string {
	if q == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('?')
	for i := 0; i < len(q); i++ {
		if c := q[i]; c >= 0x80 || c == '[' || c == ']' {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// validEscapes: every % starts a %-escape.
func validEscapes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// redirectTarget is the path a redirect sends visitors to when it stays
// on the site (one of hosts), decoded; false when it leaves.
func redirectTarget(to string, hosts []string) (string, bool) {
	u, err := url.Parse(to)
	if err != nil {
		return "", false
	}
	if u.Host != "" && !slices.Contains(hosts, strings.ToLower(u.Hostname())) {
		return "", false
	}
	if u.Path == "" {
		return "/", true
	}
	return u.Path, true
}

// checkRedirectLoops refuses redirects that send visitors round in circles
// on the site.
func checkRedirectLoops(rules []store.Redirect, hosts []string) error {
	pr := proxyRedirects(rules)
	for i, r := range rules {
		seen, prev := map[int]bool{i: true}, i
		next, ok := redirectTarget(r.To, hosts)
		for ok {
			j := proxy.MatchRedirect(pr, next)
			if j < 0 {
				break
			}
			if seen[j] {
				if j == prev {
					return fmt.Errorf("%w: rule %d (%s) sends visitors to a page it redirects again, round in circles",
						ErrInvalidInput, j+1, rules[j].From)
				}
				a, b := min(j, prev), max(j, prev)
				return fmt.Errorf("%w: rules %d (%s) and %d (%s) send visitors round in circles",
					ErrInvalidInput, a+1, rules[a].From, b+1, rules[b].From)
			}
			seen[j], prev = true, j
			next, ok = redirectTarget(rules[j].To, hosts)
		}
	}
	return nil
}

// proxyRedirects are a site's redirects for Caddy.
func proxyRedirects(rules []store.Redirect) []proxy.PathRedirect {
	out := make([]proxy.PathRedirect, len(rules))
	for i, r := range rules {
		out[i] = proxy.PathRedirect{From: r.From, To: r.To, Code: r.Code, KeepQuery: r.KeepQuery}
	}
	return out
}

// RedirectMatch is what a site's redirects do with a request.
type RedirectMatch struct {
	// Path is the path as the rules see it (decoded); Query the query
	// string.
	Path  string `json:"path"`
	Query string `json:"query"`
	// Rule is the index of the redirect that answers (-1: none, WordPress
	// does); Redirect is that rule, and Location where it sends the visitor.
	Rule     int             `json:"rule"`
	Redirect *store.Redirect `json:"redirect,omitempty"`
	Location string          `json:"location,omitempty"`
}

// TestRedirect says which of a site's redirects answers a path (an address
// pasted from the browser works too), as Caddy decides it.
func (s *Service) TestRedirect(ctx context.Context, id, raw string) (*RedirectMatch, error) {
	rules, err := s.Store.SiteRedirects(ctx, id)
	if err != nil {
		return nil, err
	}
	return MatchRedirect(rules, raw)
}

// MatchRedirect is TestRedirect for a list of rules.
func MatchRedirect(rules []store.Redirect, raw string) (*RedirectMatch, error) {
	t := strings.TrimSpace(raw)
	if len(t) > maxToLen {
		return nil, fmt.Errorf("%w: the path is longer than %d characters", ErrInvalidInput, maxToLen)
	}
	if l := strings.ToLower(t); !strings.HasPrefix(l, "https://") && !strings.HasPrefix(l, "http://") && !strings.HasPrefix(t, "/") {
		t = "/" + t
	}
	u, err := url.Parse(t)
	if err != nil {
		return nil, fmt.Errorf("%w: that is not a path or an address", ErrInvalidInput)
	}
	m := &RedirectMatch{Path: u.Path, Query: u.RawQuery, Rule: -1}
	if m.Path == "" {
		m.Path = "/"
	}
	pr := proxyRedirects(rules)
	if i := proxy.MatchRedirect(pr, m.Path); i >= 0 {
		r := rules[i]
		m.Rule, m.Redirect, m.Location = i, &r, proxy.RedirectLocation(pr[i], m.Query)
	}
	return m, nil
}
