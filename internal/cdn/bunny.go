package cdn

import (
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

// Bunny is a minimal client for bunny.net's API (pull zones). The key is
// the account's API key (Account settings → API).
type Bunny struct {
	BaseURL string // default https://api.bunny.net
	Client  *http.Client
}

// ErrBunnyAuth means bunny.net refused the API key; ErrNoPullZone that no
// pull zone has that ID.
var (
	ErrBunnyAuth  = errors.New("bunny.net refused the API key")
	ErrNoPullZone = errors.New("no pull zone with that ID in this bunny.net account")
)

var bunnyKeyRe = regexp.MustCompile(`^[A-Za-z0-9-]{20,100}$`)

func ValidBunnyKey(k string) bool { return bunnyKeyRe.MatchString(k) }

// PullZone is what WPGenie checks of a pull zone: the hostnames it
// answers on and where it fetches from.
type PullZone struct {
	ID        int64
	Name      string
	Hostnames []string
	OriginURL string
}

func (b *Bunny) do(ctx context.Context, key, method, path string, into any) error {
	if !ValidBunnyKey(key) {
		return fmt.Errorf("%w: malformed key", ErrBunnyAuth)
	}
	base := b.BaseURL
	if base == "" {
		base = "https://api.bunny.net"
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("AccessKey", key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("bunny.net: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrBunnyAuth
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoPullZone
	case resp.StatusCode >= 300:
		var e struct {
			Message string `json:"Message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("bunny.net: %s", e.Message)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			return fmt.Errorf("bunny.net: unreadable response: %w", err)
		}
	}
	return nil
}

func (b *Bunny) PullZone(ctx context.Context, key, id string) (PullZone, error) {
	var z struct {
		ID        int64  `json:"Id"`
		Name      string `json:"Name"`
		OriginURL string `json:"OriginUrl"`
		Hostnames []struct {
			Value string `json:"Value"`
		} `json:"Hostnames"`
	}
	if err := b.do(ctx, key, http.MethodGet, "/pullzone/"+url.PathEscape(id), &z); err != nil {
		return PullZone{}, err
	}
	out := PullZone{ID: z.ID, Name: z.Name, OriginURL: z.OriginURL}
	for _, h := range z.Hostnames {
		out.Hostnames = append(out.Hostnames, strings.ToLower(h.Value))
	}
	return out, nil
}

// Purge empties the pull zone's cache. The zone only serves this site's
// static files, so all of it goes.
func (b *Bunny) Purge(ctx context.Context, key, id string) error {
	return b.do(ctx, key, http.MethodPost, "/pullzone/"+url.PathEscape(id)+"/purgeCache", nil)
}
