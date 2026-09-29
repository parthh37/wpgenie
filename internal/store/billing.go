package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ---- Per-user API tokens ----

// APIToken is a user's named API credential. Only the SHA-256 of the token
// is stored; Hint is its first characters, to tell tokens apart.
type APIToken struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	Username   string    `json:"username"`
	Name       string    `json:"name"`
	Hint       string    `json:"hint"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	LastUsedIP string    `json:"last_used_ip,omitempty"`
}

const tokenCols = `t.id, t.user_id, u.username, t.name, t.hint, t.created_at, t.expires_at, t.last_used_at, t.last_used_ip`

func scanToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var created, expires, used int64
	err := row.Scan(&t.ID, &t.UserID, &t.Username, &t.Name, &t.Hint, &created, &expires, &used, &t.LastUsedIP)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	if expires > 0 {
		t.ExpiresAt = time.Unix(expires, 0).UTC()
	}
	if used > 0 {
		t.LastUsedAt = time.Unix(used, 0).UTC()
	}
	return &t, nil
}

func (s *Store) CreateAPIToken(ctx context.Context, t *APIToken, tokenHash string) (*APIToken, error) {
	var expires int64
	if !t.ExpiresAt.IsZero() {
		expires = t.ExpiresAt.Unix()
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO api_tokens (user_id, name, token_hash, hint, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, t.UserID, t.Name, tokenHash, t.Hint, time.Now().Unix(), expires).Scan(&id)
	if err != nil {
		return nil, err
	}
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.id = ?`, id))
}

// APITokenByHash finds a token by the hash of what a client presented.
func (s *Store) APITokenByHash(ctx context.Context, tokenHash string) (*APIToken, error) {
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?`, tokenHash))
}

// APITokens lists one user's tokens, or everyone's (userID 0).
func (s *Store) APITokens(ctx context.Context, userID int64) ([]*APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE ? = 0 OR t.user_id = ? ORDER BY t.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token; with userID != 0, only one of that
// user's.
func (s *Store) DeleteAPIToken(ctx context.Context, id, userID int64) error {
	return s.exec1(ctx, `DELETE FROM api_tokens WHERE id = ? AND (? = 0 OR user_id = ?)`, id, userID, userID)
}

// DeleteUserAPITokens revokes every token of a user.
func (s *Store) DeleteUserAPITokens(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID)
	return err
}

func (s *Store) TouchAPIToken(ctx context.Context, id int64, at time.Time, ip string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, at.Unix(), ip, id)
	return err
}

// ---- One-time sign-on tokens ----

func (s *Store) CreateSSOToken(ctx context.Context, tokenHash string, userID int64, expires time.Time) error {
	now := time.Now()
	// Expired ones are useless: drop them on the way.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sso_tokens WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sso_tokens (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, userID, now.Unix(), expires.Unix())
	return err
}

// SSOTokenUser returns whom a valid sign-on token is for, without using it.
func (s *Store) SSOTokenUser(ctx context.Context, tokenHash string, now time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM sso_tokens WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ConsumeSSOToken uses a sign-on token up, atomically: of two requests
// with the same token, only one gets the user.
func (s *Store) ConsumeSSOToken(ctx context.Context, tokenHash string, now time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `DELETE FROM sso_tokens WHERE token_hash = ? AND expires_at > ? RETURNING user_id`,
		tokenHash, now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ---- Usage ----

// SiteUsage is a site's last disk measurement.
type SiteUsage struct {
	SiteID     string    `json:"site_id"`
	FilesBytes int64     `json:"files_bytes"`
	DBBytes    int64     `json:"db_bytes"`
	MeasuredAt time.Time `json:"measured_at"`
}

func (s *Store) SetSiteUsage(ctx context.Context, u SiteUsage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_usage (site_id, files_bytes, db_bytes, measured_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET files_bytes = excluded.files_bytes, db_bytes = excluded.db_bytes,
		measured_at = excluded.measured_at`, u.SiteID, u.FilesBytes, u.DBBytes, u.MeasuredAt.Unix())
	return err
}

// SiteUsages returns the last measurements of the given sites (those never
// measured are missing).
func (s *Store) SiteUsages(ctx context.Context, siteIDs []string) (map[string]SiteUsage, error) {
	out := map[string]SiteUsage{}
	if len(siteIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(siteIDs))
	for i, id := range siteIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, files_bytes, db_bytes, measured_at FROM site_usage
		WHERE site_id IN (`+placeholders(len(siteIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u SiteUsage
		var at int64
		if err := rows.Scan(&u.SiteID, &u.FilesBytes, &u.DBBytes, &at); err != nil {
			return nil, err
		}
		u.MeasuredAt = time.Unix(at, 0).UTC()
		out[u.SiteID] = u
	}
	return out, rows.Err()
}

// Bandwidth sums the bytes served per site in [from, to), from the hourly
// traffic rollups.
func (s *Store) Bandwidth(ctx context.Context, siteIDs []string, from, to time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	if len(siteIDs) == 0 {
		return out, nil
	}
	args := []any{from.Unix(), to.Unix()}
	for _, id := range siteIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, SUM(bytes_out) FROM traffic_hourly WHERE hour >= ? AND hour < ?
		AND site_id IN (`+placeholders(len(siteIDs))+`) GROUP BY site_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// AccountUsage is an account's usage in one UTC month, and which
// thresholds (percent) have been notified.
type AccountUsage struct {
	AccountID      int64     `json:"account_id"`
	MonthStart     time.Time `json:"month_start"`
	BandwidthBytes int64     `json:"bandwidth_bytes"`
	DiskBytes      int64     `json:"disk_bytes"`
	BWNotified     int       `json:"bandwidth_notified"`
	DiskNotified   int       `json:"disk_notified"`
	ReportedMB     int64     `json:"reported_mb"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// RecordAccountUsage stores a month's totals (keeping what was notified
// and reported) and returns the row.
func (s *Store) RecordAccountUsage(ctx context.Context, accountID int64, month time.Time, bw, disk int64, at time.Time) (*AccountUsage, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO account_usage (account_id, month_start, bandwidth_bytes, disk_bytes, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (account_id, month_start) DO UPDATE SET bandwidth_bytes = excluded.bandwidth_bytes,
		disk_bytes = excluded.disk_bytes, updated_at = excluded.updated_at`, accountID, month.Unix(), bw, disk, at.Unix()); err != nil {
		return nil, err
	}
	return s.AccountUsageFor(ctx, accountID, month)
}

func (s *Store) AccountUsageFor(ctx context.Context, accountID int64, month time.Time) (*AccountUsage, error) {
	u := AccountUsage{AccountID: accountID, MonthStart: month.UTC()}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT bandwidth_bytes, disk_bytes, bw_notified, disk_notified, reported_mb, updated_at
		FROM account_usage WHERE account_id = ? AND month_start = ?`, accountID, month.Unix()).
		Scan(&u.BandwidthBytes, &u.DiskBytes, &u.BWNotified, &u.DiskNotified, &u.ReportedMB, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.UpdatedAt = time.Unix(at, 0).UTC()
	return &u, nil
}

// SetUsageNotified records the highest thresholds notified in a month.
func (s *Store) SetUsageNotified(ctx context.Context, accountID int64, month time.Time, bw, disk int) error {
	return s.exec1(ctx, `UPDATE account_usage SET bw_notified = ?, disk_notified = ? WHERE account_id = ? AND month_start = ?`,
		bw, disk, accountID, month.Unix())
}

// SetUsageReported records how much bandwidth (MB) was reported to Stripe.
func (s *Store) SetUsageReported(ctx context.Context, accountID int64, month time.Time, mb int64) error {
	return s.exec1(ctx, `UPDATE account_usage SET reported_mb = ? WHERE account_id = ? AND month_start = ?`,
		mb, accountID, month.Unix())
}

// ---- Stripe events ----

// StripeEventSeen reports whether an event was processed already.
func (s *Store) StripeEventSeen(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM stripe_events WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// StripeEventAt is the creation time (unix) of the last Stripe event
// applied to an account: older ones arriving late are ignored.
func (s *Store) StripeEventAt(ctx context.Context, accountID int64) (int64, error) {
	var t int64
	err := s.db.QueryRowContext(ctx, `SELECT stripe_event_at FROM accounts WHERE id = ?`, accountID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return t, err
}

// SetStripeEventAt records the last Stripe event applied (never moving back).
func (s *Store) SetStripeEventAt(ctx context.Context, accountID, created int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET stripe_event_at = ? WHERE id = ? AND stripe_event_at < ?`,
		created, accountID, created)
	return err
}

// RecordStripeEvent marks an event processed. Stripe retries for up to
// three days: IDs are kept for 30.
func (s *Store) RecordStripeEvent(ctx context.Context, id, kind string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM stripe_events WHERE received_at < ?`, at.Add(-30*24*time.Hour).Unix()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stripe_events (id, kind, received_at) VALUES (?, ?, ?)
		ON CONFLICT (id) DO NOTHING`, id, kind, at.Unix())
	return err
}

// ---- Outgoing webhooks ----

// WebhookEndpoint receives billing events. The secret signs deliveries and
// never leaves the store after creation (json:"-").
type WebhookEndpoint struct {
	ID        int64     `json:"id"`
	URL       string    `json:"url"`
	Secret    string    `json:"-"`
	Events    []string  `json:"events"` // empty: all
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

func scanEndpoint(row interface{ Scan(...any) error }) (*WebhookEndpoint, error) {
	var e WebhookEndpoint
	var events string
	var created int64
	err := row.Scan(&e.ID, &e.URL, &e.Secret, &events, &e.Enabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.Events = splitList(events)
	e.CreatedAt = time.Unix(created, 0).UTC()
	return &e, nil
}

func (s *Store) CreateWebhookEndpoint(ctx context.Context, e *WebhookEndpoint) (*WebhookEndpoint, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO webhook_endpoints (url, secret, events, enabled, created_at)
		VALUES (?, ?, ?, ?, ?) RETURNING id`, e.URL, e.Secret, strings.Join(e.Events, ","), e.Enabled,
		time.Now().Unix()).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetWebhookEndpoint(ctx, id)
}

func (s *Store) GetWebhookEndpoint(ctx context.Context, id int64) (*WebhookEndpoint, error) {
	return scanEndpoint(s.db.QueryRowContext(ctx, `SELECT id, url, secret, events, enabled, created_at
		FROM webhook_endpoints WHERE id = ?`, id))
}

func (s *Store) WebhookEndpoints(ctx context.Context) ([]*WebhookEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, url, secret, events, enabled, created_at FROM webhook_endpoints ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*WebhookEndpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) UpdateWebhookEndpoint(ctx context.Context, e *WebhookEndpoint) error {
	return s.exec1(ctx, `UPDATE webhook_endpoints SET url = ?, secret = ?, events = ?, enabled = ? WHERE id = ?`,
		e.URL, e.Secret, strings.Join(e.Events, ","), e.Enabled, e.ID)
}

func (s *Store) DeleteWebhookEndpoint(ctx context.Context, id int64) error {
	return s.exec1(ctx, `DELETE FROM webhook_endpoints WHERE id = ?`, id)
}

// Delivery states.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// WebhookDelivery is one event queued for one endpoint.
type WebhookDelivery struct {
	ID            int64     `json:"id"`
	EndpointID    int64     `json:"endpoint_id"`
	EventID       string    `json:"event_id"`
	Event         string    `json:"event"`
	Payload       string    `json:"payload"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastStatus    int       `json:"last_status,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	DeliveredAt   time.Time `json:"delivered_at,omitzero"`
}

const deliveryCols = `id, endpoint_id, event_id, event, payload, status, attempts, next_attempt_at, last_status,
	last_error, created_at, delivered_at`

func scanDelivery(row interface{ Scan(...any) error }) (*WebhookDelivery, error) {
	var d WebhookDelivery
	var next, created, delivered int64
	err := row.Scan(&d.ID, &d.EndpointID, &d.EventID, &d.Event, &d.Payload, &d.Status, &d.Attempts, &next,
		&d.LastStatus, &d.LastError, &created, &delivered)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.NextAttemptAt, d.CreatedAt = time.Unix(next, 0).UTC(), time.Unix(created, 0).UTC()
	if delivered > 0 {
		d.DeliveredAt = time.Unix(delivered, 0).UTC()
	}
	return &d, nil
}

// keepDeliveries bounds the delivery log; older finished ones go first.
const keepDeliveries = 2000

// EnqueueDelivery queues an event for an endpoint.
func (s *Store) EnqueueDelivery(ctx context.Context, endpointID int64, eventID, event, payload string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries (endpoint_id, event_id, event, payload, status,
		next_attempt_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, endpointID, eventID, event, payload, DeliveryPending,
		at.Unix(), at.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE status <> ? AND id <=
		(SELECT id FROM webhook_deliveries ORDER BY id DESC LIMIT 1 OFFSET ?)`, DeliveryPending, keepDeliveries); err != nil {
		return err
	}
	return tx.Commit()
}

// DueDeliveries returns pending deliveries whose next attempt is due.
func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]*WebhookDelivery, error) {
	return s.deliveries(ctx, `WHERE status = ? AND next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT ?`,
		DeliveryPending, now.Unix(), limit)
}

// WebhookDeliveries lists the newest deliveries (of one endpoint, or all).
func (s *Store) WebhookDeliveries(ctx context.Context, endpointID int64, limit int) ([]*WebhookDelivery, error) {
	return s.deliveries(ctx, `WHERE ? = 0 OR endpoint_id = ? ORDER BY id DESC LIMIT ?`, endpointID, endpointID, limit)
}

func (s *Store) deliveries(ctx context.Context, where string, args ...any) ([]*WebhookDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*WebhookDelivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordDelivery stores the outcome of an attempt.
func (s *Store) RecordDelivery(ctx context.Context, d *WebhookDelivery) error {
	var delivered int64
	if !d.DeliveredAt.IsZero() {
		delivered = d.DeliveredAt.Unix()
	}
	return s.exec1(ctx, `UPDATE webhook_deliveries SET status = ?, attempts = ?, next_attempt_at = ?, last_status = ?,
		last_error = ?, delivered_at = ? WHERE id = ?`, d.Status, d.Attempts, d.NextAttemptAt.Unix(), d.LastStatus,
		d.LastError, delivered, d.ID)
}

// RetryDelivery queues a delivery again, now.
func (s *Store) RetryDelivery(ctx context.Context, id int64, now time.Time) error {
	return s.exec1(ctx, `UPDATE webhook_deliveries SET status = ?, next_attempt_at = ? WHERE id = ?`,
		DeliveryPending, now.Unix(), id)
}
