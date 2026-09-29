package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SFTPUser is an SFTP login jailed (chrooted) to one site's directory.
// Password is a crypt(3) hash ("" = keys only); PublicKeys are normalised
// authorized_keys lines without options.
type SFTPUser struct {
	Username   string    `json:"username"`
	SiteID     string    `json:"site_id"`
	Password   string    `json:"-"`
	HasPass    bool      `json:"password"`
	PublicKeys []string  `json:"public_keys"`
	CreatedAt  time.Time `json:"created_at"`
}

func scanSFTPUser(row interface{ Scan(...any) error }) (*SFTPUser, error) {
	var u SFTPUser
	var keys string
	var created int64
	err := row.Scan(&u.Username, &u.SiteID, &u.Password, &keys, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.HasPass = u.Password != ""
	u.PublicKeys = []string{}
	if keys != "" {
		u.PublicKeys = strings.Split(keys, "\n")
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	return &u, nil
}

const sftpCols = `username, site_id, password, public_keys, created_at`

func (s *Store) CreateSFTPUser(ctx context.Context, u *SFTPUser) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sftp_users (`+sftpCols+`) VALUES (?, ?, ?, ?, ?)`,
		u.Username, u.SiteID, u.Password, strings.Join(u.PublicKeys, "\n"), time.Now().Unix())
	return err
}

func (s *Store) GetSFTPUser(ctx context.Context, username string) (*SFTPUser, error) {
	return scanSFTPUser(s.db.QueryRowContext(ctx, `SELECT `+sftpCols+` FROM sftp_users WHERE username = ?`, username))
}

// SFTPUsers lists the SFTP logins of one site, or all when siteID is "".
func (s *Store) SFTPUsers(ctx context.Context, siteID string) ([]*SFTPUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sftpCols+` FROM sftp_users WHERE ? = '' OR site_id = ?
		ORDER BY username`, siteID, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SFTPUser{}
	for rows.Next() {
		u, err := scanSFTPUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) SetSFTPCredentials(ctx context.Context, username, password string, keys []string) error {
	return s.exec1(ctx, `UPDATE sftp_users SET password = ?, public_keys = ? WHERE username = ?`,
		password, strings.Join(keys, "\n"), username)
}

func (s *Store) DeleteSFTPUser(ctx context.Context, username string) error {
	return s.exec1(ctx, `DELETE FROM sftp_users WHERE username = ?`, username)
}

// SiteCert describes a certificate uploaded for a site (the PEM files are
// in Caddy's config directory).
type SiteCert struct {
	SiteID    string    `json:"site_id"`
	Names     []string  `json:"names"`
	Issuer    string    `json:"issuer"`
	NotAfter  time.Time `json:"not_after"`
	Trusted   bool      `json:"trusted"` // chains to a public root
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) SetSiteCert(ctx context.Context, c *SiteCert) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_certs (site_id, names, issuer, not_after, trusted, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (site_id) DO UPDATE SET names = excluded.names, issuer = excluded.issuer,
		not_after = excluded.not_after, trusted = excluded.trusted, created_at = excluded.created_at`,
		c.SiteID, strings.Join(c.Names, ","), c.Issuer, c.NotAfter.Unix(), c.Trusted, time.Now().Unix())
	return err
}

func (s *Store) SiteCert(ctx context.Context, siteID string) (*SiteCert, error) {
	var c SiteCert
	var names string
	var notAfter, created int64
	err := s.db.QueryRowContext(ctx, `SELECT site_id, names, issuer, not_after, trusted, created_at FROM site_certs
		WHERE site_id = ?`, siteID).Scan(&c.SiteID, &names, &c.Issuer, &notAfter, &c.Trusted, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Names = splitList(names)
	c.NotAfter, c.CreatedAt = time.Unix(notAfter, 0).UTC(), time.Unix(created, 0).UTC()
	return &c, nil
}

// SiteCerts maps site IDs to their uploaded certificates.
func (s *Store) SiteCerts(ctx context.Context) (map[string]*SiteCert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT site_id FROM site_certs`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]*SiteCert{}
	for _, id := range ids {
		c, err := s.SiteCert(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, nil
}

func (s *Store) DeleteSiteCert(ctx context.Context, siteID string) error {
	return s.exec1(ctx, `DELETE FROM site_certs WHERE site_id = ?`, siteID)
}
