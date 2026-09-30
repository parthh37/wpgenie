package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Razorpay (UPI, cards, netbanking, wallets) through Payment Links: a link
// per attempt at paying an invoice (reference wpg_<invoice>_<attempt>),
// Razorpay's page, then back to the panel's callback with a signed result.
// The webhook (payment_link.paid, refund.processed) reports the same
// payments when the client closes the page too early; both are recorded
// once by Razorpay's payment ID. Signatures are HMAC-SHA256, compared in
// constant time: the callback's with the key secret, the webhook's with
// the webhook secret.

var (
	ErrRazorpayOff     = errors.New("Razorpay is not configured")
	ErrRazorpaySigned  = errors.New("invalid Razorpay signature")
	razorpayIDRe       = regexp.MustCompile(`^[a-z]{2,6}_[A-Za-z0-9]{6,40}$`)
	razorpayRefRe      = regexp.MustCompile(`^wpg_([0-9]{1,18})_[0-9]{1,9}$`)
	errRazorpayUnknown = errors.New("not a payment WPGenie asked for")
)

// RazorpayAPI calls Razorpay's REST API (JSON, the key ID and secret as
// basic auth).
type RazorpayAPI struct {
	Base   string // default https://api.razorpay.com
	Client *http.Client
}

func (s *Service) razorpay() *RazorpayAPI {
	if s.Razorpay != nil {
		return s.Razorpay
	}
	return &RazorpayAPI{}
}

func (a *RazorpayAPI) call(ctx context.Context, keyID, secret, method, path string, body, out any) error {
	base := a.Base
	if base == "" {
		base = "https://api.razorpay.com"
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(keyID, secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("razorpay: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Description string `json:"description"`
			} `json:"error"`
		}
		json.Unmarshal(b, &e)
		return fmt.Errorf("razorpay: %s: %s", resp.Status, e.Error.Description)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type razorpayLink struct {
	ID          string `json:"id"`
	ShortURL    string `json:"short_url"`
	ReferenceID string `json:"reference_id"`
}

// PaymentLink creates a payment link.
func (a *RazorpayAPI) PaymentLink(ctx context.Context, keyID, secret string, req map[string]any) (*razorpayLink, error) {
	var out razorpayLink
	if err := a.call(ctx, keyID, secret, http.MethodPost, "/v1/payment_links", req, &out); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(out.ShortURL, "https://") {
		return nil, errors.New("razorpay: the payment link has no URL")
	}
	return &out, nil
}

type razorpayPayment struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
	Fee      int64  `json:"fee"`
}

// Payment fetches a payment.
func (a *RazorpayAPI) Payment(ctx context.Context, keyID, secret, id string) (*razorpayPayment, error) {
	if !razorpayIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid Razorpay payment ID", ErrInvalid)
	}
	var out razorpayPayment
	return &out, a.call(ctx, keyID, secret, http.MethodGet, "/v1/payments/"+id, nil, &out)
}

// Refund refunds (part of) a payment and returns the refund's ID.
func (a *RazorpayAPI) Refund(ctx context.Context, keyID, secret, paymentID string, amount, invoiceID int64) (string, error) {
	if !razorpayIDRe.MatchString(paymentID) {
		return "", fmt.Errorf("%w: the payment has no Razorpay reference", ErrInvalid)
	}
	var out struct {
		ID string `json:"id"`
	}
	body := map[string]any{"amount": amount, "notes": map[string]string{"wpgenie_invoice": strconv.FormatInt(invoiceID, 10)}}
	if err := a.call(ctx, keyID, secret, http.MethodPost, "/v1/payments/"+paymentID+"/refund", body, &out); err != nil {
		return "", err
	}
	if !razorpayIDRe.MatchString(out.ID) {
		return "", fmt.Errorf("razorpay: unexpected refund ID %q", out.ID)
	}
	return out.ID, nil
}

func hmacHex(secret string, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, p := range parts {
		mac.Write(p)
	}
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

// VerifyRazorpayCallback checks the signature of a payment link's
// redirect: HMAC-SHA256(link_id|reference_id|status|payment_id, key secret).
func VerifyRazorpayCallback(q url.Values, secret string) error {
	if secret == "" {
		return ErrRazorpayOff
	}
	msg := q.Get("razorpay_payment_link_id") + "|" + q.Get("razorpay_payment_link_reference_id") + "|" +
		q.Get("razorpay_payment_link_status") + "|" + q.Get("razorpay_payment_id")
	if !hmac.Equal(hmacHex(secret, []byte(msg)), []byte(strings.ToLower(q.Get("razorpay_signature")))) {
		return ErrRazorpaySigned
	}
	return nil
}

// VerifyRazorpayWebhook checks X-Razorpay-Signature: HMAC-SHA256 of the
// raw body with the webhook secret.
func VerifyRazorpayWebhook(body []byte, sig, secret string) error {
	if secret == "" {
		return ErrRazorpayOff
	}
	if !hmac.Equal(hmacHex(secret, body), []byte(strings.ToLower(sig))) {
		return ErrRazorpaySigned
	}
	return nil
}

// razorpayLink makes a payment link for an invoice's balance.
func (s *Service) razorpayLink(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice) (string, error) {
	rz := cfg.Methods.Razorpay
	if rz.KeyID == "" || str(rz.KeySecret) == "" {
		return "", fmt.Errorf("%w: %v", ErrConflict, ErrRazorpayOff)
	}
	attempt, err := s.Store.NextPayAttempt(ctx, inv.ID)
	if err != nil {
		return "", err
	}
	a, err := s.Store.GetAccount(ctx, inv.AccountID)
	if err != nil {
		return "", err
	}
	p, err := s.profile(ctx, inv.AccountID)
	if err != nil {
		return "", err
	}
	customer := map[string]string{"name": truncateRunes(inv.BillingAddress.Name, 100)}
	if to := recipient(a, p); to != "" {
		customer["email"] = to
	}
	if c := strings.TrimSpace(p.Contact.Phone); c != "" {
		customer["contact"] = c
	}
	req := map[string]any{
		"amount": inv.Balance(), "currency": inv.Currency, "accept_partial": false,
		"description":  truncateRunes("Invoice "+displayNumber(inv), 2048),
		"reference_id": fmt.Sprintf("wpg_%d_%d", inv.ID, attempt),
		"customer":     customer, "notify": map[string]bool{"sms": false, "email": false}, "reminder_enable": false,
		"notes":        map[string]string{"wpgenie_invoice": strconv.FormatInt(inv.ID, 10)},
		"callback_url": s.PanelURL + "/api/v1/billing/razorpay/callback", "callback_method": "get",
	}
	link, err := s.razorpay().PaymentLink(ctx, rz.KeyID, str(rz.KeySecret), req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return link.ShortURL, nil
}

// RazorpayCallback records the payment a client comes back with from a
// payment link and returns the invoice to show them.
func (s *Service) RazorpayCallback(ctx context.Context, q url.Values) (int64, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return 0, err
	}
	rz := cfg.Methods.Razorpay
	if err := VerifyRazorpayCallback(q, str(rz.KeySecret)); err != nil {
		return 0, err
	}
	m := razorpayRefRe.FindStringSubmatch(q.Get("razorpay_payment_link_reference_id"))
	if m == nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, errRazorpayUnknown)
	}
	invoiceID, _ := strconv.ParseInt(m[1], 10, 64)
	if q.Get("razorpay_payment_link_status") != "paid" {
		return invoiceID, nil // cancelled or expired: back to the invoice, still unpaid
	}
	pay, err := s.razorpay().Payment(ctx, rz.KeyID, str(rz.KeySecret), q.Get("razorpay_payment_id"))
	if err != nil {
		return invoiceID, err
	}
	return invoiceID, s.razorpayPaid(ctx, cfg, invoiceID, pay)
}

// razorpayPaid records a captured Razorpay payment once.
func (s *Service) razorpayPaid(ctx context.Context, cfg *InvoicingSettings, invoiceID int64, pay *razorpayPayment) error {
	if pay.Status != "captured" && pay.Status != "authorized" {
		return nil
	}
	s.payMu.Lock()
	defer s.payMu.Unlock()
	inv, err := s.Store.GetInvoice(ctx, invoiceID)
	if errors.Is(err, store.ErrNotFound) {
		s.Log.Warn("Razorpay payment for an invoice that doesn't exist", "invoice", invoiceID, "payment", pay.ID)
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(pay.Currency, inv.Currency) {
		s.event(ctx, inv.AccountID, "billing", fmt.Sprintf("Razorpay payment %s is in %s, the invoice in %s: record it by hand",
			pay.ID, pay.Currency, inv.Currency))
		return nil
	}
	_, err = s.recordPaymentLocked(ctx, cfg, invoiceID, PaymentInput{Gateway: MethodRazorpay, Reference: pay.ID,
		Amount: pay.Amount, Fee: pay.Fee, By: "razorpay", Dedupe: "razorpay:" + pay.ID})
	return err
}

type razorpayEvent struct {
	Event   string `json:"event"`
	Payload struct {
		PaymentLink struct {
			Entity struct {
				ID          string            `json:"id"`
				ReferenceID string            `json:"reference_id"`
				Notes       map[string]string `json:"notes"`
			} `json:"entity"`
		} `json:"payment_link"`
		Payment struct {
			Entity razorpayPayment `json:"entity"`
		} `json:"payment"`
		Refund struct {
			Entity struct {
				ID        string `json:"id"`
				PaymentID string `json:"payment_id"`
				Amount    int64  `json:"amount"`
			} `json:"entity"`
		} `json:"refund"`
	} `json:"payload"`
}

// HandleRazorpayWebhook authenticates and processes a webhook call. An
// error other than ErrRazorpayOff, ErrRazorpaySigned or ErrInvalid means
// Razorpay should retry.
func (s *Service) HandleRazorpayWebhook(ctx context.Context, body []byte, sig string) error {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return err
	}
	if err := VerifyRazorpayWebhook(body, sig, str(cfg.Methods.Razorpay.WebhookSecret)); err != nil {
		return err
	}
	var ev razorpayEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Event == "" {
		return fmt.Errorf("%w: not a Razorpay event", ErrInvalid)
	}
	switch ev.Event {
	case "payment_link.paid":
		link := ev.Payload.PaymentLink.Entity
		m := razorpayRefRe.FindStringSubmatch(link.ReferenceID)
		if m == nil {
			return nil // a link someone else made
		}
		invoiceID, _ := strconv.ParseInt(m[1], 10, 64)
		pay := ev.Payload.Payment.Entity
		if !razorpayIDRe.MatchString(pay.ID) || pay.Amount <= 0 {
			return fmt.Errorf("%w: payment_link.paid without a payment", ErrInvalid)
		}
		return s.razorpayPaid(ctx, cfg, invoiceID, &pay)
	case "refund.processed":
		r := ev.Payload.Refund.Entity
		if r.PaymentID == "" || r.Amount <= 0 {
			return nil
		}
		pay, err := s.Store.PaymentByDedupe(ctx, "razorpay:"+r.PaymentID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		s.payMu.Lock()
		defer s.payMu.Unlock()
		out, _, err := s.Store.RecordRefund(ctx, store.RefundInput{PaymentID: pay.ID, Amount: r.Amount, Reference: r.ID,
			By: "razorpay", At: s.now(), Dedupe: "razorpay:" + r.ID})
		if errors.Is(err, store.ErrOverRefund) {
			s.Log.Warn("Razorpay reports a refund beyond the payment", "payment", pay.ID, "refund", r.ID)
			return nil
		}
		if err == nil && out != nil {
			s.event(ctx, pay.AccountID, "billing", "Refund made in Razorpay recorded on invoice "+pay.InvoiceNumber)
		}
		return err
	}
	return nil
}
