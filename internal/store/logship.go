package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Log shipping (internal/logship): how much of each kind of log this
// server collected per day (the Logs page's volumes), and the rows of the
// panel's own tables the exporters read after a cursor. The cursors live in
// ingest_state, next to the access log's offset.
const logshipSchema = `CREATE TABLE logship_volume (
		day     INTEGER NOT NULL,
		kind    TEXT NOT NULL,
		events  INTEGER NOT NULL DEFAULT 0,
		bytes   INTEGER NOT NULL DEFAULT 0,
		dropped INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, kind)
	);`

// LogVolume is what one kind of log amounted to on one (UTC) day.
type LogVolume struct {
	Day     time.Time `json:"day"`
	Kind    string    `json:"kind"`
	Events  int64     `json:"events"`
	Bytes   int64     `json:"bytes"`
	Dropped int64     `json:"dropped"`
}

// AddLogVolume adds to a day's counters of a kind of log (the day is
// truncated to midnight UTC).
func (s *Store) AddLogVolume(ctx context.Context, v LogVolume) error {
	day := v.Day.UTC().Truncate(24 * time.Hour).Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO logship_volume (day, kind, events, bytes, dropped) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (day, kind) DO UPDATE SET events = logship_volume.events + excluded.events,
		bytes = logship_volume.bytes + excluded.bytes, dropped = logship_volume.dropped + excluded.dropped`,
		day, v.Kind, v.Events, v.Bytes, v.Dropped)
	return err
}

// LogVolumes returns the counters of the days since since, oldest first.
func (s *Store) LogVolumes(ctx context.Context, since time.Time) ([]LogVolume, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, kind, events, bytes, dropped FROM logship_volume WHERE day >= ?
		ORDER BY day, kind`, since.UTC().Truncate(24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogVolume{}
	for rows.Next() {
		var v LogVolume
		var day int64
		if err := rows.Scan(&day, &v.Kind, &v.Events, &v.Bytes, &v.Dropped); err != nil {
			return nil, err
		}
		v.Day = time.Unix(day, 0).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// PruneLogVolumes forgets the days before before.
func (s *Store) PruneLogVolumes(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM logship_volume WHERE day < ?`, before.UTC().Truncate(24*time.Hour).Unix())
	return err
}

// LogCursor is an exporter's position: the last row ID it shipped (0 and
// false when it never ran).
func (s *Store) LogCursor(ctx context.Context, name string) (int64, bool, error) {
	var off int64
	err := s.db.QueryRowContext(ctx, `SELECT "offset" FROM ingest_state WHERE name = ?`, "logship:"+name).Scan(&off)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return off, err == nil, err
}

// SetLogCursor records an exporter's position.
func (s *Store) SetLogCursor(ctx context.Context, name string, id int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ingest_state (name, inode, "offset") VALUES (?, 0, ?)
		ON CONFLICT (name) DO UPDATE SET "offset" = excluded."offset"`, "logship:"+name, id)
	return err
}

// LogTableMax is the newest row ID of a table the exporters read (0 when
// it's empty): where a newly enabled exporter starts.
func (s *Store) LogTableMax(ctx context.Context, table string) (int64, error) {
	switch table {
	case "audit_log", "jobs", "account_events", "mail_outbox":
	default:
		return 0, fmt.Errorf("no log table %q", table)
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM `+table).Scan(&id)
	return id, err
}

// AuditAfter returns audit entries with an ID above after, oldest first.
func (s *Store) AuditAfter(ctx context.Context, after int64, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, time, actor, ip, action, target, status, detail FROM audit_log
		WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.Actor, &e.IP, &e.Action, &e.Target, &e.Status, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = time.Unix(t, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// JobsAfter returns jobs with an ID above after, oldest first, finished or
// not (the exporter waits for the unfinished).
func (s *Store) JobsAfter(ctx context.Context, after int64, limit int) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// LoggedAccountEvent is an account's activity line with the account it
// belongs to.
type LoggedAccountEvent struct {
	AccountEvent
	AccountID int64 `json:"account_id"`
}

// AccountEventsAfter returns every account's activity lines with an ID
// above after, oldest first.
func (s *Store) AccountEventsAfter(ctx context.Context, after int64, limit int) ([]LoggedAccountEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, at, kind, message FROM account_events WHERE id > ?
		ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoggedAccountEvent{}
	for rows.Next() {
		var e LoggedAccountEvent
		var t int64
		if err := rows.Scan(&e.ID, &e.AccountID, &t, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.Time = time.Unix(t, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// MailAfter returns outbox messages with an ID above after, oldest first
// (bodies included: callers keep only the metadata).
func (s *Store) MailAfter(ctx context.Context, after int64, limit int) ([]*MailMessage, error) {
	return s.mails(ctx, `WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
}
