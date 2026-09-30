package site

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/store"
)

// The site analyser: one report on a site's security, performance and
// upkeep, each finding ranked and, where WPGenie can fix it, with a
// one-click fix. It adds a live look inside WordPress (settings, database
// bloat, users) to what the nightly scan (installed versions against
// known vulnerabilities, file integrity) and plugin analysis already know.

// SiteFacts are read live from WordPress (WP-CLI, plugins skipped).
type SiteFacts struct {
	WPVersion         string   `json:"wp_version"`
	PHPVersion        string   `json:"php_version"`
	Debug             bool     `json:"debug"`
	DebugDisplay      bool     `json:"debug_display"`
	BlogPublic        bool     `json:"blog_public"`
	UsersCanRegister  bool     `json:"users_can_register"`
	DefaultRole       string   `json:"default_role"`
	Permalinks        string   `json:"permalink_structure"`
	Home              string   `json:"home"`
	Admins            []string `json:"admins"`
	AutoloadBytes     int64    `json:"autoload_bytes"`
	ExpiredTransients int      `json:"expired_transients"`
	Revisions         int      `json:"revisions"`
	AutoDrafts        int      `json:"auto_drafts"`
	Spam              int      `json:"spam"`
	DBBytes           int64    `json:"db_bytes"`
	PluginsActive     int      `json:"plugins_active"`
	PluginsInactive   int      `json:"plugins_inactive"`
	ThemesInactive    int      `json:"themes_inactive"`
	Multisite         bool     `json:"multisite"`
}

const factsPHP = `
global $wpdb;
$n = static function ( $sql ) use ( $wpdb ) { return (int) $wpdb->get_var( $sql ); };
if ( ! function_exists( 'get_plugins' ) ) {
	require_once ABSPATH . 'wp-admin/includes/plugin.php';
}
$all     = array_keys( get_plugins() );
$active  = array_intersect( $all, (array) get_option( 'active_plugins', array() ) );
$css     = get_stylesheet();
$tpl     = get_template();
$themes  = array_filter( array_keys( wp_get_themes() ), static fn( $t ) => $t !== $css && $t !== $tpl );
$debug   = defined( 'WP_DEBUG' ) && WP_DEBUG;
echo wp_json_encode( array(
	'wp_version'          => get_bloginfo( 'version' ),
	'php_version'         => PHP_VERSION,
	'debug'               => $debug,
	'debug_display'       => $debug && ( ! defined( 'WP_DEBUG_DISPLAY' ) || WP_DEBUG_DISPLAY ),
	'blog_public'         => '0' !== (string) get_option( 'blog_public' ),
	'users_can_register'  => (bool) get_option( 'users_can_register' ),
	'default_role'        => (string) get_option( 'default_role' ),
	'permalink_structure' => (string) get_option( 'permalink_structure' ),
	'home'                => home_url( '/' ),
	'admins'              => array_values( array_map( static fn( $u ) => $u->user_login, get_users( array( 'role' => 'administrator', 'fields' => array( 'user_login' ) ) ) ) ),
	'autoload_bytes'      => $n( "SELECT COALESCE(SUM(LENGTH(option_value)), 0) FROM {$wpdb->options} WHERE autoload IN ('yes', 'on', 'auto', 'auto-on')" ),
	'expired_transients'  => $n( $wpdb->prepare( "SELECT COUNT(*) FROM {$wpdb->options} WHERE option_name LIKE %s AND CAST(option_value AS UNSIGNED) < %d", $wpdb->esc_like( '_transient_timeout_' ) . '%', time() ) ),
	'revisions'           => $n( "SELECT COUNT(*) FROM {$wpdb->posts} WHERE post_type = 'revision'" ),
	'auto_drafts'         => $n( "SELECT COUNT(*) FROM {$wpdb->posts} WHERE post_status = 'auto-draft'" ),
	'spam'                => $n( "SELECT COUNT(*) FROM {$wpdb->comments} WHERE comment_approved = 'spam'" ),
	'db_bytes'            => $n( "SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.TABLES WHERE table_schema = DATABASE()" ),
	'plugins_active'      => count( $active ),
	'plugins_inactive'    => count( $all ) - count( $active ),
	'themes_inactive'     => count( $themes ),
	'multisite'           => is_multisite(),
) );
`

// Finding severities and categories.
const (
	SevCritical = "critical"
	SevWarning  = "warning"
	SevInfo     = "info"

	CatSecurity    = "security"
	CatPerformance = "performance"
	CatUpkeep      = "maintenance"
)

// Fixes the analyser can apply (ApplyFix).
const (
	FixPageCache      = "page_cache"
	FixObjectCache    = "object_cache"
	FixImages         = "images"
	FixOptimize       = "optimize"
	FixDBCleanup      = "db_cleanup"
	FixScan           = "scan"
	FixUpdateSecurity = "update_security"
	FixUpdateAll      = "update_all"
	FixAutoUpdate     = "auto_update"
	FixDefaultRole    = "default_role"
	FixSearchVisible  = "search_visible"
	FixShield         = "shield"
	FixWAF            = "waf"
)

// Finding is one thing the analyser noticed.
type Finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Category string `json:"category"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	// Fix is what ApplyFix takes to fix it ("": needs a person).
	Fix string `json:"fix,omitempty"`
}

// Analysis is the analyser's report.
type Analysis struct {
	AnalysedAt time.Time  `json:"analysed_at"`
	Score      int        `json:"score"` // 0-100
	Grade      string     `json:"grade"` // A-F
	Facts      *SiteFacts `json:"facts,omitempty"`
	// Components are core, plugins and themes with their versions,
	// available updates and known vulnerabilities (from the last scan).
	Components []Component `json:"components"`
	ScannedAt  *time.Time  `json:"scanned_at,omitempty"`
	Findings   []Finding   `json:"findings"`
	Errors     []string    `json:"errors,omitempty"`
}

// Analyse builds a site's report now: a few seconds.
func (s *Service) Analyse(ctx context.Context, id string) (*Analysis, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	a := &Analysis{AnalysedAt: now, Components: []Component{}}
	if st.Status == store.StatusActive {
		f, err := s.siteFacts(ctx, id)
		if err != nil {
			a.Errors = append(a.Errors, "reading WordPress: "+err.Error())
		}
		a.Facts = f
	} else {
		a.Errors = append(a.Errors, fmt.Sprintf("the site is %s: only its settings were checked", st.Status))
	}
	scan, err := s.LastScan(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.Errors = append(a.Errors, "reading the last scan: "+err.Error())
	}
	if scan != nil {
		at := scan.ScannedAt
		a.ScannedAt = &at
		if scan.Inventory != nil {
			a.Components = scan.Inventory.components()
		}
	}
	plugins, err := s.LastPluginReport(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.Errors = append(a.Errors, "reading the plugin analysis: "+err.Error())
	}
	a.Findings = analyse(st, a.Facts, scan, plugins, now)
	a.Score, a.Grade = score(a.Findings)
	return a, nil
}

// components is every component by value, core first.
func (inv *Inventory) components() []Component {
	out := []Component{}
	for _, c := range inv.all() {
		out = append(out, *c)
	}
	return out
}

func (s *Service) siteFacts(ctx context.Context, id string) (*SiteFacts, error) {
	var out bytes.Buffer
	if err := s.Runtime.Exec(ctx, id, nil, &out, runtime.WPArgs("eval", factsPHP)...); err != nil {
		return nil, err
	}
	var f SiteFacts
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &f); err != nil {
		return nil, fmt.Errorf("unexpected output: %w", err)
	}
	return &f, nil
}

// Weights of a finding in the score.
var severityWeight = map[string]int{SevCritical: 20, SevWarning: 8, SevInfo: 2}

func score(fs []Finding) (int, string) {
	n := 100
	for _, f := range fs {
		n -= severityWeight[f.Severity]
	}
	n = max(n, 0)
	switch {
	case n >= 90:
		return n, "A"
	case n >= 75:
		return n, "B"
	case n >= 60:
		return n, "C"
	case n >= 40:
		return n, "D"
	}
	return n, "F"
}

const (
	mb               = 1 << 20
	scanStaleAfter   = 48 * time.Hour
	slowRenderMS     = 800
	manyPlugins      = 40
	bloatRevisions   = 2000
	bloatTransients  = 500
	bloatSpam        = 1000
	bloatAutoDrafts  = 200
	autoloadWarn     = 1 * mb
	autoloadCritical = 3 * mb
)

// analyse turns what is known about a site into findings, worst first.
// Pure: facts, scan and plugins may each be nil (not available).
func analyse(st *store.Site, f *SiteFacts, scan *ScanReport, plugins *PluginReport, now time.Time) []Finding {
	var out []Finding
	add := func(id, sev, cat, title, detail, fix string) {
		out = append(out, Finding{ID: id, Severity: sev, Category: cat, Title: title, Detail: detail, Fix: fix})
	}
	live := st.ParentID == ""

	// --- Security: known vulnerabilities and signs of a compromise.
	if scan == nil {
		add("no-scan", SevWarning, CatSecurity, "Never scanned for vulnerabilities",
			"Scans check every installed version against the WPVulnerability database and verify core and plugin files.", FixScan)
	} else {
		if now.Sub(scan.ScannedAt) > scanStaleAfter {
			add("stale-scan", SevInfo, CatSecurity, "The last vulnerability scan is more than two days old",
				"Sites are scanned nightly; this one hasn't been lately.", FixScan)
		}
		if scan.Inventory != nil {
			for _, c := range scan.Inventory.all() {
				if len(c.Vulns) == 0 {
					continue
				}
				sev := SevWarning
				titles := make([]string, 0, len(c.Vulns))
				for _, v := range c.Vulns {
					if v.Severity == "critical" || v.Severity == "high" || v.Unfixed {
						sev = SevCritical
					}
					titles = append(titles, v.Title)
				}
				name := c.Slug
				if c.Type == "core" {
					name = "WordPress"
				}
				detail := strings.Join(titles, "; ")
				fix := ""
				if c.UpdateFixes {
					detail = fmt.Sprintf("Update %s to %s to fix: %s", c.Version, c.UpdateVersion, detail)
					fix = FixUpdateSecurity
				} else if c.UpdateVersion == "" {
					detail = "No fixed version available yet: consider deactivating it. " + detail
				}
				add("vuln:"+c.Type+":"+c.Slug, sev, CatSecurity,
					fmt.Sprintf("%s %s has %d known vulnerabilit%s", name, c.Version, len(c.Vulns), plural(len(c.Vulns), "y", "ies")),
					detail, fix)
			}
			var outdated []string
			for _, c := range scan.Inventory.all() {
				if c.UpdateVersion != "" && len(c.Vulns) == 0 {
					name := c.Slug
					if c.Type == "core" {
						name = "WordPress"
					}
					outdated = append(outdated, fmt.Sprintf("%s %s → %s", name, c.Version, c.UpdateVersion))
				}
			}
			if len(outdated) > 0 {
				sev := SevInfo
				if len(outdated) >= 5 || scan.Inventory.Core.UpdateVersion != "" {
					sev = SevWarning
				}
				add("updates", sev, CatUpkeep, fmt.Sprintf("%d update%s available", len(outdated), plural(len(outdated), "", "s")),
					strings.Join(outdated, ", ")+". Updates run with a snapshot and roll back if the site breaks.", FixUpdateAll)
			}
		}
		in := scan.Integrity
		if len(in.CoreModified) > 0 {
			add("core-modified", SevCritical, CatSecurity, "WordPress core files were modified",
				"Differ from wordpress.org's release: "+listSome(in.CoreModified, 5)+". Restore a backup or reinstall core.", "")
		}
		if len(in.UploadsPHP) > 0 {
			add("uploads-php", SevCritical, CatSecurity, "PHP files in the uploads folder",
				"WordPress never puts code there, attackers do: "+listSome(in.UploadsPHP, 5)+".", "")
		}
		if len(in.PluginsModified) > 0 {
			add("plugins-modified", SevWarning, CatSecurity, "Plugin files differ from their wordpress.org release",
				listSome(in.PluginsModified, 5)+". A modified or nulled copy is how many sites get backdoored.", "")
		}
	}
	if plugins != nil {
		for _, p := range plugins.Plugins {
			switch {
			case p.Directory == "closed":
				add("closed:"+p.Slug, SevCritical, CatSecurity, p.Slug+" was closed on wordpress.org",
					strings.TrimSpace("Closed plugins never get another fix. "+p.ClosedReason)+" Replace or remove it.", "")
			case p.Abandoned:
				add("abandoned:"+p.Slug, SevWarning, CatUpkeep, p.Slug+" looks abandoned",
					"No release in over two years: it's unlikely to be fixed when its next vulnerability is found.", "")
			}
			if len(p.Signatures) > 0 {
				add("nulled:"+p.Slug, SevCritical, CatSecurity, p.Slug+" contains code of nulled-plugin distributors",
					listSome(p.Signatures, 3), "")
			}
		}
		if len(plugins.ThemeSignatures) > 0 {
			add("nulled-theme", SevCritical, CatSecurity, "A theme contains nulled or malware code", listSome(plugins.ThemeSignatures, 3), "")
		}
		if p := plugins.Profile; p != nil && p.TotalMS > slowRenderMS {
			add("slow-render", SevWarning, CatPerformance, fmt.Sprintf("The front page takes %.0f ms to render", p.TotalMS),
				fmt.Sprintf("%d database queries; the heaviest plugins: %s. The page cache hides this from most visitors, not from signed-in users or carts.",
					p.Queries, heaviestPlugins(plugins, 3)), "")
		}
	}

	// --- Security: settings in the panel.
	if st.ShieldMode == string(shield.ModeOff) {
		add("shield-off", SevWarning, CatSecurity, "The shield is off",
			"No bot blocking, rate limits or login throttling on this site.", FixShield)
	}
	if !st.WAF {
		add("waf-off", SevWarning, CatSecurity, "The firewall is off",
			"Requests aren't checked for SQL injection, path traversal and other attacks.", FixWAF)
	}
	if st.AutoUpdate == AutoUpdateOff && live {
		add("auto-update-off", SevWarning, CatSecurity, "Automatic security updates are off",
			"Updates that fix known vulnerabilities aren't applied at night.", FixAutoUpdate)
	}

	// --- Inside WordPress.
	if f != nil {
		if f.UsersCanRegister && f.DefaultRole == "administrator" {
			add("register-admin", SevCritical, CatSecurity, "Anyone can register as an administrator",
				"Registration is open and new users get the administrator role.", FixDefaultRole)
		} else if f.UsersCanRegister && f.DefaultRole != "subscriber" && f.DefaultRole != "customer" {
			add("register-role", SevWarning, CatSecurity, "New registrations get the "+f.DefaultRole+" role",
				"Anyone can register and publish or edit content.", FixDefaultRole)
		}
		if slices.Contains(f.Admins, "admin") {
			add("admin-user", SevWarning, CatSecurity, `An administrator is called "admin"`,
				"The first name every password-guessing bot tries. Create another administrator and delete this one.", "")
		}
		if len(f.Admins) > 5 {
			add("many-admins", SevInfo, CatSecurity, fmt.Sprintf("%d administrators", len(f.Admins)),
				"Every administrator is a way in: give people the least role they need.", "")
		}
		if f.DebugDisplay {
			add("debug-display", SevWarning, CatSecurity, "PHP errors are shown to visitors",
				"WP_DEBUG is on with WP_DEBUG_DISPLAY: errors reveal paths and code. Turn it off in wp-config.php (below WPGenie's section).", "")
		}
		if f.PluginsInactive > 0 {
			add("inactive-plugins", SevInfo, CatSecurity, fmt.Sprintf("%d inactive plugin%s", f.PluginsInactive, plural(f.PluginsInactive, "", "s")),
				"Inactive plugins can still be attacked through their files: delete the ones you don't use.", "")
		}
		if f.ThemesInactive > 1 {
			add("inactive-themes", SevInfo, CatSecurity, fmt.Sprintf("%d unused themes", f.ThemesInactive),
				"Keep one default theme as a fallback and delete the rest.", "")
		}
		if live && !f.BlogPublic {
			add("noindex", SevWarning, CatUpkeep, "Search engines are asked not to index the site",
				`"Discourage search engines" is on in Settings → Reading.`, FixSearchVisible)
		}
		if f.Permalinks == "" {
			add("plain-permalinks", SevInfo, CatUpkeep, "Plain permalinks (?p=123)",
				"Readable URLs are better for visitors and search engines: Settings → Permalinks.", "")
		}
		switch {
		case f.AutoloadBytes > autoloadCritical:
			add("autoload", SevWarning, CatPerformance, fmt.Sprintf("%s of options load on every request", fmtMB(f.AutoloadBytes)),
				"Autoloaded options are read on every page view, cached or not in the object cache. Often left behind by removed plugins.", "")
		case f.AutoloadBytes > autoloadWarn:
			add("autoload", SevInfo, CatPerformance, fmt.Sprintf("%s of options load on every request", fmtMB(f.AutoloadBytes)),
				"Autoloaded options above 1 MB slow every page view down.", "")
		}
		var bloat []string
		if f.ExpiredTransients > bloatTransients {
			bloat = append(bloat, fmt.Sprintf("%d expired transients", f.ExpiredTransients))
		}
		if f.Revisions > bloatRevisions {
			bloat = append(bloat, fmt.Sprintf("%d revisions", f.Revisions))
		}
		if f.Spam > bloatSpam {
			bloat = append(bloat, fmt.Sprintf("%d spam comments", f.Spam))
		}
		if f.AutoDrafts > bloatAutoDrafts {
			bloat = append(bloat, fmt.Sprintf("%d auto-drafts", f.AutoDrafts))
		}
		if len(bloat) > 0 {
			add("db-bloat", SevInfo, CatPerformance, "The database carries clutter", strings.Join(bloat, ", ")+".", FixDBCleanup)
		}
		if f.PluginsActive > manyPlugins {
			add("many-plugins", SevInfo, CatPerformance, fmt.Sprintf("%d active plugins", f.PluginsActive),
				"Each one adds code to every uncached request. The plugin analysis shows what each costs.", "")
		}
		if f.Multisite {
			add("multisite", SevInfo, CatUpkeep, "Multisite network",
				"Signing in, branding and the analyser work on the main site.", "")
		}
	}

	// --- Performance: what WPGenie does for the site.
	if !st.PageCache {
		add("page-cache-off", SevWarning, CatPerformance, "The page cache is off",
			"Every page view runs PHP and the database; cached pages are served straight from disk.", FixPageCache)
	}
	if !st.ObjectCache {
		add("object-cache-off", SevWarning, CatPerformance, "The object cache is off",
			"Without Redis, WordPress repeats the same database queries on every uncached request.", FixObjectCache)
	}
	if len(st.ImageFormats) == 0 {
		add("images-off", SevInfo, CatPerformance, "Images are served as uploaded",
			"WebP copies are typically 25–35% smaller than JPEG and PNG, served to browsers that accept them.", FixImages)
	}
	var missing []string
	for _, k := range DefaultOptimizations() {
		if !slices.Contains(st.Optimize, k) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sev := SevInfo
		if len(st.Optimize) == 0 {
			sev = SevWarning
		}
		add("optimize", sev, CatPerformance, "Performance tweaks not applied",
			"Recommended: "+strings.Join(missing, ", ")+" (emoji and embed scripts, <head> clutter, Heartbeat polling, nightly database cleanup).", FixOptimize)
	}

	slices.SortStableFunc(out, func(a, b Finding) int {
		return cmp.Compare(severityRank(a.Severity), severityRank(b.Severity))
	})
	return out
}

func severityRank(s string) int {
	switch s {
	case SevCritical:
		return 0
	case SevWarning:
		return 1
	}
	return 2
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func listSome(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + fmt.Sprintf(" and %d more", len(xs)-n)
}

func heaviestPlugins(r *PluginReport, n int) string {
	ps := slices.Clone(r.Plugins)
	ps = slices.DeleteFunc(ps, func(p PluginInfo) bool { return p.Perf == nil })
	slices.SortFunc(ps, func(a, b PluginInfo) int { return cmp.Compare(b.cost(), a.cost()) })
	var names []string
	for _, p := range ps[:min(n, len(ps))] {
		names = append(names, fmt.Sprintf("%s %.0f ms", p.Slug, p.cost()))
	}
	if len(names) == 0 {
		return "not measured"
	}
	return strings.Join(names, ", ")
}

// FixResult says what a fix did.
type FixResult struct {
	Message string `json:"message"`
	// JobID or UpdateRun: the fix continues in the background.
	JobID     int64 `json:"job_id,omitempty"`
	UpdateRun int64 `json:"update_run,omitempty"`
}

// ApplyFix applies one of the analyser's fixes.
func (s *Service) ApplyFix(ctx context.Context, id, fix string) (*FixResult, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	switch fix {
	case FixPageCache, FixObjectCache:
		c := CacheSettings{PageCache: st.PageCache || fix == FixPageCache, ObjectCache: st.ObjectCache || fix == FixObjectCache}
		if _, err := s.SetCache(ctx, id, c); err != nil {
			return nil, err
		}
		if fix == FixPageCache {
			return &FixResult{Message: "Page cache on"}, nil
		}
		return &FixResult{Message: "Object cache on"}, nil
	case FixImages:
		_, job, err := s.SetImages(ctx, id, ImagesInput{Formats: []string{"webp"}})
		if err != nil {
			return nil, err
		}
		return &FixResult{Message: "Uploads are being converted to WebP", JobID: job}, nil
	case FixOptimize:
		keys := slices.Clone(st.Optimize)
		for _, k := range DefaultOptimizations() {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		if _, err := s.SetOptimize(ctx, id, OptimizeInput{Optimizations: keys}); err != nil {
			return nil, err
		}
		return &FixResult{Message: "Recommended performance tweaks applied"}, nil
	case FixDBCleanup:
		r, err := s.CleanupDatabase(ctx, id)
		if err != nil {
			return nil, err
		}
		return &FixResult{Message: fmt.Sprintf("Removed %d expired transients, %d auto-drafts, %d spam comments and %d old revisions",
			r.Transients, r.AutoDrafts, r.Spam, r.Revisions)}, nil
	case FixScan:
		if _, err := s.Scan(ctx, id); err != nil {
			return nil, err
		}
		return &FixResult{Message: "Scan finished"}, nil
	case FixUpdateSecurity, FixUpdateAll:
		req := UpdateRequest{All: true}
		if fix == FixUpdateSecurity {
			scan, err := s.LastScan(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("%w: scan the site first", ErrInvalidInput)
			}
			var ok bool
			if req, ok = autoUpdateRequest(AutoUpdateSecurity, scan); !ok {
				return nil, fmt.Errorf("%w: no update fixes a known vulnerability", ErrInvalidInput)
			}
		}
		run, err := s.StartUpdate(ctx, id, req, "analyser")
		if err != nil {
			return nil, err
		}
		return &FixResult{Message: "Update started: snapshot, update, health check", UpdateRun: run}, nil
	case FixAutoUpdate:
		if _, err := s.SetAutoUpdate(ctx, id, AutoUpdateSecurity); err != nil {
			return nil, err
		}
		return &FixResult{Message: "Automatic security updates on"}, nil
	case FixDefaultRole, FixSearchVisible:
		if err := s.requireActive(ctx, id); err != nil {
			return nil, err
		}
		args, msg := []string{"option", "update", "default_role", "subscriber"}, "New registrations now get the subscriber role"
		if fix == FixSearchVisible {
			args, msg = []string{"option", "update", "blog_public", "1"}, "Search engines may index the site again"
		}
		if _, err := s.Runtime.WP(ctx, id, nil, args...); err != nil {
			return nil, err
		}
		s.event(id, "analyser", msg)
		return &FixResult{Message: msg}, nil
	case FixShield, FixWAF:
		in := ShieldInput{Mode: shield.Mode(st.ShieldMode), BlockAIBots: st.BlockAIBots}
		msg := "Firewall on"
		if fix == FixShield {
			in.Mode, msg = shield.ModeAuto, "Shield on"
		} else {
			on := true
			in.WAF = &on
		}
		if _, err := s.SetShield(ctx, id, in); err != nil {
			return nil, err
		}
		return &FixResult{Message: msg}, nil
	}
	return nil, fmt.Errorf("%w: unknown fix %q", ErrInvalidInput, fix)
}
