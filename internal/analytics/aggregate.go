// Package analytics turns Caddy's JSON access log into per-site traffic
// rollups: requests, page views, bandwidth, bot hits, shield blocks and
// privacy-preserving unique visitor estimates.
package analytics

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/textproto"
	"path"
	"strings"
	"time"

	"github.com/axiomhq/hyperloglog"

	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/store"
)

// entry is the subset of a Caddy access-log line we care about.
type entry struct {
	TS      float64 `json:"ts"`
	Request struct {
		ClientIP string              `json:"client_ip"`
		RemoteIP string              `json:"remote_ip"`
		Host     string              `json:"host"`
		Method   string              `json:"method"`
		URI      string              `json:"uri"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
	Duration    float64             `json:"duration"` // seconds
	Size        int64               `json:"size"`
	Status      int                 `json:"status"`
	RespHeaders map[string][]string `json:"resp_headers"`
}

func parseLine(line []byte) (*entry, bool) {
	var e entry
	if json.Unmarshal(line, &e) != nil || e.Request.Host == "" || e.Status == 0 {
		return nil, false
	}
	return &e, true
}

// first returns a header value from a logged header map. Caddy logs Go's
// canonical header keys ("X-Wpgenie-Shield"), so the lookup key must be
// canonicalised the same way.
func first(h map[string][]string, k string) string {
	if v := h[textproto.CanonicalMIMEHeaderKey(k)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func normalizeHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

type aggregator struct {
	secret   []byte
	hourly   map[store.HourKey]*store.Counters
	visitors map[store.DayKey]*hyperloglog.Sketch
	perf     map[store.HourKey]*store.PerfCounters
	slow     map[store.SlowKey]*store.SlowAgg
	// recent are this batch's PHP response times, handed to Recent once
	// the batch is committed (a failed batch is read again).
	recent []observation
}

type observation struct {
	site string
	at   time.Time
	ms   float64
}

func newAggregator(secret []byte) *aggregator {
	a := &aggregator{secret: secret}
	a.reset()
	return a
}

func (a *aggregator) reset() {
	a.hourly = map[store.HourKey]*store.Counters{}
	a.visitors = map[store.DayKey]*hyperloglog.Sketch{}
	a.perf = map[store.HourKey]*store.PerfCounters{}
	a.slow = map[store.SlowKey]*store.SlowAgg{}
	a.recent = nil
}

func (a *aggregator) add(siteID string, e *entry) {
	ts := time.Unix(0, int64(e.TS*float64(time.Second))).UTC()
	hk := store.HourKey{SiteID: siteID, Hour: ts.Truncate(time.Hour).Unix()}
	c := a.hourly[hk]
	if c == nil {
		c = &store.Counters{}
		a.hourly[hk] = c
	}

	ua := first(e.Request.Headers, "User-Agent")
	class := shield.ClassifyUA(ua)
	c.Requests++
	c.BytesOut += e.Size
	if e.Status >= 500 {
		c.Errors5xx++
	}
	if first(e.RespHeaders, shield.VerdictHeader) != "" {
		c.Blocked++
		return // challenged/blocked requests are neither page views nor visitors
	}
	a.addPerf(siteID, hk, ts, e)
	if class.IsBot() {
		c.BotHits++
		return
	}
	if e.Request.Method != "GET" || e.Status != 200 ||
		!strings.HasPrefix(first(e.RespHeaders, "Content-Type"), "text/html") {
		return
	}
	c.PageViews++

	ip := e.Request.ClientIP
	if ip == "" {
		ip = e.Request.RemoteIP
	}
	dk := store.DayKey{SiteID: siteID, Day: ts.Truncate(24 * time.Hour).Unix()}
	sk := a.visitors[dk]
	if sk == nil {
		sk = hyperloglog.New16()
		a.visitors[dk] = sk
	}
	sk.Insert(a.visitorID(dk.Day, ip, ua))
}

// visitorID is a keyed hash of (day, IP, UA). It is stable within a day so
// repeat page views count once, changes daily so visitors can't be tracked
// across days, and is irreversible without the server secret. Raw IPs are
// never stored.
func (a *aggregator) visitorID(day int64, ip, ua string) []byte {
	m := hmac.New(sha256.New, a.secret)
	var d [8]byte
	binary.BigEndian.PutUint64(d[:], uint64(day))
	m.Write(d[:])
	m.Write([]byte(ip))
	m.Write([]byte{0})
	m.Write([]byte(ua))
	return m.Sum(nil)[:16]
}

func (a *aggregator) empty() bool { return len(a.hourly) == 0 && len(a.visitors) == 0 }

// batch hands over the counters (and the response times for Recent).
func (a *aggregator) batch(st store.IngestState) (*store.TrafficBatch, []observation) {
	b := &store.TrafficBatch{Hourly: a.hourly, Visitors: a.visitors, Perf: a.perf, Slow: a.slow, State: st}
	obs := a.recent
	a.reset()
	return b, obs
}

// staticExt are the files Caddy serves from disk without the shield or PHP
// (the Caddyfile's @wpg_dynamic exceptions).
var staticExt = map[string]bool{
	".css": true, ".js": true, ".mjs": true, ".map": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".avif": true, ".svg": true, ".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".eot": true, ".mp4": true, ".webm": true, ".mp3": true, ".pdf": true,
}

// requestKind tells page cache hits and PHP responses apart from everything
// else Caddy answers itself (static files, its own 404s and redirects,
// WPGenie's endpoints). PHP always sends a Content-Type; a missing static
// file falls through to WordPress, whose 404 page is HTML.
func requestKind(e *entry) (hit, php bool) {
	switch first(e.RespHeaders, "X-WPGenie-Cache") {
	case "HIT":
		return true, false
	case "MISS", "BYPASS":
		return false, true
	}
	p, _, _ := strings.Cut(e.Request.URI, "?")
	ct := first(e.RespHeaders, "Content-Type")
	if ct == "" || strings.HasPrefix(p, "/_shield/") || strings.HasPrefix(p, "/_wpgenie/") {
		return false, false
	}
	if staticExt[strings.ToLower(path.Ext(p))] && !strings.Contains(p, ".php") && !strings.HasPrefix(ct, "text/html") {
		return false, false
	}
	return false, true
}

func (a *aggregator) addPerf(siteID string, hk store.HourKey, ts time.Time, e *entry) {
	hit, php := requestKind(e)
	if !hit && !php {
		return
	}
	p := a.perf[hk]
	if p == nil {
		p = &store.PerfCounters{}
		a.perf[hk] = p
	}
	if hit {
		p.CacheHits++
		return
	}
	if first(e.RespHeaders, "X-WPGenie-Cache") == "MISS" {
		p.CacheMisses++
	}
	ms := int64(e.Duration*1000 + 0.5)
	p.PHPRequests++
	p.PHPMS += ms
	p.Hist[store.LatencyBucket(ms)]++
	a.recent = append(a.recent, observation{siteID, ts, float64(ms)})
	if ms < store.SlowMS {
		return
	}
	p.Slow++
	u, _, _ := strings.Cut(e.Request.URI, "?")
	if len(u) > 300 {
		u = u[:300]
	}
	k := store.SlowKey{SiteID: siteID, Method: e.Request.Method, Path: strings.ToValidUTF8(u, "?")}
	sa := a.slow[k]
	if sa == nil {
		sa = &store.SlowAgg{}
		a.slow[k] = sa
	}
	sa.Add(ms, e.Status, ts.Unix())
}
