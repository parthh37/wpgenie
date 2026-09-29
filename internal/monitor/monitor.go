// Package monitor watches the server and its sites from the outside and
// tells people when something breaks: Prometheus metrics (GET /metrics,
// behind its own bearer token), and alerts for sites that stop answering,
// certificates about to expire or not valid for their domain, filesystems
// filling up and backups that keep failing, sent by e-mail and signed
// webhooks.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// EvalInterval is how often alerts are evaluated (and sites probed).
const EvalInterval = time.Minute

const (
	// Certificates change rarely: one handshake per domain an hour is
	// plenty, every ten minutes while the last result was bad (so a fix
	// shows up soon).
	certInterval    = time.Hour
	certBadInterval = 10 * time.Minute
	// Probes and handshakes run a few at a time.
	probeConcurrency = 8
	probeTimeout     = 20 * time.Second
	// Known vulnerability counts come from the stored scan reports.
	vulnInterval = 10 * time.Minute
)

// Alert kinds (store.Alert.Kind).
const (
	KindSiteDown = "site_down"
	KindCert     = "certificate"
	KindDisk     = "disk"
	KindBackup   = "backup"
	KindNode     = "node"
)

const (
	SevWarning  = "warning"
	SevCritical = "critical"
)

// ShieldStats is what the metrics read from the shield.
type ShieldStats interface {
	Bans() []shield.Ban
	Decisions() map[string]uint64
}

// Service evaluates alerts and serves metrics.
//
// The func fields are its data sources. Their defaults watch this server;
// a multi-node control plane swaps them for versions that reach each
// node's sites (Probe, CertCheck route by the site's node) and report
// every node's filesystems and memory (Hosts, one Host per node). The
// evaluator itself doesn't change: alerts are keyed by site, domain and
// node + path.
type Service struct {
	Store   *store.Store
	Log     *slog.Logger
	Version string
	// Server names this installation in notifications (default: hostname).
	Server string

	// Sites lists every site (default Store.ListSites). Live ones (active,
	// not staging) are probed and have their certificates checked.
	Sites func(ctx context.Context) ([]*store.Site, error)
	// Probe checks a site the way visitors reach it (LocalProbe). nil: no
	// uptime checks.
	Probe func(ctx context.Context, st *store.Site) site.Health
	// CertCheck reports the certificate served for a domain (default
	// TLSChecker on 127.0.0.1:443).
	CertCheck func(ctx context.Context, t CertTarget) CertResult
	// Hosts reports servers' memory and filesystems (LocalHost).
	Hosts func(ctx context.Context) ([]Host, error)
	// Nodes reports the other servers of a cluster (nil: none); one that
	// stops answering the control plane is an alert. NodeMetrics fetches a
	// node's own metrics (its sites' traffic) for /metrics?node=.
	Nodes       func(ctx context.Context) ([]NodeState, error)
	NodeMetrics func(ctx context.Context, node string) ([]byte, error)
	// Load is a site's latest CPU/PHP-FPM reading (site.Service.CPU);
	// metrics only.
	Load func(siteID string) (site.CPUReading, bool)
	// Shield is the request shield (metrics only; optional).
	Shield ShieldStats
	// Notifier delivers notifications (e-mail, webhooks).
	Notifier *Notifier

	Now func() time.Time // for tests

	initOnce sync.Once
	started  time.Time
	inFlight chan struct{}
	wg       sync.WaitGroup
	evalMu   sync.Mutex // one evaluation at a time

	mu       sync.Mutex // guards what follows: the evaluator's memory, read by the metrics
	failures map[string]int
	probes   map[string]probeResult
	certs    map[string]certState
	vulns    map[string]int
	vulnsAt  time.Time
	lastEval time.Time

	traffic traffic
	token   tokenCache
}

type probeResult struct {
	at       time.Time
	up       bool
	duration time.Duration
}

type certState struct {
	at     time.Time
	siteID string
	res    CertResult
}

func (s *Service) init() {
	s.initOnce.Do(func() {
		s.started = s.now()
		s.inFlight = make(chan struct{}, maxInFlight)
		s.failures, s.probes, s.certs, s.vulns = map[string]int{}, map[string]probeResult{}, map[string]certState{}, map[string]int{}
		if s.Log == nil {
			s.Log = slog.Default()
		}
		if s.Server == "" {
			s.Server, _ = os.Hostname()
		}
		if s.Sites == nil {
			s.Sites = s.Store.ListSites
		}
		if s.CertCheck == nil {
			s.CertCheck = (&TLSChecker{}).Check
		}
		if s.Notifier == nil {
			s.Notifier = &Notifier{}
		}
		if s.Notifier.UserAgent == "" {
			s.Notifier.UserAgent = "WPGenie/" + s.Version
		}
	})
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// startDelay lets Caddy and the sites come up with the daemon before the
// first evaluation.
var startDelay = 30 * time.Second

// Run evaluates alerts every minute until ctx ends, then waits (briefly)
// for notifications still being delivered.
func (s *Service) Run(ctx context.Context) {
	s.init()
	defer s.wait(10 * time.Second)
	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}
	t := time.NewTicker(EvalInterval)
	defer t.Stop()
	for {
		if err := s.Evaluate(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("monitor: evaluating alerts", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) wait(d time.Duration) bool {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// finding is one check's verdict on one target.
type finding struct {
	key, kind, siteID, target string
	severity                  string // "" = healthy
	message                   string
	// unknown: can't be judged now (not reachable yet, a probe failure
	// short of the threshold): the current state stands.
	unknown bool
}

// Evaluate runs every check once, updates alert state and notifies.
// Notifications are delivered in the background with ctx.
func (s *Service) Evaluate(ctx context.Context) error {
	s.init()
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	now := s.now()
	set, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	sites, err := s.Sites(ctx)
	if err != nil {
		return err
	}
	rows, err := s.Store.Alerts(ctx)
	if err != nil {
		return err
	}
	existing := make(map[string]store.Alert, len(rows))
	for _, a := range rows {
		existing[a.Key] = a
	}
	var live []*store.Site
	for _, st := range sites {
		if isLive(st) {
			live = append(live, st)
		}
	}

	var findings []finding
	checked := map[string]bool{}
	if s.Probe != nil {
		if up, ok := s.checkUptime(ctx, now, set, live, existing); ok {
			findings = append(findings, up...)
			checked[KindSiteDown] = true
		}
	}
	certs, err := s.checkCerts(ctx, now, set, live)
	if err != nil {
		s.Log.Warn("monitor: certificates", "err", err)
	} else {
		findings = append(findings, certs...)
		checked[KindCert] = true
	}
	if s.Hosts != nil {
		if hosts, err := s.Hosts(ctx); err != nil {
			s.Log.Warn("monitor: filesystems", "err", err)
		} else {
			findings = append(findings, diskFindings(set, hosts)...)
			checked[KindDisk] = true
		}
	}
	if s.Nodes != nil {
		if nodes, err := s.Nodes(ctx); err != nil {
			s.Log.Warn("monitor: servers", "err", err)
		} else {
			findings = append(findings, nodeFindings(now, nodes)...)
			checked[KindNode] = true
		}
	}
	if backups, err := s.checkBackups(ctx, now, live); err != nil {
		s.Log.Warn("monitor: backups", "err", err)
	} else {
		findings = append(findings, backups...)
		checked[KindBackup] = true
	}
	s.refreshVulns(ctx, now, sites)

	notices, err := s.apply(ctx, now, set, existing, findings, checked)
	s.mu.Lock()
	s.lastEval = now
	s.mu.Unlock()
	if len(notices) > 0 {
		if s.dispatch(ctx, set, Message{Server: s.Server, Time: now, Notices: notices}) {
			var keys []string
			for _, n := range notices {
				if n.Event != EventResolved {
					keys = append(keys, n.Alert.Key)
				}
			}
			if err := s.Store.AlertsNotified(ctx, keys, now); err != nil {
				s.Log.Warn("monitor: recording notifications", "err", err)
			}
		}
	}
	return err
}

// isLive: sites visitors rely on. Staging copies, sites being created or
// that failed to be (and any status a later version adds, such as
// suspended) aren't paged about.
func isLive(st *store.Site) bool { return st.Status == store.StatusActive && st.ParentID == "" }

// apply turns findings into alert state transitions and the notices to
// send. Targets of a checked kind that no finding mentions any more (a
// deleted site or domain) are resolved and forgotten.
func (s *Service) apply(ctx context.Context, now time.Time, set Settings, existing map[string]store.Alert,
	findings []finding, checked map[string]bool) ([]Notice, error) {
	var notices []Notice
	var errs []error
	put := func(a store.Alert, history bool) bool {
		if err := s.Store.PutAlert(ctx, a, history); err != nil {
			errs = append(errs, err)
			return false
		}
		return true
	}
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.key] = true
		cur, known := existing[f.key]
		switch {
		case f.unknown:
			continue
		case f.severity == "" && !known:
			// First healthy verdict: from now on, failing is news.
			put(store.Alert{Key: f.key, Kind: f.kind, SiteID: f.siteID, Target: f.target, State: store.AlertResolved,
				Message: f.message, Since: now, UpdatedAt: now}, false)
		case f.severity == "" && cur.State == store.AlertFiring:
			a := cur
			a.State, a.Severity, a.Message, a.Since, a.UpdatedAt = store.AlertResolved, "", f.message, now, now
			if put(a, true) {
				notices = append(notices, Notice{Event: EventResolved, Alert: a})
			}
		case f.severity == "":
			// Still fine: nothing to write.
		case !known || cur.State != store.AlertFiring:
			a := store.Alert{Key: f.key, Kind: f.kind, SiteID: f.siteID, Target: f.target, Severity: f.severity,
				State: store.AlertFiring, Message: f.message, Since: now, UpdatedAt: now}
			if put(a, true) {
				notices = append(notices, Notice{Event: EventFiring, Alert: a})
			}
		case cur.Severity != f.severity:
			// Escalated or eased: notified like a new alert, same "since".
			a := cur
			a.Severity, a.Message, a.UpdatedAt = f.severity, f.message, now
			if put(a, true) {
				notices = append(notices, Notice{Event: EventFiring, Alert: a})
			}
		default:
			a := cur
			if a.Message != f.message {
				a.Message, a.UpdatedAt = f.message, now
				put(a, false)
			}
			last := a.NotifiedAt
			if last.IsZero() {
				last = a.Since
			}
			if set.RenotifyHours > 0 && now.Sub(last) >= time.Duration(set.RenotifyHours)*time.Hour {
				notices = append(notices, Notice{Event: EventReminder, Alert: a})
			}
		}
	}
	for key, cur := range existing {
		if seen[key] || !checked[cur.Kind] {
			continue
		}
		if cur.State == store.AlertFiring {
			a := cur
			a.State, a.Severity, a.Since, a.UpdatedAt = store.AlertResolved, "", now, now
			a.Message = fmt.Sprintf("No longer watched (%s removed): %s", cur.Kind, cur.Target)
			if put(a, true) {
				notices = append(notices, Notice{Event: EventResolved, Alert: a})
			}
		}
		if err := s.Store.DeleteAlert(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return notices, fmt.Errorf("saving alert state: %w", errs[0])
	}
	return notices, nil
}

// each runs fn over items, a few at a time.
func each[T any](items []T, fn func(T)) {
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			fn(it)
		}()
	}
	wg.Wait()
}

func (s *Service) checkUptime(ctx context.Context, now time.Time, set Settings, live []*store.Site,
	existing map[string]store.Alert) ([]finding, bool) {
	results := make(map[string]site.Health, len(live))
	var rmu sync.Mutex
	each(live, func(st *store.Site) {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		start := time.Now()
		h := s.Probe(pctx, st)
		d := time.Since(start)
		cancel()
		rmu.Lock()
		results[st.ID] = h
		rmu.Unlock()
		s.mu.Lock()
		s.probes[st.ID] = probeResult{at: now, up: h.OK, duration: d}
		s.mu.Unlock()
	})
	if ctx.Err() != nil {
		return nil, false // shutting down: cancelled probes say nothing about the sites
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.failures {
		if _, ok := results[id]; !ok {
			delete(s.failures, id)
		}
	}
	for id := range s.probes {
		if _, ok := results[id]; !ok {
			delete(s.probes, id)
		}
	}
	out := make([]finding, 0, len(live))
	for _, st := range live {
		h := results[st.ID]
		f := finding{key: KindSiteDown + ":" + st.ID, kind: KindSiteDown, siteID: st.ID, target: st.PrimaryDomain}
		_, judged := existing[f.key]
		switch {
		case h.OK:
			s.failures[st.ID] = 0
			f.message = st.PrimaryDomain + " responds normally"
		case !h.Checked && !judged:
			// Never seen working (no certificate yet: DNS doesn't point
			// here): the same "can't be judged" as the update manager's.
			f.unknown = true
		case s.maintaining(ctx, st.ID):
			// A restore, push or update is working on the site: it may be
			// down on purpose for a while, and says so in its own job.
			f.unknown = true
		default:
			s.failures[st.ID]++
			if n := s.failures[st.ID]; n >= set.DownAfter {
				f.severity = SevCritical
				f.message = fmt.Sprintf("%s is down: %s (%d failed checks in a row)", st.PrimaryDomain, oneLine(h.Detail, 300), n)
			} else {
				f.unknown = true
			}
		}
		out = append(out, f)
	}
	return out, true
}

// maintaining reports whether a job or a WordPress update is running on a
// site (only asked about sites failing their probe).
func (s *Service) maintaining(ctx context.Context, siteID string) bool {
	jobs, err := s.Store.Jobs(ctx, siteID, true, 20)
	if err == nil {
		for _, j := range jobs {
			if j.Status == store.JobRunning {
				return true
			}
		}
	}
	runs, err := s.Store.Updates(ctx, siteID, 1)
	return err == nil && len(runs) == 1 && runs[0].Status == store.UpdateRunning
}

func (s *Service) checkCerts(ctx context.Context, now time.Time, set Settings, live []*store.Site) ([]finding, error) {
	own, err := s.Store.SiteCerts(ctx)
	if err != nil {
		return nil, err
	}
	var targets []CertTarget
	for _, st := range live {
		verify := true
		if c, ok := own[st.ID]; ok && !c.Trusted {
			verify = false
		}
		for _, d := range append(append([]string{}, st.Domains...), st.RedirectDomains...) {
			targets = append(targets, CertTarget{Site: st, Domain: d, VerifyChain: verify})
		}
	}
	s.mu.Lock()
	var due []CertTarget
	wanted := map[string]bool{}
	for _, t := range targets {
		wanted[t.Domain] = true
		c, ok := s.certs[t.Domain]
		every := certInterval
		if ok && (c.res.Invalid != "" || c.res.Served && c.res.NotAfter.Sub(now) < time.Duration(set.CertWarnDays)*24*time.Hour) {
			every = certBadInterval
		}
		if !ok || now.Sub(c.at) >= every || c.siteID != t.Site.ID {
			due = append(due, t)
		}
	}
	for d := range s.certs {
		if !wanted[d] {
			delete(s.certs, d)
		}
	}
	s.mu.Unlock()
	each(due, func(t CertTarget) {
		res := s.CertCheck(ctx, t)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.certs[t.Domain] = certState{at: now, siteID: t.Site.ID, res: res}
		s.mu.Unlock()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]finding, 0, len(targets))
	for _, t := range targets {
		c, ok := s.certs[t.Domain]
		f := certFinding(now, set, t.Domain, c.res)
		f.siteID = t.Site.ID
		f.unknown = f.unknown || !ok
		out = append(out, f)
	}
	return out, nil
}

func certFinding(now time.Time, set Settings, domain string, r CertResult) finding {
	f := finding{key: KindCert + ":" + domain, kind: KindCert, target: domain}
	if !r.Served {
		f.unknown = true
		return f
	}
	left := r.NotAfter.Sub(now)
	days := int(left.Hours() / 24)
	date := r.NotAfter.UTC().Format(time.DateOnly)
	switch {
	case left <= 0:
		f.severity, f.message = SevCritical, fmt.Sprintf("The certificate of %s expired on %s", domain, date)
	case r.Invalid != "":
		f.severity, f.message = SevCritical, fmt.Sprintf("The certificate served for %s is invalid: %s", domain, oneLine(r.Invalid, 300))
	case left < time.Duration(set.CertCriticalDays)*24*time.Hour:
		f.severity, f.message = SevCritical, fmt.Sprintf("The certificate of %s expires in %d day(s), on %s", domain, days, date)
	case left < time.Duration(set.CertWarnDays)*24*time.Hour:
		f.severity, f.message = SevWarning, fmt.Sprintf("The certificate of %s expires in %d days, on %s", domain, days, date)
	default:
		f.message = fmt.Sprintf("The certificate of %s is valid until %s", domain, date)
	}
	return f
}

func diskFindings(set Settings, hosts []Host) []finding {
	var out []finding
	for _, h := range hosts {
		for _, d := range h.Disks {
			target := d.Path
			if h.Node != "" {
				target = h.Node + ":" + d.Path
			}
			f := finding{key: KindDisk + ":" + target, kind: KindDisk, target: target}
			used := d.UsedPercent()
			switch {
			case used >= set.DiskCriticalPercent:
				f.severity = SevCritical
			case used >= set.DiskWarnPercent:
				f.severity = SevWarning
			}
			f.message = fmt.Sprintf("The filesystem of %s is %.1f%% full (%s free)", target, used, humanBytes(d.Avail))
			if f.severity == "" {
				f.message = fmt.Sprintf("The filesystem of %s is %.0f%% full", target, used)
			}
			out = append(out, f)
		}
	}
	return out
}

// NodeState is a cluster node as the control plane last saw it.
type NodeState struct {
	ID       string
	Name     string
	LastSeen time.Time
	Error    string // the last health check's failure ("" = it answered)
}

// nodeDownAfter: health checks run every 30 s; a node missing a few isn't
// an outage yet (a restart, an update).
const nodeDownAfter = 3 * time.Minute

// nodeFindings: a node the control plane can't reach can't be managed, and
// its sites may be down with it.
func nodeFindings(now time.Time, nodes []NodeState) []finding {
	var out []finding
	for _, n := range nodes {
		f := finding{key: KindNode + ":" + n.ID, kind: KindNode, target: n.ID}
		switch {
		case n.LastSeen.IsZero() && n.Error == "":
			f.unknown = true // just added, not checked yet
		case now.Sub(n.LastSeen) > nodeDownAfter:
			f.severity = SevCritical
			f.message = fmt.Sprintf("Server %s (%s) hasn't answered the panel for %s", n.Name, n.ID,
				humanDuration(now.Sub(n.LastSeen)))
			if n.Error != "" {
				f.message += ": " + n.Error
			}
		default:
			f.message = fmt.Sprintf("Server %s (%s) answers the panel", n.Name, n.ID)
		}
		out = append(out, f)
	}
	return out
}

// checkBackups flags live sites whose scheduled backups have not succeeded
// for more than two intervals (one failure is retried by the scheduler).
func (s *Service) checkBackups(ctx context.Context, now time.Time, live []*store.Site) ([]finding, error) {
	policies, err := s.Store.BackupPolicies(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]*store.Site{}
	for _, st := range live {
		byID[st.ID] = st
	}
	var out []finding
	for _, p := range policies {
		st, ok := byID[p.SiteID]
		if !ok || p.IntervalHours <= 0 {
			continue
		}
		f := finding{key: KindBackup + ":" + st.ID, kind: KindBackup, siteID: st.ID, target: st.PrimaryDomain}
		last := p.LastBackupAt
		if last.IsZero() {
			last = st.CreatedAt
		}
		if age := now.Sub(last); age > 2*time.Duration(p.IntervalHours)*time.Hour {
			f.severity = SevWarning
			f.message = fmt.Sprintf("No successful backup of %s for %s (scheduled every %dh)", st.PrimaryDomain,
				humanDuration(age), p.IntervalHours)
			if !p.LastBackupAt.IsZero() {
				f.message = fmt.Sprintf("No successful backup of %s since %s (scheduled every %dh)", st.PrimaryDomain,
					p.LastBackupAt.UTC().Format("2006-01-02 15:04 MST"), p.IntervalHours)
			}
			if p.LastError != "" {
				f.message += ": " + oneLine(p.LastError, 200)
			}
		} else {
			f.message = "Backups of " + st.PrimaryDomain + " are up to date"
		}
		out = append(out, f)
	}
	return out, nil
}

// refreshVulns caches each site's count of components with known
// vulnerabilities, from its latest scan report.
func (s *Service) refreshVulns(ctx context.Context, now time.Time, sites []*store.Site) {
	s.mu.Lock()
	fresh := now.Sub(s.vulnsAt) < vulnInterval
	s.mu.Unlock()
	if fresh {
		return
	}
	counts := map[string]int{}
	for _, st := range sites {
		_, report, err := s.Store.Scan(ctx, st.ID)
		if err != nil {
			continue // never scanned
		}
		var r struct {
			Vulnerable int `json:"vulnerable"`
		}
		if json.Unmarshal(report, &r) == nil {
			counts[st.ID] = r.Vulnerable
		}
	}
	s.mu.Lock()
	s.vulns, s.vulnsAt = counts, now
	s.mu.Unlock()
}

// Overview is what the API shows: firing alerts and recent history.
type Overview struct {
	Active      []store.Alert      `json:"active"`
	History     []store.AlertEvent `json:"history"`
	Watched     int                `json:"watched"`
	EvaluatedAt time.Time          `json:"evaluated_at,omitzero"`
}

// Alerts returns the firing alerts and the newest history lines.
func (s *Service) Alerts(ctx context.Context, historyLimit int) (*Overview, error) {
	s.init()
	rows, err := s.Store.Alerts(ctx)
	if err != nil {
		return nil, err
	}
	hist, err := s.Store.AlertHistory(ctx, historyLimit)
	if err != nil {
		return nil, err
	}
	o := &Overview{Active: []store.Alert{}, History: hist, Watched: len(rows)}
	for _, a := range rows {
		if a.State == store.AlertFiring {
			o.Active = append(o.Active, a)
		}
	}
	s.mu.Lock()
	o.EvaluatedAt = s.lastEval
	s.mu.Unlock()
	return o, nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
