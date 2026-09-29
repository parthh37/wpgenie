package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

func containsLine(body, line string) bool {
	for _, l := range strings.Split(body, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

type fakeShield struct{}

func (fakeShield) Bans() []shield.Ban { return []shield.Ban{{Addr: "192.0.2.1"}, {Addr: "192.0.2.2"}} }
func (fakeShield) Decisions() map[string]uint64 {
	return map[string]uint64{"allow": 10, "block": 2, "challenge": 1, "throttle": 0}
}

var (
	sampleRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\.)*",?)*\})? (-?[0-9.e+-]+|\+Inf|-Inf|NaN)$`)
	typeRe   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (counter|gauge|histogram|summary|untyped)$`)
)

// checkExposition validates the text format: every sample belongs to the
// family declared just before it, and no family is declared twice.
func checkExposition(t *testing.T, body string) {
	t.Helper()
	declared := map[string]bool{}
	current, typ := "", ""
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "# HELP "):
		case strings.HasPrefix(l, "# TYPE "):
			m := typeRe.FindStringSubmatch(l)
			if m == nil || declared[m[1]] {
				t.Errorf("bad or repeated TYPE: %q", l)
				continue
			}
			declared[m[1]], current, typ = true, m[1], m[2]
		default:
			m := sampleRe.FindStringSubmatch(l)
			if m == nil {
				t.Errorf("bad sample line: %q", l)
				continue
			}
			name := m[1]
			if typ == "histogram" {
				name = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
			}
			if name != current {
				t.Errorf("sample %q outside its family (in %s)", l, current)
			}
		}
	}
}

func TestMetrics(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	st.CreateSite(ctx, &store.Site{ID: "s1", Name: "a", PrimaryDomain: "a.test", PHPVersion: "8.3", FPMPort: 19001,
		DBName: "wp_s1", Status: store.StatusActive, MemoryMB: 512, CPUs: 1, Replicas: 2})
	st.CreateSite(ctx, &store.Site{ID: "s2", Name: "b \"quoted\"", PrimaryDomain: "b.test", PHPVersion: "8.3", FPMPort: 19002,
		DBName: "wp_s2", Status: store.StatusActive, MemoryMB: 512, CPUs: 1, Replicas: 1})
	st.SaveScan(ctx, "s1", time.Now(), []byte(`{"vulnerable":3,"inventory":{}}`))
	id, _ := st.CreateJob(ctx, "s1", "backup", "t")
	st.FinishJob(ctx, id, store.JobFailed, "x", "")

	workers := 45.0
	s := &Service{Store: st, Log: quietLog(), Version: "v1.2.3", Shield: fakeShield{},
		Load: func(id string) (site.CPUReading, bool) {
			if id != "s1" {
				return site.CPUReading{}, false
			}
			return site.CPUReading{Percent: 37, Replicas: 2, Workers: &workers, Queued: 1}, true
		},
		Hosts: LocalHost(t.TempDir())}
	s.init()
	var h store.Histogram
	h[store.LatencyBucket(40)] = 3
	h[store.LatencyBucket(700)] = 1
	h[len(h)-1] = 1
	b := &store.TrafficBatch{
		Hourly: map[store.HourKey]*store.Counters{{SiteID: "s1", Hour: 0}: {Requests: 10, PageViews: 4, BytesOut: 1000, BotHits: 2, Blocked: 1, Errors5xx: 1},
			{SiteID: "gone", Hour: 0}: {Requests: 5}},
		Perf: map[store.HourKey]*store.PerfCounters{{SiteID: "s1", Hour: 0}: {PHPRequests: 5, PHPMS: 12000, CacheHits: 7, CacheMisses: 2, Hist: h}},
	}
	s.ObserveTraffic(b)
	s.ObserveTraffic(&store.TrafficBatch{Hourly: map[store.HourKey]*store.Counters{{SiteID: "s1", Hour: 3600}: {Requests: 5}}})
	s.refreshVulns(ctx, time.Now(), []*store.Site{{ID: "s1"}, {ID: "s2"}})

	out, err := s.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	checkExposition(t, body)
	for _, want := range []string{
		`wpgenie_site_requests_total{site="s1"} 15`,
		`wpgenie_site_bytes_out_total{site="s1"} 1000`,
		`wpgenie_site_http_5xx_total{site="s1"} 1`,
		`wpgenie_site_page_cache_hits_total{site="s1"} 7`,
		`wpgenie_site_php_response_seconds_bucket{site="s1",le="0.05"} 3`,
		`wpgenie_site_php_response_seconds_bucket{site="s1",le="0.5"} 3`,
		`wpgenie_site_php_response_seconds_bucket{site="s1",le="0.75"} 4`,
		`wpgenie_site_php_response_seconds_bucket{site="s1",le="10"} 4`,
		`wpgenie_site_php_response_seconds_bucket{site="s1",le="+Inf"} 5`,
		`wpgenie_site_php_response_seconds_count{site="s1"} 5`,
		`wpgenie_site_php_response_seconds_sum{site="s1"} 12`,
		`wpgenie_site_replicas_desired{site="s1"} 2`,
		`wpgenie_site_replicas_running{site="s1"} 2`,
		`wpgenie_site_cpu_percent{site="s1"} 37`,
		`wpgenie_site_php_workers_busy_percent{site="s1"} 45`,
		`wpgenie_site_php_queued_requests{site="s1"} 1`,
		`wpgenie_site_vulnerable_components{site="s1"} 3`,
		`wpgenie_shield_decisions_total{verdict="block"} 2`,
		`wpgenie_shield_active_bans 2`,
		`wpgenie_jobs{status="failed"} 1`,
		`wpgenie_site_info{site="s2",domain="b.test",status="active",staging="false"} 1`,
		`wpgenie_build_info{version="v1.2.3",goversion="` + runtime.Version() + `"} 1`,
	} {
		if !containsLine(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Deleted sites' series end; no site without traffic gets counters.
	if strings.Contains(body, `site="gone"`) || strings.Contains(body, `wpgenie_site_requests_total{site="s2"}`) {
		t.Error("unexpected series")
	}
	if !strings.Contains(body, "wpgenie_filesystem_avail_bytes{path=") {
		t.Error("no filesystem metrics")
	}
}

func TestMetricsToken(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	s := &Service{Store: st, Log: quietLog()}
	h := s.MetricsHandler()
	get := func(auth string) int {
		req := httptest.NewRequest("GET", "/metrics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == 200 && !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
			t.Errorf("content type %q", rec.Header().Get("Content-Type"))
		}
		return rec.Code
	}
	// No token yet: metrics are off, whatever is sent.
	if c := get("Bearer "); c != 401 {
		t.Errorf("no token: %d", c)
	}
	info, _ := s.MetricsTokenInfo(ctx)
	if info.Set {
		t.Error("token set")
	}
	tok, err := s.RotateMetricsToken(ctx)
	if err != nil || !strings.HasPrefix(tok, "wpgm_") {
		t.Fatal(tok, err)
	}
	// Only the hash is stored.
	if v, _ := st.Setting(ctx, tokenKey); strings.Contains(v, tok) {
		t.Error("token stored in clear")
	}
	for _, c := range []struct {
		auth string
		want int
	}{{"", 401}, {"Bearer wrong", 401}, {tok, 401}, {"Basic " + tok, 401}, {"Bearer " + tok, 200}} {
		if got := get(c.auth); got != c.want {
			t.Errorf("%q: %d, want %d", c.auth, got, c.want)
		}
	}
	// Rotation retires the old token; a fresh Service (restart) reads the new one.
	tok2, _ := s.RotateMetricsToken(ctx)
	if get("Bearer "+tok) != 401 || get("Bearer "+tok2) != 200 {
		t.Error("rotation")
	}
	s2 := &Service{Store: st, Log: quietLog()}
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+tok2)
	rec := httptest.NewRecorder()
	s2.MetricsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("after restart: %d", rec.Code)
	}
}
