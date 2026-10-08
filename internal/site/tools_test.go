package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// ---- Maintenance mode ----

func TestMaintenanceMessageValidation(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"  Back at 5 pm.  ", "Back at 5 pm.", true},
		{"", "", true},
		{"line one\r\nline two", "line one\nline two", true},
		{strings.Repeat("é", MaxMaintenanceMessage), strings.Repeat("é", MaxMaintenanceMessage), true},
		{strings.Repeat("a", MaxMaintenanceMessage+1), "", false},
		{"tab\there", "", false},
		{"bell\x07", "", false},
		{"1\n2\n3\n4\n5\n6\n7", "", false},
		{"bad \xff utf-8", "", false},
	} {
		got, err := normalizeMaintenanceMessage(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("normalizeMaintenanceMessage(%q) = %q, %v", c.in, got, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q: error %v isn't invalid input", c.in, err)
		}
	}
}

// The message can't break out of the PHP string: it travels base64-encoded,
// and reads back as it was written.
func TestMaintenanceWrapperRoundTrip(t *testing.T) {
	for _, msg := range []string{"", "Back soon!", `'); system('id'); ?><?php //`, "Ünïcødé\nsecond line <b>bold</b>"} {
		w := maintenanceWrapperFor(msg)
		if !strings.Contains(w, managedMarker) || !strings.Contains(w, "/usr/local/share/wpgenie/maintenance.php") {
			t.Fatalf("wrapper:\n%s", w)
		}
		if msg != "" && strings.Contains(w, msg) {
			t.Errorf("the raw message is in the PHP: %q", msg)
		}
		got, ok := parseMaintenanceWrapper([]byte(w))
		if !ok || got != msg {
			t.Errorf("round trip of %q: %q, %v", msg, got, ok)
		}
	}
	// The site's own file at that path isn't WPGenie's.
	if _, ok := parseMaintenanceWrapper([]byte("<?php define( 'WPGENIE_MAINTENANCE', 'eA==' );")); ok {
		t.Error("an unmanaged file read as maintenance mode")
	}
	// What the site may have written back is bounded.
	long := strings.Replace(maintenanceWrapperFor("x"), "'eA=='", "'"+strings.Repeat("QUFB", 400)+"'", 1)
	if got, ok := parseMaintenanceWrapper([]byte(long)); !ok || len([]rune(got)) != MaxMaintenanceMessage {
		t.Errorf("long message: %d characters, %v", len([]rune(got)), ok)
	}
}

func TestMaintenanceModeOnOff(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	os.MkdirAll(filepath.Join(h.svc.Cfg.SiteRoot("s1"), pageCacheDir), 0o755)
	m, err := h.svc.SetMaintenanceMode(ctx, "s1", MaintenanceInput{On: true, Message: " Upgrading the shop "})
	if err != nil {
		t.Fatal(err)
	}
	if !m.On || m.Message != "Upgrading the shop" {
		t.Fatalf("set: %+v", m)
	}
	wrapper := filepath.Join(h.svc.Cfg.SiteRoot("s1"), maintenanceWrapperPath)
	b, err := os.ReadFile(wrapper)
	if err != nil || !strings.Contains(string(b), "WPGENIE_MAINTENANCE") {
		t.Fatalf("wrapper: %s %v", b, err)
	}
	if got, err := h.svc.MaintenanceMode(ctx, "s1"); err != nil || *got != *m {
		t.Fatalf("read back: %+v %v", got, err)
	}
	// The page cache was purged: the purge marker exists.
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteRoot("s1"), pageCacheMarker)); err != nil {
		t.Errorf("page cache not purged: %v", err)
	}
	// A restore brings the file back owned by the site: rewritten, same message.
	if err := h.svc.rewriteManagedFiles(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.svc.MaintenanceMode(ctx, "s1"); !got.On || got.Message != "Upgrading the shop" {
		t.Fatalf("after rewriting the wrappers: %+v", got)
	}
	if _, err := h.svc.SetMaintenanceMode(ctx, "s1", MaintenanceInput{On: true, Message: strings.Repeat("x", 301)}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("long message: %v", err)
	}
	if m, err = h.svc.SetMaintenanceMode(ctx, "s1", MaintenanceInput{On: false, Message: "ignored"}); err != nil || m.On || m.Message != "" {
		t.Fatalf("off: %+v %v", m, err)
	}
	if _, err := os.Stat(wrapper); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wrapper still there: %v", err)
	}
	ev, _ := h.svc.Store.Events(ctx, "s1", 10)
	if len(ev) < 2 || !strings.Contains(ev[0].Message, "off") {
		t.Errorf("events: %+v", ev)
	}
	// The site's own file at that path is never taken over.
	os.MkdirAll(filepath.Dir(wrapper), 0o755)
	os.WriteFile(wrapper, []byte("<?php // mine"), 0o644)
	if _, err := h.svc.SetMaintenanceMode(ctx, "s1", MaintenanceInput{On: true}); !errors.Is(err, ErrConflict) {
		t.Errorf("over the site's own file: %v", err)
	}
	if got, _ := h.svc.MaintenanceMode(ctx, "s1"); got.On {
		t.Error("the site's own file reads as maintenance mode")
	}
}

// ---- Debug mode ----

func testWPConfig(t *testing.T) []byte {
	t.Helper()
	b, err := renderWPConfig(wpConfigData{SiteID: "s1", DBName: "wp_s1", DBUser: "u", DBPassword: "p", DBHost: "db",
		RedisHost: "valkey", TablePrefix: "wp_"})
	if err != nil {
		t.Fatal(err)
	}
	return append(b, "// the owner's own lines\n"...)
}

func TestDebugBlock(t *testing.T) {
	cfg := testWPConfig(t)
	until := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	on, err := withDebugBlock(cfg, until)
	if err != nil {
		t.Fatal(err)
	}
	s := string(on)
	block := s[strings.Index(s, debugBlockStart):strings.Index(s, wpgenieEnd)]
	for _, want := range []string{
		fmt.Sprintf("if ( time() < %d ) {", until.Unix()),
		"define( 'WP_DEBUG', true );",
		"define( 'WP_DEBUG_LOG', __DIR__ . '/logs/php-error.log' );", // outside the docroot
		"define( 'WP_DEBUG_DISPLAY', false );",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	// Inside WPGenie's section: before the owner's part and the WP_DEBUG fallback.
	if strings.Index(s, debugBlockStart) > strings.Index(s, wpgenieEnd) || strings.Index(s, wpgenieEnd) > strings.Index(s, "the owner's own lines") {
		t.Error("block outside WPGenie's section")
	}
	if got := debugUntil(on); !got.Equal(until) {
		t.Errorf("debugUntil = %v", got)
	}
	// Turning it on again replaces the block; off restores the file exactly.
	later := until.Add(time.Hour)
	again, _ := withDebugBlock(on, later)
	if strings.Count(string(again), debugBlockStart) != 1 || !debugUntil(again).Equal(later) {
		t.Errorf("on twice:\n%s", again)
	}
	off, err := withDebugBlock(again, time.Time{})
	if err != nil || !bytes.Equal(off, cfg) {
		t.Errorf("off doesn't restore the file (%v):\n%s", err, off)
	}
	if !debugUntil(cfg).IsZero() {
		t.Error("debug mode found in a plain wp-config.php")
	}
	if st := debugState(on, until.Add(-time.Minute)); !st.On || !st.Until.Equal(until) {
		t.Errorf("state before the end: %+v", st)
	}
	if st := debugState(on, until); st.On {
		t.Errorf("state at the end: %+v", st)
	}
	if _, err := withDebugBlock([]byte("<?php // someone else's\n"), until); !errors.Is(err, ErrConflict) {
		t.Errorf("no WPGenie section: %v", err)
	}
}

func writeTestWPConfig(t *testing.T, h *harness) (string, []byte) {
	t.Helper()
	path, cfg := h.svc.wpConfigPath("s1"), testWPConfig(t)
	if err := os.WriteFile(path, cfg, 0o640); err != nil {
		t.Fatal(err)
	}
	return path, cfg
}

func TestDebugModeAndExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path, orig := writeTestWPConfig(t, h)
	d, err := h.svc.SetDebugMode(ctx, "s1", DebugInput{On: true})
	if err != nil {
		t.Fatal(err)
	}
	if !d.On || d.Until.Sub(time.Now()) < DebugFor-2*time.Minute || d.Until.Sub(time.Now()) > DebugFor {
		t.Fatalf("on: %+v", d)
	}
	if got, _ := h.svc.DebugMode(ctx, "s1"); !got.On || !got.Until.Equal(d.Until) {
		t.Fatalf("read back: %+v", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode changed: %v", fi.Mode())
	}
	// Not yet: nothing happens.
	h.svc.expireDebug("s1", time.Now())
	if got, _ := h.svc.DebugMode(ctx, "s1"); !got.On {
		t.Fatal("expired early")
	}
	// After 24 hours the loop takes the block out and says so.
	h.svc.expireDebug("s1", d.Until.Add(time.Second))
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), debugBlockStart) {
		t.Fatal("block left after expiry")
	}
	ev, _ := h.svc.Store.Events(ctx, "s1", 10)
	if len(ev) == 0 || !strings.Contains(ev[0].Message, "turned itself off") {
		t.Errorf("events: %+v", ev)
	}
	if _, err := h.svc.SetDebugMode(ctx, "s1", DebugInput{On: true}); err != nil {
		t.Fatal(err)
	}
	if d, err := h.svc.SetDebugMode(ctx, "s1", DebugInput{On: false}); err != nil || d.On {
		t.Fatalf("off: %+v %v", d, err)
	}
	if b2, _ := os.ReadFile(path); !bytes.Equal(b2, orig) {
		t.Errorf("off left:\n%s", b2)
	}
}

func TestTailLines(t *testing.T) {
	lines, cut := tailLines([]byte("a\nb\nc\n"), false, 2)
	if !slices.Equal(lines, []string{"b", "c"}) || !cut {
		t.Errorf("last 2: %q %v", lines, cut)
	}
	lines, cut = tailLines([]byte("partial\nx\r\ny"), true, 10)
	if !slices.Equal(lines, []string{"x", "y"}) || !cut {
		t.Errorf("mid-file: %q %v", lines, cut)
	}
	if lines, cut = tailLines(nil, false, 5); len(lines) != 0 || cut {
		t.Errorf("empty: %q %v", lines, cut)
	}
	if lines, _ = tailLines([]byte("no newline at all"), true, 5); len(lines) != 0 {
		t.Errorf("one partial line: %q", lines)
	}
}

func TestDebugLogTailAndClear(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if l, err := h.svc.DebugLogTail(ctx, "s1"); err != nil || len(l.Lines) != 0 {
		t.Fatalf("no log yet: %+v %v", l, err)
	}
	dir := h.svc.Cfg.SiteDir("s1")
	if err := ensureLogDir(dir); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range 300 {
		fmt.Fprintf(&b, "[09-Oct-2026 10:00:00 UTC] PHP Notice:  line %d in /x.php on line 1\n", i)
	}
	logPath := filepath.Join(dir, phpLogPath)
	if err := os.WriteFile(logPath, []byte(b.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	l, err := h.svc.DebugLogTail(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Lines) != debugTailLines || !l.Truncated || !strings.Contains(l.Lines[len(l.Lines)-1], "line 299 ") ||
		l.Size != int64(b.Len()) {
		t.Fatalf("tail: %d lines, truncated %v, last %q", len(l.Lines), l.Truncated, l.Lines[len(l.Lines)-1])
	}
	if err := h.svc.ClearDebugLog(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(logPath); err != nil || fi.Size() != 0 {
		t.Fatalf("not cleared: %v %v", fi, err)
	}
	// Read into the insights before it went.
	if errs, _ := h.svc.Store.PHPErrors(ctx, "s1", time.Time{}, 10); len(errs) == 0 {
		t.Error("the log's errors weren't kept in the insights")
	}
	// A symlink planted in its place is never followed.
	os.Remove(logPath)
	os.WriteFile(filepath.Join(dir, "secret"), []byte("x"), 0o600)
	os.Symlink(filepath.Join(dir, "secret"), logPath)
	if _, err := h.svc.DebugLogTail(ctx, "s1"); err == nil {
		t.Error("followed a symlink")
	}
	if err := h.svc.ClearDebugLog(ctx, "s1"); err == nil {
		t.Error("truncated through a symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "secret")); string(b) != "x" {
		t.Error("the symlink's target changed")
	}
}

// ---- Search & replace ----

func TestSearchReplaceValidation(t *testing.T) {
	for _, c := range []struct {
		search, replace string
		ok              bool
	}{
		{"old.example.com", "new.example.com", true},
		{"http://a.test", "https://a.test", true},
		{"", "x", false},
		{"   ", "x", false},
		{"abc", "", false},
		{"ab", "xyz", false}, // too short to be safe
		{"same", "same", false},
		{"--dry-run", "x", false},
		{"abc", "--all-tables", false},
		{"-abc", "x", false},
		{"line\nbreak", "x", false},
		{strings.Repeat("a", 1001), "x", false},
		{"abc", "bad \xff", false},
	} {
		in := SearchReplaceInput{Search: c.search, Replace: c.replace}
		err := validSearchReplace(&in)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalidInput)) {
			t.Errorf("%q → %q: %v", c.search, c.replace, err)
		}
	}
	args := replaceTextArgs(SearchReplaceInput{Search: "a.test", Replace: "b.test", DryRun: true})
	want := []string{"search-replace", "a.test", "b.test", "--precise", "--all-tables-with-prefix", "--skip-columns=guid",
		"--report-changed-only", "--dry-run"}
	if !slices.Equal(args, want) {
		t.Errorf("args %q", args)
	}
	if slices.Contains(replaceTextArgs(SearchReplaceInput{Search: "a", Replace: "b"}), "--dry-run") {
		t.Error("a real run is a dry run")
	}
}

func TestParseSearchReplace(t *testing.T) {
	piped := "Table\tColumn\tReplacements\tType\n" +
		"wp_options\toption_value\t2\tPHP\n" +
		"wp_posts\tpost_content\t5\tPHP\n" +
		"wp_posts\tpost_excerpt\t1\tPHP\n" +
		"wp_postmeta\tmeta_value\t0\tPHP\n" +
		"Success: 8 replacements to be made.\n"
	total, tables, err := parseSearchReplace(piped)
	if err != nil || total != 8 {
		t.Fatalf("total %d %v", total, err)
	}
	want := []SearchReplaceTable{{"wp_posts", 6}, {"wp_options", 2}}
	if !slices.Equal(tables, want) {
		t.Errorf("tables %+v", tables)
	}
	ascii := "+------------+--------------+--------------+------+\n" +
		"| Table      | Column       | Replacements | Type |\n" +
		"+------------+--------------+--------------+------+\n" +
		"| wp_options | option_value | 1            | SQL  |\n" +
		"+------------+--------------+--------------+------+\n" +
		"Success: Made 1 replacement.\n"
	if total, tables, err = parseSearchReplace(ascii); err != nil || total != 1 || len(tables) != 1 || tables[0].Table != "wp_options" {
		t.Errorf("ascii: %d %+v %v", total, tables, err)
	}
	if total, tables, err = parseSearchReplace("Success: 0 replacements to be made.\n"); err != nil || total != 0 || len(tables) != 0 {
		t.Errorf("nothing: %d %+v %v", total, tables, err)
	}
	if _, _, err = parseSearchReplace("Error: something"); err == nil {
		t.Error("no result accepted")
	}
}

func TestSearchReplaceJob(t *testing.T) {
	h := newHarness(t)
	h.svc.Jobs = &jobs.Queue{Store: h.svc.Store, Log: slog.New(slog.DiscardHandler)}
	ctx := context.Background()
	var calls [][]string
	h.rt.exec = func(args []string, _ io.Reader, out io.Writer) error {
		calls = append(calls, args)
		if slices.Contains(args, "search-replace") {
			io.WriteString(out, "Table\tColumn\tReplacements\tType\nwp_options\toption_value\t3\tPHP\nSuccess: Made 3 replacements.\n")
		}
		return nil
	}
	// Without backups configured, a real run needs the person's word.
	if _, err := h.svc.StartSearchReplace(ctx, "s1", SearchReplaceInput{Search: "a.test", Replace: "b.test"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("real run without a backup: %v", err)
	}
	id, err := h.svc.StartSearchReplace(ctx, "s1", SearchReplaceInput{Search: "a.test", Replace: "b.test", HaveBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	j, err := h.svc.Jobs.WaitJob(ctx, id)
	if err != nil || j.Status != store.JobSucceeded || j.Kind != "search-replace" {
		t.Fatalf("job: %+v %v", j, err)
	}
	var res SearchReplaceResult
	if err := json.Unmarshal([]byte(j.Result), &res); err != nil || res.Total != 3 || res.DryRun {
		t.Fatalf("result %s: %v", j.Result, err)
	}
	if len(calls) != 1 || !slices.Equal(calls[0][:3], []string{"wp", "--skip-themes", "--skip-plugins"}) ||
		!slices.Contains(calls[0], "--precise") {
		t.Errorf("calls %q", calls)
	}
	ev, _ := h.svc.Store.Events(ctx, "s1", 5)
	if len(ev) == 0 || !strings.Contains(ev[0].Message, "3 replacements in 1 table") {
		t.Errorf("events %+v", ev)
	}
}

// ---- Scheduled tasks ----

func TestParseCronEvents(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	out := fmt.Sprintf(`[
		{"hook":"wp_version_check","time":%d,"sig":"40cd750bba9870f18aada2478b24840a","args":[],"schedule":"twicedaily","interval":43200,"recurrence":"12 hours"},
		{"hook":"my_once","time":"%d","sig":"0123456789abcdef0123456789abcdef","args":[5,"x"],"schedule":false,"interval":false,"recurrence":"Non-repeating"},
		{"hook":"obj_args","time":%d,"sig":"not-a-sig","args":{"a":1},"schedule":"hourly","interval":"3600","recurrence":"1 hour"},
		{"hook":"","time":1}
	]`, now.Unix()+600, now.Unix()-3600, now.Unix()-60)
	ev, err := parseCronEvents([]byte(out), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 3 || ev[0].Hook != "my_once" || ev[2].Hook != "wp_version_check" {
		t.Fatalf("order: %+v", ev)
	}
	once, obj, check := ev[0], ev[1], ev[2]
	if !once.Overdue || once.Recurrence != "" || once.Schedule != "" || once.Args != 2 || once.Interval != 0 {
		t.Errorf("one-off: %+v", once)
	}
	if obj.Overdue || obj.Sig != "" || obj.Args != 1 || obj.Interval != 3600 {
		t.Errorf("object args (a minute late isn't overdue): %+v", obj)
	}
	if check.Overdue || check.Recurrence != "12 hours" || check.Interval != 43200 || check.Sig != "40cd750bba9870f18aada2478b24840a" {
		t.Errorf("recurring: %+v", check)
	}
	if _, err := parseCronEvents([]byte("Error: nope"), now); err == nil {
		t.Error("garbage accepted")
	}
}

func TestCronRunnerAndRunNow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	now := time.Now()
	if r := h.svc.cronRunner(st, now); r.State != "waiting" {
		t.Errorf("before any run: %+v", r)
	}
	h.svc.recordCronRun("s1", nil, nil)
	if r := h.svc.cronRunner(st, now); r.State != "ok" {
		t.Errorf("after a run: %+v", r)
	}
	if r := h.svc.cronRunner(st, now.Add(10*time.Minute)); r.State != "failing" {
		t.Errorf("no run for 10 minutes: %+v", r)
	}
	h.svc.recordCronRun("s1", errors.New("exit status 255"), []byte("PHP Fatal error"))
	if r := h.svc.cronRunner(st, now); r.State != "failing" || !strings.Contains(r.Error, "Fatal") {
		t.Errorf("failed run: %+v", r)
	}
	staging := *st
	staging.ParentID = "live"
	if r := h.svc.cronRunner(&staging, now); r.State != "staging" {
		t.Errorf("staging: %+v", r)
	}

	for _, bad := range []CronRunInput{
		{Hook: "", Time: 1, Sig: "40cd750bba9870f18aada2478b24840a"},
		{Hook: "x", Time: 0, Sig: "40cd750bba9870f18aada2478b24840a"},
		{Hook: "x", Time: 1, Sig: "'); evil(); //"},
		{Hook: "x\ny", Time: 1, Sig: "40cd750bba9870f18aada2478b24840a"},
	} {
		if err := h.svc.RunCronEvent(ctx, "s1", bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	var code string
	h.rt.exec = func(args []string, _ io.Reader, _ io.Writer) error {
		code = args[len(args)-1]
		return nil
	}
	hook := `it's "quoted"`
	if err := h.svc.RunCronEvent(ctx, "s1", CronRunInput{Hook: hook, Time: 1_800_000_000, Sig: "40cd750bba9870f18aada2478b24840a"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(code, hook) || !strings.Contains(code, "$ts   = 1800000000;") || !strings.Contains(code, "_set_cron_array") {
		t.Errorf("PHP:\n%s", code)
	}
	h.rt.exec = func([]string, io.Reader, io.Writer) error { return errors.New("exit status 3: " + noEventMarker) }
	if err := h.svc.RunCronEvent(ctx, "s1", CronRunInput{Hook: "x", Time: 1, Sig: "40cd750bba9870f18aada2478b24840a"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("gone event: %v", err)
	}
}

// ---- WordPress settings ----

func TestWordPressSettings(t *testing.T) {
	cur := settingsFrom(map[string]json.RawMessage{
		"blogname":               json.RawMessage(`"Tom &amp; Jerry"`),
		"blogdescription":        json.RawMessage(`"Just another site"`),
		"timezone_string":        json.RawMessage(`""`),
		"gmt_offset":             json.RawMessage(`5.5`),
		"blog_public":            json.RawMessage(`"0"`),
		"default_comment_status": json.RawMessage(`"open"`),
	})
	want := WPSettings{Title: "Tom & Jerry", Tagline: "Just another site", UTCOffset: 5.5, DiscourageSearch: true, CommentsOpen: true}
	if *cur != want {
		t.Fatalf("read %+v", cur)
	}
	str := func(s string) *string { return &s }
	yes, no := true, false
	ups, err := wpSettingUpdates(cur, WPSettingsInput{Title: str(" Tom & Jerry "), Tagline: str("New tagline"),
		Timezone: str("Europe/London"), DiscourageSearch: &no, CommentsOpen: &yes})
	if err != nil {
		t.Fatal(err)
	}
	wantUps := [][2]string{{"blogdescription", "New tagline"}, {"timezone_string", "Europe/London"}, {"blog_public", "1"}}
	if !slices.Equal(ups, wantUps) {
		t.Errorf("updates %q", ups)
	}
	for _, bad := range []WPSettingsInput{
		{Title: str(strings.Repeat("t", 201))},
		{Tagline: str("two\nlines")},
		{Timezone: str("Mars/Olympus Mons")},
		{Timezone: str("../../etc/passwd")},
		{Timezone: str("--option")},
	} {
		if _, err := wpSettingUpdates(cur, bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	for _, name := range []string{"blogname", "blogdescription", "timezone_string", "blog_public", "default_comment_status"} {
		if settingLabel[name] == "" {
			t.Errorf("no label for %s", name)
		}
	}
}

func TestSetWordPressSettingsUsesStdin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	opts := map[string]string{"blogname": `"Old"`, "blogdescription": `""`, "timezone_string": `"UTC"`, "gmt_offset": `0`,
		"blog_public": `"1"`, "default_comment_status": `"open"`}
	h.rt.exec = func(args []string, _ io.Reader, out io.Writer) error {
		if len(args) >= 6 && args[3] == "option" && args[4] == "get" {
			if !slices.Contains(wpSettingOptions, args[5]) {
				return fmt.Errorf("option %s isn't allowlisted", args[5])
			}
			io.WriteString(out, opts[args[5]])
		}
		return nil
	}
	title := "--title=evil"
	if _, err := h.svc.SetWordPressSettings(ctx, "s1", WPSettingsInput{Title: &title}); err != nil {
		t.Fatal(err)
	}
	// The value never reaches argv (the fake WP logs argv only).
	for _, l := range *h.log {
		if strings.Contains(l, "evil") {
			t.Errorf("value in argv: %s", l)
		}
	}
	if !slices.Contains(*h.log, "wp option update blogname") {
		t.Errorf("calls %q", *h.log)
	}
}

// ---- Themes ----

func TestParseThemesAndSlugs(t *testing.T) {
	var items []wpThemeItem
	if err := json.Unmarshal([]byte(`[
		{"name":"twentytwentythree","title":"Twenty Twenty-Three","status":"inactive","version":"1.2","update":"available","update_version":"1.3"},
		{"name":"child","title":"Child","status":"active","version":"1.0","update":"none","update_version":""},
		{"name":"astra","title":"Astra","status":"parent","version":"4.0","update":false,"update_version":""},
		{"name":"--evil","title":"x","status":"inactive","version":"1","update":"none"}
	]`), &items); err != nil {
		t.Fatal(err)
	}
	th := parseThemes(items)
	if len(th) != 3 || th[0].Slug != "child" || th[1].Slug != "astra" || th[2].UpdateVersion != "1.3" || th[1].UpdateVersion != "" {
		t.Fatalf("themes %+v", th)
	}
	for slug, ok := range map[string]bool{"astra": true, "twenty-twenty": true, "a1": true, "-x": false, "--activate": false,
		"Astra": false, "astra_pro": false, "a/b": false, "": false, strings.Repeat("a", 101): false} {
		if err := validThemeSlug(slug); (err == nil) != ok {
			t.Errorf("validThemeSlug(%q) = %v", slug, err)
		}
	}
}

func TestDeleteThemeKeepsActiveAndParent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rt.exec = func(args []string, _ io.Reader, out io.Writer) error {
		if slices.Contains(args, "list") {
			io.WriteString(out, `[{"name":"child","title":"Child","status":"active","version":"1","update":"none"},
				{"name":"astra","title":"Astra","status":"parent","version":"4","update":"none"},
				{"name":"old","title":"Old","status":"inactive","version":"1","update":"none"}]`)
		}
		return nil
	}
	for _, slug := range []string{"child", "astra", "missing", "../x"} {
		if err := h.svc.DeleteTheme(ctx, "s1", slug); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("deleting %s: %v", slug, err)
		}
	}
	if err := h.svc.DeleteTheme(ctx, "s1", "old"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*h.log, "wp theme delete old") || slices.ContainsFunc(*h.log, func(l string) bool {
		return strings.HasPrefix(l, "wp theme delete") && l != "wp theme delete old"
	}) {
		t.Errorf("calls %q", *h.log)
	}
	if _, err := h.svc.ActivateTheme(ctx, "s1", "old"); err != nil || !slices.Contains(*h.log, "wp theme activate old") {
		t.Errorf("activate: %v %q", err, *h.log)
	}
}
