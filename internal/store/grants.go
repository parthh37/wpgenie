package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Site grants: a site's owners share it with users of other accounts, at
// an access level (auth.AccessViewer, ...). A grant lives as long as the
// site keeps its owner: deleting the site, making it staff-only or giving
// it to another account removes its grants (deleteSiteOwnership,
// AssignSite), and deleting the user removes theirs (ON DELETE CASCADE).
const grantsSchema = `CREATE TABLE site_grants (
		site_id    TEXT NOT NULL,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		access     TEXT NOT NULL,
		granted_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		PRIMARY KEY (site_id, user_id)
	);
	CREATE INDEX site_grants_by_user ON site_grants (user_id);`

// SiteGrant is a site shared with a user.
type SiteGrant struct {
	SiteID    string    `json:"site_id"`
	UserID    int64     `json:"user_id"`
	Username  string    `json:"username"`
	Access    string    `json:"access"`
	GrantedBy string    `json:"granted_by"`
	CreatedAt time.Time `json:"created_at"`
	// AccountID is the account owning the site (UserSiteGrants only).
	AccountID int64 `json:"-"`
}

const grantCols = `g.site_id, g.user_id, u.username, g.access, g.granted_by, g.created_at`

func scanGrant(row interface{ Scan(...any) error }, extra ...any) (*SiteGrant, error) {
	var g SiteGrant
	var created int64
	err := row.Scan(append([]any{&g.SiteID, &g.UserID, &g.Username, &g.Access, &g.GrantedBy, &created}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.CreatedAt = time.Unix(created, 0).UTC()
	return &g, nil
}

// CreateSiteGrant shares a site with a user: ErrExists if it already is,
// ErrConflict if the site has max grants already.
func (s *Store) CreateSiteGrant(ctx context.Context, g *SiteGrant, max int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Others' grants: sharing again with someone is ErrExists, full or not.
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM site_grants WHERE site_id = ? AND user_id <> ?`,
		g.SiteID, g.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= max {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO site_grants (site_id, user_id, access, granted_by, created_at)
		VALUES (?, ?, ?, ?, ?)`, g.SiteID, g.UserID, g.Access, g.GrantedBy, time.Now().Unix())
	if isUnique(err) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SiteGrant returns what a site's grant to a user is, or ErrNotFound.
func (s *Store) SiteGrant(ctx context.Context, siteID string, userID int64) (*SiteGrant, error) {
	return scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantCols+` FROM site_grants g JOIN users u ON u.id = g.user_id
		WHERE g.site_id = ? AND g.user_id = ?`, siteID, userID))
}

// SiteGrants lists who a site is shared with.
func (s *Store) SiteGrants(ctx context.Context, siteID string) ([]*SiteGrant, error) {
	return s.grants(ctx, `SELECT `+grantCols+` FROM site_grants g JOIN users u ON u.id = g.user_id
		WHERE g.site_id = ? ORDER BY u.username`, false, siteID)
}

// UserSiteGrants lists the owned sites shared with a user, with their
// owning accounts. (A grant on a site without an owner is never used.)
func (s *Store) UserSiteGrants(ctx context.Context, userID int64) ([]*SiteGrant, error) {
	return s.grants(ctx, `SELECT `+grantCols+`, a.account_id FROM site_grants g JOIN users u ON u.id = g.user_id
		JOIN site_accounts a ON a.site_id = g.site_id WHERE g.user_id = ? ORDER BY g.site_id`, true, userID)
}

func (s *Store) grants(ctx context.Context, q string, withAccount bool, args ...any) ([]*SiteGrant, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SiteGrant{}
	for rows.Next() {
		var acct int64
		var extra []any
		if withAccount {
			extra = []any{&acct}
		}
		g, err := scanGrant(rows, extra...)
		if err != nil {
			return nil, err
		}
		g.AccountID = acct
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetSiteGrantAccess changes a grant's access level.
func (s *Store) SetSiteGrantAccess(ctx context.Context, siteID string, userID int64, access string) error {
	return s.exec1(ctx, `UPDATE site_grants SET access = ? WHERE site_id = ? AND user_id = ?`, access, siteID, userID)
}

// DeleteSiteGrant stops sharing a site with a user.
func (s *Store) DeleteSiteGrant(ctx context.Context, siteID string, userID int64) error {
	return s.exec1(ctx, `DELETE FROM site_grants WHERE site_id = ? AND user_id = ?`, siteID, userID)
}
