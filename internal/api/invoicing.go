package api

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Built-in billing (internal/billing's invoicing): billing profiles,
// invoices, payments, promotions, taxes, orders, the automation, and the
// public order form's endpoints. Clients reach their own account's
// billing and invoices (a reseller's invoices are its own: never its
// customers', whom WPGenie doesn't bill); paying and the billing contact
// stay open to a suspended (or pending) account.

// invoicingRoutes registers this feature's routes (mux: its public, unsigned
// endpoints; r: authenticated routes).
func (s *Server) invoicingRoutes(mux *http.ServeMux, r func(string, string, handlerFunc)) {
	if s.Node {
		return // billing lives on the panel
	}
	s.storeGuard.Window = time.Hour
	// The order form (public; the CSRF header on POSTs, as for sign-in).
	mux.HandleFunc("GET /api/v1/store/catalog", s.storeCatalog)
	mux.Handle("POST /api/v1/store/quote", s.publicAuth(s.storeQuote))
	mux.Handle("POST /api/v1/store/orders", s.publicAuth(s.storeOrder))
	// Razorpay: the client's way back, and its webhook (signed).
	mux.HandleFunc("GET /api/v1/billing/razorpay/callback", s.razorpayCallback)
	mux.HandleFunc("POST /api/v1/billing/razorpay/webhook", s.razorpayWebhook)

	r("GET /api/v1/accounts/{id}/billing", viewer, s.billingProfile)
	r("PUT /api/v1/accounts/{id}/billing", admin, s.setBillingProfile)
	r("PUT /api/v1/accounts/{id}/billing/contact", admin, s.setBillingContact)
	r("PUT /api/v1/accounts/{id}/billing/auto-pay", admin, s.setAutoPay)
	r("DELETE /api/v1/accounts/{id}/billing/card", admin, s.forgetCard)
	r("GET /api/v1/accounts/{id}/credit", viewer, s.creditHistory)
	r("POST /api/v1/accounts/{id}/credit", admin, s.addCredit)
	r("POST /api/v1/accounts/{id}/plan-change/quote", viewer, s.quotePlanChange)
	r("POST /api/v1/accounts/{id}/plan-change", admin, s.changePlan)
	r("POST /api/v1/accounts/{id}/cancel", admin, s.cancelService)
	r("DELETE /api/v1/accounts/{id}/cancel", admin, s.withdrawCancel)
	r("POST /api/v1/accounts/{id}/burst/buy", admin, s.buyBurstPack)

	r("GET /api/v1/invoices", viewer, s.listInvoices)
	r("POST /api/v1/invoices", admin, s.createInvoice)
	r("GET /api/v1/invoices.csv", admin, s.invoicesCSV)
	r("GET /api/v1/invoices/{id}", viewer, s.getInvoice)
	r("GET /api/v1/invoices/{id}/print", viewer, s.printInvoice)
	r("PUT /api/v1/invoices/{id}", admin, s.updateInvoice)
	r("POST /api/v1/invoices/{id}/issue", admin, s.issueInvoice)
	r("POST /api/v1/invoices/{id}/cancel", admin, s.cancelInvoice)
	r("POST /api/v1/invoices/{id}/payments", admin, s.recordPayment)
	r("POST /api/v1/invoices/{id}/refund", admin, s.refundPayment)
	r("POST /api/v1/invoices/{id}/remind", admin, s.remindInvoice)
	r("POST /api/v1/invoices/{id}/apply-credit", admin, s.applyCredit)
	r("POST /api/v1/invoices/{id}/pay", operator, s.payInvoice)
	r("GET /api/v1/transactions", viewer, s.listTransactions)
	r("GET /api/v1/transactions.csv", admin, s.transactionsCSV)

	r("GET /api/v1/billing/invoicing", admin, s.invoicingSettings)
	r("PUT /api/v1/billing/invoicing", admin, s.setInvoicingSettings)
	r("GET /api/v1/billing/config", viewer, s.billingConfig)
	r("GET /api/v1/billing/overview", viewer, s.billingOverview)
	r("GET /api/v1/billing/promotions", admin, s.listPromotions)
	r("POST /api/v1/billing/promotions", admin, s.createPromotion)
	r("PUT /api/v1/billing/promotions/{id}", admin, s.updatePromotion)
	r("DELETE /api/v1/billing/promotions/{id}", admin, s.deletePromotion)
	r("GET /api/v1/billing/tax-rules", admin, s.listTaxRules)
	r("POST /api/v1/billing/tax-rules", admin, s.createTaxRule)
	r("PUT /api/v1/billing/tax-rules/{id}", admin, s.updateTaxRule)
	r("DELETE /api/v1/billing/tax-rules/{id}", admin, s.deleteTaxRule)
	r("GET /api/v1/billing/automation", admin, s.automationRuns)
	r("POST /api/v1/billing/automation/run", admin, s.runAutomation)
	r("GET /api/v1/orders", operator, s.listOrders)
	r("POST /api/v1/orders/{id}/accept", admin, s.acceptOrder)
	r("POST /api/v1/orders/{id}/cancel", admin, s.cancelOrder)
}

func init() {
	registerErrorStatus(billing.ErrStoreClosed, http.StatusNotFound)
	registerErrorStatus(billing.ErrRazorpayOff, http.StatusNotFound)
	registerErrorStatus(billing.ErrRazorpaySigned, http.StatusBadRequest)
	registerErrorStatus(store.ErrInvoiceState, http.StatusConflict)
	own := tenantRule{whileSuspended: true}
	registerTenantRoutes(map[string]tenantRule{
		// Their account's billing (a reseller also reads its customers').
		// Changes are to their own account only (checked in handlers).
		"GET /api/v1/accounts/{id}/billing":            anyTenant,
		"PUT /api/v1/accounts/{id}/billing/contact":    own,
		"PUT /api/v1/accounts/{id}/billing/auto-pay":   anyTenant,
		"DELETE /api/v1/accounts/{id}/billing/card":    anyTenant,
		"GET /api/v1/accounts/{id}/credit":             anyTenant,
		"POST /api/v1/accounts/{id}/plan-change/quote": own, // only prices it
		"POST /api/v1/accounts/{id}/plan-change":       anyTenant,
		"POST /api/v1/accounts/{id}/cancel":            anyTenant,
		"DELETE /api/v1/accounts/{id}/cancel":          anyTenant,
		"POST /api/v1/accounts/{id}/burst/buy":         own,
		// Their own invoices (see the scope below).
		"GET /api/v1/invoices":                    anyTenant, // filtered
		"GET /api/v1/invoices/{id}":               anyTenant,
		"GET /api/v1/invoices/{id}/print":         anyTenant,
		"POST /api/v1/invoices/{id}/apply-credit": own,
		"POST /api/v1/invoices/{id}/pay":          own,
		"GET /api/v1/transactions":                anyTenant, // filtered
		"GET /api/v1/billing/config":              anyTenant,
	})
	// An invoice is its account's: a tenant reaches only their own
	// account's, never a draft.
	registerScope("/api/v1/invoices/{id}", func(s *Server, ctx context.Context, p *Principal, id string) bool {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || s.Billing == nil {
			return false
		}
		inv, err := s.Store.GetInvoice(ctx, n)
		return err == nil && inv.AccountID == p.AccountID && inv.Status != store.InvoiceDraft
	})
}

// ownAccountOnly refuses a tenant changing another account's billing
// than their own (a reseller's customers are billed by the reseller).
func ownAccountOnly(r *http.Request, id int64) error {
	if t := tenantOf(r); t != nil && t.Account.ID != id {
		return fmt.Errorf("%w", store.ErrNotFound)
	}
	return nil
}

// actor is who a request acts as, for the ledgers.
func actor(r *http.Request) string {
	if p := principalFrom(r.Context()); p != nil {
		return p.Name
	}
	return "system"
}

func idParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%w", store.ErrNotFound)
	}
	return id, nil
}

// writeFieldError answers an input error in one field with the field, for
// forms to show it there.
func (s *Server) writeFieldError(w http.ResponseWriter, r *http.Request, err error) {
	if fe, ok := errors.AsType[*billing.FieldError](err); ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fe.Msg, "field": fe.Field})
		return
	}
	s.writeError(w, r, err)
}

// ---- Billing profiles ----

func (s *Server) billingProfile(w http.ResponseWriter, r *http.Request) error {
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	v, err := s.Billing.Profile(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) setBillingProfile(w http.ResponseWriter, r *http.Request) error {
	var in billing.ProfileInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	v, err := s.Billing.UpdateProfile(r.Context(), id, in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) setBillingContact(w http.ResponseWriter, r *http.Request) error {
	var in store.BillingContact
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	v, err := s.Billing.UpdateContact(r.Context(), id, in)
	if err != nil {
		s.writeFieldError(w, r, err)
		return nil
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) setAutoPay(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		AutoPay bool `json:"auto_pay"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	v, err := s.Billing.SetAutoPay(r.Context(), id, in.AutoPay)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) forgetCard(w http.ResponseWriter, r *http.Request) error {
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	v, err := s.Billing.ForgetCard(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) creditHistory(w http.ResponseWriter, r *http.Request) error {
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	list, err := s.Billing.CreditHistory(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) addCredit(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Amount      int64  `json:"amount"`
		Description string `json:"description"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	e, err := s.Billing.AddCredit(r.Context(), id, in.Amount, in.Description, actor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, e)
}

type planChangeInput struct {
	PlanID string `json:"plan_id"`
	Cycle  string `json:"cycle"`
}

func (s *Server) quotePlanChange(w http.ResponseWriter, r *http.Request) error {
	var in planChangeInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	q, err := s.Billing.QuotePlanChange(r.Context(), id, in.PlanID, in.Cycle, tenantOf(r) != nil)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, q)
}

func (s *Server) changePlan(w http.ResponseWriter, r *http.Request) error {
	var in planChangeInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	res, err := s.Billing.ChangePlan(r.Context(), id, in.PlanID, in.Cycle, tenantOf(r) != nil, actor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *Server) cancelService(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		When   string `json:"when"`
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	if tenantOf(r) != nil && in.When != "end_of_period" {
		return fmt.Errorf("%w: you can cancel at the end of the period you paid for", errForbidden)
	}
	v, err := s.Billing.RequestCancel(r.Context(), id, in.When, in.Reason)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) withdrawCancel(w http.ResponseWriter, r *http.Request) error {
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	v, err := s.Billing.WithdrawCancel(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// buyBurstPack invoices a burst minute pack and starts paying it (see
// payInvoice): {invoice, next}.
func (s *Server) buyBurstPack(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Pack   string `json:"pack"`
		Method string `json:"method"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if err := ownAccountOnly(r, id); err != nil {
		return err
	}
	out, err := s.Billing.BuyBurstPack(r.Context(), id, in.Pack, in.Method)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, out)
}

// ---- Invoices ----

// dateParam reads ?name= as a date (YYYY-MM-DD or RFC 3339).
func dateParam(r *http.Request, name string) (time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := billing.ParseDate(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s: %v", errBadRequest, name, err)
	}
	return t, nil
}

// int64Param reads an optional numeric query parameter.
func int64Param(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %s must be a number", errBadRequest, name)
	}
	return n, nil
}

// invoiceFilter reads a list's filters; a tenant sees only their own
// account's invoices, drafts left out.
func (s *Server) invoiceFilter(r *http.Request, def, maxLimit int) (store.InvoiceFilter, error) {
	q := r.URL.Query()
	var f store.InvoiceFilter
	var err error
	if f.Limit, err = limitParam(r, def, maxLimit); err != nil {
		return f, err
	}
	if f.AccountID, err = int64Param(r, "account"); err != nil {
		return f, err
	}
	if f.BeforeID, err = int64Param(r, "before"); err != nil {
		return f, err
	}
	if f.From, err = dateParam(r, "from"); err != nil {
		return f, err
	}
	if f.To, err = dateParam(r, "to"); err != nil {
		return f, err
	}
	if !f.To.IsZero() {
		f.To = f.To.AddDate(0, 0, 1) // to is inclusive: the whole day
	}
	switch st := q.Get("status"); st {
	case "", store.InvoiceDraft, store.InvoiceUnpaid, store.InvoicePaid, store.InvoiceCancelled, store.InvoiceRefunded,
		store.InvoicePartiallyRefunded:
		f.Status = st
	case "overdue":
		f.OverdueAt = s.now()
	default:
		return f, fmt.Errorf("%w: unknown status %q", errBadRequest, st)
	}
	if q.Get("overdue") == "1" || q.Get("overdue") == "true" {
		f.OverdueAt = s.now()
	}
	f.Q = strings.TrimSpace(q.Get("q"))
	if len(f.Q) > 100 {
		return f, fmt.Errorf("%w: q is too long", errBadRequest)
	}
	if t := tenantOf(r); t != nil {
		f.AccountID, f.NoDrafts = t.Account.ID, true
	}
	return f, nil
}

func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request) error {
	f, err := s.invoiceFilter(r, 50, 500)
	if err != nil {
		return err
	}
	list, err := s.Billing.Invoices(r.Context(), f)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) getInvoice(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	inv, err := s.Billing.Invoice(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inv)
}

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request) error {
	var in billing.ManualInvoiceInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	inv, err := s.Billing.CreateInvoice(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, inv)
}

func (s *Server) updateInvoice(w http.ResponseWriter, r *http.Request) error {
	var in billing.InvoiceUpdate
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	inv, err := s.Billing.UpdateInvoice(r.Context(), id, in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inv)
}

// optionalBody decodes a body that may be empty.
func optionalBody(w http.ResponseWriter, r *http.Request, into any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return decode(w, r, into)
}

func (s *Server) issueInvoice(w http.ResponseWriter, r *http.Request) error {
	in := struct {
		SendEmail *bool `json:"send_email"`
	}{}
	if err := optionalBody(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	inv, err := s.Billing.IssueInvoice(r.Context(), id, in.SendEmail == nil || *in.SendEmail)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inv)
}

func (s *Server) cancelInvoice(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	inv, err := s.Billing.CancelInvoice(r.Context(), id, actor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inv)
}

// recordPayment records a payment staff received outside the panel
// (bank, cash, cheque…); over-payment goes to the account's credit. The
// same method and reference are recorded once.
func (s *Server) recordPayment(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Amount    int64  `json:"amount"`
		Method    string `json:"method"`
		Reference string `json:"reference"`
		At        string `json:"at"`
		Note      string `json:"note"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if in.Method == "" {
		in.Method = "bank"
	}
	switch {
	case !slices.Contains(billing.ManualMethods, in.Method):
		return fmt.Errorf("%w: method is bank, cash, cheque or other", errBadRequest)
	case in.Amount <= 0 || in.Amount > 1_000_000_000_000:
		return fmt.Errorf("%w: the amount is a positive amount in minor units", errBadRequest)
	case len(in.Reference) > 100 || len(in.Note) > 500 || strings.ContainsAny(in.Reference, "\r\n"):
		return fmt.Errorf("%w: the reference is up to 100 characters, the note up to 500", errBadRequest)
	}
	at := s.now()
	if in.At != "" {
		if at, err = billing.ParseDate(in.At); err != nil {
			return err
		}
	}
	pin := billing.PaymentInput{Gateway: in.Method, Reference: strings.TrimSpace(in.Reference), Amount: in.Amount, At: at,
		Note: strings.TrimSpace(in.Note), By: actor(r)}
	if pin.Reference != "" {
		pin.Dedupe = in.Method + ":" + pin.Reference
	}
	res, err := s.Billing.RecordPayment(r.Context(), id, pin)
	if err != nil {
		return err
	}
	inv, err := s.Billing.Invoice(r.Context(), id)
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	return writeJSON(w, status, map[string]any{"payment": res.Payment, "duplicate": res.Duplicate, "credited": res.Credited,
		"invoice": inv})
}

func (s *Server) refundPayment(w http.ResponseWriter, r *http.Request) error {
	var in billing.RefundInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	inv, err := s.Billing.Refund(r.Context(), id, in, actor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inv)
}

func (s *Server) remindInvoice(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if s.Billing.Mailer == nil {
		return fmt.Errorf("%w: e-mail isn't set up", errConflict)
	}
	if err := s.Billing.Remind(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (s *Server) applyCredit(w http.ResponseWriter, r *http.Request) error {
	in := struct {
		Amount *int64 `json:"amount"`
	}{}
	if err := optionalBody(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	inv, err := s.Billing.ApplyCredit(r.Context(), id, in.Amount, actor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inv)
}

func (s *Server) payInvoice(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Method   string `json:"method"`
		SaveCard bool   `json:"save_card"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	next, err := s.Billing.Pay(r.Context(), id, in.Method, in.SaveCard)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, next)
}

func (s *Server) transactionFilter(r *http.Request, def, maxLimit int) (store.PaymentFilter, error) {
	var f store.PaymentFilter
	var err error
	if f.Limit, err = limitParam(r, def, maxLimit); err != nil {
		return f, err
	}
	if f.AccountID, err = int64Param(r, "account"); err != nil {
		return f, err
	}
	if f.BeforeID, err = int64Param(r, "before"); err != nil {
		return f, err
	}
	if f.From, err = dateParam(r, "from"); err != nil {
		return f, err
	}
	if f.To, err = dateParam(r, "to"); err != nil {
		return f, err
	}
	if !f.To.IsZero() {
		f.To = f.To.AddDate(0, 0, 1)
	}
	f.Gateway = r.URL.Query().Get("method")
	if len(f.Gateway) > 20 {
		return f, fmt.Errorf("%w: unknown method", errBadRequest)
	}
	if t := tenantOf(r); t != nil {
		f.AccountID = t.Account.ID
	}
	return f, nil
}

func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request) error {
	f, err := s.transactionFilter(r, 50, 500)
	if err != nil {
		return err
	}
	list, err := s.Store.Payments(r.Context(), f)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

// ---- CSV exports ----

// csvText keeps a spreadsheet from running a cell as a formula (names
// come from clients).
func csvText(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// csvAmount writes minor units as a decimal number ("118.00").
func csvAmount(v int64, decimals int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	div := int64(1)
	for range decimals {
		div *= 10
	}
	out := strconv.FormatInt(v/div, 10)
	if decimals > 0 {
		out += fmt.Sprintf(".%0*d", decimals, v%div)
	}
	if neg {
		return "-" + out
	}
	return out
}

func csvDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

func startCSV(w http.ResponseWriter, name string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	return csv.NewWriter(w)
}

func (s *Server) invoicesCSV(w http.ResponseWriter, r *http.Request) error {
	f, err := s.invoiceFilter(r, 100000, 100000)
	if err != nil {
		return err
	}
	cfg, err := s.Billing.Invoicing(r.Context())
	if err != nil {
		return err
	}
	list, err := s.Billing.Invoices(r.Context(), f)
	if err != nil {
		return err
	}
	cw := startCSV(w, "invoices.csv")
	d := cfg.Currency.Decimals
	cw.Write([]string{"number", "account_id", "client", "kind", "status", "issued", "due", "paid", "currency", "subtotal",
		"discount", "tax", "total", "credit_applied", "amount_paid", "amount_refunded", "balance"})
	for _, v := range list {
		cw.Write([]string{csvText(v.Number), strconv.FormatInt(v.AccountID, 10), csvText(v.AccountName), v.Kind, v.Status,
			csvDate(v.IssuedAt), csvDate(v.DueAt), csvDate(v.PaidAt), v.Currency, csvAmount(v.Subtotal, d), csvAmount(v.Discount, d),
			csvAmount(v.Tax, d), csvAmount(v.Total, d), csvAmount(v.CreditApplied, d), csvAmount(v.AmountPaid, d),
			csvAmount(v.AmountRefunded, d), csvAmount(v.Balance, d)})
	}
	cw.Flush()
	return nil
}

func (s *Server) transactionsCSV(w http.ResponseWriter, r *http.Request) error {
	f, err := s.transactionFilter(r, 100000, 100000)
	if err != nil {
		return err
	}
	cfg, err := s.Billing.Invoicing(r.Context())
	if err != nil {
		return err
	}
	list, err := s.Store.Payments(r.Context(), f)
	if err != nil {
		return err
	}
	cw := startCSV(w, "transactions.csv")
	d := cfg.Currency.Decimals
	cw.Write([]string{"id", "date", "invoice", "account_id", "client", "method", "reference", "amount", "fee", "refunded", "note"})
	for _, p := range list {
		at := p.At
		cw.Write([]string{strconv.FormatInt(p.ID, 10), csvDate(&at), csvText(p.InvoiceNumber), strconv.FormatInt(p.AccountID, 10),
			csvText(p.AccountName), p.Gateway, csvText(p.Reference), csvAmount(p.Amount, d), csvAmount(p.Fee, d),
			csvAmount(p.Refunded, d), csvText(p.Note)})
	}
	cw.Flush()
	return nil
}

// ---- Settings, overview, promotions, taxes, automation, orders ----

func (s *Server) invoicingSettings(w http.ResponseWriter, r *http.Request) error {
	cfg, err := s.Billing.Invoicing(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, cfg.Redacted())
}

func (s *Server) setInvoicingSettings(w http.ResponseWriter, r *http.Request) error {
	var in billing.InvoicingSettings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	cfg, err := s.Billing.SetInvoicing(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, cfg.Redacted())
}

func (s *Server) billingConfig(w http.ResponseWriter, r *http.Request) error {
	c, err := s.Billing.ClientConfig(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c)
}

func (s *Server) billingOverview(w http.ResponseWriter, r *http.Request) error {
	o, err := s.Billing.Overview(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, o)
}

func (s *Server) listPromotions(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Billing.Promotions(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) createPromotion(w http.ResponseWriter, r *http.Request) error {
	var in billing.Promotion
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p, err := s.Billing.CreatePromotion(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, p)
}

func (s *Server) updatePromotion(w http.ResponseWriter, r *http.Request) error {
	var in billing.Promotion
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	in.ID = id
	p, err := s.Billing.UpdatePromotion(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) deletePromotion(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if err := s.Billing.DeletePromotion(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listTaxRules(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Billing.TaxRules(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) createTaxRule(w http.ResponseWriter, r *http.Request) error {
	var in store.TaxRule
	if err := decode(w, r, &in); err != nil {
		return err
	}
	t, err := s.Billing.CreateTaxRule(r.Context(), &in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, t)
}

func (s *Server) updateTaxRule(w http.ResponseWriter, r *http.Request) error {
	var in store.TaxRule
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	in.ID = id
	t, err := s.Billing.UpdateTaxRule(r.Context(), &in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTaxRule(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if err := s.Billing.DeleteTaxRule(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) automationRuns(w http.ResponseWriter, r *http.Request) error {
	runs, err := s.Billing.AutomationRuns(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, runs)
}

func (s *Server) runAutomation(w http.ResponseWriter, r *http.Request) error {
	run, err := s.Billing.RunInvoicingOnce(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, run)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Billing.Orders(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) acceptOrder(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if err := s.Billing.AcceptOrder(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) cancelOrder(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if err := s.Billing.CancelOrder(r.Context(), id, actor(r)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- The public order form ----

// Order form limits per client address: attempts (of any kind) and
// orders placed, per hour.
const (
	storeQuotesPerHour   = 300
	storeAttemptsPerHour = 30
	storeOrdersPerHour   = 5
	storeBodyLimit       = 64 << 10
)

func storeKey(prefix, ip string, limit int) limitKey {
	k := ipKey(ip)
	return limitKey{prefix + k.key, limit}
}

// logoURL is the brand's logo on the panel ("" without one).
func (s *Server) logoURL(ctx context.Context) string {
	if s.Sites == nil {
		return ""
	}
	b, err := s.Sites.Branding(ctx)
	if err != nil || b.LogoVersion() == "" {
		return ""
	}
	return site.BrandLogoPath + "?v=" + b.LogoVersion()
}

// brandName is the panel's brand name ("" without one), for a store
// whose company has no name yet.
func (s *Server) brandName(ctx context.Context) string {
	if s.Sites == nil {
		return ""
	}
	if b, err := s.Sites.Branding(ctx); err == nil && b != nil {
		return b.Name
	}
	return ""
}

func (s *Server) storeCatalog(w http.ResponseWriter, r *http.Request) {
	if s.Billing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": billing.ErrStoreClosed.Error()})
		return
	}
	c, err := s.Billing.Catalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	c.Company.LogoURL = s.logoURL(r.Context())
	if c.Company.Name == "" {
		c.Company.Name = s.brandName(r.Context())
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) storeQuote(w http.ResponseWriter, r *http.Request) error {
	if s.Billing == nil {
		return billing.ErrStoreClosed
	}
	if d, _ := s.storeGuard.attempt(s.now(), storeKey("quote:", clientIP(r), storeQuotesPerHour)); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		return errTooMany
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in billing.QuoteInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	q, err := s.Billing.Quote(r.Context(), in)
	if err != nil {
		s.writeFieldError(w, r, err)
		return nil
	}
	return writeJSON(w, http.StatusOK, q)
}

// storeOrder places an order: the pending account, its first user
// (signed in, as by the sign-in form), its invoice; then the payment's
// next step. Five orders an hour per address at most.
func (s *Server) storeOrder(w http.ResponseWriter, r *http.Request) error {
	if s.Billing == nil {
		return billing.ErrStoreClosed
	}
	ctx, ip, now := r.Context(), clientIP(r), s.now()
	tries, orders := storeKey("order-try:", ip, storeAttemptsPerHour), storeKey("order:", ip, storeOrdersPerHour)
	if d, first := s.storeGuard.attempt(now, tries, orders); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		if first {
			s.audit(ctx, store.AuditEntry{Actor: "store", IP: ip, Action: "order_throttled", Status: http.StatusTooManyRequests})
		}
		return fmt.Errorf("%w: too many orders from your address; try again later", errTooMany)
	}
	placed := false
	defer func() {
		if !placed {
			s.storeGuard.undo(orders) // only orders placed count against the 5
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, storeBodyLimit)
	var in billing.OrderInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	in.User.Username = strings.TrimSpace(in.User.Username)
	if err := validUsername(in.User.Username); err != nil {
		s.writeFieldError(w, r, &billing.FieldError{Field: "user.username",
			Msg: "a username is 2-64 letters, digits or . _ @ + -, not only digits"})
		return nil
	}
	if _, err := s.Store.UserByName(ctx, in.User.Username); err == nil {
		s.writeFieldError(w, r, &billing.FieldError{Field: "user.username", Msg: "that username is taken"})
		return nil
	}
	if err := auth.ValidatePassword(in.User.Password); err != nil {
		s.writeFieldError(w, r, &billing.FieldError{Field: "user.password", Msg: err.Error()})
		return nil
	}
	res, err := s.Billing.PlaceOrder(ctx, in, ip, func(pw string) (string, error) {
		passwordSlots <- struct{}{}
		defer func() { <-passwordSlots }()
		return auth.HashPassword(pw)
	})
	if err != nil {
		s.writeFieldError(w, r, err)
		return nil
	}
	placed = true
	s.audit(ctx, store.AuditEntry{Actor: in.User.Username, IP: ip, Action: "order", Target: strconv.FormatInt(res.AccountID, 10),
		Status: http.StatusCreated})
	u, err := s.Store.UserByName(ctx, in.User.Username)
	if err != nil {
		return err
	}
	if err := s.signIn(w, r, u); err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, res)
}

// razorpayCallback is where Razorpay sends the client back after a
// payment link: the signed result is recorded, and the client lands on
// the invoice.
func (s *Server) razorpayCallback(w http.ResponseWriter, r *http.Request) {
	if s.Billing == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	id, err := s.Billing.RazorpayCallback(r.Context(), q)
	if id == 0 {
		msg := "invalid payment callback"
		if errors.Is(err, billing.ErrRazorpayOff) {
			msg = "Razorpay is not configured"
		}
		s.audit(r.Context(), store.AuditEntry{Actor: "razorpay", IP: clientIP(r), Action: "razorpay_callback_refused",
			Detail: fmt.Sprint(err), Status: http.StatusBadRequest})
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	target := fmt.Sprintf("%s/#/billing/invoices/%d", s.PanelURL, id)
	switch {
	case err != nil:
		s.Log.Warn("Razorpay callback", "invoice", id, "err", err)
		target += "?" + url.Values{"payment": {"error"}}.Encode()
	case q.Get("razorpay_payment_link_status") == "paid":
		target += "?paid=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// razorpayWebhook is Razorpay's endpoint: public, trusted only through
// X-Razorpay-Signature over the raw body.
func (s *Server) razorpayWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		return
	}
	if s.Billing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": billing.ErrRazorpayOff.Error()})
		return
	}
	err = s.Billing.HandleRazorpayWebhook(r.Context(), body, r.Header.Get("X-Razorpay-Signature"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"received": true})
	case errors.Is(err, billing.ErrRazorpayOff):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, billing.ErrRazorpaySigned), errors.Is(err, billing.ErrInvalid):
		s.audit(r.Context(), store.AuditEntry{Actor: "razorpay", IP: clientIP(r), Action: "razorpay_webhook_refused",
			Detail: err.Error(), Status: http.StatusBadRequest})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		s.Log.Error("processing a Razorpay webhook", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "processing failed; retry"})
	}
}
