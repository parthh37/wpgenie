package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/store"
)

// A CDN in front of a site (Cloudflare, free plan). Two parts:
//
//   - Real visitor IPs work for every site with no setup: Caddy believes
//     CF-Connecting-IP from Cloudflare's edge networks (cdn.Ranges), which
//     are refreshed daily.
//   - With an API token, WPGenie purges the site's hostnames from
//     Cloudflare's cache whenever WordPress purges its page cache. PHP
//     never sees the token: the daemon watches the purge marker PHP touches
//     (pageCacheMarker) and calls the API itself.

// CDNProvider is the CDN's API (cdn.Cloudflare).
type CDNProvider interface {
	FindZone(ctx context.Context, token, host string) (cdn.Zone, error)
	PurgeHosts(ctx context.Context, token, zoneID string, hosts []string) error
	SSLMode(ctx context.Context, token, zoneID string) (string, error)
}

// Resolver looks up a hostname's addresses (net.Resolver).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

const (
	cdnCloudflare = "cloudflare"
	// cdnPurgeEvery spaces automatic purges of one site. Cloudflare's free
	// plan allows 5 purge requests a minute per account, shared by all zones.
	cdnPurgeEvery = time.Minute
	// cdnRetryAfter backs off after a failed purge (revoked token, outage).
	cdnRetryAfter  = 5 * time.Minute
	cdnRangesKey   = "cloudflare_ranges"
	cdnRangesEvery = 24 * time.Hour
)

type CDNInput struct {
	Provider string `json:"provider"`  // "cloudflare", or "" to turn the integration off
	APIToken string `json:"api_token"` // empty keeps the stored token
}

type CDNStatus struct {
	Provider  string      `json:"provider"`
	Domains   []CDNDomain `json:"domains"`
	PurgedAt  *time.Time  `json:"purged_at"`
	LastError string      `json:"last_error"`
	Warnings  []string    `json:"warnings"`
}

type CDNDomain struct {
	Domain string `json:"domain"`
	// Proxied: yes (every address is Cloudflare's), no, partly, or unknown
	// (the name doesn't resolve).
	Proxied string   `json:"proxied"`
	Addrs   []string `json:"addrs"`
	// SSLMode is the zone's SSL/TLS mode (off, flexible, full, strict), ""
	// when unknown.
	SSLMode string `json:"ssl_mode"`
}

// cdnState is in-memory bookkeeping for the purge loop.
type cdnState struct {
	locks       sync.Map // site ID -> *sync.Mutex: one purge per site at a time
	lastAttempt sync.Map // site ID -> time.Time
	lastErr     sync.Map // site ID -> string, so a failure is logged once
}

func (s *Service) cdnLock(id string) *sync.Mutex {
	m, _ := s.cdn.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// SetCDN turns the CDN integration on (checking the token can find the
// site's zones and purge them) or off.
func (s *Service) SetCDN(ctx context.Context, id string, in CDNInput) (*CDNStatus, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	lock := s.cdnLock(id)
	lock.Lock()
	defer lock.Unlock()

	if in.Provider == "" {
		if err := s.Store.DeleteCDN(ctx, id); err != nil {
			return nil, err
		}
		s.event(id, "cdn", "Cloudflare cache purging turned off")
		return s.CDNStatus(ctx, id)
	}
	if in.Provider != cdnCloudflare {
		return nil, fmt.Errorf("%w: unsupported CDN provider %q", ErrInvalidInput, in.Provider)
	}
	if s.CDN == nil {
		return nil, fmt.Errorf("%w: no CDN client configured", ErrInvalidInput)
	}
	token := strings.TrimSpace(in.APIToken)
	if token == "" {
		cur, err := s.Store.GetCDN(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: api_token is required", ErrInvalidInput)
		} else if err != nil {
			return nil, err
		}
		token = cur.APIToken
	}
	if !cdn.ValidToken(token) {
		return nil, fmt.Errorf("%w: that doesn't look like a Cloudflare API token", ErrInvalidInput)
	}

	zones := map[string]string{}
	for _, d := range st.Domains {
		z, err := s.CDN.FindZone(ctx, token, d)
		if err != nil {
			return nil, cdnInputErr(err, "the token needs Zone → Zone: Read on this domain's zone")
		}
		zones[d] = z.ID
	}
	// Purging the site's own hostnames proves the token may purge (and is
	// harmless: the site may have just been put behind Cloudflare).
	started := time.Now()
	if err := s.purgeZones(ctx, token, st.Domains, zones); err != nil {
		return nil, cdnInputErr(err, "the token needs Zone → Cache Purge: Purge")
	}
	if err := s.Store.SetCDN(ctx, &store.CDN{SiteID: id, Provider: cdnCloudflare, APIToken: token,
		Zones: zones, PurgedAt: started}); err != nil {
		return nil, err
	}
	s.cdn.lastAttempt.Store(id, started)
	s.event(id, "cdn", "Cloudflare cache purging turned on: the CDN is purged whenever the site's cache is")
	return s.CDNStatus(ctx, id)
}

// cdnInputErr turns token and zone problems into a 400 with a hint;
// anything else (network, Cloudflare outage) stays an internal error.
func cdnInputErr(err error, hint string) error {
	if errors.Is(err, cdn.ErrAuth) || errors.Is(err, cdn.ErrNoZone) {
		return fmt.Errorf("%w: %v (%s)", ErrInvalidInput, err, hint)
	}
	return err
}

// purgeZones purges domains, grouped by zone.
func (s *Service) purgeZones(ctx context.Context, token string, domains []string, zones map[string]string) error {
	byZone := map[string][]string{}
	for _, d := range domains {
		if z := zones[d]; z != "" {
			byZone[z] = append(byZone[z], d)
		}
	}
	var errs []error
	for _, z := range slices.Sorted(maps.Keys(byZone)) {
		errs = append(errs, s.CDN.PurgeHosts(ctx, token, z, byZone[z]))
	}
	return errors.Join(errs...)
}

// PurgeCDN purges the site's hostnames from the CDN now.
func (s *Service) PurgeCDN(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	c, err := s.Store.GetCDN(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: the CDN integration is off for this site", ErrInvalidInput)
	} else if err != nil {
		return err
	}
	return s.purgeCDN(ctx, st, c)
}

func (s *Service) purgeCDN(ctx context.Context, st *store.Site, c *store.CDN) error {
	if s.CDN == nil {
		return errors.New("no CDN client configured")
	}
	lock := s.cdnLock(st.ID)
	lock.Lock()
	defer lock.Unlock()
	started := time.Now()
	s.cdn.lastAttempt.Store(st.ID, started)
	// Domains added since the integration was set up get their zone now.
	var errs []error
	for _, d := range st.Domains {
		if c.Zones[d] != "" {
			continue
		}
		if z, err := s.CDN.FindZone(ctx, c.APIToken, d); err != nil {
			errs = append(errs, err)
		} else {
			c.Zones[d] = z.ID
		}
	}
	errs = append(errs, s.purgeZones(ctx, c.APIToken, st.Domains, c.Zones))
	err := errors.Join(errs...)
	// Recorded on a context of its own: a cancelled request must not leave
	// a purge that happened unrecorded (it would just run again).
	rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if rerr := s.Store.RecordCDNPurge(rc, st.ID, started, c.Zones, err); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}

// CDNStatus reports the integration and checks, live, whether the site's
// domains actually go through Cloudflare and how its SSL/TLS mode is set.
// It never includes the token.
func (s *Service) CDNStatus(ctx context.Context, id string) (*CDNStatus, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := s.Store.GetCDN(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		c, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out := &CDNStatus{Domains: []CDNDomain{}, Warnings: []string{}}
	if c != nil {
		out.Provider, out.LastError = c.Provider, c.LastError
		if !c.PurgedAt.IsZero() {
			t := c.PurgedAt
			out.PurgedAt = &t
		}
	}
	sslModes := map[string]string{} // zone ID -> mode
	var sslDenied bool
	for _, d := range st.Domains {
		dom := CDNDomain{Domain: d, Proxied: "unknown", Addrs: []string{}}
		if s.CDNRanges != nil {
			dom.Proxied, dom.Addrs = s.proxiedStatus(ctx, d)
		}
		if c != nil && s.CDN != nil && c.Zones[d] != "" {
			z := c.Zones[d]
			mode, ok := sslModes[z]
			if !ok {
				var err error
				if mode, err = s.CDN.SSLMode(ctx, c.APIToken, z); err != nil {
					sslDenied = sslDenied || errors.Is(err, cdn.ErrAuth)
					mode = ""
				}
				sslModes[z] = mode
			}
			dom.SSLMode = mode
		}
		out.Domains = append(out.Domains, dom)
		out.Warnings = append(out.Warnings, cdnWarnings(dom, c != nil)...)
	}
	if sslDenied {
		out.Warnings = append(out.Warnings, "Add Zone → Zone Settings: Read to the token to let WPGenie check the SSL/TLS mode.")
	}
	return out, nil
}

func (s *Service) proxiedStatus(ctx context.Context, domain string) (string, []string) {
	var r Resolver = net.DefaultResolver
	if s.DNS != nil {
		r = s.DNS
	}
	addrs, err := r.LookupNetIP(ctx, "ip", domain)
	if err != nil || len(addrs) == 0 {
		return "unknown", []string{}
	}
	n := 0
	strs := make([]string, len(addrs))
	for i, a := range addrs {
		strs[i] = a.Unmap().String()
		if s.CDNRanges.Contains(a) {
			n++
		}
	}
	switch n {
	case len(addrs):
		return "yes", strs
	case 0:
		return "no", strs
	}
	return "partly", strs
}

func cdnWarnings(d CDNDomain, integrated bool) []string {
	var w []string
	switch d.SSLMode {
	case "off", "flexible":
		w = append(w, fmt.Sprintf("%s: Cloudflare's SSL/TLS mode is %q, so Cloudflare connects to this server over plain HTTP, "+
			"which WPGenie redirects to HTTPS: visitors get an endless redirect. Set SSL/TLS to Full (strict).", d.Domain, d.SSLMode))
	case "full":
		w = append(w, fmt.Sprintf("%s: set SSL/TLS to Full (strict). Full accepts any certificate from this server, "+
			"so the Cloudflare → server connection can be intercepted.", d.Domain))
	}
	switch {
	case integrated && d.Proxied == "no":
		w = append(w, fmt.Sprintf("%s doesn't resolve to Cloudflare: turn the proxy on (orange cloud) for its DNS records, "+
			"or visitors bypass the CDN.", d.Domain))
	case integrated && d.Proxied == "partly":
		w = append(w, fmt.Sprintf("%s resolves partly to Cloudflare and partly to other addresses: proxy every A/AAAA record.", d.Domain))
	case !integrated && d.Proxied == "yes":
		w = append(w, fmt.Sprintf("%s is behind Cloudflare, but its cache isn't purged when content changes: "+
			"add an API token to turn purging on.", d.Domain))
	}
	return w
}

// RunCDN purges a site's CDN cache after WordPress purged its page cache.
// PHP touches pageCacheMarker on every purge; a marker newer than the last
// successful purge start means the CDN is behind. Purges missed while the
// daemon was down are caught up on the first pass.
func (s *Service) RunCDN(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.cdnPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) cdnPass(ctx context.Context) {
	cdns, err := s.Store.ListCDN(ctx)
	if err != nil {
		s.Log.Warn("cdn: listing sites", "err", err)
		return
	}
	now := time.Now()
	for _, c := range cdns {
		wait := cdnPurgeEvery
		if c.LastError != "" {
			wait = cdnRetryAfter
		}
		if last, ok := s.cdn.lastAttempt.Load(c.SiteID); ok && now.Sub(last.(time.Time)) < wait {
			continue
		}
		marked, ok := s.purgeMarkerTime(c.SiteID)
		if !ok || !marked.After(c.PurgedAt) {
			continue
		}
		st, err := s.Store.GetSite(ctx, c.SiteID)
		if err != nil || st.Status != store.StatusActive {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = s.purgeCDN(pctx, st, c)
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if prev, _ := s.cdn.lastErr.Swap(c.SiteID, msg); prev != msg {
			if err != nil {
				s.Log.Warn("cdn: purge failed", "site", c.SiteID, "err", err)
				s.event(c.SiteID, "cdn", "Cloudflare purge failed: "+msg)
			} else if prev != nil && prev != "" {
				s.event(c.SiteID, "cdn", "Cloudflare purges work again")
			}
		}
	}
}

// purgeMarkerTime is when PHP (or the panel) last purged the page cache.
// The marker is in the site's docroot, so it's read through os.Root.
func (s *Service) purgeMarkerTime(id string) (time.Time, bool) {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return time.Time{}, false
	}
	defer root.Close()
	fi, err := root.Lstat(pageCacheMarker)
	if err != nil || !fi.Mode().IsRegular() {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// LoadCDNRanges restores the last Cloudflare range list fetched, so a
// restart doesn't fall back to the built-in list.
func (s *Service) LoadCDNRanges(ctx context.Context) error {
	v, err := s.Store.Setting(ctx, cdnRangesKey)
	if err != nil || v == "" {
		return err
	}
	var list []string
	if err := json.Unmarshal([]byte(v), &list); err != nil {
		return err
	}
	p, err := cdn.ParseRanges(list)
	if err != nil {
		return err
	}
	s.CDNRanges.Set(p)
	return nil
}

// RunCDNRanges refreshes Cloudflare's edge networks daily and reloads Caddy
// when they change.
func (s *Service) RunCDNRanges(ctx context.Context) {
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		if err := s.refreshCDNRanges(ctx, client, cdn.RangesURL); err != nil {
			s.Log.Warn("cdn: refreshing Cloudflare IP ranges", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cdnRangesEvery):
		}
	}
}

func (s *Service) refreshCDNRanges(ctx context.Context, client *http.Client, url string) error {
	p, err := cdn.FetchRanges(ctx, client, url)
	if err != nil {
		return err
	}
	if !s.CDNRanges.Set(p) {
		return nil
	}
	b, _ := json.Marshal(cdn.Strings(p))
	if err := s.Store.SetSetting(ctx, cdnRangesKey, string(b)); err != nil {
		return err
	}
	s.Log.Info("cdn: Cloudflare IP ranges changed", "ranges", len(p))
	return s.Sync(ctx)
}
