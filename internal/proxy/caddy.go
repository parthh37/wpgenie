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
}

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
}

type Caddy struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex // serialise reloads so the last writer always wins

	// wafOK caches whether the running Caddy has the Coraza module: a
	// config using it would otherwise be rejected, taking every site's
	// changes down with it. wafChecked is when a "no" was last seen.
	wafOK      atomic.Bool
	wafChecked atomic.Int64
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
		if len(s.Upstreams) == 0 && s.Proxy == "" {
			return nil, fmt.Errorf("site %s: no PHP-FPM upstreams", s.ID)
		}
		for _, u := range append(slices.Clone(s.Upstreams), s.Proxy) {
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
		switch {
		case s.BodyWAF == "" || s.BodyWAF == WAFOff || s.Proxy != "":
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
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, map[string]any{
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
		"Sites":          sites,
		"WAF":            useWAF,
		"WAFDirectives":  directives,
	})
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
