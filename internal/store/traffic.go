package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/axiomhq/hyperloglog"
)

type HourKey struct {
	SiteID string
	Hour   int64 // unix seconds, truncated to the hour
}

type DayKey struct {
	SiteID string
	Day    int64 // unix seconds, truncated to the UTC day
}

type Counters struct {
	Requests  int64 `json:"requests"`
	PageViews int64 `json:"page_views"`
	BytesOut  int64 `json:"bytes_out"`
	BotHits   int64 `json:"bot_hits"`
	Blocked   int64 `json:"blocked"`
	Errors5xx int64 `json:"errors_5xx"`
}

func (c *Counters) Add(o Counters) {
	c.Requests += o.Requests
	c.PageViews += o.PageViews
	c.BytesOut += o.BytesOut
	c.BotHits += o.BotHits
	c.Blocked += o.Blocked
	c.Errors5xx += o.Errors5xx
}

type IngestState struct {
	Name   string
	Inode  uint64
	Offset int64
}

// TrafficBatch is one flush from the analytics aggregator. It is applied in a
// single transaction together with the log offset, so a crash can neither
// lose nor double-count a batch.
type TrafficBatch struct {
	Hourly   map[HourKey]*Counters
	Visitors map[DayKey]*hyperloglog.Sketch
	// Perf and Slow are response times and slow URLs (see insights.go).
	Perf  map[HourKey]*PerfCounters
	Slow  map[SlowKey]*SlowAgg
	State IngestState
}

// ApplyTraffic applies a batch. Rows are written in key order, so two
// batches touching the same rows at once (nodes shipping rollups to one
// PostgreSQL store) take their row locks in the same order and can't
// deadlock; a transaction that loses a conflict anyway is retried.
func (s *Store) ApplyTraffic(ctx context.Context, b *TrafficBatch) error {
	return s.db.inTx(ctx, func(tx *Tx) error { return applyTraffic(ctx, tx, b) })
}

func applyTraffic(ctx context.Context, tx *Tx, b *TrafficBatch) error {
	for _, k := range sortedHourKeys(b.Hourly) {
		c := b.Hourly[k]
		_, err := tx.ExecContext(ctx, `
			INSERT INTO traffic_hourly (site_id, hour, requests, page_views, bytes_out, bot_hits, blocked, errors_5xx)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT (site_id, hour) DO UPDATE SET
				requests   = requests   + excluded.requests,
				page_views = page_views + excluded.page_views,
				bytes_out  = bytes_out  + excluded.bytes_out,
				bot_hits   = bot_hits   + excluded.bot_hits,
				blocked    = blocked    + excluded.blocked,
				errors_5xx = errors_5xx + excluded.errors_5xx`,
			k.SiteID, k.Hour, c.Requests, c.PageViews, c.BytesOut, c.BotHits, c.Blocked, c.Errors5xx)
		if err != nil {
			return err
		}
	}

	days := slices.SortedFunc(maps.Keys(b.Visitors), func(x, y DayKey) int {
		return cmp.Or(strings.Compare(x.SiteID, y.SiteID), cmp.Compare(x.Day, y.Day))
	})
	for _, k := range days {
		if err := mergeVisitors(ctx, tx, k, b.Visitors[k]); err != nil {
			return err
		}
	}

	if err := applyPerf(ctx, tx, b.Perf, b.Slow); err != nil {
		return err
	}

	if b.State.Name != "" {
		// "offset" is quoted: PostgreSQL reserves the word.
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingest_state (name, inode, "offset") VALUES (?,?,?)
			ON CONFLICT (name) DO UPDATE SET inode = excluded.inode, "offset" = excluded."offset"`,
			b.State.Name, int64(b.State.Inode), b.State.Offset); err != nil {
			return err
		}
	}
	return nil
}

// mergeVisitors merges a day's sketch into the stored one. The stored
// sketch is read locked (FOR UPDATE), so a concurrent batch can't merge
// into the same old value and overwrite this one's visitors; when there is
// none yet, the insert doesn't overwrite one a concurrent batch has just
// added (DO NOTHING) and the merge starts over from that one.
func mergeVisitors(ctx context.Context, tx *Tx, k DayKey, sk *hyperloglog.Sketch) error {
	for {
		var blob []byte
		err := tx.QueryRowContext(ctx, `SELECT sketch FROM visitors_daily WHERE site_id = ? AND day = ? FOR UPDATE`,
			k.SiteID, k.Day).Scan(&blob)
		if errors.Is(err, sql.ErrNoRows) {
			out, err := sk.MarshalBinary()
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO visitors_daily (site_id, day, sketch) VALUES (?,?,?)
				ON CONFLICT (site_id, day) DO NOTHING`, k.SiteID, k.Day, out)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		merged := hyperloglog.New16()
		if err := merged.UnmarshalBinary(blob); err != nil {
			return err
		}
		if err := merged.Merge(sk); err != nil {
			return err
		}
		out, err := merged.MarshalBinary()
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE visitors_daily SET sketch = ? WHERE site_id = ? AND day = ?`, out, k.SiteID, k.Day)
		return err
	}
}

func sortedHourKeys[V any](m map[HourKey]V) []HourKey {
	return slices.SortedFunc(maps.Keys(m), func(x, y HourKey) int {
		return cmp.Or(strings.Compare(x.SiteID, y.SiteID), cmp.Compare(x.Hour, y.Hour))
	})
}

func (s *Store) IngestState(ctx context.Context, name string) (IngestState, error) {
	st := IngestState{Name: name}
	var inode int64
	err := s.db.QueryRowContext(ctx, `SELECT inode, "offset" FROM ingest_state WHERE name = ?`, name).Scan(&inode, &st.Offset)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	st.Inode = uint64(inode)
	return st, err
}

type TrafficPoint struct {
	Hour time.Time `json:"hour"`
	Counters
}

type SiteStats struct {
	Since          time.Time      `json:"since"`
	Totals         Counters       `json:"totals"`
	UniqueVisitors uint64         `json:"unique_visitors"`
	Series         []TrafficPoint `json:"series"`
}

// SiteStats returns hourly traffic since the given time. Unique visitors are
// estimated with HyperLogLog (~0.8% error) at whole-day granularity: a
// "last 24h" query counts visitors of every UTC day the window touches.
func (s *Store) SiteStats(ctx context.Context, siteID string, since time.Time) (*SiteStats, error) {
	since = since.UTC().Truncate(time.Hour)
	st := &SiteStats{Since: since, Series: []TrafficPoint{}}

	rows, err := s.db.QueryContext(ctx, `
		SELECT hour, requests, page_views, bytes_out, bot_hits, blocked, errors_5xx
		FROM traffic_hourly WHERE site_id = ? AND hour >= ? ORDER BY hour`, siteID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p TrafficPoint
		var h int64
		if err := rows.Scan(&h, &p.Requests, &p.PageViews, &p.BytesOut, &p.BotHits, &p.Blocked, &p.Errors5xx); err != nil {
			return nil, err
		}
		p.Hour = time.Unix(h, 0).UTC()
		st.Totals.Add(p.Counters)
		st.Series = append(st.Series, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	day := since.Truncate(24 * time.Hour).Unix()
	vrows, err := s.db.QueryContext(ctx, `SELECT sketch FROM visitors_daily WHERE site_id = ? AND day >= ?`, siteID, day)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	total := hyperloglog.New16()
	for vrows.Next() {
		var blob []byte
		if err := vrows.Scan(&blob); err != nil {
			return nil, err
		}
		sk := hyperloglog.New16()
		if err := sk.UnmarshalBinary(blob); err != nil {
			return nil, err
		}
		if err := total.Merge(sk); err != nil {
			return nil, err
		}
	}
	st.UniqueVisitors = total.Estimate()
	return st, vrows.Err()
}
