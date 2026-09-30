package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The outbox of internal/mailer: every e-mail the panel sends (invoices,
// reminders, ticket replies) is rendered once, stored here and delivered
// with retries by a background loop. The table doubles as the e-mail log
// staff and clients see. A dedupe key (NULL: none) makes sending
// idempotent: automation that runs again queues nothing twice.
const mailerSchema = `CREATE TABLE mail_outbox (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		dedupe_key      TEXT UNIQUE,
		account_id      INTEGER NOT NULL DEFAULT 0,
		template        TEXT NOT NULL DEFAULT '',
		recipients      TEXT NOT NULL,
		reply_to        TEXT NOT NULL DEFAULT '',
		subject         TEXT NOT NULL,
		body_text       TEXT NOT NULL,
		body_html       TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL,
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL,
		last_error      TEXT NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		sent_at         INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX mail_outbox_due ON mail_outbox (status, next_attempt_at);
	CREATE INDEX mail_outbox_by_account ON mail_outbox (account_id, id);`

// Outbox message states.
const (
	MailPending = "pending"
	MailSent    = "sent"
	MailFailed  = "failed"
)

// MailMessage is one queued e-mail, already rendered.
type MailMessage struct {
	ID            int64     `json:"id"`
	DedupeKey     string    `json:"-"`
	AccountID     int64     `json:"account_id"`
	Template      string    `json:"template"`
	To            []string  `json:"to"`
	ReplyTo       string    `json:"reply_to,omitempty"`
	Subject       string    `json:"subject"`
	Text          string    `json:"text,omitempty"`
	HTML          string    `json:"html,omitempty"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	SentAt        time.Time `json:"sent_at,omitzero"`
}

// keepMail bounds the outbox; the oldest finished messages go first.
const keepMail = 10000

const mailCols = `id, COALESCE(dedupe_key, ''), account_id, template, recipients, reply_to, subject, body_text, body_html,
	status, attempts, next_attempt_at, last_error, created_at, sent_at`

func scanMail(row interface{ Scan(...any) error }) (*MailMessage, error) {
	var m MailMessage
	var to string
	var next, created, sent int64
	err := row.Scan(&m.ID, &m.DedupeKey, &m.AccountID, &m.Template, &to, &m.ReplyTo, &m.Subject, &m.Text, &m.HTML,
		&m.Status, &m.Attempts, &next, &m.LastError, &created, &sent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.To = splitList(to)
	m.NextAttemptAt, m.CreatedAt = time.Unix(next, 0).UTC(), time.Unix(created, 0).UTC()
	if sent > 0 {
		m.SentAt = time.Unix(sent, 0).UTC()
	}
	return &m, nil
}

// EnqueueMail queues a rendered message, due now. With a dedupe key that
// was queued before it does nothing and reports false.
func (s *Store) EnqueueMail(ctx context.Context, m *MailMessage, now time.Time) (bool, error) {
	var key any
	if m.DedupeKey != "" {
		key = m.DedupeKey
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO mail_outbox (dedupe_key, account_id, template, recipients, reply_to,
		subject, body_text, body_html, status, next_attempt_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO NOTHING RETURNING id`, key, m.AccountID, m.Template, strings.Join(m.To, ","), m.ReplyTo,
		m.Subject, m.Text, m.HTML, MailPending, now.Unix(), now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mail_outbox WHERE status <> ? AND id <=
		(SELECT id FROM mail_outbox ORDER BY id DESC LIMIT 1 OFFSET ?)`, MailPending, keepMail); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	m.ID, m.Status, m.NextAttemptAt, m.CreatedAt = id, MailPending, now.UTC(), now.UTC()
	return true, nil
}

// MailQueued reports whether a message with this dedupe key was queued.
func (s *Store) MailQueued(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_outbox WHERE dedupe_key = ?`, key).Scan(&n)
	return n > 0, err
}

// DueMail returns pending messages whose next attempt is due.
func (s *Store) DueMail(ctx context.Context, now time.Time, limit int) ([]*MailMessage, error) {
	return s.mails(ctx, `WHERE status = ? AND next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT ?`,
		MailPending, now.Unix(), limit)
}

// MailFilter selects messages for the e-mail log.
type MailFilter struct {
	AccountID int64  // 0: every account (and none)
	Status    string // "": any
	BeforeID  int64  // paging: only older messages (0: from the newest)
	Limit     int
}

// MailLog lists messages, newest first.
func (s *Store) MailLog(ctx context.Context, f MailFilter) ([]*MailMessage, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	return s.mails(ctx, `WHERE (? = 0 OR account_id = ?) AND (? = '' OR status = ?) AND (? = 0 OR id < ?)
		ORDER BY id DESC LIMIT ?`, f.AccountID, f.AccountID, f.Status, f.Status, f.BeforeID, f.BeforeID, f.Limit)
}

func (s *Store) GetMail(ctx context.Context, id int64) (*MailMessage, error) {
	return scanMail(s.db.QueryRowContext(ctx, `SELECT `+mailCols+` FROM mail_outbox WHERE id = ?`, id))
}

func (s *Store) mails(ctx context.Context, where string, args ...any) ([]*MailMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mailCols+` FROM mail_outbox `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MailMessage{}
	for rows.Next() {
		m, err := scanMail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordMail stores the outcome of a delivery attempt.
func (s *Store) RecordMail(ctx context.Context, m *MailMessage) error {
	var sent int64
	if !m.SentAt.IsZero() {
		sent = m.SentAt.Unix()
	}
	return s.exec1(ctx, `UPDATE mail_outbox SET status = ?, attempts = ?, next_attempt_at = ?, last_error = ?,
		sent_at = ? WHERE id = ?`, m.Status, m.Attempts, m.NextAttemptAt.Unix(), m.LastError, sent, m.ID)
}

// RetryMail queues a message again, now (a failed one, or a sent one to
// resend it).
func (s *Store) RetryMail(ctx context.Context, id int64, now time.Time) error {
	return s.exec1(ctx, `UPDATE mail_outbox SET status = ?, next_attempt_at = ?, last_error = '' WHERE id = ?`,
		MailPending, now.Unix(), id)
}
