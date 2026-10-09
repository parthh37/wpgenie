package proxy

import (
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Rules at a site's edge, rendered into its block (see Caddyfile.tmpl):
//
//   - the site lock: Caddy's basic_auth asks every visitor for a username
//     and password, except for the panel's own paths (/_wpgenie/*, routed
//     before it), Let's Encrypt's challenges and the allowed networks;
//   - redirects from paths of the site, before cached pages and PHP.
//
// internal/site checks what people type; the checks here only make sure
// nothing can change the Caddyfile's structure, whoever calls Render.

// Lock is a site's lock. Hash is a bcrypt hash (what basic_auth takes);
// Allow are networks (CIDR prefixes) that skip it.
type Lock struct {
	User  string
	Hash  string
	Allow []string
}

// PathRedirect sends requests for a path of a site to To with Code. From is
// an exact path ("/old-page", which also matches "/old-page/") or a prefix
// ending in "/*" ("/old/*" matches "/old" and everything under it). Paths
// match as Caddy's path matcher does: case-insensitively, on the decoded
// and cleaned path (MatchRedirect). KeepQuery appends the visitor's query
// string to To.
type PathRedirect struct {
	From      string
	To        string
	Code      int
	KeepQuery bool
}

var (
	// LockUserRe is what a lock's username may be: a single Caddyfile token
	// that can't be taken for anything else, and without the colon that
	// ends a username in basic authentication.
	LockUserRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)
	// bcryptRe is a bcrypt hash in modular crypt format, as Go's bcrypt
	// writes it ($2a$) and as Caddy reads it without base64.
	bcryptRe = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)
	// redirectToRe is a redirect's target after internal/site normalised it:
	// ASCII only, none of whitespace, quotes, braces (placeholders),
	// backslashes or #.
	redirectToRe = regexp.MustCompile(`^(?:https://[a-z0-9.-]+(?::[0-9]{1,5})?(?:[/?]` + urlChars + `*)?|/(?:` + urlNoSlash + urlChars + `*)?)$`)
)

// urlChars: what a redirect's target is made of after the host, as a
// regexp class (RFC 3986's path and query characters, # excepted).
const (
	urlChars   = `[A-Za-z0-9\-._~!$&'()*+,;=:@%/?]`
	urlNoSlash = `[A-Za-z0-9\-._~!$&'()*+,;=:@%?]` // after a relative target's first /: never //
)

// RedirectCodes are the status codes a redirect may answer with.
var RedirectCodes = []int{301, 302, 307, 308}

// ValidRedirectFrom reports whether p can be a redirect's From: a clean
// path (no empty, "." or ".." segments, no trailing slash but "/"), of
// letters, digits and the URL punctuation that has no meaning to Caddy's
// path matcher or the Caddyfile, optionally ending in "/*".
func ValidRedirectFrom(p string) bool {
	base, prefix := strings.CutSuffix(p, "/*")
	if prefix && base == "" {
		return true // everything
	}
	if !strings.HasPrefix(base, "/") || (prefix && base == "/") || len(p) > 1024 {
		return false
	}
	if base != "/" && (strings.HasSuffix(base, "/") || path.Clean(base) != base) {
		return false
	}
	for _, r := range base {
		if !pathRune(r) {
			return false
		}
	}
	return true
}

// pathRune: a character a redirect's From may hold. Not *, ?, [ ] or \
// (glob syntax to Caddy's path matcher), % (makes it match the escaped
// path), { } (placeholders), quotes, #, whitespace or controls.
func pathRune(r rune) bool {
	switch {
	case r < 0x80:
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("/-._~!$&'()+,;=:@", r)
	default:
		return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
	}
}

// redirectView is a redirect as the template renders it.
type redirectView struct {
	Paths    []string // the path matcher's patterns
	Location string
	Code     int
}

// redirectPatterns are the path matcher patterns of a From.
func redirectPatterns(from string) []string {
	if base, ok := strings.CutSuffix(from, "/*"); ok {
		if base == "" {
			return []string{"/*"}
		}
		return []string{base, base + "/*"}
	}
	if from == "/" {
		return []string{"/"}
	}
	return []string{from, from + "/"}
}

// redirectOrder is the order Caddy tries a site's redirects in (the first
// that matches answers): exact paths first, then prefixes, longest first,
// so the most specific rule wins whatever order they were made in. It
// returns indices into rs.
func redirectOrder(rs []PathRedirect) []int {
	idx := make([]int, len(rs))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		pa, pb := strings.HasSuffix(rs[a].From, "/*"), strings.HasSuffix(rs[b].From, "/*")
		switch {
		case pa != pb:
			if pa {
				return 1
			}
			return -1
		case pa:
			return len(rs[b].From) - len(rs[a].From)
		}
		return 0
	})
	return idx
}

// MatchRedirect returns the index in rs of the redirect Caddy answers a
// request for reqPath (the decoded path, as in URL.Path) with, or -1. It
// mirrors Caddy's path matcher: lower case, path.Clean keeping a trailing
// slash; patterns ending in * match by prefix, others exactly.
func MatchRedirect(rs []PathRedirect, reqPath string) int {
	p := strings.ToLower(reqPath)
	if p == "" {
		p = "/"
	}
	clean := path.Clean(p)
	if clean != "/" && strings.HasSuffix(p, "/") {
		clean += "/"
	}
	for _, i := range redirectOrder(rs) {
		for _, pat := range redirectPatterns(strings.ToLower(rs[i].From)) {
			if pre, ok := strings.CutSuffix(pat, "*"); ok {
				if strings.HasPrefix(clean, pre) {
					return i
				}
			} else if clean == pat {
				return i
			}
		}
	}
	return -1
}

// RedirectLocation is where a redirect sends a request with the raw query
// string query (Caddy's {?query}: "?" and the query, when there is one).
func RedirectLocation(r PathRedirect, query string) string {
	if r.KeepQuery && query != "" {
		return r.To + "?" + query
	}
	return r.To
}

// ValidLockHash reports whether h is a bcrypt hash basic_auth can take.
func ValidLockHash(h string) bool { return bcryptRe.MatchString(h) }

// ValidRedirectTo reports whether to can be a redirect's target: an https
// URL or a path of the site ("/..." but never "//..."), ASCII, with none
// of whitespace, quotes, braces, backslashes or #.
func ValidRedirectTo(to string) bool {
	return len(to) <= 4096 && redirectToRe.MatchString(to)
}

// checkEdge refuses edge rules that could change the Caddyfile's
// structure.
func checkEdge(s Site) error {
	if l := s.Lock; l != nil {
		if !LockUserRe.MatchString(l.User) {
			return fmt.Errorf("site %s: unsafe lock username %q", s.ID, l.User)
		}
		if !ValidLockHash(l.Hash) {
			return fmt.Errorf("site %s: the lock's password hash is not a bcrypt hash", s.ID)
		}
		for _, a := range l.Allow {
			if p, err := netip.ParsePrefix(a); err != nil || p.Masked().String() != a {
				return fmt.Errorf("site %s: unsafe lock network %q", s.ID, a)
			}
		}
	}
	for _, r := range s.PathRedirects {
		if !ValidRedirectFrom(r.From) {
			return fmt.Errorf("site %s: unsafe redirect path %q", s.ID, r.From)
		}
		if !ValidRedirectTo(r.To) {
			return fmt.Errorf("site %s: unsafe redirect target %q", s.ID, r.To)
		}
		if !slices.Contains(RedirectCodes, r.Code) {
			return fmt.Errorf("site %s: redirect status %d", s.ID, r.Code)
		}
	}
	return nil
}

// redirectViews are a site's redirects in the order Caddy must try them.
func redirectViews(rs []PathRedirect) []redirectView {
	var out []redirectView
	for _, i := range redirectOrder(rs) {
		r := rs[i]
		loc := r.To
		if r.KeepQuery {
			loc += "{?query}"
		}
		out = append(out, redirectView{Paths: redirectPatterns(r.From), Location: loc, Code: r.Code})
	}
	return out
}
