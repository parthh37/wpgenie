package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWP simulates a WordPress install behind Exec: WP-CLI commands, the
// snapshot tar and the in-container restore script. Its whole state is what
// a snapshot captures, so a restore must bring every field back.
type fakeWP struct {
	mu        sync.Mutex
	Core      string            `json:"core"`
	CoreNext  string            `json:"core_next"`
	Plugins   map[string]string `json:"plugins"` // slug -> version
	Available map[string]string `json:"available"`
	Broken    bool              `json:"broken"`
	breaks    map[string]bool   // updating this plugin breaks the site
	tables    *[]string         // shared with fakeDB: updates may add tables
	calls     []string
	uploads   []string
	coreDiff  []string
}

func (w *fakeWP) exec(args []string, stdin io.Reader, stdout io.Writer) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, strings.Join(args, " "))
	switch args[0] {
	case "tar": // snapshot: the "archive" is the serialised state
		return json.NewEncoder(stdout).Encode(w)
	case "find":
		fmt.Fprint(stdout, strings.Join(w.uploads, "\n"))
		return nil
	case "sh":
		if strings.Contains(args[2], "tar -xzf -") { // restoreScript
			var snap fakeWP
			if err := json.NewDecoder(stdin).Decode(&snap); err != nil {
				return err
			}
			w.Core, w.CoreNext, w.Plugins, w.Available, w.Broken = snap.Core, snap.CoreNext, snap.Plugins, snap.Available, snap.Broken
			return nil
		}
		return w.wp(args[4:], stdout) // exec "$@" 2>&1 sh wp ...
	case "wp":
		return w.wp(args, stdout)
	}
	return fmt.Errorf("unexpected command %v", args)
}

func (w *fakeWP) wp(args []string, out io.Writer) error {
	if !slices.Equal(args[:3], []string{"wp", "--skip-themes", "--skip-plugins"}) {
		return fmt.Errorf("WP-CLI must never load plugins: %v", args)
	}
	cmd := strings.Join(args[3:5], " ")
	switch cmd {
	case "core version":
		fmt.Fprintln(out, w.Core)
	case "core check-update":
		if w.CoreNext == "" {
			fmt.Fprint(out, "[]")
		} else {
			fmt.Fprintf(out, `[{"version":%q,"update_type":"minor","package_url":"x"}]`, w.CoreNext)
		}
	case "core update":
		w.Core, w.CoreNext = w.CoreNext, ""
	case "core update-db":
	case "core verify-checksums":
		for _, f := range w.coreDiff {
			fmt.Fprintf(out, "Warning: File doesn't verify against checksum: %s\n", f)
		}
		fmt.Fprintln(out, "Warning: File should not exist: wp-config-docker.php") // from the image, always there
		if len(w.coreDiff) > 0 {
			fmt.Fprintln(out, "Error: WordPress installation doesn't verify against checksums.")
			return errors.New("exit status 1")
		}
	case "plugin list":
		var items []map[string]string
		for _, slug := range slices.Sorted(mapKeys(w.Plugins)) {
			it := map[string]string{"name": slug, "status": "active", "version": w.Plugins[slug], "update": "none", "update_version": ""}
			if v := w.Available[slug]; v != "" {
				it["update"], it["update_version"] = "available", v
			}
			items = append(items, it)
		}
		items = append(items, map[string]string{"name": "object-cache.php", "status": "dropin", "version": ""})
		return json.NewEncoder(out).Encode(items)
	case "theme list":
		fmt.Fprint(out, "[]")
	case "plugin update":
		var res []wpUpdateItem
		for _, slug := range args[7:] { // after --format=json --quiet
			res = append(res, wpUpdateItem{Name: slug, OldVersion: w.Plugins[slug], NewVersion: w.Available[slug], Status: "Updated"})
			w.Plugins[slug], w.Available[slug] = w.Available[slug], ""
			if w.breaks[slug] {
				w.Broken = true
				*w.tables = append(*w.tables, "wp_"+slug+"_v2")
			}
		}
		return json.NewEncoder(out).Encode(res)
	case "plugin verify-checksums":
		fmt.Fprint(out, `[{"plugin_name":"akismet","file":"akismet.php","message":"Checksum does not match"}]`)
		return errors.New("exit status 1: Error: Only verified 1 of 2 plugins (1 failed).")
	default:
		return fmt.Errorf("unexpected wp %v", args[3:])
	}
	return nil
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// probeWP reports the fake site's health the way HTTPProber would.
type probeWP struct {
	wp        *fakeWP
	reachable bool
}

func (p probeWP) Probe(context.Context, string) Health {
	if !p.reachable {
		return Health{Detail: "unreachable: no certificate"}
	}
	p.wp.mu.Lock()
	defer p.wp.mu.Unlock()
	if p.wp.Broken {
		return Health{Checked: true, Detail: `/: page shows "There has been a critical error on this website"`}
	}
	return Health{Checked: true, OK: true}
}

type fakeDumper struct{ db *string }

func (d fakeDumper) Dump(_ context.Context, _ string, w io.Writer, _ ...string) error {
	_, err := io.WriteString(w, *d.db)
	return err
}
func (d fakeDumper) RestoreAs(ctx context.Context, db, _, _ string, r io.Reader) error {
	return d.Restore(ctx, db, r)
}
func (d fakeDumper) Restore(_ context.Context, _ string, r io.Reader) error {
	b, err := io.ReadAll(r)
	*d.db = string(b)
	return err
}

type updateHarness struct {
	*harness
	wp *fakeWP
	db *string
}

func newUpdateHarness(t *testing.T, reachable bool) *updateHarness {
	h := newHarness(t)
	wp := &fakeWP{Core: "6.8.2", Plugins: map[string]string{"akismet": "5.3", "breaker": "1.0"},
		Available: map[string]string{"akismet": "5.4", "breaker": "2.0"},
		breaks:    map[string]bool{"breaker": true}, tables: h.db.tables}
	h.rt.exec = wp.exec
	db := "-- dump v1"
	h.svc.Dumper = fakeDumper{&db}
	h.svc.Prober = probeWP{wp, reachable}
	return &updateHarness{harness: h, wp: wp, db: &db}
}

// runSync runs an update to completion and returns its history row.
func (u *updateHarness) runSync(t *testing.T, req UpdateRequest) (string, string, UpdateDetails) {
	t.Helper()
	st, _ := u.svc.Store.GetSite(context.Background(), "s1")
	return u.svc.update(context.Background(), st, req)
}

func TestUpdateHappyPath(t *testing.T) {
	u := newUpdateHarness(t, true)
	u.svc.Store.SetCache(context.Background(), "s1", false, true, false)
	status, summary, d := u.runSync(t, UpdateRequest{Plugins: []string{"akismet"}})
	if status != UpdateUpdated || u.wp.Plugins["akismet"] != "5.4" {
		t.Fatalf("status %s (%s), akismet %s", status, summary, u.wp.Plugins["akismet"])
	}
	if summary != "Updated akismet 5.3 → 5.4." || !d.After.OK {
		t.Errorf("summary %q, after %+v", summary, d.After)
	}
	if u.wp.Plugins["breaker"] != "1.0" {
		t.Error("updated a plugin that wasn't selected")
	}
	snaps, _ := os.ReadDir(u.svc.snapshotRoot("s1"))
	if len(snaps) != 1 {
		t.Fatalf("%d snapshots, want 1", len(snaps))
	}
	for _, f := range []string{"files.tar.gz", "db.sql.gz", "meta.json"} {
		fi, err := os.Stat(filepath.Join(u.svc.snapshotRoot("s1"), snaps[0].Name(), f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("snapshot %s: %v %v (must be root-only: it holds the whole database)", f, err, fi.Mode())
		}
	}
	if !slices.Equal(*u.flushed, []string{"s1:"}) {
		t.Errorf("object cache flushes = %v: caches must be purged after an update", *u.flushed)
	}
}

func TestUpdateRollsBackWhenItBreaksTheSite(t *testing.T) {
	u := newUpdateHarness(t, true)
	// Cache wrappers exist before the update; the restore must put back
	// WPGenie's own copies.
	if err := u.svc.writeCacheFiles("s1", true, false, false); err != nil {
		t.Fatal(err)
	}
	u.svc.Store.SetCache(context.Background(), "s1", true, false, false)
	u.rt.exec = func(args []string, stdin io.Reader, stdout io.Writer) error {
		if len(args) > 3 && args[3] == "plugin" && args[4] == "update" {
			*u.db = "-- dump v2 (migrated)"
		}
		return u.wp.exec(args, stdin, stdout)
	}

	status, summary, d := u.runSync(t, UpdateRequest{All: true})
	if status != UpdateRolledBack {
		t.Fatalf("status %s: %s", status, summary)
	}
	if u.wp.Broken || u.wp.Plugins["breaker"] != "1.0" || u.wp.Plugins["akismet"] != "5.3" {
		t.Fatalf("after rollback: %+v", u.wp)
	}
	if !strings.HasPrefix(*u.db, "-- dump v1") {
		t.Fatalf("database not restored: %q", *u.db)
	}
	if slices.Contains(*u.wp.tables, "wp_breaker_v2") {
		t.Fatalf("table created by the update survived the rollback: %v", *u.wp.tables)
	}
	if d.Restored == nil || !d.Restored.OK || !strings.Contains(summary, "critical error") {
		t.Errorf("restored %+v, summary %q", d.Restored, summary)
	}
	root, _ := os.OpenRoot(u.svc.Cfg.SiteRoot("s1"))
	defer root.Close()
	if b, err := root.ReadFile(pageCacheWrapperPath); err != nil || string(b) != pageCacheWrapperFor(false) {
		t.Errorf("page cache wrapper after restore: %v", err)
	}
}

func TestUpdateNeverRollsBackWhatItCannotJudge(t *testing.T) {
	// DNS doesn't point here yet: no certificate, no health check.
	u := newUpdateHarness(t, false)
	status, summary, _ := u.runSync(t, UpdateRequest{Plugins: []string{"breaker"}})
	if status != UpdateUpdated || !strings.Contains(summary, "couldn't be health-checked") {
		t.Fatalf("unreachable site: %s %q", status, summary)
	}

	// Already broken before the update: rolling back wouldn't fix anything.
	u = newUpdateHarness(t, true)
	u.wp.Broken = true
	status, summary, _ = u.runSync(t, UpdateRequest{Plugins: []string{"akismet"}})
	if status != UpdateUpdated || u.wp.Plugins["akismet"] != "5.4" || !strings.Contains(summary, "already failing") {
		t.Fatalf("broken-before site: %s %q", status, summary)
	}
}

func TestDeleteRemovesSnapshots(t *testing.T) {
	u := newUpdateHarness(t, true)
	if status, _, _ := u.runSync(t, UpdateRequest{Plugins: []string{"akismet"}}); status != UpdateUpdated {
		t.Fatal(status)
	}
	if err := u.svc.Delete(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.svc.snapshotRoot("s1")); !os.IsNotExist(err) {
		t.Fatalf("snapshots (database dumps) of a deleted site survived: %v", err)
	}
}

func TestUpdateNothingToDo(t *testing.T) {
	u := newUpdateHarness(t, true)
	if status, _, _ := u.runSync(t, UpdateRequest{Plugins: []string{"not-installed"}}); status != UpdateUpToDate {
		t.Fatalf("status %s", status)
	}
	if snaps, _ := os.ReadDir(u.svc.snapshotRoot("s1")); len(snaps) != 0 {
		t.Error("snapshotted a site with nothing to update")
	}
}

func TestStartUpdateRecordsHistoryAndRefusesOverlap(t *testing.T) {
	u := newUpdateHarness(t, true)
	ctx := context.Background()
	u.svc.maintLock("s1").Lock()
	if _, err := u.svc.StartUpdate(ctx, "s1", UpdateRequest{All: true}, "manual"); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping update: %v", err)
	}
	u.svc.maintLock("s1").Unlock()
	if _, err := u.svc.StartUpdate(ctx, "s1", UpdateRequest{}, "manual"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty request: %v", err)
	}
	if _, err := u.svc.StartUpdate(ctx, "s1", UpdateRequest{Plugins: []string{"../etc"}}, "manual"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad slug: %v", err)
	}

	id, err := u.svc.StartUpdate(ctx, "s1", UpdateRequest{Plugins: []string{"akismet"}}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _ := u.svc.Store.Updates(ctx, "s1", 5)
		if len(runs) == 1 && runs[0].ID == id && runs[0].Status != "running" {
			if runs[0].Status != UpdateUpdated || runs[0].Trigger != "manual" {
				t.Fatalf("run = %+v", runs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("update never finished: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPruneSnapshotsKeepsNewest(t *testing.T) {
	h := newHarness(t)
	root := h.svc.snapshotRoot("s1")
	for _, n := range []string{"20260101T000000Z", "20260201T000000Z", "20260301T000000Z", "20260401T000000Z"} {
		os.MkdirAll(filepath.Join(root, n), 0o700)
	}
	h.svc.pruneSnapshots("s1")
	entries, _ := os.ReadDir(root)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"20260201T000000Z", "20260301T000000Z", "20260401T000000Z"}; !slices.Equal(names, want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
}

type fakeVulnDB map[string][]Vuln // "kind/slug@version" -> vulns

func (f fakeVulnDB) Lookup(_ context.Context, kind, slug, version string) ([]Vuln, error) {
	return f[kind+"/"+slug+"@"+version], nil
}

func TestScanAndSecurityAutoUpdate(t *testing.T) {
	u := newUpdateHarness(t, true)
	u.svc.Vulns = fakeVulnDB{
		"plugin/breaker@1.0": {{Title: "Breaker < 2.0 - Unauthenticated RCE", Severity: "critical", FixedIn: "2.0"}},
		"plugin/akismet@5.3": {{Title: "Akismet <= 5.4 - XSS", Severity: "medium"}},
		"plugin/akismet@5.4": {{Title: "Akismet <= 5.4 - XSS", Severity: "medium"}}, // the update doesn't fix it
	}
	u.wp.uploads = []string{u.svc.Cfg.SiteRoot("s1") + "/wp-content/uploads/2026/09/x.php"}
	u.wp.coreDiff = []string{"wp-includes/version.php"}

	rep, err := u.svc.Scan(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Vulnerable != 2 || len(rep.Errors) != 0 {
		t.Fatalf("vulnerable = %d, errors %v", rep.Vulnerable, rep.Errors)
	}
	if !slices.Equal(rep.Integrity.UploadsPHP, []string{"wp-content/uploads/2026/09/x.php"}) ||
		!slices.Equal(rep.Integrity.CoreModified, []string{"wp-includes/version.php"}) ||
		!slices.Equal(rep.Integrity.PluginsModified, []string{"akismet: akismet.php"}) {
		t.Fatalf("integrity = %+v", rep.Integrity)
	}
	if len(rep.Inventory.Plugins) != 2 {
		t.Errorf("drop-ins must not be inventoried: %+v", rep.Inventory.Plugins)
	}

	// "security" updates only what an update actually fixes.
	req, ok := autoUpdateRequest(AutoUpdateSecurity, rep)
	if !ok || !slices.Equal(req.Plugins, []string{"breaker"}) || req.Core {
		t.Fatalf("security policy picks %+v", req)
	}
	if req, _ := autoUpdateRequest(AutoUpdateAll, rep); len(req.Plugins) != 2 {
		t.Fatalf("all policy picks %+v", req)
	}

	last, err := u.svc.LastScan(context.Background(), "s1")
	if err != nil || last.Vulnerable != 2 {
		t.Fatalf("stored report: %+v %v", last, err)
	}
	ev, _ := u.svc.Store.Events(context.Background(), "s1", 1)
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "breaker 1.0 (fixed in 2.0)") ||
		!strings.Contains(ev[0].Message, "1 PHP file(s) in uploads") {
		t.Fatalf("scan event = %+v", ev)
	}
}

func TestMaintenanceWindow(t *testing.T) {
	h := newHarness(t)
	h.svc.Cfg.MaintenanceHour = 23
	loc := time.UTC
	for hour, want := range map[int]bool{22: false, 23: true, 0: true, 1: true, 2: false, 12: false} {
		now := time.Date(2026, 9, 29, hour, 30, 0, 0, loc)
		if got := h.svc.inMaintenanceWindow(now); got != want {
			t.Errorf("window at %02d:30 with start 23:00 = %v, want %v", hour, got, want)
		}
	}
}

func TestMaintenanceTickFirstScanAnytimeAutoUpdateInWindowOnly(t *testing.T) {
	u := newUpdateHarness(t, true)
	u.svc.Vulns = fakeVulnDB{"plugin/breaker@1.0": {{Title: "RCE", FixedIn: "2.0"}}}
	u.wp.breaks = nil
	u.svc.Cfg.MaintenanceHour = 3
	ctx := context.Background()

	// Scans are stamped with the real clock, so the ticks are relative to it.
	today := time.Now()
	u.svc.maintenanceTick(ctx, time.Date(today.Year(), today.Month(), today.Day(), 14, 0, 0, 0, time.Local))
	if _, _, err := u.svc.Store.Scan(ctx, "s1"); err != nil {
		t.Fatal("a never-scanned site must be scanned right away")
	}
	if u.wp.Plugins["breaker"] != "1.0" {
		t.Fatal("auto-updated outside the maintenance window")
	}
	later := today.AddDate(0, 0, 2)
	u.svc.maintenanceTick(ctx, time.Date(later.Year(), later.Month(), later.Day(), 3, 10, 0, 0, time.Local))
	if u.wp.Plugins["breaker"] != "2.0" || u.wp.Plugins["akismet"] != "5.3" {
		t.Fatalf("in the window, security policy: %+v", u.wp.Plugins)
	}
	runs, _ := u.svc.Store.Updates(ctx, "s1", 5)
	if len(runs) != 1 || runs[0].Trigger != "auto" || runs[0].Status != UpdateUpdated {
		t.Fatalf("runs = %+v", runs)
	}
	// Once a night: the next tick in the same window does nothing.
	scans := len(u.wp.calls)
	u.svc.maintenanceTick(ctx, time.Date(later.Year(), later.Month(), later.Day(), 3, 20, 0, 0, time.Local))
	if len(u.wp.calls) != scans {
		t.Fatal("the nightly job ran twice in one window")
	}
}

func TestMaintenanceBacksOffFailingFirstScan(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.rt.exec = func([]string, io.Reader, io.Writer) error { calls++; return errors.New("container down") }
	now := time.Date(2026, 9, 29, 14, 0, 0, 0, time.Local)
	h.svc.maintenanceTick(context.Background(), now)
	first := calls
	h.svc.maintenanceTick(context.Background(), now.Add(10*time.Minute))
	if first == 0 || calls != first {
		t.Fatalf("a failing first scan was retried after 10 minutes (%d then %d calls)", first, calls)
	}
}

func TestWPJSONIgnoresStderrNoise(t *testing.T) {
	h := newHarness(t)
	h.rt.exec = func(args []string, _ io.Reader, stdout io.Writer) error {
		io.WriteString(stdout, "  [{\"name\":\"a\"}]\n")
		return nil
	}
	var v []map[string]string
	if err := h.svc.wpJSON(context.Background(), "s1", &v, "plugin", "list"); err != nil || v[0]["name"] != "a" {
		t.Fatalf("%v %v", v, err)
	}
	h.rt.exec = func(args []string, _ io.Reader, stdout io.Writer) error {
		io.WriteString(stdout, "PHP Notice: something\n[]")
		return nil
	}
	if err := h.svc.wpJSON(context.Background(), "s1", &v, "plugin", "list"); err == nil {
		t.Fatal("garbage on stdout must be an error, not an empty list")
	}
}
