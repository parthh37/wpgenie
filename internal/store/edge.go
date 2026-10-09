package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Rules at a site's edge (Caddy): the site lock (a username and password
// for every visitor) and redirects from paths of the site. The lock is part
// of the site's record (Site.Lock...); redirects are a list of their own,
// read only by what renders or edits them (internal/site/edge.go).
const edgeSchema = `ALTER TABLE sites ADD COLUMN lock_on INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN lock_user TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN lock_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN lock_allow TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN redirect_rules TEXT NOT NULL DEFAULT '[]';`

// Redirect sends visitors from a path of a site elsewhere: From is an exact
// path ("/old-page") or a prefix ("/old/*"); To an https URL or a path of
// the site; Code 301, 302, 307 or 308; KeepQuery passes the visitor's query
// string on. internal/site checks them before they are stored.
type Redirect struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Code      int    `json:"code"`
	KeepQuery bool   `json:"keep_query"`
}

// SetSiteLock records a site's lock: on or off, and its username, password
// hash and allowed networks (kept while it is off).
func (s *Store) SetSiteLock(ctx context.Context, id string, on bool, user, hash string, allow []string) error {
	return s.exec1(ctx, `UPDATE sites SET lock_on = ?, lock_user = ?, lock_hash = ?, lock_allow = ?, updated_at = ? WHERE id = ?`,
		on, user, hash, strings.Join(allow, ","), time.Now().Unix(), id)
}

// SiteRedirects returns a site's redirects, in the order they were made.
func (s *Store) SiteRedirects(ctx context.Context, id string) ([]Redirect, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT redirect_rules FROM sites WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return parseRedirects(raw)
}

// SetSiteRedirects replaces a site's redirects.
func (s *Store) SetSiteRedirects(ctx context.Context, id string, rules []Redirect) error {
	if rules == nil {
		rules = []Redirect{}
	}
	b, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	return s.exec1(ctx, `UPDATE sites SET redirect_rules = ?, updated_at = ? WHERE id = ?`, string(b), time.Now().Unix(), id)
}

// AllSiteRedirects maps the sites that have redirects to them.
func (s *Store) AllSiteRedirects(ctx context.Context) (map[string][]Redirect, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, redirect_rules FROM sites WHERE redirect_rules <> '[]'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Redirect{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		rules, err := parseRedirects(raw)
		if err != nil {
			return nil, err
		}
		if len(rules) > 0 {
			out[id] = rules
		}
	}
	return out, rows.Err()
}

func parseRedirects(raw string) ([]Redirect, error) {
	out := []Redirect{}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}
