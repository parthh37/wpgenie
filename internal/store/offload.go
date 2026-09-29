package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Offload is a site's uploads offload: where its uploads are copied (an
// S3-compatible bucket and key prefix), where they are publicly readable,
// and how the syncs went. SecretKey is a secret: it is never serialised to
// API clients.
type Offload struct {
	SiteID      string
	Endpoint    string
	Region      string
	Bucket      string
	Prefix      string
	AccessKeyID string
	SecretKey   string
	PublicURL   string
	ACL         string
	// LocalDays > 0 removes local copies of files offloaded that long ago.
	LocalDays int

	// Starts of the last successful syncs (zero: never).
	IncrementalAt time.Time
	FullAt        time.Time
	AttemptAt     time.Time
	// Failures in a row (backoff); LastError is the latest one.
	Failures  int
	LastError string
	// LastObjects/LastBytes: uploaded by the last successful sync; Total*
	// since offload was turned on (or its location changed).
	LastObjects, LastBytes     int64
	TotalObjects, TotalBytes   int64
	TotalDeleted               int64
	RemovedLocal, RemovedBytes int64
	CleanedAt                  time.Time
	CreatedAt                  time.Time
}

const offloadCols = `site_id, endpoint, region, bucket, key_prefix, access_key_id, secret_key, public_url, object_acl,
	local_days, incremental_at, full_at, attempt_at, failures, last_error, last_objects, last_bytes, total_objects,
	total_bytes, total_deleted, removed_local, removed_bytes, cleaned_at, created_at`

func unixTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func scanOffload(row interface{ Scan(...any) error }) (*Offload, error) {
	var o Offload
	var inc, full, attempt, cleaned, created int64
	err := row.Scan(&o.SiteID, &o.Endpoint, &o.Region, &o.Bucket, &o.Prefix, &o.AccessKeyID, &o.SecretKey, &o.PublicURL,
		&o.ACL, &o.LocalDays, &inc, &full, &attempt, &o.Failures, &o.LastError, &o.LastObjects, &o.LastBytes,
		&o.TotalObjects, &o.TotalBytes, &o.TotalDeleted, &o.RemovedLocal, &o.RemovedBytes, &cleaned, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.IncrementalAt, o.FullAt, o.AttemptAt = unixTime(inc), unixTime(full), unixTime(attempt)
	o.CleanedAt, o.CreatedAt = unixTime(cleaned), unixTime(created)
	return &o, nil
}

// SetOffload creates or updates a site's offload settings. resetSync
// forgets the sync history (a new bucket or prefix starts empty): the next
// sync is a full one.
func (s *Store) SetOffload(ctx context.Context, o *Offload, resetSync bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO site_offload (site_id, endpoint, region, bucket, key_prefix,
		access_key_id, secret_key, public_url, object_acl, local_days, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (site_id) DO UPDATE SET endpoint = excluded.endpoint, region = excluded.region,
		bucket = excluded.bucket, key_prefix = excluded.key_prefix, access_key_id = excluded.access_key_id,
		secret_key = excluded.secret_key, public_url = excluded.public_url, object_acl = excluded.object_acl,
		local_days = excluded.local_days`,
		o.SiteID, o.Endpoint, o.Region, o.Bucket, o.Prefix, o.AccessKeyID, o.SecretKey, o.PublicURL, o.ACL,
		o.LocalDays, time.Now().Unix()); err != nil {
		return err
	}
	if resetSync {
		if _, err := tx.ExecContext(ctx, `UPDATE site_offload SET incremental_at = 0, full_at = 0, attempt_at = 0,
			failures = 0, last_error = '', last_objects = 0, last_bytes = 0, total_objects = 0, total_bytes = 0,
			total_deleted = 0, cleaned_at = 0 WHERE site_id = ?`, o.SiteID); err != nil {
			return err
		}
		// Deletes queued for the old location don't apply to the new one.
		if _, err := tx.ExecContext(ctx, `DELETE FROM offload_deletes WHERE site_id = ?`, o.SiteID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetOffload(ctx context.Context, siteID string) (*Offload, error) {
	return scanOffload(s.db.QueryRowContext(ctx, `SELECT `+offloadCols+` FROM site_offload WHERE site_id = ?`, siteID))
}

func (s *Store) ListOffload(ctx context.Context) ([]*Offload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+offloadCols+` FROM site_offload ORDER BY site_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Offload
	for rows.Next() {
		o, err := scanOffload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteOffload turns offload off for a site (its queued deletes go too).
func (s *Store) DeleteOffload(ctx context.Context, siteID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM offload_deletes WHERE site_id = ?`, `DELETE FROM site_offload WHERE site_id = ?`} {
		if _, err := tx.ExecContext(ctx, q, siteID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OffloadRun is the outcome of one sync.
type OffloadRun struct {
	Full    bool
	Started time.Time
	Err     error
	Objects int64 // uploaded
	Bytes   int64
	Deleted int64 // objects deleted from the bucket
}

// RecordOffloadRun stores a sync's outcome. Only a success moves the sync
// times forward (a failed incremental is retried from the same point); the
// counters count what was done either way.
func (s *Store) RecordOffloadRun(ctx context.Context, siteID string, r OffloadRun) error {
	if r.Err != nil {
		msg := r.Err.Error()
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		_, err := s.db.ExecContext(ctx, `UPDATE site_offload SET attempt_at = ?, failures = failures + 1, last_error = ?,
			total_objects = total_objects + ?, total_bytes = total_bytes + ?, total_deleted = total_deleted + ?
			WHERE site_id = ?`, r.Started.Unix(), msg, r.Objects, r.Bytes, r.Deleted, siteID)
		return err
	}
	full := `full_at`
	if r.Full {
		full = `?`
	}
	args := []any{r.Started.Unix(), r.Started.Unix()}
	if r.Full {
		args = append(args, r.Started.Unix())
	}
	args = append(args, r.Objects, r.Bytes, r.Objects, r.Bytes, r.Deleted, siteID)
	_, err := s.db.ExecContext(ctx, `UPDATE site_offload SET attempt_at = ?, incremental_at = ?, full_at = `+full+`,
		failures = 0, last_error = '', last_objects = ?, last_bytes = ?, total_objects = total_objects + ?,
		total_bytes = total_bytes + ?, total_deleted = total_deleted + ? WHERE site_id = ?`, args...)
	return err
}

// RecordOffloadCleanup counts local copies removed (local_days).
func (s *Store) RecordOffloadCleanup(ctx context.Context, siteID string, at time.Time, files, bytes int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE site_offload SET cleaned_at = ?, removed_local = removed_local + ?,
		removed_bytes = removed_bytes + ? WHERE site_id = ?`, at.Unix(), files, bytes, siteID)
	return err
}

// ResetOffloadRemoved records that every removed local copy is back.
func (s *Store) ResetOffloadRemoved(ctx context.Context, siteID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE site_offload SET removed_local = 0, removed_bytes = 0 WHERE site_id = ?`, siteID)
	return err
}

// QueueOffloadDeletes adds objects to delete, up to max pending for the
// site; it returns how many were refused for lack of room.
func (s *Store) QueueOffloadDeletes(ctx context.Context, siteID string, paths []string, max int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM offload_deletes WHERE site_id = ?`, siteID).Scan(&n); err != nil {
		return 0, err
	}
	now, refused := time.Now().Unix(), 0
	for _, p := range paths {
		if n >= max {
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM offload_deletes WHERE site_id = ? AND object_path = ?`, siteID, p).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				refused++ // already queued is no refusal
			} else if err != nil {
				return 0, err
			}
			continue
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO offload_deletes (site_id, object_path, queued_at) VALUES (?,?,?)
			ON CONFLICT (site_id, object_path) DO NOTHING`, siteID, p, now)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return refused, tx.Commit()
}

// PendingOffloadDeletes lists up to limit queued deletes, oldest first.
func (s *Store) PendingOffloadDeletes(ctx context.Context, siteID string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT object_path FROM offload_deletes WHERE site_id = ?
		ORDER BY queued_at, object_path LIMIT ?`, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DoneOffloadDeletes removes deletes that were carried out.
func (s *Store) DoneOffloadDeletes(ctx context.Context, siteID string, paths []string) error {
	for len(paths) > 0 {
		n := min(len(paths), 500)
		args := []any{siteID}
		for _, p := range paths[:n] {
			args = append(args, p)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM offload_deletes WHERE site_id = ? AND object_path IN (?`+
			strings.Repeat(",?", n-1)+`)`, args...); err != nil {
			return err
		}
		paths = paths[n:]
	}
	return nil
}

// CountOffloadDeletes is how many deletes are waiting.
func (s *Store) CountOffloadDeletes(ctx context.Context, siteID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM offload_deletes WHERE site_id = ?`, siteID).Scan(&n)
	return n, err
}
