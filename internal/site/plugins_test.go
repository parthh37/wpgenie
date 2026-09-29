package site

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeDirectory map[string]DirectoryInfo

func (d fakeDirectory) Plugin(_ context.Context, slug string) (DirectoryInfo, error) {
	if slug == "flaky" {
		return DirectoryInfo{}, errors.New("wordpress.org: HTTP 503")
	}
	if info, ok := d[slug]; ok {
		return info, nil
	}
	return DirectoryInfo{Status: "not_listed"}, nil
}

// analyserWP answers the commands the analyser runs, with output recorded
// from real WP-CLI and profile.php runs.
func analyserWP(profile string) func([]string, io.Reader, io.Writer) error {
	return func(args []string, _ io.Reader, out io.Writer) error {
		cmd := strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "plugin list"):
			fmt.Fprint(out, `[{"name":"akismet","title":"Akismet","status":"active","version":"5.7.2","update":"none","update_version":""},
				{"name":"old-gallery","title":"Old Gallery","status":"active","version":"1.0","update":"none","update_version":""},
				{"name":"gone-seo","title":"Gone SEO","status":"inactive","version":"3.1","update":"available","update_version":"3.2"},
				{"name":"pro-builder","title":"Pro Builder","status":"active","version":"2.0","update":"none","update_version":""},
				{"name":"heavy","title":"Heavy","status":"active","version":"9","update":"none","update_version":""},
				{"name":"flaky","title":"Flaky","status":"active","version":"1","update":"none","update_version":""},
				{"name":"object-cache.php","title":"","status":"dropin","version":"","update":"none","update_version":""}]`)
		case strings.Contains(cmd, "plugin verify-checksums"):
			// Verbatim shape: the JSON isn't newline-terminated.
			fmt.Fprint(out, "Warning: Couldn't fetch response from https://downloads.wordpress.org/plugin-checksums/pro-builder/2.0.json (HTTP code 404).\n"+
				"Warning: Could not retrieve the checksums for version 2.0 of plugin pro-builder, skipping.\n"+
				`[{"plugin_name":"old-gallery","file":"gallery.php","message":"Checksum does not match"},{"plugin_name":"old-gallery","file":"x.php","message":"File was added"}]`+
				"Error: Only verified 4 of 6 plugins (1 failed, 1 skipped).")
			return errors.New("exit status 1")
		case strings.Contains(cmd, "grep -o -i -E"):
			root := args[len(args)-3] // .../wp-content/plugins
			fmt.Fprintf(out, "%s/pro-builder/inc/license.php:Nulled by\n%s/akismet/akismet.php:wp-vcd\n", root, root)
			fmt.Fprintf(out, "%s/twentyten/functions.php:div_code_name\n", strings.TrimSuffix(root, "/plugins")+"/themes")
		case args[0] == "env":
			if profile == "" {
				return &exitCodeError{99}
			}
			fmt.Fprint(out, "<html>leftover output</html>\n"+profileMarker+"\n"+profile+"\n")
		default:
			return fmt.Errorf("unexpected command %v", args)
		}
		return nil
	}
}

type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

const sampleProfile = `{"version":1,"total_ms":400,"peak_memory_kb":50000,"queries":30,"output_bytes":70000,"status":200,"redirect":null,` +
	`"owners":{"core":{"load_ms":0,"hook_ms":0,"queries":20},"plugin:akismet":{"load_ms":2,"load_kb":300,"hook_ms":1,"calls":4,"queries":1},` +
	`"plugin:heavy":{"load_ms":30,"load_kb":4000,"hook_ms":150,"calls":900,"queries":9},"theme:twentyten":{"load_ms":1,"hook_ms":0.5}}}`

func TestAnalysePlugins(t *testing.T) {
	h := newHarness(t)
	h.rt.exec = analyserWP(sampleProfile)
	now := time.Now()
	h.svc.Directory = fakeDirectory{
		"akismet":     {Status: "listed", LastUpdated: now.AddDate(0, -1, 0)},
		"old-gallery": {Status: "listed", LastUpdated: now.AddDate(-3, 0, 0)},
		"gone-seo":    {Status: "closed", ClosedDate: "2024-02-23", ClosedReason: "Security Issue"},
		"heavy":       {Status: "listed", LastUpdated: now},
	}
	rep, err := h.svc.AnalysePlugins(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]PluginInfo{}
	for _, p := range rep.Plugins {
		by[p.Slug] = p
	}
	if p := by["gone-seo"]; p.Directory != "closed" || !strings.Contains(p.Flags[0], "Security Issue") || p.UpdateVersion != "3.2" {
		t.Errorf("closed plugin: %+v", p)
	}
	if p := by["old-gallery"]; !p.Abandoned || p.Checksums != "modified" || len(p.Modified) != 2 {
		t.Errorf("abandoned, modified plugin: %+v", p)
	}
	if p := by["pro-builder"]; p.Checksums != "unavailable" || len(p.Signatures) != 1 || p.Signatures[0] != "plugins/pro-builder/inc/license.php: nulled by" {
		t.Errorf("nulled plugin: %+v", p)
	}
	// A marker in a file identical to the wordpress.org release is the
	// plugin's own (a security plugin's signature list), not an infection.
	if p := by["akismet"]; p.Checksums != "verified" || len(p.Signatures) != 0 || p.Perf == nil || p.Perf.Queries != 1 || len(p.Flags) != 0 {
		t.Errorf("clean plugin: %+v", p)
	}
	if p := by["heavy"]; p.Perf == nil || p.Perf.HookMS != 150 || !strings.HasPrefix(p.Flags[0], "slow: 180 ms of a 400 ms page") {
		t.Errorf("slow plugin: %+v", p)
	}
	if p := by["flaky"]; p.Directory != "unknown" {
		t.Errorf("failed lookup: %+v", p)
	}
	if p := by["object-cache.php"]; p.Directory != "not_listed" || p.Checksums != "not_checked" || len(p.Flags) != 0 {
		t.Errorf("drop-in: %+v", p)
	}
	if rep.Profile == nil || rep.Profile.TotalMS != 400 || rep.Profile.Others["theme:twentyten"].LoadMS != 1 || rep.Profile.Others["plugin:heavy"].LoadMS != 0 {
		t.Errorf("profile %+v", rep.Profile)
	}
	if len(rep.ThemeSignatures) != 1 || !strings.HasPrefix(rep.ThemeSignatures[0], "themes/twentyten/functions.php") {
		t.Errorf("theme signatures %v", rep.ThemeSignatures)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "503") {
		t.Errorf("errors %v", rep.Errors)
	}
	// Worst first: security findings before the merely slow.
	if rep.Plugins[0].Slug != "gone-seo" && rep.Plugins[0].Slug != "pro-builder" {
		t.Errorf("order: %s first", rep.Plugins[0].Slug)
	}
	ev, _ := h.svc.Store.Events(context.Background(), "s1", 5)
	if len(ev) == 0 || !strings.Contains(ev[0].Message, "closed on wordpress.org: gone-seo") ||
		!strings.Contains(ev[0].Message, "possibly nulled or infected: pro-builder") {
		t.Errorf("activity log %+v", ev)
	}
	stored, err := h.svc.LastPluginReport(context.Background(), "s1")
	if err != nil || len(stored.Plugins) != len(rep.Plugins) {
		t.Fatalf("stored report: %v", err)
	}
}

func TestAnalyseOnOldImage(t *testing.T) {
	h := newHarness(t)
	h.rt.exec = analyserWP("")
	rep, err := h.svc.AnalysePlugins(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	// exitCodeError isn't an *exec.ExitError: the profile fails with the
	// raw error, and everything else is still reported.
	if rep.Profile != nil || len(rep.Plugins) != 7 || len(rep.Errors) == 0 {
		t.Errorf("%+v", rep)
	}
}

func TestWordPressOrgClient(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Query().Get("request[slug]") {
		case "akismet":
			fmt.Fprint(w, `{"name":"Akismet","slug":"akismet","tested":"7.1.2","active_installs":5000000,"last_updated":"2026-08-18 11:42pm GMT"}`)
		case "wp-gdpr-compliance":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"closed","name":"x","slug":"wp-gdpr-compliance","closed":true,"closed_date":"2024-02-23","reason":"author-request","reason_text":"Author Request"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"Plugin not found."}`)
		}
	}))
	defer srv.Close()
	c := &WordPressOrg{BaseURL: srv.URL}
	ctx := context.Background()
	info, err := c.Plugin(ctx, "akismet")
	if err != nil || info.Status != "listed" || info.ActiveInstalls != 5000000 ||
		!info.LastUpdated.Equal(time.Date(2026, 8, 18, 23, 42, 0, 0, time.UTC)) {
		t.Errorf("akismet: %+v %v", info, err)
	}
	if info, _ := c.Plugin(ctx, "wp-gdpr-compliance"); info.Status != "closed" || info.ClosedReason != "Author Request" {
		t.Errorf("closed: %+v", info)
	}
	if info, _ := c.Plugin(ctx, "my-premium"); info.Status != "not_listed" {
		t.Errorf("unknown: %+v", info)
	}
	c.Plugin(ctx, "akismet")
	if calls != 3 {
		t.Errorf("%d requests: answers must be cached", calls)
	}
	if info, _ := c.Plugin(ctx, "../etc"); info.Status != "not_listed" || calls != 3 {
		t.Error("an invalid slug must not reach the API")
	}
}
