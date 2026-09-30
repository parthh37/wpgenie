package billing

import (
	"context"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// run runs the automation at a time, twice (the second must do nothing),
// and returns the first run's counts.
func (e *invEnv) run(at time.Time) map[string]int {
	e.t.Helper()
	e.now = at
	r, err := e.svc.RunInvoicingOnce(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	if len(r.Errors) > 0 {
		e.t.Fatalf("run at %s: %v", at, r.Errors)
	}
	again, err := e.svc.RunInvoicingOnce(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for k, n := range again.Counts {
		if n > 0 && k != CountAutochargeFailed {
			e.t.Fatalf("run again at %s did %s %d time(s)", at, k, n)
		}
	}
	return r.Counts
}

func TestAutomationDunning(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	e.setting(func(c *InvoicingSettings) {
		c.Automation.TerminateAfterDays = 10
		c.Automation.LateFee = LateFee{Type: "fixed", Amount: 500}
	})
	a := e.billed("Acme", "basic", "monthly", day(2026, 10, 1))
	b := e.billed("Bee", "basic", "monthly", day(2026, 10, 1))
	e.store.AssignSite(ctx, "sa", a.ID)
	e.store.AssignSite(ctx, "sb", b.ID)

	if c := e.run(day(2026, 9, 23)); c[CountRenewals] != 0 {
		t.Fatalf("renewal 8 days ahead: %v", c)
	}
	// 7 days before: the renewal, e-mailed once.
	if c := e.run(day(2026, 9, 24)); c[CountRenewals] != 2 {
		t.Fatalf("renewals %v", c)
	}
	list := e.invoices(a.ID)
	if len(list) != 1 || list[0].Kind != KindRenewal || list[0].Total != 1000 || !list[0].DueAt.Equal(day(2026, 10, 1)) ||
		!list[0].PeriodEnd.Equal(day(2026, 11, 1)) || e.mails("invoice.created", a.ID) != 1 {
		t.Fatalf("renewal %+v", list)
	}
	inv := list[0]
	// 3 days before: the reminder.
	if c := e.run(day(2026, 9, 28)); c[CountReminders] != 2 || e.mails("invoice.reminder", a.ID) != 1 {
		t.Fatalf("reminders %v", c)
	}
	// Overdue reminders on days 1 and 3; the late fee on day 3, once.
	if c := e.run(day(2026, 10, 2)); c[CountOverdueReminders] != 2 {
		t.Fatalf("overdue day 1 %v", c)
	}
	if c := e.run(day(2026, 10, 4)); c[CountOverdueReminders] != 2 || c[CountLateFees] != 2 {
		t.Fatalf("overdue day 3 %v", c)
	}
	got, _ := e.store.GetInvoice(ctx, inv.ID)
	if got.Total != 1500 || len(got.Items) != 2 || got.Items[1].Kind != ItemLateFee || e.mails("invoice.overdue", a.ID) != 2 {
		t.Fatalf("late fee %+v", got)
	}
	// Day 5: suspended, sites too.
	if c := e.run(day(2026, 10, 6)); c[CountSuspended] != 2 {
		t.Fatalf("suspensions %v", c)
	}
	if st, reason := e.status(a.ID); st != store.AccountSuspended || reason != ReasonBilling || !e.sites.suspended["sa"] ||
		e.mails("account.suspended", a.ID) != 1 {
		t.Fatalf("suspended: %s %s %v", st, reason, e.sites.suspended)
	}
	// A pays: back at once, the next period is billed from Nov 1.
	if _, err := e.svc.RecordPayment(ctx, inv.ID, PaymentInput{Gateway: "bank", Amount: 1500}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.status(a.ID); st != store.AccountActive || e.sites.suspended["sa"] || e.mails("account.unsuspended", a.ID) != 1 {
		t.Fatalf("after payment: %s %v", st, e.sites.suspended)
	}
	if p, _ := e.svc.Profile(ctx, a.ID); !p.NextDueAt.Equal(day(2026, 11, 1)) {
		t.Fatalf("next due %v", p.NextDueAt)
	}
	// Day 10: B is terminated (its sites kept: the setting is off).
	if c := e.run(day(2026, 10, 11)); c[CountTerminated] != 1 {
		t.Fatalf("terminations %v", c)
	}
	if st, _ := e.status(b.ID); st != store.AccountTerminated || e.mails("account.cancelled", b.ID) != 1 || len(e.sites.deleted) != 0 {
		t.Fatalf("B %s", st)
	}
	// A's next renewal, once, on its anchor.
	if c := e.run(day(2026, 10, 25)); c[CountRenewals] != 1 {
		t.Fatalf("second renewal %v", c)
	}
	runs, _ := e.svc.AutomationRuns(ctx)
	if len(runs) < 10 || runs[0].FinishedAt.IsZero() {
		t.Fatalf("runs %d", len(runs))
	}
}

func TestAutomationAutoPayCancellationsOverageOrders(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	// Auto-pay: charged on the due date, once a day; a decline e-mails.
	card := func(a *store.Account) {
		p, _ := e.store.GetBillingProfile(ctx, a.ID)
		p.StripeCustomer, p.CardPM, p.CardLast4, p.AutoPay = "cus_T1", "pm_card1", "4242", true
		e.store.SaveBillingProfile(ctx, p)
	}
	good := e.billed("Good", "basic", "monthly", day(2026, 10, 1))
	card(good)
	e.run(day(2026, 9, 24))
	if c := e.run(day(2026, 10, 1)); c[CountAutocharged] != 1 {
		t.Fatalf("autocharge %v", c)
	}
	if list := e.invoices(good.ID); list[0].Status != "paid" || list[0].Payments == nil && len(e.gw.charges) != 1 ||
		e.gw.charges[0].Get("off_session") != "true" || e.gw.charges[0].Get("amount") != "1000" {
		t.Fatalf("charged %+v %v", list[0], e.gw.charges)
	}
	off := false
	e.svc.SetAutoPay(ctx, good.ID, off)
	bad := e.billed("Bad", "basic", "monthly", day(2026, 11, 1))
	card(bad)
	e.gw.decline = true
	e.run(day(2026, 10, 25))
	if c := e.run(day(2026, 11, 1)); c[CountAutochargeFailed] != 1 || e.mails("invoice.payment_failed", bad.ID) != 1 {
		t.Fatalf("declined %v", c)
	}
	if n := len(e.gw.charges); n != 2 {
		t.Fatalf("%d charges (one a day)", n)
	}
	e.run(day(2026, 11, 2))
	if n := len(e.gw.charges); n != 3 || e.mails("invoice.payment_failed", bad.ID) != 2 {
		t.Fatalf("next day: %d charges", n)
	}

	// A cancellation at the end of the period: no renewal, terminated then.
	e.now = day(2026, 11, 2)
	c := e.billed("Leaving", "basic", "monthly", day(2026, 11, 10))
	if _, err := e.svc.RequestCancel(ctx, c.ID, "end_of_period", "too expensive"); err != nil {
		t.Fatal(err)
	}
	if n := e.run(day(2026, 11, 5))[CountRenewals]; n != 0 {
		t.Fatalf("renewed a cancelled account: %d", n)
	}
	if n := e.run(day(2026, 11, 10))[CountCancelled]; n != 1 {
		t.Fatalf("cancellations %d", n)
	}
	if st, _ := e.status(c.ID); st != store.AccountTerminated || e.mails("account.cancelled", c.ID) != 1 {
		t.Fatalf("cancelled: %s", st)
	}

	// Bandwidth overage for the month just ended: per started GB.
	e.now = day(2026, 11, 20)
	o := e.billed("Heavy", "basic", "monthly", day(2026, 12, 20)) // 10 GB, 1.00 per GB beyond
	e.store.AssignSite(ctx, "sh", o.ID)
	traffic(t, e.store, "sh", day(2026, 11, 21), 12*gb+gb/2)
	if c := e.run(day(2026, 12, 1).Add(time.Hour)); c[CountOverage] != 1 {
		t.Fatalf("overage %v", c)
	}
	var over *store.Invoice
	for _, inv := range e.invoices(o.ID) {
		if inv.Kind == KindOverage {
			over = inv
		}
	}
	if over == nil || over.Total != 300 || over.Items == nil && over.Subtotal != 300 {
		t.Fatalf("overage invoice %+v", over)
	}

	// Orders left unpaid for 14 days are cancelled.
	res, err := e.svc.PlaceOrder(ctx, OrderInput{PlanID: "basic", Cycle: "monthly", Method: MethodManual,
		Contact: store.BillingContact{Email: "x@y.test"}, User: struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}{"orderer", "a long password"}}, "203.0.113.1", func(string) (string, error) { return "h", nil })
	if err != nil {
		t.Fatal(err)
	}
	if n := e.run(day(2026, 12, 14))[CountOrdersCancelled]; n != 0 {
		t.Fatalf("cancelled early: %d", n)
	}
	if n := e.run(day(2026, 12, 16))[CountOrdersCancelled]; n != 1 {
		t.Fatalf("stale orders %d", n)
	}
	if st, _ := e.status(res.AccountID); st != store.AccountTerminated {
		t.Fatalf("stale order account %s", st)
	}
	if inv, _ := e.store.GetInvoice(ctx, res.InvoiceID); inv.Status != "cancelled" {
		t.Fatalf("stale order invoice %s", inv.Status)
	}
}

func TestOverview(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "annually", day(2026, 9, 25)) // 10000 a year: 833 a month
	e.billed("Bee", "pro", "monthly", day(2026, 10, 30))
	e.run(day(2026, 9, 21))
	list := e.invoices(a.ID)
	e.svc.RecordPayment(ctx, list[0].ID, PaymentInput{Gateway: "bank", Amount: 4000})
	e.now = day(2026, 10, 3)
	o, err := e.svc.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o.MRR != 833+3000 || o.BilledAccounts != 2 || o.IncomeLastMonth != 4000 || o.IncomeThisMonth != 0 ||
		o.UnpaidCount != 1 || o.Outstanding != 6000 || o.OverdueCount != 1 || o.OverdueTotal != 6000 ||
		len(o.IncomeByMonth) != 12 || o.IncomeByMonth[10].Month != "2026-09" || o.IncomeByMonth[10].Amount != 4000 ||
		len(o.RecentPayments) != 1 || len(o.OverdueInvoices) != 1 || len(o.UpcomingRenewals) != 1 {
		t.Fatalf("overview %+v", o)
	}
}
