package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Tenant accounts, their plans and which sites they own. The rules (who may
// do what, quotas, suspension) live in internal/billing and internal/api;
// this file only stores them.

var (
	// ErrInUse means a record can't be deleted while others refer to it.
	ErrInUse = errors.New("in use")
	// ErrDuplicateWHMCS: another account of the same billing owner has
	// that WHMCS service ID.
	ErrDuplicateWHMCS = errors.New("another account already has that WHMCS service ID")
)

// isDuplicateWHMCS tells a clash on the WHMCS service ID from others.
func isDuplicateWHMCS(err error) bool {
	return isUnique(err) && strings.Contains(err.Error(), "whmcs_service_id")
}

// Plan is an admin-defined set of limits. Zero limits mean unlimited (the
// server's own limits still apply to resources).
type Plan struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Totals per account: sites (staging copies included), disk (site
	// files + databases) and bandwidth per UTC calendar month.
	MaxSites    int   `json:"max_sites"`
	DiskMB      int64 `json:"disk_mb"`
	BandwidthGB int64 `json:"bandwidth_gb"`
	// Per site: replicas, memory and CPUs per replica, domains.
	MaxReplicas int     `json:"max_replicas"`
	MaxMemoryMB int     `json:"max_memory_mb"`
	MaxCPUs     float64 `json:"max_cpus"`
	MaxDomains  int     `json:"max_domains"`
	// BurstMinutes are the burst minutes included per account per UTC
	// calendar month (0: unlimited; the "burst" feature allows burst at all).
	BurstMinutes int64 `json:"burst_minutes"`
	// Features are optional capabilities (see billing.Features); a tenant
	// only gets those listed.
	Features []string `json:"features"`
	// BackupRepos are the backup destinations tenants may choose.
	BackupRepos []string `json:"backup_repos"`
	// Overage is what happens past 100% of the bandwidth: notify or suspend.
	Overage string `json:"overage"`
	// Resellable plans may be assigned by resellers to their customers.
	Resellable bool `json:"resellable"`
	// The built-in store (see invoicing.go): what the order form shows and
	// what an order creates, the price per billing cycle ("monthly": …; a
	// plan without prices is free and not orderable), and the price per
	// started GB of bandwidth beyond the plan (0: none), in minor units.
	Description    string               `json:"description"`
	Public         bool                 `json:"public"`
	Sort           int                  `json:"sort"`
	AccountKind    string               `json:"account_kind"`
	Prices         map[string]PlanPrice `json:"prices"`
	OverageGBPrice int64                `json:"overage_gb_price"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

// PlanPrice is a plan's price for one billing cycle, in minor units.
type PlanPrice struct {
	Price    int64 `json:"price"`
	SetupFee int64 `json:"setup_fee"`
}

const planCols = `id, name, max_sites, disk_mb, bandwidth_gb, max_replicas, max_memory_mb, max_cpus, max_domains,
	features, backup_repos, overage, resellable, burst_minutes, description, is_public, sort_order, account_kind, prices,
	overage_gb_price, created_at, updated_at`

func scanPlan(row interface{ Scan(...any) error }) (*Plan, error) {
	var p Plan
	var features, repos, prices string
	var created, updated int64
	err := row.Scan(&p.ID, &p.Name, &p.MaxSites, &p.DiskMB, &p.BandwidthGB, &p.MaxReplicas, &p.MaxMemoryMB,
		&p.MaxCPUs, &p.MaxDomains, &features, &repos, &p.Overage, &p.Resellable, &p.BurstMinutes, &p.Description,
		&p.Public, &p.Sort, &p.AccountKind, &prices, &p.OverageGBPrice, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Features, p.BackupRepos = splitList(features), splitList(repos)
	p.Prices = map[string]PlanPrice{}
	if prices != "" {
		if err := json.Unmarshal([]byte(prices), &p.Prices); err != nil {
			return nil, err
		}
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return &p, nil
}

// planStore is what a plan's store columns hold that the struct doesn't
// have as is: its kind (customer unless set) and prices (JSON).
func planStore(p *Plan) (kind, prices string, err error) {
	kind = p.AccountKind
	if kind == "" {
		kind = AccountCustomer
	}
	if p.Prices == nil {
		return kind, "{}", nil
	}
	b, err := json.Marshal(p.Prices)
	return kind, string(b), err
}

func (s *Store) CreatePlan(ctx context.Context, p *Plan) error {
	kind, prices, err := planStore(p)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `INSERT INTO plans (`+planCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.MaxSites, p.DiskMB, p.BandwidthGB, p.MaxReplicas, p.MaxMemoryMB, p.MaxCPUs, p.MaxDomains,
		strings.Join(p.Features, ","), strings.Join(p.BackupRepos, ","), p.Overage, p.Resellable, p.BurstMinutes,
		p.Description, p.Public, p.Sort, kind, prices, p.OverageGBPrice, now, now)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) UpdatePlan(ctx context.Context, p *Plan) error {
	kind, prices, err := planStore(p)
	if err != nil {
		return err
	}
	return s.exec1(ctx, `UPDATE plans SET name = ?, max_sites = ?, disk_mb = ?, bandwidth_gb = ?, max_replicas = ?,
		max_memory_mb = ?, max_cpus = ?, max_domains = ?, features = ?, backup_repos = ?, overage = ?, resellable = ?,
		burst_minutes = ?, description = ?, is_public = ?, sort_order = ?, account_kind = ?, prices = ?,
		overage_gb_price = ?, updated_at = ? WHERE id = ?`, p.Name, p.MaxSites, p.DiskMB, p.BandwidthGB, p.MaxReplicas,
		p.MaxMemoryMB, p.MaxCPUs, p.MaxDomains, strings.Join(p.Features, ","), strings.Join(p.BackupRepos, ","), p.Overage,
		p.Resellable, p.BurstMinutes, p.Description, p.Public, p.Sort, kind, prices, p.OverageGBPrice, time.Now().Unix(), p.ID)
}

func (s *Store) GetPlan(ctx context.Context, id string) (*Plan, error) {
	return scanPlan(s.db.QueryRowContext(ctx, `SELECT `+planCols+` FROM plans WHERE id = ?`, id))
}

func (s *Store) ListPlans(ctx context.Context) ([]*Plan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+planCols+` FROM plans ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePlan removes a plan no account uses (ErrInUse otherwise).
func (s *Store) DeletePlan(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE plan_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrInUse
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM plans WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// Account kinds and statuses.
const (
	AccountCustomer = "customer"
	AccountReseller = "reseller"

	AccountActive     = "active"
	AccountSuspended  = "suspended"
	AccountTerminated = "terminated"
	// AccountPending: ordered from the store, awaiting its first payment
	// (or an administrator's approval). Treated as suspended (no sites),
	// but its users can sign in, pay and ask for help.
	AccountPending = "pending"
)

// Account is a tenant: an organisation whose users manage its sites. A
// customer account may belong to a reseller (ParentID); resellers are
// top-level.
type Account struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// SuspendReason is who suspended it: admin, billing, overage or
	// reseller (only that party, or an administrator, lifts it).
	SuspendReason        string `json:"suspend_reason,omitempty"`
	PlanID               string `json:"plan_id"`
	ParentID             int64  `json:"parent_id,omitempty"`
	Email                string `json:"email,omitempty"`
	WHMCSServiceID       string `json:"whmcs_service_id,omitempty"`
	StripeCustomerID     string `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string `json:"stripe_subscription_id,omitempty"`
	// BurstCredit is burst minutes bought on top of the plan's monthly
	// ones; they never expire and are used once the month's are gone.
	BurstCredit int64     `json:"burst_credit"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	SuspendedAt time.Time `json:"suspended_at,omitzero"`
}

const accountCols = `id, name, kind, status, suspend_reason, plan_id, parent_id, email, whmcs_service_id,
	stripe_customer_id, stripe_subscription_id, burst_credit, created_at, updated_at, suspended_at`

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var created, updated, suspended int64
	err := row.Scan(&a.ID, &a.Name, &a.Kind, &a.Status, &a.SuspendReason, &a.PlanID, &a.ParentID, &a.Email,
		&a.WHMCSServiceID, &a.StripeCustomerID, &a.StripeSubscriptionID, &a.BurstCredit, &created, &updated, &suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, a.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	if suspended > 0 {
		a.SuspendedAt = time.Unix(suspended, 0).UTC()
	}
	return &a, nil
}

// NewUser is a user created together with an account.
type NewUser struct {
	Username     string
	PasswordHash string
	Role         string
}

// CreateAccount inserts an account and, optionally, its first user, in one
// transaction: a taken username leaves no account behind. With an
// idempotency key, a key seen before returns that account (existed true)
// and creates nothing: a billing system retrying a timed-out request
// can't create a second account.
func (s *Store) CreateAccount(ctx context.Context, a *Account, idemKey string, first *NewUser) (acct *Account, user *User, existed bool, err error) {
	if idemKey != "" {
		if got, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts
			WHERE idempotency_key = ?`, idemKey)); err == nil {
			return got, nil, true, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, nil, false, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var key any // NULL: UNIQUE allows any number of them
	if idemKey != "" {
		key = idemKey
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO accounts (name, kind, status, suspend_reason, plan_id, parent_id, email,
		whmcs_service_id, stripe_customer_id, stripe_subscription_id, idempotency_key, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, a.Name, a.Kind, AccountActive, a.PlanID, a.ParentID,
		a.Email, a.WHMCSServiceID, a.StripeCustomerID, a.StripeSubscriptionID, key, now, now).Scan(&id)
	if isUnique(err) && idemKey != "" && !isDuplicateWHMCS(err) {
		// A concurrent request with the same key won the race.
		tx.Rollback()
		got, gerr := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts
			WHERE idempotency_key = ?`, idemKey))
		return got, nil, gerr == nil, gerr
	}
	if isDuplicateWHMCS(err) {
		return nil, nil, false, ErrDuplicateWHMCS
	}
	if err != nil {
		return nil, nil, false, err
	}
	var uid int64
	if first != nil {
		err := tx.QueryRowContext(ctx, `INSERT INTO users (username, password, role, account_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, first.Username, first.PasswordHash, first.Role, id, now, now).Scan(&uid)
		if isUnique(err) {
			return nil, nil, false, ErrExists
		}
		if err != nil {
			return nil, nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, false, err
	}
	if acct, err = s.GetAccount(ctx, id); err != nil {
		return nil, nil, false, err
	}
	if uid != 0 {
		if user, err = s.GetUser(ctx, uid); err != nil {
			return nil, nil, false, err
		}
	}
	return acct, user, false, nil
}

func (s *Store) GetAccount(ctx context.Context, id int64) (*Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
}

// AccountByStripeCustomer finds the account billed to a Stripe customer.
func (s *Store) AccountByStripeCustomer(ctx context.Context, customer string) (*Account, error) {
	if customer == "" {
		return nil, ErrNotFound
	}
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE stripe_customer_id = ?
		ORDER BY id LIMIT 1`, customer))
}

// AccountFilter narrows ListAccounts; zero values don't filter. Scope, when
// not 0, keeps that account and its children (a reseller's view); TopLevel
// keeps accounts without a reseller.
type AccountFilter struct {
	Scope            int64
	ParentID         int64
	TopLevel         bool
	WHMCSServiceID   string
	StripeCustomerID string
}

func (s *Store) ListAccounts(ctx context.Context, f AccountFilter) ([]*Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts
		WHERE (? = 0 OR id = ? OR parent_id = ?) AND (? = 0 OR parent_id = ?) AND (? = 0 OR parent_id = 0)
		AND (? = '' OR whmcs_service_id = ?) AND (? = '' OR stripe_customer_id = ?) ORDER BY id`,
		f.Scope, f.Scope, f.Scope, f.ParentID, f.ParentID, f.TopLevel, f.WHMCSServiceID, f.WHMCSServiceID,
		f.StripeCustomerID, f.StripeCustomerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAccount stores an account's editable fields (not its status).
func (s *Store) UpdateAccount(ctx context.Context, a *Account) error {
	err := s.exec1(ctx, `UPDATE accounts SET name = ?, kind = ?, plan_id = ?, parent_id = ?, email = ?,
		whmcs_service_id = ?, stripe_customer_id = ?, stripe_subscription_id = ?, updated_at = ? WHERE id = ?`,
		a.Name, a.Kind, a.PlanID, a.ParentID, a.Email, a.WHMCSServiceID, a.StripeCustomerID, a.StripeSubscriptionID,
		time.Now().Unix(), a.ID)
	if isDuplicateWHMCS(err) {
		return ErrDuplicateWHMCS
	}
	return err
}

// SetAccountStatus records a status change; reason is kept only while
// suspended or terminated.
func (s *Store) SetAccountStatus(ctx context.Context, id int64, status, reason string, at time.Time) error {
	suspended := at.Unix()
	if status == AccountActive {
		reason, suspended = "", 0
	}
	return s.exec1(ctx, `UPDATE accounts SET status = ?, suspend_reason = ?, suspended_at = ?, updated_at = ? WHERE id = ?`,
		status, reason, suspended, time.Now().Unix(), id)
}

// DeleteAccount removes an account with no sites and no child accounts,
// with its users (their sessions and tokens cascade), events and usage.
func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM site_accounts WHERE account_id = ?) +
		(SELECT COUNT(*) FROM accounts WHERE parent_id = ?)`, id, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrInUse
	}
	for _, q := range []string{`DELETE FROM users WHERE account_id = ?`, `DELETE FROM account_events WHERE account_id = ?`,
		`DELETE FROM account_usage WHERE account_id = ?`} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	if err := deleteAccountTickets(ctx, tx, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// CreateAccountUser adds a user to an account.
func (s *Store) CreateAccountUser(ctx context.Context, accountID int64, username, passwordHash, role string) (*User, error) {
	now := time.Now().Unix()
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO users (username, password, role, account_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, username, passwordHash, role, accountID, now, now).Scan(&id)
	if isUnique(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, id)
}

// SetAccountUsersRole records the role of every user of an account (when
// its kind changes; authentication derives it from the account anyway).
func (s *Store) SetAccountUsersRole(ctx context.Context, accountID int64, role string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role = ?, updated_at = ? WHERE account_id = ?`,
		role, time.Now().Unix(), accountID)
	return err
}

// AccountUsers lists the users of an account.
func (s *Store) AccountUsers(ctx context.Context, accountID int64) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE account_id = ? ORDER BY username`, accountID)
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

// ---- Site ownership ----

// SiteOwner is one row of site ownership. Suspended records that billing
// suspended the site (so unsuspending the account knows what to bring
// back), independent of where the site runs.
type SiteOwner struct {
	SiteID    string `json:"site_id"`
	AccountID int64  `json:"account_id"`
	Suspended bool   `json:"suspended"`
}

// AssignSite makes an account own a site (moving it if another did). A
// site moving to another account stops being shared: its new owners
// decide who else reaches it.
func (s *Store) AssignSite(ctx context.Context, siteID string, accountID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev int64
	err = tx.QueryRowContext(ctx, `SELECT account_id FROM site_accounts WHERE site_id = ?`, siteID).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO site_accounts (site_id, account_id, created_at) VALUES (?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET account_id = excluded.account_id`, siteID, accountID, time.Now().Unix()); err != nil {
		return err
	}
	if prev != accountID {
		if _, err := tx.ExecContext(ctx, `DELETE FROM site_grants WHERE site_id = ?`, siteID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnassignSite forgets a site's owner (and its measured usage and grants):
// for sites removed some other way than DeleteSite, e.g. on another node.
func (s *Store) UnassignSite(ctx context.Context, siteID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteSiteOwnership(ctx, tx, siteID); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteSiteOwnership(ctx context.Context, tx *Tx, siteID string) error {
	for _, q := range []string{`DELETE FROM site_accounts WHERE site_id = ?`, `DELETE FROM site_usage WHERE site_id = ?`,
		`DELETE FROM site_grants WHERE site_id = ?`} {
		if _, err := tx.ExecContext(ctx, q, siteID); err != nil {
			return err
		}
	}
	return nil
}

// SiteOwnerOf returns who owns a site, or ErrNotFound (staff-only site).
func (s *Store) SiteOwnerOf(ctx context.Context, siteID string) (*SiteOwner, error) {
	o := SiteOwner{SiteID: siteID}
	err := s.db.QueryRowContext(ctx, `SELECT account_id, suspended FROM site_accounts WHERE site_id = ?`, siteID).
		Scan(&o.AccountID, &o.Suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// SiteOwners lists the sites owned by the given accounts (none: nothing).
func (s *Store) SiteOwners(ctx context.Context, accountIDs ...int64) ([]SiteOwner, error) {
	if len(accountIDs) == 0 {
		return []SiteOwner{}, nil
	}
	args := make([]any, len(accountIDs))
	for i, id := range accountIDs {
		args[i] = id
	}
	return s.siteOwners(ctx, ` WHERE account_id IN (`+placeholders(len(accountIDs))+`)`, args...)
}

// AllSiteOwners lists every owned site.
func (s *Store) AllSiteOwners(ctx context.Context) ([]SiteOwner, error) {
	return s.siteOwners(ctx, "")
}

func (s *Store) siteOwners(ctx context.Context, where string, args ...any) ([]SiteOwner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, account_id, suspended FROM site_accounts`+where+
		` ORDER BY site_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SiteOwner{}
	for rows.Next() {
		var o SiteOwner
		if err := rows.Scan(&o.SiteID, &o.AccountID, &o.Suspended); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetSiteSuspended records whether billing suspended an owned site.
func (s *Store) SetSiteSuspended(ctx context.Context, siteID string, suspended bool) error {
	return s.exec1(ctx, `UPDATE site_accounts SET suspended = ? WHERE site_id = ?`, suspended, siteID)
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// ---- Account activity log ----

// AccountEvent is a line in an account's activity log: status and plan
// changes, usage notifications, billing events.
type AccountEvent struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

const keepAccountEvents = 500

func (s *Store) AddAccountEvent(ctx context.Context, accountID int64, kind, msg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_events (account_id, at, kind, message) VALUES (?, ?, ?, ?)`,
		accountID, time.Now().Unix(), kind, msg); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_events WHERE account_id = ? AND id <=
		(SELECT id FROM account_events WHERE account_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		accountID, accountID, keepAccountEvents); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AccountEvents(ctx context.Context, accountID int64, limit int) ([]AccountEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, kind, message FROM account_events WHERE account_id = ?
		ORDER BY id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountEvent{}
	for rows.Next() {
		var e AccountEvent
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.Time = time.Unix(t, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Job owners ----

// SetJobOwner records the stable identity (jobs.WithOwner) that started a
// job, so a tenant can follow a job whose site is gone (a failed create).
func (s *Store) SetJobOwner(ctx context.Context, id int64, owner string) error {
	return s.exec1(ctx, `UPDATE jobs SET owner = ? WHERE id = ?`, owner, id)
}

// JobOwner returns who started a job ("" when unknown).
func (s *Store) JobOwner(ctx context.Context, id int64) (string, error) {
	var o string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM jobs WHERE id = ?`, id).Scan(&o)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return o, err
}

// VisibleJobs lists the newest jobs of the given sites or started by owner
// (a tenant's view), only unfinished ones when active.
func (s *Store) VisibleJobs(ctx context.Context, siteIDs []string, owner string, active bool, limit int) ([]Job, error) {
	cond := `owner = ?`
	args := []any{owner}
	if owner == "" {
		cond = `1 = 0`
		args = nil
	}
	if len(siteIDs) > 0 {
		cond += ` OR site_id IN (` + placeholders(len(siteIDs)) + `)`
		for _, id := range siteIDs {
			args = append(args, id)
		}
	}
	q := `SELECT ` + jobCols + ` FROM jobs WHERE (` + cond + `)`
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
