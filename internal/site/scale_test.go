package site

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// fakeRuntime records every call in one log shared with fakeProxy, so tests
// can assert on ordering across the two (e.g. drain only after the switch).
type fakeRuntime struct {
	log        *[]string
	containers map[string]runtime.Replica
	imageID    string
	failStart  int // fail the Nth StartReplica call (1-based); 0 = never
	starts     int
	busy       map[string]int // replica -> polls that still report a request
	onBusy     func()
	cpu        map[string]float64
	// exec simulates commands run with Exec (see fakeWP in updates_test.go).
	exec func(args []string, stdin io.Reader, stdout io.Writer) error
	// onStart sees the image of every replica started; lastSpec is the
	// spec of the last one.
	onStart  func(image string)
	lastSpec runtime.SiteSpec
}

func (f *fakeRuntime) ImageID(context.Context, string) (string, error) { return f.imageID, nil }

func (f *fakeRuntime) StartReplica(_ context.Context, spec runtime.SiteSpec, port int) error {
	f.starts++
	f.lastSpec = spec
	if f.onStart != nil {
		f.onStart(spec.Image)
	}
	name := runtime.ContainerName(spec.ID, port)
	*f.log = append(*f.log, "start "+name)
	f.containers[name] = runtime.Replica{Name: name, Port: port, SpecHash: spec.Hash(), Running: true}
	if f.starts == f.failStart {
		return errors.New("port already allocated")
	}
	return nil
}

func (f *fakeRuntime) Ready(_ context.Context, name string) (bool, error) {
	return f.containers[name].Running, nil
}

func (f *fakeRuntime) ActiveConnections(_ context.Context, name string) (int, error) {
	if f.busy[name] > 0 {
		f.busy[name]--
		if f.onBusy != nil {
			f.onBusy()
		}
		*f.log = append(*f.log, "busy "+name)
		return 1, nil
	}
	return 0, nil
}

func (f *fakeRuntime) StopReplica(_ context.Context, name string) error {
	*f.log = append(*f.log, "stop "+name)
	delete(f.containers, name)
	return nil
}

func (f *fakeRuntime) Replicas(_ context.Context, id string) ([]runtime.Replica, error) {
	var out []runtime.Replica
	for name, r := range f.containers {
		if strings.HasPrefix(name, "wpg-"+id) { // id "" lists every site
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b runtime.Replica) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *fakeRuntime) RemoveSite(context.Context, string) error { return nil }
func (f *fakeRuntime) WP(_ context.Context, _ string, _ io.Reader, args ...string) ([]byte, error) {
	*f.log = append(*f.log, "wp "+strings.Join(args, " "))
	return nil, nil
}
func (f *fakeRuntime) RunCron(context.Context, runtime.SiteSpec) ([]byte, error) { return nil, nil }
func (f *fakeRuntime) Exec(_ context.Context, _ string, stdin io.Reader, stdout io.Writer, args ...string) error {
	if f.exec == nil {
		return nil
	}
	if stdout == nil {
		stdout = io.Discard
	}
	return f.exec(args, stdin, stdout)
}
func (f *fakeRuntime) CPUUsage(context.Context) (map[string]float64, error) {
	out := map[string]float64{}
	for name, c := range f.containers {
		if v, ok := f.cpu[name]; ok && c.Running {
			out[name] = v
		}
	}
	return out, nil
}

type fakeProxy struct {
	log     *[]string
	rejects int // reject (and don't apply) the next N configs, like Caddy on a bad config
	last    []proxy.Site
}

func (p *fakeProxy) Apply(_ context.Context, sites []proxy.Site) error {
	if p.rejects > 0 {
		p.rejects--
		return errors.New("caddy rejected config")
	}
	var ups []string
	for _, s := range sites {
		ups = append(ups, s.Upstreams...)
	}
	*p.log = append(*p.log, "proxy "+strings.Join(ups, ","))
	p.last = sites
	return nil
}

type fakeDB struct {
	limits map[string]int
	tables *[]string
}

func (fakeDB) CreateSiteDB(context.Context, string, string, string) error { return nil }
func (fakeDB) DropSiteDB(context.Context, string, string) error           { return nil }
func (d fakeDB) Tables(context.Context, string) ([]string, error) {
	return slices.Clone(*d.tables), nil
}
func (d fakeDB) DropTables(_ context.Context, _ string, drop []string) error {
	*d.tables = slices.DeleteFunc(*d.tables, func(t string) bool { return slices.Contains(drop, t) })
	return nil
}
func (fakeDB) CreateTempUser(context.Context, string, string, string) error { return nil }
func (fakeDB) DropTempUser(context.Context, string) error                   { return nil }
func (d fakeDB) SetConnectionLimit(_ context.Context, user string, n int) error {
	d.limits[user] = n
	return nil
}

type fakeCache struct{ flushed *[]string }

func (c fakeCache) FlushPrefix(_ context.Context, prefix string) error {
	*c.flushed = append(*c.flushed, prefix)
	return nil
}

type harness struct {
	svc     *Service
	rt      *fakeRuntime
	proxy   *fakeProxy
	db      fakeDB
	log     *[]string
	flushed *[]string
}

// newHarness has one active site "s1" served by a current replica on 19000.
func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	log := &[]string{}
	h := &harness{
		rt: &fakeRuntime{log: log, containers: map[string]runtime.Replica{}, imageID: "sha256:v1",
			busy: map[string]int{}},
		proxy: &fakeProxy{log: log},
		db:    fakeDB{limits: map[string]int{}, tables: &[]string{"wp_options", "wp_posts"}},
		log:   log,
	}
	h.flushed = &[]string{}
	h.svc = &Service{Cfg: cfg, Store: st, Runtime: h.rt, DB: h.db, Proxy: h.proxy,
		Cache: fakeCache{h.flushed}, Log: slog.New(slog.DiscardHandler)}
	ctx := context.Background()
	site := &store.Site{ID: "s1", Name: "s1", PrimaryDomain: "a.test", PHPVersion: "8.3", FPMPort: 19000,
		DBName: "wp_s1", Status: store.StatusActive, ShieldMode: "standard",
		MemoryMB: 512, CPUs: 1, Replicas: 1, Upstreams: []int{19000}}
	if err := st.CreateSite(ctx, site); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.SiteRoot("s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec, _ := h.svc.specFor(ctx, site)
	h.rt.StartReplica(ctx, spec, 19000)
	*log = nil
	return h
}

func (h *harness) names() []string {
	var out []string
	for n := range h.rt.containers {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func (h *harness) upstreams(t *testing.T) []int {
	st, err := h.svc.Store.GetSite(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	return st.Upstreams
}

func TestScaleOutKeepsCurrentReplicas(t *testing.T) {
	h := newHarness(t)
	st, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 3})
	if err != nil {
		t.Fatal(err)
	}
	// 19000 is both the reserved port and the current upstream: new replicas
	// get the next free ports and the existing one is not restarted.
	if want := []int{19000, 19001, 19002}; !slices.Equal(st.Upstreams, want) {
		t.Fatalf("upstreams = %v, want %v", st.Upstreams, want)
	}
	if slices.ContainsFunc(*h.log, func(l string) bool { return strings.HasPrefix(l, "stop") }) {
		t.Errorf("scaling out must not stop a current replica: %v", *h.log)
	}
	want := "proxy 127.0.0.1:19000,127.0.0.1:19001,127.0.0.1:19002"
	if last := (*h.log)[len(*h.log)-1]; last != want {
		t.Errorf("last action %q, want %q", last, want)
	}
	if got := h.db.limits["u_s1"]; got != dbConnLimit(3, runtime.FPMMaxChildren(512)) {
		t.Errorf("db connection limit = %d", got)
	}
}

func TestResizeIsBlueGreen(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 1024, CPUs: 2, Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start wpg-s1-19001", "start wpg-s1-19002", // new spec comes up first,
		"proxy 127.0.0.1:19001,127.0.0.1:19002", //   traffic switches atomically,
		"stop wpg-s1-19000",                     //                       then the old replica drains.
	}
	if !slices.Equal(*h.log, want) {
		t.Fatalf("actions:\n got %v\nwant %v", *h.log, want)
	}
	st, _ := h.svc.Store.GetSite(context.Background(), "s1")
	if st.MemoryMB != 1024 || st.CPUs != 2 || st.Replicas != 2 {
		t.Errorf("resources not saved: %+v", st)
	}
}

func TestOldReplicaDrainsBeforeStop(t *testing.T) {
	h := newHarness(t)
	h.rt.busy["wpg-s1-19000"] = 2 // two more polls see a request in flight
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start wpg-s1-19001", "proxy 127.0.0.1:19001",
		"busy wpg-s1-19000", "busy wpg-s1-19000", // traffic already moved: wait out in-flight requests
		"stop wpg-s1-19000",
	}
	if !slices.Equal(*h.log, want) {
		t.Fatalf("actions:\n got %v\nwant %v", *h.log, want)
	}
}

func TestDrainGivesUpAtTimeout(t *testing.T) {
	h := newHarness(t)
	defer func(d time.Duration) { drainTimeout = d }(drainTimeout)
	drainTimeout = 300 * time.Millisecond
	h.rt.busy["wpg-s1-19000"] = 1 << 30 // a request that never finishes
	start := time.Now()
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("drain did not respect its timeout")
	}
	if !slices.Equal(h.names(), []string{"wpg-s1-19001"}) {
		t.Errorf("stuck replica must still be removed after the timeout: %v", h.names())
	}
}

func TestScaleInRetiresExtraReplicas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.Scale(ctx, "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 3})
	*h.log = nil
	if _, err := h.svc.Scale(ctx, "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if got := h.upstreams(t); len(got) != 1 {
		t.Fatalf("upstreams = %v, want one", got)
	}
	if len(h.names()) != 1 || strings.HasPrefix((*h.log)[0], "start") {
		t.Errorf("scale in must reuse a replica, not start one: log=%v containers=%v", *h.log, h.names())
	}
}

func TestImageRebuildRollsReplicas(t *testing.T) {
	h := newHarness(t)
	h.rt.imageID = "sha256:v2" // installer rebuilt wpgenie/php
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.names(), []string{"wpg-s1-19001"}) {
		t.Fatalf("containers = %v, want the old-image replica replaced", h.names())
	}
}

func TestLegacyContainerIsRetired(t *testing.T) {
	h := newHarness(t)
	// A container from before replicas existed: no port/spec labels.
	h.rt.containers = map[string]runtime.Replica{"wpg-s1": {Name: "wpg-s1", Running: true}}
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.names(), []string{"wpg-s1-19001"}) {
		t.Fatalf("containers = %v", h.names())
	}
}

func TestFailedStartLeavesSiteUntouched(t *testing.T) {
	h := newHarness(t)
	h.rt.failStart = 2
	_, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 2})
	if err == nil {
		t.Fatal("expected error")
	}
	if !slices.Equal(h.names(), []string{"wpg-s1-19000"}) {
		t.Errorf("containers = %v, want only the original (both new ones cleaned up)", h.names())
	}
	if got := h.upstreams(t); !slices.Equal(got, []int{19000}) {
		t.Errorf("upstreams = %v, want unchanged", got)
	}
	st, _ := h.svc.Store.GetSite(context.Background(), "s1")
	if st.MemoryMB != 512 || st.Replicas != 1 {
		t.Errorf("resources must be restored after a failed scale: %+v", st)
	}
	if slices.ContainsFunc(*h.log, func(l string) bool { return strings.HasPrefix(l, "proxy") }) {
		t.Error("proxy must not be touched when replicas fail to start")
	}
}

func TestFailedProxySyncKeepsOldReplicas(t *testing.T) {
	h := newHarness(t)
	h.proxy.rejects = 1
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 1}); err == nil {
		t.Fatal("expected error")
	}
	if !slices.Equal(h.names(), []string{"wpg-s1-19000"}) {
		t.Errorf("containers = %v, want the original still serving", h.names())
	}
	if got := h.upstreams(t); !slices.Equal(got, []int{19000}) {
		t.Errorf("upstreams = %v, want restored", got)
	}
}

// Apply can fail after Caddy switched (persisting the Caddyfile failed):
// the new replicas may be live, so only stop them once Caddy is back on the
// old ones.
type flakyProxy struct {
	fakeProxy
	failures int
}

func (p *flakyProxy) Apply(ctx context.Context, sites []proxy.Site) error {
	p.fakeProxy.Apply(ctx, sites) // the switch happens either way
	if p.failures > 0 {
		p.failures--
		return errors.New("write Caddyfile: read-only file system")
	}
	return nil
}

func TestPartialProxyFailureRestoresBeforeCleanup(t *testing.T) {
	h := newHarness(t)
	fp := &flakyProxy{fakeProxy: fakeProxy{log: h.log}, failures: 1}
	h.svc.Proxy = fp
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 1}); err == nil {
		t.Fatal("expected error")
	}
	want := []string{"start wpg-s1-19001", "proxy 127.0.0.1:19001", "proxy 127.0.0.1:19000", "stop wpg-s1-19001"}
	if !slices.Equal(*h.log, want) {
		t.Fatalf("actions:\n got %v\nwant %v", *h.log, want)
	}
}

func TestProxyStateUnknownKeepsBothSets(t *testing.T) {
	h := newHarness(t)
	h.svc.Proxy = &flakyProxy{fakeProxy: fakeProxy{log: h.log}, failures: 2} // the restore fails too
	if _, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 1}); err == nil {
		t.Fatal("expected error")
	}
	if !slices.Equal(h.names(), []string{"wpg-s1-19000", "wpg-s1-19001"}) {
		t.Fatalf("with the proxy state unknown, nothing it may route to can be stopped: %v", h.names())
	}
}

func TestLeakedContainerPortNotReused(t *testing.T) {
	h := newHarness(t)
	// Left behind by an interrupted drain of another site; not in the store.
	h.rt.containers["wpg-s9-19001"] = runtime.Replica{Name: "wpg-s9-19001", Port: 19001, Running: true}
	st, err := h.svc.Scale(context.Background(), "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(st.Upstreams, 19001) {
		t.Fatalf("allocated a port still published by a container: %v", st.Upstreams)
	}
}

func TestPostSwitchWorkSurvivesCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.rt.busy["wpg-s1-19000"] = 1
	h.rt.onBusy = cancel // the client disconnects while the old replica drains
	if _, err := h.svc.Scale(ctx, "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 1}); err != nil {
		t.Logf("Scale returned %v after cancel (acceptable)", err)
	}
	if !slices.Equal(h.names(), []string{"wpg-s1-19001"}) {
		t.Fatalf("old replica must still be retired after the caller goes away: %v", h.names())
	}
}

func TestScaleValidation(t *testing.T) {
	h := newHarness(t)
	for _, r := range []Resources{
		{MemoryMB: 128, CPUs: 1, Replicas: 1},
		{MemoryMB: 512, CPUs: 0.1, Replicas: 1},
		{MemoryMB: 512, CPUs: 1, Replicas: 0},
		{MemoryMB: 512, CPUs: 1, Replicas: 99},
	} {
		if _, err := h.svc.Scale(context.Background(), "s1", r); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("Scale(%+v) = %v, want ErrInvalidInput", r, err)
		}
	}
	// A site may not take more than half of MariaDB's connections.
	h.svc.Cfg.DBMaxConnections = 20
	r := Resources{MemoryMB: 512, CPUs: 1, Replicas: 3}
	if _, err := h.svc.Scale(context.Background(), "s1", r); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("connection budget not enforced: %v", err)
	}
}

func TestEnsureManagedRefusesSymlinkEscape(t *testing.T) {
	docroot, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(docroot, "wp-content"), 0o755)
	// A compromised site points mu-plugins at a directory outside its docroot.
	if err := os.Symlink(outside, filepath.Join(docroot, "wp-content", "mu-plugins")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(docroot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureManaged(root, pageCacheWrapperPath, pageCacheWrapper, true); err == nil {
		t.Error("write through a symlink leaving the docroot must fail")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the docroot: %v", entries)
	}
}

func TestEnsureManagedLeavesForeignFilesAlone(t *testing.T) {
	docroot := t.TempDir()
	os.MkdirAll(filepath.Join(docroot, "wp-content"), 0o755)
	foreign := filepath.Join(docroot, objectCacheDropIn)
	os.WriteFile(foreign, []byte("<?php // W3 Total Cache"), 0o644)
	root, _ := os.OpenRoot(docroot)
	defer root.Close()

	if err := ensureManaged(root, objectCacheDropIn, objectCacheWrapper, true); !errors.Is(err, ErrConflict) {
		t.Errorf("enabling over another plugin's drop-in = %v, want ErrConflict", err)
	}
	if err := ensureManaged(root, objectCacheDropIn, objectCacheWrapper, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "<?php // W3 Total Cache" {
		t.Error("disabling must not remove a drop-in WPGenie did not write")
	}

	// Our own file: written, then removed.
	if err := ensureManaged(root, pageCacheWrapperPath, pageCacheWrapper, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(docroot, pageCacheWrapperPath)); string(b) != pageCacheWrapper {
		t.Fatal("wrapper not written")
	}
	if err := ensureManaged(root, pageCacheWrapperPath, pageCacheWrapper, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(docroot, pageCacheWrapperPath)); !os.IsNotExist(err) {
		t.Error("wrapper not removed")
	}
}

func TestPurgeFlushesOnlyThisSitesKeys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Purge(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if len(*h.flushed) != 0 {
		t.Errorf("object cache off: nothing to flush, got %v", *h.flushed)
	}
	h.svc.Store.SetCache(ctx, "s1", false, true)
	if err := h.svc.Purge(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*h.flushed, []string{"s1:"}) {
		t.Errorf("flushed %v, want only the site's own prefix", *h.flushed)
	}
	if slices.ContainsFunc(*h.log, func(l string) bool { return strings.HasPrefix(l, "wp ") }) {
		t.Error("purge must not run WP-CLI: it would load the site's drop-in outside the PHP jail")
	}
}

func TestPurgePageCacheFiles(t *testing.T) {
	h := newHarness(t)
	docroot := h.svc.Cfg.SiteRoot("s1")
	page := filepath.Join(docroot, pageCacheDir, "about", "index.html")
	os.MkdirAll(filepath.Dir(page), 0o755)
	os.WriteFile(page, []byte("<html></html>"), 0o644)

	if err := h.svc.purgePageCacheFiles("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(docroot, pageCacheDir)); !os.IsNotExist(err) {
		t.Error("cache directory still exists")
	}
	if _, err := os.Stat(filepath.Join(docroot, pageCacheMarker)); err != nil {
		t.Errorf("purge marker not written: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(docroot, pageCacheDir+".trash-*"))
	if len(matches) != 0 {
		t.Errorf("trash left behind: %v", matches)
	}
	// Nothing cached yet (fresh site): not an error.
	os.RemoveAll(filepath.Join(docroot, "wp-content"))
	if err := h.svc.purgePageCacheFiles("s1"); err != nil {
		t.Errorf("purge of a never-cached site: %v", err)
	}
}
