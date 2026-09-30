package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

// Built-in billing, WHMCS-style: plans have prices per billing cycle,
// accounts billed "by invoice" get renewal invoices ahead of their due
// date, pay them by card (Stripe), Razorpay or bank transfer, and are
// reminded, suspended and (optionally) terminated when they don't. Orders
// from the public order form create pending accounts that the first
// payment activates.
//
// Money is int64 minor units of the store's one currency; percentages are
// hundredths of a percent. Every total is computed here, server-side, and
// rounded half up once per tax line. Paying is idempotent (gateway
// references are recorded once) and serialised (payMu, then the store's
// transactions); the automation (RunInvoicing) can run any number of
// times: every step is keyed so it happens once.

// Billing modes of an account.
const (
	ModeNone               = "none"
	ModeInvoice            = "invoice"
	ModeStripeSubscription = "stripe_subscription"
	ModeWHMCS              = "whmcs"
)

// Invoice kinds and item kinds.
const (
	KindOrder      = "order"
	KindRenewal    = "renewal"
	KindPlanChange = "plan_change"
	KindOverage    = "overage"
	KindManual     = "manual"

	ItemPlan      = "plan"
	ItemSetup     = "setup"
	ItemDiscount  = "discount"
	ItemProration = "proration"
	ItemCredit    = "credit"
	ItemLateFee   = "late_fee"
	ItemOverage   = "overage"
	ItemCustom    = "custom"
)

// Payment methods (gateways) clients pay with, and how staff record the
// ones made outside.
const (
	MethodStripe   = "stripe"
	MethodRazorpay = "razorpay"
	MethodManual   = "manual"
)

// ManualMethods are what staff record a payment as.
var ManualMethods = []string{"bank", "cash", "cheque", "other"}

// SettingInvoicing holds InvoicingSettings (JSON, secrets included).
const SettingInvoicing = "billing_invoicing"

// Cycles are the billing cycles, shortest first, with their months.
var Cycles = []string{"monthly", "quarterly", "semiannually", "annually", "biennially", "triennially"}

var cycleMonths = map[string]int{"monthly": 1, "quarterly": 3, "semiannually": 6, "annually": 12, "biennially": 24,
	"triennially": 36}

// CycleMonths is a cycle's length in months (0: not a cycle).
func CycleMonths(cycle string) int { return cycleMonths[cycle] }

// Day is the start of t's UTC day: due dates and periods are whole days.
// A due date is a whole day (UTC): an invoice due on the 21st may be paid
// all that day, and is overdue from the 22nd. Automation's "N days after
// the due date" counts from the due date itself (the 26th for 5 days), and
// never acts before the invoice is overdue.
const dueGrace = 24 * time.Hour

// PastDue reports whether something due at due is overdue at now.
func PastDue(due, now time.Time) bool { return !due.IsZero() && !now.Before(due.Add(dueGrace)) }

// OverdueCutoff is the latest due date that is overdue at now (for the
// store's filters: due_at <= cutoff).
func OverdueCutoff(now time.Time) time.Time { return now.Add(-dueGrace) }

func Day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// AddMonths moves a due date by n whole months, onto the anchor day: the
// 31st falls on the last day of shorter months and comes back to the 31st
// when it can (Jan 31 → Feb 28 → Mar 31). anchor 0 is t's own day.
func AddMonths(t time.Time, n, anchor int) time.Time {
	t = Day(t)
	if anchor <= 0 {
		anchor = t.Day()
	}
	first := time.Date(t.Year(), t.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, 0, min(anchor, daysIn(first.Year(), first.Month()))-1)
}

// roundDiv is n/d rounded half away from zero (d > 0), exactly, however
// large n is.
func roundDiv(n, d *big.Int) int64 {
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if new(big.Int).Abs(new(big.Int).Lsh(r, 1)).Cmp(d) >= 0 {
		if n.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q.Int64()
}

// MulDiv is a×b/c rounded half up (away from zero), without overflow.
func MulDiv(a, b, c int64) int64 {
	return roundDiv(new(big.Int).Mul(big.NewInt(a), big.NewInt(b)), big.NewInt(c))
}

// maxAmount bounds any amount (10^12 minor units): far above any hosting
// bill, far below what int64 arithmetic on sums could overflow.
const maxAmount = 1_000_000_000_000

// ---- Settings ----

// Currency is the store's currency; Decimals is how many digits minor
// units have (2 for USD: 1500 is 15.00).
type Currency struct {
	Code     string `json:"code"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

// Company is who issues the invoices.
type Company struct {
	Name    string `json:"name"`
	Address string `json:"address"` // several lines
	Email   string `json:"email"`
	Phone   string `json:"phone"`
	TaxID   string `json:"tax_id"`
	Website string `json:"website"`
}

// InvoiceOptions is numbering and presentation. NextNumber is the
// sequence's (not stored with the rest).
type InvoiceOptions struct {
	Prefix        string `json:"prefix"`
	NextNumber    int64  `json:"next_number"`
	NumberOn      string `json:"number_on"` // issue | payment
	DaysBeforeDue int    `json:"days_before_due"`
	Footer        string `json:"footer"`
	TermsURL      string `json:"terms_url"`
}

type TaxOptions struct {
	Enabled bool `json:"enabled"`
	// Inclusive: prices include the tax, which invoices back out.
	Inclusive bool `json:"inclusive"`
	// ExemptWithTaxID: clients giving a tax ID pay no tax (reverse charge).
	ExemptWithTaxID bool `json:"exempt_with_tax_id"`
}

type LateFee struct {
	Type   string `json:"type"` // none | fixed | percent
	Amount int64  `json:"amount"`
}

// Automation is the dunning timeline, in days relative to the due date.
type Automation struct {
	ReminderDaysBefore  int   `json:"reminder_days_before"`
	OverdueReminderDays []int `json:"overdue_reminder_days"`
	// SuspendAfterDays: 0 suspends as soon as an invoice is overdue.
	SuspendAfterDays int `json:"suspend_after_days"`
	// TerminateAfterDays: 0 never terminates.
	TerminateAfterDays    int     `json:"terminate_after_days"`
	TerminateDeletesSites bool    `json:"terminate_deletes_sites"`
	LateFee               LateFee `json:"late_fee"`
	LateFeeAfterDays      int     `json:"late_fee_after_days"`
	AutoApplyCredit       bool    `json:"auto_apply_credit"`
	OrdersNeedApproval    bool    `json:"orders_need_approval"`
	Autocharge            bool    `json:"autocharge"`
}

type StripeMethod struct {
	Enabled bool   `json:"enabled"`
	Name    string `json:"name"`
}

// RazorpayMethod: the secrets are write-only (nil keeps, "" clears; the
// API shows *_set).
type RazorpayMethod struct {
	Enabled          bool    `json:"enabled"`
	Name             string  `json:"name"`
	KeyID            string  `json:"key_id"`
	KeySecret        *string `json:"key_secret,omitempty"`
	WebhookSecret    *string `json:"webhook_secret,omitempty"`
	KeySecretSet     bool    `json:"key_secret_set"`
	WebhookSecretSet bool    `json:"webhook_secret_set"`
	WebhookURL       string  `json:"webhook_url"`
}

type ManualMethod struct {
	Enabled      bool   `json:"enabled"`
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}

type Methods struct {
	Stripe   StripeMethod   `json:"stripe"`
	Razorpay RazorpayMethod `json:"razorpay"`
	Manual   ManualMethod   `json:"manual"`
}

// BurstPack is a pack of burst minutes clients can buy (see burstpacks.go).
type BurstPack struct {
	ID      string `json:"id"`
	Minutes int64  `json:"minutes"`
	Price   int64  `json:"price"`
}

// InvoicingSettings configure built-in billing. StripeReady is output
// only: Stripe's keys live in the Stripe settings.
type InvoicingSettings struct {
	Enabled     bool           `json:"enabled"`
	Currency    Currency       `json:"currency"`
	Company     Company        `json:"company"`
	Invoice     InvoiceOptions `json:"invoice"`
	Tax         TaxOptions     `json:"tax"`
	Automation  Automation     `json:"automation"`
	Methods     Methods        `json:"methods"`
	BurstPacks  []BurstPack    `json:"burst_packs"`
	StripeReady bool           `json:"stripe_ready"`
}

// DefaultInvoicing is what a panel starts with: off, USD, invoices numbered
// when issued a week before they are due, and a dunning timeline most
// hosts use.
func DefaultInvoicing() InvoicingSettings {
	return InvoicingSettings{
		Currency: Currency{Code: "USD", Symbol: "$", Decimals: 2},
		Invoice:  InvoiceOptions{Prefix: "INV-", NumberOn: "issue", DaysBeforeDue: 7},
		Automation: Automation{ReminderDaysBefore: 3, OverdueReminderDays: []int{1, 3, 7}, SuspendAfterDays: 5,
			LateFee: LateFee{Type: "none"}, LateFeeAfterDays: 3, AutoApplyCredit: true, Autocharge: true},
		Methods: Methods{Stripe: StripeMethod{Name: "Card"},
			Razorpay: RazorpayMethod{Name: "UPI, cards & netbanking"},
			Manual:   ManualMethod{Enabled: true, Name: "Bank transfer"}},
	}
}

// Invoicing returns the settings, secrets included.
func (s *Service) Invoicing(ctx context.Context) (*InvoicingSettings, error) {
	cfg := DefaultInvoicing()
	v, err := s.Store.Setting(ctx, SettingInvoicing)
	if err != nil {
		return nil, err
	}
	if v != "" {
		if err := json.Unmarshal([]byte(v), &cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Invoice.NextNumber, err = s.Store.NextInvoiceNumber(ctx); err != nil {
		return nil, err
	}
	if cfg.BurstPacks == nil {
		cfg.BurstPacks = []BurstPack{}
	}
	stripe, err := s.StripeSettings(ctx)
	if err != nil {
		return nil, err
	}
	cfg.StripeReady = stripe.SecretKey != "" && stripe.WebhookSecret != ""
	cfg.Methods.Razorpay.KeySecretSet = str(cfg.Methods.Razorpay.KeySecret) != ""
	cfg.Methods.Razorpay.WebhookSecretSet = str(cfg.Methods.Razorpay.WebhookSecret) != ""
	cfg.Methods.Razorpay.WebhookURL = s.PanelURL + "/api/v1/billing/razorpay/webhook"
	return &cfg, nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Redacted is the settings as the API shows them: secrets blanked.
func (c InvoicingSettings) Redacted() InvoicingSettings {
	c.Methods.Razorpay.KeySecret, c.Methods.Razorpay.WebhookSecret = nil, nil
	return c
}

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	prefixRe   = regexp.MustCompile(`^[A-Za-z0-9/_. -]{0,20}$`)
	rzpKeyRe   = regexp.MustCompile(`^rzp_(test|live)_[A-Za-z0-9]{6,40}$`)
)

// textField checks free text: at most n characters, no control
// characters but line breaks when multiline.
func textField(name, v string, n int, multiline bool) error {
	if utf8.RuneCountInString(v) > n || strings.IndexFunc(v, func(r rune) bool {
		return unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\r' || r == '\t'))
	}) >= 0 {
		return fmt.Errorf("%w: %s must be at most %d characters%s", ErrInvalid, name, n, map[bool]string{false: " on one line", true: ""}[multiline])
	}
	return nil
}

func httpsURL(name, v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(v) > 500 {
		return fmt.Errorf("%w: %s must be an http(s) URL", ErrInvalid, name)
	}
	return nil
}

// SetInvoicing validates and stores new settings. Secrets left nil keep
// their stored value; NextNumber moves the invoice sequence when changed.
func (s *Service) SetInvoicing(ctx context.Context, in InvoicingSettings) (*InvoicingSettings, error) {
	cur, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	c := in
	c.Currency.Code = strings.ToUpper(strings.TrimSpace(c.Currency.Code))
	c.Company.Name, c.Company.Email = strings.TrimSpace(c.Company.Name), strings.TrimSpace(c.Company.Email)
	rz := &c.Methods.Razorpay
	if rz.KeySecret == nil {
		rz.KeySecret = cur.Methods.Razorpay.KeySecret
	}
	if rz.WebhookSecret == nil {
		rz.WebhookSecret = cur.Methods.Razorpay.WebhookSecret
	}
	rz.KeyID = strings.TrimSpace(rz.KeyID)
	if c.Invoice.NumberOn == "" {
		c.Invoice.NumberOn = "issue"
	}
	if c.Automation.LateFee.Type == "" {
		c.Automation.LateFee.Type = "none"
	}
	a := &c.Automation
	days := slices.Clone(a.OverdueReminderDays)
	slices.Sort(days)
	a.OverdueReminderDays = slices.Compact(days)
	if a.OverdueReminderDays == nil {
		a.OverdueReminderDays = []int{}
	}
	checks := []error{
		textField("company name", c.Company.Name, 100, false), textField("company address", c.Company.Address, 500, true),
		textField("company phone", c.Company.Phone, 50, false), textField("company tax ID", c.Company.TaxID, 50, false),
		httpsURL("company website", c.Company.Website), textField("invoice footer", c.Invoice.Footer, 1000, true),
		httpsURL("terms URL", c.Invoice.TermsURL), textField("method name", c.Methods.Stripe.Name, 50, false),
		textField("method name", rz.Name, 50, false), textField("method name", c.Methods.Manual.Name, 50, false),
		textField("bank transfer instructions", c.Methods.Manual.Instructions, 2000, true),
	}
	if err := errors.Join(checks...); err != nil {
		return nil, err
	}
	switch {
	case !currencyRe.MatchString(c.Currency.Code):
		return nil, fmt.Errorf("%w: the currency is a three-letter ISO code (USD, EUR, INR…)", ErrInvalid)
	case utf8.RuneCountInString(c.Currency.Symbol) > 8 || strings.ContainsAny(c.Currency.Symbol, "<>&\"'\n"):
		return nil, fmt.Errorf("%w: the currency symbol is at most 8 characters", ErrInvalid)
	case c.Currency.Decimals < 0 || c.Currency.Decimals > 3:
		return nil, fmt.Errorf("%w: a currency has 0 to 3 decimals", ErrInvalid)
	case c.Company.Email != "" && !mailer.ValidAddress(c.Company.Email):
		return nil, fmt.Errorf("%w: the company e-mail isn't an address", ErrInvalid)
	case !prefixRe.MatchString(c.Invoice.Prefix):
		return nil, fmt.Errorf("%w: the invoice prefix is up to 20 letters, digits and - / _ .", ErrInvalid)
	case c.Invoice.NumberOn != "issue" && c.Invoice.NumberOn != "payment":
		return nil, fmt.Errorf("%w: number_on is issue or payment", ErrInvalid)
	case c.Invoice.DaysBeforeDue < 0 || c.Invoice.DaysBeforeDue > 60:
		return nil, fmt.Errorf("%w: invoices are made 0 to 60 days before they are due", ErrInvalid)
	case a.ReminderDaysBefore < 0 || a.ReminderDaysBefore > 30:
		return nil, fmt.Errorf("%w: the reminder goes 0 to 30 days before the due date", ErrInvalid)
	case len(a.OverdueReminderDays) > 10 || (len(a.OverdueReminderDays) > 0 &&
		(a.OverdueReminderDays[0] < 1 || a.OverdueReminderDays[len(a.OverdueReminderDays)-1] > 365)):
		return nil, fmt.Errorf("%w: up to 10 overdue reminders, 1 to 365 days after the due date", ErrInvalid)
	case a.SuspendAfterDays < 0 || a.SuspendAfterDays > 365:
		return nil, fmt.Errorf("%w: suspension comes 0 to 365 days after the due date", ErrInvalid)
	case a.TerminateAfterDays < 0 || a.TerminateAfterDays > 730 || (a.TerminateAfterDays > 0 && a.TerminateAfterDays < a.SuspendAfterDays):
		return nil, fmt.Errorf("%w: termination comes 0 (never) or up to 730 days after the due date, not before suspension", ErrInvalid)
	case a.LateFee.Type != "none" && a.LateFee.Type != "fixed" && a.LateFee.Type != "percent":
		return nil, fmt.Errorf("%w: the late fee is none, fixed or percent", ErrInvalid)
	case a.LateFee.Amount < 0 || a.LateFee.Amount > maxAmount || (a.LateFee.Type == "percent" && a.LateFee.Amount > 10000):
		return nil, fmt.Errorf("%w: invalid late fee amount", ErrInvalid)
	case a.LateFeeAfterDays < 0 || a.LateFeeAfterDays > 365:
		return nil, fmt.Errorf("%w: the late fee comes 0 to 365 days after the due date", ErrInvalid)
	case rz.KeyID != "" && !rzpKeyRe.MatchString(rz.KeyID):
		return nil, fmt.Errorf("%w: the Razorpay key ID starts with rzp_test_ or rzp_live_", ErrInvalid)
	case str(rz.KeySecret) != "" && !printableRe.MatchString(str(rz.KeySecret)),
		str(rz.WebhookSecret) != "" && !printableRe.MatchString(str(rz.WebhookSecret)):
		return nil, fmt.Errorf("%w: Razorpay secrets are printable characters without spaces", ErrInvalid)
	case rz.Enabled && (rz.KeyID == "" || str(rz.KeySecret) == ""):
		return nil, fmt.Errorf("%w: enter the Razorpay key ID and secret to enable it", ErrInvalid)
	}
	for _, d := range a.OverdueReminderDays {
		if d < 1 || d > 365 {
			return nil, fmt.Errorf("%w: overdue reminders go 1 to 365 days after the due date", ErrInvalid)
		}
	}
	if err := validPacks(c.BurstPacks); err != nil {
		return nil, err
	}
	if c.Invoice.NextNumber != 0 && c.Invoice.NextNumber != cur.Invoice.NextNumber {
		if c.Invoice.NextNumber < 1 {
			return nil, fmt.Errorf("%w: the next invoice number is at least 1", ErrInvalid)
		}
		if err := s.Store.SetNextInvoiceNumber(ctx, c.Invoice.NextNumber, c.Invoice.Prefix); errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		} else if err != nil {
			return nil, err
		}
	}
	// Output-only fields aren't stored.
	c.Invoice.NextNumber, c.StripeReady = 0, false
	rz.KeySecretSet, rz.WebhookSecretSet, rz.WebhookURL = false, false, ""
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetSetting(ctx, SettingInvoicing, string(b)); err != nil {
		return nil, err
	}
	return s.Invoicing(ctx)
}

// ---- What clients see ----

// MethodInfo is a payment method offered to clients.
type MethodInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PayMethods are the methods clients can pay with now: enabled and
// configured.
func (c *InvoicingSettings) PayMethods() []MethodInfo {
	out := []MethodInfo{}
	if c.Methods.Stripe.Enabled && c.StripeReady {
		out = append(out, MethodInfo{MethodStripe, c.Methods.Stripe.Name, "Pay securely by card; you can save it for next time."})
	}
	if c.Methods.Razorpay.Enabled && c.Methods.Razorpay.KeyID != "" && str(c.Methods.Razorpay.KeySecret) != "" {
		out = append(out, MethodInfo{MethodRazorpay, c.Methods.Razorpay.Name, "Pay with UPI, cards, netbanking or wallets through Razorpay."})
	}
	if c.Methods.Manual.Enabled {
		out = append(out, MethodInfo{MethodManual, c.Methods.Manual.Name, "Pay by transfer; the details are shown with the invoice."})
	}
	return out
}

func (c *InvoicingSettings) methodIDs() []string {
	out := []string{}
	for _, m := range c.PayMethods() {
		out = append(out, m.ID)
	}
	return out
}

// ClientConfig is what the client area needs to show prices and offer
// payment methods.
type ClientConfig struct {
	Enabled      bool         `json:"enabled"`
	Currency     Currency     `json:"currency"`
	CompanyName  string       `json:"company_name"`
	Methods      []MethodInfo `json:"methods"`
	TaxInclusive bool         `json:"tax_inclusive"`
	TermsURL     string       `json:"terms_url"`
	BurstPacks   []BurstPack  `json:"burst_packs"`
}

func (s *Service) ClientConfig(ctx context.Context) (*ClientConfig, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	c := &ClientConfig{Enabled: cfg.Enabled, Currency: cfg.Currency, CompanyName: cfg.Company.Name,
		Methods: cfg.PayMethods(), TaxInclusive: cfg.Tax.Enabled && cfg.Tax.Inclusive, TermsURL: cfg.Invoice.TermsURL,
		BurstPacks: []BurstPack{}}
	if cfg.Enabled {
		c.BurstPacks = cfg.BurstPacks
	}
	return c, nil
}

// MailBrand is the e-mail layout's branding from the company settings,
// when built-in billing is on and names a company (a zero Brand
// otherwise: the mailer falls back to the panel's own).
func (s *Service) MailBrand(ctx context.Context) mailer.Brand {
	cfg, err := s.Invoicing(ctx)
	if err != nil || !cfg.Enabled || cfg.Company.Name == "" {
		return mailer.Brand{}
	}
	footer := strings.Join(strings.Fields(strings.ReplaceAll(cfg.Company.Address, "\n", " · ")), " ")
	if cfg.Company.TaxID != "" {
		footer = strings.TrimPrefix(footer+" · Tax ID "+cfg.Company.TaxID, " · ")
	}
	return mailer.Brand{Name: cfg.Company.Name, URL: cfg.Company.Website, Footer: footer}
}

// FormatMoney writes an amount for people: symbol, thousands separators,
// the currency's decimals ("$1,234.50", "-₹99.00").
func FormatMoney(amount int64, c Currency) string {
	neg := amount < 0
	if neg {
		amount = -amount
	}
	div := int64(1)
	for range c.Decimals {
		div *= 10
	}
	whole, frac := amount/div, amount%div
	digits := strconv.FormatInt(whole, 10)
	var b strings.Builder
	for i, ch := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	out := b.String()
	if c.Decimals > 0 {
		out += "." + fmt.Sprintf("%0*d", c.Decimals, frac)
	}
	sym := c.Symbol
	if sym == "" {
		sym = c.Code + " "
	}
	if neg {
		return "-" + sym + out
	}
	return sym + out
}
