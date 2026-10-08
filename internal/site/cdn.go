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
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/proxy"
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
	SetEdgeRule(ctx context.Context, token, zoneID string, r cdn.EdgeRule) error
	DeleteEdgeRule(ctx context.Context, token, zoneID, ref, desc string) error
}

// PullZoneAPI is a pull-zone CDN's API (cdn.Bunny).
type PullZoneAPI interface {
	PullZone(ctx context.Context, key, id string) (cdn.PullZone, error)
	Purge(ctx context.Context, key, id string) error
}

// Resolver looks up a hostname's addresses (net.Resolver).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

const (
	cdnCloudflare = "cloudflare"
	// Pull zones: the CDN fetches static files for its own hostname
	// (links are rewritten); Bunny's API purges it.
	cdnBunny       = "bunny"
	cdnGeneric     = "generic"
	cdnWrapperPath = "wp-content/mu-plugins/wpgenie-cdn.php"
	// cdnPurgeEvery spaces automatic purges of one site. Cloudflare's free
	// plan allows 5 purge requests a minute per account, shared by all zones.
	cdnPurgeEvery = time.Minute
	// cdnRetryAfter backs off after a failed purge (revoked token, outage).
	cdnRetryAfter  = 5 * time.Minute
	cdnRangesKey   = "cloudflare_ranges"
	cdnRangesEvery = 24 * time.Hour
)

type CDNInput struct {
	// Provider: "cloudflare" (the whole site behind it), "bunny" or
	// "generic" (a pull zone for static files), or "" to turn it off.
	Provider string `json:"provider"`
	// APIToken is the Cloudflare API token or the bunny.net API key; empty
	// keeps the stored one (same provider).
	APIToken string `json:"api_token"`
	// AssetHost is the pull zone's hostname (bunny, generic), e.g.
	// cdn.example.com; PullZone the bunny.net pull zone ID.
	AssetHost string `json:"asset_host"`
	PullZone  string `json:"pull_zone"`
	// EdgeHTML (cloudflare) lets Cloudflare keep cached pages too.
	EdgeHTML bool `json:"edge_html"`
}

type CDNStatus struct {
	Provider  string      `json:"provider"`
	AssetHost string      `json:"asset_host"`
	PullZone  string      `json:"pull_zone"`
	EdgeHTML  bool        `json:"edge_html"`
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
	// Edge rules: site ID -> the hostnames last applied (joined), and when
	// applying last failed.
	edgeApplied sync.Map
	edgeFailed  sync.Map
}

func (s *Service) cdnLock(id string) *sync.Mutex {
	m, _ := s.cdn.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// SetCDN turns a CDN integration on (checking the credentials really work:
// they are refused, not stored, otherwise), changes it, or turns it off.
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
	cur, err := s.Store.GetCDN(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		cur = nil
	} else if err != nil {
		return nil, err
	}

	var next *store.CDN
	switch in.Provider {
	case "":
	case cdnCloudflare:
		next, err = s.cloudflareConfig(ctx, st, cur, in)
	case cdnBunny, cdnGeneric:
		next, err = s.pullZoneConfig(ctx, st, cur, in)
	default:
		err = fmt.Errorf("%w: unsupported CDN provider %q (cloudflare, bunny or generic)", ErrInvalidInput, in.Provider)
	}
	if err != nil {
		return nil, err
	}

	// Cloudflare's edge rules go when edge caching does (or Cloudflare).
	if cur != nil && cur.Provider == cdnCloudflare && cur.EdgeHTML && (next == nil || !next.EdgeHTML) {
		if err := s.applyEdgeRules(ctx, st, cur, false); err != nil {
			if next != nil && next.Provider == cdnCloudflare {
				return nil, cdnInputErr(err, "the token needs Zone → Cache Rules: Edit")
			}
			// Turning Cloudflare off with a revoked token: say so, carry on.
			s.event(id, "cdn", "Couldn't remove Cloudflare's edge cache rule (remove the WPGenie rule under Caching → Cache Rules): "+err.Error())
		}
		s.cdn.edgeApplied.Delete(id)
	}
	if next == nil {
		if err := s.Store.DeleteCDN(ctx, id); err != nil {
			return nil, err
		}
	} else if err := s.Store.SetCDN(ctx, next); err != nil {
		return nil, err
	}
	// Links to a pull zone live in the page cache: rewrite and purge when
	// its hostname changes. Caddy needs to know too (CORS, s-maxage).
	oldHost, newHost := "", ""
	if cur != nil {
		oldHost = cur.AssetHost
	}
	if next != nil {
		newHost = next.AssetHost
	}
	assetURL, err := s.assetCDNURL(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.writeCDNWrapper(id, assetURL); err != nil {
		return nil, err
	}
	if oldHost != newHost {
		if err := s.purgePageCacheFiles(id); err != nil {
			s.Log.Warn("purging after a CDN change", "site", id, "err", err)
		}
	}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	if next != nil {
		s.cdn.lastAttempt.Store(id, next.PurgedAt)
	}
	s.event(id, "cdn", cdnEventText(next))
	return s.CDNStatus(ctx, id)
}

func cdnEventText(c *store.CDN) string {
	switch {
	case c == nil:
		return "CDN integration turned off"
	case c.Provider == cdnCloudflare && c.EdgeHTML:
		return "Cloudflare: cache purging on, cached pages served from Cloudflare's edge"
	case c.Provider == cdnCloudflare:
		return "Cloudflare cache purging turned on: the CDN is purged whenever the site's cache is"
	case c.Provider == cdnBunny:
		return fmt.Sprintf("bunny.net pull zone %s: static files served from %s, purged with the site's cache", c.PullZone, c.AssetHost)
	}
	return "Static files served from the CDN at " + c.AssetHost
}

// cloudflareConfig checks a Cloudflare token: it must find each domain's
// zone and purge it (and manage cache rules, for edge caching).
func (s *Service) cloudflareConfig(ctx context.Context, st *store.Site, cur *store.CDN, in CDNInput) (*store.CDN, error) {
	if s.CDN == nil {
		return nil, fmt.Errorf("%w: no CDN client configured", ErrInvalidInput)
	}
	token := strings.TrimSpace(in.APIToken)
	if token == "" {
		if cur == nil || cur.Provider != cdnCloudflare {
			return nil, fmt.Errorf("%w: api_token is required", ErrInvalidInput)
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
	c := &store.CDN{SiteID: st.ID, Provider: cdnCloudflare, APIToken: token, Zones: zones, PurgedAt: started, EdgeHTML: in.EdgeHTML}
	if in.EdgeHTML {
		if err := s.applyEdgeRules(ctx, st, c, true); err != nil {
			return nil, cdnInputErr(err, "the token needs Zone → Cache Rules: Edit for edge caching")
		}
		s.cdn.edgeApplied.Store(st.ID, edgeKey(st))
	}
	return c, nil
}

// pullZoneConfig checks a pull zone: its hostname must be a real, separate
// hostname and, for Bunny, one the pull zone answers on, with a key that
// can purge it.
func (s *Service) pullZoneConfig(ctx context.Context, st *store.Site, cur *store.CDN, in CDNInput) (*store.CDN, error) {
	host, err := NormalizeDomain(in.AssetHost)
	if err != nil {
		return nil, fmt.Errorf("%w: asset_host: %v", ErrInvalidInput, err)
	}
	if slices.Contains(st.Domains, host) || slices.Contains(st.RedirectDomains, host) {
		return nil, fmt.Errorf("%w: the CDN needs a hostname of its own (e.g. cdn.%s), not one the site serves", ErrInvalidInput, st.PrimaryDomain)
	}
	c := &store.CDN{SiteID: st.ID, Provider: in.Provider, Zones: map[string]string{}, AssetHost: host, PurgedAt: time.Now()}
	if in.Provider == cdnGeneric {
		return c, nil
	}
	if s.Bunny == nil {
		return nil, fmt.Errorf("%w: no bunny.net client configured", ErrInvalidInput)
	}
	key := strings.TrimSpace(in.APIToken)
	if key == "" {
		if cur == nil || cur.Provider != cdnBunny {
			return nil, fmt.Errorf("%w: api_token (the bunny.net API key) is required", ErrInvalidInput)
		}
		key = cur.APIToken
	}
	if !cdn.ValidBunnyKey(key) {
		return nil, fmt.Errorf("%w: that doesn't look like a bunny.net API key", ErrInvalidInput)
	}
	zone := strings.TrimSpace(in.PullZone)
	if _, err := strconv.ParseUint(zone, 10, 63); err != nil {
		return nil, fmt.Errorf("%w: pull_zone is the pull zone's numeric ID", ErrInvalidInput)
	}
	z, err := s.Bunny.PullZone(ctx, key, zone)
	if err != nil {
		return nil, bunnyInputErr(err)
	}
	if !slices.Contains(z.Hostnames, host) {
		return nil, fmt.Errorf("%w: pull zone %s doesn't answer on %s (it has %s): add it as a hostname there, "+
			"with a CNAME to the zone", ErrInvalidInput, zone, host, strings.Join(z.Hostnames, ", "))
	}
	c.PurgedAt = time.Now()
	if err := s.Bunny.Purge(ctx, key, zone); err != nil {
		return nil, bunnyInputErr(err)
	}
	c.APIToken, c.PullZone = key, zone
	return c, nil
}

func bunnyInputErr(err error) error {
	if errors.Is(err, cdn.ErrBunnyAuth) || errors.Is(err, cdn.ErrNoPullZone) {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return err
}

func cdnURL(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host
}

// assetCDNURL is the site's pull-zone URL, or "".
// A locked site serves its own files: the pull zone can't fetch them
// without the lock's password, so links to it would load no styles.
func (s *Service) assetCDNURL(ctx context.Context, id string) (string, error) {
	if st, err := s.Store.GetSite(ctx, id); err != nil {
		return "", err
	} else if st.Lock {
		return "", nil
	}
	c, err := s.Store.GetCDN(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return cdnURL(c.AssetHost), nil
}

const cdnWrapper = `<?php
/**
 * Plugin Name: WPGenie CDN
 * Description: Links static files to the site's CDN. Managed by WPGenie: change it in the WPGenie panel; this file is rewritten on changes.
 */
define( 'WPGENIE_CDN_URL', '%s' );
if ( is_file( '/usr/local/share/wpgenie/cdn.php' ) ) {
	require_once '/usr/local/share/wpgenie/cdn.php';
}
`

// writeCDNWrapper writes (url != "") or removes the CDN wrapper. url is
// https:// and a normalised hostname: nothing that could end the PHP string.
func (s *Service) writeCDNWrapper(id, url string) error {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureManaged(root, cdnWrapperPath, fmt.Sprintf(cdnWrapper, url), url != "")
}

// ---- Cloudflare edge caching ----

func edgeRuleFor(st *store.Site, hosts []string) cdn.EdgeRule {
	return cdn.EdgeRule{
		Ref:         "wpgenie_" + st.ID,
		Description: "WPGenie " + st.ID + ": cached pages at the edge (managed by WPGenie)",
		Expression:  cdn.EdgeExpression(hosts, proxy.CacheBypassCookies),
	}
}

func edgeKey(st *store.Site) string {
	return strings.Join(slices.Sorted(slices.Values(st.Domains)), " ")
}

// applyEdgeRules puts the site's cache rule in every zone that serves one
// of its domains (on) or removes it from every zone it knows (off, and
// zones left without any of its domains).
func (s *Service) applyEdgeRules(ctx context.Context, st *store.Site, c *store.CDN, on bool) error {
	byZone := map[string][]string{}
	for _, z := range c.Zones {
		byZone[z] = nil
	}
	if on {
		for _, d := range st.Domains {
			z := c.Zones[d]
			if z == "" {
				found, err := s.CDN.FindZone(ctx, c.APIToken, d)
				if err != nil {
					return err
				}
				z, c.Zones[d] = found.ID, found.ID
			}
			byZone[z] = append(byZone[z], d)
		}
	}
	var errs []error
	for _, z := range slices.Sorted(maps.Keys(byZone)) {
		r := edgeRuleFor(st, byZone[z])
		if len(byZone[z]) > 0 {
			errs = append(errs, s.CDN.SetEdgeRule(ctx, c.APIToken, z, r))
		} else {
			errs = append(errs, s.CDN.DeleteEdgeRule(ctx, c.APIToken, z, r.Ref, r.Description))
		}
	}
	return errors.Join(errs...)
}

// syncEdgeRules keeps edge rules in step with the site's domains (added,
// removed, a new primary): checked every CDN pass, applied when the
// hostnames differ from what was applied last (once after a restart).
func (s *Service) syncEdgeRules(ctx context.Context, st *store.Site, c *store.CDN) {
	key := edgeKey(st)
	if v, ok := s.cdn.edgeApplied.Load(st.ID); ok && v.(string) == key {
		return
	}
	if v, ok := s.cdn.edgeFailed.Load(st.ID); ok && time.Since(v.(time.Time)) < cdnRetryAfter {
		return
	}
	lock := s.cdnLock(st.ID)
	lock.Lock()
	defer lock.Unlock()
	// c was listed before the lock: SetCDN may have turned edge caching
	// off meanwhile, and its rule must not come back.
	c, err := s.Store.GetCDN(ctx, st.ID)
	if err != nil || c.Provider != cdnCloudflare || !c.EdgeHTML {
		return
	}
	if err := s.applyEdgeRules(ctx, st, c, true); err != nil {
		if _, failed := s.cdn.edgeFailed.Swap(st.ID, time.Now()); !failed {
			s.Log.Warn("cdn: updating the edge cache rule", "site", st.ID, "err", err)
			s.event(st.ID, "cdn", "Updating Cloudflare's edge cache rule failed: "+err.Error())
		}
		return
	}
	s.cdn.edgeFailed.Delete(st.ID)
	s.cdn.edgeApplied.Store(st.ID, key)
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
	switch c.Provider {
	case cdnGeneric:
		return nil // no API: static URLs are versioned (?ver=) or renamed
	case cdnBunny:
		if s.Bunny == nil {
			return errors.New("no bunny.net client configured")
		}
	default:
		if s.CDN == nil {
			return errors.New("no CDN client configured")
		}
	}
	lock := s.cdnLock(st.ID)
	lock.Lock()
	defer lock.Unlock()
	// The settings as they are now: the caller's copy may predate a change
	// (the CDN loop lists every site before purging them one by one).
	fresh, err := s.Store.GetCDN(ctx, st.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && fresh.Provider != c.Provider) {
		return nil // turned off or switched: nothing of the old one to purge
	} else if err != nil {
		return err
	}
	c = fresh
	started := time.Now()
	s.cdn.lastAttempt.Store(st.ID, started)
	if c.Provider == cdnBunny {
		err := s.Bunny.Purge(ctx, c.APIToken, c.PullZone)
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return errors.Join(err, s.Store.RecordCDNPurge(rc, st.ID, started, c.Zones, err))
	}
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
	err = errors.Join(errs...)
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
		out.AssetHost, out.PullZone, out.EdgeHTML = c.AssetHost, c.PullZone, c.EdgeHTML
		if !c.PurgedAt.IsZero() && c.Provider != cdnGeneric {
			t := c.PurgedAt
			out.PurgedAt = &t
		}
		if c.Provider == cdnBunny && s.Bunny != nil {
			out.Warnings = append(out.Warnings, s.pullZoneWarnings(ctx, st, c)...)
		}
		if c.AssetHost != "" {
			// The site itself isn't behind the pull zone: only its files are.
			c = &store.CDN{Provider: c.Provider, Zones: map[string]string{}}
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
		out.Warnings = append(out.Warnings, cdnWarnings(dom, c != nil && c.Provider == cdnCloudflare)...)
	}
	if sslDenied {
		out.Warnings = append(out.Warnings, "Add Zone → Zone Settings: Read to the token to let WPGenie check the SSL/TLS mode.")
	}
	return out, nil
}

// pullZoneWarnings checks the Bunny pull zone fetches from this site.
func (s *Service) pullZoneWarnings(ctx context.Context, st *store.Site, c *store.CDN) []string {
	z, err := s.Bunny.PullZone(ctx, c.APIToken, c.PullZone)
	if err != nil {
		return []string{"Couldn't read the pull zone from bunny.net: " + err.Error()}
	}
	var w []string
	if u, err := url.Parse(z.OriginURL); err != nil || !slices.Contains(st.Domains, strings.ToLower(u.Hostname())) {
		w = append(w, fmt.Sprintf("The pull zone's origin is %q: set it to https://%s, or the CDN serves another site's files.",
			z.OriginURL, st.PrimaryDomain))
	} else if u.Scheme != "https" {
		w = append(w, "Set the pull zone's origin URL to https://: Caddy redirects plain HTTP.")
	}
	if !slices.Contains(z.Hostnames, c.AssetHost) {
		w = append(w, fmt.Sprintf("The pull zone no longer answers on %s: add the hostname back or change it here.", c.AssetHost))
	}
	return w
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
		if c.Provider == cdnCloudflare && c.EdgeHTML && s.CDN != nil {
			if st, err := s.Store.GetSite(ctx, c.SiteID); err == nil && st.Status == store.StatusActive {
				s.syncEdgeRules(ctx, st, c)
			}
		}
		if c.Provider == cdnGeneric {
			continue // nothing to purge through
		}
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
				s.event(c.SiteID, "cdn", "CDN purge failed: "+msg)
			} else if prev != nil && prev != "" {
				s.event(c.SiteID, "cdn", "CDN purges work again")
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
