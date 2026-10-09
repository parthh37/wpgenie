package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ---- OAuth clients (AI assistants connecting over MCP) ----
//
// Claude, ChatGPT and other MCP clients register themselves (dynamic client
// registration) and then ask a signed-in user for access. What they get is
// an ordinary API token of that user (client_id set), renewable with a
// refresh token: everything that limits or revokes API tokens (roles,
// accounts, two-factor, password resets) applies to them unchanged.

const oauthSchema = `
	CREATE TABLE oauth_clients (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		redirect_uris TEXT NOT NULL,
		secret_hash   TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		last_used_at  INTEGER NOT NULL DEFAULT 0
	);
	ALTER TABLE api_tokens ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE api_tokens ADD COLUMN refresh_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE api_tokens ADD COLUMN refresh_expires_at INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX api_tokens_by_refresh ON api_tokens (refresh_hash);`

// OAuthClient is a registered OAuth client. Only the SHA-256 of a
// confidential client's secret is kept.
type OAuthClient struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	SecretHash   string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	LastUsedAt   time.Time `json:"last_used_at,omitzero"`
}

func (s *Store) CreateOAuthClient(ctx context.Context, c *OAuthClient) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris, secret_hash, created_at)
		VALUES (?, ?, ?, ?, ?)`, c.ID, c.Name, strings.Join(c.RedirectURIs, "\n"), c.SecretHash, c.CreatedAt.Unix())
	return err
}

func (s *Store) OAuthClient(ctx context.Context, id string) (*OAuthClient, error) {
	var c OAuthClient
	var uris string
	var created, used int64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, redirect_uris, secret_hash, created_at, last_used_at
		FROM oauth_clients WHERE id = ?`, id).Scan(&c.ID, &c.Name, &uris, &c.SecretHash, &created, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.RedirectURIs = strings.Split(uris, "\n")
	c.CreatedAt = time.Unix(created, 0).UTC()
	if used > 0 {
		c.LastUsedAt = time.Unix(used, 0).UTC()
	}
	return &c, nil
}

func (s *Store) TouchOAuthClient(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oauth_clients SET last_used_at = ? WHERE id = ?`, at.Unix(), id)
	return err
}

// CountOAuthClients is how many clients are registered.
func (s *Store) CountOAuthClients(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&n)
	return n, err
}

// PruneOAuthClients forgets clients registered or last used before cutoff
// that hold no tokens: registration is open to anyone, so abandoned ones
// must not pile up.
func (s *Store) PruneOAuthClients(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_clients
		WHERE created_at < ? AND last_used_at < ?
		AND NOT EXISTS (SELECT 1 FROM api_tokens t WHERE t.client_id = oauth_clients.id)`, cutoff.Unix(), cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CreateOAuthToken stores the API token an OAuth client was granted with
// its refresh token (hashes). Tokens the same user granted the same client
// before go: connecting an assistant again replaces its access.
func (s *Store) CreateOAuthToken(ctx context.Context, t *APIToken, tokenHash, refreshHash string) (*APIToken, error) {
	if t.ClientID == "" {
		return nil, errors.New("store: an OAuth token needs its client")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ? AND client_id = ?`,
		t.UserID, t.ClientID); err != nil {
		return nil, err
	}
	return s.createAPIToken(ctx, t, tokenHash, refreshHash)
}

// APITokenByRefresh finds an OAuth token by its refresh token's hash.
func (s *Store) APITokenByRefresh(ctx context.Context, refreshHash string) (*APIToken, error) {
	if refreshHash == "" {
		return nil, ErrNotFound
	}
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.refresh_hash = ?`, refreshHash))
}

// RotateOAuthToken renews an OAuth token: new access and refresh tokens
// replace the old ones, only if oldRefreshHash is still current (a refresh
// token works once, even with two requests racing).
func (s *Store) RotateOAuthToken(ctx context.Context, id int64, oldRefreshHash, tokenHash, refreshHash string,
	expires, refreshExpires time.Time) error {
	return s.exec1(ctx, `UPDATE api_tokens SET token_hash = ?, refresh_hash = ?, expires_at = ?, refresh_expires_at = ?
		WHERE id = ? AND refresh_hash = ? AND refresh_hash != ''`,
		tokenHash, refreshHash, unixOrZero(expires), unixOrZero(refreshExpires), id, oldRefreshHash)
}

// DeleteAPITokenByHash revokes the token with this access or refresh
// token hash (OAuth revocation), if it belongs to clientID.
func (s *Store) DeleteAPITokenByHash(ctx context.Context, hash, clientID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE client_id = ? AND client_id != ''
		AND (token_hash = ? OR refresh_hash = ?)`, clientID, hash, hash)
	return err
}
