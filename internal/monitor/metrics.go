package monitor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Prometheus metrics, in the text exposition format (version 0.0.4),
// written by hand: it is a few lines of formatting, not worth a dependency.
//
// Labels are bounded by design: sites (IDs), domains, filesystems, verdicts
// and job statuses. Never paths, IPs or user agents: anyone can make up
// those, and every distinct value is a new time series in Prometheus.

// ---- Traffic counters, fed by the access-log ingester ----

type siteTraffic struct {
	c           store.Counters
	cacheHits   int64
	cacheMisses int64
	phpMS       int64
	hist        store.Histogram
}

// traffic accumulates the ingester's committed batches. Counters start at
// zero with the daemon, as Prometheus expects of counters (it detects the
// reset).
type traffic struct {
	mu    sync.Mutex
	sites map[string]*siteTraffic
}

// ObserveTraffic is analytics.Ingester.Observer.
func (s *Service) ObserveTraffic(b *store.TrafficBatch) {
	t := &s.traffic
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sites == nil {
		t.sites = map[string]*siteTraffic{}
	}
	get := func(id string) *siteTraffic {
		st := t.sites[id]
		if st == nil {
			st = &siteTraffic{}
			t.sites[id] = st
		}
		return st
	}
	for k, c := range b.Hourly {
		get(k.SiteID).c.Add(*c)
	}
	for k, p := range b.Perf {
		st := get(k.SiteID)
		st.cacheHits += p.CacheHits
		st.cacheMisses += p.CacheMisses
		st.phpMS += p.PHPMS
		st.hist.Add(p.Hist)
	}
}

// snapshot copies the counters of the given sites and forgets the others
// (deleted sites): their series simply end.
func (t *traffic) snapshot(ids map[string]bool) map[string]siteTraffic {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]siteTraffic{}
	for id, st := range t.sites {
		if !ids[id] {
			delete(t.sites, id)
			continue
		}
		out[id] = *st
	}
	return out
}

// ---- Scrape token ----

// tokenKey holds the scrape token's SHA-256 (hex) and when it was made. The
// token itself is shown once, when created: a database leak must not hand
// out scrape access, and it is never the panel's API token, which can do
// everything.
const tokenKey = "monitoring_metrics_token"

type storedToken struct {
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
}

type tokenCache struct {
	mu     sync.Mutex
	loaded bool
	tok    storedToken
}

func (s *Service) storedToken(ctx context.Context) (storedToken, error) {
	s.token.mu.Lock()
	defer s.token.mu.Unlock()
	if s.token.loaded {
		return s.token.tok, nil
	}
	v, err := s.Store.Setting(ctx, tokenKey)
	if err != nil {
		return storedToken{}, err
	}
	var t storedToken
	if v != "" {
		if err := json.Unmarshal([]byte(v), &t); err != nil {
			return storedToken{}, err
		}
	}
	s.token.tok, s.token.loaded = t, true
	return t, nil
}

// TokenInfo says whether a scrape token exists (never the token).
type TokenInfo struct {
	Set       bool      `json:"set"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

func (s *Service) MetricsTokenInfo(ctx context.Context) (TokenInfo, error) {
	t, err := s.storedToken(ctx)
	return TokenInfo{Set: t.Hash != "", CreatedAt: t.CreatedAt}, err
}

// RotateMetricsToken creates a new scrape token, replacing any previous
// one, and returns it: this is the only time it is shown.
func (s *Service) RotateMetricsToken(ctx context.Context) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "wpgm_" + hex.EncodeToString(b)
	st := storedToken{Hash: auth.HashToken(tok), CreatedAt: s.now().UTC().Truncate(time.Second)}
	v, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	s.token.mu.Lock()
	defer s.token.mu.Unlock()
	if err := s.Store.SetSetting(ctx, tokenKey, string(v)); err != nil {
		return "", err
	}
	s.token.tok, s.token.loaded = st, true
	return tok, nil
}

func (s *Service) scrapeAllowed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return false
	}
	st, err := s.storedToken(r.Context())
	if err != nil {
		s.Log.Error("monitor: reading the metrics token", "err", err)
		return false
	}
	if st.Hash == "" {
		return false // no token yet: metrics are off
	}
	return subtle.ConstantTimeCompare([]byte(auth.HashToken(tok)), []byte(st.Hash)) == 1
}

// MetricsHandler serves GET /metrics to holders of the scrape token.
func (s *Service) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.init()
		if !s.scrapeAllowed(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wpgenie-metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body []byte
		var err error
		if node := r.URL.Query().Get("node"); node != "" {
			// Another server's sites: scraped through the panel (one token,
			// no route from Prometheus to the nodes needed).
			if s.NodeMetrics == nil {
				http.Error(w, "no such server", http.StatusNotFound)
				return
			}
			body, err = s.NodeMetrics(r.Context(), node)
		} else {
			body, err = s.Collect(r.Context())
		}
		if err != nil {
			s.Log.Error("monitor: collecting metrics", "err", err)
			http.Error(w, "collecting metrics failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write(body)
	})
}

// ---- Exposition ----

type promWriter struct {
	b bytes.Buffer
}

// family starts a metric family: every sample of a name must follow its
// HELP and TYPE lines, together.
func (w *promWriter) family(name, typ, help string) {
	w.b.WriteString("# HELP " + name + " " + strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(help) + "\n")
	w.b.WriteString("# TYPE " + name + " " + typ + "\n")
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// sample writes one line; labels are name, value pairs.
func (w *promWriter) sample(name string, v float64, labels ...string) {
	w.b.WriteString(name)
	if len(labels) > 0 {
		w.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.b.WriteByte(',')
			}
			w.b.WriteString(labels[i] + `="` + labelEscaper.Replace(labels[i+1]) + `"`)
		}
		w.b.WriteByte('}')
	}
	w.b.WriteByte(' ')
	w.b.WriteString(formatValue(v))
	w.b.WriteByte('\n')
}

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64) // byte counts as plain integers
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect renders every metric.
func (s *Service) Collect(ctx context.Context) ([]byte, error) {
	s.init()
	now := s.now()
	sites, err := s.Sites(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(sites, func(a, b *store.Site) int { return strings.Compare(a.ID, b.ID) })
	ids := map[string]bool{}
	for _, st := range sites {
		ids[st.ID] = true
	}
	policies, err := s.Store.BackupPolicies(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := s.Store.JobCounts(ctx)
	if err != nil {
		return nil, err
	}
	alerts, err := s.Store.Alerts(ctx)
	if err != nil {
		return nil, err
	}
	var hosts []Host
	if s.Hosts != nil {
		if hosts, err = s.Hosts(ctx); err != nil {
			s.Log.Warn("monitor: host metrics", "err", err)
		}
	}
	traffic := s.traffic.snapshot(ids)

	s.mu.Lock()
	probes := make(map[string]probeResult, len(s.probes))
	for k, v := range s.probes {
		probes[k] = v
	}
	certs := make(map[string]certState, len(s.certs))
	for k, v := range s.certs {
		certs[k] = v
	}
	vulns := make(map[string]int, len(s.vulns))
	for k, v := range s.vulns {
		vulns[k] = v
	}
	s.mu.Unlock()

	w := &promWriter{}
	w.family("wpgenie_build_info", "gauge", "WPGenie version (always 1).")
	w.sample("wpgenie_build_info", 1, "version", s.Version, "goversion", runtime.Version())
	w.family("wpgenie_start_time_seconds", "gauge", "When the daemon started (unix seconds).")
	w.sample("wpgenie_start_time_seconds", float64(s.started.Unix()))
	w.family("wpgenie_uptime_seconds", "gauge", "Seconds since the daemon started.")
	w.sample("wpgenie_uptime_seconds", math.Round(now.Sub(s.started).Seconds()))

	w.family("wpgenie_site_info", "gauge", "Sites (always 1): primary domain, status, staging.")
	for _, st := range sites {
		w.sample("wpgenie_site_info", 1, "site", st.ID, "domain", st.PrimaryDomain, "status", string(st.Status),
			"staging", strconv.FormatBool(st.ParentID != ""))
	}

	counter := func(name, help string, v func(siteTraffic) int64) {
		w.family(name, "counter", help)
		for _, st := range sites {
			if t, ok := traffic[st.ID]; ok {
				w.sample(name, float64(v(t)), "site", st.ID)
			}
		}
	}
	counter("wpgenie_site_requests_total", "Requests served (access log).", func(t siteTraffic) int64 { return t.c.Requests })
	counter("wpgenie_site_bytes_out_total", "Response bytes sent.", func(t siteTraffic) int64 { return t.c.BytesOut })
	counter("wpgenie_site_page_views_total", "HTML page views by people (not bots, not blocked).",
		func(t siteTraffic) int64 { return t.c.PageViews })
	counter("wpgenie_site_bot_hits_total", "Requests from bots.", func(t siteTraffic) int64 { return t.c.BotHits })
	counter("wpgenie_site_shield_blocked_total", "Requests the shield blocked, challenged or throttled.",
		func(t siteTraffic) int64 { return t.c.Blocked })
	counter("wpgenie_site_http_5xx_total", "Responses with a 5xx status.", func(t siteTraffic) int64 { return t.c.Errors5xx })
	counter("wpgenie_site_page_cache_hits_total", "Pages served from the page cache.", func(t siteTraffic) int64 { return t.cacheHits })
	counter("wpgenie_site_page_cache_misses_total", "Cacheable pages PHP had to render.", func(t siteTraffic) int64 { return t.cacheMisses })

	const hist = "wpgenie_site_php_response_seconds"
	w.family(hist, "histogram", "PHP response times (from Caddy's access log).")
	for _, st := range sites {
		t, ok := traffic[st.ID]
		if !ok {
			continue
		}
		var cum, total int64
		for _, c := range t.hist {
			total += c
		}
		for i, le := range store.LatencyBuckets {
			cum += t.hist[i]
			w.sample(hist+"_bucket", float64(cum), "site", st.ID, "le", formatValue(float64(le)/1000))
		}
		w.sample(hist+"_bucket", float64(total), "site", st.ID, "le", "+Inf")
		w.sample(hist+"_sum", float64(t.phpMS)/1000, "site", st.ID)
		w.sample(hist+"_count", float64(total), "site", st.ID)
	}

	w.family("wpgenie_site_replicas_desired", "gauge", "PHP-FPM replicas the site is configured for.")
	for _, st := range sites {
		if st.Status == store.StatusActive {
			w.sample("wpgenie_site_replicas_desired", float64(st.Replicas), "site", st.ID)
		}
	}
	if s.Load != nil {
		type reading struct {
			id string
			l  site.CPUReading
		}
		var rs []reading
		for _, st := range sites {
			if l, ok := s.Load(st.ID); ok {
				rs = append(rs, reading{st.ID, l})
			}
		}
		w.family("wpgenie_site_replicas_running", "gauge", "PHP-FPM replicas serving the site (last sample).")
		for _, r := range rs {
			w.sample("wpgenie_site_replicas_running", float64(r.l.Replicas), "site", r.id)
		}
		w.family("wpgenie_site_cpu_percent", "gauge", "CPU use in percent of each replica's allowance (average).")
		for _, r := range rs {
			w.sample("wpgenie_site_cpu_percent", r.l.Percent, "site", r.id)
		}
		// Workers is nil when PHP-FPM couldn't be sampled.
		w.family("wpgenie_site_php_workers_busy_percent", "gauge", "PHP workers busy, queued requests included (over 100: requests wait).")
		for _, r := range rs {
			if r.l.Workers != nil {
				w.sample("wpgenie_site_php_workers_busy_percent", *r.l.Workers, "site", r.id)
			}
		}
		w.family("wpgenie_site_php_queued_requests", "gauge", "Requests waiting for a PHP worker.")
		for _, r := range rs {
			if r.l.Workers != nil {
				w.sample("wpgenie_site_php_queued_requests", float64(r.l.Queued), "site", r.id)
			}
		}
	}

	w.family("wpgenie_site_vulnerable_components", "gauge", "WordPress core, plugins and themes with known vulnerabilities (latest scan).")
	for _, st := range sites {
		if n, ok := vulns[st.ID]; ok {
			w.sample("wpgenie_site_vulnerable_components", float64(n), "site", st.ID)
		}
	}

	slices.SortFunc(policies, func(a, b *store.BackupPolicy) int { return strings.Compare(a.SiteID, b.SiteID) })
	w.family("wpgenie_site_last_backup_timestamp_seconds", "gauge", "When the last successful backup finished (unix seconds).")
	for _, p := range policies {
		if ids[p.SiteID] && !p.LastBackupAt.IsZero() {
			w.sample("wpgenie_site_last_backup_timestamp_seconds", float64(p.LastBackupAt.Unix()), "site", p.SiteID)
		}
	}
	w.family("wpgenie_site_backup_age_seconds", "gauge", "Seconds since the last successful backup.")
	for _, p := range policies {
		if ids[p.SiteID] && !p.LastBackupAt.IsZero() {
			w.sample("wpgenie_site_backup_age_seconds", math.Round(now.Sub(p.LastBackupAt).Seconds()), "site", p.SiteID)
		}
	}

	w.family("wpgenie_site_up", "gauge", "Whether the site answered its last uptime probe (home and login pages through Caddy).")
	for _, st := range sites {
		if p, ok := probes[st.ID]; ok {
			w.sample("wpgenie_site_up", b2f(p.up), "site", st.ID)
		}
	}
	w.family("wpgenie_site_probe_duration_seconds", "gauge", "How long the last uptime probe took.")
	for _, st := range sites {
		if p, ok := probes[st.ID]; ok {
			w.sample("wpgenie_site_probe_duration_seconds", p.duration.Seconds(), "site", st.ID)
		}
	}

	domains := make([]string, 0, len(certs))
	for d, c := range certs {
		if c.res.Served {
			domains = append(domains, d)
		}
	}
	slices.Sort(domains)
	w.family("wpgenie_cert_expiry_seconds", "gauge", "Seconds until the certificate served for the domain expires (negative: expired).")
	for _, d := range domains {
		c := certs[d]
		w.sample("wpgenie_cert_expiry_seconds", math.Round(c.res.NotAfter.Sub(now).Seconds()), "domain", d, "site", c.siteID)
	}
	w.family("wpgenie_cert_valid", "gauge", "Whether the certificate served for the domain covers it and is trusted.")
	for _, d := range domains {
		c := certs[d]
		w.sample("wpgenie_cert_valid", b2f(c.res.Invalid == "" && c.res.NotAfter.After(now)), "domain", d, "site", c.siteID)
	}

	if s.Shield != nil {
		dec := s.Shield.Decisions()
		verdicts := make([]string, 0, len(dec))
		for v := range dec {
			verdicts = append(verdicts, v)
		}
		slices.Sort(verdicts)
		w.family("wpgenie_shield_decisions_total", "counter", "Shield decisions by verdict.")
		for _, v := range verdicts {
			w.sample("wpgenie_shield_decisions_total", float64(dec[v]), "verdict", v)
		}
		w.family("wpgenie_shield_active_bans", "gauge", "Addresses banned right now.")
		w.sample("wpgenie_shield_active_bans", float64(len(s.Shield.Bans())))
	}

	w.family("wpgenie_jobs", "gauge", "Jobs in the (bounded) job table, by status.")
	for _, st := range []string{store.JobQueued, store.JobRunning, store.JobSucceeded, store.JobFailed} {
		w.sample("wpgenie_jobs", float64(jobs[st]), "status", st)
	}

	fs := func(name, help string, v func(Disk) uint64) {
		w.family(name, "gauge", help)
		for _, h := range hosts {
			for _, d := range h.Disks {
				labels := []string{"path", d.Path}
				if h.Node != "" {
					labels = append(labels, "node", h.Node)
				}
				w.sample(name, float64(v(d)), labels...)
			}
		}
	}
	fs("wpgenie_filesystem_size_bytes", "Size of the filesystems WPGenie keeps data on.", func(d Disk) uint64 { return d.Size })
	fs("wpgenie_filesystem_free_bytes", "Free space, including blocks reserved for root.", func(d Disk) uint64 { return d.Free })
	fs("wpgenie_filesystem_avail_bytes", "Space available to unprivileged processes.", func(d Disk) uint64 { return d.Avail })
	mem := func(name, help string, v func(Host) uint64) {
		w.family(name, "gauge", help)
		for _, h := range hosts {
			if h.MemTotal == 0 {
				continue
			}
			if h.Node != "" {
				w.sample(name, float64(v(h)), "node", h.Node)
			} else {
				w.sample(name, float64(v(h)))
			}
		}
	}
	mem("wpgenie_memory_total_bytes", "Host memory (MemTotal).", func(h Host) uint64 { return h.MemTotal })
	mem("wpgenie_memory_available_bytes", "Host memory available without swapping (MemAvailable).", func(h Host) uint64 { return h.MemAvailable })

	firing := map[string]int{SevWarning: 0, SevCritical: 0}
	for _, a := range alerts {
		if a.State == store.AlertFiring {
			firing[a.Severity]++
		}
	}
	w.family("wpgenie_alerts_firing", "gauge", "Alerts firing, by severity.")
	for _, sev := range []string{SevCritical, SevWarning} {
		w.sample("wpgenie_alerts_firing", float64(firing[sev]), "severity", sev)
	}
	return w.b.Bytes(), nil
}
