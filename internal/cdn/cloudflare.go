package cdn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrAuth means Cloudflare refused the token (wrong token, or missing a
// permission); ErrNoZone that no zone the token can see covers a host.
var (
	ErrAuth   = errors.New("cloudflare refused the API token")
	ErrNoZone = errors.New("no Cloudflare zone for this domain is accessible with the token")
)

// tokenRe: Cloudflare API tokens are 40 URL-safe characters. Checked before
// the token goes into a header.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{20,200}$`)

func ValidToken(t string) bool { return tokenRe.MatchString(t) }

type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Plan string `json:"plan"`
}

// Cloudflare is a minimal client for the v4 API. The token needs
// Zone → Zone: Read and Zone → Cache Purge: Purge; Zone → Zone Settings: Read
// additionally lets the panel check the SSL/TLS mode.
type Cloudflare struct {
	BaseURL string // default https://api.cloudflare.com/client/v4
	Client  *http.Client
}

type apiResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c *Cloudflare) do(ctx context.Context, token, method, path string, body, into any) error {
	if !ValidToken(token) {
		return fmt.Errorf("%w: malformed token", ErrAuth)
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	var r apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return fmt.Errorf("cloudflare: HTTP %d, unreadable response", resp.StatusCode)
	}
	if !r.Success || resp.StatusCode >= 300 {
		msgs := make([]string, 0, len(r.Errors))
		for _, e := range r.Errors {
			msgs = append(msgs, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		msg := strings.Join(msgs, "; ")
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: %s", ErrAuth, msg)
		}
		return fmt.Errorf("cloudflare: %s", msg)
	}
	if into != nil {
		return json.Unmarshal(r.Result, into)
	}
	return nil
}

// FindZone returns the zone that serves host: the longest parent domain
// (host itself included) the token can see.
func (c *Cloudflare) FindZone(ctx context.Context, token, host string) (Zone, error) {
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	for i := 0; i+2 <= len(labels) && i < 6; i++ {
		name := strings.Join(labels[i:], ".")
		var zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Plan struct {
				Name string `json:"name"`
			} `json:"plan"`
		}
		if err := c.do(ctx, token, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
			return Zone{}, err
		}
		for _, z := range zones {
			if strings.EqualFold(z.Name, name) {
				return Zone{ID: z.ID, Name: z.Name, Plan: z.Plan.Name}, nil
			}
		}
	}
	return Zone{}, fmt.Errorf("%w (%s)", ErrNoZone, host)
}

// PurgeHosts empties Cloudflare's cache for these hostnames only: the zone
// may serve other hosts that have nothing to do with this site. Purging by
// hostname is available on every plan (Free: 5 requests/minute per account).
func (c *Cloudflare) PurgeHosts(ctx context.Context, token, zoneID string, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	if len(hosts) > 100 {
		return errors.New("cloudflare: at most 100 hosts per purge")
	}
	return c.do(ctx, token, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/purge_cache",
		map[string][]string{"hosts": hosts}, nil)
}

// SSLMode returns the zone's SSL/TLS encryption mode: off, flexible, full
// or strict.
func (c *Cloudflare) SSLMode(ctx context.Context, token, zoneID string) (string, error) {
	var s struct {
		Value string `json:"value"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/settings/ssl", nil, &s); err != nil {
		return "", err
	}
	return s.Value, nil
}
