package site

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// ---- Autoscaling on PHP workers and response times ----

func TestDesiredOverSeveralMetrics(t *testing.T) {
	p := autoscalePolicy{min: 1, max: 8, target: 0.7, workers: 0.8, ms: 500}
	for _, c := range []struct {
		name    string
		current int
		s       cpuSample
		want    int
		why     string
	}{
		{"quiet", 2, cpuSample{util: 0.3, workers: 0.3, n: 100, p95: 100}, 1, "cpu"},
		// Waiting on an external API: CPU idle, every worker busy and 15
		// requests queued behind 5 workers per replica.
		{"queued", 2, cpuSample{util: 0.1, workers: 2.5, n: 100, p95: 100}, 7, "workers"},
		{"workers unknown", 2, cpuSample{util: 0.7, workers: -1, n: 0}, 2, "cpu"},
		// Slow and busy: one more replica at a time.
		{"slow under load", 2, cpuSample{util: 0.6, workers: 0.75, n: 100, p95: 900}, 3, "latency"},
		// Slow but idle: slow code, not a lack of replicas.
		{"slow when idle", 2, cpuSample{util: 0.2, workers: 0.2, n: 100, p95: 3000}, 1, "cpu"},
		// A handful of slow requests is no signal.
		{"few samples", 2, cpuSample{util: 0.6, workers: 0.75, n: 5, p95: 3000}, 2, "cpu"},
	} {
		got, why := p.desired(c.current, c.s)
		if got != c.want || why != c.why {
			t.Errorf("%s: desired = %d (%s), want %d (%s)", c.name, got, why, c.want, c.why)
		}
	}
	// Off metrics never count.
	off := autoscalePolicy{min: 1, max: 8, target: 0.7}
	if got, _ := off.desired(2, cpuSample{util: 0.7, workers: 3, n: 100, p95: 9000}); got != 2 {
		t.Errorf("workers and latency off: desired %d", got)
	}
}

func TestScaleDownWaitsForEveryMetric(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	sc := &scaler{policy: autoscalePolicy{min: 1, max: 4, target: 0.7, workers: 0.8}, since: t0.Add(-time.Hour)}
	now := t0
	for i := range int(scaleDownWindow/autoscaleInterval) + 1 {
		now = t0.Add(time.Duration(i) * autoscaleInterval)
		workers := 0.2
		if i == 3 {
			workers = 1.6 // one burst of queued requests in the window
		}
		sc.observe(cpuSample{at: now, util: 0.1, replicas: 2, workers: workers})
	}
	if want, _ := sc.next(now, 2); want != 2 {
		t.Fatalf("scaled down to %d with a worker burst in the window", want)
	}
}

type fakeLatency map[string][]float64

func (f fakeLatency) Percentile(site string, _ time.Time, _ float64) (float64, int) {
	v := f[site]
	if len(v) == 0 {
		return 0, 0
	}
	return slices.Max(v), len(v)
}

func TestAutoscalerTickScalesOnQueuedRequests(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{Enabled: true, MinReplicas: 1, MaxReplicas: 4,
		TargetCPU: 70, TargetWorkers: 80}); err != nil {
		t.Fatal(err)
	}
	name := runtime.ContainerName("s1", 19000)
	h.rt.cpu = map[string]float64{name: 5} // waiting, not computing
	workers := runtime.FPMMaxChildren(512)
	h.rt.fpm = map[string]runtime.FPMLoad{name: {Requests: 2 * workers, Queued: workers}}
	h.svc.Latency = fakeLatency{"s1": {120, 180}}
	scalers := map[string]*scaler{}
	t0 := time.Now()
	h.svc.autoscaleTick(ctx, t0, scalers)
	h.svc.autoscaleTick(ctx, t0.Add(autoscaleInterval), scalers)
	waitIdle(t, scalers["s1"])
	if got := h.upstreams(t); len(got) != 3 {
		t.Fatalf("upstreams %v: 200%% of the workers asked for ceil(1 × 2/0.8) = 3", got)
	}
	r, ok := h.svc.CPU("s1")
	if !ok || r.Workers == nil || *r.Workers != 200 || r.Queued != workers || r.P95MS == nil || *r.P95MS != 180 {
		t.Fatalf("reading %+v", r)
	}
	events, _ := h.svc.Store.Events(ctx, "s1", 5)
	if !strings.Contains(events[0].Message, "PHP workers 200% busy") {
		t.Errorf("event %q", events[0].Message)
	}
	for _, bad := range []AutoscaleSettings{
		{Enabled: true, MinReplicas: 1, MaxReplicas: 2, TargetCPU: 70, TargetWorkers: 5},
		{Enabled: true, MinReplicas: 1, MaxReplicas: 2, TargetCPU: 70, TargetResponseMS: 10},
	} {
		if _, err := h.svc.SetAutoscale(ctx, "s1", bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
}

// ---- Page cache: separate mobile copies ----

func TestMobileCacheSetting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	root := h.svc.Cfg.SiteRoot("s1")
	stale := filepath.Join(root, pageCacheDir, "index.html")
	on := true
	if _, err := h.svc.SetCache(ctx, "s1", CacheSettings{PageCache: true}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("cached for every device"), 0o644)
	st, err := h.svc.SetCache(ctx, "s1", CacheSettings{PageCache: true, Mobile: &on})
	if err != nil || !st.CacheMobile {
		t.Fatalf("%+v %v", st, err)
	}
	b, _ := os.ReadFile(filepath.Join(root, pageCacheWrapperPath))
	if !strings.Contains(string(b), "WPGENIE_CACHE_MOBILE_ALWAYS") {
		t.Errorf("wrapper:\n%s", b)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("pages stored for every device survived the switch to separate copies")
	}
	// Omitted: unchanged (older API clients).
	if st, _ := h.svc.SetCache(ctx, "s1", CacheSettings{PageCache: true, ObjectCache: true}); !st.CacheMobile {
		t.Error("an omitted setting turned separate copies off")
	}
}

// ---- Images ----

func withJobs(h *harness) {
	h.svc.Jobs = &jobs.Queue{Store: h.svc.Store, Log: slog.New(slog.DiscardHandler)}
}

func TestSetImagesConvertsAsAJob(t *testing.T) {
	h := newHarness(t)
	withJobs(h)
	ctx := context.Background()
	var ran []string
	h.rt.exec = func(args []string, _ io.Reader, stdout io.Writer) error {
		ran = args
		fmt.Fprint(stdout, `{"done":0,"total":2}`+"\n"+`{"done":2,"total":2}`+"\n"+
			`{"summary":{"images":2,"converted":3,"current":0,"larger":1,"skipped":0,"removed":0,"bytes_before":2097152,"bytes_after":1048576,"formats":["avif","webp"]}}`+"\n")
		return nil
	}
	if _, _, err := h.svc.SetImages(ctx, "s1", ImagesInput{Formats: []string{"jxl"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown format: %v", err)
	}
	st, jobID, err := h.svc.SetImages(ctx, "s1", ImagesInput{Formats: []string{"webp", "avif"}})
	if err != nil || jobID == 0 || !slices.Equal(st.ImageFormats, []string{"avif", "webp"}) {
		t.Fatalf("%+v %d %v", st, jobID, err)
	}
	j, err := h.svc.Jobs.WaitJob(ctx, jobID)
	if err != nil || j.Status != store.JobSucceeded || !strings.Contains(j.Result, `"converted":3`) {
		t.Fatalf("job %+v %v", j, err)
	}
	if !slices.Contains(ran, convertScript) || ran[len(ran)-1] != "avif,webp" || !strings.Contains(strings.Join(ran, " "), "nice -n 19") {
		t.Errorf("ran %q", ran)
	}
	b, _ := os.ReadFile(filepath.Join(h.svc.Cfg.SiteRoot("s1"), imagesWrapperPath))
	if !strings.Contains(string(b), "define( 'WPGENIE_IMAGE_FORMATS', 'avif,webp' )") {
		t.Errorf("wrapper:\n%s", b)
	}
	if got := h.proxy.last[0].Images; !slices.Equal(got, []string{"avif", "webp"}) {
		t.Errorf("proxy images %v", got)
	}
	// Unchanged: nothing to convert.
	if _, jobID, _ := h.svc.SetImages(ctx, "s1", ImagesInput{Formats: []string{"avif", "webp"}}); jobID != 0 {
		t.Error("a job for unchanged settings")
	}
	// Off: the wrapper goes, a job removes the copies.
	st, jobID, _ = h.svc.SetImages(ctx, "s1", ImagesInput{})
	h.svc.Jobs.WaitJob(ctx, jobID)
	if len(st.ImageFormats) != 0 || ran[len(ran)-1] != "" || len(h.proxy.last[0].Images) != 0 {
		t.Errorf("off: %v, ran %q", st.ImageFormats, ran)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteRoot("s1"), imagesWrapperPath)); err == nil {
		t.Error("wrapper left behind")
	}
}

func TestImageConversionOnOldImage(t *testing.T) {
	h := newHarness(t)
	h.rt.exec = func([]string, io.Reader, io.Writer) error {
		return fmt.Errorf("docker exec: %w", exec.Command("sh", "-c", "exit 99").Run())
	}
	st := mustSite(t, h.svc.Store, "s1")
	st.ImageFormats = []string{"webp"}
	if _, err := h.svc.convertImages(context.Background(), st, noProgress, convertTimeout); !errors.Is(err, ErrConflict) ||
		!strings.Contains(err.Error(), "wpgenie site scale s1") {
		t.Fatalf("old image: %v", err)
	}
}

// ---- CDN: pull zones and edge caching ----

func TestPullZoneCDN(t *testing.T) {
	h, _ := cdnHarness(t)
	b := &fakeBunny{origin: "https://a.test"}
	h.svc.Bunny = b
	h.svc.DNS = fakeResolver{"a.test": {"203.0.113.5"}} // the site itself isn't behind a CDN
	ctx := context.Background()
	for _, bad := range []CDNInput{
		{Provider: "bunny", APIToken: bunnyKey, PullZone: "42", AssetHost: "a.test"},     // the site itself
		{Provider: "bunny", APIToken: bunnyKey, PullZone: "42", AssetHost: "img.a.test"}, // not on the zone
		{Provider: "bunny", APIToken: bunnyKey, PullZone: "7", AssetHost: "cdn.a.test"},  // no such zone
		{Provider: "bunny", APIToken: "ffffffffffffffffffff-00", PullZone: "42", AssetHost: "cdn.a.test"},
		{Provider: "bunny", APIToken: bunnyKey, PullZone: "x", AssetHost: "cdn.a.test"},
	} {
		if _, err := h.svc.SetCDN(ctx, "s1", bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	root := h.svc.Cfg.SiteRoot("s1")
	cached := filepath.Join(root, pageCacheDir, "index.html")
	os.MkdirAll(filepath.Dir(cached), 0o755)
	os.WriteFile(cached, []byte("links to the origin"), 0o644)
	st, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "bunny", APIToken: bunnyKey, PullZone: "42", AssetHost: "CDN.a.test"})
	if err != nil || st.AssetHost != "cdn.a.test" || b.purges != 1 || len(st.Warnings) != 0 {
		t.Fatalf("%+v %v (purges %d)", st, err, b.purges)
	}
	w, _ := os.ReadFile(filepath.Join(root, cdnWrapperPath))
	if !strings.Contains(string(w), "define( 'WPGENIE_CDN_URL', 'https://cdn.a.test' )") {
		t.Errorf("wrapper:\n%s", w)
	}
	if _, err := os.Stat(cached); err == nil {
		t.Error("cached pages still link to the origin")
	}
	if !h.proxy.last[0].AssetCDN {
		t.Error("Caddy doesn't know about the pull zone (fonts need CORS)")
	}
	// The site's cache purges go to the pull zone too.
	if err := h.svc.Purge(ctx, "s1"); err != nil || b.purges != 2 {
		t.Fatalf("purge: %v (%d)", err, b.purges)
	}
	// The key is kept when not given again.
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "bunny", PullZone: "42", AssetHost: "cdn.a.test"}); err != nil {
		t.Fatal(err)
	}
	b.origin = "https://elsewhere.test"
	if st, _ := h.svc.CDNStatus(ctx, "s1"); len(st.Warnings) == 0 || !strings.Contains(st.Warnings[0], "origin") {
		t.Errorf("wrong origin not reported: %v", st.Warnings)
	}
	// Generic pull zone: no API at all.
	st, err = h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "generic", AssetHost: "static.example.net"})
	if err != nil || st.Provider != "generic" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := h.svc.Purge(ctx, "s1"); err != nil {
		t.Fatalf("purge with a generic CDN: %v", err)
	}
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, cdnWrapperPath)); err == nil || h.proxy.last[0].AssetCDN {
		t.Error("the CDN wrapper or CORS survived turning the CDN off")
	}
}

func TestCloudflareEdgeHTML(t *testing.T) {
	h, f := cdnHarness(t)
	ctx := context.Background()
	f.ruleErr = fmt.Errorf("%w: missing Cache Rules: Edit", cdn.ErrAuth)
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken, EdgeHTML: true}); !errors.Is(err, ErrInvalidInput) ||
		!strings.Contains(err.Error(), "Cache Rules: Edit") {
		t.Fatalf("token without cache rules: %v", err)
	}
	f.ruleErr = nil
	st, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken, EdgeHTML: true})
	if err != nil || !st.EdgeHTML {
		t.Fatalf("%+v %v", st, err)
	}
	if e := f.rules["z-test"]; !strings.Contains(e, `http.host in {"a.test"}`) || !strings.Contains(e, `not http.cookie contains "wordpress_logged_in_"`) {
		t.Fatalf("rule %q", e)
	}
	if !h.proxy.last[0].EdgeHTML {
		t.Error("Caddy doesn't mark cached pages cacheable at the edge")
	}
	// A new domain reaches the rule on the next pass.
	if _, err := h.svc.AddDomain(ctx, "s1", "www.a.test", false); err != nil {
		t.Fatal(err)
	}
	h.svc.cdnPass(ctx)
	if e := f.rules["z-test"]; !strings.Contains(e, `"www.a.test"`) {
		t.Errorf("rule after adding a domain: %q", e)
	}
	// Edge caching off: the rule goes, purging stays.
	st, err = h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare"})
	if err != nil || st.EdgeHTML || len(f.rules) != 0 || h.proxy.last[0].EdgeHTML {
		t.Fatalf("%+v %v rules %v", st, err, f.rules)
	}
}

// ---- PHP errors ----

func TestParsePHPLog(t *testing.T) {
	const root, dir = "/var/lib/wpgenie/sites/s1/public", "/var/lib/wpgenie/sites/s1"
	log := `[29-Sep-2026 10:00:00 UTC] PHP Warning:  Undefined variable $x in /var/lib/wpgenie/sites/s1/public/wp-content/plugins/shop/cart.php on line 12
[29-Sep-2026 10:00:05 UTC] PHP Warning:  Undefined variable $x in /var/lib/wpgenie/sites/s1/public/wp-content/plugins/shop/cart.php on line 12
[29-Sep-2026 10:01:00 UTC] PHP Fatal error:  Uncaught TypeError: count(): Argument #1 ($value) must be of type Countable|array, null given in /var/lib/wpgenie/sites/s1/public/wp-includes/class-wp-hook.php:324
Stack trace:
#0 /var/lib/wpgenie/sites/s1/public/wp-includes/plugin.php(517): WP_Hook->do_action(Array)
#1 /var/lib/wpgenie/sites/s1/public/wp-content/themes/fancy/functions.php(88): do_action('init')
#2 {main}
  thrown in /var/lib/wpgenie/sites/s1/public/wp-includes/class-wp-hook.php on line 324
[29-Sep-2026 10:02:00 UTC] PHP Deprecated:  Creation of dynamic property Foo::$bar is deprecated in /var/lib/wpgenie/sites/s1/public/wp-content/mu-plugins/x.php on line 3
[29-Sep-2026 10:03:00 UTC] my plugin says hello
`
	errs := parsePHPLog([]byte(log), root, dir, time.Now())
	if len(errs) != 4 {
		t.Fatalf("%d kinds: %+v", len(errs), errs)
	}
	w := errs[0]
	if w.Level != "warning" || w.Message != "Undefined variable $x" || w.File != "wp-content/plugins/shop/cart.php" ||
		w.Line != 12 || w.Source != "plugin:shop" || w.Count != 2 || !w.LastSeen.After(w.FirstSeen) {
		t.Errorf("warning %+v", w)
	}
	if f := errs[1]; f.Level != "fatal error" || f.File != "wp-includes/class-wp-hook.php" || f.Line != 324 || f.Source != "theme:fancy" {
		t.Errorf("fatal (core, caused by the theme) %+v", f)
	}
	if d := errs[2]; d.Source != "mu-plugin" || d.Level != "deprecated" {
		t.Errorf("deprecated %+v", d)
	}
	if l := errs[3]; l.Level != "log" || l.Message != "my plugin says hello" || l.File != "" {
		t.Errorf("error_log() line %+v", l)
	}
	if errs[0].Fingerprint == errs[1].Fingerprint || len(errs[0].Fingerprint) != 16 {
		t.Error("fingerprints")
	}
}

func TestIngestPHPErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := h.svc.Cfg.SiteDir("s1")
	logf := filepath.Join(dir, phpLogPath)
	line := "[29-Sep-2026 10:00:00 UTC] PHP Notice:  Something in " + h.svc.Cfg.SiteRoot("s1") + "/wp-content/plugins/p/p.php on line 1\n"
	var teed []string
	h.svc.PHPLogTee = func(id string, lines []byte) { teed = append(teed, id+"|"+string(lines)) }
	h.svc.ingestPHPErrors(ctx, "s1") // creates logs/
	if err := os.WriteFile(logf, []byte(line+line+"[29-Sep-2026 10:00:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.svc.ingestPHPErrors(ctx, "s1")
	h.svc.ingestPHPErrors(ctx, "s1") // nothing new: no double counting
	errs, _ := h.svc.Store.PHPErrors(ctx, "s1", time.Time{}, 10)
	if len(errs) != 1 || errs[0].Count != 2 || errs[0].Source != "plugin:p" {
		t.Fatalf("%+v", errs)
	}
	// Log shipping gets the whole lines, once (not the partial last one).
	if len(teed) != 1 || teed[0] != "s1|"+line+line {
		t.Fatalf("teed %q", teed)
	}
	// A symlink in place of the log is never followed.
	os.Remove(logf)
	os.WriteFile(filepath.Join(dir, "wp-config.php"), []byte("[29-Sep-2026 10:00:00 UTC] define('DB_PASSWORD', 'x');\n"), 0o640)
	os.Symlink("../wp-config.php", logf)
	h.svc.ingestPHPErrors(ctx, "s1")
	if errs, _ := h.svc.Store.PHPErrors(ctx, "s1", time.Time{}, 10); len(errs) != 1 {
		t.Fatalf("read through a symlink: %+v", errs)
	}
	ins, err := h.svc.Insights(ctx, "s1", time.Now().Add(-24*time.Hour*400))
	if err != nil || len(ins.Errors) != 1 || ins.Perf == nil {
		t.Fatalf("%+v %v", ins, err)
	}
}

// A site can put a named pipe where its error log goes: opening it for
// reading would wait for a writer forever, stopping every site's insights.
func TestPHPLogFIFODoesNotBlock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.ingestPHPErrors(ctx, "s1") // creates logs/
	if err := syscall.Mkfifo(filepath.Join(h.svc.Cfg.SiteDir("s1"), phpLogPath), 0o644); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	done := make(chan struct{})
	go func() { h.svc.ingestPHPErrors(ctx, "s1"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reading the error log blocked on a FIFO")
	}
}

func TestPHPLogTruncatedOnceRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.ingestPHPErrors(ctx, "s1")
	logf := filepath.Join(h.svc.Cfg.SiteDir("s1"), phpLogPath)
	line := "[29-Sep-2026 10:00:00 UTC] PHP Notice:  Loud plugin in /x/wp-content/plugins/loud/l.php on line 1\n"
	os.WriteFile(logf, bytes.Repeat([]byte(line), phpLogMax/len(line)+1), 0o644)
	for range 4 { // read in phpLogChunk pieces
		h.svc.ingestPHPErrors(ctx, "s1")
	}
	if fi, err := os.Stat(logf); err != nil || fi.Size() != 0 {
		t.Fatalf("log not truncated: %v %v", fi.Size(), err)
	}
	errs, _ := h.svc.Store.PHPErrors(ctx, "s1", time.Time{}, 10)
	if len(errs) != 1 || errs[0].Count != int64(phpLogMax/len(line)+1) {
		t.Fatalf("counted %+v", errs)
	}
}

// A CDN pass that listed the settings before they changed must not act on
// the old ones: no purge through a provider that was turned off.
func TestStaleCDNSettingsAreNotUsed(t *testing.T) {
	h, f := cdnHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken, EdgeHTML: true}); err != nil {
		t.Fatal(err)
	}
	stale, _ := h.svc.Store.GetCDN(ctx, "s1")
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{}); err != nil {
		t.Fatal(err)
	}
	purges := len(f.purges)
	st := mustSite(t, h.svc.Store, "s1")
	if err := h.svc.purgeCDN(ctx, st, stale); err != nil || len(f.purges) != purges {
		t.Fatalf("purged with stale settings: %v %v", err, f.purges)
	}
	h.svc.syncEdgeRules(ctx, st, stale)
	if len(f.rules) != 0 {
		t.Fatalf("edge rule brought back from stale settings: %v", f.rules)
	}
}

// Links rewritten in the database behind WordPress's back must not be
// hidden by options the object cache still holds (a fresh staging copy
// showing its live site's URLs).
func TestSearchReplaceFlushesObjectCache(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.searchReplace(context.Background(), "s1", "a.test", "staging.a.test"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*h.flushed, []string{"s1:"}) {
		t.Fatalf("flushed %v", *h.flushed)
	}
}
