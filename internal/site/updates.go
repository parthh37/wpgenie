package site

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// The update manager ("OTA" for WordPress): snapshot, update, check the
// site still works, and restore the snapshot if the update broke it. WP
// Engine's Smart Plugin Manager, on a single server.

// DBDumper snapshots and restores a site's database.
type DBDumper interface {
	Dump(ctx context.Context, db string, w io.Writer) error
	Restore(ctx context.Context, db string, r io.Reader) error
}

// Component is an installed piece of WordPress: core, a plugin or a theme.
type Component struct {
	Type          string `json:"type"` // core | plugin | theme
	Slug          string `json:"slug"`
	Status        string `json:"status,omitempty"`
	Version       string `json:"version"`
	UpdateVersion string `json:"update_version,omitempty"`
	Vulns         []Vuln `json:"vulns,omitempty"`
	// UpdateFixes: the available update has none of Vulns (set by scans).
	UpdateFixes bool `json:"update_fixes,omitempty"`
}

type Inventory struct {
	Core    Component   `json:"core"`
	Plugins []Component `json:"plugins"`
	Themes  []Component `json:"themes"`
}

func (inv *Inventory) all() []*Component {
	out := []*Component{&inv.Core}
	for i := range inv.Plugins {
		out = append(out, &inv.Plugins[i])
	}
	for i := range inv.Themes {
		out = append(out, &inv.Themes[i])
	}
	return out
}

// UpdateRequest selects what to update. All means everything with an
// update available; otherwise only the listed slugs (and core if Core).
type UpdateRequest struct {
	All     bool     `json:"all"`
	Core    bool     `json:"core"`
	Plugins []string `json:"plugins"`
	Themes  []string `json:"themes"`
}

// UpdateResult is the outcome for one component.
type UpdateResult struct {
	Type   string `json:"type"`
	Slug   string `json:"slug"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"` // updated | failed
	Error  string `json:"error,omitempty"`
}

// UpdateDetails is stored with each run (store.UpdateRun.Details).
type UpdateDetails struct {
	Results  []UpdateResult `json:"results"`
	Before   Health         `json:"before"`
	After    Health         `json:"after"`
	Restored *Health        `json:"restored,omitempty"` // health after a rollback
	Snapshot string         `json:"snapshot,omitempty"`
}

const (
	UpdateUpdated    = "updated"
	UpdateRolledBack = "rolled_back"
	UpdateFailed     = "failed"
	UpdateUpToDate   = "up_to_date"

	keepSnapshots  = 3
	updateTimeout  = 30 * time.Minute
	restoreTimeout = 15 * time.Minute
)

// wpJSON runs WP-CLI and decodes its JSON stdout. Warnings go to stderr,
// so they can't corrupt the JSON; on a non-zero exit, whatever JSON was
// printed is still decoded (e.g. per-item results of a partial update).
func (s *Service) wpJSON(ctx context.Context, id string, into any, args ...string) error {
	var out bytes.Buffer
	err := s.Runtime.Exec(ctx, id, nil, &out, runtime.WPArgs(args...)...)
	if b := bytes.TrimSpace(out.Bytes()); len(b) > 0 {
		if jerr := json.Unmarshal(b, into); jerr != nil && err == nil {
			err = fmt.Errorf("wp %s: unexpected output: %w", args[0], jerr)
		}
	}
	return err
}

// Inventory lists core, plugins and themes with their available updates,
// as wordpress.org reports them. Premium plugins that ship their own
// updater only report updates with plugins loaded, which the panel never
// does (see runtime.WPArgs): update those from wp-admin.
func (s *Service) Inventory(ctx context.Context, id string) (*Inventory, error) {
	inv := &Inventory{Core: Component{Type: "core", Slug: "wordpress"}}
	var ver bytes.Buffer
	if err := s.Runtime.Exec(ctx, id, nil, &ver, runtime.WPArgs("core", "version")...); err != nil {
		return nil, err
	}
	inv.Core.Version = strings.TrimSpace(ver.String())
	var core []struct {
		Version string `json:"version"`
	}
	if err := s.wpJSON(ctx, id, &core, "core", "check-update", "--format=json"); err != nil {
		return nil, err
	}
	for _, c := range core {
		if versionCompare(c.Version, inv.Core.UpdateVersion) > 0 {
			inv.Core.UpdateVersion = c.Version
		}
	}
	for _, kind := range []string{"plugin", "theme"} {
		var items []struct {
			Name          string `json:"name"`
			Status        string `json:"status"`
			Version       string `json:"version"`
			Update        string `json:"update"`
			UpdateVersion string `json:"update_version"`
		}
		if err := s.wpJSON(ctx, id, &items, kind, "list", "--format=json",
			"--fields=name,status,version,update,update_version"); err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Status == "must-use" || it.Status == "dropin" {
				continue // not updatable from wordpress.org
			}
			c := Component{Type: kind, Slug: it.Name, Status: it.Status, Version: it.Version}
			if it.Update == "available" {
				c.UpdateVersion = it.UpdateVersion
			}
			if kind == "plugin" {
				inv.Plugins = append(inv.Plugins, c)
			} else {
				inv.Themes = append(inv.Themes, c)
			}
		}
	}
	return inv, nil
}

// WaitUpdates waits up to timeout for running WordPress updates, so a
// daemon stop (or a WPGenie self-update restart) doesn't cut one in half.
func (s *Service) WaitUpdates(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *Service) maintLock(id string) *sync.Mutex {
	m, _ := s.maint.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// StartUpdate runs an update in the background and returns its run ID;
// progress and the outcome are in the site's update history.
func (s *Service) StartUpdate(ctx context.Context, id string, req UpdateRequest, trigger string) (int64, error) {
	if err := validateUpdateRequest(req); err != nil {
		return 0, err
	}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return 0, err
	}
	if st.Status != store.StatusActive {
		return 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return 0, fmt.Errorf("%w: an update or scan is already running on this site", ErrConflict)
	}
	runID, err := s.Store.StartUpdate(ctx, id, trigger)
	if err != nil {
		lock.Unlock()
		return 0, err
	}
	go func() {
		defer lock.Unlock()
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), updateTimeout)
		defer cancel()
		s.runUpdate(c, st, runID, req)
	}()
	return runID, nil
}

func validateUpdateRequest(req UpdateRequest) error {
	if !req.All && !req.Core && len(req.Plugins) == 0 && len(req.Themes) == 0 {
		return fmt.Errorf("%w: nothing selected to update", ErrInvalidInput)
	}
	for _, slug := range slices.Concat(req.Plugins, req.Themes) {
		if !slugRe.MatchString(slug) {
			return fmt.Errorf("%w: invalid slug %q", ErrInvalidInput, slug)
		}
	}
	return nil
}

// plan picks the components the request asks for that have an update.
func plan(inv *Inventory, req UpdateRequest) []Component {
	var out []Component
	for _, c := range inv.all() {
		if c.UpdateVersion == "" {
			continue
		}
		want := req.All
		switch c.Type {
		case "core":
			want = want || req.Core
		case "plugin":
			want = want || slices.Contains(req.Plugins, c.Slug)
		case "theme":
			want = want || slices.Contains(req.Themes, c.Slug)
		}
		if want {
			out = append(out, *c)
		}
	}
	return out
}

// runUpdate does the whole update and records the outcome. Caller holds the
// site's maintenance lock.
func (s *Service) runUpdate(ctx context.Context, st *store.Site, runID int64, req UpdateRequest) {
	s.inflight.Add(1)
	defer s.inflight.Done()
	status, summary, details := s.update(ctx, st, req)
	b, _ := json.Marshal(details)
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.Store.FinishUpdate(fctx, runID, status, summary, string(b)); err != nil {
		s.Log.Error("recording update result", "site", st.ID, "err", err)
	}
	if status != UpdateUpToDate {
		s.event(st.ID, "update", summary)
	}
	s.Log.Info("update finished", "site", st.ID, "status", status, "summary", summary)
}

func (s *Service) update(ctx context.Context, st *store.Site, req UpdateRequest) (string, string, UpdateDetails) {
	var d UpdateDetails
	inv, err := s.Inventory(ctx, st.ID)
	if err != nil {
		return UpdateFailed, "Could not list installed components: " + err.Error(), d
	}
	todo := plan(inv, req)
	if len(todo) == 0 {
		return UpdateUpToDate, "Everything selected is already up to date.", d
	}

	d.Before = s.Prober.Probe(ctx, st.PrimaryDomain)
	snap, err := s.takeSnapshot(ctx, st)
	if err != nil {
		return UpdateFailed, "Snapshot failed, nothing was changed: " + err.Error(), d
	}
	d.Snapshot = filepath.Base(snap.dir)

	d.Results = s.applyUpdates(ctx, st.ID, todo)
	// Checking and, if needed, restoring must happen even if the update
	// itself ran out of time or the daemon is shutting down: a rollback on
	// an already-cancelled context would leave a broken site behind.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	ctx = rctx
	if err := s.Purge(ctx, st.ID); err != nil {
		s.Log.Warn("purging caches after update", "site", st.ID, "err", err)
	}
	d.After = s.Prober.Probe(ctx, st.PrimaryDomain)
	s.pruneSnapshots(st.ID)

	changed := describe(d.Results)
	switch {
	case d.Before.OK && !d.After.OK:
		// The update broke a working site: put it back.
		if err := s.restoreSnapshot(ctx, st, snap); err != nil {
			return UpdateFailed, fmt.Sprintf("The update broke the site (%s) and restoring snapshot %s FAILED: %v. "+
				"The site needs attention.", d.After.Detail, d.Snapshot, err), d
		}
		restored := s.Prober.Probe(ctx, st.PrimaryDomain)
		d.Restored = &restored
		return UpdateRolledBack, fmt.Sprintf("Rolled back: after updating %s the site failed its health check (%s). "+
			"Snapshot %s was restored.", changed, d.After.Detail, d.Snapshot), d
	case failedCount(d.Results) == len(d.Results):
		return UpdateFailed, "No component could be updated: " + changed, d
	}
	summary := "Updated " + changed + "."
	switch {
	case !d.Before.Checked:
		summary += " The site couldn't be health-checked (" + d.Before.Detail + "), so the update was not verified."
	case !d.Before.OK:
		summary += " The site was already failing its health check before the update (" + d.Before.Detail + ")."
	}
	return UpdateUpdated, summary, d
}

// applyUpdates updates core first (plugins may need the new core), then
// plugins, then themes.
func (s *Service) applyUpdates(ctx context.Context, id string, todo []Component) []UpdateResult {
	var results []UpdateResult
	byKind := map[string][]Component{}
	for _, c := range todo {
		byKind[c.Type] = append(byKind[c.Type], c)
	}
	if core := byKind["core"]; len(core) > 0 {
		r := UpdateResult{Type: "core", Slug: "wordpress", From: core[0].Version, To: core[0].UpdateVersion, Status: "updated"}
		var out bytes.Buffer
		err := s.Runtime.Exec(ctx, id, nil, &out, runtime.WPArgs("core", "update")...)
		if err == nil {
			err = s.Runtime.Exec(ctx, id, nil, &out, runtime.WPArgs("core", "update-db")...)
		}
		if err != nil {
			r.Status, r.Error = "failed", err.Error()
		}
		results = append(results, r)
	}
	for _, kind := range []string{"plugin", "theme"} {
		items := byKind[kind]
		if len(items) == 0 {
			continue
		}
		args := []string{kind, "update", "--format=json", "--quiet"}
		for _, c := range items {
			args = append(args, c.Slug)
		}
		var res []wpUpdateItem
		err := s.wpJSON(ctx, id, &res, args...)
		for _, c := range items {
			r := UpdateResult{Type: kind, Slug: c.Slug, From: c.Version, To: c.UpdateVersion, Status: "failed"}
			i := slices.IndexFunc(res, func(x wpUpdateItem) bool { return x.Name == c.Slug })
			switch {
			case i >= 0 && res[i].Status == "Updated":
				r.Status, r.To = "updated", res[i].NewVersion
			case i >= 0:
				r.Error = res[i].Status
			case err != nil:
				r.Error = err.Error()
			default:
				r.Error = "not reported by WP-CLI"
			}
			results = append(results, r)
		}
	}
	return results
}

// wpUpdateItem is one line of `wp plugin|theme update --format=json`.
type wpUpdateItem struct {
	Name       string `json:"name"`
	OldVersion string `json:"old_version"`
	NewVersion string `json:"new_version"`
	Status     string `json:"status"` // "Updated" or an error
}

func failedCount(rs []UpdateResult) int {
	n := 0
	for _, r := range rs {
		if r.Status != "updated" {
			n++
		}
	}
	return n
}

func describe(rs []UpdateResult) string {
	var parts []string
	for _, r := range rs {
		p := fmt.Sprintf("%s %s → %s", r.Slug, r.From, r.To)
		if r.Status != "updated" {
			p = fmt.Sprintf("%s %s (failed: %s)", r.Slug, r.From, truncate(r.Error, 120))
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// Snapshots: <data>/snapshots/<site>/<UTC timestamp>/ with files.tar.gz (the
// WordPress install minus uploads and caches, which updates never touch),
// db.sql.gz and meta.json. Root-only: they contain the whole database.

type snapshot struct {
	dir    string
	Tables []string `json:"tables"`
}

func (s *Service) snapshotRoot(id string) string {
	return filepath.Join(s.Cfg.DataDir, "snapshots", id)
}

// Tar runs inside the site's container as the site user: see restoreScript.
var snapshotArgs = []string{"tar", "-czf", "-",
	"--exclude=./wp-content/uploads", "--exclude=./wp-content/cache",
	"--exclude=./wp-content/upgrade", "--exclude=./.maintenance", "-C"}

func (s *Service) takeSnapshot(ctx context.Context, st *store.Site) (_ *snapshot, err error) {
	dir := filepath.Join(s.snapshotRoot(st.ID), time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	snap := &snapshot{dir: dir}
	if err := writeFile(filepath.Join(dir, "files.tar.gz"), func(w io.Writer) error {
		return s.Runtime.Exec(ctx, st.ID, nil, w, append(snapshotArgs, s.Cfg.SiteRoot(st.ID), ".")...)
	}); err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	if err := writeFile(filepath.Join(dir, "db.sql.gz"), func(w io.Writer) error {
		zw := gzip.NewWriter(w)
		if err := s.Dumper.Dump(ctx, st.DBName, zw); err != nil {
			return err
		}
		return zw.Close()
	}); err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	// Listed right after the dump: a table created before this point is in
	// the dump, so a rollback must not treat it as the update's and drop it.
	if snap.Tables, err = s.DB.Tables(ctx, st.DBName); err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	meta, _ := json.Marshal(snap)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		return nil, err
	}
	return snap, nil
}

func writeFile(path string, fill func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := fill(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// restoreScript runs as the site user inside the site's container. The
// daemon is root and the docroot is writable by the site, so a root-side
// extract could be steered through planted symlinks (plugins -> /etc);
// inside the container the site can only ever reach its own mount.
//
// The snapshot is extracted into a temporary directory first, so a broken
// stream or a timeout leaves the live site untouched; only then is
// everything the snapshot covers removed (files the update added must not
// linger) and the restored entries moved into place, each with a rename.
const restoreScript = `set -e
cd "$1"
tmp=.wpgenie-restore
rm -rf "$tmp"
mkdir "$tmp"
tar -xzf - --no-same-owner -C "$tmp"
find . -mindepth 1 -maxdepth 1 ! -name wp-content ! -name "$tmp" -exec rm -rf {} +
mkdir -p wp-content
find wp-content -mindepth 1 -maxdepth 1 ! -name uploads ! -name cache -exec rm -rf {} +
for f in "$tmp"/* "$tmp"/.[!.]* "$tmp"/..?*; do
  [ -e "$f" ] || continue
  [ "${f##*/}" = wp-content ] && continue
  mv "$f" .
done
if [ -d "$tmp/wp-content" ]; then
  for f in "$tmp"/wp-content/* "$tmp"/wp-content/.[!.]* "$tmp"/wp-content/..?*; do
    [ -e "$f" ] || continue
    mv "$f" wp-content/
  done
fi
rm -rf "$tmp"`

func (s *Service) restoreSnapshot(ctx context.Context, st *store.Site, snap *snapshot) error {
	f, err := os.Open(filepath.Join(snap.dir, "files.tar.gz"))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.Runtime.Exec(ctx, st.ID, f, nil, "sh", "-c", restoreScript, "sh", s.Cfg.SiteRoot(st.ID)); err != nil {
		return fmt.Errorf("restoring files: %w", err)
	}
	// The extract recreated WPGenie's cache wrappers owned by the site;
	// put back the root-owned versions for the current settings.
	if err := s.rewriteManagedFiles(ctx, st.ID); err != nil {
		return fmt.Errorf("restoring cache wrappers: %w", err)
	}

	db, err := os.Open(filepath.Join(snap.dir, "db.sql.gz"))
	if err != nil {
		return err
	}
	defer db.Close()
	zr, err := gzip.NewReader(db)
	if err != nil {
		return err
	}
	if err := s.Dumper.Restore(ctx, st.DBName, zr); err != nil {
		return fmt.Errorf("restoring database: %w", err)
	}
	// A dump only recreates the tables it contains: drop the ones the update
	// created, or the rolled-back plugin would find its future schema.
	now, err := s.DB.Tables(ctx, st.DBName)
	if err != nil {
		return err
	}
	var extra []string
	for _, t := range now {
		if !slices.Contains(snap.Tables, t) {
			extra = append(extra, t)
		}
	}
	if len(extra) > 0 {
		if err := s.DB.DropTables(ctx, st.DBName, extra); err != nil {
			return fmt.Errorf("dropping tables created by the update: %w", err)
		}
	}
	return s.Purge(ctx, st.ID)
}

// rewriteManagedFiles removes WPGenie's wrappers (whatever their owner) and
// writes them again for the site's current settings, read now: they may
// have changed since the update started.
func (s *Service) rewriteManagedFiles(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(s.Cfg.SiteRoot(st.ID))
	if err != nil {
		return err
	}
	for _, name := range []string{pageCacheWrapperPath, objectCacheDropIn, smtpWrapperPath} {
		if b, err := root.ReadFile(name); err == nil && bytes.Contains(b, []byte(managedMarker)) {
			if err := root.Remove(name); err != nil {
				root.Close()
				return err
			}
		}
	}
	root.Close()
	if err := s.writeSMTPWrapper(st.ID, st.SMTP); err != nil {
		return err
	}
	return s.writeCacheFiles(st.ID, CacheSettings{PageCache: st.PageCache, ObjectCache: st.ObjectCache})
}

// pruneSnapshots keeps the newest keepSnapshots of a site.
func (s *Service) pruneSnapshots(id string) {
	entries, err := os.ReadDir(s.snapshotRoot(id))
	if err != nil {
		return
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return cmp.Compare(b.Name(), a.Name()) })
	for i, e := range entries {
		if i >= keepSnapshots && e.IsDir() {
			if err := os.RemoveAll(filepath.Join(s.snapshotRoot(id), e.Name())); err != nil {
				s.Log.Warn("pruning snapshot", "site", id, "snapshot", e.Name(), "err", err)
			}
		}
	}
}
