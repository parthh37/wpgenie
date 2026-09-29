package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// LatencyBuckets are the upper bounds (ms) of the PHP response time
// histogram; a last, open bucket holds everything slower.
var LatencyBuckets = [...]int64{50, 100, 200, 300, 500, 750, 1000, 2000, 3000, 5000, 10000}

// SlowMS: PHP responses at least this slow are listed as slow requests.
const SlowMS = 1000

// maxSlowRequests bounds the slow URLs kept per site: anyone can make up
// URLs (a slow 404 each), and none of them may grow the panel's database.
const maxSlowRequests = 500

// LatencyBucket is the histogram bucket of a response time.
func LatencyBucket(ms int64) int {
	for i, b := range LatencyBuckets {
		if ms <= b {
			return i
		}
	}
	return len(LatencyBuckets)
}

// Histogram counts responses per LatencyBuckets bucket.
type Histogram [len(LatencyBuckets) + 1]int64

func (h *Histogram) Add(o Histogram) {
	for i := range h {
		h[i] += o[i]
	}
}

// Percentile estimates the p-th percentile (0 < p < 1) in ms, interpolating
// linearly inside the bucket it falls in; 0 without data. The open bucket
// reports its lower bound.
func (h *Histogram) Percentile(p float64) float64 {
	var n int64
	for _, c := range h {
		n += c
	}
	if n == 0 {
		return 0
	}
	rank := p * float64(n)
	var seen float64
	for i, c := range h {
		if c == 0 {
			continue
		}
		if seen+float64(c) >= rank {
			lo := 0.0
			if i > 0 {
				lo = float64(LatencyBuckets[i-1])
			}
			if i == len(LatencyBuckets) {
				return lo
			}
			return lo + (float64(LatencyBuckets[i])-lo)*(rank-seen)/float64(c)
		}
		seen += float64(c)
	}
	return float64(LatencyBuckets[len(LatencyBuckets)-1])
}

// PerfCounters are one hour of a site's PHP response times and page cache
// use (from the access log).
type PerfCounters struct {
	PHPRequests int64     `json:"php_requests"`
	PHPMS       int64     `json:"php_ms"`
	Slow        int64     `json:"slow"`
	CacheHits   int64     `json:"cache_hits"`
	CacheMisses int64     `json:"cache_misses"`
	Hist        Histogram `json:"-"`
}

func (c *PerfCounters) Add(o PerfCounters) {
	c.PHPRequests += o.PHPRequests
	c.PHPMS += o.PHPMS
	c.Slow += o.Slow
	c.CacheHits += o.CacheHits
	c.CacheMisses += o.CacheMisses
	c.Hist.Add(o.Hist)
}

// SlowKey is a slow URL: method and path (no query string).
type SlowKey struct {
	SiteID, Method, Path string
}

type SlowAgg struct {
	Count      int64
	TotalMS    int64
	MaxMS      int64
	LastStatus int
	LastSeen   int64 // unix seconds
}

func (a *SlowAgg) Add(ms int64, status int, at int64) {
	a.Count++
	a.TotalMS += ms
	a.MaxMS = max(a.MaxMS, ms)
	if at >= a.LastSeen {
		a.LastSeen, a.LastStatus = at, status
	}
}

func applyPerf(ctx context.Context, tx *sql.Tx, perf map[HourKey]*PerfCounters, slow map[SlowKey]*SlowAgg) error {
	for k, c := range perf {
		var old Histogram
		var blob string
		err := tx.QueryRowContext(ctx, `SELECT hist FROM perf_hourly WHERE site_id = ? AND hour = ?`, k.SiteID, k.Hour).Scan(&blob)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			_ = json.Unmarshal([]byte(blob), &old) // a damaged row restarts its histogram
		}
		old.Add(c.Hist)
		h, _ := json.Marshal(old)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO perf_hourly (site_id, hour, php_requests, php_ms, slow, cache_hits, cache_misses, hist)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT (site_id, hour) DO UPDATE SET
				php_requests = php_requests + excluded.php_requests,
				php_ms       = php_ms       + excluded.php_ms,
				slow         = slow         + excluded.slow,
				cache_hits   = cache_hits   + excluded.cache_hits,
				cache_misses = cache_misses + excluded.cache_misses,
				hist         = excluded.hist`,
			k.SiteID, k.Hour, c.PHPRequests, c.PHPMS, c.Slow, c.CacheHits, c.CacheMisses, string(h)); err != nil {
			return err
		}
	}
	for k, a := range slow {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO slow_requests (site_id, method, path, count, total_ms, max_ms, last_status, last_seen)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT (site_id, method, path) DO UPDATE SET
				count    = count + excluded.count,
				total_ms = total_ms + excluded.total_ms,
				max_ms   = MAX(max_ms, excluded.max_ms),
				last_status = CASE WHEN excluded.last_seen >= last_seen THEN excluded.last_status ELSE last_status END,
				last_seen   = MAX(last_seen, excluded.last_seen)`,
			k.SiteID, k.Method, k.Path, a.Count, a.TotalMS, a.MaxMS, a.LastStatus, a.LastSeen); err != nil {
			return err
		}
	}
	trimmed := map[string]bool{}
	for k := range slow {
		if trimmed[k.SiteID] {
			continue
		}
		trimmed[k.SiteID] = true
		if _, err := tx.ExecContext(ctx, `DELETE FROM slow_requests WHERE site_id = ? AND rowid NOT IN
			(SELECT rowid FROM slow_requests WHERE site_id = ? ORDER BY last_seen DESC, total_ms DESC LIMIT ?)`,
			k.SiteID, k.SiteID, maxSlowRequests); err != nil {
			return err
		}
	}
	return nil
}

// PerfPoint is one hour of a site's performance.
type PerfPoint struct {
	Hour time.Time `json:"hour"`
	PerfCounters
	P95MS float64 `json:"p95_ms"`
}

type PerfStats struct {
	Since  time.Time    `json:"since"`
	Totals PerfCounters `json:"totals"`
	// Response times of requests PHP answered (cache hits and static files
	// excluded), in ms.
	AvgMS  float64     `json:"avg_ms"`
	P50MS  float64     `json:"p50_ms"`
	P95MS  float64     `json:"p95_ms"`
	P99MS  float64     `json:"p99_ms"`
	Series []PerfPoint `json:"series"`
	// Hist is the response time histogram over the whole range, bucket i
	// counting responses up to Buckets[i] ms (the last one: slower).
	Hist    Histogram                  `json:"histogram"`
	Buckets [len(LatencyBuckets)]int64 `json:"buckets"`
}

// SitePerf returns a site's hourly performance since the given time.
func (s *Store) SitePerf(ctx context.Context, siteID string, since time.Time) (*PerfStats, error) {
	since = since.UTC().Truncate(time.Hour)
	out := &PerfStats{Since: since, Series: []PerfPoint{}, Buckets: LatencyBuckets}
	rows, err := s.db.QueryContext(ctx, `SELECT hour, php_requests, php_ms, slow, cache_hits, cache_misses, hist
		FROM perf_hourly WHERE site_id = ? AND hour >= ? ORDER BY hour`, siteID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PerfPoint
		var h int64
		var hist string
		if err := rows.Scan(&h, &p.PHPRequests, &p.PHPMS, &p.Slow, &p.CacheHits, &p.CacheMisses, &hist); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(hist), &p.Hist)
		p.Hour = time.Unix(h, 0).UTC()
		p.P95MS = p.Hist.Percentile(0.95)
		out.Totals.Add(p.PerfCounters)
		out.Series = append(out.Series, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Hist = out.Totals.Hist
	if out.Totals.PHPRequests > 0 {
		out.AvgMS = float64(out.Totals.PHPMS) / float64(out.Totals.PHPRequests)
	}
	out.P50MS, out.P95MS, out.P99MS = out.Hist.Percentile(0.5), out.Hist.Percentile(0.95), out.Hist.Percentile(0.99)
	return out, nil
}

type SlowRequest struct {
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Count      int64     `json:"count"`
	AvgMS      int64     `json:"avg_ms"`
	MaxMS      int64     `json:"max_ms"`
	LastStatus int       `json:"last_status"`
	LastSeen   time.Time `json:"last_seen"`
}

// SlowRequests lists a site's slowest URLs seen since the given time, the
// ones costing the most time in total first.
func (s *Store) SlowRequests(ctx context.Context, siteID string, since time.Time, limit int) ([]SlowRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT method, path, count, total_ms, max_ms, last_status, last_seen
		FROM slow_requests WHERE site_id = ? AND last_seen >= ? ORDER BY total_ms DESC LIMIT ?`,
		siteID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SlowRequest{}
	for rows.Next() {
		var r SlowRequest
		var total, seen int64
		if err := rows.Scan(&r.Method, &r.Path, &r.Count, &total, &r.MaxMS, &r.LastStatus, &seen); err != nil {
			return nil, err
		}
		r.AvgMS = total / max(r.Count, 1)
		r.LastSeen = time.Unix(seen, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// PHPError is one kind of PHP error on a site: same level, message and
// place. Source says whose code it is: "plugin:<slug>", "theme:<slug>",
// "mu-plugin", "core" or "other".
type PHPError struct {
	Fingerprint string    `json:"fingerprint"`
	Level       string    `json:"level"`
	Message     string    `json:"message"`
	File        string    `json:"file"`
	Line        int       `json:"line"`
	Source      string    `json:"source"`
	Count       int64     `json:"count"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
}

// maxPHPErrors bounds the kinds of errors kept per site: a plugin logging
// a distinct message per request must not grow the panel's database.
const maxPHPErrors = 300

// RecordPHPErrors adds occurrences (Count, FirstSeen, LastSeen per
// fingerprint), keeps the maxPHPErrors most recent kinds and saves how far
// the log was read, in one transaction: a crash neither loses nor
// double-counts errors.
func (s *Store) RecordPHPErrors(ctx context.Context, siteID string, errs []PHPError, st IngestState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range errs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO php_errors (site_id, fingerprint, level, message, file, line, source, count, first_seen, last_seen)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (site_id, fingerprint) DO UPDATE SET
				count      = count + excluded.count,
				first_seen = MIN(first_seen, excluded.first_seen),
				last_seen  = MAX(last_seen, excluded.last_seen)`,
			siteID, e.Fingerprint, e.Level, e.Message, e.File, e.Line, e.Source, e.Count,
			e.FirstSeen.Unix(), e.LastSeen.Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM php_errors WHERE site_id = ? AND fingerprint NOT IN
		(SELECT fingerprint FROM php_errors WHERE site_id = ? ORDER BY last_seen DESC LIMIT ?)`,
		siteID, siteID, maxPHPErrors); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ingest_state (name, inode, offset) VALUES (?,?,?)
		ON CONFLICT (name) DO UPDATE SET inode = excluded.inode, offset = excluded.offset`,
		st.Name, int64(st.Inode), st.Offset); err != nil {
		return err
	}
	return tx.Commit()
}

// PHPErrors lists the kinds of PHP errors seen since the given time, the
// most frequent first.
func (s *Store) PHPErrors(ctx context.Context, siteID string, since time.Time, limit int) ([]PHPError, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT fingerprint, level, message, file, line, source, count, first_seen, last_seen
		FROM php_errors WHERE site_id = ? AND last_seen >= ? ORDER BY count DESC, last_seen DESC LIMIT ?`,
		siteID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PHPError{}
	for rows.Next() {
		var e PHPError
		var first, last int64
		if err := rows.Scan(&e.Fingerprint, &e.Level, &e.Message, &e.File, &e.Line, &e.Source, &e.Count, &first, &last); err != nil {
			return nil, err
		}
		e.FirstSeen, e.LastSeen = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearPHPErrors forgets a site's PHP errors (after fixing them).
func (s *Store) ClearPHPErrors(ctx context.Context, siteID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM php_errors WHERE site_id = ?`, siteID)
	return err
}

// PruneInsights drops performance data older than what the panel shows:
// hourly response times after 90 days, slow URLs after 7 and PHP errors
// after 30 days without a new occurrence. Rows of deleted sites go too.
func (s *Store) PruneInsights(ctx context.Context, now time.Time) error {
	for _, q := range []struct {
		sql    string
		before time.Duration
	}{
		{`DELETE FROM perf_hourly WHERE hour < ? OR site_id NOT IN (SELECT id FROM sites)`, 90 * 24 * time.Hour},
		{`DELETE FROM slow_requests WHERE last_seen < ? OR site_id NOT IN (SELECT id FROM sites)`, 7 * 24 * time.Hour},
		{`DELETE FROM php_errors WHERE last_seen < ?`, 30 * 24 * time.Hour},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, now.Add(-q.before).Unix()); err != nil {
			return err
		}
	}
	return nil
}
