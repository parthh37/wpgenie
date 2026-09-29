package shield

import (
	"path"
	"regexp"
	"strings"
)

// Threat is an attack pattern found by request inspection: a small,
// WordPress-tuned web application firewall working on what forward_auth
// sees (method, URI and headers; never the body). It targets what automated
// attacks against WordPress send in URLs and has almost no false positives
// on real sites, because post content and form data travel in bodies.
type Threat int

const (
	ThreatNone      Threat = iota
	ThreatTraversal        // ../ and system file reads
	ThreatSQLi
	ThreatXSS
	ThreatInjection   // PHP / shell / JNDI code injection
	ThreatProbe       // hunting for backups, secrets, other stacks, plugin versions
	ThreatEnumeration // listing WordPress user names for brute force
)

func (t Threat) String() string {
	return [...]string{"none", "path_traversal", "sql_injection", "xss", "code_injection", "probe", "user_enumeration"}[t]
}

// Request is the part of a request inspection looks at.
type Request struct {
	Method  string
	URI     string // raw request URI (X-Forwarded-Uri)
	UA      string
	Referer string
	Cookie  string
	// Browser: the client looks like a browser (ClassHuman), which relaxes
	// rules for things browsers legitimately do.
	Browser bool
}

var (
	traversalRe = regexp.MustCompile(`(?:^|[/\\=])\.\.[/\\]|/etc/(?:passwd|shadow|hosts)\b|/proc/self/|\bwin\.ini\b|\bboot\.ini\b|\x00`)
	sqliRe      = regexp.MustCompile(
		`\bunion\b[\s(]+(?:all\s+|distinct\s+)?select\b` +
			`|\binformation_schema\b|\bmysql\.user\b` +
			// sleep(5), benchmark(1000000,md5(1)); not "sleep (8 hours)".
			`|\b(?:sleep|benchmark|pg_sleep)\s*\(\s*\d+(?:\.\d+)?\s*[),]` +
			`|\bwaitfor\s+delay\b` +
			`|\bload_file\s*\(|\binto\s+(?:out|dump)file\b` +
			`|\b(?:extractvalue|updatexml)\s*\(` +
			`|'\s*(?:or|and)\s+'?\w+'?\s*(?:=|like)\s*'?\w+` +
			`|;\s*(?:drop|truncate|alter)\s+table\b`)
	xssRe = regexp.MustCompile(
		`<\s*/?\s*script\b` +
			// A javascript: URL in a URL-valued parameter (redirect_to, url,
			// return, …), or anywhere when code follows at once; "javascript:
			// the good parts" in a search box is neither.
			`|(?:^|&)[\w\[\]-]*(?:url|uri|redirect|return|next|goto|link|href|src|dest|callback|referer)[\w\[\]-]*=\s*(?:javascript|vbscript)\s*:` +
			`|(?:^|[=&])\s*(?:javascript|vbscript):[^\s]` +
			`|<\s*(?:iframe|svg|img|body|object|embed|details|video|audio|math)\b[^>]*\bon[a-z]+\s*=` +
			`|\bdocument\s*\.\s*(?:cookie|domain)\b|\bstring\.fromcharcode\s*\(`)
	injectionRe = regexp.MustCompile(
		`<\?php|\b(?:php|phar|expect|zip|glob|data)://` +
			// No space before "(" and something inside: payloads never need
			// the space, and "solar system (2020)" or "system() call" are searches.
			`|\b(?:base64_decode|eval|assert|system|passthru|shell_exec|proc_open|popen|create_function)\(\s*[^\s)]` +
			`|\$\{\s*jndi\s*:|\(\s*\)\s*\{\s*:\s*;\s*\}` + // log4shell, shellshock
			`|[;|&` + "`" + `]\s*(?:wget|curl|nc|bash|sh)\s+`)
	// Paths that only scanners request: secrets, backups, admin tools of
	// other stacks, shells, and plugin readme.txt (how scanners learn a
	// plugin's exact version to pick an exploit).
	probeRe = regexp.MustCompile(
		`/\.(?:env|git|svn|hg|aws|ssh|docker|vscode|idea|htpasswd|ds_store)(?:/|$|\.)` +
			`|/wp-config[^/]*$` + // the file itself, its backups and the sample
			`|\.(?:sql|sql\.gz|bak|old|orig|save|swp|asp|aspx|jsp|cgi|action)$|~$` +
			`|^/(?:phpmyadmin|pma|myadmin|adminer|mysqladmin|dbadmin|cgi-bin|boaform|actuator|hnap1|\.well-known/.+\.php)` +
			`|/(?:phpinfo|info|shell|c99|r57|wso|alfa|adminer|eval-stdin)\.php$` +
			`|/vendor/phpunit/|/wp-admin/(?:install|setup-config)\.php$` +
			`|^/wp-content/(?:plugins|themes)/[^/]+/(?:readme|changelog)\.txt$`)
	authorQueryRe = regexp.MustCompile(`(?:^|&)author=\d`)
)

// Inspect looks for attack patterns in a request.
func Inspect(r Request) Threat {
	rawPath, rawQuery, _ := strings.Cut(r.URI, "?")
	// Traversal is checked before cleaning, which would erase it; every
	// other path rule sees the path as it reaches PHP.
	dp := strings.ToLower(decodeFully(rawPath, false))
	p := cleanPath(dp)
	q := strings.ToLower(decodeFully(rawQuery, true))
	qs := sqlSpaces(q)
	headers := strings.ToLower(r.UA + "\n" + r.Referer)

	switch {
	case traversalRe.MatchString(dp), traversalRe.MatchString(q):
		return ThreatTraversal
	case injectionRe.MatchString(q), injectionRe.MatchString(headers):
		return ThreatInjection
	case sqliRe.MatchString(qs):
		return ThreatSQLi
	case xssRe.MatchString(q):
		return ThreatXSS
	case probeRe.MatchString(p):
		return ThreatProbe
	}
	if !strings.Contains(r.Cookie, "wordpress_logged_in_") {
		// Anonymous user enumeration: the REST users route lists logins. On
		// sites with plain permalinks ?author=N is the normal author link,
		// so browsers may follow it; tools may not (it redirects to
		// /author/<login>/, which is how they harvest user names).
		if strings.HasPrefix(p, "/wp-json/wp/v2/users") || strings.Contains(q, "rest_route=/wp/v2/users") ||
			(!r.Browser && authorQueryRe.MatchString(q)) {
			return ThreatEnumeration
		}
	}
	return ThreatNone
}

// decodeFully percent-decodes up to three times, so double-encoded payloads
// (%253Cscript) are seen as the application will eventually see them.
// Like PHP it is lenient: invalid escapes stay as they are rather than
// making the whole string undecodable (a stray "&x=%" must not hide a
// payload elsewhere in the query). In queries "+" is a space.
func decodeFully(s string, query bool) string {
	for range 3 {
		d := decodeLenient(s, query)
		if d == s {
			return s
		}
		s = d
	}
	return s
}

func decodeLenient(s string, query bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
		case c == '+' && query:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	}
	return c - 'a' + 10
}

// cleanPath resolves a decoded request path the way Caddy does before
// choosing the file PHP runs: dot segments and duplicate slashes removed.
func cleanPath(decoded string) string { return path.Clean("/" + decoded) }

var (
	// MySQL runs the body of /*!50000 ... */ comments; other comments, "+"
	// and every control character (RE2's \s lacks \v) are whitespace.
	sqlVersionedRe = regexp.MustCompile(`/\*!\d*(.*?)\*/`)
	sqlCommentRe   = regexp.MustCompile(`/\*.*?\*/|\+|[\x00-\x1f]`)
)

func sqlSpaces(s string) string {
	return sqlCommentRe.ReplaceAllString(sqlVersionedRe.ReplaceAllString(s, " ${1} "), " ")
}

// ScriptPath is the PHP script a URI executes. Caddy's php_fastcgi splits
// the path at the first ".php", so /wp-login.php/x runs wp-login.php (with
// PATH_INFO /x): every check on "is this the login page" must use this form,
// or appending a suffix bypasses it.
func ScriptPath(uri string) string {
	rawPath, _, _ := strings.Cut(uri, "?")
	p := cleanPath(decodeFully(rawPath, false))
	if i := strings.Index(strings.ToLower(p), ".php"); i >= 0 {
		return p[:i+4]
	}
	return p
}

// isAdminPath reports whether a script belongs to the WordPress admin that
// an admin IP allowlist protects. admin-ajax.php and admin-post.php stay
// public: front-end forms, carts and search plugins post to them.
func isAdminPath(script string) bool {
	switch {
	case strings.HasSuffix(script, "/wp-login.php"):
		return true
	case strings.HasSuffix(script, "/wp-admin/admin-ajax.php"), strings.HasSuffix(script, "/wp-admin/admin-post.php"):
		return false
	}
	return strings.HasPrefix(script, "/wp-admin/") || script == "/wp-admin"
}
