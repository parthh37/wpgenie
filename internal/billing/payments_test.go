package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

func TestStripeInvoicePaymentsAreIdempotent(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv := e.manualInvoice(a.ID, 1180, false)
	if inv.Balance != 1180 || strings.Join(inv.PayMethods, ",") != "stripe,razorpay,manual" {
		t.Fatalf("invoice %+v", inv)
	}
	// Checkout: the balance, the customer (made once), the card saved.
	next, err := e.svc.Pay(ctx, inv.ID, MethodStripe, true)
	if err != nil || next.RedirectURL != "https://checkout.stripe.com/c/pay/cs_test_1" {
		t.Fatalf("pay %+v %v", next, err)
	}
	e.svc.Pay(ctx, inv.ID, MethodStripe, false)
	co := e.gw.checkouts[0]
	id := itoa(inv.ID)
	if co.Get("mode") != "payment" || co.Get("customer") != "cus_T1" || co.Get("line_items[0][price_data][unit_amount]") != "1180" ||
		co.Get("line_items[0][price_data][currency]") != "usd" || co.Get("metadata[wpgenie_invoice]") != id ||
		co.Get("client_reference_id") != id || co.Get("payment_intent_data[setup_future_usage]") != "off_session" ||
		co.Get("success_url") != "https://panel.test/#/billing/invoices/"+id+"?paid=1" ||
		e.gw.checkouts[1].Get("payment_intent_data[setup_future_usage]") != "" || e.gw.count("POST /v1/customers") != 1 {
		t.Fatalf("checkout %v", co)
	}
	session := fmt.Sprintf(`{"id":"cs_test_1","mode":"payment","payment_status":"paid","payment_intent":"pi_1",
		"amount_total":1180,"currency":"usd","customer":"cus_T1","metadata":{"wpgenie_invoice":%q,"wpgenie_save_card":"1"}}`, id)
	intent := fmt.Sprintf(`{"id":"pi_1","status":"succeeded","amount_received":1180,"currency":"usd","customer":"cus_T1",
		"payment_method":"pm_card1","metadata":{"wpgenie_invoice":%q,"wpgenie_save_card":"1"}}`, id)
	for _, ev := range []struct{ id, typ, obj string }{
		{"evt_c1", "checkout.session.completed", session},
		{"evt_c1", "checkout.session.completed", session}, // a replay
		{"evt_p1", "payment_intent.succeeded", intent},    // the same payment, reported again
	} {
		if err := e.stripe(t, ev.id, ev.typ, ev.obj); err != nil {
			t.Fatalf("%s: %v", ev.id, err)
		}
	}
	got, _ := e.svc.Invoice(ctx, inv.ID)
	if got.Status != "paid" || len(got.Payments) != 1 || got.Payments[0].Reference != "pi_1" || got.AmountPaid != 1180 {
		t.Fatalf("paid %+v", got)
	}
	prof, _ := e.svc.Profile(ctx, a.ID)
	if prof.Card == nil || prof.Card.Last4 != "4242" || prof.Card.Brand != "visa" || prof.Credit != 0 {
		t.Fatalf("card %+v", prof)
	}
	if n := e.mails("invoice.paid", a.ID); n != 1 {
		t.Fatalf("%d receipts", n)
	}
	// Refunds made in Stripe's dashboard: the charge's refunded total.
	refunded := func(evt string, total int64) {
		t.Helper()
		if err := e.stripe(t, evt, "charge.refunded", fmt.Sprintf(`{"id":"ch_1","payment_intent":"pi_1","amount_refunded":%d}`, total)); err != nil {
			t.Fatal(err)
		}
	}
	refunded("evt_r1", 300)
	refunded("evt_r2", 300) // the same state again (another event)
	if got, _ = e.svc.Invoice(ctx, inv.ID); got.Status != "partially_refunded" || got.AmountRefunded != 300 {
		t.Fatalf("partial refund %+v", got)
	}
	// A refund made here is reported back by Stripe: not counted twice.
	if _, err := e.svc.Refund(ctx, inv.ID, RefundInput{PaymentID: got.Payments[0].ID, Amount: 500, To: "gateway"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if e.gw.refunds[0].Get("payment_intent") != "pi_1" || e.gw.refunds[0].Get("amount") != "500" {
		t.Fatalf("refund request %v", e.gw.refunds)
	}
	refunded("evt_r3", 800)
	if got, _ = e.svc.Invoice(ctx, inv.ID); got.AmountRefunded != 800 {
		t.Fatalf("after our refund %+v", got)
	}
	refunded("evt_r4", 1180)
	if got, _ = e.svc.Invoice(ctx, inv.ID); got.Status != "refunded" || got.AmountRefunded != 1180 {
		t.Fatalf("refunded %+v", got)
	}
	if _, err := e.svc.Refund(ctx, inv.ID, RefundInput{PaymentID: got.Payments[0].ID, To: "gateway"}, "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("refund beyond the payment: %v", err)
	}
	// Forgetting the card detaches it.
	if v, err := e.svc.ForgetCard(ctx, a.ID); err != nil || v.Card != nil || e.gw.count("POST /v1/payment_methods/pm_card1/detach") != 1 {
		t.Fatalf("forget %+v %v", v, err)
	}
	// Payments for someone else's checkout, or in another currency, are
	// not recorded; the subscriptions' flow is untouched.
	inv2 := e.manualInvoice(a.ID, 100, false)
	e.stripe(t, "evt_x", "payment_intent.succeeded", fmt.Sprintf(`{"id":"pi_eur","amount_received":100,"currency":"eur",
		"metadata":{"wpgenie_invoice":"%d"}}`, inv2.ID))
	e.stripe(t, "evt_y", "payment_intent.succeeded", `{"id":"pi_other","amount_received":100,"currency":"usd","metadata":{}}`)
	if got, _ := e.svc.Invoice(ctx, inv2.ID); got.Status != "unpaid" {
		t.Fatalf("foreign payments recorded: %+v", got)
	}
}

func rzpSign(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func TestRazorpayPayments(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.setting(func(c *InvoicingSettings) { c.Currency = Currency{Code: "INR", Symbol: "₹", Decimals: 2} })
	a := e.billed("Shop", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv := e.manualInvoice(a.ID, 11800, false)
	for i := 1; i <= 2; i++ {
		next, err := e.svc.Pay(ctx, inv.ID, MethodRazorpay, false)
		if err != nil || next.RedirectURL != "https://rzp.io/i/T1" {
			t.Fatalf("pay %+v %v", next, err)
		}
		link := e.gw.links[i-1]
		if link["reference_id"] != fmt.Sprintf("wpg_%d_%d", inv.ID, i) || link["amount"].(float64) != 11800 ||
			link["currency"] != "INR" || link["callback_url"] != "https://panel.test/api/v1/billing/razorpay/callback" ||
			link["callback_method"] != "get" {
			t.Fatalf("link %v", link)
		}
	}
	e.gw.payments["pay_ABC12345"] = `{"id":"pay_ABC12345","amount":11800,"currency":"INR","status":"captured","fee":236}`
	q := url.Values{"razorpay_payment_id": {"pay_ABC12345"}, "razorpay_payment_link_id": {"plink_T1"},
		"razorpay_payment_link_reference_id": {fmt.Sprintf("wpg_%d_2", inv.ID)}, "razorpay_payment_link_status": {"paid"}}
	q.Set("razorpay_signature", rzpSign("rzsecret", "plink_T1|"+q.Get("razorpay_payment_link_reference_id")+"|paid|pay_ABC12345"))
	forged := url.Values{}
	for k, v := range q {
		forged[k] = v
	}
	forged.Set("razorpay_payment_id", "pay_OTHER123")
	if _, err := e.svc.RazorpayCallback(ctx, forged); !errors.Is(err, ErrRazorpaySigned) {
		t.Fatalf("forged callback: %v", err)
	}
	for range 2 {
		if id, err := e.svc.RazorpayCallback(ctx, q); err != nil || id != inv.ID {
			t.Fatalf("callback %d %v", id, err)
		}
	}
	body := fmt.Sprintf(`{"event":"payment_link.paid","payload":{"payment_link":{"entity":{"id":"plink_T1",
		"reference_id":"wpg_%d_2"}},"payment":{"entity":{"id":"pay_ABC12345","amount":11800,"currency":"INR",
		"status":"captured","fee":236}}}}`, inv.ID)
	if err := e.svc.HandleRazorpayWebhook(ctx, []byte(body), rzpSign("wrong", body)); !errors.Is(err, ErrRazorpaySigned) {
		t.Fatalf("forged webhook: %v", err)
	}
	if err := e.svc.HandleRazorpayWebhook(ctx, []byte(body), rzpSign("rzwebhook", body)); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.Invoice(ctx, inv.ID)
	if got.Status != "paid" || len(got.Payments) != 1 || got.Payments[0].Fee != 236 || got.Payments[0].Gateway != "razorpay" {
		t.Fatalf("paid %+v", got)
	}
	// A refund made here, then reported by the webhook: once.
	if _, err := e.svc.Refund(ctx, inv.ID, RefundInput{PaymentID: got.Payments[0].ID, Amount: 1000, To: "gateway"}, "admin"); err != nil {
		t.Fatal(err)
	}
	rb := `{"event":"refund.processed","payload":{"refund":{"entity":{"id":"rfnd_TEST0001","payment_id":"pay_ABC12345","amount":1000}}}}`
	if err := e.svc.HandleRazorpayWebhook(ctx, []byte(rb), rzpSign("rzwebhook", rb)); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.svc.Invoice(ctx, inv.ID); got.AmountRefunded != 1000 || got.Status != "partially_refunded" {
		t.Fatalf("refund %+v", got)
	}
	// Cancelled on Razorpay's page: back to the invoice, nothing recorded.
	q2 := url.Values{"razorpay_payment_id": {""}, "razorpay_payment_link_id": {"plink_T2"},
		"razorpay_payment_link_reference_id": {fmt.Sprintf("wpg_%d_3", inv.ID)}, "razorpay_payment_link_status": {"cancelled"}}
	q2.Set("razorpay_signature", rzpSign("rzsecret", "plink_T2|"+q2.Get("razorpay_payment_link_reference_id")+"|cancelled|"))
	if id, err := e.svc.RazorpayCallback(ctx, q2); err != nil || id != inv.ID {
		t.Fatalf("cancelled callback %d %v", id, err)
	}
}

func TestManualPaymentsCreditAndCancel(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv := e.manualInvoice(a.ID, 1000, false)
	next, err := e.svc.Pay(ctx, inv.ID, MethodManual, false)
	if err != nil || next.Instructions != "IBAN XX00 1234" || next.Reference != inv.Number {
		t.Fatalf("manual %+v %v", next, err)
	}
	if _, err := e.svc.Pay(ctx, inv.ID, "paypal", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown method: %v", err)
	}
	// Partial, the same bank reference again (once), then more than due.
	pay := func(amount int64, ref string) *store.PaymentResult {
		t.Helper()
		r, err := e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: "bank", Reference: ref, Amount: amount, By: "admin",
			Dedupe: map[bool]string{true: "bank:" + ref}[ref != ""]})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	pay(400, "T1")
	if r := pay(400, "T1"); !r.Duplicate {
		t.Fatal("same reference recorded twice")
	}
	if r := pay(1000, ""); !r.Paid || r.Credited != 400 {
		t.Fatalf("overpaid %+v", r)
	}
	prof, _ := e.svc.Profile(ctx, a.ID)
	if prof.Credit != 400 {
		t.Fatalf("credit %d", prof.Credit)
	}
	// Credit pays the next invoice: automatically when issued (the default).
	inv2 := e.manualInvoice(a.ID, 300, false)
	if inv2.Status != "paid" || inv2.CreditApplied != 300 {
		t.Fatalf("auto-applied %+v", inv2)
	}
	e.setting(func(c *InvoicingSettings) { c.Automation.AutoApplyCredit = false })
	inv3 := e.manualInvoice(a.ID, 500, false)
	if inv3.CreditApplied != 0 {
		t.Fatal("credit applied while off")
	}
	amt := int64(50)
	if got, err := e.svc.ApplyCredit(ctx, inv3.ID, &amt, "jo"); err != nil || got.CreditApplied != 50 || got.Balance != 450 {
		t.Fatalf("apply 50 %+v %v", got, err)
	}
	if got, _ := e.svc.ApplyCredit(ctx, inv3.ID, nil, "jo"); got.CreditApplied != 100 || got.Status != "unpaid" {
		t.Fatalf("apply the rest %+v", got)
	}
	// Staff credit: added (e-mailed), removed, never below zero.
	if _, err := e.svc.AddCredit(ctx, a.ID, 1000, "Goodwill", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddCredit(ctx, a.ID, -5000, "", "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("below zero: %v", err)
	}
	if e.mails("credit.added", a.ID) != 1 {
		t.Fatal("credit.added not sent")
	}
	// Cancelling gives back what was paid on it.
	if _, err := e.svc.CancelInvoice(ctx, inv3.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if prof, _ = e.svc.Profile(ctx, a.ID); prof.Credit != 1000+100 {
		t.Fatalf("credit after cancel %d", prof.Credit)
	}
	if _, err := e.svc.CancelInvoice(ctx, inv.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled a paid invoice: %v", err)
	}
	// Drafts change whole; unpaid ones only their notes and due date.
	d := e.manualInvoice(a.ID, 100, true)
	notes := "Thanks"
	upd, err := e.svc.UpdateInvoice(ctx, d.ID, InvoiceUpdate{Notes: &notes, Items: []ItemInput{{Description: "A", Quantity: 3,
		UnitPrice: 250}}})
	if err != nil || upd.Total != 750 || upd.Notes != "Thanks" || len(upd.Items) != 1 {
		t.Fatalf("draft %+v %v", upd, err)
	}
	if _, err := e.svc.UpdateInvoice(ctx, inv.ID, InvoiceUpdate{Items: []ItemInput{{Description: "B", Quantity: 1}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("edited a paid invoice: %v", err)
	}
	// A reseller's customers aren't invoiced here.
	r := e.account(AccountInput{Name: "Res", Kind: "reseller", PlanID: "reseller"})
	kid := e.account(AccountInput{Name: "Kid", PlanID: "small", ParentID: r.ID})
	if _, err := e.svc.CreateInvoice(ctx, ManualInvoiceInput{AccountID: kid.ID, Items: []ItemInput{{Description: "x",
		Quantity: 1, UnitPrice: 1}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invoiced a reseller's customer: %v", err)
	}
	mode := ModeInvoice
	if _, err := e.svc.UpdateProfile(ctx, kid.ID, ProfileInput{Mode: &mode}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a reseller's customer billed by invoice: %v", err)
	}
}

func TestPlanChangeProration(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	// Now: Sep 20 12:00; the period Sep 1 - Oct 1 (30 days, 10.5 unused)
	// is paid.
	a := e.billed("Acme", "basic", "monthly", day(2026, 9, 1))
	// Nothing paid for the period yet: nothing to credit.
	if q, err := e.svc.QuotePlanChange(ctx, a.ID, "pro", "", true); err != nil || q.Credit != 0 {
		t.Fatalf("credit without a payment: %+v %v", q, err)
	}
	e.svc.RunInvoicingOnce(ctx)
	sept := e.invoices(a.ID)[0]
	if _, err := e.svc.QuotePlanChange(ctx, a.ID, "pro", "", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("change with an unpaid renewal: %v", err)
	}
	e.svc.RecordPayment(ctx, sept.ID, PaymentInput{Gateway: "bank", Amount: 1000})
	q, err := e.svc.QuotePlanChange(ctx, a.ID, "pro", "", true)
	if err != nil || q.Credit != 350 || q.Charge != 1050 || q.Subtotal != 700 || q.Total != 700 ||
		!q.NewNextDueAt.Equal(day(2026, 10, 1)) || len(q.Items) != 2 || q.Items[1].Amount != -350 {
		t.Fatalf("upgrade %+v %v", q, err)
	}
	// A new cycle: a whole year from today, less the unused month.
	q, _ = e.svc.QuotePlanChange(ctx, a.ID, "pro", "annually", true)
	if q.Charge != 30000 || q.Credit != 350 || q.Total != 29650 || !q.NewNextDueAt.Equal(day(2027, 9, 20)) {
		t.Fatalf("new cycle %+v", q)
	}
	for _, bad := range []struct{ plan, cycle string }{{"private", ""}, {"basic", ""}, {"pro", "weekly"}, {"agency", ""},
		{"pro", "biennially"}} {
		if _, err := e.svc.QuotePlanChange(ctx, a.ID, bad.plan, bad.cycle, true); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	res, err := e.svc.ChangePlan(ctx, a.ID, "pro", "", true, "jo")
	if err != nil || res.Invoice == nil || res.Invoice.Total != 700 || res.Invoice.Kind != "plan_change" {
		t.Fatalf("change %+v %v", res, err)
	}
	if acct, _ := e.store.GetAccount(ctx, a.ID); acct.PlanID != "basic" {
		t.Fatal("switched before payment")
	}
	// The October renewal is issued first: the unpaid change's quote is
	// stale, so it is withdrawn (and can't be asked for again until the
	// renewal is paid).
	e.run(day(2026, 9, 24))
	if got, _ := e.store.GetInvoice(ctx, res.Invoice.ID); got.Status != "cancelled" {
		t.Fatalf("stale plan change %s", got.Status)
	}
	if _, err := e.svc.ChangePlan(ctx, a.ID, "pro", "", true, "jo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("change with an unpaid renewal: %v", err)
	}
	var oct *store.Invoice
	for _, inv := range e.invoices(a.ID) {
		if inv.Kind == KindRenewal && inv.Status == "unpaid" {
			oct = inv
		}
	}
	e.svc.RecordPayment(ctx, oct.ID, PaymentInput{Gateway: "bank", Amount: 1000})
	res, err = e.svc.ChangePlan(ctx, a.ID, "pro", "", true, "jo")
	if err != nil || res.Invoice == nil {
		t.Fatalf("change after the renewal %+v %v", res, err)
	}
	// Paid late, after staff moved the next due date on: it never moves
	// back (no time is billed twice, none is lost).
	nd := "2026-12-01"
	e.svc.UpdateProfile(ctx, a.ID, ProfileInput{NextDueAt: &nd})
	if _, err := e.svc.RecordPayment(ctx, res.Invoice.ID, PaymentInput{Gateway: "bank", Amount: res.Invoice.Total}); err != nil {
		t.Fatal(err)
	}
	prof, _ := e.svc.Profile(ctx, a.ID)
	if acct, _ := e.store.GetAccount(ctx, a.ID); acct.PlanID != "pro" || !prof.NextDueAt.Equal(day(2026, 12, 1)) {
		t.Fatalf("after the change: %s, next due %v", acct.PlanID, prof.NextDueAt)
	}
}

// The credit for unused time is what was paid for the period (after
// discounts), not the plan's list price; a new cycle may start earlier than
// the paid-through date only because that time was credited.
func TestDowngradeCreditsWhatWasPaid(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	half, _ := e.svc.CreatePromotion(ctx, Promotion{Code: "HALF", Type: "percent", Value: 5000, Recurring: true, Enabled: true})
	a := e.billed("Acme", "pro", "monthly", day(2026, 9, 21))
	p, _ := e.store.GetBillingProfile(ctx, a.ID)
	p.PromoID = half.ID
	e.store.SaveBillingProfile(ctx, p)
	e.run(day(2026, 9, 20)) // the renewal Sep 21 - Oct 21: 3000 less 50%
	inv := e.invoices(a.ID)[0]
	if inv.Total != 1500 {
		t.Fatalf("renewal %+v", inv)
	}
	e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: "bank", Amount: 1500})
	e.now = day(2026, 9, 21) // the whole period is unused
	q, err := e.svc.QuotePlanChange(ctx, a.ID, "basic", "", true)
	if err != nil || q.Credit != 1500 || q.Charge != 1000 || q.Total != -500 {
		t.Fatalf("downgrade quote %+v %v", q, err)
	}
	res, err := e.svc.ChangePlan(ctx, a.ID, "basic", "", true, "jo")
	if err != nil || !res.Applied {
		t.Fatalf("downgrade %+v %v", res, err)
	}
	prof, _ := e.svc.Profile(ctx, a.ID)
	if prof.Credit != 500 || !prof.NextDueAt.Equal(day(2026, 10, 21)) {
		t.Fatalf("after the downgrade: credit %d, next due %v", prof.Credit, prof.NextDueAt)
	}
	// To a shorter cycle from a paid year: the unused year is credited, so
	// the next due date may come back to the new cycle's end.
	b := e.billed("Bee", "basic", "annually", day(2026, 9, 21))
	e.run(day(2026, 9, 21))
	var year *store.Invoice
	for _, inv := range e.invoices(b.ID) {
		year = inv
	}
	e.svc.RecordPayment(ctx, year.ID, PaymentInput{Gateway: "bank", Amount: year.Total})
	res, err = e.svc.ChangePlan(ctx, b.ID, "basic", "monthly", true, "jo")
	if err != nil || !res.Applied {
		t.Fatalf("to monthly %+v %v", res, err)
	}
	prof, _ = e.svc.Profile(ctx, b.ID)
	if !prof.NextDueAt.Equal(day(2026, 10, 21)) || prof.Credit != 10000-1000 || prof.Cycle != "monthly" {
		t.Fatalf("to monthly: %+v", prof)
	}
}

func TestOrderFlow(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.setting(func(c *InvoicingSettings) { c.Invoice.TermsURL = "https://host.test/terms" })
	e.svc.CreatePromotion(ctx, Promotion{Code: "WELCOME", Type: "percent", Value: 1000, Recurring: true, Enabled: true})
	cat, err := e.svc.Catalog(ctx)
	if err != nil || !cat.Enabled || len(cat.Plans) != 3 || cat.Plans[0].ID != "agency" && cat.Plans[0].ID != "basic" ||
		len(cat.Methods) != 3 || cat.TermsURL == "" {
		t.Fatalf("catalog %+v %v", cat, err)
	}
	hash := func(pw string) (string, error) { return "hash:" + pw, nil }
	order := func(mut func(*OrderInput)) (*OrderResult, error) {
		in := OrderInput{PlanID: "basic", Cycle: "annually", Promo: "welcome", Method: MethodStripe, AcceptTerms: true,
			Contact: store.BillingContact{FirstName: "Jo", LastName: "Doe", Email: "jo@shop.test", Country: "FR"}}
		in.User.Username, in.User.Password = "jo", "a long password"
		if mut != nil {
			mut(&in)
		}
		return e.svc.PlaceOrder(ctx, in, "203.0.113.5", hash)
	}
	for field, mut := range map[string]func(*OrderInput){
		"contact.email": func(in *OrderInput) { in.Contact.Email = "nope" },
		"contact.state": func(in *OrderInput) { in.Contact.Country = "US" },
		"plan_id":       func(in *OrderInput) { in.PlanID = "private" },
		"cycle":         func(in *OrderInput) { in.Cycle = "triennially" },
		"promo":         func(in *OrderInput) { in.Promo = "NOPE" },
		"method":        func(in *OrderInput) { in.Method = "" },
		"accept_terms":  func(in *OrderInput) { in.AcceptTerms = false },
	} {
		if _, err := order(mut); !isField(err, field) {
			t.Errorf("%s: %v", field, err)
		}
	}
	if _, err := order(func(in *OrderInput) { in.Website = "http://spam" }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("honeypot: %v", err)
	}
	res, err := order(nil)
	if err != nil || res.Next == nil || res.Next.RedirectURL == "" {
		t.Fatalf("order %+v %v", res, err)
	}
	a, _ := e.store.GetAccount(ctx, res.AccountID)
	u, _ := e.store.UserByName(ctx, "jo")
	inv, _ := e.svc.Invoice(ctx, res.InvoiceID)
	if a.Status != store.AccountPending || a.Name != "Jo Doe" || u.AccountID != a.ID || u.Role != "customer" ||
		inv.Kind != "order" || inv.Total != 10500-1000 || len(inv.Items) != 3 {
		t.Fatalf("pending order %+v %+v %+v", a, u, inv)
	}
	if suspended, _ := e.svc.Suspended(ctx, a); !suspended {
		t.Fatal("a pending account isn't treated as suspended")
	}
	if _, err := e.svc.Suspend(ctx, a.ID, ReasonAdmin); !errors.Is(err, ErrConflict) {
		t.Fatalf("suspended a pending order: %v", err)
	}
	if _, err := order(nil); !isField(err, "user.username") {
		t.Fatalf("taken username: %v", err)
	}
	if e.mails("order.received", a.ID) != 1 {
		t.Fatal("order.received not sent")
	}
	// Paid (twice reported): active, welcomed, billed from today.
	e.now = e.now.Add(48 * time.Hour)
	for _, evt := range []string{"evt_o1", "evt_o2"} {
		if err := e.stripe(t, evt, "payment_intent.succeeded", fmt.Sprintf(`{"id":"pi_o1","amount_received":9500,"currency":"usd",
			"metadata":{"wpgenie_invoice":"%d"}}`, inv.ID)); err != nil {
			t.Fatal(err)
		}
	}
	a, _ = e.store.GetAccount(ctx, a.ID)
	prof, _ := e.svc.Profile(ctx, a.ID)
	orders, _ := e.svc.Orders(ctx, "")
	if a.Status != store.AccountActive || e.mails("account.welcome", a.ID) != 1 || !prof.NextDueAt.Equal(day(2027, 9, 22)) ||
		prof.AnchorDay != 22 || prof.Mode != ModeInvoice || orders[0].Status != "active" || orders[0].InvoiceStatus != "paid" {
		t.Fatalf("activated %+v %+v %+v", a, prof, orders)
	}
	// The recurring promotion discounts renewals too.
	if prof.Upcoming == nil || prof.Upcoming.Amount != 9000 {
		t.Fatalf("upcoming %+v", prof.Upcoming)
	}

	// Orders waiting for approval stay pending once paid; staff accept.
	e.setting(func(c *InvoicingSettings) { c.Automation.OrdersNeedApproval = true })
	res, err = order(func(in *OrderInput) { in.User.Username = "kim"; in.Method = MethodManual; in.Promo = "" })
	if err != nil || res.Next.Instructions == "" {
		t.Fatalf("manual order %+v %v", res, err)
	}
	e.svc.RecordPayment(ctx, res.InvoiceID, PaymentInput{Gateway: "bank", Amount: 10500})
	if a, _ = e.store.GetAccount(ctx, res.AccountID); a.Status != store.AccountPending {
		t.Fatal("activated without approval")
	}
	pending, _ := e.svc.Orders(ctx, "pending")
	if len(pending) != 1 || pending[0].InvoiceStatus != "paid" || pending[0].IP != "203.0.113.5" {
		t.Fatalf("pending orders %+v", pending)
	}
	if err := e.svc.AcceptOrder(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AcceptOrder(ctx, pending[0].ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted twice: %v", err)
	}
	if a, _ = e.store.GetAccount(ctx, res.AccountID); a.Status != store.AccountActive {
		t.Fatal("not activated by approval")
	}
	// Cancelled: invoice cancelled, account terminated.
	res, _ = order(func(in *OrderInput) { in.User.Username = "lee"; in.Method = MethodManual; in.Promo = "" })
	pending, _ = e.svc.Orders(ctx, "pending")
	if err := e.svc.CancelOrder(ctx, pending[0].ID, "admin"); err != nil {
		t.Fatal(err)
	}
	a, _ = e.store.GetAccount(ctx, res.AccountID)
	inv, _ = e.svc.Invoice(ctx, res.InvoiceID)
	if a.Status != store.AccountTerminated || inv.Status != "cancelled" {
		t.Fatalf("cancelled order: %s, invoice %s", a.Status, inv.Status)
	}
	// A free order (a reseller plan with a 100% code) is active at once.
	e.setting(func(c *InvoicingSettings) { c.Automation.OrdersNeedApproval = false })
	e.svc.CreatePromotion(ctx, Promotion{Code: "FREE", Type: "percent", Value: 10000, Enabled: true})
	res, err = order(func(in *OrderInput) {
		in.User.Username, in.PlanID, in.Cycle, in.Promo, in.Method = "rex", "agency", "monthly", "FREE", ""
	})
	if err != nil || !res.Next.Paid {
		t.Fatalf("free order %+v %v", res, err)
	}
	if a, _ = e.store.GetAccount(ctx, res.AccountID); a.Status != store.AccountActive || a.Kind != "reseller" {
		t.Fatalf("free order account %+v", a)
	}
	if u, _ := e.store.UserByName(ctx, "rex"); u.Role != "reseller" {
		t.Fatalf("role %s", u.Role)
	}
	// The store closed: no quotes, no orders.
	e.setting(func(c *InvoicingSettings) { c.Enabled = false })
	if _, err := order(func(in *OrderInput) { in.User.Username = "zed" }); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("closed store: %v", err)
	}
	if cat, _ := e.svc.Catalog(ctx); cat.Enabled || len(cat.Plans) != 0 {
		t.Fatalf("closed catalog %+v", cat)
	}
}

func TestBurstPacks(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	for _, bad := range [][]BurstPack{{{ID: "B 1", Minutes: 1}}, {{ID: "b", Minutes: 0}}, {{ID: "b", Minutes: 1, Price: -1}},
		{{ID: "b", Minutes: 1}, {ID: "b", Minutes: 2}}} {
		cfg, _ := e.svc.Invoicing(ctx)
		cfg.BurstPacks = bad
		if _, err := e.svc.SetInvoicing(ctx, cfg.Redacted()); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	e.setting(func(c *InvoicingSettings) { c.BurstPacks = []BurstPack{{ID: "b500", Minutes: 500, Price: 500}} })
	if cc, _ := e.svc.ClientConfig(ctx); len(cc.BurstPacks) != 1 || cc.BurstPacks[0].Minutes != 500 {
		t.Fatalf("config %+v", cc)
	}
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	buy, err := e.svc.BuyBurstPack(ctx, a.ID, "b500", MethodStripe)
	if err != nil || buy.Invoice.Kind != KindBurstTopup || buy.Invoice.Total != 500 || buy.Next.RedirectURL == "" ||
		buy.Invoice.Items[0].Kind != ItemBurst || buy.Invoice.Items[0].Description != "500 burst minutes" {
		t.Fatalf("buy %+v %v", buy, err)
	}
	// Paid, reported three times: the minutes are added once.
	obj := fmt.Sprintf(`{"id":"pi_b1","amount_received":500,"currency":"usd","metadata":{"wpgenie_invoice":"%d"}}`, buy.Invoice.ID)
	for _, evt := range []string{"evt_b1", "evt_b1", "evt_b2"} {
		if err := e.stripe(t, evt, "payment_intent.succeeded", obj); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.RunInvoicingOnce(ctx) // retrying effects doesn't add them again either
	if acct, _ := e.store.GetAccount(ctx, a.ID); acct.BurstCredit != 500 {
		t.Fatalf("burst credit %d", acct.BurstCredit)
	}
	msgs, _ := e.store.MailLog(ctx, store.MailFilter{AccountID: a.ID})
	receipt := ""
	for _, m := range msgs {
		if m.Template == "invoice.paid" {
			receipt = m.Text
		}
	}
	if !strings.Contains(receipt, "500 burst minutes were added") {
		t.Fatalf("receipt %q", receipt)
	}
	// From credit: paid at once; not enough credit: nothing made.
	if _, err := e.svc.BuyBurstPack(ctx, a.ID, "b500", MethodCredit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("without credit: %v", err)
	}
	n := len(e.invoices(a.ID))
	e.svc.AddCredit(ctx, a.ID, 600, "", "admin")
	buy, err = e.svc.BuyBurstPack(ctx, a.ID, "b500", MethodCredit)
	if err != nil || !buy.Next.Paid || buy.Invoice.Status != "paid" || len(e.invoices(a.ID)) != n+1 {
		t.Fatalf("with credit %+v %v", buy, err)
	}
	if acct, _ := e.store.GetAccount(ctx, a.ID); acct.BurstCredit != 1000 {
		t.Fatalf("burst credit %d", acct.BurstCredit)
	}
	// Not for a reseller's customers; not a pack that doesn't exist.
	r := e.account(AccountInput{Name: "Res", Kind: "reseller", PlanID: "reseller"})
	kid := e.account(AccountInput{Name: "Kid", PlanID: "small", ParentID: r.ID})
	if _, err := e.svc.BuyBurstPack(ctx, kid.ID, "b500", MethodStripe); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a reseller's customer: %v", err)
	}
	if _, err := e.svc.BuyBurstPack(ctx, a.ID, "b9", MethodStripe); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown pack: %v", err)
	}
}

// Concurrent reports of the same payment (a webhook and the client's
// return racing) record it once; different payments all count.
func TestConcurrentPayments(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv := e.manualInvoice(a.ID, 10000, false)
	done := make(chan error, 20)
	for i := range 20 {
		go func() {
			ref := "same"
			if i%2 == 1 {
				ref = fmt.Sprintf("p%d", i)
			}
			_, err := e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: MethodStripe, Reference: ref, Amount: 1000,
				Dedupe: "stripe:" + ref})
			done <- err
		}()
	}
	for range 20 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	got, _ := e.svc.Invoice(ctx, inv.ID)
	if len(got.Payments) != 11 || got.Status != "paid" || got.AmountPaid != 10000 {
		t.Fatalf("%d payments, %+v", len(got.Payments), got)
	}
	if p, _ := e.svc.Profile(ctx, a.ID); p.Credit != 1000 {
		t.Fatalf("credit %d", p.Credit)
	}
	if e.mails("invoice.paid", a.ID) != 1 {
		t.Fatal("receipts")
	}
}
