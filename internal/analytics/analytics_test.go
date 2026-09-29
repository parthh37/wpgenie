package analytics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
)

func logLine(ts time.Time, host, ip, ua, method string, status int, size int64, ctype, shieldHdr string) string {
	resp := fmt.Sprintf(`{"Content-Type":[%q]}`, ctype)
	if shieldHdr != "" {
		resp = fmt.Sprintf(`{"Content-Type":[%q],"X-Wpgenie-Shield":[%q]}`, ctype, shieldHdr)
	}
	return fmt.Sprintf(`{"level":"info","ts":%f,"logger":"http.log.access.log0","msg":"handled request","request":{"remote_ip":%q,"client_ip":%q,"proto":"HTTP/2.0","method":%q,"host":%q,"uri":"/","headers":{"User-Agent":[%q]}},"bytes_read":0,"duration":0.01,"size":%d,"status":%d,"resp_headers":%s}`+"\n",
		float64(ts.UnixNano())/1e9, ip, ip, method, host, ua, size, status, resp)
}

func TestAggregator(t *testing.T) {
	ts := time.Date(2026, 9, 29, 10, 30, 0, 0, time.UTC)
	browser := "Mozilla/5.0 (X11; Linux) Firefox/130.0"
	a := newAggregator([]byte("secret"))
	add := func(line string) {
		e, ok := parseLine([]byte(line))
		if !ok {
			t.Fatalf("parse failed: %s", line)
		}
		a.add("s1", e)
	}
	add(logLine(ts, "example.com", "203.0.113.1", browser, "GET", 200, 1000, "text/html; charset=UTF-8", ""))
	add(logLine(ts, "example.com", "203.0.113.1", browser, "GET", 200, 1000, "text/html; charset=UTF-8", "")) // same visitor
	add(logLine(ts, "example.com", "203.0.113.2", browser, "GET", 200, 500, "text/html", ""))
	add(logLine(ts, "example.com", "203.0.113.2", browser, "GET", 200, 20000, "image/webp", ""))       // asset
	add(logLine(ts, "example.com", "203.0.113.3", "GPTBot/1.2", "GET", 200, 800, "text/html", ""))     // bot
	add(logLine(ts, "example.com", "203.0.113.4", browser, "GET", 403, 300, "text/html", "challenge")) // shield
	add(logLine(ts, "example.com", "203.0.113.5", browser, "GET", 502, 100, "text/html", ""))

	c := a.hourly[store.HourKey{SiteID: "s1", Hour: ts.Truncate(time.Hour).Unix()}]
	want := store.Counters{Requests: 7, PageViews: 3, BytesOut: 23700, BotHits: 1, Blocked: 1, Errors5xx: 1}
	if *c != want {
		t.Errorf("counters = %+v\nwant       %+v", *c, want)
	}
	sk := a.visitors[store.DayKey{SiteID: "s1", Day: ts.Truncate(24 * time.Hour).Unix()}]
	if n := sk.Estimate(); n != 2 {
		t.Errorf("unique visitors = %d, want 2", n)
	}
}

func TestReadNewHandlesPartialLinesAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	var got []string
	collect := func(b []byte) { got = append(got, string(b)) }

	os.WriteFile(path, []byte("a\nb\npart"), 0o644)
	st, err := readNew(path, store.IngestState{}, 1<<20, collect)
	if err != nil || len(got) != 2 || st.Offset != 4 {
		t.Fatalf("first read: err=%v got=%q offset=%d", err, got, st.Offset)
	}

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("ial\n")
	f.Close()
	got = nil
	st, _ = readNew(path, st, 1<<20, collect)
	if len(got) != 1 || got[0] != "partial\n" {
		t.Fatalf("partial line not completed: %q", got)
	}

	// Rotation: Caddy renames the file and starts a new one.
	os.Rename(path, path+".1")
	os.WriteFile(path, []byte("new\n"), 0o644)
	got = nil
	st, _ = readNew(path, st, 1<<20, collect)
	if len(got) != 1 || got[0] != "new\n" || st.Offset != 4 {
		t.Fatalf("after rotation: got=%q offset=%d", got, st.Offset)
	}
}

func TestIngestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st := storetest.Open(t)
	ctx := context.Background()
	if err := st.CreateSite(ctx, &store.Site{ID: "s1", Name: "x", PrimaryDomain: "example.com",
		PHPVersion: "8.3", FPMPort: 19000, DBName: "wp_s1", Status: store.StatusActive, ShieldMode: "standard"}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "access.log")
	now := time.Now().UTC()
	line := logLine(now, "Example.com:443", "203.0.113.9", "Mozilla/5.0 Firefox", "GET", 200, 4096, "text/html", "")
	os.WriteFile(logPath, []byte(line+line+logLine(now, "unknown.test", "1.1.1.1", "x", "GET", 200, 1, "text/html", "")), 0o644)

	in := &Ingester{Path: logPath, Store: st, Secret: []byte("s")}
	state, _ := st.IngestState(ctx, stateName)
	if _, err := in.tick(ctx, state, newAggregator(in.Secret)); err != nil {
		t.Fatal(err)
	}
	stats, err := st.SiteStats(ctx, "s1", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.Requests != 2 || stats.Totals.BytesOut != 8192 || stats.UniqueVisitors != 1 {
		t.Errorf("stats = %+v visitors=%d", stats.Totals, stats.UniqueVisitors)
	}

	// A second tick with no new data must not double count.
	state, _ = st.IngestState(ctx, stateName)
	in.tick(ctx, state, newAggregator(in.Secret))
	stats, _ = st.SiteStats(ctx, "s1", now.Add(-time.Hour))
	if stats.Totals.Requests != 2 {
		t.Errorf("double counted: %d", stats.Totals.Requests)
	}
}

// The daemon's own probes carry the health token and aren't traffic; the
// same User-Agent without the token (or with a wrong one) is. Caddy logs
// the token's hash, never the token.
func TestHealthProbesAreNotTraffic(t *testing.T) {
	ts := time.Date(2026, 9, 29, 10, 30, 0, 0, time.UTC)
	a := newAggregator([]byte("secret"))
	a.health = []byte(HealthTokenHash("tok123"))
	probe := func(token string) string {
		return strings.Replace(logLine(ts, "example.com", "127.0.0.1", "WPGenie-Health/1.0", "GET", 200, 500, "text/html", ""),
			`"headers":{`, `"headers":{"X-Wpgenie-Health":[`+strconv.Quote(token)+`],`, 1)
	}
	for _, line := range []string{probe(HealthTokenHash("tok123")), probe(HealthTokenHash("tok123")), probe(HealthTokenHash("wrong"))} {
		e, ok := parseLine([]byte(line))
		if !ok {
			t.Fatalf("parse failed: %s", line)
		}
		a.add("s1", e)
	}
	if c := a.hourly[store.HourKey{SiteID: "s1", Hour: ts.Truncate(time.Hour).Unix()}]; c == nil || c.Requests != 1 || c.BotHits != 1 {
		t.Errorf("counters = %+v, want only the request without the token", c)
	}
}
