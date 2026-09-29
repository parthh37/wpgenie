package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Event is a line in a site's activity log: autoscaling decisions, updates,
// scans. Kept short and human-readable; the panel shows them as-is.
type Event struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// keepEvents bounds each site's log; older lines are dropped on insert.
const keepEvents = 500

func (s *Store) AddEvent(ctx context.Context, siteID, kind, msg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO site_events (site_id, time, kind, message) VALUES (?, ?, ?, ?)`,
		siteID, time.Now().Unix(), kind, msg); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM site_events WHERE site_id = ? AND id <=
		(SELECT id FROM site_events WHERE site_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		siteID, siteID, keepEvents); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Events(ctx context.Context, siteID string, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, time, kind, message FROM site_events
		WHERE site_id = ? ORDER BY id DESC LIMIT ?`, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.Time = time.Unix(t, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateRun is one run of the WordPress update manager on a site.
type UpdateRun struct {
	ID         int64     `json:"id"`
	SiteID     string    `json:"site_id"`
	Trigger    string    `json:"trigger"` // manual | auto
	Status     string    `json:"status"`  // running | updated | rolled_back | failed | up_to_date
	Summary    string    `json:"summary"`
	Details    string    `json:"-"` // JSON, decoded by the site package
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

const UpdateRunning = "running"

func (s *Store) StartUpdate(ctx context.Context, siteID, trigger string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO site_updates (site_id, trigger, status, started_at) VALUES (?, ?, ?, ?)`,
		siteID, trigger, UpdateRunning, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishUpdate(ctx context.Context, id int64, status, summary, details string) error {
	return s.exec1(ctx, `UPDATE site_updates SET status = ?, summary = ?, details = ?, finished_at = ? WHERE id = ?`,
		status, summary, details, time.Now().Unix(), id)
}

// FailInterruptedUpdates marks runs left "running" by a daemon that stopped
// mid-update. Run once at startup, before any new update can start.
func (s *Store) FailInterruptedUpdates(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE site_updates SET status = 'failed',
		summary = 'interrupted: the WPGenie daemon stopped during the update; check the site and its snapshot', finished_at = ?
		WHERE status = ?`, time.Now().Unix(), UpdateRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) Updates(ctx context.Context, siteID string, limit int) ([]UpdateRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, site_id, trigger, status, summary, details, started_at, finished_at
		FROM site_updates WHERE site_id = ? ORDER BY id DESC LIMIT ?`, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UpdateRun{}
	for rows.Next() {
		var u UpdateRun
		var started, finished int64
		if err := rows.Scan(&u.ID, &u.SiteID, &u.Trigger, &u.Status, &u.Summary, &u.Details, &started, &finished); err != nil {
			return nil, err
		}
		u.StartedAt = time.Unix(started, 0).UTC()
		if finished > 0 {
			u.FinishedAt = time.Unix(finished, 0).UTC()
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SaveScan stores the latest security scan report (JSON) of a site.
func (s *Store) SaveScan(ctx context.Context, siteID string, at time.Time, report []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_scans (site_id, scanned_at, report) VALUES (?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET scanned_at = excluded.scanned_at, report = excluded.report`,
		siteID, at.Unix(), string(report))
	return err
}

// Scan returns the latest scan report, or ErrNotFound if the site was never
// scanned.
func (s *Store) Scan(ctx context.Context, siteID string) (time.Time, []byte, error) {
	var at int64
	var report string
	err := s.db.QueryRowContext(ctx, `SELECT scanned_at, report FROM site_scans WHERE site_id = ?`, siteID).Scan(&at, &report)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil, ErrNotFound
	}
	return time.Unix(at, 0).UTC(), []byte(report), err
}

// Setting returns a panel-wide setting, or "" if it was never set.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
