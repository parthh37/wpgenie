package billing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

// The public order form's backend: the catalog of public plans, quotes,
// and orders. An order creates, at once, a pending account (no sites
// until paid), its first user, its billing profile and its first
// invoice; paying the invoice activates the account (or, with
// orders_need_approval, leaves it for staff to accept). Orders left unpaid
// are cancelled by the automation after staleOrderAge.

// ErrStoreClosed: built-in billing (and so the order form) is off.
var ErrStoreClosed = errors.New("the store is closed")

const staleOrderAge = 14 * 24 * time.Hour

// CatalogPlan is a plan on the order form.
type CatalogPlan struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	AccountKind string                     `json:"account_kind"`
	Features    []string                   `json:"features"`
	Limits      CatalogLimits              `json:"limits"`
	Prices      map[string]store.PlanPrice `json:"prices"`
}

// CatalogLimits are a plan's limits (0: unlimited).
type CatalogLimits struct {
	MaxSites     int     `json:"max_sites"`
	DiskMB       int64   `json:"disk_mb"`
	BandwidthGB  int64   `json:"bandwidth_gb"`
	MaxReplicas  int     `json:"max_replicas"`
	MaxMemoryMB  int     `json:"max_memory_mb"`
	MaxCPUs      float64 `json:"max_cpus"`
	MaxDomains   int     `json:"max_domains"`
	BurstMinutes int64   `json:"burst_minutes"`
}

// Catalog is what the order form offers.
type Catalog struct {
	Enabled            bool          `json:"enabled"`
	Company            CatalogBrand  `json:"company"`
	Currency           Currency      `json:"currency"`
	TaxInclusive       bool          `json:"tax_inclusive"`
	TermsURL           string        `json:"terms_url"`
	Methods            []MethodInfo  `json:"methods"`
	Plans              []CatalogPlan `json:"plans"`
	CountriesNeedState []string      `json:"countries_need_state"`
}

// CatalogBrand is who sells (LogoURL is the API's to fill in).
type CatalogBrand struct {
	Name    string `json:"name"`
	LogoURL string `json:"logo_url"`
}

// Catalog lists the public plans with a price, by their sort order.
func (s *Service) Catalog(ctx context.Context) (*Catalog, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	c := &Catalog{Enabled: cfg.Enabled, Company: CatalogBrand{Name: cfg.Company.Name}, Currency: cfg.Currency,
		TaxInclusive: cfg.Tax.Enabled && cfg.Tax.Inclusive, TermsURL: cfg.Invoice.TermsURL, Methods: []MethodInfo{},
		Plans: []CatalogPlan{}, CountriesNeedState: CountriesNeedState}
	if !cfg.Enabled {
		return c, nil
	}
	c.Methods = cfg.PayMethods()
	plans, err := s.Store.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(plans, func(a, b *store.Plan) int { return a.Sort - b.Sort })
	for _, p := range plans {
		if !p.Public || len(p.Prices) == 0 {
			continue
		}
		c.Plans = append(c.Plans, CatalogPlan{ID: p.ID, Name: p.Name, Description: p.Description, AccountKind: p.AccountKind,
			Features: p.Features, Prices: p.Prices, Limits: CatalogLimits{MaxSites: p.MaxSites, DiskMB: p.DiskMB,
				BandwidthGB: p.BandwidthGB, MaxReplicas: p.MaxReplicas, MaxMemoryMB: p.MaxMemoryMB, MaxCPUs: p.MaxCPUs,
				MaxDomains: p.MaxDomains, BurstMinutes: p.BurstMinutes}})
	}
	return c, nil
}

// QuoteInput is what an order would be.
type QuoteInput struct {
	PlanID  string `json:"plan_id"`
	Cycle   string `json:"cycle"`
	Promo   string `json:"promo"`
	Country string `json:"country"`
	State   string `json:"state"`
	TaxID   string `json:"tax_id"`
}

// PromoCheck says whether a promo code applies.
type PromoCheck struct {
	Valid   bool   `json:"valid"`
	Message string `json:"message"`
}

// Quote is what an order costs: the first invoice, and what renews.
type Quote struct {
	Items          []store.InvoiceItem `json:"items"`
	Subtotal       int64               `json:"subtotal"`
	Discount       int64               `json:"discount"`
	TaxLines       []store.TaxLine     `json:"tax_lines"`
	Tax            int64               `json:"tax"`
	Total          int64               `json:"total"`
	RecurringTotal int64               `json:"recurring_total"`
	Promo          *PromoCheck         `json:"promo"`
}

// orderDraft is a priced order.
type orderDraft struct {
	plan        *store.Plan
	cycle       string
	promo       *store.Promotion
	promoErr    error
	items       []store.InvoiceItem
	totals      store.InvoiceTotals
	recurring   int64
	periodStart time.Time
	periodEnd   time.Time
}

// priceOrder prices a plan and cycle for a client in a country: the plan
// for the first period, its setup fee, the promotion's discount.
func (s *Service) priceOrder(ctx context.Context, cfg *InvoicingSettings, in QuoteInput) (*orderDraft, error) {
	plan, err := s.Store.GetPlan(ctx, in.PlanID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (!plan.Public || len(plan.Prices) == 0)) {
		return nil, fieldErr("plan_id", "choose a plan")
	}
	if err != nil {
		return nil, err
	}
	price, ok := plan.Prices[in.Cycle]
	if !ok {
		return nil, fieldErr("cycle", "that plan isn't sold %s", in.Cycle)
	}
	d := &orderDraft{plan: plan, cycle: in.Cycle}
	d.periodStart = Day(s.now())
	d.periodEnd = AddMonths(d.periodStart, CycleMonths(in.Cycle), d.periodStart.Day())
	if code := strings.TrimSpace(in.Promo); code != "" {
		d.promo, d.promoErr = s.checkPromo(ctx, code, plan.ID, in.Cycle)
		if d.promoErr != nil && !errors.Is(d.promoErr, ErrInvalid) {
			return nil, d.promoErr
		}
	}
	d.items = []store.InvoiceItem{{Kind: ItemPlan, Description: fmt.Sprintf("%s plan (%s) — %s", plan.Name, in.Cycle,
		periodText(d.periodStart, d.periodEnd)), Quantity: 1, UnitPrice: price.Price, Amount: price.Price, Taxable: true}}
	if price.SetupFee > 0 {
		d.items = append(d.items, store.InvoiceItem{Kind: ItemSetup, Description: "Setup fee", Quantity: 1,
			UnitPrice: price.SetupFee, Amount: price.SetupFee, Taxable: true})
	}
	discount := promoDiscount(d.promo, price.Price)
	d.items = append(d.items, discountItem(d.promo, discount, cfg.Currency)...)
	contact := store.BillingContact{Country: strings.ToUpper(strings.TrimSpace(in.Country)), State: in.State, TaxID: in.TaxID}
	tc, err := s.taxContext(ctx, cfg, contact, false)
	if err != nil {
		return nil, err
	}
	d.totals = ComputeTotals(d.items, tc)
	renew := []store.InvoiceItem{{Kind: ItemPlan, Quantity: 1, UnitPrice: price.Price, Amount: price.Price, Taxable: true}}
	if d.promo != nil && d.promo.Recurring {
		renew = append(renew, discountItem(d.promo, discount, cfg.Currency)...)
	}
	d.recurring = ComputeTotals(renew, tc).Total
	return d, nil
}

// Quote prices an order for the order form. An invalid promo code is
// reported, not an error: the quote is without it.
func (s *Service) Quote(ctx context.Context, in QuoteInput) (*Quote, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrStoreClosed
	}
	d, err := s.priceOrder(ctx, cfg, in)
	if err != nil {
		return nil, err
	}
	q := &Quote{Items: d.items, Subtotal: d.totals.Subtotal, Discount: d.totals.Discount, TaxLines: d.totals.TaxLines,
		Tax: d.totals.Tax, Total: d.totals.Total, RecurringTotal: d.recurring}
	switch {
	case d.promoErr != nil:
		q.Promo = &PromoCheck{Message: strings.TrimPrefix(d.promoErr.Error(), ErrInvalid.Error()+": ")}
	case d.promo != nil:
		msg := "Code applied"
		if d.promo.Description != "" {
			msg += ": " + d.promo.Description
		}
		q.Promo = &PromoCheck{Valid: true, Message: msg}
	}
	return q, nil
}

// OrderInput is an order from the order form. Website is a honeypot: a
// person never fills it in.
type OrderInput struct {
	PlanID      string               `json:"plan_id"`
	Cycle       string               `json:"cycle"`
	Promo       string               `json:"promo"`
	Method      string               `json:"method"`
	AcceptTerms bool                 `json:"accept_terms"`
	Contact     store.BillingContact `json:"contact"`
	User        struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"user"`
	Website string `json:"website"`
}

// PayNext is what a client does next to pay: go to a payment page, follow
// instructions, or nothing (paid). Error: the gateway failed; the invoice
// can be paid later from the client area.
type PayNext struct {
	RedirectURL  string `json:"redirect_url,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Reference    string `json:"reference,omitempty"`
	Paid         bool   `json:"paid,omitempty"`
	Error        string `json:"error,omitempty"`
}

// OrderResult is a placed order.
type OrderResult struct {
	OrderID   int64    `json:"-"`
	AccountID int64    `json:"account_id"`
	InvoiceID int64    `json:"invoice_id"`
	Next      *PayNext `json:"next"`
}

// PlaceOrder creates an order's account, user, profile and invoice (see
// store.CreateOrder) and starts its payment. Input errors are
// *FieldError. hash turns the password into its stored hash; it is called
// only once everything else is valid (hashing is slow).
func (s *Service) PlaceOrder(ctx context.Context, in OrderInput, ip string, hash func(string) (string, error)) (*OrderResult, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrStoreClosed
	}
	if in.Website != "" {
		return nil, fmt.Errorf("%w: the order was refused", ErrInvalid)
	}
	c := in.Contact
	if err := NormalizeContact(&c, true); err != nil {
		return nil, err
	}
	d, err := s.priceOrder(ctx, cfg, QuoteInput{PlanID: in.PlanID, Cycle: in.Cycle, Promo: in.Promo, Country: c.Country,
		State: c.State, TaxID: c.TaxID})
	if err != nil {
		return nil, err
	}
	if d.promoErr != nil {
		return nil, fieldErr("promo", "%s", strings.TrimPrefix(d.promoErr.Error(), ErrInvalid.Error()+": "))
	}
	if d.promo != nil && d.promo.NewClientsOnly {
		// Someone with an account already (by e-mail) isn't new.
		known, err := s.Store.AccountEmailExists(ctx, c.Email)
		if err != nil {
			return nil, err
		}
		if known {
			return nil, fieldErr("promo", "that code is for new clients only")
		}
	}
	if d.totals.Total > 0 && !slices.Contains(cfg.methodIDs(), in.Method) {
		return nil, fieldErr("method", "choose how to pay")
	}
	if cfg.Invoice.TermsURL != "" && !in.AcceptTerms {
		return nil, fieldErr("accept_terms", "please accept the terms of service")
	}
	name := c.Company
	if name == "" {
		name = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	if name == "" {
		name, _, _ = strings.Cut(c.Email, "@")
	}
	name = truncateRunes(strings.Join(strings.Fields(name), " "), 100)
	if validName(name) != nil {
		return nil, fieldErr("contact.company", "enter your name or your company's")
	}
	pwHash, err := hash(in.User.Password)
	if err != nil {
		return nil, err
	}
	var promoID int64
	if d.promo != nil {
		promoID = d.promo.ID
	}
	profile := &store.BillingProfile{Mode: ModeInvoice, Cycle: d.cycle, NextDueAt: d.periodEnd, AnchorDay: d.periodStart.Day(),
		Contact: c}
	if d.promo != nil && d.promo.Recurring {
		profile.PromoID = promoID
	}
	acct := &store.Account{Name: name, Kind: d.plan.AccountKind, PlanID: d.plan.ID, Email: c.Email}
	inv, err := s.buildInvoice(ctx, cfg, newInvoice{Account: acct, Profile: profile, Kind: KindOrder, Items: d.items,
		Due: d.periodStart, PeriodStart: d.periodStart, PeriodEnd: d.periodEnd, PlanID: d.plan.ID, Cycle: d.cycle, PromoID: promoID})
	if err != nil {
		return nil, err
	}
	o, inv, err := s.createOrder(ctx, cfg, &store.NewOrder{Account: acct,
		User:    &store.NewUser{Username: in.User.Username, PasswordHash: pwHash, Role: auth.TenantRole(d.plan.AccountKind)},
		Profile: profile, Invoice: inv, Numbering: issueNumbering(cfg), PromoID: promoID,
		Order: &store.Order{PlanID: d.plan.ID, Cycle: d.cycle, PromoCode: strings.ToUpper(strings.TrimSpace(in.Promo)),
			Total: inv.Total, IP: ip}, At: s.now()})
	switch {
	case errors.Is(err, store.ErrExists):
		return nil, fieldErr("user.username", "that username is taken")
	case errors.Is(err, store.ErrPromoUsedUp):
		return nil, fieldErr("promo", "that code has been used up")
	case err != nil:
		return nil, err
	}
	s.event(ctx, o.AccountID, "order", fmt.Sprintf("Ordered %s (%s) from %s", d.plan.Name, d.cycle, ip))
	s.mailInvoice(ctx, cfg, inv, "order.received", fmt.Sprintf("order.received:%d", o.ID), map[string]any{
		"Order": map[string]any{"Plan": d.plan.Name, "Cycle": d.cycle}, "Username": in.User.Username})
	res := &OrderResult{OrderID: o.ID, AccountID: o.AccountID, InvoiceID: o.InvoiceID}
	if inv.Status != store.InvoiceUnpaid {
		res.Next = &PayNext{Paid: true} // free, or fully discounted: paid, and so active
		return res, nil
	}
	res.Next, err = s.startPayment(ctx, cfg, inv, in.Method, false)
	if err != nil {
		// The order stands: the client pays from their client area.
		s.Log.Warn("starting an order's payment", "order", o.ID, "err", err)
		res.Next = &PayNext{Error: "The payment couldn't be started; you can pay the invoice from your account."}
	}
	return res, nil
}

// createOrder stores an order and, when its invoice has nothing to pay,
// settles it (which activates the account).
func (s *Service) createOrder(ctx context.Context, cfg *InvoicingSettings, n *store.NewOrder) (*store.Order, *store.Invoice, error) {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	o, err := s.Store.CreateOrder(ctx, n)
	if err != nil {
		return nil, nil, err
	}
	inv, err := s.Store.GetInvoice(ctx, o.InvoiceID)
	if err != nil {
		return nil, nil, err
	}
	if inv.Balance() <= 0 {
		if inv, err = s.issuedLocked(ctx, cfg, inv, false); err != nil {
			return nil, nil, err
		}
	}
	return o, inv, nil
}

// Pay starts paying an invoice with a method (see PayNext).
func (s *Service) Pay(ctx context.Context, invoiceID int64, method string, saveCard bool) (*PayNext, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	inv, err := s.Store.GetInvoice(ctx, invoiceID)
	if err != nil {
		return nil, err
	}
	switch inv.Status {
	case store.InvoicePaid, store.InvoicePartiallyRefunded, store.InvoiceRefunded:
		return &PayNext{Paid: true}, nil
	case store.InvoiceUnpaid:
	default:
		return nil, fmt.Errorf("%w: the invoice is %s", ErrConflict, inv.Status)
	}
	if inv.Balance() <= 0 {
		s.payMu.Lock()
		defer s.payMu.Unlock()
		if _, err := s.issuedLocked(ctx, cfg, inv, false); err != nil {
			return nil, err
		}
		return &PayNext{Paid: true}, nil
	}
	if !slices.Contains(cfg.methodIDs(), method) {
		return nil, fmt.Errorf("%w: %q isn't a payment method available now", ErrInvalid, method)
	}
	return s.startPayment(ctx, cfg, inv, method, saveCard)
}

func (s *Service) startPayment(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice, method string, saveCard bool) (*PayNext, error) {
	switch method {
	case MethodStripe:
		u, err := s.stripeCheckout(ctx, cfg, inv, saveCard)
		if err != nil {
			return nil, err
		}
		return &PayNext{RedirectURL: u}, nil
	case MethodRazorpay:
		u, err := s.razorpayLink(ctx, cfg, inv)
		if err != nil {
			return nil, err
		}
		return &PayNext{RedirectURL: u}, nil
	case MethodManual:
		ins := cfg.Methods.Manual.Instructions
		if ins == "" {
			ins = "Please pay by bank transfer, quoting the invoice number. Contact us for the bank details."
		}
		return &PayNext{Instructions: ins, Reference: displayNumber(inv)}, nil
	}
	return nil, fmt.Errorf("%w: unknown payment method %q", ErrInvalid, method)
}

// orderPaidLocked activates a paid order's account, unless orders wait
// for approval.
func (s *Service) orderPaidLocked(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice) error {
	o, err := s.Store.OrderByInvoice(ctx, inv.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || o.Status != store.OrderPending {
		return err
	}
	if cfg.Automation.OrdersNeedApproval {
		s.event(ctx, o.AccountID, "order", "Order paid; waiting for approval")
		return nil
	}
	return s.activateOrderLocked(ctx, o)
}

// activateOrderLocked makes an order's account active: billed from today
// for a whole cycle, welcomed, announced (account.created).
func (s *Service) activateOrderLocked(ctx context.Context, o *store.Order) error {
	a, err := s.Store.GetAccount(ctx, o.AccountID)
	if err != nil {
		return err
	}
	if a.Status == store.AccountPending {
		today := Day(s.now())
		p, err := s.profile(ctx, a.ID)
		if err != nil {
			return err
		}
		p.Mode, p.Cycle = ModeInvoice, o.Cycle
		p.NextDueAt, p.AnchorDay = AddMonths(today, CycleMonths(o.Cycle), today.Day()), today.Day()
		if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
			return err
		}
		// The order's invoice paid for this first period (a plan change
		// credits its unused part from it).
		if err := s.Store.SetInvoicePeriod(ctx, o.InvoiceID, today, p.NextDueAt); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		s.mu.Lock()
		err = s.Store.SetAccountStatus(ctx, a.ID, store.AccountActive, "", s.now())
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	ok, err := s.Store.SetOrderStatus(ctx, o.ID, store.OrderPending, store.OrderActive)
	if err != nil || !ok {
		return err
	}
	a, err = s.Store.GetAccount(ctx, o.AccountID)
	if err != nil {
		return err
	}
	s.event(ctx, a.ID, "account", "Order accepted: account active")
	s.emit(ctx, EventAccountCreated, map[string]any{"account": a})
	planName := a.PlanID
	if plan, err := s.Store.GetPlan(ctx, a.PlanID); err == nil {
		planName = plan.Name
	}
	username := ""
	if users, err := s.Store.AccountUsers(ctx, a.ID); err == nil && len(users) > 0 {
		username = users[0].Username
	}
	s.mail(ctx, a.ID, "account.welcome", fmt.Sprintf("account.welcome:%d", a.ID), map[string]any{"Plan": planName,
		"Username": username})
	return nil
}

// OrderView is an order as staff see it.
type OrderView struct {
	ID            int64     `json:"id"`
	AccountID     int64     `json:"account_id"`
	AccountName   string    `json:"account_name"`
	AccountStatus string    `json:"account_status"`
	Email         string    `json:"email"`
	PlanID        string    `json:"plan_id"`
	PlanName      string    `json:"plan_name"`
	Cycle         string    `json:"cycle"`
	PromoCode     string    `json:"promo_code"`
	Total         int64     `json:"total"`
	InvoiceID     int64     `json:"invoice_id"`
	InvoiceNumber string    `json:"invoice_number"`
	InvoiceStatus string    `json:"invoice_status"`
	IP            string    `json:"ip"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// Orders lists orders ("" status: all).
func (s *Service) Orders(ctx context.Context, status string) ([]OrderView, error) {
	if status != "" && status != store.OrderPending && status != store.OrderActive && status != store.OrderCancelled {
		return nil, fmt.Errorf("%w: status is pending, active or cancelled", ErrInvalid)
	}
	list, err := s.Store.Orders(ctx, status, 500)
	if err != nil {
		return nil, err
	}
	out := make([]OrderView, 0, len(list))
	for _, o := range list {
		v := OrderView{ID: o.ID, AccountID: o.AccountID, PlanID: o.PlanID, PlanName: o.PlanID, Cycle: o.Cycle,
			PromoCode: o.PromoCode, Total: o.Total, InvoiceID: o.InvoiceID, IP: o.IP, Status: o.Status, CreatedAt: o.CreatedAt}
		if a, err := s.Store.GetAccount(ctx, o.AccountID); err == nil {
			v.AccountName, v.AccountStatus, v.Email = a.Name, a.Status, a.Email
		}
		if p, err := s.Store.GetPlan(ctx, o.PlanID); err == nil {
			v.PlanName = p.Name
		}
		if inv, err := s.Store.GetInvoice(ctx, o.InvoiceID); err == nil {
			v.InvoiceNumber, v.InvoiceStatus = displayNumber(inv), inv.Status
		}
		out = append(out, v)
	}
	return out, nil
}

// AcceptOrder activates a pending order's account, paid or not (its
// invoice stays to be paid).
func (s *Service) AcceptOrder(ctx context.Context, id int64) error {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	o, err := s.Store.GetOrder(ctx, id)
	if err != nil {
		return err
	}
	if o.Status != store.OrderPending {
		return fmt.Errorf("%w: the order is %s", ErrConflict, o.Status)
	}
	return s.activateOrderLocked(ctx, o)
}

// CancelOrder cancels a pending order: its invoice (if unpaid) and its
// account (terminated). A paid invoice is left for staff to refund.
func (s *Service) CancelOrder(ctx context.Context, id int64, by string) error {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	o, err := s.Store.GetOrder(ctx, id)
	if err != nil {
		return err
	}
	if o.Status != store.OrderPending {
		return fmt.Errorf("%w: the order is %s", ErrConflict, o.Status)
	}
	return s.cancelOrderLocked(ctx, o, by)
}

func (s *Service) cancelOrderLocked(ctx context.Context, o *store.Order, by string) error {
	inv, err := s.Store.GetInvoice(ctx, o.InvoiceID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if inv != nil && (inv.Status == store.InvoiceUnpaid || inv.Status == store.InvoiceDraft) {
		if err := s.cancelInvoiceLocked(ctx, inv.ID, by, false); err != nil {
			return err
		}
	}
	ok, err := s.Store.SetOrderStatus(ctx, o.ID, store.OrderPending, store.OrderCancelled)
	if err != nil || !ok {
		return err
	}
	a, err := s.Store.GetAccount(ctx, o.AccountID)
	if err != nil {
		return err
	}
	if a.Status == store.AccountPending {
		if _, err := s.Terminate(ctx, a.ID, false); err != nil {
			return err
		}
	}
	s.event(ctx, a.ID, "order", "Order cancelled ("+by+")")
	return nil
}
