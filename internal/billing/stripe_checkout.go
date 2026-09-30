package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/parthh37/wpgenie/internal/store"
)

// Invoices paid by card through Stripe: a Checkout Session (mode payment)
// for the invoice's balance, optionally saving the card for later; saved
// cards are charged off-session on the due date (auto-pay). Stripe's
// webhook (the same endpoint as subscriptions) reports the payments:
// checkout.session.completed and payment_intent.succeeded both carry the
// invoice in their metadata and are recorded once by PaymentIntent ID;
// charge.refunded records refunds made in Stripe's dashboard.

type stripeSession struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Mode          string            `json:"mode"`
	PaymentStatus string            `json:"payment_status"`
	PaymentIntent stripeRef         `json:"payment_intent"`
	AmountTotal   int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
	Customer      stripeRef         `json:"customer"`
	Metadata      map[string]string `json:"metadata"`
}

type stripePaymentIntent struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	AmountReceived int64             `json:"amount_received"`
	Currency       string            `json:"currency"`
	Customer       stripeRef         `json:"customer"`
	PaymentMethod  stripeRef         `json:"payment_method"`
	Metadata       map[string]string `json:"metadata"`
}

type stripeCharge struct {
	ID             string    `json:"id"`
	PaymentIntent  stripeRef `json:"payment_intent"`
	AmountRefunded int64     `json:"amount_refunded"`
}

type stripePaymentMethod struct {
	ID   string `json:"id"`
	Card struct {
		Brand    string `json:"brand"`
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
}

// CreateCustomer makes the Stripe customer an account's cards belong to.
func (a *StripeAPI) CreateCustomer(ctx context.Context, key, email, name string, accountID int64) (string, error) {
	form := url.Values{"metadata[wpgenie_account]": {strconv.FormatInt(accountID, 10)}}
	if email != "" {
		form.Set("email", email)
	}
	if name != "" {
		form.Set("name", name)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := a.callIdem(ctx, key, fmt.Sprintf("wpgenie-customer-%d", accountID), http.MethodPost, "/v1/customers", form, &out); err != nil {
		return "", err
	}
	if !stripeIDRe.MatchString(out.ID) {
		return "", fmt.Errorf("stripe: unexpected customer ID %q", out.ID)
	}
	return out.ID, nil
}

// CheckoutSession creates a Checkout Session and returns it (its URL).
func (a *StripeAPI) CheckoutSession(ctx context.Context, key string, form url.Values) (*stripeSession, error) {
	var out stripeSession
	if err := a.call(ctx, key, http.MethodPost, "/v1/checkout/sessions", form, &out); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(out.URL, "https://") {
		return nil, fmt.Errorf("stripe: the checkout session has no URL")
	}
	return &out, nil
}

// PaymentIntent fetches a PaymentIntent.
func (a *StripeAPI) PaymentIntent(ctx context.Context, key, id string) (*stripePaymentIntent, error) {
	if !stripeIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid PaymentIntent ID", ErrInvalid)
	}
	var out stripePaymentIntent
	return &out, a.call(ctx, key, http.MethodGet, "/v1/payment_intents/"+id, nil, &out)
}

// Charge creates and confirms a PaymentIntent (an off-session charge of a
// saved card). A decline is an error.
func (a *StripeAPI) Charge(ctx context.Context, key, idem string, form url.Values) (*stripePaymentIntent, error) {
	var out stripePaymentIntent
	if err := a.callIdem(ctx, key, idem, http.MethodPost, "/v1/payment_intents", form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaymentMethod fetches a saved card's details.
func (a *StripeAPI) PaymentMethod(ctx context.Context, key, id string) (*stripePaymentMethod, error) {
	if !stripeIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid payment method ID", ErrInvalid)
	}
	var out stripePaymentMethod
	return &out, a.call(ctx, key, http.MethodGet, "/v1/payment_methods/"+id, nil, &out)
}

// DetachPaymentMethod removes a saved card from its customer.
func (a *StripeAPI) DetachPaymentMethod(ctx context.Context, key, id string) error {
	if !stripeIDRe.MatchString(id) {
		return fmt.Errorf("%w: invalid payment method ID", ErrInvalid)
	}
	return a.call(ctx, key, http.MethodPost, "/v1/payment_methods/"+id+"/detach", url.Values{}, nil)
}

// Refund refunds (part of) a PaymentIntent and returns the refund's ID.
func (a *StripeAPI) Refund(ctx context.Context, key, paymentIntent string, amount int64, idem string) (string, error) {
	if !stripeIDRe.MatchString(paymentIntent) {
		return "", fmt.Errorf("%w: the payment has no Stripe reference", ErrInvalid)
	}
	var out struct {
		ID string `json:"id"`
	}
	form := url.Values{"payment_intent": {paymentIntent}, "amount": {strconv.FormatInt(amount, 10)}}
	if err := a.callIdem(ctx, key, idem, http.MethodPost, "/v1/refunds", form, &out); err != nil {
		return "", err
	}
	if !stripeIDRe.MatchString(out.ID) {
		return "", fmt.Errorf("stripe: unexpected refund ID %q", out.ID)
	}
	return out.ID, nil
}

// stripeCustomer returns the account's Stripe customer, making it on
// first use. Caller holds payMu.
func (s *Service) stripeCustomerLocked(ctx context.Context, key string, a *store.Account, p *store.BillingProfile) (string, error) {
	if p.StripeCustomer != "" {
		return p.StripeCustomer, nil
	}
	name := strings.TrimSpace(p.Contact.FirstName + " " + p.Contact.LastName)
	if p.Contact.Company != "" {
		name = p.Contact.Company
	}
	if name == "" {
		name = a.Name
	}
	id, err := s.Stripe.CreateCustomer(ctx, key, recipient(a, p), truncateRunes(name, 100), a.ID)
	if err != nil {
		return "", err
	}
	p.StripeCustomer = id
	return id, s.Store.SaveBillingProfile(ctx, p)
}

// stripeCheckout opens a Checkout Session for an invoice's balance.
func (s *Service) stripeCheckout(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice, saveCard bool) (string, error) {
	sc, err := s.StripeSettings(ctx)
	if err != nil {
		return "", err
	}
	if sc.SecretKey == "" || s.Stripe == nil {
		return "", fmt.Errorf("%w: card payments aren't configured", ErrConflict)
	}
	s.payMu.Lock()
	a, err := s.Store.GetAccount(ctx, inv.AccountID)
	var p *store.BillingProfile
	var customer string
	if err == nil {
		if p, err = s.profile(ctx, inv.AccountID); err == nil {
			customer, err = s.stripeCustomerLocked(ctx, sc.SecretKey, a, p)
		}
	}
	s.payMu.Unlock()
	if err != nil {
		return "", err
	}
	id := strconv.FormatInt(inv.ID, 10)
	back := s.InvoiceURL(inv.ID)
	form := url.Values{
		"mode": {"payment"}, "client_reference_id": {id}, "customer": {customer},
		"line_items[0][quantity]":                        {"1"},
		"line_items[0][price_data][currency]":            {strings.ToLower(inv.Currency)},
		"line_items[0][price_data][unit_amount]":         {strconv.FormatInt(inv.Balance(), 10)},
		"line_items[0][price_data][product_data][name]":  {"Invoice " + displayNumber(inv)},
		"metadata[wpgenie_invoice]":                      {id},
		"payment_intent_data[metadata][wpgenie_invoice]": {id},
		"payment_intent_data[description]":               {"Invoice " + displayNumber(inv)},
		"success_url":                                    {back + "?paid=1"},
		"cancel_url":                                     {back},
	}
	if saveCard {
		form.Set("payment_intent_data[setup_future_usage]", "off_session")
		form.Set("metadata[wpgenie_save_card]", "1")
		form.Set("payment_intent_data[metadata][wpgenie_save_card]", "1")
	}
	sess, err := s.Stripe.CheckoutSession(ctx, sc.SecretKey, form)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return sess.URL, nil
}

// stripeInvoiceEvent processes a webhook event about an invoice payment.
// handled is false for events that aren't (a subscription's checkout, a
// charge WPGenie didn't take): processStripe goes on with them.
func (s *Service) stripeInvoiceEvent(ctx context.Context, cfg *StripeSettings, ev *stripeEvent) (bool, error) {
	switch ev.Type {
	case "checkout.session.completed":
		var c stripeSession
		if err := json.Unmarshal(ev.Data.Object, &c); err != nil {
			return false, err
		}
		if c.Metadata["wpgenie_invoice"] == "" {
			return false, nil
		}
		if c.Mode != "payment" || c.PaymentStatus != "paid" || c.PaymentIntent == "" {
			// Paid later (a delayed method) or not at all: the
			// PaymentIntent's own event reports it.
			return true, nil
		}
		pm := ""
		if c.Metadata["wpgenie_save_card"] == "1" && cfg.SecretKey != "" && s.Stripe != nil {
			pi, err := s.Stripe.PaymentIntent(ctx, cfg.SecretKey, string(c.PaymentIntent))
			if err != nil {
				return true, err
			}
			pm = string(pi.PaymentMethod)
		}
		return true, s.stripePaid(ctx, cfg, c.Metadata, string(c.PaymentIntent), c.AmountTotal, c.Currency, string(c.Customer), pm)
	case "payment_intent.succeeded":
		var pi stripePaymentIntent
		if err := json.Unmarshal(ev.Data.Object, &pi); err != nil {
			return false, err
		}
		if pi.Metadata["wpgenie_invoice"] == "" {
			return false, nil
		}
		return true, s.stripePaid(ctx, cfg, pi.Metadata, pi.ID, pi.AmountReceived, pi.Currency, string(pi.Customer),
			string(pi.PaymentMethod))
	case "charge.refunded":
		var ch stripeCharge
		if err := json.Unmarshal(ev.Data.Object, &ch); err != nil {
			return false, err
		}
		if ch.PaymentIntent == "" {
			return false, nil
		}
		pay, err := s.Store.PaymentByDedupe(ctx, "stripe:"+string(ch.PaymentIntent))
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return true, err
		}
		// Stripe says how much of the charge is refunded in all; what we
		// haven't recorded yet (a refund made in Stripe's dashboard) is new.
		s.payMu.Lock()
		defer s.payMu.Unlock()
		if pay, err = s.Store.GetPayment(ctx, pay.ID); err != nil {
			return true, err
		}
		// Only refunds that went back through Stripe count: one made to the
		// account's credit is no part of the charge's refunded total.
		viaStripe, err := s.Store.GatewayRefunded(ctx, pay.ID)
		if err != nil {
			return true, err
		}
		if d := ch.AmountRefunded - viaStripe; d > 0 {
			r, _, err := s.Store.RecordRefund(ctx, store.RefundInput{PaymentID: pay.ID, Amount: d, Reference: ch.ID, By: "stripe",
				At: s.now(), Dedupe: fmt.Sprintf("stripe:%s:refunded:%d", ch.ID, ch.AmountRefunded), Made: true})
			if errors.Is(err, store.ErrOverRefund) {
				s.Log.Warn("Stripe reports a refund beyond the payment", "payment", pay.ID, "charge", ch.ID)
				return true, nil
			}
			if err != nil {
				return true, err
			}
			s.event(ctx, pay.AccountID, "billing", fmt.Sprintf("Refund made in Stripe recorded on invoice %s", pay.InvoiceNumber))
			s.refundShortfall(ctx, r, pay.AccountID)
		}
		return true, nil
	}
	return false, nil
}

// refundShortfall notes a refund made at a gateway that should have taken
// back credit the account had already spent.
func (s *Service) refundShortfall(ctx context.Context, r *store.Refund, accountID int64) {
	if r == nil || r.Shortfall <= 0 {
		return
	}
	s.Log.Warn("a gateway refund of credited money that was spent", "account", accountID, "refund", r.ID, "shortfall", r.Shortfall)
	s.event(ctx, accountID, "billing", fmt.Sprintf("A refund made at the gateway included %d (minor units) that had become "+
		"credit and was spent since: the account received it twice", r.Shortfall))
}

// stripePaid records a Stripe payment of an invoice (once per
// PaymentIntent) and saves the card when the client asked to.
func (s *Service) stripePaid(ctx context.Context, sc *StripeSettings, meta map[string]string, pi string, amount int64, currency, customer, pm string) error {
	id, err := strconv.ParseInt(meta["wpgenie_invoice"], 10, 64)
	if err != nil {
		return nil // not an ID of ours: nothing to do
	}
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return err
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	inv, err := s.Store.GetInvoice(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.Log.Warn("Stripe payment for an invoice that doesn't exist", "invoice", id, "payment_intent", pi)
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(currency, inv.Currency) {
		s.Log.Error("Stripe payment in another currency than its invoice's: record it by hand", "invoice", id,
			"payment_intent", pi, "currency", currency)
		s.event(ctx, inv.AccountID, "billing", fmt.Sprintf("Stripe payment %s is in %s, the invoice in %s: record it by hand",
			pi, strings.ToUpper(currency), inv.Currency))
		return nil
	}
	if amount > 0 {
		if _, err := s.recordPaymentLocked(ctx, cfg, id, PaymentInput{Gateway: MethodStripe, Reference: pi, Amount: amount,
			By: "stripe", Dedupe: "stripe:" + pi}); err != nil {
			return err
		}
	}
	if meta["wpgenie_save_card"] == "1" && pm != "" && sc.SecretKey != "" && s.Stripe != nil {
		if err := s.saveCardLocked(ctx, sc.SecretKey, inv.AccountID, customer, pm); err != nil {
			// The payment is recorded; a card not saved isn't worth a retry.
			s.Log.Warn("saving a card", "account", inv.AccountID, "err", err)
		}
	}
	return nil
}

func (s *Service) saveCardLocked(ctx context.Context, key string, accountID int64, customer, pm string) error {
	p, err := s.profile(ctx, accountID)
	if err != nil {
		return err
	}
	if p.CardPM == pm {
		return nil
	}
	m, err := s.Stripe.PaymentMethod(ctx, key, pm)
	if err != nil {
		return err
	}
	if customer != "" {
		p.StripeCustomer = customer
	}
	p.CardPM, p.CardBrand, p.CardLast4 = m.ID, truncateRunes(m.Card.Brand, 20), truncateRunes(m.Card.Last4, 4)
	p.CardExpMonth, p.CardExpYear = m.Card.ExpMonth, m.Card.ExpYear
	if err := s.Store.SaveBillingProfile(ctx, p); err != nil {
		return err
	}
	s.event(ctx, accountID, "billing", fmt.Sprintf("Card saved (%s ending %s)", p.CardBrand, p.CardLast4))
	return nil
}

// autocharge charges an account's saved card for an invoice, once per
// invoice per day. A decline (or a card needing the client, 3-D Secure)
// e-mails them a link to pay. It reports whether a charge was tried today,
// and whether it paid the invoice.
func (s *Service) autocharge(ctx context.Context, cfg *InvoicingSettings, sc *StripeSettings, inv *store.Invoice, p *store.BillingProfile) (tried, paid bool, err error) {
	day := Day(s.now()).Format("2006-01-02")
	if ok, err := s.Store.SetInvoiceAutocharge(ctx, inv.ID, day); err != nil || !ok {
		return false, false, err
	}
	bal := inv.Balance()
	id := strconv.FormatInt(inv.ID, 10)
	form := url.Values{"amount": {strconv.FormatInt(bal, 10)}, "currency": {strings.ToLower(inv.Currency)},
		"customer": {p.StripeCustomer}, "payment_method": {p.CardPM}, "off_session": {"true"}, "confirm": {"true"},
		"metadata[wpgenie_invoice]": {id}, "description": {"Invoice " + displayNumber(inv)}}
	pi, err := s.Stripe.Charge(ctx, sc.SecretKey, fmt.Sprintf("wpgenie-autocharge-%d-%s-%d", inv.ID, day, bal), form)
	if err == nil && pi.Status != "succeeded" {
		err = fmt.Errorf("the payment needs your confirmation (%s)", strings.ReplaceAll(pi.Status, "_", " "))
	}
	if err != nil {
		reason := strings.TrimPrefix(err.Error(), "stripe: ")
		if _, msg, ok := strings.Cut(reason, ": "); ok && strings.HasPrefix(reason, "402") {
			reason = msg
		}
		s.event(ctx, inv.AccountID, "billing", "Charging the saved card failed for invoice "+displayNumber(inv)+": "+reason)
		s.mailInvoice(ctx, cfg, inv, "invoice.payment_failed", fmt.Sprintf("invoice.payment_failed:%d:%s", inv.ID, day),
			map[string]any{"Reason": reason})
		return true, false, nil
	}
	amount := pi.AmountReceived
	if amount <= 0 {
		amount = bal
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	res, err := s.recordPaymentLocked(ctx, cfg, inv.ID, PaymentInput{Gateway: MethodStripe, Reference: pi.ID,
		Amount: amount, By: "auto-pay", Dedupe: "stripe:" + pi.ID})
	if err != nil {
		return true, false, err
	}
	return true, res.Paid, nil
}
