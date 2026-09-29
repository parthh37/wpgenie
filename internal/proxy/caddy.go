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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

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
}

type Config struct {
	ACMEEmail      string
	AdminURL       string // e.g. http://127.0.0.1:2019
	PanelDomain    string
	PanelUpstream  string // wpgenie listen address
	ShieldUpstream string
	AccessLog      string
	CaddyfilePath  string
}

type Caddy struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex // serialise reloads so the last writer always wins
}

func NewCaddy(cfg Config) *Caddy {
	return &Caddy{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

//go:embed Caddyfile.tmpl
var caddyfileTmpl string

var tmpl = template.Must(template.New("Caddyfile").
	Funcs(template.FuncMap{"join": strings.Join}).
	Parse(caddyfileTmpl))

func (c *Caddy) Render(sites []Site) ([]byte, error) {
	for _, s := range sites {
		if len(s.Upstreams) == 0 {
			return nil, fmt.Errorf("site %s: no PHP-FPM upstreams", s.ID)
		}
		for _, u := range s.Upstreams {
			if strings.ContainsAny(u, " \t\n{}#\"") {
				return nil, fmt.Errorf("site %s: unsafe upstream %q", s.ID, u)
			}
		}
		for _, d := range s.Domains {
			if strings.ContainsAny(d, " \t\n{}#\"") {
				return nil, fmt.Errorf("site %s: unsafe domain %q", s.ID, d)
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
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, map[string]any{
		"ACMEEmail":      email,
		"AdminListen":    adminListen,
		"PanelDomain":    c.cfg.PanelDomain,
		"PanelUpstream":  c.cfg.PanelUpstream,
		"ShieldUpstream": c.cfg.ShieldUpstream,
		"SiteHeader":     shield.SiteHeader,
		"AccessLog":      c.cfg.AccessLog,
		"Sites":          sites,
	})
	return buf.Bytes(), err
}

// Apply writes the Caddyfile (so Caddy boots with it after a restart) and
// loads it live. Caddy validates the whole config before switching over, so a
// bad render never takes existing sites down.
func (c *Caddy) Apply(ctx context.Context, sites []Site) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, err := c.Render(sites)
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
