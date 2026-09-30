// Package proxy renders the Caddy configuration for all sites and hot-loads
// it through Caddy's admin API. Caddy obtains and renews TLS certificates on
// its own for every hostname that appears in a site block.
package proxy

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"
	"unicode"

	"github.com/parthh37/wpgenie/internal/shield"
)

type Site struct {
	ID      string
	Name    string
	Domains []string
	Root    string
	// Upstreams are the PHP-FPM replicas ("host:port"). Addresses rather
	// than ports so replicas on other nodes can be added later.
	Upstreams     []string
	ShieldEnabled bool
	BlockXMLRPC   bool
	PageCache     bool
	// BodyWAF inspects request bodies with Coraza and the OWASP CRS.
	BodyWAF WAFMode
	// Proxy, if set, makes this a plain reverse-proxied app (webmail)
	// instead of a WordPress site: no docroot, PHP or page cache.
	Proxy string
	// Redirects answer with a permanent redirect to Domains[0] (the
	// primary domain), keeping the path.
	Redirects []string
	// CustomCert: serve the site's own certificate (Config.CertDir/<ID>/
	// cert.pem and key.pem) instead of obtaining one.
	CustomCert bool
	// Staging sites ask search engines not to index them.
	Staging bool
	// EdgeHTML lets a CDN keep pages served from the page cache (Cloudflare
	// Cache Rules that only cache what the origin marks cacheable): those
	// responses get s-maxage, every other response stays uncacheable.
	EdgeHTML bool
	// Images are the formats served instead of a JPEG/PNG upload to browsers
	// that accept them, when a converted copy (<file>.avif, <file>.webp)
	// exists: "avif" and/or "webp".
	Images []string
	// AssetCDN: a pull-zone CDN fetches the site's static files for its own
	// hostname. Fonts then need CORS, and formats aren't negotiated: the CDN
	// would cache one format for every browser.
	AssetCDN bool
	// Offload is the public URL of the object storage holding the site's
	// uploads (https://host/path; /wp-content/uploads maps onto the path),
	// or "". Uploads missing on disk are fetched from there (offload.go).
	Offload string
	// Suspended sites (their account is suspended) get a static 503 page on
	// every domain (redirect domains included): no PHP, no shield, no
	// access log. Their files and database are untouched.
	Suspended bool

	// Multi-server. HomeUpstreams are the replicas on the site's own server
	// when others also run on other servers (Upstreams has all of them):
	// anything that may write files goes only there. Forwarded: the site's
	// old server passes visitors on (a move), so it is also served on the
	// loopback ingress listener (Config.IngressListen).
	HomeUpstreams []string
	Forwarded     bool
	// Forward, set for a site that moved away from this server: its domains
	// go to this local tunnel port (the new server's ingress), and
	// ForwardHTTP (its port 80) gets Let's Encrypt's HTTP challenges.
	Forward     string
	ForwardHTTP string
}

// siteView is a site as the template renders it: the site, the shared
// settings (Cfg), and the address line of its block.
type siteView struct {
	Site
	Cfg         map[string]any
	Address     string
	HTTPAddress string
	// Ingress: this is the loopback copy for forwarded visitors.
	Ingress     bool
	IngressView *siteView
	// OffloadTo is the parsed Offload URL (nil: uploads only on disk).
	OffloadTo *offloadTarget
}

// CacheBypassCookies are the cookies (name prefixes) that mean the visitor
// may see a personalised page: a stored page is never served to them.
// images/php/page-cache.php refuses to store pages for them (and for any
// cookie not known to be harmless); the Cloudflare edge rule reuses them.
var CacheBypassCookies = []string{
	"wordpress_logged_in_", "wordpress_sec_", "wp-postpass_", "comment_author_", "woocommerce_items_in_cart",
	"woocommerce_cart_hash", "wp_woocommerce_session_", "edd_items_in_cart", "PHPSESSID",
}

// MobileUA is what makes a request "mobile" when it has no Sec-CH-UA-Mobile
// client hint: WordPress's wp_is_mobile() rule. The page cache keeps
// separate mobile and desktop copies of pages whose HTML depends on it, and
// images/php/page-cache.php applies exactly this rule when storing them.
const MobileUA = "Mobile|Android|Silk/|Kindle|BlackBerry|Opera Mini|Opera Mobi"

// mobileExpr is the same rule as a Caddy CEL expression. The client hint
// decides whenever it is sent; the User-Agent only without it.
const mobileExpr = `({header.Sec-CH-UA-Mobile} == "?1" || ({header.Sec-CH-UA-Mobile} == "" && {header.User-Agent}.matches("` +
	MobileUA + `")))`

// EdgeTTL is how long a CDN may keep a cached page (s-maxage). Pages are at
// most 10 h old when served from the page cache: with this, no copy anywhere
// outlives WordPress's 12 h nonce tick.
const EdgeTTL = 3600

// imageFormats are the formats Caddy can serve instead of an upload, best
// first.
var imageFormats = []string{"avif", "webp"}

// WAFMode is a site's request-body inspection: "off", "detect" (matches are
// logged, nothing is blocked) or "block".
type WAFMode string

const (
	WAFOff    WAFMode = "off"
	WAFDetect WAFMode = "detect"
	WAFBlock  WAFMode = "block"
)

// Engine is Coraza's SecRuleEngine value for the mode.
func (m WAFMode) Engine() string {
	switch m {
	case WAFDetect:
		return "DetectionOnly"
	case WAFBlock:
		return "On"
	}
	return ""
}

type Config struct {
	ACMEEmail      string
	AdminURL       string // e.g. http://127.0.0.1:2019
	PanelDomain    string
	PanelUpstream  string // wpgenie listen address
	ShieldUpstream string
	AccessLog      string
	CaddyfilePath  string
	// CloudflareRanges returns the networks whose CF-Connecting-IP header
	// Caddy believes (nil: none). Behind Cloudflare every connection comes
	// from its edge; without this the shield would rate-limit and ban
	// Cloudflare instead of the visitor.
	CloudflareRanges func() []netip.Prefix
	// WAFLog is the Coraza audit log, a path valid inside the Caddy
	// container (default: waf.log next to the access log).
	WAFLog string
	// CertDir holds sites' own certificates, a path valid inside the Caddy
	// container (default: certs next to the Caddyfile, as mounted there).
	CertDir string
	// IngressListen is the loopback listener (host:port) for visitors another
	// server of the cluster passes on, PROXY protocol only from loopback;
	// "" turns it off.
	IngressListen string
}

// defaultLogKeep is how many rotated access logs Caddy keeps unless log
// shipping says otherwise (see SetAccessLogKeep).
const defaultLogKeep = 10

type Caddy struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex // serialise reloads so the last writer always wins

	// wafOK caches whether the running Caddy has the Coraza module: a
	// config using it would otherwise be rejected, taking every site's
	// changes down with it. wafChecked is when a "no" was last seen.
	wafOK      atomic.Bool
	wafChecked atomic.Int64
	// logKeep, if set, is how many rotated access log files to keep (0:
	// defaultLogKeep): fewer once they're shipped elsewhere.
	logKeep atomic.Pointer[func() int]
}

// SetAccessLogKeep makes the number of rotated access log files Caddy keeps
// come from keep (log shipping; 0 or nil: the default). It applies from the
// next render (Sync).
func (c *Caddy) SetAccessLogKeep(keep func() int) {
	if keep == nil {
		c.logKeep.Store(nil)
		return
	}
	c.logKeep.Store(&keep)
}

func (c *Caddy) accessLogKeep() int {
	if f := c.logKeep.Load(); f != nil {
		if n := (*f)(); n > 0 && n <= 1000 {
			return n
		}
	}
	return defaultLogKeep
}

func NewCaddy(cfg Config) *Caddy {
	return &Caddy{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

//go:embed Caddyfile.tmpl
var caddyfileTmpl string

var (
	//go:embed waf/wpgenie.conf
	wafSettings string
	//go:embed waf/wordpress-rule-exclusions-config.conf
	wafWPConfig string
	//go:embed waf/wordpress-rule-exclusions-before.conf
	wafWPBefore string
)

// wafDirectives is the Coraza configuration shared by every site: the CRS
// setup, WPGenie's settings, the CRS WordPress exclusions (which must come
// before the rules they exclude), then the rules. Comments are dropped: the
// text goes inside a Caddyfile backtick string, which a backtick in a
// comment would end.
func wafDirectives(auditLog string) (string, error) {
	var b strings.Builder
	b.WriteString("Include @coraza.conf-recommended\nInclude @crs-setup.conf.example\n")
	for _, part := range []string{wafSettings, "SecAuditLog " + auditLog, wafWPConfig, wafWPBefore} {
		for _, line := range strings.Split(part, "\n") {
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
	}
	b.WriteString("Include @owasp_crs/*.conf\n")
	out := b.String()
	// Backticks would end the Caddyfile string; {$...} is Caddyfile
	// environment substitution.
	if strings.ContainsAny(out, "`") || strings.Contains(out, "{$") {
		return "", fmt.Errorf("WAF rules contain Caddyfile syntax")
	}
	return out, nil
}

var tmpl = template.Must(template.New("Caddyfile").
	Funcs(template.FuncMap{"join": strings.Join}).
	Parse(caddyfileTmpl))

// Render writes the Caddyfile; see RenderWAF.
func (c *Caddy) Render(sites []Site) ([]byte, error) { return c.RenderWAF(sites, true) }

// RenderWAF writes the Caddyfile. Without waf, sites are rendered without
// body inspection (the running Caddy lacks the module).
func (c *Caddy) RenderWAF(sites []Site, waf bool) ([]byte, error) {
	sites = slices.Clone(sites) // BodyWAF is adjusted below
	for _, s := range sites {
		if len(s.Upstreams) == 0 && s.Proxy == "" && s.Forward == "" && !s.Suspended {
			return nil, fmt.Errorf("site %s: no PHP-FPM upstreams", s.ID)
		}
		if s.Forward != "" && s.ForwardHTTP == "" {
			return nil, fmt.Errorf("site %s: forward without a challenge tunnel", s.ID)
		}
		for _, u := range slices.Concat(s.Upstreams, s.HomeUpstreams, []string{s.Proxy, s.Forward, s.ForwardHTTP}) {
			if strings.ContainsAny(u, " \t\n{}#\"") {
				return nil, fmt.Errorf("site %s: unsafe upstream %q", s.ID, u)
			}
		}
		for _, d := range append(slices.Clone(s.Domains), s.Redirects...) {
			if strings.ContainsAny(d, " \t\n{}#\"") {
				return nil, fmt.Errorf("site %s: unsafe domain %q", s.ID, d)
			}
		}
		if len(s.Domains) == 0 {
			return nil, fmt.Errorf("site %s: no domains", s.ID)
		}
		if strings.ContainsAny(s.ID, " \t\n{}#\"/.") {
			return nil, fmt.Errorf("unsafe site ID %q", s.ID)
		}
		for _, f := range s.Images {
			if !slices.Contains(imageFormats, f) {
				return nil, fmt.Errorf("site %s: unknown image format %q", s.ID, f)
			}
		}
		if s.Offload != "" {
			if _, err := parseOffload(s.Offload); err != nil {
				return nil, fmt.Errorf("site %s: %w", s.ID, err)
			}
		}
	}
	adminListen := "localhost:2019"
	if u, err := url.Parse(c.cfg.AdminURL); err == nil && u.Host != "" {
		adminListen = u.Host
	}
	email := c.cfg.ACMEEmail
	if email == "" {
		email = "admin@localhost" // Caddy still works; ACME just won't send expiry notices
	}
	useWAF := false
	for i, s := range sites {
		// Names only appear in comments; a line break would end the comment
		// and let the rest be parsed as config.
		sites[i].Name = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, s.Name)
		// Best format first (the first rewrite that matches wins); none behind
		// a pull-zone CDN, which caches whatever it fetched first for everyone.
		sites[i].Images = nil
		for _, f := range imageFormats {
			if slices.Contains(s.Images, f) && !s.AssetCDN && s.Proxy == "" {
				sites[i].Images = append(sites[i].Images, f)
			}
		}
		switch {
		case s.BodyWAF == "" || s.BodyWAF == WAFOff || s.Proxy != "" || s.Suspended:
			sites[i].BodyWAF = WAFOff
		case s.BodyWAF.Engine() == "":
			return nil, fmt.Errorf("site %s: unknown body WAF mode %q", s.ID, s.BodyWAF)
		case !waf:
			sites[i].BodyWAF = WAFOff
		default:
			useWAF = true
		}
	}
	wafLog := c.cfg.WAFLog
	if wafLog == "" {
		wafLog = filepath.Join(filepath.Dir(c.cfg.AccessLog), "waf.log")
	}
	var directives string
	if useWAF {
		if strings.ContainsAny(wafLog, " \t\n{}#\"`") {
			return nil, fmt.Errorf("unsafe WAF log path %q", wafLog)
		}
		var err error
		if directives, err = wafDirectives(wafLog); err != nil {
			return nil, err
		}
	}
	var trusted []string
	if c.cfg.CloudflareRanges != nil {
		for _, p := range c.cfg.CloudflareRanges() {
			trusted = append(trusted, p.Masked().String()) // netip output: no Caddyfile syntax
		}
	}
	certDir := c.cfg.CertDir
	if certDir == "" {
		certDir = "/etc/caddy/certs"
	}
	if strings.ContainsAny(certDir, " \t\n{}#\"`") {
		return nil, fmt.Errorf("unsafe certificate directory %q", certDir)
	}
	ingressHost, ingressPort := "", ""
	if c.cfg.IngressListen != "" {
		var err error
		if ingressHost, ingressPort, err = net.SplitHostPort(c.cfg.IngressListen); err != nil ||
			net.ParseIP(ingressHost) == nil || !net.ParseIP(ingressHost).IsLoopback() {
			return nil, fmt.Errorf("ingress listener %q must be a loopback host:port", c.cfg.IngressListen)
		}
	}
	root := map[string]any{
		"CertDir":        certDir,
		"TrustedProxies": trusted,
		"ACMEEmail":      email,
		"AdminListen":    adminListen,
		"PanelDomain":    c.cfg.PanelDomain,
		"PanelUpstream":  c.cfg.PanelUpstream,
		"ShieldUpstream": c.cfg.ShieldUpstream,
		"SiteHeader":     shield.SiteHeader,
		"VerdictHeader":  shield.VerdictHeader,
		"AccessLog":      c.cfg.AccessLog,
		"AccessLogKeep":  c.accessLogKeep(),
		"BypassCookies":  strings.Join(CacheBypassCookies, "|"),
		"MobileExpr":     mobileExpr,
		"EdgeTTL":        EdgeTTL,
		"WAF":            useWAF,
		"WAFDirectives":  directives,
		"SuspendedPage":  SuspendedPage,
		"IngressHost":    ingressHost,
	}
	views := make([]*siteView, len(sites))
	anyIngress := false
	for i, st := range sites {
		v := &siteView{Site: st, Cfg: root, Address: strings.Join(st.Domains, ", ")}
		if st.Offload != "" && st.Proxy == "" && st.Forward == "" {
			if t, err := parseOffload(st.Offload); err == nil { // checked above
				v.OffloadTo = &t
			}
		}
		if st.Forward != "" {
			v.HTTPAddress = "http://" + strings.Join(st.Domains, ", http://")
		}
		if st.Forwarded && ingressPort != "" && st.Proxy == "" && st.Forward == "" {
			iv := &siteView{Site: st, Cfg: root, Ingress: true,
				Address: "http://" + strings.Join(st.Domains, ":"+ingressPort+", http://") + ":" + ingressPort}
			iv.CustomCert = false
			iv.OffloadTo = v.OffloadTo
			v.IngressView = iv
			anyIngress = true
		}
		views[i] = v
	}
	root["Sites"] = views
	if anyIngress {
		root["IngressListen"] = ingressHost + ":" + ingressPort
	}
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, root)
	return buf.Bytes(), err
}

// Apply writes the Caddyfile (so Caddy boots with it after a restart) and
// loads it live. Caddy validates the whole config before switching over, so a
// bad render never takes existing sites down.
func (c *Caddy) Apply(ctx context.Context, sites []Site) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	waf := false
	for _, s := range sites {
		if s.BodyWAF == WAFDetect || s.BodyWAF == WAFBlock {
			waf = c.wafSupported(ctx)
			break
		}
	}
	body, err := c.RenderWAF(sites, waf)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.AdminURL, "/")+"/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("caddy load: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy rejected config (%d): %s", resp.StatusCode, msg)
	}
	// Persist only after Caddy accepted it, so the file on disk is always a
	// config that is known to load.
	return writeAtomic(c.cfg.CaddyfilePath, body)
}

// WAFAvailable reports whether the running Caddy can inspect request
// bodies, as last checked.
func (c *Caddy) WAFAvailable() bool { return c.wafOK.Load() }

// wafSupported asks Caddy to adapt (not load) a config using the Coraza
// directive. A "yes" is remembered; a "no" is re-checked at most every
// minute, so an upgraded Caddy is picked up without a daemon restart.
func (c *Caddy) wafSupported(ctx context.Context) bool {
	if c.wafOK.Load() {
		return true
	}
	if time.Since(time.Unix(0, c.wafChecked.Load())) < time.Minute {
		return false
	}
	c.wafChecked.Store(time.Now().UnixNano())
	// Inside route, as the real config uses it: coraza_waf has no place in
	// Caddy's directive order, so outside a route it never adapts.
	probe := ":1 {\n\troute {\n\t\tcoraza_waf {\n\t\t\tdirectives `SecRuleEngine Off`\n\t\t}\n\t}\n}\n"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.AdminURL, "/")+"/adapt",
		strings.NewReader(probe))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := c.client.Do(req)
	if err != nil {
		c.wafChecked.Store(0) // Caddy unreachable: ask again next time
		return false
	}
	resp.Body.Close()
	ok := resp.StatusCode == http.StatusOK
	c.wafOK.Store(ok)
	return ok
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
