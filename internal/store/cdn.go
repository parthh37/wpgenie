package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// CDN is a site's CDN configuration. APIToken is a secret: it is never
// serialised to API clients.
type CDN struct {
	SiteID    string
	Provider  string
	APIToken  string
	Zones     map[string]string // domain -> zone ID
	PurgedAt  time.Time         // zero: never purged
	LastError string
	CreatedAt time.Time
}

const cdnCols = `site_id, provider, api_token, zones, purged_at, last_error, created_at`

func scanCDN(row interface{ Scan(...any) error }) (*CDN, error) {
	var c CDN
	var zones string
	var purged, created int64
	err := row.Scan(&c.SiteID, &c.Provider, &c.APIToken, &zones, &purged, &c.LastError, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(zones), &c.Zones); err != nil {
		return nil, err
	}
	if c.Zones == nil {
		c.Zones = map[string]string{}
	}
	if purged > 0 {
		c.PurgedAt = time.Unix(0, purged).UTC()
	}
	c.CreatedAt = time.Unix(created, 0).UTC()
	return &c, nil
}

// SetCDN creates or replaces a site's CDN configuration.
func (s *Store) SetCDN(ctx context.Context, c *CDN) error {
	zones, err := json.Marshal(c.Zones)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO site_cdn (`+cdnCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (site_id) DO UPDATE SET provider = excluded.provider, api_token = excluded.api_token,
		zones = excluded.zones, purged_at = excluded.purged_at, last_error = excluded.last_error`,
		c.SiteID, c.Provider, c.APIToken, string(zones), unixNano(c.PurgedAt), c.LastError, time.Now().Unix())
	return err
}

func (s *Store) GetCDN(ctx context.Context, siteID string) (*CDN, error) {
	return scanCDN(s.db.QueryRowContext(ctx, `SELECT `+cdnCols+` FROM site_cdn WHERE site_id = ?`, siteID))
}

func (s *Store) ListCDN(ctx context.Context) ([]*CDN, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cdnCols+` FROM site_cdn ORDER BY site_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CDN
	for rows.Next() {
		c, err := scanCDN(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCDN(ctx context.Context, siteID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM site_cdn WHERE site_id = ?`, siteID)
	return err
}

// RecordCDNPurge stores the outcome of a purge attempt. started is only
// recorded on success; an error keeps the previous purged_at, so the purge
// is retried.
func (s *Store) RecordCDNPurge(ctx context.Context, siteID string, started time.Time, zones map[string]string, purgeErr error) error {
	z, err := json.Marshal(zones)
	if err != nil {
		return err
	}
	if purgeErr != nil {
		_, err = s.db.ExecContext(ctx, `UPDATE site_cdn SET last_error = ?, zones = ? WHERE site_id = ?`,
			purgeErr.Error(), string(z), siteID)
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE site_cdn SET purged_at = ?, last_error = '', zones = ? WHERE site_id = ?`,
		unixNano(started), string(z), siteID)
	return err
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
