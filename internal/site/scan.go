package site

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// Security scans: known vulnerabilities of what's installed, plus signs of
// a compromise (modified core or plugin files, PHP hidden in uploads).

type Integrity struct {
	// CoreModified lists core files that differ from wordpress.org's
	// checksums, or shouldn't exist at all.
	CoreModified []string `json:"core_modified"`
	// PluginsModified lists "plugin: file" entries that differ from the
	// wordpress.org release (premium plugins can't be checked).
	PluginsModified []string `json:"plugins_modified"`
	// UploadsPHP lists PHP files under wp-content/uploads: WordPress never
	// puts code there, attackers do (Caddy refuses to run it regardless).
	UploadsPHP []string `json:"uploads_php"`
}

type ScanReport struct {
	ScannedAt  time.Time  `json:"scanned_at"`
	Inventory  *Inventory `json:"inventory"`
	Integrity  Integrity  `json:"integrity"`
	Vulnerable int        `json:"vulnerable"` // components with at least one known vulnerability
	Errors     []string   `json:"errors,omitempty"`
}

const maxListed = 100

// Scan runs a security scan now and stores the report.
func (s *Service) Scan(ctx context.Context, id string) (*ScanReport, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, fmt.Errorf("%w: an update or scan is already running on this site", ErrConflict)
	}
	defer lock.Unlock()
	return s.scanLocked(ctx, st)
}

// LastScan returns the stored report of the latest scan.
func (s *Service) LastScan(ctx context.Context, id string) (*ScanReport, error) {
	_, b, err := s.Store.Scan(ctx, id)
	if err != nil {
		return nil, err
	}
	var r ScanReport
	return &r, json.Unmarshal(b, &r)
}

func (s *Service) scanLocked(ctx context.Context, st *store.Site) (*ScanReport, error) {
	rep := &ScanReport{ScannedAt: time.Now().UTC()}
	inv, err := s.Inventory(ctx, st.ID)
	if err != nil {
		return nil, fmt.Errorf("listing installed components: %w", err)
	}
	rep.Inventory = inv
	rep.Errors = append(rep.Errors, s.checkVulns(ctx, inv)...)
	for _, c := range inv.all() {
		if len(c.Vulns) > 0 {
			rep.Vulnerable++
		}
	}
	rep.Integrity, err = s.checkIntegrity(ctx, st)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
	b, _ := json.Marshal(rep)
	if err := s.Store.SaveScan(ctx, st.ID, rep.ScannedAt, b); err != nil {
		return nil, err
	}
	if msg := rep.headline(); msg != "" {
		s.event(st.ID, "security", msg)
	}
	return rep, nil
}

// checkVulns fills in Vulns and UpdateFixes for every component. Lookups run
// a few at a time; the database client caches answers across sites.
func (s *Service) checkVulns(ctx context.Context, inv *Inventory) []string {
	var mu sync.Mutex
	var errs []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, c := range inv.all() {
		if c.Version == "" {
			continue
		}
		wg.Add(1)
		go func(c *Component) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			vulns, err := s.Vulns.Lookup(ctx, c.Type, c.Slug, c.Version)
			if err == nil && len(vulns) > 0 && c.UpdateVersion != "" {
				// Does the available update fix every known issue?
				var after []Vuln
				after, err = s.Vulns.Lookup(ctx, c.Type, c.Slug, c.UpdateVersion)
				c.UpdateFixes = err == nil && len(after) == 0
			}
			mu.Lock()
			defer mu.Unlock()
			c.Vulns = vulns
			if err != nil && !slices.Contains(errs, err.Error()) {
				errs = append(errs, err.Error())
			}
		}(c)
	}
	wg.Wait()
	return errs
}

// wpMerged runs WP-CLI with stderr folded into stdout: verify-checksums
// reports each finding as a warning line and exits 1 when it has any.
func (s *Service) wpMerged(ctx context.Context, id string, args ...string) (string, error) {
	var out bytes.Buffer
	err := s.Runtime.Exec(ctx, id, nil, &out,
		append([]string{"sh", "-c", `exec "$@" 2>&1`, "sh"}, runtime.WPArgs(args...)...)...)
	return out.String(), err
}

func (s *Service) checkIntegrity(ctx context.Context, st *store.Site) (Integrity, error) {
	in := Integrity{CoreModified: []string{}, PluginsModified: []string{}, UploadsPHP: []string{}}
	var errs []string

	out, err := s.wpMerged(ctx, st.ID, "core", "verify-checksums")
	in.CoreModified = parseCoreChecksums(out)
	if err != nil && len(in.CoreModified) == 0 {
		errs = append(errs, "core checksums: "+lastLine(out, err))
	}

	var plugins []struct {
		Plugin  string `json:"plugin_name"`
		File    string `json:"file"`
		Message string `json:"message"`
	}
	// Exits 1 with "Only verified N of M plugins" when anything differs or a
	// (premium) plugin can't be checked; the JSON on stdout is what matters.
	// Any other failure (a site whose PHP no longer parses) is reported, or
	// the scan would claim nothing was modified.
	err = s.wpJSON(ctx, st.ID, &plugins, "plugin", "verify-checksums", "--all", "--format=json")
	if err != nil && len(plugins) == 0 && !strings.Contains(err.Error(), "Only verified") {
		errs = append(errs, "plugin checksums: "+err.Error())
	}
	for _, p := range plugins {
		if len(in.PluginsModified) < maxListed {
			in.PluginsModified = append(in.PluginsModified, p.Plugin+": "+p.File)
		}
	}

	var found bytes.Buffer
	// busybox find; the site's own view of its docroot (no symlink escapes).
	err = s.Runtime.Exec(ctx, st.ID, nil, &found, "find", s.Cfg.SiteRoot(st.ID)+"/wp-content/uploads",
		"-type", "f", "(", "-iname", "*.php", "-o", "-iname", "*.php[0-9]", "-o", "-iname", "*.phtml",
		"-o", "-iname", "*.phar", "-o", "-iname", "*.pht", ")")
	for _, f := range strings.Split(strings.TrimSpace(found.String()), "\n") {
		if f != "" && len(in.UploadsPHP) < maxListed {
			in.UploadsPHP = append(in.UploadsPHP, strings.TrimPrefix(f, s.Cfg.SiteRoot(st.ID)+"/"))
		}
	}
	if err != nil && !strings.Contains(err.Error(), "No such file") {
		errs = append(errs, "uploads: "+err.Error())
	}
	if len(errs) > 0 {
		return in, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return in, nil
}

// parseCoreChecksums extracts file names from `wp core verify-checksums`.
func parseCoreChecksums(out string) []string {
	files := []string{}
	for _, l := range strings.Split(out, "\n") {
		for _, prefix := range []string{"File doesn't verify against checksum: ", "File should not exist: "} {
			_, f, ok := strings.Cut(l, prefix)
			// The official WordPress image puts its env-driven sample config
			// in every docroot; WPGenie's wp-config.php is elsewhere.
			if f = strings.TrimSpace(f); ok && f != "wp-config-docker.php" && len(files) < maxListed {
				files = append(files, f)
			}
		}
	}
	return files
}

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}

// headline is the activity-log line for a scan that found something.
func (r *ScanReport) headline() string {
	var parts []string
	var vulnerable []string
	for _, c := range r.Inventory.all() {
		if len(c.Vulns) > 0 {
			label := c.Slug + " " + c.Version
			if c.UpdateFixes {
				label += " (fixed in " + c.UpdateVersion + ")"
			}
			vulnerable = append(vulnerable, label)
		}
	}
	if len(vulnerable) > 0 {
		parts = append(parts, fmt.Sprintf("%d vulnerable: %s", len(vulnerable), strings.Join(vulnerable, ", ")))
	}
	if n := len(r.Integrity.CoreModified); n > 0 {
		parts = append(parts, fmt.Sprintf("%d modified core file(s)", n))
	}
	if n := len(r.Integrity.PluginsModified); n > 0 {
		parts = append(parts, fmt.Sprintf("%d modified plugin file(s)", n))
	}
	if n := len(r.Integrity.UploadsPHP); n > 0 {
		parts = append(parts, fmt.Sprintf("%d PHP file(s) in uploads", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Security scan: " + strings.Join(parts, "; ")
}

// Auto-update policies.
const (
	AutoUpdateOff      = "off"
	AutoUpdateSecurity = "security" // only components whose update fixes a known vulnerability
	AutoUpdateAll      = "all"
)

func validAutoUpdate(p string) bool {
	return p == AutoUpdateOff || p == AutoUpdateSecurity || p == AutoUpdateAll
}

func (s *Service) SetAutoUpdate(ctx context.Context, id, policy string) (*store.Site, error) {
	if !validAutoUpdate(policy) {
		return nil, fmt.Errorf("%w: auto_update must be off, security or all", ErrInvalidInput)
	}
	if err := s.Store.SetAutoUpdate(ctx, id, policy); err != nil {
		return nil, err
	}
	return s.Store.GetSite(ctx, id)
}

// autoUpdateRequest is what a policy updates after a scan, if anything.
func autoUpdateRequest(policy string, r *ScanReport) (UpdateRequest, bool) {
	var req UpdateRequest
	for _, c := range r.Inventory.all() {
		if c.UpdateVersion == "" {
			continue
		}
		if policy == AutoUpdateAll || (policy == AutoUpdateSecurity && c.UpdateFixes) {
			switch c.Type {
			case "core":
				req.Core = true
			case "plugin":
				req.Plugins = append(req.Plugins, c.Slug)
			case "theme":
				req.Themes = append(req.Themes, c.Slug)
			}
		}
	}
	return req, req.Core || len(req.Plugins) > 0 || len(req.Themes) > 0
}

// maintenanceWindow is how long after MaintenanceHour nightly jobs may start.
const maintenanceWindow = 3 * time.Hour

func (s *Service) inMaintenanceWindow(now time.Time) bool {
	start := time.Date(now.Year(), now.Month(), now.Day(), s.Cfg.MaintenanceHour, 0, 0, 0, now.Location())
	if now.Before(start) {
		start = start.AddDate(0, 0, -1) // a window that started yesterday evening
	}
	return now.Sub(start) < maintenanceWindow
}

// RunMaintenance scans every site daily (and once right away if it was
// never scanned), and applies each site's auto-update policy. Scans and
// updates only start inside the maintenance window, except a site's very
// first scan; sites are handled one at a time to keep the load low.
func (s *Service) RunMaintenance(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		s.maintenanceTick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) maintenanceTick(ctx context.Context, now time.Time) {
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		s.Log.Error("maintenance: listing sites", "err", err)
		return
	}
	window := s.inMaintenanceWindow(now)
	for _, st := range sites {
		if ctx.Err() != nil {
			return
		}
		if st.Status != store.StatusActive {
			continue
		}
		// The nightly job (scan + auto-update) is tracked separately from the
		// last scan: a manual scan during the day must not skip the night's
		// security updates. A site never scanned gets a first scan at once,
		// retried at most every few hours if it keeps failing.
		last, _ := s.lastMaint.Load(st.ID)
		lastRun, _ := last.(time.Time)
		_, _, scanErr := s.Store.Scan(ctx, st.ID)
		nightly := window && now.Sub(lastRun) > 20*time.Hour
		first := scanErr != nil && now.Sub(lastRun) > 4*time.Hour
		if !nightly && !first {
			continue
		}
		lock := s.maintLock(st.ID)
		if !lock.TryLock() {
			continue // someone is updating it by hand right now
		}
		s.lastMaint.Store(st.ID, now)
		s.maintainSite(ctx, st, nightly)
		lock.Unlock()
	}
}

func (s *Service) maintainSite(ctx context.Context, st *store.Site, nightly bool) {
	rep, err := s.scanLocked(ctx, st)
	if err != nil {
		s.Log.Warn("scheduled scan failed", "site", st.ID, "err", err)
		return
	}
	if !nightly || st.AutoUpdate == AutoUpdateOff {
		return
	}
	req, ok := autoUpdateRequest(st.AutoUpdate, rep)
	if !ok {
		return
	}
	runID, err := s.Store.StartUpdate(ctx, st.ID, "auto")
	if err != nil {
		s.Log.Error("recording auto-update", "site", st.ID, "err", err)
		return
	}
	// Not cancelled by shutdown: stopping halfway through an update is what
	// breaks sites. (The service stop timeout still bounds it.)
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), updateTimeout)
	defer cancel()
	s.runUpdate(c, st, runID, req)
}
