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
	// errNotFound: the API object doesn't exist (a zone without cache rules).
	errNotFound = errors.New("cloudflare: not found")
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
		if resp.StatusCode == http.StatusNotFound {
			return errNotFound
		}
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
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", errNotFound, msg)
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

// Edge caching of HTML: one Cache Rule per site and zone, in the zone's
// http_request_cache_settings phase. The rule makes the site's pages
// eligible for caching with edge TTL "bypass_by_default": Cloudflare only
// keeps what the origin sends Cache-Control for, which WPGenie's Caddy does
// for page-cache hits alone. Rules are found again by ref (description as a
// fallback), so they are updated rather than duplicated, and the zone's
// other rules are never touched. Needs Zone → Cache Rules: Edit.

const cachePhase = "/rulesets/phases/http_request_cache_settings/entrypoint"

// EdgeRule is the Cache Rule of one site in one zone.
type EdgeRule struct {
	Ref         string // stable: "wpgenie_<site ID>"
	Description string
	Expression  string
}

type ruleset struct {
	ID    string `json:"id"`
	Rules []rule `json:"rules"`
}

type rule struct {
	ID               string         `json:"id,omitempty"`
	Ref              string         `json:"ref,omitempty"`
	Description      string         `json:"description"`
	Expression       string         `json:"expression"`
	Action           string         `json:"action"`
	ActionParameters map[string]any `json:"action_parameters"`
	Enabled          bool           `json:"enabled"`
}

func (r EdgeRule) rule() rule {
	return rule{Ref: r.Ref, Description: r.Description, Expression: r.Expression, Action: "set_cache_settings", Enabled: true,
		ActionParameters: map[string]any{
			"cache":       true,
			"edge_ttl":    map[string]string{"mode": "bypass_by_default"},
			"browser_ttl": map[string]string{"mode": "respect_origin"},
		}}
}

// entrypoint returns the zone's cache rules ruleset; ok is false when the
// zone has none yet.
func (c *Cloudflare) entrypoint(ctx context.Context, token, zoneID string) (rs ruleset, ok bool, err error) {
	err = c.do(ctx, token, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+cachePhase, nil, &rs)
	if errors.Is(err, errNotFound) {
		return ruleset{}, false, nil // no ruleset for the phase yet
	}
	return rs, err == nil, err
}

func (rs ruleset) find(ref, desc string) (rule, bool) {
	for _, r := range rs.Rules {
		if r.Ref == ref || (desc != "" && r.Description == desc) {
			return r, true
		}
	}
	return rule{}, false
}

// SetEdgeRule creates the rule, or updates it if the zone has it already
// (a no-op when it is unchanged).
func (c *Cloudflare) SetEdgeRule(ctx context.Context, token, zoneID string, r EdgeRule) error {
	rs, ok, err := c.entrypoint(ctx, token, zoneID)
	if err != nil {
		return err
	}
	z := "/zones/" + url.PathEscape(zoneID)
	if !ok {
		// Only safe because the phase has no rules: PUT replaces them all.
		return c.do(ctx, token, http.MethodPut, z+cachePhase, map[string][]rule{"rules": {r.rule()}}, nil)
	}
	cur, found := rs.find(r.Ref, r.Description)
	if !found {
		return c.do(ctx, token, http.MethodPost, z+"/rulesets/"+url.PathEscape(rs.ID)+"/rules", r.rule(), nil)
	}
	if cur.Expression == r.Expression && cur.Enabled && cur.Action == "set_cache_settings" {
		return nil
	}
	return c.do(ctx, token, http.MethodPatch, z+"/rulesets/"+url.PathEscape(rs.ID)+"/rules/"+url.PathEscape(cur.ID), r.rule(), nil)
}

// DeleteEdgeRule removes the rule if the zone has it.
func (c *Cloudflare) DeleteEdgeRule(ctx context.Context, token, zoneID, ref, desc string) error {
	rs, ok, err := c.entrypoint(ctx, token, zoneID)
	if err != nil || !ok {
		return err
	}
	cur, found := rs.find(ref, desc)
	if !found {
		return nil
	}
	return c.do(ctx, token, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/rulesets/"+url.PathEscape(rs.ID)+"/rules/"+url.PathEscape(cur.ID), nil, nil)
}

// EdgeExpression is the rule's filter: the site's hostnames, GET/HEAD,
// and none of the cookies that mean a personalised page (the page cache's
// own list: Cloudflare's cache key ignores cookies, so without this a
// logged-in visitor could be served, and a stored page could be, anybody's
// cached copy).
func EdgeExpression(hosts, bypassCookies []string) string {
	q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	hs := make([]string, len(hosts))
	for i, h := range hosts {
		hs[i] = q(h)
	}
	parts := []string{"http.host in {" + strings.Join(hs, " ") + "}", `http.request.method in {"GET" "HEAD"}`}
	for _, c := range bypassCookies {
		parts = append(parts, "not http.cookie contains "+q(c))
	}
	return "(" + strings.Join(parts, " and ") + ")"
}
