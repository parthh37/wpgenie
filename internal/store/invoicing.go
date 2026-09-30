package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Built-in billing (internal/billing's invoicing): per-account billing
// profiles, invoices and their items, payments and refunds, a credit
// ledger, promotions, tax rules and orders from the public order form.
// Money is INTEGER minor units everywhere; nothing here computes a price
// or a tax: billing computes, the store keeps the numbers consistent.
//
// The money-moving statements (payments, credit, refunds, cancellation)
// each run in one transaction that reads the rows it changes FOR UPDATE,
// so two concurrent writers (a webhook and its retry, the automation and
// a client) serialise on PostgreSQL as they do on SQLite's single
// connection. Idempotency comes from UNIQUE dedupe keys: a gateway
// payment, a refund, a renewal of one period, a credit adjustment is only
// ever recorded once.

// invoicingSchema is this feature's migration (appended to migrations in
// store.go).
const invoicingSchema = `ALTER TABLE plans ADD COLUMN description TEXT NOT NULL DEFAULT '';
	ALTER TABLE plans ADD COLUMN is_public INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE plans ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE plans ADD COLUMN account_kind TEXT NOT NULL DEFAULT 'customer';
	ALTER TABLE plans ADD COLUMN prices TEXT NOT NULL DEFAULT '{}';
	ALTER TABLE plans ADD COLUMN overage_gb_price INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE billing_profiles (
		account_id     INTEGER NOT NULL,
		mode           TEXT NOT NULL DEFAULT '',
		cycle          TEXT NOT NULL DEFAULT 'monthly',
		price_override INTEGER,
		next_due_at    INTEGER NOT NULL DEFAULT 0,
		anchor_day     INTEGER NOT NULL DEFAULT 0,
		auto_pay       INTEGER NOT NULL DEFAULT 0,
		stripe_customer TEXT NOT NULL DEFAULT '',
		card_pm        TEXT NOT NULL DEFAULT '',
		card_brand     TEXT NOT NULL DEFAULT '',
		card_last4     TEXT NOT NULL DEFAULT '',
		card_exp_month INTEGER NOT NULL DEFAULT 0,
		card_exp_year  INTEGER NOT NULL DEFAULT 0,
		credit         INTEGER NOT NULL DEFAULT 0,
		tax_exempt     INTEGER NOT NULL DEFAULT 0,
		cancel_at      INTEGER NOT NULL DEFAULT 0,
		cancel_reason  TEXT NOT NULL DEFAULT '',
		promo_id       INTEGER NOT NULL DEFAULT 0,
		contact        TEXT NOT NULL DEFAULT '{}',
		updated_at     INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id)
	);
	CREATE TABLE credit_ledger (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id  INTEGER NOT NULL,
		amount      INTEGER NOT NULL,
		balance     INTEGER NOT NULL,
		description TEXT NOT NULL,
		invoice_id  INTEGER NOT NULL DEFAULT 0,
		at          INTEGER NOT NULL,
		by_name     TEXT NOT NULL DEFAULT '',
		ref         TEXT UNIQUE
	);
	CREATE INDEX credit_ledger_by_account ON credit_ledger (account_id, id);
	CREATE TABLE invoice_counters (
		name     TEXT NOT NULL,
		next_seq INTEGER NOT NULL,
		PRIMARY KEY (name)
	);
	CREATE TABLE invoices (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		number          TEXT UNIQUE,
		number_seq      INTEGER NOT NULL DEFAULT 0,
		number_prefix   TEXT NOT NULL DEFAULT '',
		account_id      INTEGER NOT NULL,
		account_name    TEXT NOT NULL DEFAULT '',
		kind            TEXT NOT NULL,
		status          TEXT NOT NULL,
		currency        TEXT NOT NULL,
		issued_at       INTEGER NOT NULL DEFAULT 0,
		due_at          INTEGER NOT NULL DEFAULT 0,
		paid_at         INTEGER NOT NULL DEFAULT 0,
		period_start    INTEGER NOT NULL DEFAULT 0,
		period_end      INTEGER NOT NULL DEFAULT 0,
		subtotal        INTEGER NOT NULL DEFAULT 0,
		discount        INTEGER NOT NULL DEFAULT 0,
		tax             INTEGER NOT NULL DEFAULT 0,
		total           INTEGER NOT NULL DEFAULT 0,
		credit_applied  INTEGER NOT NULL DEFAULT 0,
		amount_paid     INTEGER NOT NULL DEFAULT 0,
		amount_refunded INTEGER NOT NULL DEFAULT 0,
		tax_lines       TEXT NOT NULL DEFAULT '[]',
		billing_address TEXT NOT NULL DEFAULT '{}',
		notes           TEXT NOT NULL DEFAULT '',
		plan_id         TEXT NOT NULL DEFAULT '',
		cycle           TEXT NOT NULL DEFAULT '',
		promo_id        INTEGER NOT NULL DEFAULT 0,
		dedupe_key      TEXT UNIQUE,
		late_fee_at     INTEGER NOT NULL DEFAULT 0,
		autocharge_day  TEXT NOT NULL DEFAULT '',
		pay_attempts    INTEGER NOT NULL DEFAULT 0,
		units           INTEGER NOT NULL DEFAULT 0,
		units_granted   INTEGER NOT NULL DEFAULT 0,
		effects_done    INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	);
	CREATE INDEX invoices_by_account ON invoices (account_id, id);
	CREATE INDEX invoices_by_status ON invoices (status, due_at);
	CREATE TABLE invoice_items (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		invoice_id  INTEGER NOT NULL,
		kind        TEXT NOT NULL,
		description TEXT NOT NULL,
		quantity    INTEGER NOT NULL,
		unit_price  INTEGER NOT NULL,
		amount      INTEGER NOT NULL,
		taxable     INTEGER NOT NULL DEFAULT 1
	);
	CREATE INDEX invoice_items_by_invoice ON invoice_items (invoice_id, id);
	CREATE TABLE payments (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		invoice_id INTEGER NOT NULL,
		account_id INTEGER NOT NULL,
		gateway    TEXT NOT NULL,
		reference  TEXT NOT NULL DEFAULT '',
		amount     INTEGER NOT NULL,
		fee        INTEGER NOT NULL DEFAULT 0,
		refunded   INTEGER NOT NULL DEFAULT 0,
		note       TEXT NOT NULL DEFAULT '',
		by_name    TEXT NOT NULL DEFAULT '',
		at         INTEGER NOT NULL,
		dedupe     TEXT UNIQUE
	);
	CREATE INDEX payments_by_invoice ON payments (invoice_id, id);
	CREATE INDEX payments_by_account ON payments (account_id, id);
	CREATE INDEX payments_by_time ON payments (at);
	CREATE TABLE refunds (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		payment_id INTEGER NOT NULL,
		invoice_id INTEGER NOT NULL,
		account_id INTEGER NOT NULL,
		amount     INTEGER NOT NULL,
		to_credit  INTEGER NOT NULL DEFAULT 0,
		reference  TEXT NOT NULL DEFAULT '',
		by_name    TEXT NOT NULL DEFAULT '',
		at         INTEGER NOT NULL,
		dedupe     TEXT UNIQUE
	);
	CREATE INDEX refunds_by_payment ON refunds (payment_id, id);
	CREATE INDEX refunds_by_time ON refunds (at);
	CREATE TABLE promotions (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		code             TEXT NOT NULL UNIQUE,
		description      TEXT NOT NULL DEFAULT '',
		promo_type       TEXT NOT NULL,
		value            INTEGER NOT NULL,
		plans            TEXT NOT NULL DEFAULT '',
		cycles           TEXT NOT NULL DEFAULT '',
		recurring        INTEGER NOT NULL DEFAULT 0,
		max_uses         INTEGER NOT NULL DEFAULT 0,
		uses             INTEGER NOT NULL DEFAULT 0,
		starts_at        INTEGER NOT NULL DEFAULT 0,
		expires_at       INTEGER NOT NULL DEFAULT 0,
		new_clients_only INTEGER NOT NULL DEFAULT 0,
		enabled          INTEGER NOT NULL DEFAULT 1,
		created_at       INTEGER NOT NULL
	);
	CREATE TABLE tax_rules (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL,
		country    TEXT NOT NULL DEFAULT '',
		state      TEXT NOT NULL DEFAULT '',
		rate       INTEGER NOT NULL,
		level      INTEGER NOT NULL DEFAULT 1,
		compound   INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE orders (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL,
		invoice_id INTEGER NOT NULL DEFAULT 0,
		plan_id    TEXT NOT NULL,
		cycle      TEXT NOT NULL,
		promo_code TEXT NOT NULL DEFAULT '',
		total      INTEGER NOT NULL DEFAULT 0,
		status     TEXT NOT NULL,
		ip         TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX orders_by_status ON orders (status, id);
	CREATE TABLE invoicing_runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		started_at  INTEGER NOT NULL,
		finished_at INTEGER NOT NULL,
		counts      TEXT NOT NULL DEFAULT '{}',
		errors      TEXT NOT NULL DEFAULT '[]'
	);`

var (
	// ErrInsufficientCredit: the account's credit doesn't cover the amount.
	ErrInsufficientCredit = errors.New("not enough credit")
	// ErrInvoiceState: the invoice's status doesn't allow the change.
	ErrInvoiceState = errors.New("the invoice's status doesn't allow that")
	// ErrOverRefund: more than what is left of the payment.
	ErrOverRefund = errors.New("the refund exceeds what is left of the payment")
	// ErrPromoUsedUp: the promotion reached its maximum uses (or is off).
	ErrPromoUsedUp = errors.New("the promotion is no longer available")
)

// Invoice statuses.
const (
	InvoiceDraft             = "draft"
	InvoiceUnpaid            = "unpaid"
	InvoicePaid              = "paid"
	InvoiceCancelled         = "cancelled"
	InvoiceRefunded          = "refunded"
	InvoicePartiallyRefunded = "partially_refunded"
)

// Order statuses.
const (
	OrderPending   = "pending"
	OrderActive    = "active"
	OrderCancelled = "cancelled"
)

// ---- Billing profiles ----

// BillingContact is who an account's invoices are addressed to.
type BillingContact struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Company   string `json:"company"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Address1  string `json:"address1"`
	Address2  string `json:"address2"`
	City      string `json:"city"`
	State     string `json:"state"`
	Postcode  string `json:"postcode"`
	Country   string `json:"country"`
	TaxID     string `json:"tax_id"`
}

// BillingProfile is how an account is billed. Mode "" means not decided
// here: the billing package derives it from the account (Stripe
// subscription, WHMCS, or none).
type BillingProfile struct {
	AccountID int64
	Mode      string
	Cycle     string
	// PriceOverride replaces the plan's price for the cycle (nil: none).
	PriceOverride *int64
	// NextDueAt is when the next unpaid period starts (what was paid for
	// runs until then); AnchorDay the day of the month due dates fall on.
	NextDueAt time.Time
	AnchorDay int
	AutoPay   bool
	// The saved card (a Stripe payment method of StripeCustomer).
	StripeCustomer string
	CardPM         string
	CardBrand      string
	CardLast4      string
	CardExpMonth   int
	CardExpYear    int
	// Credit is changed only through the ledger (AddCredit, ApplyCredit…).
	Credit       int64
	TaxExempt    bool
	CancelAt     time.Time
	CancelReason string
	// PromoID is a recurring promotion applying to renewals (0: none).
	PromoID   int64
	Contact   BillingContact
	UpdatedAt time.Time
}

const profileCols = `account_id, mode, cycle, price_override, next_due_at, anchor_day, auto_pay, stripe_customer, card_pm,
	card_brand, card_last4, card_exp_month, card_exp_year, credit, tax_exempt, cancel_at, cancel_reason, promo_id, contact,
	updated_at`

func scanProfile(row interface{ Scan(...any) error }) (*BillingProfile, error) {
	var p BillingProfile
	var override sql.NullInt64
	var next, cancel, updated int64
	var contact string
	err := row.Scan(&p.AccountID, &p.Mode, &p.Cycle, &override, &next, &p.AnchorDay, &p.AutoPay, &p.StripeCustomer,
		&p.CardPM, &p.CardBrand, &p.CardLast4, &p.CardExpMonth, &p.CardExpYear, &p.Credit, &p.TaxExempt, &cancel,
		&p.CancelReason, &p.PromoID, &contact, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if override.Valid {
		v := override.Int64
		p.PriceOverride = &v
	}
	p.NextDueAt, p.CancelAt, p.UpdatedAt = unixTime(next), unixTime(cancel), unixTime(updated)
	if err := json.Unmarshal([]byte(contact), &p.Contact); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetBillingProfile returns an account's profile (ErrNotFound: none yet).
func (s *Store) GetBillingProfile(ctx context.Context, accountID int64) (*BillingProfile, error) {
	return scanProfile(s.db.QueryRowContext(ctx, `SELECT `+profileCols+` FROM billing_profiles WHERE account_id = ?`, accountID))
}

// BillingProfiles lists every profile.
func (s *Store) BillingProfiles(ctx context.Context) ([]*BillingProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+profileCols+` FROM billing_profiles ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*BillingProfile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveBillingProfile creates or updates a profile: everything but the
// credit, which only the ledger changes.
func (s *Store) SaveBillingProfile(ctx context.Context, p *BillingProfile) error {
	contact, err := json.Marshal(p.Contact)
	if err != nil {
		return err
	}
	var override any
	if p.PriceOverride != nil {
		override = *p.PriceOverride
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO billing_profiles (account_id, mode, cycle, price_override, next_due_at,
		anchor_day, auto_pay, stripe_customer, card_pm, card_brand, card_last4, card_exp_month, card_exp_year, tax_exempt,
		cancel_at, cancel_reason, promo_id, contact, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET mode = excluded.mode, cycle = excluded.cycle,
		price_override = excluded.price_override, next_due_at = excluded.next_due_at, anchor_day = excluded.anchor_day,
		auto_pay = excluded.auto_pay, stripe_customer = excluded.stripe_customer, card_pm = excluded.card_pm,
		card_brand = excluded.card_brand, card_last4 = excluded.card_last4, card_exp_month = excluded.card_exp_month,
		card_exp_year = excluded.card_exp_year, tax_exempt = excluded.tax_exempt, cancel_at = excluded.cancel_at,
		cancel_reason = excluded.cancel_reason, promo_id = excluded.promo_id, contact = excluded.contact,
		updated_at = excluded.updated_at`,
		p.AccountID, p.Mode, p.Cycle, override, unixOrZero(p.NextDueAt), p.AnchorDay, p.AutoPay, p.StripeCustomer, p.CardPM,
		p.CardBrand, p.CardLast4, p.CardExpMonth, p.CardExpYear, p.TaxExempt, unixOrZero(p.CancelAt), p.CancelReason,
		p.PromoID, string(contact), time.Now().Unix())
	return err
}

// ensureProfile makes sure an account has a profile row, so the credit
// can be locked and changed.
func ensureProfile(ctx context.Context, tx *Tx, accountID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO billing_profiles (account_id, updated_at) VALUES (?, ?)
		ON CONFLICT (account_id) DO NOTHING`, accountID, time.Now().Unix())
	return err
}

// ---- Credit ----

// CreditEntry is a line of an account's credit ledger; Balance is the
// credit after it.
type CreditEntry struct {
	ID          int64     `json:"id"`
	AccountID   int64     `json:"-"`
	Amount      int64     `json:"amount"`
	Balance     int64     `json:"balance"`
	Description string    `json:"description"`
	InvoiceID   int64     `json:"invoice_id"`
	At          time.Time `json:"at"`
	By          string    `json:"by"`
}

// changeCredit adds amount (negative: takes) to an account's credit and
// records it, in tx. A ref already recorded does nothing (dup true). The
// credit never goes negative: ErrInsufficientCredit.
func changeCredit(ctx context.Context, tx *Tx, accountID, amount int64, desc string, invoiceID int64, by, ref string, at time.Time) (*CreditEntry, bool, error) {
	if ref != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM credit_ledger WHERE ref = ?`, ref).Scan(&n); err != nil {
			return nil, false, err
		}
		if n > 0 {
			return nil, true, nil
		}
	}
	if err := ensureProfile(ctx, tx, accountID); err != nil {
		return nil, false, err
	}
	var credit int64
	if err := tx.QueryRowContext(ctx, `SELECT credit FROM billing_profiles WHERE account_id = ? FOR UPDATE`, accountID).
		Scan(&credit); err != nil {
		return nil, false, err
	}
	if credit+amount < 0 {
		return nil, false, fmt.Errorf("%w: %d available", ErrInsufficientCredit, credit)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_profiles SET credit = ? WHERE account_id = ?`, credit+amount, accountID); err != nil {
		return nil, false, err
	}
	var key any
	if ref != "" {
		key = ref
	}
	e := &CreditEntry{AccountID: accountID, Amount: amount, Balance: credit + amount, Description: desc, InvoiceID: invoiceID,
		At: at.UTC().Truncate(time.Second), By: by}
	err := tx.QueryRowContext(ctx, `INSERT INTO credit_ledger (account_id, amount, balance, description, invoice_id, at, by_name,
		ref) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, accountID, amount, e.Balance, desc, invoiceID, at.Unix(), by, key).Scan(&e.ID)
	if err != nil {
		return nil, false, err
	}
	return e, false, nil
}

// AddCredit changes an account's credit by amount (negative removes it,
// never below zero). With a ref, the same adjustment is only ever made
// once (dup true the next times).
func (s *Store) AddCredit(ctx context.Context, accountID, amount int64, desc, by, ref string, at time.Time) (*CreditEntry, bool, error) {
	var e *CreditEntry
	var dup bool
	err := s.db.inTx(ctx, func(tx *Tx) error {
		var err error
		e, dup, err = changeCredit(ctx, tx, accountID, amount, desc, 0, by, ref, at)
		return err
	})
	return e, dup, err
}

// CreditEntries lists an account's credit ledger, newest first.
func (s *Store) CreditEntries(ctx context.Context, accountID int64, limit int) ([]CreditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, amount, balance, description, invoice_id, at, by_name
		FROM credit_ledger WHERE account_id = ? ORDER BY id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CreditEntry{}
	for rows.Next() {
		var e CreditEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.AccountID, &e.Amount, &e.Balance, &e.Description, &e.InvoiceID, &at, &e.By); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Invoices ----

// TaxLine is one tax on an invoice (rate in hundredths of a percent).
type TaxLine struct {
	Name   string `json:"name"`
	Rate   int64  `json:"rate"`
	Amount int64  `json:"amount"`
}

// BillingAddress is the invoice's "bill to", frozen when it is made.
type BillingAddress struct {
	Name    string   `json:"name"`
	Company string   `json:"company"`
	Lines   []string `json:"lines"`
	Country string   `json:"country"`
	TaxID   string   `json:"tax_id"`
	Email   string   `json:"email,omitempty"`
}

// InvoiceItem is a line of an invoice. Amount is quantity × unit price
// (negative for discounts and credits).
type InvoiceItem struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"`
	Amount      int64  `json:"amount"`
	Taxable     bool   `json:"taxable"`
}

// InvoiceTotals are an invoice's computed totals.
type InvoiceTotals struct {
	Subtotal int64
	Discount int64
	Tax      int64
	Total    int64
	TaxLines []TaxLine
}

// Invoice is a bill. Number is "" until one is assigned (gapless, at
// issue or at payment, see Numbering). DedupeKey makes an invoice the
// automation creates unique (one renewal per period).
type Invoice struct {
	ID          int64
	Number      string
	AccountID   int64
	AccountName string
	Kind        string
	Status      string
	Currency    string
	IssuedAt    time.Time
	DueAt       time.Time
	PaidAt      time.Time
	PeriodStart time.Time
	PeriodEnd   time.Time
	InvoiceTotals
	CreditApplied  int64
	AmountPaid     int64
	AmountRefunded int64
	BillingAddress BillingAddress
	Notes          string
	// What paying it does: the plan and cycle it is for, and a promotion
	// it used.
	PlanID        string
	Cycle         string
	PromoID       int64
	DedupeKey     string
	LateFeeAt     time.Time
	AutochargeDay string
	PayAttempts   int
	// Units is what a top-up buys (burst minutes), granted once when paid
	// (UnitsGranted).
	Units        int64
	UnitsGranted bool
	// EffectsDone: what paying it does (activation, renewal…) was applied.
	EffectsDone bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Items and Payments are filled by GetInvoice.
	Items    []InvoiceItem
	Payments []Payment
}

// Balance is what is left to pay on an unpaid invoice.
func (i *Invoice) Balance() int64 {
	return i.Total - i.CreditApplied - i.AmountPaid + i.AmountRefunded
}

// Numbering assigns invoice numbers: Prefix + the next number of the
// gapless sequence, zero-padded to six digits.
type Numbering struct{ Prefix string }

const invoiceCols = `id, COALESCE(number, ''), account_id, account_name, kind, status, currency, issued_at, due_at, paid_at,
	period_start, period_end, subtotal, discount, tax, total, credit_applied, amount_paid, amount_refunded, tax_lines,
	billing_address, notes, plan_id, cycle, promo_id, COALESCE(dedupe_key, ''), late_fee_at, autocharge_day, pay_attempts,
	units, units_granted, effects_done, created_at, updated_at`

func scanInvoice(row interface{ Scan(...any) error }) (*Invoice, error) {
	var i Invoice
	var issued, due, paid, pstart, pend, late, created, updated int64
	var lines, addr string
	err := row.Scan(&i.ID, &i.Number, &i.AccountID, &i.AccountName, &i.Kind, &i.Status, &i.Currency, &issued, &due, &paid,
		&pstart, &pend, &i.Subtotal, &i.Discount, &i.Tax, &i.Total, &i.CreditApplied, &i.AmountPaid, &i.AmountRefunded,
		&lines, &addr, &i.Notes, &i.PlanID, &i.Cycle, &i.PromoID, &i.DedupeKey, &late, &i.AutochargeDay, &i.PayAttempts,
		&i.Units, &i.UnitsGranted, &i.EffectsDone, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	i.IssuedAt, i.DueAt, i.PaidAt = unixTime(issued), unixTime(due), unixTime(paid)
	i.PeriodStart, i.PeriodEnd, i.LateFeeAt = unixTime(pstart), unixTime(pend), unixTime(late)
	i.CreatedAt, i.UpdatedAt = unixTime(created), unixTime(updated)
	if err := json.Unmarshal([]byte(lines), &i.TaxLines); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(addr), &i.BillingAddress); err != nil {
		return nil, err
	}
	return &i, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateInvoice stores a new invoice with its items; with numbering (and
// a status other than draft) it gets its number now. A dedupe key used
// before: ErrExists.
func (s *Store) CreateInvoice(ctx context.Context, inv *Invoice, numbering *Numbering) (*Invoice, error) {
	var id int64
	err := s.db.inTx(ctx, func(tx *Tx) error {
		var err error
		id, err = createInvoice(ctx, tx, inv, numbering)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetInvoice(ctx, id)
}

func createInvoice(ctx context.Context, tx *Tx, inv *Invoice, numbering *Numbering) (int64, error) {
	lines, err := json.Marshal(nonNilLines(inv.TaxLines))
	if err != nil {
		return 0, err
	}
	addr, err := json.Marshal(inv.BillingAddress)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO invoices (account_id, account_name, kind, status, currency, issued_at, due_at,
		paid_at, period_start, period_end, subtotal, discount, tax, total, tax_lines, billing_address, notes, plan_id, cycle,
		promo_id, dedupe_key, units, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`, inv.AccountID, inv.AccountName, inv.Kind, inv.Status, inv.Currency, unixOrZero(inv.IssuedAt),
		unixOrZero(inv.DueAt), unixOrZero(inv.PaidAt), unixOrZero(inv.PeriodStart), unixOrZero(inv.PeriodEnd), inv.Subtotal,
		inv.Discount, inv.Tax, inv.Total, string(lines), string(addr), inv.Notes, inv.PlanID, inv.Cycle, inv.PromoID,
		nullable(inv.DedupeKey), inv.Units, now, now).Scan(&id)
	if isUnique(err) {
		return 0, ErrExists
	}
	if err != nil {
		return 0, err
	}
	if err := insertItems(ctx, tx, id, inv.Items); err != nil {
		return 0, err
	}
	if numbering != nil && inv.Status != InvoiceDraft {
		if err := assignNumber(ctx, tx, id, numbering.Prefix); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func nonNilLines(l []TaxLine) []TaxLine {
	if l == nil {
		return []TaxLine{}
	}
	return l
}

func insertItems(ctx context.Context, tx *Tx, invoiceID int64, items []InvoiceItem) error {
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO invoice_items (invoice_id, kind, description, quantity, unit_price,
			amount, taxable) VALUES (?, ?, ?, ?, ?, ?, ?)`, invoiceID, it.Kind, it.Description, it.Quantity, it.UnitPrice,
			it.Amount, it.Taxable); err != nil {
			return err
		}
	}
	return nil
}

// assignNumber gives an invoice the next number of the sequence, in the
// transaction that makes it final (issue or payment): the counter row is
// locked by its update, and rolled back with the rest, so numbers have no
// gaps. An invoice with a number keeps it.
func assignNumber(ctx context.Context, tx *Tx, invoiceID int64, prefix string) error {
	var has string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(number, '') FROM invoices WHERE id = ?`, invoiceID).Scan(&has); err != nil {
		return err
	}
	if has != "" {
		return nil
	}
	if err := ensureCounter(ctx, tx); err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `UPDATE invoice_counters SET next_seq = next_seq + 1 WHERE name = 'invoice'
		RETURNING next_seq - 1`).Scan(&seq); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE invoices SET number = ?, number_seq = ?, number_prefix = ? WHERE id = ?`,
		FormatInvoiceNumber(prefix, seq), seq, prefix, invoiceID)
	if isUnique(err) {
		return fmt.Errorf("%w: invoice number %s is taken; raise the next number", ErrConflict, FormatInvoiceNumber(prefix, seq))
	}
	return err
}

// ensureCounter creates the sequence's row on first use (a migration
// doesn't seed it: a new database must stay empty for `wpgenie db copy`).
func ensureCounter(ctx context.Context, tx *Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO invoice_counters (name, next_seq) VALUES ('invoice', 1)
		ON CONFLICT (name) DO NOTHING`)
	return err
}

// FormatInvoiceNumber is prefix + the sequence number, six digits at least.
func FormatInvoiceNumber(prefix string, seq int64) string {
	return fmt.Sprintf("%s%06d", prefix, seq)
}

// NextInvoiceNumber is the number the next invoice gets.
func (s *Store) NextInvoiceNumber(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT next_seq FROM invoice_counters WHERE name = 'invoice'`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	return n, err
}

// SetNextInvoiceNumber moves the sequence. It can't go back to numbers
// already used with the prefix (ErrConflict): numbers are unique. The
// counter is locked first, so a number assigned meanwhile is seen.
func (s *Store) SetNextInvoiceNumber(ctx context.Context, next int64, prefix string) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		if err := ensureCounter(ctx, tx); err != nil {
			return err
		}
		var cur int64
		if err := tx.QueryRowContext(ctx, `SELECT next_seq FROM invoice_counters WHERE name = 'invoice' FOR UPDATE`).
			Scan(&cur); err != nil {
			return err
		}
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number_seq), 0) FROM invoices WHERE number_prefix = ?`,
			prefix).Scan(&used); err != nil {
			return err
		}
		if next <= used {
			return fmt.Errorf("%w: %s is already used; the next number must be above %d", ErrConflict,
				FormatInvoiceNumber(prefix, used), used)
		}
		_, err := tx.ExecContext(ctx, `UPDATE invoice_counters SET next_seq = ? WHERE name = 'invoice'`, next)
		return err
	})
}

// GetInvoice returns an invoice with its items and payments.
func (s *Store) GetInvoice(ctx context.Context, id int64) (*Invoice, error) {
	inv, err := scanInvoice(s.db.QueryRowContext(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if inv.Items, err = s.invoiceItems(ctx, id); err != nil {
		return nil, err
	}
	if inv.Payments, err = s.Payments(ctx, PaymentFilter{InvoiceID: id, Limit: 1000}); err != nil {
		return nil, err
	}
	return inv, nil
}

// InvoiceByDedupe finds the invoice made for a dedupe key.
func (s *Store) InvoiceByDedupe(ctx context.Context, key string) (*Invoice, error) {
	return scanInvoice(s.db.QueryRowContext(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE dedupe_key = ?`, key))
}

func (s *Store) invoiceItems(ctx context.Context, id int64) ([]InvoiceItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, description, quantity, unit_price, amount, taxable FROM invoice_items
		WHERE invoice_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InvoiceItem{}
	for rows.Next() {
		var it InvoiceItem
		if err := rows.Scan(&it.ID, &it.Kind, &it.Description, &it.Quantity, &it.UnitPrice, &it.Amount, &it.Taxable); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// InvoiceFilter narrows ListInvoices; zero values don't filter.
type InvoiceFilter struct {
	AccountID int64
	Status    string
	// Statuses: any of them (with Status: both apply).
	Statuses []string
	Kind     string
	// Q matches the number or the account's name.
	Q string
	// From/To: issued (created, for drafts) in [From, To).
	From, To time.Time
	// OverdueAt: unpaid invoices due before it.
	OverdueAt time.Time
	// NoDrafts leaves drafts out (what clients see).
	NoDrafts bool
	// BeforeID pages: only older invoices.
	BeforeID int64
	Limit    int
}

// ListInvoices lists invoices, newest first, without items.
func (s *Store) ListInvoices(ctx context.Context, f InvoiceFilter) ([]*Invoice, error) {
	where, args := invoiceWhere(f)
	q := `SELECT ` + invoiceCols + ` FROM invoices WHERE ` + where + ` ORDER BY id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Invoice{}
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func invoiceWhere(f InvoiceFilter) (string, []any) {
	conds := []string{"1 = 1"}
	var args []any
	add := func(c string, a ...any) {
		conds = append(conds, c)
		args = append(args, a...)
	}
	if f.AccountID != 0 {
		add(`account_id = ?`, f.AccountID)
	}
	if f.Status != "" {
		add(`status = ?`, f.Status)
	}
	if len(f.Statuses) > 0 {
		c := `status IN (` + placeholders(len(f.Statuses)) + `)`
		for _, st := range f.Statuses {
			args = append(args, st)
		}
		conds = append(conds, c)
	}
	if f.Kind != "" {
		add(`kind = ?`, f.Kind)
	}
	if f.Q != "" {
		like := "%" + f.Q + "%"
		add(`(COALESCE(number, '') LIKE ? OR account_name LIKE ?)`, like, like)
	}
	if !f.From.IsZero() {
		add(`(CASE WHEN issued_at > 0 THEN issued_at ELSE created_at END) >= ?`, f.From.Unix())
	}
	if !f.To.IsZero() {
		add(`(CASE WHEN issued_at > 0 THEN issued_at ELSE created_at END) < ?`, f.To.Unix())
	}
	if !f.OverdueAt.IsZero() {
		add(`status = ? AND due_at > 0 AND due_at < ?`, InvoiceUnpaid, f.OverdueAt.Unix())
	}
	if f.NoDrafts {
		add(`status <> ?`, InvoiceDraft)
	}
	if f.BeforeID > 0 {
		add(`id < ?`, f.BeforeID)
	}
	return strings.Join(conds, " AND "), args
}

// InvoiceStats are the totals of unpaid invoices: all, and those overdue
// at a time.
type InvoiceStats struct {
	UnpaidCount  int
	Outstanding  int64
	OverdueCount int
	OverdueTotal int64
}

func (s *Store) InvoiceStats(ctx context.Context, now time.Time) (*InvoiceStats, error) {
	var st InvoiceStats
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(total - credit_applied - amount_paid + amount_refunded), 0),
		COALESCE(SUM(CASE WHEN due_at > 0 AND due_at < ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN due_at > 0 AND due_at < ? THEN total - credit_applied - amount_paid + amount_refunded ELSE 0 END), 0)
		FROM invoices WHERE status = ?`, now.Unix(), now.Unix(), InvoiceUnpaid).
		Scan(&st.UnpaidCount, &st.Outstanding, &st.OverdueCount, &st.OverdueTotal)
	return &st, err
}

// lockInvoice reads an invoice for update in tx.
func lockInvoice(ctx context.Context, tx *Tx, id int64) (*Invoice, error) {
	return scanInvoice(tx.QueryRowContext(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE id = ? FOR UPDATE`, id))
}

// UpdateDraftInvoice replaces a draft's items, totals, due date and notes.
func (s *Store) UpdateDraftInvoice(ctx context.Context, inv *Invoice) error {
	lines, err := json.Marshal(nonNilLines(inv.TaxLines))
	if err != nil {
		return err
	}
	return s.db.inTx(ctx, func(tx *Tx) error {
		cur, err := lockInvoice(ctx, tx, inv.ID)
		if err != nil {
			return err
		}
		if cur.Status != InvoiceDraft {
			return fmt.Errorf("%w: only drafts can be edited", ErrInvoiceState)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invoice_items WHERE invoice_id = ?`, inv.ID); err != nil {
			return err
		}
		if err := insertItems(ctx, tx, inv.ID, inv.Items); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE invoices SET subtotal = ?, discount = ?, tax = ?, total = ?, tax_lines = ?,
			due_at = ?, notes = ?, updated_at = ? WHERE id = ?`, inv.Subtotal, inv.Discount, inv.Tax, inv.Total, string(lines),
			unixOrZero(inv.DueAt), inv.Notes, time.Now().Unix(), inv.ID)
		return err
	})
}

// UpdateInvoiceNotes changes an unpaid (or draft) invoice's notes and due
// date.
func (s *Store) UpdateInvoiceNotes(ctx context.Context, id int64, notes string, due time.Time) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		cur, err := lockInvoice(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Status != InvoiceDraft && cur.Status != InvoiceUnpaid {
			return fmt.Errorf("%w: the invoice is %s", ErrInvoiceState, cur.Status)
		}
		_, err = tx.ExecContext(ctx, `UPDATE invoices SET notes = ?, due_at = ?, updated_at = ? WHERE id = ?`,
			notes, unixOrZero(due), time.Now().Unix(), id)
		return err
	})
}

// AddInvoiceItem adds a line to an unpaid invoice with its new totals.
// lateFee marks it as the invoice's late fee: added once (false when it
// already has one).
func (s *Store) AddInvoiceItem(ctx context.Context, id int64, it InvoiceItem, t InvoiceTotals, lateFee bool, at time.Time) (bool, error) {
	lines, err := json.Marshal(nonNilLines(t.TaxLines))
	if err != nil {
		return false, err
	}
	added := false
	err = s.db.inTx(ctx, func(tx *Tx) error {
		added = false
		cur, err := lockInvoice(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Status != InvoiceUnpaid {
			return fmt.Errorf("%w: the invoice is %s", ErrInvoiceState, cur.Status)
		}
		if lateFee && !cur.LateFeeAt.IsZero() {
			return nil
		}
		if err := insertItems(ctx, tx, id, []InvoiceItem{it}); err != nil {
			return err
		}
		late := unixOrZero(cur.LateFeeAt)
		if lateFee {
			late = at.Unix()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET subtotal = ?, discount = ?, tax = ?, total = ?, tax_lines = ?,
			late_fee_at = ?, updated_at = ? WHERE id = ?`, t.Subtotal, t.Discount, t.Tax, t.Total, string(lines), late,
			time.Now().Unix(), id); err != nil {
			return err
		}
		added = true
		return nil
	})
	return added, err
}

// IssueInvoice makes a draft final: unpaid, issued at, and numbered with
// numbering.
func (s *Store) IssueInvoice(ctx context.Context, id int64, at time.Time, numbering *Numbering) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		cur, err := lockInvoice(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Status != InvoiceDraft {
			return fmt.Errorf("%w: only drafts are issued", ErrInvoiceState)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET status = ?, issued_at = ?, updated_at = ? WHERE id = ?`,
			InvoiceUnpaid, at.Unix(), time.Now().Unix(), id); err != nil {
			return err
		}
		if numbering != nil {
			return assignNumber(ctx, tx, id, numbering.Prefix)
		}
		return nil
	})
}

// settle marks an unpaid invoice paid when nothing is left to pay, and
// numbers it with numbering. It reports whether it did.
func settle(ctx context.Context, tx *Tx, inv *Invoice, at time.Time, numbering *Numbering) (bool, error) {
	if inv.Status != InvoiceUnpaid || inv.Balance() > 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET status = ?, paid_at = ?, updated_at = ? WHERE id = ?`,
		InvoicePaid, at.Unix(), time.Now().Unix(), inv.ID); err != nil {
		return false, err
	}
	if numbering != nil {
		if err := assignNumber(ctx, tx, inv.ID, numbering.Prefix); err != nil {
			return false, err
		}
	}
	return true, nil
}

// SettleInvoice marks an unpaid invoice with nothing to pay (a zero total)
// paid; true if it did.
func (s *Store) SettleInvoice(ctx context.Context, id int64, at time.Time, numbering *Numbering) (bool, error) {
	paid := false
	err := s.db.inTx(ctx, func(tx *Tx) error {
		cur, err := lockInvoice(ctx, tx, id)
		if err != nil {
			return err
		}
		paid, err = settle(ctx, tx, cur, at, numbering)
		return err
	})
	return paid, err
}

// CancelInvoice cancels a draft or unpaid invoice. What was already paid
// on it (credit applied, payments net of refunds) goes back to the
// account's credit.
//
// release frees the invoice's dedupe key, so what it was for (a renewal
// of a period) can be invoiced again (after a plan change, at the new
// price); otherwise the automation never makes it again.
func (s *Store) CancelInvoice(ctx context.Context, id int64, by string, at time.Time, release bool) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		cur, err := lockInvoice(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Status != InvoiceDraft && cur.Status != InvoiceUnpaid {
			return fmt.Errorf("%w: the invoice is %s", ErrInvoiceState, cur.Status)
		}
		q := `UPDATE invoices SET status = ?, updated_at = ? WHERE id = ?`
		if release {
			q = `UPDATE invoices SET status = ?, updated_at = ?, dedupe_key = NULL WHERE id = ?`
		}
		if _, err := tx.ExecContext(ctx, q, InvoiceCancelled, time.Now().Unix(), id); err != nil {
			return err
		}
		if back := cur.CreditApplied + cur.AmountPaid - cur.AmountRefunded; back > 0 {
			_, _, err := changeCredit(ctx, tx, cur.AccountID, back, "Invoice "+invoiceLabel(cur)+" cancelled: amounts paid returned",
				id, by, fmt.Sprintf("cancel:%d", id), at)
			return err
		}
		return nil
	})
}

func invoiceLabel(inv *Invoice) string {
	if inv.Number != "" {
		return inv.Number
	}
	return fmt.Sprintf("#%d", inv.ID)
}

// GrantBurstMinutes adds a paid top-up invoice's burst minutes (its Units)
// to its account's burst credit, in the transaction that marks them
// granted: whoever calls it, however often, they are added once. It
// returns the minutes added now (0: granted before, or nothing to grant).
func (s *Store) GrantBurstMinutes(ctx context.Context, invoiceID int64) (int64, error) {
	var granted int64
	err := s.db.inTx(ctx, func(tx *Tx) error {
		granted = 0
		inv, err := lockInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if inv.UnitsGranted || inv.Units <= 0 {
			return nil
		}
		if inv.Status != InvoicePaid && inv.Status != InvoicePartiallyRefunded && inv.Status != InvoiceRefunded {
			return fmt.Errorf("%w: the invoice isn't paid", ErrInvoiceState)
		}
		res, err := tx.ExecContext(ctx, `UPDATE accounts SET burst_credit = burst_credit + ?, updated_at = ? WHERE id = ?`,
			inv.Units, time.Now().Unix(), inv.AccountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET units_granted = 1, updated_at = ? WHERE id = ?`,
			time.Now().Unix(), invoiceID); err != nil {
			return err
		}
		granted = inv.Units
		return nil
	})
	return granted, err
}

// SetInvoiceEffectsDone records that what paying an invoice does was done.
func (s *Store) SetInvoiceEffectsDone(ctx context.Context, id int64) error {
	return s.exec1(ctx, `UPDATE invoices SET effects_done = 1, updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

// SetInvoiceAutocharge records the day ("2006-01-02") a saved card was
// charged for an invoice; false if it already was that day (one attempt a
// day, whoever runs it).
func (s *Store) SetInvoiceAutocharge(ctx context.Context, id int64, day string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE invoices SET autocharge_day = ?, updated_at = ? WHERE id = ? AND autocharge_day <> ?`,
		day, time.Now().Unix(), id, day)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// NextPayAttempt counts a payment link made for an invoice and returns its
// number (gateways want a new reference per link).
func (s *Store) NextPayAttempt(ctx context.Context, id int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `UPDATE invoices SET pay_attempts = pay_attempts + 1 WHERE id = ? RETURNING pay_attempts`,
		id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// PaidInvoicesPending lists paid invoices whose effects weren't applied.
func (s *Store) PaidInvoicesPending(ctx context.Context) ([]*Invoice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE effects_done = 0 AND status IN (?, ?, ?)
		ORDER BY id`, InvoicePaid, InvoicePartiallyRefunded, InvoiceRefunded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Invoice{}
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ---- Payments ----

// Payment is money received for an invoice (a transaction). Amount is
// what came in; what went beyond the invoice's balance became credit.
type Payment struct {
	ID            int64     `json:"id"`
	InvoiceID     int64     `json:"invoice_id"`
	InvoiceNumber string    `json:"invoice_number,omitempty"`
	AccountID     int64     `json:"account_id"`
	AccountName   string    `json:"account_name,omitempty"`
	Gateway       string    `json:"gateway"`
	Reference     string    `json:"reference"`
	Amount        int64     `json:"amount"`
	Fee           int64     `json:"fee"`
	Refunded      int64     `json:"refunded"`
	Note          string    `json:"note"`
	By            string    `json:"-"` // staff's names stay staff's
	At            time.Time `json:"at"`
}

// PaymentInput records a payment. Dedupe ("stripe:pi_…") makes the same
// payment count once however often it is reported.
type PaymentInput struct {
	InvoiceID int64
	Gateway   string
	Reference string
	Amount    int64
	Fee       int64
	Note      string
	By        string
	At        time.Time
	Dedupe    string
	// Numbering: number the invoice when this pays it (number on payment).
	Numbering *Numbering
}

// PaymentResult is what recording a payment did.
type PaymentResult struct {
	Payment *Payment
	// Duplicate: the payment was recorded before; nothing changed.
	Duplicate bool
	// Paid: this payment made the invoice paid.
	Paid bool
	// Credited: the part beyond the balance, added to the account's credit.
	Credited int64
}

// RecordPayment records a payment and applies it to its invoice, in one
// transaction: the invoice's amount paid, paid when nothing is left, and
// anything beyond the balance (or paid on an invoice no longer unpaid) to
// the account's credit.
func (s *Store) RecordPayment(ctx context.Context, in PaymentInput) (*PaymentResult, error) {
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: a payment's amount must be positive", ErrInvoiceState)
	}
	var res *PaymentResult
	err := s.db.inTx(ctx, func(tx *Tx) error {
		res = &PaymentResult{}
		inv, err := lockInvoice(ctx, tx, in.InvoiceID)
		if err != nil {
			return err
		}
		if inv.Status == InvoiceDraft {
			return fmt.Errorf("%w: issue the invoice before recording payments", ErrInvoiceState)
		}
		if in.Dedupe != "" {
			p, err := scanPayment(tx.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments p
				JOIN invoices i ON i.id = p.invoice_id WHERE p.dedupe = ?`, in.Dedupe))
			if err == nil {
				res.Payment, res.Duplicate = p, true
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		applied := int64(0)
		if inv.Status == InvoiceUnpaid {
			applied = min(in.Amount, max(inv.Balance(), 0))
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO payments (invoice_id, account_id, gateway, reference, amount, fee, note,
			by_name, at, dedupe) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, inv.ID, inv.AccountID, in.Gateway,
			in.Reference, in.Amount, in.Fee, in.Note, in.By, in.At.Unix(), nullable(in.Dedupe)).Scan(&id); err != nil {
			return err
		}
		if applied > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE invoices SET amount_paid = amount_paid + ?, updated_at = ? WHERE id = ?`,
				applied, time.Now().Unix(), inv.ID); err != nil {
				return err
			}
			inv.AmountPaid += applied
			if res.Paid, err = settle(ctx, tx, inv, in.At, in.Numbering); err != nil {
				return err
			}
		}
		if extra := in.Amount - applied; extra > 0 {
			if _, _, err := changeCredit(ctx, tx, inv.AccountID, extra, "Payment beyond invoice "+invoiceLabel(inv),
				inv.ID, in.By, fmt.Sprintf("payment:%d", id), in.At); err != nil {
				return err
			}
			res.Credited = extra
		}
		res.Payment, err = scanPayment(tx.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments p
			JOIN invoices i ON i.id = p.invoice_id WHERE p.id = ?`, id))
		return err
	})
	if isUnique(err) && in.Dedupe != "" {
		// A concurrent report of the same payment won (PostgreSQL).
		p, perr := s.PaymentByDedupe(ctx, in.Dedupe)
		if perr != nil {
			return nil, err
		}
		return &PaymentResult{Payment: p, Duplicate: true}, nil
	}
	return res, err
}

// ApplyCredit pays an unpaid invoice from the account's credit: amount, or
// as much as possible when 0. It returns what was applied and whether the
// invoice is now paid.
func (s *Store) ApplyCredit(ctx context.Context, invoiceID, amount int64, by string, at time.Time, numbering *Numbering) (int64, bool, error) {
	var applied int64
	var paid bool
	err := s.db.inTx(ctx, func(tx *Tx) error {
		applied, paid = 0, false
		inv, err := lockInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if inv.Status != InvoiceUnpaid {
			return fmt.Errorf("%w: the invoice is %s", ErrInvoiceState, inv.Status)
		}
		if err := ensureProfile(ctx, tx, inv.AccountID); err != nil {
			return err
		}
		var credit int64
		if err := tx.QueryRowContext(ctx, `SELECT credit FROM billing_profiles WHERE account_id = ? FOR UPDATE`,
			inv.AccountID).Scan(&credit); err != nil {
			return err
		}
		bal := max(inv.Balance(), 0)
		amt := min(credit, bal)
		if amount > 0 {
			if amount > bal {
				return fmt.Errorf("%w: the balance is %d", ErrOverRefund, bal)
			}
			if amount > credit {
				return fmt.Errorf("%w: %d available", ErrInsufficientCredit, credit)
			}
			amt = amount
		}
		if amt <= 0 {
			return nil
		}
		if _, _, err := changeCredit(ctx, tx, inv.AccountID, -amt, "Applied to invoice "+invoiceLabel(inv), inv.ID, by, "", at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET credit_applied = credit_applied + ?, updated_at = ? WHERE id = ?`,
			amt, time.Now().Unix(), inv.ID); err != nil {
			return err
		}
		inv.CreditApplied += amt
		applied = amt
		paid, err = settle(ctx, tx, inv, at, numbering)
		return err
	})
	return applied, paid, err
}

const paymentCols = `p.id, p.invoice_id, COALESCE(i.number, ''), p.account_id, i.account_name, p.gateway, p.reference, p.amount,
	p.fee, p.refunded, p.note, p.by_name, p.at`

func scanPayment(row interface{ Scan(...any) error }) (*Payment, error) {
	var p Payment
	var at int64
	err := row.Scan(&p.ID, &p.InvoiceID, &p.InvoiceNumber, &p.AccountID, &p.AccountName, &p.Gateway, &p.Reference, &p.Amount,
		&p.Fee, &p.Refunded, &p.Note, &p.By, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.At = time.Unix(at, 0).UTC()
	return &p, nil
}

// GetPayment returns one payment.
func (s *Store) GetPayment(ctx context.Context, id int64) (*Payment, error) {
	return scanPayment(s.db.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments p JOIN invoices i ON i.id = p.invoice_id
		WHERE p.id = ?`, id))
}

// PaymentByDedupe finds a payment by its dedupe key.
func (s *Store) PaymentByDedupe(ctx context.Context, key string) (*Payment, error) {
	return scanPayment(s.db.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments p JOIN invoices i ON i.id = p.invoice_id
		WHERE p.dedupe = ?`, key))
}

// PaymentFilter narrows Payments; zero values don't filter.
type PaymentFilter struct {
	InvoiceID int64
	AccountID int64
	Gateway   string
	From, To  time.Time
	BeforeID  int64
	Limit     int
}

// Payments lists payments, newest first (an invoice's: oldest first).
func (s *Store) Payments(ctx context.Context, f PaymentFilter) ([]Payment, error) {
	conds := []string{"1 = 1"}
	var args []any
	add := func(c string, a any) {
		conds = append(conds, c)
		args = append(args, a)
	}
	order := `p.id DESC`
	if f.InvoiceID != 0 {
		add(`p.invoice_id = ?`, f.InvoiceID)
		order = `p.id`
	}
	if f.AccountID != 0 {
		add(`p.account_id = ?`, f.AccountID)
	}
	if f.Gateway != "" {
		add(`p.gateway = ?`, f.Gateway)
	}
	if !f.From.IsZero() {
		add(`p.at >= ?`, f.From.Unix())
	}
	if !f.To.IsZero() {
		add(`p.at < ?`, f.To.Unix())
	}
	if f.BeforeID > 0 {
		add(`p.id < ?`, f.BeforeID)
	}
	q := `SELECT ` + paymentCols + ` FROM payments p JOIN invoices i ON i.id = p.invoice_id WHERE ` +
		strings.Join(conds, " AND ") + ` ORDER BY ` + order
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payment{}
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ---- Refunds ----

// Refund is money given back from a payment: to the payer (through the
// gateway, or by hand) or to the account's credit.
type Refund struct {
	ID        int64     `json:"id"`
	PaymentID int64     `json:"payment_id"`
	InvoiceID int64     `json:"invoice_id"`
	AccountID int64     `json:"account_id"`
	Amount    int64     `json:"amount"`
	ToCredit  bool      `json:"to_credit"`
	Reference string    `json:"reference"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

// RefundInput records a refund; Dedupe ("razorpay:rfnd_…") records a
// gateway's refund once.
type RefundInput struct {
	PaymentID int64
	Amount    int64
	ToCredit  bool
	Reference string
	By        string
	At        time.Time
	Dedupe    string
}

// RecordRefund records a refund of (part of) a payment: the payment's and
// the invoice's refunded amounts, the invoice's status (refunded once all
// it was paid is refunded, partially before; an unpaid invoice owes it
// again), and the credit when refunded to credit. A dedupe key seen
// before: dup true, nothing changed.
func (s *Store) RecordRefund(ctx context.Context, in RefundInput) (*Refund, bool, error) {
	if in.Amount <= 0 {
		return nil, false, fmt.Errorf("%w: a refund's amount must be positive", ErrOverRefund)
	}
	var out *Refund
	var dup bool
	err := s.db.inTx(ctx, func(tx *Tx) error {
		out, dup = nil, false
		if in.Dedupe != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM refunds WHERE dedupe = ?`, in.Dedupe).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				dup = true
				return nil
			}
		}
		var invoiceID, accountID, amount, refunded int64
		err := tx.QueryRowContext(ctx, `SELECT invoice_id, account_id, amount, refunded FROM payments WHERE id = ? FOR UPDATE`,
			in.PaymentID).Scan(&invoiceID, &accountID, &amount, &refunded)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if in.Amount > amount-refunded {
			return fmt.Errorf("%w (%d left)", ErrOverRefund, amount-refunded)
		}
		inv, err := lockInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		out = &Refund{PaymentID: in.PaymentID, InvoiceID: invoiceID, AccountID: accountID, Amount: in.Amount,
			ToCredit: in.ToCredit, Reference: in.Reference, By: in.By, At: in.At.UTC().Truncate(time.Second)}
		if err := tx.QueryRowContext(ctx, `INSERT INTO refunds (payment_id, invoice_id, account_id, amount, to_credit, reference,
			by_name, at, dedupe) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, in.PaymentID, invoiceID, accountID, in.Amount,
			in.ToCredit, in.Reference, in.By, in.At.Unix(), nullable(in.Dedupe)).Scan(&out.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE payments SET refunded = refunded + ? WHERE id = ?`, in.Amount, in.PaymentID); err != nil {
			return err
		}
		status := inv.Status
		switch inv.Status {
		case InvoicePaid, InvoicePartiallyRefunded, InvoiceRefunded:
			status = InvoicePartiallyRefunded
			if inv.AmountRefunded+in.Amount >= inv.AmountPaid {
				status = InvoiceRefunded
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET amount_refunded = amount_refunded + ?, status = ?, updated_at = ?
			WHERE id = ?`, in.Amount, status, time.Now().Unix(), invoiceID); err != nil {
			return err
		}
		if in.ToCredit {
			_, _, err := changeCredit(ctx, tx, accountID, in.Amount, "Refund of a payment on invoice "+invoiceLabel(inv),
				invoiceID, in.By, fmt.Sprintf("refund:%d", out.ID), in.At)
			return err
		}
		return nil
	})
	if isUnique(err) && in.Dedupe != "" {
		return nil, true, nil
	}
	return out, dup, err
}

// Refunds lists refunds made in [from, to) (from zero: all).
func (s *Store) Refunds(ctx context.Context, from, to time.Time) ([]Refund, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, payment_id, invoice_id, account_id, amount, to_credit, reference, by_name, at
		FROM refunds WHERE at >= ? AND (? = 0 OR at < ?) ORDER BY id`, unixOrZero(from), unixOrZero(to), unixOrZero(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Refund{}
	for rows.Next() {
		var r Refund
		var at int64
		if err := rows.Scan(&r.ID, &r.PaymentID, &r.InvoiceID, &r.AccountID, &r.Amount, &r.ToCredit, &r.Reference, &r.By, &at); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Promotions ----

// Promotion is a promo code. Value is hundredths of a percent (percent) or
// minor units (fixed); empty Plans/Cycles mean all.
type Promotion struct {
	ID             int64
	Code           string
	Description    string
	Type           string
	Value          int64
	Plans          []string
	Cycles         []string
	Recurring      bool
	MaxUses        int
	Uses           int
	StartsAt       time.Time
	ExpiresAt      time.Time
	NewClientsOnly bool
	Enabled        bool
	CreatedAt      time.Time
}

const promoCols = `id, code, description, promo_type, value, plans, cycles, recurring, max_uses, uses, starts_at, expires_at,
	new_clients_only, enabled, created_at`

func scanPromo(row interface{ Scan(...any) error }) (*Promotion, error) {
	var p Promotion
	var plans, cycles string
	var starts, expires, created int64
	err := row.Scan(&p.ID, &p.Code, &p.Description, &p.Type, &p.Value, &plans, &cycles, &p.Recurring, &p.MaxUses, &p.Uses,
		&starts, &expires, &p.NewClientsOnly, &p.Enabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Plans, p.Cycles = splitList(plans), splitList(cycles)
	p.StartsAt, p.ExpiresAt, p.CreatedAt = unixTime(starts), unixTime(expires), unixTime(created)
	return &p, nil
}

// CreatePromotion adds a promotion (codes are unique: ErrExists).
func (s *Store) CreatePromotion(ctx context.Context, p *Promotion) (*Promotion, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO promotions (code, description, promo_type, value, plans, cycles, recurring,
		max_uses, starts_at, expires_at, new_clients_only, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`, p.Code, p.Description, p.Type, p.Value, strings.Join(p.Plans, ","), strings.Join(p.Cycles, ","),
		p.Recurring, p.MaxUses, unixOrZero(p.StartsAt), unixOrZero(p.ExpiresAt), p.NewClientsOnly, p.Enabled,
		time.Now().Unix()).Scan(&id)
	if isUnique(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	return s.GetPromotion(ctx, id)
}

// UpdatePromotion changes a promotion (not its use count).
func (s *Store) UpdatePromotion(ctx context.Context, p *Promotion) error {
	err := s.exec1(ctx, `UPDATE promotions SET code = ?, description = ?, promo_type = ?, value = ?, plans = ?, cycles = ?,
		recurring = ?, max_uses = ?, starts_at = ?, expires_at = ?, new_clients_only = ?, enabled = ? WHERE id = ?`,
		p.Code, p.Description, p.Type, p.Value, strings.Join(p.Plans, ","), strings.Join(p.Cycles, ","), p.Recurring,
		p.MaxUses, unixOrZero(p.StartsAt), unixOrZero(p.ExpiresAt), p.NewClientsOnly, p.Enabled, p.ID)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) DeletePromotion(ctx context.Context, id int64) error {
	return s.exec1(ctx, `DELETE FROM promotions WHERE id = ?`, id)
}

func (s *Store) GetPromotion(ctx context.Context, id int64) (*Promotion, error) {
	return scanPromo(s.db.QueryRowContext(ctx, `SELECT `+promoCols+` FROM promotions WHERE id = ?`, id))
}

// PromotionByCode finds a promotion by its code (stored upper case).
func (s *Store) PromotionByCode(ctx context.Context, code string) (*Promotion, error) {
	return scanPromo(s.db.QueryRowContext(ctx, `SELECT `+promoCols+` FROM promotions WHERE code = ?`, strings.ToUpper(code)))
}

func (s *Store) Promotions(ctx context.Context) ([]*Promotion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+promoCols+` FROM promotions ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Promotion{}
	for rows.Next() {
		p, err := scanPromo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// usePromotion counts one use of a promotion in tx, unless it is off or
// used up (ErrPromoUsedUp).
func usePromotion(ctx context.Context, tx *Tx, id int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE promotions SET uses = uses + 1 WHERE id = ? AND enabled = 1
		AND (max_uses = 0 OR uses < max_uses)`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPromoUsedUp
	}
	return nil
}

// UsePromotion counts one use of a promotion (ErrPromoUsedUp: none left).
func (s *Store) UsePromotion(ctx context.Context, id int64) error {
	return s.db.inTx(ctx, func(tx *Tx) error { return usePromotion(ctx, tx, id) })
}

// ---- Tax rules ----

// TaxRule is a tax applying to clients in a country (and state); "" is
// everywhere. Level 2 taxes may compound on level 1.
type TaxRule struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Country   string    `json:"country"`
	State     string    `json:"state"`
	Rate      int64     `json:"rate"`
	Level     int       `json:"level"`
	Compound  bool      `json:"compound"`
	CreatedAt time.Time `json:"-"`
}

const taxCols = `id, name, country, state, rate, level, compound, created_at`

func scanTax(row interface{ Scan(...any) error }) (*TaxRule, error) {
	var t TaxRule
	var created int64
	err := row.Scan(&t.ID, &t.Name, &t.Country, &t.State, &t.Rate, &t.Level, &t.Compound, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = unixTime(created)
	return &t, nil
}

func (s *Store) CreateTaxRule(ctx context.Context, t *TaxRule) (*TaxRule, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO tax_rules (name, country, state, rate, level, compound, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`, t.Name, t.Country, t.State, t.Rate, t.Level, t.Compound,
		time.Now().Unix()).Scan(&id); err != nil {
		return nil, err
	}
	return scanTax(s.db.QueryRowContext(ctx, `SELECT `+taxCols+` FROM tax_rules WHERE id = ?`, id))
}

func (s *Store) UpdateTaxRule(ctx context.Context, t *TaxRule) error {
	return s.exec1(ctx, `UPDATE tax_rules SET name = ?, country = ?, state = ?, rate = ?, level = ?, compound = ? WHERE id = ?`,
		t.Name, t.Country, t.State, t.Rate, t.Level, t.Compound, t.ID)
}

func (s *Store) DeleteTaxRule(ctx context.Context, id int64) error {
	return s.exec1(ctx, `DELETE FROM tax_rules WHERE id = ?`, id)
}

func (s *Store) GetTaxRule(ctx context.Context, id int64) (*TaxRule, error) {
	return scanTax(s.db.QueryRowContext(ctx, `SELECT `+taxCols+` FROM tax_rules WHERE id = ?`, id))
}

func (s *Store) TaxRules(ctx context.Context) ([]*TaxRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taxCols+` FROM tax_rules ORDER BY level, country, state, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TaxRule{}
	for rows.Next() {
		t, err := scanTax(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- Orders ----

// Order is an order from the public order form: the pending account it
// created and its first invoice.
type Order struct {
	ID        int64
	AccountID int64
	InvoiceID int64
	PlanID    string
	Cycle     string
	PromoCode string
	Total     int64
	Status    string
	IP        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const orderCols = `id, account_id, invoice_id, plan_id, cycle, promo_code, total, status, ip, created_at, updated_at`

func scanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	var created, updated int64
	err := row.Scan(&o.ID, &o.AccountID, &o.InvoiceID, &o.PlanID, &o.Cycle, &o.PromoCode, &o.Total, &o.Status, &o.IP,
		&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.CreatedAt, o.UpdatedAt = unixTime(created), unixTime(updated)
	return &o, nil
}

// NewOrder is everything an order creates, at once.
type NewOrder struct {
	Account *Account // Name, Kind, PlanID, Email
	User    *NewUser
	Profile *BillingProfile // AccountID is filled in
	Invoice *Invoice        // AccountID is filled in
	// Numbering numbers the invoice now (number on issue).
	Numbering *Numbering
	// PromoID: a promotion to count a use of (0: none).
	PromoID int64
	Order   *Order // PlanID, Cycle, PromoCode, Total, IP
	At      time.Time
}

// CreateOrder creates, in one transaction, a pending account, its first
// user, its billing profile, the order's invoice and the order: a taken
// username (ErrExists) or a promotion used up meanwhile (ErrPromoUsedUp)
// leaves nothing behind.
func (s *Store) CreateOrder(ctx context.Context, in *NewOrder) (*Order, error) {
	var orderID int64
	err := s.db.inTx(ctx, func(tx *Tx) error {
		now := in.At.Unix()
		var acctID int64
		a := in.Account
		if err := tx.QueryRowContext(ctx, `INSERT INTO accounts (name, kind, status, suspend_reason, plan_id, parent_id, email,
			whmcs_service_id, stripe_customer_id, stripe_subscription_id, created_at, updated_at)
			VALUES (?, ?, ?, '', ?, 0, ?, '', '', '', ?, ?) RETURNING id`, a.Name, a.Kind, AccountPending, a.PlanID, a.Email,
			now, now).Scan(&acctID); err != nil {
			return err
		}
		if in.User != nil {
			var uid int64
			err := tx.QueryRowContext(ctx, `INSERT INTO users (username, password, role, account_id, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, in.User.Username, in.User.PasswordHash, in.User.Role, acctID, now, now).Scan(&uid)
			if isUnique(err) {
				return ErrExists
			}
			if err != nil {
				return err
			}
		}
		p := *in.Profile
		p.AccountID = acctID
		contact, err := json.Marshal(p.Contact)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_profiles (account_id, mode, cycle, next_due_at, anchor_day,
			promo_id, contact, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, acctID, p.Mode, p.Cycle, unixOrZero(p.NextDueAt),
			p.AnchorDay, p.PromoID, string(contact), now); err != nil {
			return err
		}
		if in.PromoID != 0 {
			if err := usePromotion(ctx, tx, in.PromoID); err != nil {
				return err
			}
		}
		inv := *in.Invoice
		inv.AccountID = acctID
		invID, err := createInvoice(ctx, tx, &inv, in.Numbering)
		if err != nil {
			return err
		}
		o := in.Order
		return tx.QueryRowContext(ctx, `INSERT INTO orders (account_id, invoice_id, plan_id, cycle, promo_code, total, status,
			ip, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, acctID, invID, o.PlanID, o.Cycle,
			o.PromoCode, o.Total, OrderPending, o.IP, now, now).Scan(&orderID)
	})
	if err != nil {
		return nil, err
	}
	return s.GetOrder(ctx, orderID)
}

func (s *Store) GetOrder(ctx context.Context, id int64) (*Order, error) {
	return scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
}

// OrderByInvoice finds the order an invoice was made for.
func (s *Store) OrderByInvoice(ctx context.Context, invoiceID int64) (*Order, error) {
	return scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE invoice_id = ?`, invoiceID))
}

// Orders lists orders, newest first ("" status: all).
func (s *Store) Orders(ctx context.Context, status string, limit int) ([]*Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderCols+` FROM orders WHERE (? = '' OR status = ?) ORDER BY id DESC LIMIT ?`,
		status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetOrderStatus moves an order from one status to another; false when it
// wasn't in from (someone else moved it first).
func (s *Store) SetOrderStatus(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, time.Now().Unix(), id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- Automation runs ----

// InvoicingRun is one run of the billing automation.
type InvoicingRun struct {
	ID         int64          `json:"id"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Counts     map[string]int `json:"counts"`
	Errors     []string       `json:"errors"`
}

const keepInvoicingRuns = 100

// AddInvoicingRun records a run (the last 100 are kept).
func (s *Store) AddInvoicingRun(ctx context.Context, r *InvoicingRun) error {
	counts, err := json.Marshal(r.Counts)
	if err != nil {
		return err
	}
	errs := r.Errors
	if errs == nil {
		errs = []string{}
	}
	errJSON, err := json.Marshal(errs)
	if err != nil {
		return err
	}
	return s.db.inTx(ctx, func(tx *Tx) error {
		if err := tx.QueryRowContext(ctx, `INSERT INTO invoicing_runs (started_at, finished_at, counts, errors)
			VALUES (?, ?, ?, ?) RETURNING id`, r.StartedAt.Unix(), r.FinishedAt.Unix(), string(counts), string(errJSON)).Scan(&r.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM invoicing_runs WHERE id <=
			(SELECT id FROM invoicing_runs ORDER BY id DESC LIMIT 1 OFFSET ?)`, keepInvoicingRuns)
		return err
	})
}

// InvoicingRuns lists the latest runs, newest first.
func (s *Store) InvoicingRuns(ctx context.Context, limit int) ([]InvoicingRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, started_at, finished_at, counts, errors FROM invoicing_runs
		ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InvoicingRun{}
	for rows.Next() {
		var r InvoicingRun
		var started, finished int64
		var counts, errs string
		if err := rows.Scan(&r.ID, &started, &finished, &counts, &errs); err != nil {
			return nil, err
		}
		r.StartedAt, r.FinishedAt = unixTime(started), unixTime(finished)
		if err := json.Unmarshal([]byte(counts), &r.Counts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(errs), &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
