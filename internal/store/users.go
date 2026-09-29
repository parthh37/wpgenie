package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// User is a panel account. Password and TOTP material never leave the store
// in API responses (json:"-").
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
	// AccountID is the tenant account the user belongs to (see
	// accounts.go); 0 for staff (viewer, operator, admin).
	AccountID int64 `json:"account_id,omitempty"`
	// TOTPEnabled is derived from TOTPSecret.
	TOTPEnabled bool      `json:"totp_enabled"`
	CreatedAt   time.Time `json:"created_at"`
	LastLoginAt time.Time `json:"last_login_at,omitzero"`

	PasswordHash string `json:"-"`
	TOTPSecret   string `json:"-"`
	// TOTPPending is a secret being enrolled: it becomes TOTPSecret once the
	// user proves their authenticator produces codes for it.
	TOTPPending  string `json:"-"`
	TOTPLastStep int64  `json:"-"` // last accepted time step: codes are single-use
	// RecoveryHashes are SHA-256 hashes of unused recovery codes.
	RecoveryHashes []string `json:"-"`
	RecoveryLeft   int      `json:"recovery_codes_left"`
}

const userCols = `id, username, password, role, totp_secret, totp_pending, totp_last_step, recovery, disabled,
	created_at, last_login_at, account_id`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var recovery string
	var created, lastLogin int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.TOTPSecret, &u.TOTPPending, &u.TOTPLastStep,
		&recovery, &u.Disabled, &created, &lastLogin, &u.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(recovery), &u.RecoveryHashes); err != nil {
		return nil, err
	}
	u.TOTPEnabled, u.RecoveryLeft = u.TOTPSecret != "", len(u.RecoveryHashes)
	u.CreatedAt = time.Unix(created, 0).UTC()
	if lastLogin > 0 {
		u.LastLoginAt = time.Unix(lastLogin, 0).UTC()
	}
	return &u, nil
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	now := time.Now().Unix()
	// The username is unique case-insensitively (COLLATE NOCASE; its
	// PostgreSQL translation is a case-insensitive collation).
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO users (username, password, role, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?) RETURNING id`, username, passwordHash, role, now, now).Scan(&id)
	if isUnique(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, id)
}

// CreateFirstUser creates a user only if there are none yet, atomically:
// two concurrent first-run setups can't both create an administrator.
// NOT EXISTS alone would not do on PostgreSQL, where two transactions can
// both see an empty table; the table lock makes the second one wait and
// then see the first one's user.
func (s *Store) CreateFirstUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	now := time.Now().Unix()
	var id int64
	err := s.db.inTx(ctx, func(tx *Tx) error {
		if err := tx.lockTable(ctx, "users"); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `INSERT INTO users (username, password, role, created_at, updated_at)
			SELECT ?, ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users) RETURNING id`,
			username, passwordHash, role, now, now).Scan(&id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, id)
}

func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountActiveAdmins counts enabled administrators other than exceptID
// (0 counts them all): the panel must never lose its last one.
func (s *Store) CountActiveAdmins(ctx context.Context, exceptID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id != ?`,
		exceptID).Scan(&n)
	return n, err
}

func (s *Store) SetUserPassword(ctx context.Context, id int64, hash string) error {
	return s.exec1(ctx, `UPDATE users SET password = ?, updated_at = ? WHERE id = ?`, hash, time.Now().Unix(), id)
}

func (s *Store) SetUserRole(ctx context.Context, id int64, role string, disabled bool) error {
	return s.exec1(ctx, `UPDATE users SET role = ?, disabled = ?, updated_at = ? WHERE id = ?`,
		role, disabled, time.Now().Unix(), id)
}

func (s *Store) SetTOTPPending(ctx context.Context, id int64, secret string) error {
	return s.exec1(ctx, `UPDATE users SET totp_pending = ?, updated_at = ? WHERE id = ?`, secret, time.Now().Unix(), id)
}

// EnableTOTP promotes the pending secret, stores the recovery code hashes
// and the step of the code that proved it (so that code can't be replayed).
func (s *Store) EnableTOTP(ctx context.Context, id int64, secret string, step int64, recovery []string) error {
	b, err := json.Marshal(recovery)
	if err != nil {
		return err
	}
	return s.exec1(ctx, `UPDATE users SET totp_secret = ?, totp_pending = '', totp_last_step = ?, recovery = ?,
		updated_at = ? WHERE id = ?`, secret, step, string(b), time.Now().Unix(), id)
}

func (s *Store) DisableTOTP(ctx context.Context, id int64) error {
	return s.exec1(ctx, `UPDATE users SET totp_secret = '', totp_pending = '', totp_last_step = 0, recovery = '[]',
		updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

// UseTOTPStep records an accepted code's time step, only if it is newer than
// the last one: of two concurrent logins with the same code, one fails. One
// conditional UPDATE is atomic on both backends (on PostgreSQL the second
// writer waits for the first one's row lock, then re-checks the condition
// against the updated row).
func (s *Store) UseTOTPStep(ctx context.Context, id, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`,
		step, id, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// UseRecoveryCode removes a recovery code hash, reporting whether it was
// still there (each code works once). The list is replaced only if it is
// still the one read (compare-and-swap): of two concurrent sign-ins with
// the same code, whichever writes second finds the list changed, reads it
// again and no longer finds its code. No lock needed on either backend.
func (s *Store) UseRecoveryCode(ctx context.Context, id int64, hash string) (bool, error) {
	for {
		var raw string
		if err := s.db.QueryRowContext(ctx, `SELECT recovery FROM users WHERE id = ?`, id).Scan(&raw); err != nil {
			return false, err
		}
		var hashes []string
		if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
			return false, err
		}
		i := -1
		for j, h := range hashes {
			if h == hash {
				i = j
			}
		}
		if i < 0 {
			return false, nil
		}
		b, _ := json.Marshal(append(hashes[:i:i], hashes[i+1:]...))
		res, err := s.db.ExecContext(ctx, `UPDATE users SET recovery = ? WHERE id = ? AND recovery = ?`, string(b), id, raw)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return true, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
}

func (s *Store) TouchLogin(ctx context.Context, id int64, at time.Time) error {
	return s.exec1(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, at.Unix(), id)
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.exec1(ctx, `DELETE FROM users WHERE id = ?`, id)
}

// Session is a signed-in browser. The cookie holds a random token; only
// its hash is stored, so a copy of the database can't be used to sign in.
type Session struct {
	ID         string    `json:"id"`
	UserID     int64     `json:"user_id"`
	Username   string    `json:"username"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
}

func (s *Store) CreateSession(ctx context.Context, sess *Session, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, token_hash, user_id, created_at, last_seen_at, expires_at,
		ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sess.ID, tokenHash, sess.UserID, sess.CreatedAt.Unix(),
		sess.LastSeenAt.Unix(), sess.ExpiresAt.Unix(), sess.IP, sess.UserAgent)
	return err
}

const sessionCols = `s.id, s.user_id, u.username, s.created_at, s.last_seen_at, s.expires_at, s.ip, s.user_agent`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	var created, seen, expires int64
	err := row.Scan(&sess.ID, &sess.UserID, &sess.Username, &created, &seen, &expires, &sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, sess.LastSeenAt = time.Unix(created, 0).UTC(), time.Unix(seen, 0).UTC()
	sess.ExpiresAt = time.Unix(expires, 0).UTC()
	return &sess, nil
}

func (s *Store) SessionByToken(ctx context.Context, tokenHash string) (*Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions s
		JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, tokenHash))
}

// Sessions lists sessions, of one user or (userID 0) everyone's.
func (s *Store) Sessions(ctx context.Context, userID int64) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE ? = 0 OR s.user_id = ? ORDER BY s.last_seen_at DESC, s.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// TouchSession moves a session's last activity forward (never back: with
// several requests, or nodes, touching at once, the latest time wins
// whatever order the writes land in).
func (s *Store) TouchSession(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ? AND last_seen_at < ?`,
		at.Unix(), id, at.Unix())
	return err
}

// DeleteSession removes a session; with userID != 0, only one of that
// user's (a user may only sign out their own sessions).
func (s *Store) DeleteSession(ctx context.Context, id string, userID int64) error {
	return s.exec1(ctx, `DELETE FROM sessions WHERE id = ? AND (? = 0 OR user_id = ?)`, id, userID, userID)
}

// DeleteUserSessions signs a user out everywhere, except keepID.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keepID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id != ?`, userID, keepID)
	return err
}

// PruneSessions deletes sessions that expired or were idle for longer than idle.
func (s *Store) PruneSessions(ctx context.Context, now time.Time, idle time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		now.Unix(), now.Add(-idle).Unix())
	return err
}

// AuditEntry is one line of the audit log: who did what, from where.
type AuditEntry struct {
	ID     int64     `json:"id"`
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	IP     string    `json:"ip"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Status int       `json:"status,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// keepAudit bounds the audit log; the oldest entries go first.
const keepAudit = 20000

func (s *Store) AddAudit(ctx context.Context, e AuditEntry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO audit_log (time, actor, ip, action, target, status, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`, e.Time.Unix(), e.Actor, e.IP, e.Action, e.Target, e.Status,
		e.Detail).Scan(&id); err != nil {
		return err
	}
	// Trim occasionally rather than on every insert.
	if id%100 == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE id <= ?`, id-keepAudit)
		return err
	}
	return nil
}

// Audit returns the newest entries first; actor filters when not empty.
func (s *Store) Audit(ctx context.Context, actor string, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, time, actor, ip, action, target, status, detail FROM audit_log
		WHERE ? = '' OR actor = ? ORDER BY id DESC LIMIT ?`, actor, actor, limit)
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

// SavePluginReport stores the latest plugin analysis (JSON) of a site.
func (s *Store) SavePluginReport(ctx context.Context, siteID string, at time.Time, report []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_plugin_reports (site_id, analysed_at, report) VALUES (?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET analysed_at = excluded.analysed_at, report = excluded.report`,
		siteID, at.Unix(), string(report))
	return err
}

// PluginReport returns the latest plugin analysis, or ErrNotFound.
func (s *Store) PluginReport(ctx context.Context, siteID string) (time.Time, []byte, error) {
	var at int64
	var report string
	err := s.db.QueryRowContext(ctx, `SELECT analysed_at, report FROM site_plugin_reports WHERE site_id = ?`,
		siteID).Scan(&at, &report)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil, ErrNotFound
	}
	return time.Unix(at, 0).UTC(), []byte(report), err
}
