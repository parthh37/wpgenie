package billing

// Regression tests for the billing review's findings.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Refunding a payment whose excess became credit takes the credit back:
// never refunded twice, and refused (before any money moves at the
// gateway) once the credit is spent.
func TestRefundsTakeBackCredit(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv := e.manualInvoice(a.ID, 1000, false)
	e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: "bank", Amount: 1500})
	got, _ := e.svc.Invoice(ctx, inv.ID)
	pay := got.Payments[0]
	if pay.Credited != 500 {
		t.Fatalf("credited %+v", pay)
	}
	// Spent on another invoice: a refund of the payment is refused.
	e.manualInvoice(a.ID, 500, false) // credit applied at issue
	if _, err := e.svc.Refund(ctx, inv.ID, RefundInput{PaymentID: pay.ID, To: "gateway"}, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("refund of spent credit: %v", err)
	}
	// A Stripe payment the same: nothing asked of Stripe.
	b := e.billed("Bee", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv = e.manualInvoice(b.ID, 1000, false)
	e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: MethodStripe, Reference: "pi_x1", Amount: 1500, Dedupe: "stripe:pi_x1"})
	e.manualInvoice(b.ID, 500, false)
	got, _ = e.svc.Invoice(ctx, inv.ID)
	if _, err := e.svc.Refund(ctx, inv.ID, RefundInput{PaymentID: got.Payments[0].ID, To: "gateway"}, "admin"); !errors.Is(err, ErrConflict) ||
		len(e.gw.refunds) != 0 {
		t.Fatalf("refund at Stripe of spent credit: %v, %d refunds", err, len(e.gw.refunds))
	}
	// Not spent: all 1500 back to the card, the credit gone with it.
	c := e.billed("Cee", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv = e.manualInvoice(c.ID, 1000, false)
	e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: MethodStripe, Reference: "pi_x2", Amount: 1500, Dedupe: "stripe:pi_x2"})
	got, _ = e.svc.Invoice(ctx, inv.ID)
	if _, err := e.svc.Refund(ctx, inv.ID, RefundInput{PaymentID: got.Payments[0].ID, To: "gateway"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.Profile(ctx, c.ID); p.Credit != 0 || e.gw.refunds[0].Get("amount") != "1500" {
		t.Fatalf("credit %d after a full refund", p.Credit)
	}
}

// Stripe's refunded total is compared with the refunds made through
// Stripe only: a refund to credit is no part of it.
func TestStripeRefundedTotalIgnoresRefundsToCredit(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv := e.manualInvoice(a.ID, 1000, false)
	e.stripe(t, "evt_p", "payment_intent.succeeded", fmt.Sprintf(`{"id":"pi_r1","amount_received":1000,"currency":"usd",
		"metadata":{"wpgenie_invoice":"%d"}}`, inv.ID))
	got, _ := e.svc.Invoice(ctx, inv.ID)
	if _, err := e.svc.Refund(ctx, inv.ID, RefundInput{PaymentID: got.Payments[0].ID, Amount: 300, To: "credit"}, "admin"); err != nil {
		t.Fatal(err)
	}
	// 200 refunded in Stripe's dashboard.
	e.stripe(t, "evt_r", "charge.refunded", `{"id":"ch_r1","payment_intent":"pi_r1","amount_refunded":200}`)
	got, _ = e.svc.Invoice(ctx, inv.ID)
	if got.AmountRefunded != 500 || got.Payments[0].Refunded != 500 {
		t.Fatalf("refunds %+v", got)
	}
}

// A cancellation at the end of the period drops the renewal of the next
// one (not charged, not chased); withdrawing it invoices it again.
func TestEndOfPeriodCancellationDropsTheNextRenewal(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", day(2026, 10, 1))
	card(e, a)
	e.run(day(2026, 9, 24)) // the October renewal
	if _, err := e.svc.RequestCancel(ctx, a.ID, "end_of_period", "moving"); err != nil {
		t.Fatal(err)
	}
	list := e.invoices(a.ID)
	if len(list) != 1 || list[0].Status != "cancelled" {
		t.Fatalf("renewal after a cancellation %+v", list)
	}
	e.run(day(2026, 9, 25))
	if n := len(e.invoices(a.ID)); n != 1 {
		t.Fatalf("renewed a cancelled account: %d invoices", n)
	}
	if _, err := e.svc.WithdrawCancel(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if c := e.run(day(2026, 9, 26)); c[CountRenewals] != 1 {
		t.Fatalf("not invoiced again after the withdrawal: %v", c)
	}
	// Due and charged again as usual.
	if c := e.run(day(2026, 10, 1)); c[CountAutocharged] != 1 {
		t.Fatalf("auto-pay %v", c)
	}
}

func card(e *invEnv, a *store.Account) {
	ctx := context.Background()
	p, _ := e.store.GetBillingProfile(ctx, a.ID)
	p.StripeCustomer, p.CardPM, p.CardLast4, p.AutoPay = "cus_T1", "pm_card1", "4242", true
	e.store.SaveBillingProfile(ctx, p)
}

// Staff cancelling a renewal waives its period: billing goes on with the
// next one.
func TestCancellingARenewalWaivesItsPeriod(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", day(2026, 10, 1))
	e.run(day(2026, 9, 24))
	inv := e.invoices(a.ID)[0]
	if _, err := e.svc.CancelInvoice(ctx, inv.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.Profile(ctx, a.ID); !p.NextDueAt.Equal(day(2026, 11, 1)) {
		t.Fatalf("next due %v", p.NextDueAt)
	}
	ev, _ := e.store.AccountEvents(ctx, a.ID, 5)
	if len(ev) == 0 || ev[0].Message[:6] != "Period" {
		t.Fatalf("events %+v", ev)
	}
	if c := e.run(day(2026, 10, 25)); c[CountRenewals] != 1 {
		t.Fatalf("November not billed: %v", c)
	}
	if st, _ := e.status(a.ID); st != store.AccountActive {
		t.Fatalf("suspended for a waived period: %s", st)
	}
}

// Suspension and termination look at the invoice as it is now.
func TestDunningRechecksTheInvoice(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", day(2026, 10, 1))
	e.run(day(2026, 9, 24))
	inv := e.invoices(a.ID)[0]
	now := day(2026, 10, 7)
	if ok, _ := e.svc.stillOwed(ctx, inv.ID, 5, now); !ok {
		t.Fatal("not owed")
	}
	// Due date moved on by staff: not yet.
	due := "2026-10-05"
	e.svc.UpdateInvoice(ctx, inv.ID, InvoiceUpdate{DueAt: &due})
	if ok, _ := e.svc.stillOwed(ctx, inv.ID, 5, now); ok {
		t.Fatal("owed before its new threshold")
	}
	e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: "bank", Amount: 1000})
	if ok, _ := e.svc.stillOwed(ctx, inv.ID, 1, day(2026, 12, 1)); ok {
		t.Fatal("owed once paid")
	}
}

// suspend_after_days 0: never suspended (the reminders still go).
func TestSuspendAfterZeroDaysIsNever(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.setting(func(c *InvoicingSettings) { c.Automation.SuspendAfterDays = 0 })
	a := e.billed("Acme", "basic", "monthly", day(2026, 10, 1))
	e.run(day(2026, 9, 24))
	if c := e.run(day(2026, 11, 20)); c[CountSuspended] != 0 || c[CountOverdueReminders] != 1 {
		t.Fatalf("run %v", c)
	}
	if st, _ := e.status(a.ID); st != store.AccountActive {
		t.Fatalf("suspended: %s", st)
	}
	cfg, _ := e.svc.Invoicing(ctx)
	if d := e.svc.suspendDate(cfg, e.invoices(a.ID)[0]); d != "" {
		t.Fatalf("suspend date %q", d)
	}
}

// Top-ups and plan changes left unpaid are never chased; they are
// withdrawn after a week.
func TestOffersAreNotChased(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.setting(func(c *InvoicingSettings) {
		c.BurstPacks = []BurstPack{{ID: "b500", Minutes: 500, Price: 500}}
		c.Automation.LateFee = LateFee{Type: "fixed", Amount: 100}
		c.Automation.TerminateAfterDays = 6
	})
	a := e.billed("Acme", "basic", "monthly", day(2026, 12, 1))
	buy, err := e.svc.BuyBurstPack(ctx, a.ID, "b500", MethodManual)
	if err != nil {
		t.Fatal(err)
	}
	// Even long past its due date: no reminder, fee, suspension.
	due := "2026-09-01"
	e.svc.UpdateInvoice(ctx, buy.Invoice.ID, InvoiceUpdate{DueAt: &due})
	if c := e.run(day(2026, 9, 25)); len(c) != 0 {
		t.Fatalf("chased: %v", c)
	}
	if p, _ := e.svc.Profile(ctx, a.ID); p.Overdue {
		t.Fatal("an unpaid top-up makes the account overdue")
	}
	c := e.run(day(2026, 9, 27).Add(13 * time.Hour)) // a week after it was issued (Sep 20 12:00)
	if c[CountOffersCancelled] != 1 || c[CountSuspended]+c[CountTerminated]+c[CountLateFees]+c[CountOverdueReminders] != 0 {
		t.Fatalf("run %v", c)
	}
	got, _ := e.store.GetInvoice(ctx, buy.Invoice.ID)
	if got.Status != "cancelled" || got.LateFeeAt != (time.Time{}) || e.mails("invoice.overdue", a.ID) != 0 {
		t.Fatalf("top-up %+v", got)
	}
	if st, _ := e.status(a.ID); st != store.AccountActive {
		t.Fatalf("account %s", st)
	}
}

// A reseller can't cancel with customers left; if customers appear after
// the request, the reseller is suspended (once) instead of an error every
// run. Cancellation dates are forgotten once terminated (or reactivated).
func TestResellerCancellationAndClearedDates(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	r := e.account(AccountInput{Name: "Res", Kind: "reseller", PlanID: "agency", Email: "res@x.test"})
	mode, cycle, nd := ModeInvoice, "monthly", "2026-10-01"
	if _, err := e.svc.UpdateProfile(ctx, r.ID, ProfileInput{Mode: &mode, Cycle: &cycle, NextDueAt: &nd}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RequestCancel(ctx, r.ID, "end_of_period", ""); err != nil {
		t.Fatal(err)
	}
	kid := e.account(AccountInput{Name: "Kid", PlanID: "small", ParentID: r.ID})
	if _, err := e.svc.RequestCancel(ctx, r.ID, "immediately", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled with customers: %v", err)
	}
	c := e.run(day(2026, 10, 1))
	if c[CountCancelBlocked] != 1 || c[CountCancelled] != 0 {
		t.Fatalf("run %v", c)
	}
	if st, reason := e.status(r.ID); st != store.AccountSuspended || reason != ReasonAdmin {
		t.Fatalf("reseller %s %s", st, reason)
	}
	if c := e.run(day(2026, 10, 2)); c[CountCancelBlocked] != 0 {
		t.Fatalf("blocked again: %v", c)
	}
	// Once the customer is gone, the cancellation goes through, and the
	// date is forgotten.
	e.svc.Terminate(ctx, kid.ID, false)
	if c := e.run(day(2026, 10, 3)); c[CountCancelled] != 1 {
		t.Fatalf("run %v", c)
	}
	p, _ := e.svc.Profile(ctx, r.ID)
	if st, _ := e.status(r.ID); st != store.AccountTerminated || p.CancelAt != nil || p.CancelReason != "" {
		t.Fatalf("after: %s %+v", st, p)
	}
	// Terminated otherwise (by staff) with a cancellation pending: cleared.
	a := e.billed("Acme", "basic", "monthly", day(2026, 11, 1))
	e.svc.RequestCancel(ctx, a.ID, "end_of_period", "bye")
	e.svc.Terminate(ctx, a.ID, false)
	if p, _ := e.svc.Profile(ctx, a.ID); p.CancelAt != nil {
		t.Fatal("cancellation kept after termination")
	}
}

// new_clients_only codes aren't for someone with an account already.
func TestNewClientsOnlyPromotions(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.svc.CreatePromotion(ctx, Promotion{Code: "NEW", Type: "percent", Value: 1000, Enabled: true, NewClientsOnly: true})
	e.account(AccountInput{Name: "Old", PlanID: "basic", Email: "old@client.test"})
	order := func(email, user string) error {
		in := OrderInput{PlanID: "basic", Cycle: "monthly", Promo: "new", Method: MethodManual,
			Contact: store.BillingContact{Email: email}}
		in.User.Username, in.User.Password = user, "a long password"
		_, err := e.svc.PlaceOrder(ctx, in, "203.0.113.1", func(string) (string, error) { return "h", nil })
		return err
	}
	if err := order("OLD@client.test", "old2"); !isField(err, "promo") {
		t.Fatalf("an existing client used a new-client code: %v", err)
	}
	if err := order("new@client.test", "newbie"); err != nil {
		t.Fatal(err)
	}
}

// An authorized (not captured) Razorpay payment isn't money yet.
func TestRazorpayAuthorizedIsNotPaid(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.setting(func(c *InvoicingSettings) { c.Currency = Currency{Code: "INR", Symbol: "₹", Decimals: 2} })
	a := e.billed("Shop", "basic", "monthly", e.now.AddDate(0, 1, 0))
	inv := e.manualInvoice(a.ID, 1000, false)
	e.gw.payments["pay_AUTH1234"] = `{"id":"pay_AUTH1234","amount":1000,"currency":"INR","status":"authorized"}`
	ref := fmt.Sprintf("wpg_%d_1", inv.ID)
	q := url.Values{"razorpay_payment_id": {"pay_AUTH1234"}, "razorpay_payment_link_id": {"plink_A"},
		"razorpay_payment_link_reference_id": {ref}, "razorpay_payment_link_status": {"paid"}}
	q.Set("razorpay_signature", rzpSign("rzsecret", "plink_A|"+ref+"|paid|pay_AUTH1234"))
	if _, err := e.svc.RazorpayCallback(ctx, q); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.Invoice(ctx, inv.ID); got.Status != "unpaid" || len(got.Payments) != 0 {
		t.Fatalf("authorized recorded as paid: %+v", got)
	}
}

// Staff see which accounts pay no tax because of their tax ID.
func TestProfileShowsTaxIDExemption(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.setting(func(c *InvoicingSettings) { c.Tax.Enabled, c.Tax.ExemptWithTaxID = true, true })
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	v, _ := e.svc.UpdateContact(ctx, a.ID, store.BillingContact{Email: "a@b.test", TaxID: "none"})
	if v.TaxExemptByTaxID {
		t.Fatal("exempt by a made-up tax ID")
	}
	v, _ = e.svc.UpdateContact(ctx, a.ID, store.BillingContact{Email: "a@b.test", TaxID: "DE123456789"})
	if !v.TaxExemptByTaxID {
		t.Fatal("not shown as exempt")
	}
}
