package site

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// The plugin analyser: what is installed, whether wordpress.org still
// maintains it, whether its files are the published ones (modified or
// nulled copies are how many sites get backdoored), and what each plugin
// costs on a page load.

// PluginInfo is one installed plugin in an analysis report.
type PluginInfo struct {
	Slug          string `json:"slug"`
	Title         string `json:"title,omitempty"`
	Version       string `json:"version"`
	Status        string `json:"status"` // active | inactive | active-network | must-use | dropin
	UpdateVersion string `json:"update_version,omitempty"`

	// Directory is the plugin's wordpress.org status: listed, closed,
	// not_listed (premium or custom) or unknown (lookup failed).
	Directory      string    `json:"directory"`
	LastUpdated    time.Time `json:"last_updated,omitzero"`
	Abandoned      bool      `json:"abandoned,omitempty"`
	ClosedDate     string    `json:"closed_date,omitempty"`
	ClosedReason   string    `json:"closed_reason,omitempty"`
	ActiveInstalls int       `json:"active_installs,omitempty"`
	TestedUpTo     string    `json:"tested_up_to,omitempty"`

	// Checksums: verified, modified, unavailable (not a wordpress.org
	// release) or not_checked.
	Checksums  string   `json:"checksums"`
	Modified   []string `json:"modified,omitempty"`   // "file: what differs"
	Signatures []string `json:"signatures,omitempty"` // "file: marker"
	// Vulns is the number of known vulnerabilities, from the last scan.
	Vulns int `json:"vulns,omitempty"`

	Perf *PluginPerf `json:"perf,omitempty"`
	// Flags are the findings, worst first, for the panel.
	Flags []string `json:"flags"`
}

// PluginPerf is what a plugin cost on one front-page render.
type PluginPerf struct {
	LoadMS  float64 `json:"load_ms"` // including its main file's includes
	LoadKB  int     `json:"load_kb"`
	HookMS  float64 `json:"hook_ms"` // in its own hook callbacks
	Calls   int     `json:"calls"`
	Queries int     `json:"queries"`
}

// PageProfile is the whole front-page render.
type PageProfile struct {
	TotalMS      float64 `json:"total_ms"`
	PeakMemoryKB int     `json:"peak_memory_kb"`
	Queries      int     `json:"queries"`
	OutputBytes  int     `json:"output_bytes"`
	Status       int     `json:"status,omitempty"`
	Redirect     string  `json:"redirect,omitempty"`
	// Others is the theme, must-use plugins and core, by name.
	Others map[string]PluginPerf `json:"others"`
}

type PluginReport struct {
	AnalysedAt time.Time    `json:"analysed_at"`
	Plugins    []PluginInfo `json:"plugins"`
	Profile    *PageProfile `json:"profile,omitempty"`
	// ThemeSignatures are nulled/malware markers found in themes.
	ThemeSignatures []string `json:"theme_signatures,omitempty"`
	Errors          []string `json:"errors,omitempty"`
}

// abandonedAfter: a plugin whose last release is older than this is
// unlikely to get a fix when its next vulnerability is found.
const abandonedAfter = 2 * 365 * 24 * time.Hour

// AnalysePlugins runs an analysis now and stores the report.
func (s *Service) AnalysePlugins(ctx context.Context, id string) (*PluginReport, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, fmt.Errorf("%w: an update, scan or analysis is already running on this site", ErrConflict)
	}
	defer lock.Unlock()
	return s.analyseLocked(ctx, st)
}

// LastPluginReport returns the stored report of the latest analysis.
func (s *Service) LastPluginReport(ctx context.Context, id string) (*PluginReport, error) {
	_, b, err := s.Store.PluginReport(ctx, id)
	if err != nil {
		return nil, err
	}
	var r PluginReport
	return &r, json.Unmarshal(b, &r)
}

func (s *Service) analyseLocked(ctx context.Context, st *store.Site) (*PluginReport, error) {
	rep := &PluginReport{AnalysedAt: time.Now().UTC()}
	var items []struct {
		Name          string   `json:"name"`
		Title         string   `json:"title"`
		Status        string   `json:"status"`
		Version       string   `json:"version"`
		Update        wpUpdate `json:"update"`
		UpdateVersion string   `json:"update_version"`
	}
	if err := s.wpJSON(ctx, st.ID, &items, "plugin", "list", "--format=json",
		"--fields=name,title,status,version,update,update_version"); err != nil {
		return nil, fmt.Errorf("listing plugins: %w", err)
	}
	for _, it := range items {
		p := PluginInfo{Slug: it.Name, Title: it.Title, Version: it.Version, Status: it.Status, Flags: []string{}}
		if it.Update == "available" {
			p.UpdateVersion = it.UpdateVersion
		}
		rep.Plugins = append(rep.Plugins, p)
	}

	rep.Errors = append(rep.Errors, s.lookupDirectory(ctx, rep.Plugins)...)
	if err := s.checkPluginChecksums(ctx, st.ID, rep.Plugins); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
	themeSigs, err := s.findSignatures(ctx, st, rep.Plugins)
	rep.ThemeSignatures = themeSigs
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
	if prof, owners, err := s.profile(ctx, st); err != nil {
		rep.Errors = append(rep.Errors, "performance profile: "+err.Error())
	} else {
		rep.Profile = prof
		for i := range rep.Plugins {
			if perf, ok := owners["plugin:"+rep.Plugins[i].Slug]; ok {
				rep.Plugins[i].Perf = &perf
				delete(owners, "plugin:"+rep.Plugins[i].Slug)
			}
		}
		prof.Others = owners
	}
	if scan, err := s.LastScan(ctx, st.ID); err == nil && scan.Inventory != nil {
		for _, c := range scan.Inventory.Plugins {
			for i := range rep.Plugins {
				if rep.Plugins[i].Slug == c.Slug && rep.Plugins[i].Version == c.Version {
					rep.Plugins[i].Vulns = len(c.Vulns)
				}
			}
		}
	}
	for i := range rep.Plugins {
		rep.Plugins[i].Flags = pluginFlags(&rep.Plugins[i], rep.Profile, rep.AnalysedAt)
	}
	// Worst first: most flags, then slowest.
	slices.SortStableFunc(rep.Plugins, func(a, b PluginInfo) int {
		if c := cmp.Compare(severity(b), severity(a)); c != 0 {
			return c
		}
		return cmp.Compare(b.cost(), a.cost())
	})

	b, _ := json.Marshal(rep)
	if err := s.Store.SavePluginReport(ctx, st.ID, rep.AnalysedAt, b); err != nil {
		return nil, err
	}
	if msg := rep.headline(); msg != "" {
		s.event(st.ID, "security", msg)
	}
	return rep, nil
}

func (p PluginInfo) cost() float64 {
	if p.Perf == nil {
		return 0
	}
	return p.Perf.LoadMS + p.Perf.HookMS
}

// severity ranks a plugin for sorting: security findings outweigh the rest.
func severity(p PluginInfo) int {
	n := 0
	if p.Directory == "closed" {
		n += 8
	}
	if len(p.Signatures) > 0 {
		n += 8
	}
	if p.Vulns > 0 {
		n += 6
	}
	if p.Checksums == "modified" {
		n += 4
	}
	if p.Abandoned {
		n += 2
	}
	if len(p.Flags) > 0 {
		n++
	}
	return n
}

// pluginFlags lists a plugin's findings in plain words, worst first.
func pluginFlags(p *PluginInfo, prof *PageProfile, now time.Time) []string {
	flags := []string{}
	if p.Directory == "closed" {
		f := "closed on wordpress.org"
		if p.ClosedDate != "" {
			f += " since " + p.ClosedDate
		}
		if p.ClosedReason != "" {
			f += " (" + p.ClosedReason + ")"
		}
		flags = append(flags, f+": no more updates; replace it")
	}
	if len(p.Signatures) > 0 {
		flags = append(flags, "contains code seen in nulled (pirated) or infected plugins")
	}
	if p.Vulns > 0 {
		flags = append(flags, fmt.Sprintf("%d known vulnerabilit%s", p.Vulns, map[bool]string{true: "y", false: "ies"}[p.Vulns == 1]))
	}
	if p.Checksums == "modified" {
		flags = append(flags, fmt.Sprintf("%d file(s) differ from the wordpress.org release", len(p.Modified)))
	}
	if p.Abandoned {
		flags = append(flags, fmt.Sprintf("abandoned: last updated %s ago", humanAge(now.Sub(p.LastUpdated))))
	}
	if p.Directory == "not_listed" && p.Checksums != "verified" && p.Status != "must-use" && p.Status != "dropin" {
		flags = append(flags, "not on wordpress.org: files can't be verified; keep it updated from its vendor")
	}
	if p.Perf != nil && prof != nil {
		if c := p.cost(); c >= 100 || prof.TotalMS > 0 && c >= 0.25*prof.TotalMS && c >= 20 {
			flags = append(flags, fmt.Sprintf("slow: %.0f ms of a %.0f ms page", c, prof.TotalMS))
		}
	}
	if p.Status == "inactive" {
		flags = append(flags, "inactive: its files can still be reached; delete it if unused")
	}
	return flags
}

func humanAge(d time.Duration) string {
	years := d.Hours() / 24 / 365
	if years >= 1 {
		return fmt.Sprintf("%.1f years", years)
	}
	return fmt.Sprintf("%d months", int(d.Hours()/24/30))
}

// headline is the activity-log line for an analysis that found something
// worth acting on.
func (r *PluginReport) headline() string {
	var closed, abandoned, modified, nulled, slow []string
	for _, p := range r.Plugins {
		switch {
		case p.Directory == "closed":
			closed = append(closed, p.Slug)
		case p.Abandoned:
			abandoned = append(abandoned, p.Slug)
		}
		if p.Checksums == "modified" {
			modified = append(modified, p.Slug)
		}
		if len(p.Signatures) > 0 {
			nulled = append(nulled, p.Slug)
		}
		for _, f := range p.Flags {
			if strings.HasPrefix(f, "slow:") {
				slow = append(slow, p.Slug)
			}
		}
	}
	var parts []string
	add := func(what string, slugs []string) {
		if len(slugs) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", what, strings.Join(slugs, ", ")))
		}
	}
	add("closed on wordpress.org", closed)
	add("possibly nulled or infected", nulled)
	add("modified files", modified)
	add("abandoned", abandoned)
	add("slow", slow)
	if len(r.ThemeSignatures) > 0 {
		parts = append(parts, fmt.Sprintf("%d suspicious theme file(s)", len(r.ThemeSignatures)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Plugin analysis: " + strings.Join(parts, "; ")
}

// lookupDirectory asks wordpress.org about every plugin, a few at a time.
func (s *Service) lookupDirectory(ctx context.Context, plugins []PluginInfo) []string {
	if s.Directory == nil {
		for i := range plugins {
			plugins[i].Directory = "unknown"
		}
		return nil
	}
	var mu sync.Mutex
	var errs []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range plugins {
		p := &plugins[i]
		if p.Status == "must-use" || p.Status == "dropin" {
			p.Directory = "not_listed"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, err := s.Directory.Plugin(ctx, p.Slug)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				p.Directory = "unknown"
				if !slices.Contains(errs, err.Error()) {
					errs = append(errs, err.Error())
				}
				return
			}
			p.Directory, p.LastUpdated, p.ActiveInstalls, p.TestedUpTo = info.Status, info.LastUpdated, info.ActiveInstalls, info.Tested
			p.ClosedDate, p.ClosedReason = info.ClosedDate, info.ClosedReason
			p.Abandoned = info.Status == "listed" && !info.LastUpdated.IsZero() && time.Since(info.LastUpdated) > abandonedAfter
		}()
	}
	wg.Wait()
	return errs
}

// The version is empty for plugins without a Version header.
var skippedChecksumRe = regexp.MustCompile(`Could not retrieve the checksums for version .*? of plugin (\S+?),? skipping`)

// checkPluginChecksums compares every plugin with its wordpress.org
// release. WP-CLI reports differences as JSON on stdout and plugins it
// can't check (not on wordpress.org, or a version it doesn't know) as
// warnings, so both streams are read.
func (s *Service) checkPluginChecksums(ctx context.Context, id string, plugins []PluginInfo) error {
	out, err := s.wpMerged(ctx, id, "plugin", "verify-checksums", "--all", "--format=json")
	modified := map[string][]string{}
	skipped := map[string]bool{}
	completed := strings.Contains(out, "Verified ") || strings.Contains(out, "Only verified")
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			var rows []struct {
				Plugin  string `json:"plugin_name"`
				File    string `json:"file"`
				Message string `json:"message"`
			}
			// The array isn't newline-terminated: "Error: Only verified …"
			// follows on the same line. Decode just the first value.
			if json.NewDecoder(strings.NewReader(line)).Decode(&rows) == nil {
				completed = true
				for _, r := range rows {
					if len(modified[r.Plugin]) < maxListed {
						modified[r.Plugin] = append(modified[r.Plugin], r.File+": "+r.Message)
					}
				}
			}
		}
		if m := skippedChecksumRe.FindStringSubmatch(line); m != nil {
			skipped[m[1]] = true
		}
	}
	for i := range plugins {
		p := &plugins[i]
		switch {
		case !completed || p.Status == "must-use" || p.Status == "dropin":
			p.Checksums = "not_checked"
		case len(modified[p.Slug]) > 0:
			p.Checksums, p.Modified = "modified", modified[p.Slug]
		case skipped[p.Slug]:
			p.Checksums = "unavailable"
		default:
			p.Checksums = "verified"
		}
	}
	if !completed {
		return fmt.Errorf("plugin checksums: %s", lastLine(out, cmp.Or(err, errors.New("no result"))))
	}
	return nil
}

// signatureRe matches markers of nulled plugin distributors and of WP-VCD,
// the malware family spread through them. Plain strings, so a match in a
// file that verifies against wordpress.org is discarded.
const signatureRe = `nulled by|null24|wpnull|nulljungle|babiato|wplocker|gpldl\.com|codelist\.cc|` +
	`wp-vcd|class\.theme-modules\.php|class\.plugin-modules\.php|div_code_name|wp-tmp\.php|tmpcontentx`

// findSignatures greps plugin and theme PHP files for signatureRe. Plugin
// hits go on the plugin (unless its files verified against wordpress.org);
// theme hits are returned.
func (s *Service) findSignatures(ctx context.Context, st *store.Site, plugins []PluginInfo) ([]string, error) {
	root := s.Cfg.SiteRoot(st.ID) + "/wp-content"
	var out bytes.Buffer
	// busybox find/grep in the site's own view of its docroot. -m bounds the
	// output per file; missing directories (no mu-plugins) are fine.
	err := s.Runtime.Exec(ctx, st.ID, nil, &limitWriter{w: &out, n: 256 << 10}, "sh", "-c",
		`re="$1"; shift; for d in "$@"; do [ -d "$d" ] || continue; `+
			`find "$d" -type f -name '*.php' -size -4096k -exec grep -o -i -E -H -m 3 "$re" {} + ; done; exit 0`,
		"sh", signatureRe, root+"/plugins", root+"/mu-plugins", root+"/themes")
	if err != nil {
		return nil, fmt.Errorf("signature scan: %w", err)
	}
	var themes []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		file, match, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		_, rel, ok := strings.Cut(file, "/wp-content/")
		if !ok {
			continue
		}
		hit := rel + ": " + strings.ToLower(match)
		kind, rest, _ := strings.Cut(rel, "/")
		slug, _, _ := strings.Cut(rest, "/")
		slug = strings.TrimSuffix(slug, ".php")
		switch kind {
		case "plugins", "mu-plugins":
			for i := range plugins {
				p := &plugins[i]
				if p.Slug == slug && p.Checksums != "verified" && !slices.Contains(p.Signatures, hit) && len(p.Signatures) < 20 {
					p.Signatures = append(p.Signatures, hit)
				}
			}
		case "themes":
			if !slices.Contains(themes, hit) && len(themes) < maxListed {
				themes = append(themes, hit)
			}
		}
	}
	return themes, nil
}

// limitWriter keeps the first n bytes and discards the rest.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

const (
	profileScript = "/usr/local/share/wpgenie/profile.php"
	profileMarker = "@@WPGENIE-PROFILE@@"
	noProfiler    = 99
)

// profile renders the front page with profile.php (see there), in the web
// jail like cron. It runs twice with an opcache file cache in the
// container's tmpfs: the first run compiles every file, the second measures
// what FPM (which has opcache) would spend. The cache is removed afterwards.
func (s *Service) profile(ctx context.Context, st *store.Site) (*PageProfile, map[string]PluginPerf, error) {
	var out bytes.Buffer
	err := s.Runtime.Exec(ctx, st.ID, nil, &limitWriter{w: &out, n: 4 << 20},
		"env", "PHP_INI_SCAN_DIR=:/usr/local/etc/php/jail.d", "HTTP_HOST="+st.PrimaryDomain,
		"sh", "-c", `[ -f "$1" ] && [ -f "$2" ] || exit `+fmt.Sprint(noProfiler)+`
d=$(mktemp -d /tmp/wpg-profile.XXXXXX) || exit 1
trap 'rm -rf "$d"' EXIT
set -- -d opcache.enable_cli=1 -d opcache.file_cache="$d" -d opcache.file_cache_only=1 -d display_errors=0 "$2" "$3"
timeout 90 php "$@" >/dev/null 2>&1
timeout 90 php "$@"`,
		"sh", "/usr/local/etc/php/jail.d/zz-jail.ini", profileScript, s.Cfg.SiteRoot(st.ID))
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.ExitCode() == noProfiler {
		return nil, nil, errors.New("the site runs an older PHP image; roll it onto the current one (wpgenie site scale " + st.ID + ")")
	}
	_, report, found := strings.Cut(out.String(), "\n"+profileMarker+"\n")
	if !found {
		if err == nil {
			err = errors.New("no report: the page did not finish rendering")
		}
		return nil, nil, err
	}
	report, _, _ = strings.Cut(report, "\n")
	var raw struct {
		Version      int     `json:"version"`
		TotalMS      float64 `json:"total_ms"`
		PeakMemoryKB int     `json:"peak_memory_kb"`
		Queries      int     `json:"queries"`
		OutputBytes  int     `json:"output_bytes"`
		Status       int     `json:"status"`
		Redirect     *struct {
			Location string `json:"location"`
		} `json:"redirect"`
		Owners map[string]PluginPerf `json:"owners"`
	}
	// Plugin code ran to produce this: treat it as untrusted input.
	if err := json.Unmarshal([]byte(report), &raw); err != nil || raw.Version != 1 || len(raw.Owners) > 1000 {
		return nil, nil, fmt.Errorf("unreadable report")
	}
	prof := &PageProfile{TotalMS: raw.TotalMS, PeakMemoryKB: raw.PeakMemoryKB, Queries: raw.Queries,
		OutputBytes: raw.OutputBytes, Status: raw.Status}
	if raw.Redirect != nil {
		prof.Redirect = truncate(raw.Redirect.Location, 200)
	}
	return prof, raw.Owners, nil
}

// PluginDirectory looks plugins up in the wordpress.org directory.
type PluginDirectory interface {
	Plugin(ctx context.Context, slug string) (DirectoryInfo, error)
}

// DirectoryInfo is what wordpress.org says about a plugin.
type DirectoryInfo struct {
	Status         string // listed | closed | not_listed
	LastUpdated    time.Time
	ActiveInstalls int
	Tested         string
	ClosedDate     string
	ClosedReason   string
}

// WordPressOrg queries the public plugin API (no key). Answers are cached:
// many sites share the same plugins.
type WordPressOrg struct {
	BaseURL   string // default https://api.wordpress.org
	UserAgent string
	Client    *http.Client
	TTL       time.Duration // default 12h

	mu    sync.Mutex
	cache map[string]cachedDir
}

type cachedDir struct {
	at   time.Time
	info DirectoryInfo
}

func (w *WordPressOrg) Plugin(ctx context.Context, slug string) (DirectoryInfo, error) {
	if !slugRe.MatchString(slug) {
		return DirectoryInfo{Status: "not_listed"}, nil // single-file or odd names: not a directory slug
	}
	ttl := cmp.Or(w.TTL, 12*time.Hour)
	w.mu.Lock()
	if c, ok := w.cache[slug]; ok && time.Since(c.at) < ttl {
		w.mu.Unlock()
		return c.info, nil
	}
	w.mu.Unlock()

	q := url.Values{"action": {"plugin_information"}, "request[slug]": {slug}}
	for _, f := range []string{"sections", "description", "versions", "reviews", "banners", "icons", "screenshots",
		"contributors", "tags", "ratings", "compatibility", "donate_link"} {
		q.Set("request[fields]["+f+"]", "0")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		cmp.Or(w.BaseURL, "https://api.wordpress.org")+"/plugins/info/1.2/?"+q.Encode(), nil)
	if err != nil {
		return DirectoryInfo{}, err
	}
	req.Header.Set("User-Agent", w.UserAgent)
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return DirectoryInfo{}, fmt.Errorf("wordpress.org: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Error          string `json:"error"`
		Closed         bool   `json:"closed"`
		ClosedDate     string `json:"closed_date"`
		ReasonText     string `json:"reason_text"`
		LastUpdated    string `json:"last_updated"`
		ActiveInstalls int    `json:"active_installs"`
		Tested         string `json:"tested"`
	}
	// Unknown and closed plugins are 404s with a JSON body.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return DirectoryInfo{}, fmt.Errorf("wordpress.org: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return DirectoryInfo{}, fmt.Errorf("wordpress.org: %w", err)
	}
	var info DirectoryInfo
	switch {
	case body.Closed || body.Error == "closed":
		info = DirectoryInfo{Status: "closed", ClosedDate: body.ClosedDate, ClosedReason: body.ReasonText}
	case body.Error != "" || resp.StatusCode == http.StatusNotFound:
		info = DirectoryInfo{Status: "not_listed"}
	default:
		info = DirectoryInfo{Status: "listed", ActiveInstalls: body.ActiveInstalls, Tested: body.Tested}
		// "2026-09-24 10:26am GMT"
		if t, err := time.Parse("2006-01-02 3:04pm MST", body.LastUpdated); err == nil {
			info.LastUpdated = t.UTC()
		}
	}
	w.mu.Lock()
	if w.cache == nil {
		w.cache = map[string]cachedDir{}
	}
	w.cache[slug] = cachedDir{at: time.Now(), info: info}
	w.mu.Unlock()
	return info, nil
}
