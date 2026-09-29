package store

import (
	"context"
	"time"
)

// Alert is the state of one watched target (see internal/monitor). A row
// exists once the target has been judged at least once: that is how the
// monitor tells "was working and stopped" (page) from "never worked" (a
// site whose DNS doesn't point here yet: don't).
type Alert struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`              // site_down | certificate | disk | backup
	SiteID   string `json:"site_id,omitempty"` // for filtering; "" for server-wide alerts
	Target   string `json:"target"`            // what the alert is about: a domain, a path
	Severity string `json:"severity"`          // warning | critical; "" while resolved
	State    string `json:"state"`             // firing | resolved
	Message  string `json:"message"`
	// Since is when the current state (or severity) began.
	Since      time.Time `json:"since"`
	UpdatedAt  time.Time `json:"updated_at"`
	NotifiedAt time.Time `json:"notified_at,omitzero"`
}

const (
	AlertFiring   = "firing"
	AlertResolved = "resolved"
)

// AlertEvent is a line in the alert history: an alert fired, changed
// severity or resolved.
type AlertEvent struct {
	ID       int64     `json:"id"`
	Key      string    `json:"key"`
	Kind     string    `json:"kind"`
	SiteID   string    `json:"site_id,omitempty"`
	Target   string    `json:"target"`
	Severity string    `json:"severity"`
	State    string    `json:"state"`
	Message  string    `json:"message"`
	Time     time.Time `json:"time"`
}

// keepAlertHistory bounds the history; older lines are dropped on insert.
const keepAlertHistory = 1000

const alertCols = `alert_key, kind, site_id, target, severity, state, message, since, updated_at, notified_at`

// Alerts returns every alert row, firing ones first.
func (s *Store) Alerts(ctx context.Context) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+alertCols+` FROM alerts
		ORDER BY CASE WHEN state = ? THEN 0 ELSE 1 END, since DESC, alert_key`, AlertFiring)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		var since, updated, notified int64
		if err := rows.Scan(&a.Key, &a.Kind, &a.SiteID, &a.Target, &a.Severity, &a.State, &a.Message,
			&since, &updated, &notified); err != nil {
			return nil, err
		}
		a.Since, a.UpdatedAt = time.Unix(since, 0).UTC(), time.Unix(updated, 0).UTC()
		if notified > 0 {
			a.NotifiedAt = time.Unix(notified, 0).UTC()
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PutAlert creates or replaces an alert's state. With history, the change
// is also recorded in the (bounded) alert history, in the same transaction.
func (s *Store) PutAlert(ctx context.Context, a Alert, history bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO alerts (`+alertCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (alert_key) DO UPDATE SET kind = excluded.kind, site_id = excluded.site_id, target = excluded.target,
		severity = excluded.severity, state = excluded.state, message = excluded.message, since = excluded.since,
		updated_at = excluded.updated_at, notified_at = excluded.notified_at`,
		a.Key, a.Kind, a.SiteID, a.Target, a.Severity, a.State, a.Message, a.Since.Unix(), a.UpdatedAt.Unix(),
		unixOrZero(a.NotifiedAt)); err != nil {
		return err
	}
	if history {
		if _, err := tx.ExecContext(ctx, `INSERT INTO alert_history
			(alert_key, kind, site_id, target, severity, state, message, happened_at) VALUES (?,?,?,?,?,?,?,?)`,
			a.Key, a.Kind, a.SiteID, a.Target, a.Severity, a.State, a.Message, a.UpdatedAt.Unix()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM alert_history WHERE id <=
			(SELECT id FROM alert_history ORDER BY id DESC LIMIT 1 OFFSET ?)`, keepAlertHistory); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteAlert forgets a target that is no longer watched.
func (s *Store) DeleteAlert(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE alert_key = ?`, key)
	return err
}

// AlertsNotified records when alerts were last sent to the notification
// channels (re-notification counts from there).
func (s *Store) AlertsNotified(ctx context.Context, keys []string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `UPDATE alerts SET notified_at = ? WHERE alert_key = ?`, at.Unix(), k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AlertHistory returns the newest history lines first.
func (s *Store) AlertHistory(ctx context.Context, limit int) ([]AlertEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, alert_key, kind, site_id, target, severity, state, message, happened_at
		FROM alert_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertEvent{}
	for rows.Next() {
		var e AlertEvent
		var at int64
		if err := rows.Scan(&e.ID, &e.Key, &e.Kind, &e.SiteID, &e.Target, &e.Severity, &e.State, &e.Message, &at); err != nil {
			return nil, err
		}
		e.Time = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// JobCounts counts jobs by status (the jobs table is bounded).
func (s *Store) JobCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
