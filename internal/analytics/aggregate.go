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
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
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
}

func newAggregator(secret []byte) *aggregator {
	a := &aggregator{secret: secret}
	a.reset()
	return a
}

func (a *aggregator) reset() {
	a.hourly = map[store.HourKey]*store.Counters{}
	a.visitors = map[store.DayKey]*hyperloglog.Sketch{}
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

func (a *aggregator) batch(st store.IngestState) *store.TrafficBatch {
	b := &store.TrafficBatch{Hourly: a.hourly, Visitors: a.visitors, State: st}
	a.reset()
	return b
}
