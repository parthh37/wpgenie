package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

// ---- Billing profiles ----

// CountriesNeedState are the countries whose clients must give a state
// (taxes differ by state there).
var CountriesNeedState = []string{"US", "CA", "IN", "AU"}

// FieldError is invalid input in one field ("contact.email"), for forms
// to show next to it.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Msg }
func (e *FieldError) Unwrap() error { return ErrInvalid }

func fieldErr(field, format string, args ...any) error {
	return &FieldError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// profile returns an account's billing profile, or its defaults.
func (s *Service) profile(ctx context.Context, accountID int64) (*store.BillingProfile, error) {
	p, err := s.Store.GetBillingProfile(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return &store.BillingProfile{AccountID: accountID, Cycle: "monthly"}, nil
	}
	if err != nil {
		return nil, err
	}
	if p.Cycle == "" {
		p.Cycle = "monthly"
	}
	return p, nil
}

// ModeOf is an account's billing mode: its profile's, else what its
// external IDs say.
func ModeOf(a *store.Account, p *store.BillingProfile) string {
	if p != nil && p.Mode != "" {
		return p.Mode
	}
	switch {
	case a.StripeSubscriptionID != "":
		return ModeStripeSubscription
	case a.WHMCSServiceID != "":
		return ModeWHMCS
	}
	return ModeNone
}

// recurringPrice is what an account pays per cycle: its override, else
// the plan's price for the cycle (ok false: the plan has none).
func recurringPrice(plan *store.Plan, p *store.BillingProfile) (int64, bool) {
	if p.PriceOverride != nil {
		return *p.PriceOverride, true
	}
	pr, ok := plan.Prices[p.Cycle]
	return pr.Price, ok
}

// Card is a saved card as the API shows it.
type Card struct {
	Brand    string `json:"brand"`
	Last4    string `json:"last4"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

// Upcoming is the next renewal.
type Upcoming struct {
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	Amount      int64     `json:"amount"`
}

// ProfileView is an account's billing as the API shows it.
type ProfileView struct {
	Mode            string               `json:"mode"`
	Cycle           string               `json:"cycle"`
	PriceOverride   *int64               `json:"price_override"`
	NextDueAt       *time.Time           `json:"next_due_at"`
	AnchorDay       int                  `json:"anchor_day"`
	AutoPay         bool                 `json:"auto_pay"`
	Card            *Card                `json:"card"`
	Credit          int64                `json:"credit"`
	TaxExempt       bool                 `json:"tax_exempt"`
	CancelAt        *time.Time           `json:"cancel_at"`
	CancelReason    string               `json:"cancel_reason"`
	Contact         store.BillingContact `json:"contact"`
	BalanceDue      int64                `json:"balance_due"`
	Overdue         bool                 `json:"overdue"`
	RecurringAmount int64                `json:"recurring_amount"`
	Upcoming        *Upcoming            `json:"upcoming"`
}

// Profile returns an account's billing.
func (s *Service) Profile(ctx context.Context, accountID int64) (*ProfileView, error) {
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	plan, err := s.Store.GetPlan(ctx, a.PlanID)
	if err != nil {
		return nil, err
	}
	v := &ProfileView{Mode: ModeOf(a, p), Cycle: p.Cycle, PriceOverride: p.PriceOverride, NextDueAt: tptr(p.NextDueAt),
		AnchorDay: p.AnchorDay, AutoPay: p.AutoPay, Credit: p.Credit, TaxExempt: p.TaxExempt, CancelAt: tptr(p.CancelAt),
		CancelReason: p.CancelReason, Contact: p.Contact}
	if p.CardPM != "" {
		v.Card = &Card{Brand: p.CardBrand, Last4: p.CardLast4, ExpMonth: p.CardExpMonth, ExpYear: p.CardExpYear}
	}
	if price, ok := recurringPrice(plan, p); ok {
		v.RecurringAmount = price
	}
	unpaid, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{AccountID: accountID, Status: store.InvoiceUnpaid})
	if err != nil {
		return nil, err
	}
	now := s.now()
	for _, inv := range unpaid {
		v.BalanceDue += max(inv.Balance(), 0)
		if !inv.DueAt.IsZero() && inv.DueAt.Before(now) {
			v.Overdue = true
		}
	}
	if v.Mode == ModeInvoice && !p.NextDueAt.IsZero() && v.RecurringAmount > 0 {
		end := AddMonths(p.NextDueAt, CycleMonths(p.Cycle), p.AnchorDay)
		amount := v.RecurringAmount
		if promo := s.recurringPromo(ctx, p, a.PlanID); promo != nil {
			amount -= promoDiscount(promo, amount)
		}
		v.Upcoming = &Upcoming{PeriodStart: p.NextDueAt, PeriodEnd: end, Amount: amount}
	}
	return v, nil
}

// recurringPromo is the account's recurring promotion when it still
// applies to its plan and cycle.
func (s *Service) recurringPromo(ctx context.Context, p *store.BillingProfile, planID string) *store.Promotion {
	if p.PromoID == 0 {
		return nil
	}
	promo, err := s.Store.GetPromotion(ctx, p.PromoID)
	if err != nil || !promo.Enabled || !promo.Recurring || !promoApplies(promo, planID, p.Cycle) {
		return nil
	}
	return promo
}

// OptionalAmount is a JSON field that may be absent, null or an amount.
type OptionalAmount struct {
	Set   bool
	Value *int64
}

func (o *OptionalAmount) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// ProfileInput is what staff change in an account's billing (nil: kept).
type ProfileInput struct {
	Mode          *string        `json:"mode"`
	Cycle         *string        `json:"cycle"`
	PriceOverride OptionalAmount `json:"price_override"`
	NextDueAt     *string        `json:"next_due_at"`
	TaxExempt     *bool          `json:"tax_exempt"`
	AutoPay       *bool          `json:"auto_pay"`
}

// ParseDate reads a date given as RFC 3339 or YYYY-MM-DD, as that UTC day.
func ParseDate(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return Day(t), nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: dates are YYYY-MM-DD or RFC 3339", ErrInvalid)
	}
	return t, nil
}

// UpdateProfile changes how an account is billed (staff).
func (s *Service) UpdateProfile(ctx context.Context, accountID int64, in ProfileInput) (*ProfileView, error) {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	oldMode := ModeOf(a, p)
	if in.Mode != nil {
		if !slices.Contains([]string{ModeNone, ModeInvoice, ModeStripeSubscription, ModeWHMCS}, *in.Mode) {
			return nil, fmt.Errorf("%w: mode must be none, invoice, stripe_subscription or whmcs", ErrInvalid)
		}
		p.Mode = *in.Mode
	}
	if in.Cycle != nil {
		if CycleMonths(*in.Cycle) == 0 {
			return nil, fmt.Errorf("%w: cycle must be one of %s", ErrInvalid, strings.Join(Cycles, ", "))
		}
		p.Cycle = *in.Cycle
	}
	if in.PriceOverride.Set {
		if v := in.PriceOverride.Value; v != nil && (*v < 0 || *v > maxAmount) {
			return nil, fmt.Errorf("%w: invalid price override", ErrInvalid)
		}
		p.PriceOverride = in.PriceOverride.Value
	}
	if in.NextDueAt != nil {
		if *in.NextDueAt == "" {
			p.NextDueAt = time.Time{}
		} else {
			t, err := ParseDate(*in.NextDueAt)
			if err != nil {
				return nil, err
			}
			p.NextDueAt, p.AnchorDay = t, t.Day()
		}
	}
	if in.TaxExempt != nil {
		p.TaxExempt = *in.TaxExempt
	}
	if in.AutoPay != nil {
		if *in.AutoPay && p.CardPM == "" {
			return nil, fmt.Errorf("%w: automatic payment needs a saved card", ErrInvalid)
		}
		p.AutoPay = *in.AutoPay
	}
	mode := ModeOf(a, p)
	if mode == ModeInvoice {
		if a.ParentID != 0 {
			return nil, fmt.Errorf("%w: a reseller's customers are billed by their reseller, not by invoice", ErrInvalid)
		}
		plan, err := s.Store.GetPlan(ctx, a.PlanID)
		if err != nil {
			return nil, err
		}
		if _, ok := recurringPrice(plan, p); !ok {
			return nil, fmt.Errorf("%w: plan %s has no %s price: set a price override or choose a cycle it has", ErrInvalid,
				plan.ID, p.Cycle)
		}
		if p.NextDueAt.IsZero() {
			// Billed from today: the first renewal invoice covers today on.
			p.NextDueAt = Day(s.now())
			p.AnchorDay = p.NextDueAt.Day()
		}
	}
	if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
		return nil, err
	}
	if mode != oldMode {
		s.event(ctx, accountID, "billing", "Billing mode changed from "+oldMode+" to "+mode)
	}
	return s.Profile(ctx, accountID)
}

var postcodeRe = regexp.MustCompile(`^[A-Za-z0-9 -]{0,20}$`)

// NormalizeContact validates a billing contact (email required when
// requireEmail) and puts it in canonical form.
func NormalizeContact(c *store.BillingContact, requireEmail bool) error {
	for _, f := range []*string{&c.FirstName, &c.LastName, &c.Company, &c.Email, &c.Phone, &c.Address1, &c.Address2,
		&c.City, &c.State, &c.Postcode, &c.Country, &c.TaxID} {
		*f = strings.TrimSpace(*f)
	}
	c.Country = strings.ToUpper(c.Country)
	fields := []struct {
		name, v string
		n       int
	}{{"first_name", c.FirstName, 100}, {"last_name", c.LastName, 100}, {"company", c.Company, 100},
		{"phone", c.Phone, 30}, {"address1", c.Address1, 200}, {"address2", c.Address2, 200}, {"city", c.City, 100},
		{"state", c.State, 50}, {"tax_id", c.TaxID, 50}}
	for _, f := range fields {
		if textField(f.name, f.v, f.n, false) != nil {
			return fieldErr("contact."+f.name, "%s: at most %d characters on one line", strings.ReplaceAll(f.name, "_", " "), f.n)
		}
	}
	switch {
	case c.Email == "" && requireEmail:
		return fieldErr("contact.email", "enter an e-mail address")
	case c.Email != "" && !mailer.ValidAddress(c.Email):
		return fieldErr("contact.email", "that isn't an e-mail address")
	case c.Country != "" && !countryRe.MatchString(c.Country):
		return fieldErr("contact.country", "choose a country")
	case c.State == "" && slices.Contains(CountriesNeedState, c.Country):
		return fieldErr("contact.state", "enter your state or province")
	case !postcodeRe.MatchString(c.Postcode):
		return fieldErr("contact.postcode", "the postal code is up to 20 letters and digits")
	}
	return nil
}

// UpdateContact changes an account's billing contact.
func (s *Service) UpdateContact(ctx context.Context, accountID int64, c store.BillingContact) (*ProfileView, error) {
	if err := NormalizeContact(&c, false); err != nil {
		return nil, err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	if _, err := s.Store.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	p.Contact = c
	if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
		return nil, err
	}
	return s.Profile(ctx, accountID)
}

// SetAutoPay turns paying invoices with the saved card on or off.
func (s *Service) SetAutoPay(ctx context.Context, accountID int64, on bool) (*ProfileView, error) {
	return s.UpdateProfile(ctx, accountID, ProfileInput{AutoPay: &on})
}

// ForgetCard removes the saved card (detached at Stripe when it can be).
func (s *Service) ForgetCard(ctx context.Context, accountID int64) (*ProfileView, error) {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if p.CardPM == "" {
		return s.Profile(ctx, accountID)
	}
	if cfg, err := s.StripeSettings(ctx); err == nil && cfg.SecretKey != "" && s.Stripe != nil {
		// Forgotten here whatever Stripe says: that is what was asked.
		if err := s.Stripe.DetachPaymentMethod(ctx, cfg.SecretKey, p.CardPM); err != nil {
			s.Log.Warn("detaching a card at Stripe", "account", accountID, "err", err)
		}
	}
	p.CardPM, p.CardBrand, p.CardLast4, p.CardExpMonth, p.CardExpYear, p.AutoPay = "", "", "", 0, 0, false
	if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
		return nil, err
	}
	s.event(ctx, accountID, "billing", "Saved card removed")
	return s.Profile(ctx, accountID)
}

// billingAddress is an invoice's "bill to".
func billingAddress(a *store.Account, p *store.BillingProfile) store.BillingAddress {
	c := p.Contact
	name := strings.TrimSpace(c.FirstName + " " + c.LastName)
	if name == "" {
		name = a.Name
	}
	var lines []string
	for _, l := range []string{c.Address1, c.Address2,
		strings.Join(strings.Fields(strings.Join([]string{c.City, c.State, c.Postcode}, " ")), " ")} {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if lines == nil {
		lines = []string{}
	}
	email := c.Email
	if email == "" {
		email = a.Email
	}
	return store.BillingAddress{Name: name, Company: c.Company, Lines: lines, Country: c.Country, TaxID: c.TaxID, Email: email}
}

// ---- Credit ----

// CreditHistory lists an account's credit ledger.
func (s *Service) CreditHistory(ctx context.Context, accountID int64) ([]store.CreditEntry, error) {
	if _, err := s.Store.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	return s.Store.CreditEntries(ctx, accountID, 500)
}

// AddCredit adds credit to an account (negative removes it; never below
// zero) and tells the client when it is added.
func (s *Service) AddCredit(ctx context.Context, accountID, amount int64, desc, by string) (*store.CreditEntry, error) {
	desc = strings.TrimSpace(desc)
	switch {
	case amount == 0 || amount > maxAmount || amount < -maxAmount:
		return nil, fmt.Errorf("%w: the amount is a non-zero amount in minor units", ErrInvalid)
	case textField("description", desc, 200, false) != nil:
		return nil, fmt.Errorf("%w: the description is at most 200 characters on one line", ErrInvalid)
	}
	if desc == "" {
		desc = map[bool]string{true: "Credit added", false: "Credit removed"}[amount > 0]
	}
	if _, err := s.Store.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	e, _, err := s.Store.AddCredit(ctx, accountID, amount, desc, by, "", s.now())
	if errors.Is(err, store.ErrInsufficientCredit) {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err != nil {
		return nil, err
	}
	s.event(ctx, accountID, "billing", fmt.Sprintf("Credit %s: %s (%s)", FormatMoney(amount, cfg.Currency), desc, by))
	if amount > 0 {
		s.mail(ctx, accountID, "credit.added", fmt.Sprintf("credit.added:%d", e.ID), map[string]any{"Credit": map[string]any{
			"Amount": FormatMoney(amount, cfg.Currency), "Balance": FormatMoney(e.Balance, cfg.Currency), "Description": desc}})
	}
	return e, nil
}

// ---- Invoices: views ----

// InvoiceView is an invoice in a list.
type InvoiceView struct {
	ID             int64           `json:"id"`
	Number         string          `json:"number"`
	AccountID      int64           `json:"account_id"`
	AccountName    string          `json:"account_name"`
	Kind           string          `json:"kind"`
	Status         string          `json:"status"`
	Overdue        bool            `json:"overdue"`
	Currency       string          `json:"currency"`
	IssuedAt       *time.Time      `json:"issued_at"`
	DueAt          *time.Time      `json:"due_at"`
	PaidAt         *time.Time      `json:"paid_at"`
	PeriodStart    *time.Time      `json:"period_start"`
	PeriodEnd      *time.Time      `json:"period_end"`
	Subtotal       int64           `json:"subtotal"`
	Discount       int64           `json:"discount"`
	TaxLines       []store.TaxLine `json:"tax_lines"`
	Tax            int64           `json:"tax"`
	Total          int64           `json:"total"`
	CreditApplied  int64           `json:"credit_applied"`
	AmountPaid     int64           `json:"amount_paid"`
	AmountRefunded int64           `json:"amount_refunded"`
	Balance        int64           `json:"balance"`
	Notes          string          `json:"notes"`
	PayMethods     []string        `json:"pay_methods"`
}

// InvoiceDetail is one invoice with its lines, address and payments.
type InvoiceDetail struct {
	InvoiceView
	Items          []store.InvoiceItem  `json:"items"`
	BillingAddress store.BillingAddress `json:"billing_address"`
	Payments       []store.Payment      `json:"payments"`
}

// displayNumber is an invoice's number, or what stands for it until it
// has one.
func displayNumber(inv *store.Invoice) string {
	switch {
	case inv.Number != "":
		return inv.Number
	case inv.Status == store.InvoiceDraft:
		return fmt.Sprintf("Draft #%d", inv.ID)
	}
	return fmt.Sprintf("Proforma #%d", inv.ID)
}

func (s *Service) invoiceView(cfg *InvoicingSettings, inv *store.Invoice, now time.Time) InvoiceView {
	v := InvoiceView{ID: inv.ID, Number: displayNumber(inv), AccountID: inv.AccountID, AccountName: inv.AccountName,
		Kind: inv.Kind, Status: inv.Status, Currency: inv.Currency, IssuedAt: tptr(inv.IssuedAt), DueAt: tptr(inv.DueAt),
		PaidAt: tptr(inv.PaidAt), PeriodStart: tptr(inv.PeriodStart), PeriodEnd: tptr(inv.PeriodEnd), Subtotal: inv.Subtotal,
		Discount: inv.Discount, TaxLines: inv.TaxLines, Tax: inv.Tax, Total: inv.Total, CreditApplied: inv.CreditApplied,
		AmountPaid: inv.AmountPaid, AmountRefunded: inv.AmountRefunded, Notes: inv.Notes, PayMethods: []string{}}
	if v.TaxLines == nil {
		v.TaxLines = []store.TaxLine{}
	}
	if inv.Status == store.InvoiceUnpaid {
		v.Balance = max(inv.Balance(), 0)
		v.Overdue = !inv.DueAt.IsZero() && inv.DueAt.Before(now)
		if v.Balance > 0 {
			v.PayMethods = cfg.methodIDs()
		}
	}
	return v
}

func (s *Service) invoiceDetail(cfg *InvoicingSettings, inv *store.Invoice) *InvoiceDetail {
	d := &InvoiceDetail{InvoiceView: s.invoiceView(cfg, inv, s.now()), Items: inv.Items, BillingAddress: inv.BillingAddress,
		Payments: inv.Payments}
	if d.Items == nil {
		d.Items = []store.InvoiceItem{}
	}
	if d.Payments == nil {
		d.Payments = []store.Payment{}
	}
	if d.BillingAddress.Lines == nil {
		d.BillingAddress.Lines = []string{}
	}
	return d
}

// Invoice returns one invoice.
func (s *Service) Invoice(ctx context.Context, id int64) (*InvoiceDetail, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	inv, err := s.Store.GetInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.invoiceDetail(cfg, inv), nil
}

// RawInvoice is the stored invoice (the printable page renders it).
func (s *Service) RawInvoice(ctx context.Context, id int64) (*store.Invoice, error) {
	return s.Store.GetInvoice(ctx, id)
}

// Invoices lists invoices.
func (s *Service) Invoices(ctx context.Context, f store.InvoiceFilter) ([]InvoiceView, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListInvoices(ctx, f)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]InvoiceView, len(list))
	for i, inv := range list {
		out[i] = s.invoiceView(cfg, inv, now)
	}
	return out, nil
}

// ---- Invoices: making them ----

func issueNumbering(cfg *InvoicingSettings) *store.Numbering {
	if cfg.Invoice.NumberOn == "issue" {
		return &store.Numbering{Prefix: cfg.Invoice.Prefix}
	}
	return nil
}

// payNumbering numbers an invoice when it is paid if it has no number yet
// (number on payment, or issued before the setting changed).
func payNumbering(cfg *InvoicingSettings) *store.Numbering {
	return &store.Numbering{Prefix: cfg.Invoice.Prefix}
}

// newInvoice is what makes an invoice.
type newInvoice struct {
	Account     *store.Account
	Profile     *store.BillingProfile
	Kind        string
	Items       []store.InvoiceItem
	Due         time.Time
	PeriodStart time.Time
	PeriodEnd   time.Time
	PlanID      string
	Cycle       string
	PromoID     int64
	Notes       string
	Dedupe      string
	Draft       bool
}

// buildInvoice computes an invoice's totals and freezes its address.
func (s *Service) buildInvoice(ctx context.Context, cfg *InvoicingSettings, n newInvoice) (*store.Invoice, error) {
	tc, err := s.taxContext(ctx, cfg, n.Profile.Contact, n.Profile.TaxExempt)
	if err != nil {
		return nil, err
	}
	inv := &store.Invoice{AccountID: n.Account.ID, AccountName: n.Account.Name, Kind: n.Kind, Status: store.InvoiceUnpaid,
		Currency: cfg.Currency.Code, DueAt: n.Due, PeriodStart: n.PeriodStart, PeriodEnd: n.PeriodEnd,
		InvoiceTotals: ComputeTotals(n.Items, tc), BillingAddress: billingAddress(n.Account, n.Profile), Notes: n.Notes,
		PlanID: n.PlanID, Cycle: n.Cycle, PromoID: n.PromoID, DedupeKey: n.Dedupe, Items: n.Items}
	if n.Draft {
		inv.Status = store.InvoiceDraft
	} else {
		inv.IssuedAt = s.now()
	}
	return inv, nil
}

// createInvoiceLocked stores a built invoice and, when it is issued,
// does what issuing does (issuedLocked). Caller holds payMu.
func (s *Service) createInvoiceLocked(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice, sendMail bool) (*store.Invoice, error) {
	out, err := s.Store.CreateInvoice(ctx, inv, issueNumbering(cfg))
	if err != nil {
		return nil, err
	}
	if out.Status == store.InvoiceUnpaid {
		return s.issuedLocked(ctx, cfg, out, sendMail)
	}
	return out, nil
}

// issuedLocked is what follows an invoice's issue: the account's credit
// pays it when the settings say so, one with nothing to pay is paid (and
// its effects applied), and the client is told (sendMail).
func (s *Service) issuedLocked(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice, sendMail bool) (*store.Invoice, error) {
	now := s.now()
	if cfg.Automation.AutoApplyCredit && inv.Balance() > 0 {
		if _, _, err := s.Store.ApplyCredit(ctx, inv.ID, 0, "system", now, payNumbering(cfg)); err != nil {
			return nil, err
		}
	}
	if _, err := s.Store.SettleInvoice(ctx, inv.ID, now, payNumbering(cfg)); err != nil {
		return nil, err
	}
	inv, err := s.Store.GetInvoice(ctx, inv.ID)
	if err != nil {
		return nil, err
	}
	if inv.Status == store.InvoicePaid {
		if err := s.paidLocked(ctx, cfg, inv, nil); err != nil {
			s.Log.Warn("applying a paid invoice (the automation retries)", "invoice", inv.ID, "err", err)
		}
		return s.Store.GetInvoice(ctx, inv.ID)
	}
	if sendMail {
		s.mailInvoice(ctx, cfg, inv, "invoice.created", fmt.Sprintf("invoice.created:%d", inv.ID), nil)
	}
	return inv, nil
}

// ItemInput is a line of an invoice staff write.
type ItemInput struct {
	Description string `json:"description"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"`
	Taxable     bool   `json:"taxable"`
}

// ManualInvoiceInput is an invoice staff create.
type ManualInvoiceInput struct {
	AccountID int64       `json:"account_id"`
	Items     []ItemInput `json:"items"`
	DueAt     string      `json:"due_at"`
	Notes     string      `json:"notes"`
	Draft     bool        `json:"draft"`
	SendEmail bool        `json:"send_email"`
}

func customItems(in []ItemInput) ([]store.InvoiceItem, error) {
	if len(in) == 0 || len(in) > 100 {
		return nil, fmt.Errorf("%w: an invoice has 1 to 100 lines", ErrInvalid)
	}
	out := make([]store.InvoiceItem, 0, len(in))
	for _, it := range in {
		desc := strings.TrimSpace(it.Description)
		if desc == "" || textField("description", desc, 300, true) != nil {
			return nil, fmt.Errorf("%w: each line has a description of up to 300 characters", ErrInvalid)
		}
		if it.Quantity < 1 || it.Quantity > 100000 || it.UnitPrice > maxAmount || it.UnitPrice < -maxAmount ||
			it.Quantity*it.UnitPrice > maxAmount || it.Quantity*it.UnitPrice < -maxAmount {
			return nil, fmt.Errorf("%w: quantities are 1 to 100000 and amounts reasonable", ErrInvalid)
		}
		out = append(out, store.InvoiceItem{Kind: ItemCustom, Description: desc, Quantity: it.Quantity,
			UnitPrice: it.UnitPrice, Amount: it.Quantity * it.UnitPrice, Taxable: it.Taxable})
	}
	return out, nil
}

// billableAccount is an account WPGenie may invoice: not a reseller's
// customer (their reseller bills them).
func (s *Service) billableAccount(ctx context.Context, id int64) (*store.Account, error) {
	a, err := s.Store.GetAccount(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: no account %d", ErrInvalid, id)
	}
	if err != nil {
		return nil, err
	}
	if a.ParentID != 0 {
		return nil, fmt.Errorf("%w: a reseller's customers are billed by their reseller", ErrInvalid)
	}
	return a, nil
}

// CreateInvoice makes an invoice by hand (a draft, or issued now).
func (s *Service) CreateInvoice(ctx context.Context, in ManualInvoiceInput) (*InvoiceDetail, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.billableAccount(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	items, err := customItems(in.Items)
	if err != nil {
		return nil, err
	}
	notes := strings.TrimSpace(in.Notes)
	if err := textField("notes", notes, 2000, true); err != nil {
		return nil, err
	}
	due := Day(s.now()).AddDate(0, 0, cfg.Invoice.DaysBeforeDue)
	if in.DueAt != "" {
		if due, err = ParseDate(in.DueAt); err != nil {
			return nil, err
		}
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	p, err := s.profile(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	inv, err := s.buildInvoice(ctx, cfg, newInvoice{Account: a, Profile: p, Kind: KindManual, Items: items, Due: due,
		Notes: notes, Draft: in.Draft})
	if err != nil {
		return nil, err
	}
	if inv, err = s.createInvoiceLocked(ctx, cfg, inv, in.SendEmail); err != nil {
		return nil, err
	}
	s.event(ctx, a.ID, "billing", "Invoice "+displayNumber(inv)+" created")
	return s.invoiceDetail(cfg, inv), nil
}

// InvoiceUpdate changes an invoice: a draft's lines, due date and notes;
// an unpaid invoice's due date and notes only.
type InvoiceUpdate struct {
	Items []ItemInput `json:"items"`
	DueAt *string     `json:"due_at"`
	Notes *string     `json:"notes"`
}

func (s *Service) UpdateInvoice(ctx context.Context, id int64, in InvoiceUpdate) (*InvoiceDetail, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	inv, err := s.Store.GetInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	due, notes := inv.DueAt, inv.Notes
	if in.DueAt != nil {
		if due, err = ParseDate(*in.DueAt); err != nil {
			return nil, err
		}
	}
	if in.Notes != nil {
		notes = strings.TrimSpace(*in.Notes)
		if err := textField("notes", notes, 2000, true); err != nil {
			return nil, err
		}
	}
	switch {
	case inv.Status == store.InvoiceDraft && in.Items != nil:
		items, err := customItems(in.Items)
		if err != nil {
			return nil, err
		}
		a, err := s.Store.GetAccount(ctx, inv.AccountID)
		if err != nil {
			return nil, err
		}
		p, err := s.profile(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		built, err := s.buildInvoice(ctx, cfg, newInvoice{Account: a, Profile: p, Kind: inv.Kind, Items: items, Due: due,
			Notes: notes, Draft: true})
		if err != nil {
			return nil, err
		}
		built.ID = id
		err = s.Store.UpdateDraftInvoice(ctx, built)
		if err != nil {
			return nil, err
		}
	case in.Items != nil:
		return nil, fmt.Errorf("%w: only a draft's lines can change; cancel the invoice and make another", ErrConflict)
	default:
		if err := s.Store.UpdateInvoiceNotes(ctx, id, notes, due); errors.Is(err, store.ErrInvoiceState) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		} else if err != nil {
			return nil, err
		}
	}
	return s.Invoice(ctx, id)
}

// IssueInvoice makes a draft final (numbered, payable) and e-mails it.
func (s *Service) IssueInvoice(ctx context.Context, id int64, sendMail bool) (*InvoiceDetail, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	if err := s.Store.IssueInvoice(ctx, id, s.now(), issueNumbering(cfg)); errors.Is(err, store.ErrInvoiceState) {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	} else if err != nil {
		return nil, err
	}
	inv, err := s.Store.GetInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	if inv, err = s.issuedLocked(ctx, cfg, inv, sendMail); err != nil {
		return nil, err
	}
	return s.invoiceDetail(cfg, inv), nil
}

// CancelInvoice cancels a draft or unpaid invoice (what was paid on it
// goes to credit).
func (s *Service) CancelInvoice(ctx context.Context, id int64, by string) (*InvoiceDetail, error) {
	s.payMu.Lock()
	err := s.cancelInvoiceLocked(ctx, id, by, false)
	s.payMu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Invoice(ctx, id)
}

func (s *Service) cancelInvoiceLocked(ctx context.Context, id int64, by string, release bool) error {
	err := s.Store.CancelInvoice(ctx, id, by, s.now(), release)
	if errors.Is(err, store.ErrInvoiceState) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err != nil {
		return err
	}
	if inv, err := s.Store.GetInvoice(ctx, id); err == nil {
		s.event(ctx, inv.AccountID, "billing", "Invoice "+displayNumber(inv)+" cancelled ("+by+")")
	}
	return nil
}

// Remind e-mails a reminder about an unpaid invoice now.
func (s *Service) Remind(ctx context.Context, id int64) error {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return err
	}
	inv, err := s.Store.GetInvoice(ctx, id)
	if err != nil {
		return err
	}
	if inv.Status != store.InvoiceUnpaid {
		return fmt.Errorf("%w: the invoice is %s", ErrConflict, inv.Status)
	}
	now := s.now()
	if !inv.DueAt.IsZero() && inv.DueAt.Before(now) {
		days := int(now.Sub(inv.DueAt).Hours() / 24)
		s.mailInvoice(ctx, cfg, inv, "invoice.overdue", "", map[string]any{"Days": max(days, 1), "SuspendDate": s.suspendDate(cfg, inv)})
	} else {
		s.mailInvoice(ctx, cfg, inv, "invoice.reminder", "", nil)
	}
	s.event(ctx, inv.AccountID, "billing", "Reminder sent for invoice "+displayNumber(inv))
	return nil
}

// suspendDate is when an overdue invoice suspends the account ("" when it
// doesn't: not billed by invoice, or already past).
func (s *Service) suspendDate(cfg *InvoicingSettings, inv *store.Invoice) string {
	at := inv.DueAt.AddDate(0, 0, cfg.Automation.SuspendAfterDays)
	if inv.DueAt.IsZero() || !at.After(s.now()) {
		return ""
	}
	return shortDate(at)
}

// ---- Paying ----

// PaymentInput is a payment recorded for an invoice. Dedupe (a gateway's
// reference) makes it count once.
type PaymentInput struct {
	Gateway   string
	Reference string
	Amount    int64
	Fee       int64
	At        time.Time
	Note      string
	By        string
	Dedupe    string
}

// RecordPayment records a payment and, if it pays the invoice, applies
// what that does (see paidLocked).
func (s *Service) RecordPayment(ctx context.Context, invoiceID int64, in PaymentInput) (*store.PaymentResult, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	return s.recordPaymentLocked(ctx, cfg, invoiceID, in)
}

func (s *Service) recordPaymentLocked(ctx context.Context, cfg *InvoicingSettings, invoiceID int64, in PaymentInput) (*store.PaymentResult, error) {
	if in.At.IsZero() {
		in.At = s.now()
	}
	res, err := s.Store.RecordPayment(ctx, store.PaymentInput{InvoiceID: invoiceID, Gateway: in.Gateway, Reference: in.Reference,
		Amount: in.Amount, Fee: in.Fee, Note: in.Note, By: in.By, At: in.At, Dedupe: in.Dedupe, Numbering: payNumbering(cfg)})
	if errors.Is(err, store.ErrInvoiceState) {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err != nil || res.Duplicate {
		return res, err
	}
	p := res.Payment
	msg := fmt.Sprintf("Payment of %s received (%s) for invoice %s", FormatMoney(p.Amount, cfg.Currency), p.Gateway,
		map[bool]string{true: p.InvoiceNumber, false: fmt.Sprintf("#%d", p.InvoiceID)}[p.InvoiceNumber != ""])
	if res.Credited > 0 {
		msg += fmt.Sprintf("; %s beyond the balance added to credit", FormatMoney(res.Credited, cfg.Currency))
	}
	s.event(ctx, p.AccountID, "billing", msg)
	if res.Paid {
		inv, err := s.Store.GetInvoice(ctx, invoiceID)
		if err != nil {
			return res, err
		}
		if err := s.paidLocked(ctx, cfg, inv, p); err != nil {
			// Recorded: the automation applies it again.
			s.Log.Warn("applying a paid invoice (the automation retries)", "invoice", invoiceID, "err", err)
		}
	}
	return res, nil
}

// ApplyCredit pays an invoice from the account's credit (nil amount: as
// much as possible).
func (s *Service) ApplyCredit(ctx context.Context, invoiceID int64, amount *int64, by string) (*InvoiceDetail, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	var want int64
	if amount != nil {
		if *amount <= 0 {
			return nil, fmt.Errorf("%w: the amount must be positive", ErrInvalid)
		}
		want = *amount
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	applied, paid, err := s.Store.ApplyCredit(ctx, invoiceID, want, by, s.now(), payNumbering(cfg))
	switch {
	case errors.Is(err, store.ErrInvoiceState):
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	case errors.Is(err, store.ErrInsufficientCredit), errors.Is(err, store.ErrOverRefund):
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	case err != nil:
		return nil, err
	}
	inv, err := s.Store.GetInvoice(ctx, invoiceID)
	if err != nil {
		return nil, err
	}
	if applied > 0 {
		s.event(ctx, inv.AccountID, "billing", fmt.Sprintf("Credit of %s applied to invoice %s", FormatMoney(applied, cfg.Currency),
			displayNumber(inv)))
	}
	if paid {
		if err := s.paidLocked(ctx, cfg, inv, nil); err != nil {
			s.Log.Warn("applying a paid invoice (the automation retries)", "invoice", invoiceID, "err", err)
		}
		if inv, err = s.Store.GetInvoice(ctx, invoiceID); err != nil {
			return nil, err
		}
	}
	return s.invoiceDetail(cfg, inv), nil
}

// paidLocked applies what paying an invoice does, once: an order's
// account is activated, a renewal moves the next due date, a plan change
// switches the plan; a billing suspension is lifted when nothing is
// overdue any more; the client gets a receipt and webhooks invoice.paid.
// Each step is idempotent, and the invoice is marked done only when all
// succeeded: the automation retries the others.
func (s *Service) paidLocked(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice, pay *store.Payment) error {
	if inv.EffectsDone {
		return nil
	}
	var err error
	switch inv.Kind {
	case KindOrder:
		err = s.orderPaidLocked(ctx, cfg, inv)
	case KindRenewal:
		err = s.renewalPaid(ctx, inv)
	case KindPlanChange:
		err = s.applyPlanChangeLocked(ctx, inv.AccountID, inv.PlanID, inv.Cycle, inv.PeriodStart, inv.PeriodEnd, inv.ID)
	case KindBurstTopup:
		err = s.burstPaid(ctx, inv)
	}
	if err != nil {
		return err
	}
	if err := s.liftBillingSuspension(ctx, inv.AccountID); err != nil {
		return err
	}
	amount, method := inv.AmountPaid+inv.CreditApplied, ""
	if pay != nil {
		amount, method = pay.Amount, methodName(cfg, pay.Gateway)
	}
	if inv.Total > 0 {
		data := map[string]any{"Payment": map[string]any{"Amount": FormatMoney(amount, cfg.Currency), "Method": method}}
		if inv.Kind == KindBurstTopup {
			data["BurstMinutes"] = inv.Units
		}
		s.mailInvoice(ctx, cfg, inv, "invoice.paid", fmt.Sprintf("invoice.paid:%d", inv.ID), data)
	}
	s.emit(ctx, EventInvoicePaid, map[string]any{"invoice_id": inv.ID, "number": inv.Number, "account_id": inv.AccountID,
		"kind": inv.Kind, "total": inv.Total, "currency": inv.Currency})
	return s.Store.SetInvoiceEffectsDone(ctx, inv.ID)
}

func methodName(cfg *InvoicingSettings, gateway string) string {
	switch gateway {
	case MethodStripe:
		return cfg.Methods.Stripe.Name
	case MethodRazorpay:
		return cfg.Methods.Razorpay.Name
	case "bank":
		return cfg.Methods.Manual.Name
	}
	return gateway
}

// renewalPaid moves the account's next due date past the period paid.
func (s *Service) renewalPaid(ctx context.Context, inv *store.Invoice) error {
	p, err := s.profile(ctx, inv.AccountID)
	if err != nil {
		return err
	}
	if !inv.PeriodEnd.After(p.NextDueAt) {
		return nil
	}
	p.NextDueAt = inv.PeriodEnd
	return s.Store.SaveBillingProfile(ctx, p)
}

// liftBillingSuspension brings back an account billing suspended once
// none of its invoices is overdue.
func (s *Service) liftBillingSuspension(ctx context.Context, accountID int64) error {
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if a.Status != store.AccountSuspended || a.SuspendReason != ReasonBilling {
		return nil
	}
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return err
	}
	if ModeOf(a, p) != ModeInvoice {
		return nil // a Stripe subscription's suspension is Stripe's to lift
	}
	overdue, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{AccountID: accountID, OverdueAt: s.now(), Limit: 1})
	if err != nil || len(overdue) > 0 {
		return err
	}
	if _, err := s.Unsuspend(ctx, accountID, ReasonBilling); err != nil {
		return err
	}
	s.mail(ctx, accountID, "account.unsuspended", fmt.Sprintf("account.unsuspended:%d:%d", accountID, a.SuspendedAt.Unix()), nil)
	return nil
}

// ---- Refunds ----

// RefundInput refunds (part of) a payment: to the payer through its
// gateway ("gateway"; payments recorded by hand are only marked refunded)
// or to the account's credit ("credit"). Amount 0: what is left of it.
type RefundInput struct {
	PaymentID int64  `json:"payment_id"`
	Amount    int64  `json:"amount"`
	To        string `json:"to"`
}

func (s *Service) Refund(ctx context.Context, invoiceID int64, in RefundInput, by string) (*InvoiceDetail, error) {
	if in.To != "gateway" && in.To != "credit" {
		return nil, fmt.Errorf("%w: refund to gateway or credit", ErrInvalid)
	}
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	pay, err := s.Store.GetPayment(ctx, in.PaymentID)
	if err != nil || pay.InvoiceID != invoiceID {
		return nil, fmt.Errorf("%w: no payment %d on this invoice", store.ErrNotFound, in.PaymentID)
	}
	left := pay.Amount - pay.Refunded
	amount := in.Amount
	if amount == 0 {
		amount = left
	}
	if amount <= 0 || amount > left {
		return nil, fmt.Errorf("%w: %s of this payment can be refunded", ErrInvalid, FormatMoney(left, cfg.Currency))
	}
	rin := store.RefundInput{PaymentID: pay.ID, Amount: amount, ToCredit: in.To == "credit", By: by, At: s.now()}
	if in.To == "gateway" {
		switch pay.Gateway {
		case MethodStripe:
			sc, err := s.StripeSettings(ctx)
			if err != nil {
				return nil, err
			}
			if sc.SecretKey == "" || s.Stripe == nil {
				return nil, fmt.Errorf("%w: Stripe's secret key isn't set", ErrConflict)
			}
			id, err := s.Stripe.Refund(ctx, sc.SecretKey, pay.Reference, amount,
				fmt.Sprintf("wpgenie-refund-%d-%d-%d", pay.ID, pay.Refunded, amount))
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrConflict, err)
			}
			rin.Reference, rin.Dedupe = id, "stripe:"+id
		case MethodRazorpay:
			rz := cfg.Methods.Razorpay
			if rz.KeyID == "" || str(rz.KeySecret) == "" {
				return nil, fmt.Errorf("%w: Razorpay's keys aren't set", ErrConflict)
			}
			id, err := s.razorpay().Refund(ctx, rz.KeyID, str(rz.KeySecret), pay.Reference, amount, invoiceID)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrConflict, err)
			}
			rin.Reference, rin.Dedupe = id, "razorpay:"+id
		}
	}
	r, _, err := s.Store.RecordRefund(ctx, rin)
	if errors.Is(err, store.ErrOverRefund) {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err != nil {
		return nil, err
	}
	if r != nil {
		s.event(ctx, pay.AccountID, "billing", fmt.Sprintf("Refund of %s (%s) on invoice %s", FormatMoney(amount, cfg.Currency),
			map[bool]string{true: "to credit", false: pay.Gateway}[r.ToCredit], pay.InvoiceNumber))
	}
	return s.Invoice(ctx, invoiceID)
}

// ---- Plan changes ----

// PlanChangeQuote is what changing plan costs now.
type PlanChangeQuote struct {
	// Credit is the unused part of what was paid for the current period;
	// Charge the new plan until the next due date (or a whole new cycle).
	Credit       int64               `json:"credit"`
	Charge       int64               `json:"charge"`
	Subtotal     int64               `json:"subtotal"`
	TaxLines     []store.TaxLine     `json:"tax_lines"`
	Tax          int64               `json:"tax"`
	Total        int64               `json:"total"`
	Effective    string              `json:"effective"`
	NewNextDueAt time.Time           `json:"new_next_due_at"`
	Items        []store.InvoiceItem `json:"items"`

	plan        *store.Plan
	cycle       string
	periodStart time.Time
	account     *store.Account
	profile     *store.BillingProfile
}

// QuotePlanChange prices a change to plan and cycle ("" keeps the
// cycle). byTenant: only public plans.
func (s *Service) QuotePlanChange(ctx context.Context, accountID int64, planID, cycle string, byTenant bool) (*PlanChangeQuote, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	return s.quotePlanChange(ctx, cfg, accountID, planID, cycle, byTenant)
}

func (s *Service) quotePlanChange(ctx context.Context, cfg *InvoicingSettings, accountID int64, planID, cycle string, byTenant bool) (*PlanChangeQuote, error) {
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	switch {
	case ModeOf(a, p) != ModeInvoice:
		return nil, fmt.Errorf("%w: this account isn't billed by invoice; staff change its plan", ErrConflict)
	case a.Status == store.AccountPending || a.Status == store.AccountTerminated:
		return nil, fmt.Errorf("%w: the account is %s", ErrConflict, a.Status)
	}
	if cycle == "" {
		cycle = p.Cycle
	}
	if CycleMonths(cycle) == 0 {
		return nil, fmt.Errorf("%w: unknown cycle %q", ErrInvalid, cycle)
	}
	plan, err := s.Store.GetPlan(ctx, planID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && byTenant && !plan.Public && plan.ID != a.PlanID) {
		return nil, fmt.Errorf("%w: no plan %q", ErrInvalid, planID)
	}
	if err != nil {
		return nil, err
	}
	price, ok := plan.Prices[cycle]
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: plan %s has no %s price", ErrInvalid, plan.Name, cycle)
	case plan.AccountKind != a.Kind:
		return nil, fmt.Errorf("%w: plan %s is for %s accounts", ErrInvalid, plan.Name, plan.AccountKind)
	case plan.ID == a.PlanID && cycle == p.Cycle:
		return nil, fmt.Errorf("%w: the account is already on %s, %s", ErrInvalid, plan.Name, cycle)
	}
	now := s.now()
	overdue, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{AccountID: accountID, OverdueAt: now, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(overdue) > 0 {
		return nil, fmt.Errorf("%w: pay the overdue invoice %s first", ErrConflict, displayNumber(overdue[0]))
	}
	curPlan, err := s.Store.GetPlan(ctx, a.PlanID)
	if err != nil {
		return nil, err
	}
	q := &PlanChangeQuote{Effective: "now", plan: plan, cycle: cycle, account: a, profile: p}
	var items []store.InvoiceItem
	today := Day(now)
	// What was paid for runs until NextDueAt: its unused part comes back.
	paidThrough := p.NextDueAt
	var periodSecs int64
	if !paidThrough.IsZero() && paidThrough.After(now) {
		start := AddMonths(paidThrough, -CycleMonths(p.Cycle), p.AnchorDay)
		periodSecs = int64(paidThrough.Sub(start).Seconds())
		if old, ok := recurringPrice(curPlan, p); ok && old > 0 && periodSecs > 0 {
			q.Credit = MulDiv(old, int64(paidThrough.Sub(now).Seconds()), periodSecs)
		}
	}
	if cycle == p.Cycle && periodSecs > 0 {
		// Same cycle: the new plan, pro rata until the next due date.
		q.Charge = MulDiv(price.Price, int64(paidThrough.Sub(now).Seconds()), periodSecs)
		q.periodStart, q.NewNextDueAt = today, paidThrough
	} else {
		// A new cycle (or nothing paid ahead): a whole cycle from today.
		q.Charge = price.Price
		q.periodStart, q.NewNextDueAt = today, AddMonths(today, CycleMonths(cycle), today.Day())
	}
	items = append(items, store.InvoiceItem{Kind: ItemProration, Description: fmt.Sprintf("%s plan (%s) — %s", plan.Name,
		cycle, periodText(q.periodStart, q.NewNextDueAt)), Quantity: 1, UnitPrice: q.Charge, Amount: q.Charge, Taxable: true})
	if q.Credit > 0 {
		items = append(items, store.InvoiceItem{Kind: ItemCredit, Description: fmt.Sprintf("Unused time on the %s plan until %s",
			curPlan.Name, shortDate(paidThrough)), Quantity: 1, UnitPrice: -q.Credit, Amount: -q.Credit, Taxable: true})
	}
	tc, err := s.taxContext(ctx, cfg, p.Contact, p.TaxExempt)
	if err != nil {
		return nil, err
	}
	t := ComputeTotals(items, tc)
	q.Subtotal, q.TaxLines, q.Tax, q.Total, q.Items = t.Subtotal, t.TaxLines, t.Tax, t.Total, items
	return q, nil
}

// PlanChangeResult is a plan change: its invoice, or applied at once when
// nothing was due.
type PlanChangeResult struct {
	Invoice *InvoiceDetail `json:"invoice,omitempty"`
	Applied bool           `json:"applied,omitempty"`
}

// ChangePlan invoices a plan change; the plan switches when the invoice is
// paid. Nothing to pay (a downgrade): switched now, and a negative total
// is added to the account's credit.
func (s *Service) ChangePlan(ctx context.Context, accountID int64, planID, cycle string, byTenant bool, by string) (*PlanChangeResult, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	q, err := s.quotePlanChange(ctx, cfg, accountID, planID, cycle, byTenant)
	if err != nil {
		return nil, err
	}
	// A newer change replaces one not paid yet.
	open, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{AccountID: accountID, Status: store.InvoiceUnpaid, Kind: KindPlanChange})
	if err != nil {
		return nil, err
	}
	for _, inv := range open {
		if err := s.cancelInvoiceLocked(ctx, inv.ID, by, false); err != nil {
			return nil, err
		}
	}
	if q.Total <= 0 {
		if err := s.applyPlanChangeLocked(ctx, accountID, q.plan.ID, q.cycle, q.periodStart, q.NewNextDueAt, 0); err != nil {
			return nil, err
		}
		if q.Total < 0 {
			if _, _, err := s.Store.AddCredit(ctx, accountID, -q.Total, "Plan change to "+q.plan.Name+": unused time", by, "",
				s.now()); err != nil {
				return nil, err
			}
		}
		return &PlanChangeResult{Applied: true}, nil
	}
	inv, err := s.buildInvoice(ctx, cfg, newInvoice{Account: q.account, Profile: q.profile, Kind: KindPlanChange, Items: q.Items,
		Due: s.now(), PeriodStart: q.periodStart, PeriodEnd: q.NewNextDueAt, PlanID: q.plan.ID, Cycle: q.cycle})
	if err != nil {
		return nil, err
	}
	if inv, err = s.createInvoiceLocked(ctx, cfg, inv, true); err != nil {
		return nil, err
	}
	s.event(ctx, accountID, "billing", fmt.Sprintf("Plan change to %s (%s) requested (%s)", q.plan.Name, q.cycle, by))
	return &PlanChangeResult{Invoice: s.invoiceDetail(cfg, inv)}, nil
}

// applyPlanChangeLocked switches an account's plan and cycle, with its
// next due date at the end of the period now paid. Renewals not paid yet
// were for the old plan: they are cancelled (and made again).
func (s *Service) applyPlanChangeLocked(ctx context.Context, accountID int64, planID, cycle string, start, end time.Time, invoiceID int64) error {
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if a.PlanID != planID {
		in := inputOf(a)
		in.PlanID = planID
		if _, err := s.UpdateAccount(ctx, accountID, in, false); err != nil {
			return err
		}
	}
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return err
	}
	if p.Cycle != cycle {
		p.AnchorDay = start.Day()
	}
	p.Cycle, p.NextDueAt, p.PriceOverride = cycle, end, nil
	if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
		return err
	}
	open, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{AccountID: accountID, Status: store.InvoiceUnpaid, Kind: KindRenewal})
	if err != nil {
		return err
	}
	for _, inv := range open {
		if inv.ID != invoiceID {
			if err := s.cancelInvoiceLocked(ctx, inv.ID, "plan change", true); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- Cancellation ----

// RequestCancel cancels an account's service at the end of the period paid
// for, or (staff) now.
func (s *Service) RequestCancel(ctx context.Context, accountID int64, when, reason string) (*ProfileView, error) {
	reason = strings.TrimSpace(reason)
	if err := textField("reason", reason, 500, true); err != nil {
		return nil, err
	}
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if a.Status == store.AccountTerminated {
		return nil, fmt.Errorf("%w: the account is terminated", ErrConflict)
	}
	switch when {
	case "immediately":
		s.payMu.Lock()
		err := s.cancelNowLocked(ctx, cfg, a, reason)
		s.payMu.Unlock()
		if err != nil {
			return nil, err
		}
	case "end_of_period":
		s.payMu.Lock()
		defer s.payMu.Unlock()
		p, err := s.profile(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if ModeOf(a, p) != ModeInvoice || p.NextDueAt.IsZero() {
			return nil, fmt.Errorf("%w: this account isn't billed by invoice: cancel it with its billing system", ErrConflict)
		}
		p.CancelAt, p.CancelReason = p.NextDueAt, reason
		if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
			return nil, err
		}
		s.event(ctx, accountID, "billing", "Cancellation requested for "+shortDate(p.CancelAt))
	default:
		return nil, fmt.Errorf("%w: when is end_of_period or immediately", ErrInvalid)
	}
	return s.Profile(ctx, accountID)
}

// WithdrawCancel withdraws a cancellation not yet done.
func (s *Service) WithdrawCancel(ctx context.Context, accountID int64) (*ProfileView, error) {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if p.CancelAt.IsZero() {
		return nil, fmt.Errorf("%w: no cancellation is pending", ErrConflict)
	}
	p.CancelAt, p.CancelReason = time.Time{}, ""
	if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
		return nil, err
	}
	s.event(ctx, accountID, "billing", "Cancellation withdrawn")
	return s.Profile(ctx, accountID)
}

// cancelNowLocked ends a cancelled account's service: unpaid renewals and
// plan changes are cancelled, the account terminated (its sites deleted
// only if the settings say so), the client told.
func (s *Service) cancelNowLocked(ctx context.Context, cfg *InvoicingSettings, a *store.Account, reason string) error {
	unpaid, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{AccountID: a.ID, Status: store.InvoiceUnpaid})
	if err != nil {
		return err
	}
	for _, inv := range unpaid {
		if inv.Kind == KindRenewal || inv.Kind == KindPlanChange {
			if err := s.cancelInvoiceLocked(ctx, inv.ID, "cancellation", false); err != nil {
				return err
			}
		}
	}
	if _, err := s.Terminate(ctx, a.ID, cfg.Automation.TerminateDeletesSites); err != nil {
		return err
	}
	if reason == "" {
		reason = "cancelled"
	}
	s.event(ctx, a.ID, "billing", "Service cancelled: "+reason)
	s.mail(ctx, a.ID, "account.cancelled", fmt.Sprintf("account.cancelled:%d", a.ID), map[string]any{"Reason": reason})
	return nil
}
