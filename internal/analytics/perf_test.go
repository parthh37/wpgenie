package analytics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
)

func perfLine(ts time.Time, method, uri string, status int, seconds float64, headers map[string]string) string {
	var h []string
	for k, v := range headers {
		h = append(h, fmt.Sprintf("%q:[%q]", k, v))
	}
	return fmt.Sprintf(`{"ts":%f,"request":{"remote_ip":"203.0.113.1","client_ip":"203.0.113.1","method":%q,"host":"example.com","uri":%q,"headers":{"User-Agent":["Mozilla/5.0 Firefox/130.0"]}},"duration":%f,"size":100,"status":%d,"resp_headers":{%s}}`,
		float64(ts.UnixNano())/1e9, method, uri, seconds, status, strings.Join(h, ","))
}

// Only what PHP answered counts towards response times: cache hits,
// static files, Caddy's own answers and shield blocks are other costs.
func TestAggregatorPerf(t *testing.T) {
	ts := time.Now().UTC()
	a := newAggregator([]byte("secret"))
	html := map[string]string{"Content-Type": "text/html; charset=UTF-8"}
	for _, l := range []string{
		perfLine(ts, "GET", "/", 200, 0.004, map[string]string{"Content-Type": "text/html", "X-Wpgenie-Cache": "HIT"}),
		perfLine(ts, "GET", "/about/", 200, 0.180, map[string]string{"Content-Type": "text/html", "X-Wpgenie-Cache": "MISS"}),
		perfLine(ts, "POST", "/wp-admin/admin-ajax.php?action=x", 200, 2.5, map[string]string{"Content-Type": "application/json"}),
		perfLine(ts, "POST", "/wp-admin/admin-ajax.php?action=y", 200, 1.5, map[string]string{"Content-Type": "application/json"}),
		perfLine(ts, "GET", "/wp-content/themes/t/style.css", 200, 0.001, map[string]string{"Content-Type": "text/css"}),
		perfLine(ts, "GET", "/wp-content/themes/t/missing.css", 404, 0.090, html), // WordPress's 404 page
		perfLine(ts, "GET", "/wp-config.php", 404, 0.0001, nil),                   // Caddy's own 404
		perfLine(ts, "GET", "/_shield/challenge.js", 200, 0.0001, map[string]string{"Content-Type": "text/javascript"}),
		perfLine(ts, "GET", "/x/", 403, 0.001, map[string]string{"Content-Type": "text/html", "X-Wpgenie-Shield": "block"}),
	} {
		e, ok := parseLine([]byte(l))
		if !ok {
			t.Fatalf("parse failed: %s", l)
		}
		a.add("s1", e)
	}
	p := a.perf[store.HourKey{SiteID: "s1", Hour: ts.Truncate(time.Hour).Unix()}]
	if p == nil {
		t.Fatal("no perf counters")
	}
	want := store.PerfCounters{PHPRequests: 4, PHPMS: 180 + 2500 + 1500 + 90, Slow: 2, CacheHits: 1, CacheMisses: 1}
	want.Hist[store.LatencyBucket(180)]++
	want.Hist[store.LatencyBucket(2500)]++
	want.Hist[store.LatencyBucket(1500)]++
	want.Hist[store.LatencyBucket(90)]++
	if *p != want {
		t.Errorf("perf = %+v\nwant   %+v", *p, want)
	}
	// Slow URLs are grouped without their query strings.
	slow := a.slow[store.SlowKey{SiteID: "s1", Method: "POST", Path: "/wp-admin/admin-ajax.php"}]
	if slow == nil || slow.Count != 2 || slow.MaxMS != 2500 || slow.TotalMS != 4000 {
		t.Errorf("slow = %+v", slow)
	}
	if len(a.recent) != 4 {
		t.Errorf("%d recent observations, want 4", len(a.recent))
	}
}

func TestIngestPerfRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	if err := st.CreateSite(ctx, &store.Site{ID: "s1", Name: "x", PrimaryDomain: "example.com", PHPVersion: "8.3",
		FPMPort: 19000, DBName: "wp_s1", Status: store.StatusActive, PHP: store.PHPSettings{}}); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "access.log")
	now := time.Now().UTC()
	var lines string
	for i := range 100 {
		lines += perfLine(now, "GET", fmt.Sprintf("/p%d/", i%3), 200, float64(i+1)/1000*10, // 10 ms … 1 s
			map[string]string{"Content-Type": "text/html", "X-Wpgenie-Cache": "MISS"}) + "\n"
	}
	os.WriteFile(log, []byte(lines), 0o644)
	recent := &Recent{}
	var observed int64
	in := &Ingester{Path: log, Store: st, Secret: []byte("k"), Recent: recent,
		Observer: func(b *store.TrafficBatch) {
			observed += b.Perf[store.HourKey{SiteID: "s1", Hour: now.Truncate(time.Hour).Unix()}].PHPRequests
		}}
	if _, err := in.tick(ctx, store.IngestState{Name: stateName}, newAggregator(in.Secret)); err != nil {
		t.Fatal(err)
	}
	if observed != 100 {
		t.Errorf("observer saw %d PHP requests", observed)
	}
	perf, err := st.SitePerf(ctx, "s1", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if perf.Totals.PHPRequests != 100 || perf.Totals.CacheMisses != 100 || perf.Totals.Slow != 1 {
		t.Fatalf("totals %+v", perf.Totals)
	}
	// Uniform 10…1000 ms: the median is near 500 ms, p95 near 950 ms. The
	// histogram only knows buckets, so allow for their width.
	if perf.P50MS < 400 || perf.P50MS > 600 || perf.P95MS < 750 || perf.P95MS > 1000 {
		t.Errorf("p50 %.0f p95 %.0f", perf.P50MS, perf.P95MS)
	}
	if p95, n := recent.Percentile("s1", now.Add(-time.Minute), 0.95); n != 100 || p95 != 950 {
		t.Errorf("recent p95 %.0f over %d", p95, n)
	}
	slow, err := st.SlowRequests(ctx, "s1", now.Add(-time.Hour), 10)
	if err != nil || len(slow) != 1 || slow[0].MaxMS != 1000 {
		t.Errorf("slow %+v %v", slow, err)
	}
}
