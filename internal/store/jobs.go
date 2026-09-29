package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Job is a long operation (creating a site, a backup, a restore, a clone)
// run in the background, with progress the panel can follow.
type Job struct {
	ID         int64     `json:"id"`
	SiteID     string    `json:"site_id"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`   // queued | running | succeeded | failed
	Progress   int       `json:"progress"` // 0-100
	Step       string    `json:"step"`
	Error      string    `json:"error,omitempty"`
	Result     string    `json:"result,omitempty"` // JSON set by the job, never secrets
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// keepJobs bounds the jobs table; older finished jobs are dropped on insert.
const keepJobs = 2000

func (s *Store) CreateJob(ctx context.Context, siteID, kind, actor string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO jobs (site_id, kind, status, actor, created_at) VALUES (?, ?, ?, ?, ?)
		RETURNING id`, siteID, kind, JobQueued, actor, time.Now().Unix()).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE status IN (?, ?) AND id <=
		(SELECT id FROM jobs ORDER BY id DESC LIMIT 1 OFFSET ?)`, JobSucceeded, JobFailed, keepJobs); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) StartJob(ctx context.Context, id int64) error {
	return s.exec1(ctx, `UPDATE jobs SET status = ?, started_at = ? WHERE id = ?`, JobRunning, time.Now().Unix(), id)
}

func (s *Store) JobProgress(ctx context.Context, id int64, progress int, step string) error {
	return s.exec1(ctx, `UPDATE jobs SET progress = ?, step = ? WHERE id = ?`, progress, step, id)
}

func (s *Store) FinishJob(ctx context.Context, id int64, status, errMsg, result string) error {
	progress := 100
	if status != JobSucceeded {
		progress = -1 // keep what it reached
	}
	return s.exec1(ctx, `UPDATE jobs SET status = ?, error = ?, result = ?, finished_at = ?,
		progress = CASE WHEN ? < 0 THEN progress ELSE ? END WHERE id = ?`,
		status, errMsg, result, time.Now().Unix(), progress, progress, id)
}

// FailInterruptedJobs marks jobs a stopped daemon left queued or running.
// Run once at startup, before any job starts.
func (s *Store) FailInterruptedJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, finished_at = ?,
		error = 'interrupted: the WPGenie daemon stopped before this finished; check the site'
		WHERE status IN (?, ?)`, JobFailed, time.Now().Unix(), JobQueued, JobRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const jobCols = `id, site_id, kind, status, progress, step, error, result, actor, created_at, started_at, finished_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var created, started, finished int64
	err := row.Scan(&j.ID, &j.SiteID, &j.Kind, &j.Status, &j.Progress, &j.Step, &j.Error, &j.Result, &j.Actor,
		&created, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.CreatedAt = time.Unix(created, 0).UTC()
	if started > 0 {
		j.StartedAt = time.Unix(started, 0).UTC()
	}
	if finished > 0 {
		j.FinishedAt = time.Unix(finished, 0).UTC()
	}
	return &j, nil
}

func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

// Jobs lists the newest jobs, of one site when siteID isn't "", and only
// unfinished ones when active.
func (s *Store) Jobs(ctx context.Context, siteID string, active bool, limit int) ([]Job, error) {
	q := `SELECT ` + jobCols + ` FROM jobs WHERE (? = '' OR site_id = ?)`
	args := []any{siteID, siteID}
	if active {
		q += ` AND status IN (?, ?)`
		args = append(args, JobQueued, JobRunning)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}
