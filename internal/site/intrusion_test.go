package site

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

const (
	hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParseManifest(t *testing.T) {
	out := hashA + "  plugins/akismet/akismet.php\n" +
		hashB + "  ./themes/astra/functions.php\n" +
		"sha256sum: can't open 'x': Permission denied\n" +
		hashA + "  ../../etc/passwd\n" +
		"deadbeef  plugins/short.php\n" +
		hashB + "  mu-plugins/odd name.php\n"
	got := parseManifest([]byte(out))
	want := map[string]string{
		"plugins/akismet/akismet.php": hashA,
		"themes/astra/functions.php":  hashB,
		"mu-plugins/odd name.php":     hashB,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("parsed %v, want %v", got, want)
	}
}

func TestComponentOf(t *testing.T) {
	for p, want := range map[string]string{
		"plugins/akismet/akismet.php":     "plugin akismet",
		"plugins/hello.php":               "plugin hello",
		"themes/astra/inc/core.php":       "theme astra",
		"mu-plugins/wpgenie-optimize.php": "must-use plugins",
		"mu-plugins/x/y.php":              "must-use plugins",
	} {
		if got := componentLabel(componentOf(p)); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}

func TestDiffManifests(t *testing.T) {
	prev := &manifest{Files: map[string]string{
		"plugins/forms/forms.php":         hashA,
		"plugins/forms/inc/x.php":         hashA,
		"plugins/seo/seo.php":             hashA,
		"themes/astra/functions.php":      hashA,
		"mu-plugins/wpgenie-optimize.php": hashA,
		"mu-plugins/loader.php":           hashA,
	}}
	cur := &manifest{Files: map[string]string{
		"plugins/forms/forms.php":         hashB, // changed, no update: reported
		"plugins/forms/inc/x.php":         hashA,
		"plugins/forms/inc/shell.php":     hashA, // added: reported
		"plugins/seo/seo.php":             hashB, // updated: not reported
		"plugins/seo/new.php":             hashB,
		"themes/astra/functions.php":      hashA,
		"mu-plugins/wpgenie-optimize.php": hashB, // WPGenie's own wrapper
		"mu-plugins/loader.php":           hashB, // nothing updates mu-plugins
	}}
	explained := map[string]string{"plugin seo": "plugin seo updated 1.0 → 1.1"}
	changes, total := diffManifests(prev, cur, explained)
	want := []FileChange{
		{Path: "mu-plugins/loader.php", Change: ChangeChanged, Component: "must-use plugins"},
		{Path: "plugins/forms/forms.php", Change: ChangeChanged, Component: "plugin forms"},
		{Path: "plugins/forms/inc/shell.php", Change: ChangeAdded, Component: "plugin forms"},
	}
	if total != 3 || !slices.Equal(changes, want) {
		t.Errorf("changes (%d) %+v, want %+v", total, changes, want)
	}

	// A manifest cut short can't tell a new file from one past the cut.
	cur.Truncated = true
	changes, total = diffManifests(prev, cur, explained)
	if total != 2 || slices.ContainsFunc(changes, func(c FileChange) bool { return c.Change == ChangeAdded }) {
		t.Errorf("truncated: %d %+v", total, changes)
	}

	// The list is bounded; the total isn't.
	big := &manifest{Files: map[string]string{}}
	for i := range maxListed + 20 {
		big.Files[fmt.Sprintf("plugins/p/f%03d.php", i)] = hashA
	}
	changes, total = diffManifests(&manifest{Files: map[string]string{}}, big, nil)
	if len(changes) != maxListed || total != maxListed+20 {
		t.Errorf("bounded: %d listed of %d", len(changes), total)
	}
}

func TestExplainedChanges(t *testing.T) {
	prev := &Inventory{Plugins: []Component{{Type: "plugin", Slug: "seo", Version: "1.0"}, {Type: "plugin", Slug: "forms", Version: "2.0"}},
		Themes: []Component{{Type: "theme", Slug: "astra", Version: "4.0"}}}
	cur := &Inventory{Plugins: []Component{{Type: "plugin", Slug: "seo", Version: "1.1"}, {Type: "plugin", Slug: "forms", Version: "2.0"},
		{Type: "plugin", Slug: "shop", Version: "9.0"}}, Themes: []Component{{Type: "theme", Slug: "astra", Version: "4.0"}}}
	got := explainedChanges(prev, cur, []UpdateResult{
		{Type: "theme", Slug: "astra", From: "4.0", To: "4.0.1", Status: UpdateUpdated}, // updated, then the scan raced it
		{Type: "plugin", Slug: "forms", From: "2.0", To: "2.1", Status: UpdateFailed},
	})
	want := map[string]string{
		"plugin seo":  "plugin seo updated 1.0 → 1.1",
		"plugin shop": "plugin shop installed",
		"theme astra": "theme astra updated 4.0 → 4.0.1",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("explained %v, want %v", got, want)
	}
	if len(explainedChanges(nil, cur, nil)) != 0 {
		t.Error("nothing to compare with explains nothing")
	}
}

func TestNewAdmins(t *testing.T) {
	prev := []AdminAccount{{ID: 1, Login: "owner"}, {ID: 5, Login: "old"}}
	cur := []AdminAccount{{ID: 1, Login: "owner-renamed"}, {ID: 9, Login: "wp_update_service"}}
	if got := newAdmins(prev, cur); len(got) != 1 || got[0].ID != 9 {
		t.Errorf("new admins %+v", got)
	}
	if got := newAdmins(prev, nil); got == nil || len(got) != 0 {
		t.Errorf("none: %#v", got)
	}
}

func TestScanHeadlineReportsIntrusions(t *testing.T) {
	r := &ScanReport{Inventory: &Inventory{}, Intrusion: &Intrusion{
		NewAdmins:        []AdminAccount{{ID: 9, Login: "wp_update_service"}},
		FileChangesTotal: 2,
	}}
	h := r.headline()
	for _, want := range []string{"1 new administrator(s): wp_update_service", "2 PHP file(s) changed without an update"} {
		if !strings.Contains(h, want) {
			t.Errorf("headline %q missing %q", h, want)
		}
	}
	// Reports from before intrusion detection still read.
	var old ScanReport
	if err := json.Unmarshal([]byte(`{"scanned_at":"2026-01-01T00:00:00Z","inventory":null,"integrity":{},"vulnerable":0}`), &old); err != nil || old.Intrusion != nil {
		t.Fatalf("old report: %v %+v", err, old.Intrusion)
	}
	old.Inventory = &Inventory{}
	if old.headline() != "" {
		t.Errorf("old report headline %q", old.headline())
	}
}

// intrusionSite answers the scan's commands like a WordPress site whose
// administrators and files the test sets.
type intrusionSite struct {
	admins string // JSON
	files  map[string]string
}

func (f *intrusionSite) exec(args []string, _ io.Reader, out io.Writer) error {
	switch {
	case args[0] == "sh" && args[2] == manifestScript:
		paths := make([]string, 0, len(f.files))
		for p := range f.files {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, p := range paths {
			fmt.Fprintf(out, "%s  %s\n", f.files[p], p)
		}
		return nil
	case slices.Contains(args, "eval") && args[len(args)-1] == listAdminsPHP:
		_, err := io.WriteString(out, f.admins)
		return err
	}
	return fmt.Errorf("unexpected command %v", args)
}

func TestCheckIntrusionAcrossScans(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, err := h.svc.Store.GetSite(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	site := &intrusionSite{admins: `[{"id":1,"login":"owner","email":"o@a.test","registered":"2024-01-01 00:00:00"}]`,
		files: map[string]string{"plugins/forms/forms.php": hashA, "themes/astra/functions.php": hashA}}
	h.rt.exec = site.exec
	inv := &Inventory{Plugins: []Component{{Type: "plugin", Slug: "forms", Version: "1.0"}}}

	// First scan: a baseline.
	first, err := h.svc.checkIntrusion(ctx, st, nil, inv)
	if err != nil {
		t.Fatal(err)
	}
	if first.AdminsBaseline == "" || first.FilesBaseline == "" || len(first.NewAdmins) != 0 || first.FilesChecked != 2 {
		t.Fatalf("first scan: %+v", first)
	}
	fi, err := os.Stat(h.svc.manifestPath("s1"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("manifest: %v %v", err, fi)
	}
	prev := &ScanReport{ScannedAt: time.Now().Add(-time.Minute), Inventory: inv, Intrusion: first}

	// An administrator made from the panel isn't an intruder.
	if err := h.svc.Store.SaveScan(ctx, "s1", prev.ScannedAt, mustJSON(t, prev)); err != nil {
		t.Fatal(err)
	}
	h.svc.noteAdmin(ctx, "s1", AdminAccount{ID: 2, Login: "editor-in-chief", Role: RoleAdministrator})
	prev, err = h.svc.LastScan(ctx, "s1")
	if err != nil || len(prev.Intrusion.Admins) != 2 {
		t.Fatalf("noted admin: %v %+v", err, prev.Intrusion)
	}

	// Then a backdoor: an administrator nobody made here, and a file.
	site.admins = `[{"id":1,"login":"owner"},{"id":2,"login":"editor-in-chief"},{"id":"7","login":"wp_service","role":"administrator"}]`
	site.files["plugins/forms/forms.php"] = hashB
	site.files["themes/astra/404.php"] = hashB
	second, err := h.svc.checkIntrusion(ctx, st, prev, inv)
	if err != nil {
		t.Fatal(err)
	}
	if second.AdminsBaseline != "" || len(second.NewAdmins) != 1 || second.NewAdmins[0].Login != "wp_service" {
		t.Errorf("new admins: %+v", second)
	}
	if second.FilesBaseline != "" || second.FileChangesTotal != 2 || second.FileChanges[0].Path != "plugins/forms/forms.php" ||
		second.FileChanges[1] != (FileChange{Path: "themes/astra/404.php", Change: ChangeAdded, Component: "theme astra"}) {
		t.Errorf("file changes: %+v", second.FileChanges)
	}

	// An update explains its own files.
	prev = &ScanReport{ScannedAt: time.Now(), Inventory: inv, Intrusion: second}
	site.files["plugins/forms/forms.php"] = hashA
	updated := &Inventory{Plugins: []Component{{Type: "plugin", Slug: "forms", Version: "1.1"}}}
	third, err := h.svc.checkIntrusion(ctx, st, prev, updated)
	if err != nil {
		t.Fatal(err)
	}
	if third.FileChangesTotal != 0 || len(third.NewAdmins) != 0 || !slices.Contains(third.Updated, "plugin forms updated 1.0 → 1.1") {
		t.Errorf("after an update: %+v", third)
	}

	// A restore replaces everything: the comparison starts over.
	job, err := h.svc.Store.CreateJob(ctx, "s1", "restore", "someone")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Store.FinishJob(ctx, job, store.JobSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	prev = &ScanReport{ScannedAt: time.Now().Add(-time.Hour), Inventory: updated, Intrusion: third}
	site.files["plugins/forms/forms.php"] = hashB
	fourth, err := h.svc.checkIntrusion(ctx, st, prev, updated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fourth.FilesBaseline, "restored") || fourth.FileChangesTotal != 0 {
		t.Errorf("after a restore: %+v", fourth)
	}

	// Administrators that can't be listed keep the previous baseline.
	site.admins = "not json"
	prev = &ScanReport{ScannedAt: time.Now(), Inventory: updated, Intrusion: fourth}
	fifth, err := h.svc.checkIntrusion(ctx, st, prev, updated)
	if err == nil || len(fifth.Admins) != len(fourth.Admins) || fifth.AdminsBaseline == adminsUnknown {
		t.Errorf("unlisted admins: %v %+v", err, fifth)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
