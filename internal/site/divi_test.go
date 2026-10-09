package site

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

const (
	testETUser = "acme+web@example.com"
	testETKey  = "0123456789abcdefABCDEF0123456789abcdef12"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// diviZip is a minimal Divi theme package.
func diviZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"Divi/style.css":     "/*\nTheme Name: Divi\nVersion: 4.27.4\n*/\n",
		"Divi/functions.php": "<?php\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// etServer stands in for Elegant Themes' download endpoint: the theme for
// the test account, a short error otherwise (as the real one does).
type etServer struct {
	*httptest.Server
	mu       sync.Mutex
	queries  []string
	zip      []byte
	override func(w http.ResponseWriter) bool
}

func newETServer(t *testing.T) *etServer {
	e := &etServer{zip: diviZip(t)}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.queries = append(e.queries, r.URL.RawQuery)
		e.mu.Unlock()
		if e.override != nil && e.override(w) {
			return
		}
		q := r.URL.Query()
		if q.Get("api_update") != "1" || q.Get("theme") != "Divi" || q.Get("username") != testETUser || q.Get("api_key") != testETKey {
			io.WriteString(w, "Invalid username or API key")
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(e.zip)
	}))
	t.Cleanup(e.Close)
	return e
}

func diviHarness(t *testing.T) (*harness, *etServer) {
	h := newHarness(t)
	et := newETServer(t)
	h.svc.DiviAPI = et.URL
	h.svc.DiviClient = et.Client()
	return h, et
}

func setLicense(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.svc.SetDivi(context.Background(), DiviInput{Username: strp(testETUser), APIKey: strp(testETKey)}); err != nil {
		t.Fatal(err)
	}
}

func TestDiviSettings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	l, err := h.svc.Divi(ctx)
	if err != nil || l.Configured() || l.NewSites {
		t.Fatalf("fresh: %+v %v", l, err)
	}
	for _, in := range []DiviInput{
		{Username: strp("acme"), APIKey: strp("short")},
		{Username: strp("acme"), APIKey: strp("abc'def0123456789")},
		{Username: strp("acme"), APIKey: strp(testETKey + "\n')); evil();//")},
		{Username: strp("ac'me"), APIKey: strp(testETKey)},
		{Username: strp("ac me"), APIKey: strp(testETKey)},
		{Username: strp(strings.Repeat("a", 101)), APIKey: strp(testETKey)},
		{Username: strp("acme")},  // no key yet
		{APIKey: strp(testETKey)}, // no username yet
	} {
		_, err := h.svc.SetDivi(ctx, in)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("accepted %+v: %v", in, err)
		} else if in.APIKey != nil && *in.APIKey != "" && strings.Contains(err.Error(), *in.APIKey) {
			t.Errorf("the error repeats the key: %v", err)
		}
	}
	// The new-sites switch alone, before any license.
	if l, err := h.svc.SetDivi(ctx, DiviInput{NewSites: boolp(false)}); err != nil || l.Configured() {
		t.Fatalf("switch only: %+v %v", l, err)
	}
	// First saved: on for new sites unless told otherwise.
	l, err = h.svc.SetDivi(ctx, DiviInput{Username: strp(" " + testETUser + " "), APIKey: strp(testETKey)})
	if err != nil || !l.Configured() || !l.NewSites || l.Username != testETUser {
		t.Fatalf("set: %+v %v", l, err)
	}
	// Absent fields keep what's saved.
	if l, err = h.svc.SetDivi(ctx, DiviInput{NewSites: boolp(false)}); err != nil || l.APIKey != testETKey || l.Username != testETUser || l.NewSites {
		t.Fatalf("keep: %+v %v", l, err)
	}
	if l, err = h.svc.SetDivi(ctx, DiviInput{Username: strp("other")}); err != nil || l.APIKey != testETKey || l.Username != "other" {
		t.Fatalf("new username, same key: %+v %v", l, err)
	}
	// Saved for good (a fresh service reads it back).
	again := &Service{Cfg: h.svc.Cfg, Store: h.svc.Store, Log: h.svc.Log}
	if l, err = again.Divi(ctx); err != nil || l.Username != "other" || l.APIKey != testETKey {
		t.Fatalf("reloaded: %+v %v", l, err)
	}
	// What the API shows: never the key.
	v := l.View()
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), testETKey) || !v.KeySet || v.KeyHint != "…ef12" || !v.Configured {
		t.Errorf("admin view %s", b)
	}
	b, _ = json.Marshal(v.Public())
	if strings.Contains(string(b), "other") || strings.Contains(string(b), "ef12") || strings.Contains(string(b), "key_hint") {
		t.Errorf("public view %s", b)
	}
	// The whole license for nodes.
	in := l.Input()
	if *in.Username != "other" || *in.APIKey != testETKey || in.NewSites == nil {
		t.Errorf("input %+v", in)
	}
	// An empty key (or username) removes the license.
	if l, err = h.svc.SetDivi(ctx, DiviInput{APIKey: strp("")}); err != nil || l.Configured() || l.Username != "" || l.APIKey != "" {
		t.Fatalf("clear: %+v %v", l, err)
	}
	if h.svc.DiviDefault(ctx) {
		t.Error("new sites get Divi without a license")
	}
}

func TestDiviCredsFile(t *testing.T) {
	b, err := diviCreds(testETUser, testETKey)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"define( 'WPGENIE_ET_USER', '" + testETUser + "' );", "define( 'WPGENIE_ET_KEY', '" + testETKey + "' );"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if !strings.HasPrefix(s, "<?php\n") || strings.Count(s, "'") != 8 {
		t.Errorf("unexpected file:\n%s", s)
	}
	for _, bad := range [][2]string{{"a'b", testETKey}, {"a\\b", testETKey}, {"a", "abc'defghijk"}, {"a", "abcd\\efghijk"},
		{"a", "abcdefgh\n"}, {"", testETKey}, {"a", ""}, {"a b", testETKey}} {
		if _, err := diviCreds(bad[0], bad[1]); err == nil {
			t.Errorf("rendered %q", bad)
		}
	}
}

func TestCheckDivi(t *testing.T) {
	h, et := diviHarness(t)
	ctx := context.Background()
	if err := h.svc.CheckDivi(ctx); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("no license: %v", err)
	}
	setLicense(t, h)
	if err := h.svc.CheckDivi(ctx); err != nil {
		t.Fatalf("good license: %v", err)
	}
	q := et.queries[len(et.queries)-1]
	if !strings.Contains(q, "api_update=1") || !strings.Contains(q, "theme=Divi") || !strings.Contains(q, "api_key="+testETKey) {
		t.Errorf("query %s", q)
	}

	refused := func(name string, override func(w http.ResponseWriter) bool) {
		t.Helper()
		et.override = override
		defer func() { et.override = nil }()
		err := h.svc.CheckDivi(ctx)
		if !errors.Is(err, ErrDiviRefused) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), testETKey) {
			t.Errorf("%s: the error shows the key: %v", name, err)
		}
	}
	refused("text answer", func(w http.ResponseWriter) bool { io.WriteString(w, "Invalid username or API key"); return true })
	refused("HTML page", func(w http.ResponseWriter) bool {
		io.WriteString(w, "<!DOCTYPE html><html><body>Your subscription has expired</body></html>")
		return true
	})
	refused("echoing the key", func(w http.ResponseWriter) bool { io.WriteString(w, "bad key "+testETKey); return true })
	refused("forbidden", func(w http.ResponseWriter) bool { w.WriteHeader(http.StatusForbidden); return true })
	refused("empty", func(w http.ResponseWriter) bool { return true })

	// Too big to be a theme: refused before reading it.
	et.override = func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Length", strconv.Itoa(maxDiviZip+1))
		w.Write([]byte("PK\x03\x04"))
		return true
	}
	if err := h.svc.CheckDivi(ctx); err == nil || !strings.Contains(err.Error(), "MB") {
		t.Errorf("oversize: %v", err)
	}
	et.override = func(w http.ResponseWriter) bool { w.WriteHeader(http.StatusBadGateway); return true }
	if err := h.svc.CheckDivi(ctx); err == nil || errors.Is(err, ErrDiviRefused) {
		t.Errorf("server error: %v", err)
	}
	et.override = nil

	// Unreachable: the error names neither the address with the key nor the key.
	et.Close()
	err := h.svc.CheckDivi(ctx)
	if err == nil || strings.Contains(err.Error(), testETKey) || strings.Contains(err.Error(), "api_key") {
		t.Errorf("unreachable: %v", err)
	}
}

// TestDiviDownloadCap: a body past the cap is refused even without a
// Content-Length, and nothing is left behind.
func TestDiviDownloadCap(t *testing.T) {
	h, et := diviHarness(t)
	setLicense(t, h)
	et.override = func(w http.ResponseWriter) bool {
		w.Write([]byte("PK\x03\x04"))
		chunk := make([]byte, 1<<20)
		for range maxDiviZip>>20 + 2 {
			if _, err := w.Write(chunk); err != nil {
				break
			}
		}
		return true
	}
	l, _ := h.svc.Divi(context.Background())
	if _, err := h.svc.downloadDivi(context.Background(), "s1", l); err == nil || !strings.Contains(err.Error(), "MB") {
		t.Fatalf("oversize download: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(h.svc.Cfg.SiteDir("s1"), ".wpgenie-divi-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	// A zip that isn't Divi.
	et.override = func(w http.ResponseWriter) bool {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		zw.Create("astra/style.css")
		zw.Close()
		w.Write(buf.Bytes())
		return true
	}
	if _, err := h.svc.downloadDivi(context.Background(), "s1", l); err == nil || !strings.Contains(err.Error(), "isn't the Divi theme") {
		t.Fatalf("other theme: %v", err)
	}
}

// diviWP makes the fake runtime's WP-CLI install themes from zips (like
// `wp theme install <zip>`) and list them; it records every call.
func diviWP(t *testing.T, h *harness) *[][]string {
	t.Helper()
	var calls [][]string
	var mu sync.Mutex
	rt := h.rt
	h.svc.Runtime = &wpRuntime{fakeRuntime: rt, wp: func(id string, args []string) error {
		mu.Lock()
		calls = append(calls, slices.Clone(args))
		mu.Unlock()
		if len(args) >= 3 && args[0] == "theme" && args[1] == "install" {
			zr, err := zip.OpenReader(args[2])
			if err != nil {
				return err
			}
			defer zr.Close()
			for _, f := range zr.File {
				dst := filepath.Join(h.svc.Cfg.SiteRoot(id), "wp-content/themes", f.Name)
				os.MkdirAll(filepath.Dir(dst), 0o755)
				os.WriteFile(dst, []byte("x"), 0o644)
			}
		}
		return nil
	}}
	rt.exec = func(args []string, _ io.Reader, out io.Writer) error {
		if slices.Contains(args, "theme") && slices.Contains(args, "list") {
			io.WriteString(out, `[{"name":"Divi","title":"Divi","status":"active","version":"4.27.4","update":"none"},`+
				`{"name":"twentytwentyfive","title":"Twenty Twenty-Five","status":"inactive","version":"1.2","update":"none"}]`)
		}
		return nil
	}
	return &calls
}

type wpRuntime struct {
	*fakeRuntime
	wp func(id string, args []string) error
}

func (r *wpRuntime) WP(ctx context.Context, id string, stdin io.Reader, args ...string) ([]byte, error) {
	r.fakeRuntime.WP(ctx, id, stdin, args...)
	return nil, r.wp(id, args)
}

// noKeyIn fails when the key is anywhere in what's shown about a site.
func noKeyIn(t *testing.T, h *harness, id string, more ...string) {
	t.Helper()
	ev, _ := h.svc.Store.Events(context.Background(), id, 50)
	for _, e := range ev {
		more = append(more, e.Message)
	}
	more = append(more, *h.log...)
	for _, s := range more {
		if strings.Contains(s, testETKey) {
			t.Errorf("the key shows in %q", s)
		}
	}
}

func TestCreateInstallsDivi(t *testing.T) {
	h, _ := diviHarness(t)
	calls := diviWP(t, h)
	setLicense(t, h)
	ctx := context.Background()
	st, creds, err := h.svc.Create(ctx, CreateInput{Domain: "divi.test", AdminEmail: "a@divi.test"})
	if err != nil || creds == nil {
		t.Fatalf("create: %v", err)
	}
	var install []string
	for _, c := range *calls {
		if strings.Contains(strings.Join(c, " "), testETKey) {
			t.Errorf("the key is in WP-CLI's arguments: %q", c)
		}
		if len(c) > 1 && c[0] == "theme" && c[1] == "install" {
			install = c
		}
	}
	dir := h.svc.Cfg.SiteDir(st.ID)
	if len(install) != 4 || install[3] != "--activate" || filepath.Dir(install[2]) != dir || !strings.HasSuffix(install[2], ".zip") {
		t.Fatalf("install call %q", install)
	}
	if _, err := os.Stat(install[2]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the download was left in the site's directory: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, diviCredsFile))
	if err != nil || !strings.Contains(string(b), testETKey) {
		t.Errorf("credentials: %s %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, diviCredsFile)); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("credentials mode: %v %v", fi, err)
	}
	w, err := os.ReadFile(filepath.Join(h.svc.Cfg.SiteRoot(st.ID), diviWrapperPath))
	if err != nil || !strings.Contains(string(w), "/usr/local/share/wpgenie/divi.php") || strings.Contains(string(w), testETKey) {
		t.Errorf("wrapper: %s %v", w, err)
	}
	ev, _ := h.svc.Store.Events(ctx, st.ID, 10)
	if len(ev) == 0 || !strings.Contains(ev[0].Message, "Divi 4.27.4 installed and activated") {
		t.Errorf("events %+v", ev)
	}
	noKeyIn(t, h, st.ID)

	// Skipped for one site.
	*calls = nil
	st2, _, err := h.svc.Create(ctx, CreateInput{Domain: "plain.test", AdminEmail: "a@plain.test", Divi: boolp(false)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if c[0] == "theme" {
			t.Errorf("Divi installed on a site that skipped it: %q", c)
		}
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteDir(st2.ID), diviCredsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a site without Divi got the license: %v", err)
	}

	// Removing the license takes it off every site; setting it again puts
	// it back on those with Divi only.
	if _, err := h.svc.SetDivi(ctx, DiviInput{Username: strp("")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, diviCredsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("credentials kept after the license was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteRoot(st.ID), diviWrapperPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wrapper kept after the license was removed: %v", err)
	}
	setLicense(t, h)
	if _, err := os.Stat(filepath.Join(dir, diviCredsFile)); err != nil {
		t.Errorf("credentials not written back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteDir(st2.ID), diviCredsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a site without Divi got the license: %v", err)
	}
	// Restores, clones and updates rewrite the managed files: still there.
	if err := h.svc.rewriteManagedFiles(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteRoot(st.ID), diviWrapperPath)); err != nil {
		t.Errorf("wrapper gone after rewriting managed files: %v", err)
	}
}

// TestDiviFailureDoesNotFailCreate: a refused license (or no license) only
// costs the site its Divi, with the reason in its activity.
func TestDiviFailureDoesNotFailCreate(t *testing.T) {
	h, et := diviHarness(t)
	diviWP(t, h)
	setLicense(t, h)
	et.override = func(w http.ResponseWriter) bool { io.WriteString(w, "Invalid username or API key"); return true }
	ctx := context.Background()
	st, creds, err := h.svc.Create(ctx, CreateInput{Domain: "nodivi.test", AdminEmail: "a@nodivi.test"})
	if err != nil || creds == nil {
		t.Fatalf("create failed with Divi refused: %v", err)
	}
	got, err := h.svc.Store.GetSite(ctx, st.ID)
	if err != nil || got.Status != store.StatusActive {
		t.Fatalf("site %+v %v", got, err)
	}
	ev, _ := h.svc.Store.Events(ctx, st.ID, 10)
	found := slices.ContainsFunc(ev, func(e store.Event) bool {
		return strings.Contains(e.Message, "Divi couldn't be installed") && strings.Contains(e.Message, "refused")
	})
	if !found {
		t.Errorf("events %+v", ev)
	}
	if left, _ := filepath.Glob(filepath.Join(h.svc.Cfg.SiteDir(st.ID), ".wpgenie-divi-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	noKeyIn(t, h, st.ID)

	// Asked for explicitly without a license: the same.
	if _, err := h.svc.SetDivi(ctx, DiviInput{APIKey: strp("")}); err != nil {
		t.Fatal(err)
	}
	st, _, err = h.svc.Create(ctx, CreateInput{Domain: "nolicense.test", AdminEmail: "a@nolicense.test", Divi: boolp(true)})
	if err != nil {
		t.Fatalf("create failed without a license: %v", err)
	}
	ev, _ = h.svc.Store.Events(ctx, st.ID, 10)
	if !slices.ContainsFunc(ev, func(e store.Event) bool { return strings.Contains(e.Message, "no Divi license") }) {
		t.Errorf("events %+v", ev)
	}
}

func TestStartDiviInstall(t *testing.T) {
	h, _ := diviHarness(t)
	h.svc.Jobs = &jobs.Queue{Store: h.svc.Store, Log: slog.New(slog.DiscardHandler)}
	calls := diviWP(t, h)
	ctx := context.Background()
	if _, err := h.svc.StartDiviInstall(ctx, "s1"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("without a license: %v", err)
	}
	setLicense(t, h)
	run := func() *store.Job {
		t.Helper()
		id, err := h.svc.StartDiviInstall(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		j, err := h.svc.Jobs.WaitJob(ctx, id)
		if err != nil || j.Status != store.JobSucceeded || j.Kind != "divi-install" {
			t.Fatalf("job %+v %v", j, err)
		}
		if strings.Contains(j.Result+j.Error+j.Step, testETKey) {
			t.Errorf("the key is in the job: %+v", j)
		}
		return j
	}
	j := run()
	var th Theme
	if err := json.Unmarshal([]byte(j.Result), &th); err != nil || th.Slug != "Divi" || th.Status != "active" {
		t.Errorf("result %s %v", j.Result, err)
	}
	if !slices.ContainsFunc(*calls, func(c []string) bool { return len(c) > 1 && c[1] == "install" }) {
		t.Errorf("not installed: %q", *calls)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteDir("s1"), diviCredsFile)); err != nil {
		t.Errorf("credentials: %v", err)
	}
	// Installed already: only activated, nothing downloaded.
	*calls = nil
	run()
	if len(*calls) == 0 || !slices.Equal((*calls)[0], []string{"theme", "activate", "Divi"}) ||
		slices.ContainsFunc(*calls, func(c []string) bool { return len(c) > 1 && c[1] == "install" }) {
		t.Errorf("calls %q", *calls)
	}
	noKeyIn(t, h, "s1")

	// Deleting the theme takes the license away from the site.
	if err := os.RemoveAll(filepath.Join(h.svc.Cfg.SiteRoot("s1"), diviThemeDir)); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.writeDiviFiles(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteDir("s1"), diviCredsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("credentials kept without Divi: %v", err)
	}
}

// TestDiviWrapperIsManaged: the wrapper carries the marker (so updates and
// restores rewrite it, and the intrusion scan trusts it) and the PHP
// library loads the credentials file the daemon writes.
func TestDiviWrapperIsManaged(t *testing.T) {
	if !strings.Contains(diviWrapper, managedMarker) || !slices.Contains(managedFiles, diviWrapperPath) {
		t.Error("the Divi wrapper isn't a managed file")
	}
	php, err := os.ReadFile("../../images/php/divi.php")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'/" + diviCredsFile + "'", "WPGENIE_ET_USER", "WPGENIE_ET_KEY", "pre_option_", "pre_site_option_",
		"pre_update_option_", "et_automatic_updates_options", "et_divi_options", "defined( 'ABSPATH' ) || exit;"} {
		if !strings.Contains(string(php), want) {
			t.Errorf("divi.php lacks %q", want)
		}
	}
	docker, err := os.ReadFile("../../images/php/Dockerfile")
	if err != nil || !strings.Contains(string(docker), " divi.php ") {
		t.Errorf("the PHP image doesn't ship divi.php: %v", err)
	}
}
