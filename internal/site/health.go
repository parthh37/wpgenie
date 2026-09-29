package site

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/shield"
)

// Health is what a site looked like from the outside at one moment.
type Health struct {
	// Checked is false when the site couldn't be reached at all (typically
	// no TLS certificate yet because DNS doesn't point here): there is then
	// nothing to compare, and updates can't be judged by it.
	Checked bool   `json:"checked"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
}

// Prober checks a site the way visitors reach it.
type Prober interface {
	Probe(ctx context.Context, domain string) Health
}

// fatalMarkers are what WordPress renders when PHP dies: a broken plugin
// update typically still returns a page, just not the site.
var fatalMarkers = []string{
	"There has been a critical error on this website",
	"Error establishing a database connection",
	"<b>Fatal error</b>:",
	"Parse error: syntax error",
}

// HTTPProber requests the home page and the login page through Caddy on
// loopback, so the check covers the whole stack (Caddy, PHP-FPM, MariaDB)
// without depending on public DNS. Requests are trusted by
// the shield when they carry its health token; the query string bypasses
// the page cache.
type HTTPProber struct {
	Addr  string // Caddy's HTTPS listener, default 127.0.0.1:443
	Token string // shield.Options.HealthToken
	// Dial, if set, reaches Caddy instead of Addr (a site on another node:
	// the cluster tunnel to that node's Caddy, with that node's Token).
	Dial func(ctx context.Context) (net.Conn, error)
}

func (p *HTTPProber) Probe(ctx context.Context, domain string) Health {
	addr := p.Addr
	if addr == "" {
		addr = "127.0.0.1:443"
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				if p.Dial != nil {
					return p.Dial(ctx)
				}
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			},
			// Full verification: Caddy serves the site's real (ACME)
			// certificate on loopback too. Without one the handshake fails and
			// the site counts as unchecked, which is the honest answer.
			TLSClientConfig:   &tls.Config{ServerName: domain, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for i, path := range []string{"/", "/wp-login.php"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://"+domain+path+"?wpgenie-health="+randString(8, lowerAlnum), nil)
		if err != nil {
			return Health{Detail: err.Error()}
		}
		req.Header.Set("User-Agent", "WPGenie-Health/1.0")
		if p.Token != "" {
			req.Header.Set(shield.HealthHeader, p.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			if i == 0 {
				return Health{Detail: "unreachable: " + err.Error()}
			}
			return Health{Checked: true, Detail: path + ": " + err.Error()}
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if h := judge(path, resp.StatusCode, string(body)); !h.OK {
			return h
		}
	}
	return Health{Checked: true, OK: true}
}

func judge(path string, status int, body string) Health {
	if status >= 500 {
		return Health{Checked: true, Detail: fmt.Sprintf("%s: HTTP %d", path, status)}
	}
	for _, m := range fatalMarkers {
		if strings.Contains(body, m) {
			return Health{Checked: true, Detail: fmt.Sprintf("%s: page shows %q", path, m)}
		}
	}
	return Health{Checked: true, OK: true}
}
