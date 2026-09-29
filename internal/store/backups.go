package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// BackupRepo is a restic repository backups go to. The password and
// secrets never leave the panel through the API (except the password, on
// an administrator's explicit request: without it a copy of the repository
// can't be restored anywhere else).
type BackupRepo struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Kind     string      `json:"kind"`     // local | s3 | b2 | sftp
	Location string      `json:"location"` // restic repository string, secrets excluded
	Password string      `json:"-"`
	Secrets  RepoSecrets `json:"-"`
	// PublicKey is the SSH key an SFTP repository logs in with (to add to
	// the remote's authorized_keys); HostKey the pinned server key.
	PublicKey  string    `json:"public_key,omitempty"`
	HostKey    string    `json:"host_key,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	CheckedAt  time.Time `json:"checked_at,omitzero"`
	CheckError string    `json:"check_error,omitempty"`
	PrunedAt   time.Time `json:"pruned_at,omitzero"`
	SitesUsing int       `json:"sites_using"`
}

// RepoSecrets are the credentials a repository needs besides its password.
type RepoSecrets struct {
	AccessKeyID     string `json:"access_key_id,omitempty"` // S3 / B2 key ID
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	Region          string `json:"region,omitempty"`
	SSHPrivateKey   string `json:"ssh_private_key,omitempty"` // OpenSSH PEM
	SSHPublicKey    string `json:"ssh_public_key,omitempty"`
	KnownHosts      string `json:"known_hosts,omitempty"` // pinned host key line
}

const repoCols = `r.id, r.name, r.kind, r.location, r.password, r.secrets, r.created_at, r.checked_at, r.check_error,
	r.pruned_at, (SELECT COUNT(*) FROM site_backup_policy p WHERE p.repo_id = r.id)`

func scanRepo(row interface{ Scan(...any) error }) (*BackupRepo, error) {
	var r BackupRepo
	var secrets string
	var created, checked, pruned int64
	err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.Location, &r.Password, &secrets, &created, &checked, &r.CheckError,
		&pruned, &r.SitesUsing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(secrets), &r.Secrets); err != nil {
		return nil, err
	}
	r.PublicKey, r.HostKey = r.Secrets.SSHPublicKey, r.Secrets.KnownHosts
	r.CreatedAt = time.Unix(created, 0).UTC()
	if checked > 0 {
		r.CheckedAt = time.Unix(checked, 0).UTC()
	}
	if pruned > 0 {
		r.PrunedAt = time.Unix(pruned, 0).UTC()
	}
	return &r, nil
}

func (s *Store) CreateRepo(ctx context.Context, r *BackupRepo) error {
	b, err := json.Marshal(r.Secrets)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO backup_repos (id, name, kind, location, password, secrets, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Name, r.Kind, r.Location, r.Password, string(b), time.Now().Unix())
	return err
}

// PutRepo creates or updates a repository copied from the panel (on a
// node): same ID, password and secrets as where it was added.
func (s *Store) PutRepo(ctx context.Context, r *BackupRepo) error {
	b, err := json.Marshal(r.Secrets)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO backup_repos (id, name, kind, location, password, secrets, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET name = excluded.name, kind = excluded.kind,
		location = excluded.location, password = excluded.password, secrets = excluded.secrets`,
		r.ID, r.Name, r.Kind, r.Location, r.Password, string(b), time.Now().Unix())
	return err
}

func (s *Store) GetRepo(ctx context.Context, id string) (*BackupRepo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM backup_repos r WHERE r.id = ?`, id))
}

func (s *Store) Repos(ctx context.Context) ([]*BackupRepo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoCols+` FROM backup_repos r ORDER BY r.created_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*BackupRepo{}
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RenameRepo(ctx context.Context, id, name string) error {
	return s.exec1(ctx, `UPDATE backup_repos SET name = ? WHERE id = ?`, name, id)
}

// DeleteRepo forgets a repository (its data stays where it is). The
// foreign key refuses while a site's policy still uses it.
func (s *Store) DeleteRepo(ctx context.Context, id string) error {
	return s.exec1(ctx, `DELETE FROM backup_repos WHERE id = ?`, id)
}

func (s *Store) RepoChecked(ctx context.Context, id string, checkErr string) error {
	return s.exec1(ctx, `UPDATE backup_repos SET checked_at = ?, check_error = ? WHERE id = ?`,
		time.Now().Unix(), checkErr, id)
}

func (s *Store) RepoPruned(ctx context.Context, id string) error {
	return s.exec1(ctx, `UPDATE backup_repos SET pruned_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

// BackupPolicy is when and where a site is backed up, and which backups
// are kept (restic's --keep-* semantics; 0 disables that rule).
type BackupPolicy struct {
	SiteID        string    `json:"site_id"`
	RepoID        string    `json:"repo_id"`
	IntervalHours int       `json:"interval_hours"` // 0 = manual backups only
	KeepLast      int       `json:"keep_last"`
	KeepDaily     int       `json:"keep_daily"`
	KeepWeekly    int       `json:"keep_weekly"`
	KeepMonthly   int       `json:"keep_monthly"`
	LastBackupAt  time.Time `json:"last_backup_at,omitzero"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
}

const policyCols = `site_id, repo_id, interval_hours, keep_last, keep_daily, keep_weekly, keep_monthly,
	last_backup_at, last_attempt_at, last_error`

func scanPolicy(row interface{ Scan(...any) error }) (*BackupPolicy, error) {
	var p BackupPolicy
	var last, attempt int64
	err := row.Scan(&p.SiteID, &p.RepoID, &p.IntervalHours, &p.KeepLast, &p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly,
		&last, &attempt, &p.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if last > 0 {
		p.LastBackupAt = time.Unix(last, 0).UTC()
	}
	if attempt > 0 {
		p.LastAttemptAt = time.Unix(attempt, 0).UTC()
	}
	return &p, nil
}

func (s *Store) BackupPolicy(ctx context.Context, siteID string) (*BackupPolicy, error) {
	return scanPolicy(s.db.QueryRowContext(ctx, `SELECT `+policyCols+` FROM site_backup_policy WHERE site_id = ?`, siteID))
}

func (s *Store) BackupPolicies(ctx context.Context) ([]*BackupPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+policyCols+` FROM site_backup_policy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BackupPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetBackupPolicy creates or replaces a site's policy, keeping its history
// (last backup and attempt) when the repository stays the same.
func (s *Store) SetBackupPolicy(ctx context.Context, p *BackupPolicy) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_backup_policy (site_id, repo_id, interval_hours, keep_last,
		keep_daily, keep_weekly, keep_monthly) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET
			last_backup_at = CASE WHEN repo_id = excluded.repo_id THEN last_backup_at ELSE 0 END,
			last_attempt_at = CASE WHEN repo_id = excluded.repo_id THEN last_attempt_at ELSE 0 END,
			last_error = CASE WHEN repo_id = excluded.repo_id THEN last_error ELSE '' END,
			repo_id = excluded.repo_id, interval_hours = excluded.interval_hours, keep_last = excluded.keep_last,
			keep_daily = excluded.keep_daily, keep_weekly = excluded.keep_weekly, keep_monthly = excluded.keep_monthly`,
		p.SiteID, p.RepoID, p.IntervalHours, p.KeepLast, p.KeepDaily, p.KeepWeekly, p.KeepMonthly)
	return err
}

func (s *Store) DeleteBackupPolicy(ctx context.Context, siteID string) error {
	return s.exec1(ctx, `DELETE FROM site_backup_policy WHERE site_id = ?`, siteID)
}

// BackupAttempted records a backup's outcome; a success also moves
// last_backup_at, from which the schedule counts.
func (s *Store) BackupAttempted(ctx context.Context, siteID string, at time.Time, errMsg string) error {
	if errMsg == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE site_backup_policy SET last_attempt_at = ?, last_backup_at = ?,
			last_error = '' WHERE site_id = ?`, at.Unix(), at.Unix(), siteID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE site_backup_policy SET last_attempt_at = ?, last_error = ? WHERE site_id = ?`,
		at.Unix(), errMsg, siteID)
	return err
}
