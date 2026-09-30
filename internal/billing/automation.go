package billing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// The invoicing automation: every 15 minutes, one run at a time, each step
// keyed so that running it again (or after a crash half-way) does nothing
// twice:
//
//  1. renewal invoices, days_before_due ahead (one per account and
//     period: the invoice's dedupe key), credit applied, e-mailed;
//  2. saved cards charged on the due date (once per invoice per day);
//  3. a reminder before the due date and overdue reminders after it
//     (e-mail dedupe keys);
//  4. a late fee, once;
//  5. suspension (reason billing) after suspend_after_days, and back
//     once nothing is overdue;
//  6. termination after terminate_after_days (0: never);
//  7. cancellations whose date has come; bandwidth overage invoices for
//     the month just ended;
//  8. orders unpaid for 14 days cancelled.
//
// Paid invoices whose effects (activation, renewal…) failed are retried
// first.

// RunInvoicing runs the automation until ctx ends.
func (s *Service) RunInvoicing(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute): // let the panel settle after a start
	}
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		if _, err := s.RunInvoicingOnce(ctx); err != nil && !errors.Is(err, ErrConflict) {
			s.Log.Error("billing automation", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Automation run counters.
const (
	CountRenewals         = "renewals"
	CountAutocharged      = "autocharged"
	CountAutochargeFailed = "autocharge_failed"
	CountReminders        = "reminders"
	CountOverdueReminders = "overdue_reminders"
	CountLateFees         = "late_fees"
	CountSuspended        = "suspended"
	CountUnsuspended      = "unsuspended"
	CountTerminated       = "terminated"
	CountCancelled        = "cancelled"
	CountOverage          = "overage_invoices"
	CountOrdersCancelled  = "orders_cancelled"
	CountEffects          = "effects_applied"
)

// run is one automation run's state.
type run struct {
	*store.InvoicingRun
	cfg      *InvoicingSettings
	now      time.Time
	accounts map[int64]*store.Account
	profiles map[int64]*store.BillingProfile
	plans    map[string]*store.Plan
}

func (r *run) fail(what string, err error) {
	if len(r.Errors) < 50 {
		r.Errors = append(r.Errors, what+": "+err.Error())
	}
}

// RunInvoicingOnce runs the automation now (ErrConflict: it is running).
func (s *Service) RunInvoicingOnce(ctx context.Context) (*store.InvoicingRun, error) {
	if !s.runMu.TryLock() {
		return nil, fmt.Errorf("%w: the billing automation is running", ErrConflict)
	}
	defer s.runMu.Unlock()
	r := &run{InvoicingRun: &store.InvoicingRun{StartedAt: s.now(), Counts: map[string]int{}, Errors: []string{}}}
	var err error
	if r.cfg, err = s.Invoicing(ctx); err != nil {
		return nil, err
	}
	s.retryEffects(ctx, r)
	if r.cfg.Enabled {
		for _, step := range []func(context.Context, *run) error{s.renewals, s.dunning, s.liftSuspensions, s.dueCancellations,
			s.overageInvoices, s.staleOrders} {
			if ctx.Err() != nil {
				break
			}
			if err := s.loadRun(ctx, r); err != nil {
				return nil, err
			}
			if err := step(ctx, r); err != nil {
				r.fail("automation", err)
			}
		}
	}
	r.FinishedAt = s.now()
	if err := s.Store.AddInvoicingRun(context.WithoutCancel(ctx), r.InvoicingRun); err != nil {
		return nil, err
	}
	return r.InvoicingRun, nil
}

// loadRun reads what the steps look at, fresh for each (a step changes
// accounts and profiles).
func (s *Service) loadRun(ctx context.Context, r *run) error {
	r.now = s.now()
	accounts, err := s.Store.ListAccounts(ctx, store.AccountFilter{})
	if err != nil {
		return err
	}
	r.accounts = map[int64]*store.Account{}
	for _, a := range accounts {
		r.accounts[a.ID] = a
	}
	profiles, err := s.Store.BillingProfiles(ctx)
	if err != nil {
		return err
	}
	r.profiles = map[int64]*store.BillingProfile{}
	for _, p := range profiles {
		if p.Cycle == "" {
			p.Cycle = "monthly"
		}
		r.profiles[p.AccountID] = p
	}
	plans, err := s.Store.ListPlans(ctx)
	if err != nil {
		return err
	}
	r.plans = map[string]*store.Plan{}
	for _, p := range plans {
		r.plans[p.ID] = p
	}
	return nil
}

// invoiced: the account is billed by invoice and still a client.
func (r *run) invoiced(id int64) (*store.Account, *store.BillingProfile, bool) {
	a, p := r.accounts[id], r.profiles[id]
	if a == nil || p == nil || ModeOf(a, p) != ModeInvoice || a.ParentID != 0 ||
		a.Status == store.AccountTerminated || a.Status == store.AccountPending {
		return nil, nil, false
	}
	return a, p, true
}

func (s *Service) retryEffects(ctx context.Context, r *run) {
	list, err := s.Store.PaidInvoicesPending(ctx)
	if err != nil {
		r.fail("paid invoices", err)
		return
	}
	for _, inv := range list {
		s.payMu.Lock()
		err := s.paidLocked(ctx, r.cfg, inv, nil)
		s.payMu.Unlock()
		if err != nil {
			r.fail(fmt.Sprintf("invoice %d", inv.ID), err)
			continue
		}
		r.Counts[CountEffects]++
	}
}

// renewals makes the renewal invoices due within days_before_due.
func (s *Service) renewals(ctx context.Context, r *run) error {
	for id := range r.profiles {
		a, p, ok := r.invoiced(id)
		if !ok || p.NextDueAt.IsZero() || r.now.Before(p.NextDueAt.AddDate(0, 0, -r.cfg.Invoice.DaysBeforeDue)) {
			continue
		}
		if !p.CancelAt.IsZero() && !p.CancelAt.After(p.NextDueAt) {
			continue // cancelled by then
		}
		plan := r.plans[a.PlanID]
		if plan == nil {
			continue
		}
		price, ok := recurringPrice(plan, p)
		if !ok || price <= 0 {
			continue
		}
		key := fmt.Sprintf("renewal:%d:%d", a.ID, p.NextDueAt.Unix())
		if _, err := s.Store.InvoiceByDedupe(ctx, key); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		end := AddMonths(p.NextDueAt, CycleMonths(p.Cycle), p.AnchorDay)
		items := []store.InvoiceItem{{Kind: ItemPlan, Description: fmt.Sprintf("%s plan — %s", plan.Name,
			periodText(p.NextDueAt, end)), Quantity: 1, UnitPrice: price, Amount: price, Taxable: true}}
		var promoID int64
		if promo := s.recurringPromo(ctx, p, a.PlanID); promo != nil {
			items = append(items, discountItem(promo, promoDiscount(promo, price), r.cfg.Currency)...)
			promoID = promo.ID
		}
		s.payMu.Lock()
		inv, err := s.buildInvoice(ctx, r.cfg, newInvoice{Account: a, Profile: p, Kind: KindRenewal, Items: items,
			Due: p.NextDueAt, PeriodStart: p.NextDueAt, PeriodEnd: end, PlanID: a.PlanID, Cycle: p.Cycle, PromoID: promoID,
			Dedupe: key})
		if err == nil {
			_, err = s.createInvoiceLocked(ctx, r.cfg, inv, true)
		}
		s.payMu.Unlock()
		switch {
		case errors.Is(err, store.ErrExists):
		case err != nil:
			r.fail(fmt.Sprintf("renewal of account %d", a.ID), err)
		default:
			r.Counts[CountRenewals]++
		}
	}
	return nil
}

func days(t time.Time, n int) time.Time { return t.AddDate(0, 0, n) }

// dunning goes through the unpaid invoices: auto-pay, reminders, late
// fees, suspension and termination.
func (s *Service) dunning(ctx context.Context, r *run) error {
	unpaid, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{Status: store.InvoiceUnpaid})
	if err != nil {
		return err
	}
	a := r.cfg.Automation
	var sc *StripeSettings
	if a.Autocharge && r.cfg.Methods.Stripe.Enabled && r.cfg.StripeReady && s.Stripe != nil {
		if sc, err = s.StripeSettings(ctx); err != nil {
			return err
		}
	}
	// Oldest first: the first overdue invoice is the one e-mails name.
	slices.Reverse(unpaid)
	for _, inv := range unpaid {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		acct, p := r.accounts[inv.AccountID], r.profiles[inv.AccountID]
		if acct == nil || inv.DueAt.IsZero() || inv.Balance() <= 0 ||
			acct.Status == store.AccountTerminated || acct.Status == store.AccountPending {
			continue // orders awaiting payment are cancelled, not chased
		}
		if p == nil {
			p = &store.BillingProfile{AccountID: acct.ID}
		}
		due := inv.DueAt
		// 2. Auto-pay.
		if sc != nil && p.AutoPay && p.CardPM != "" && p.StripeCustomer != "" && !r.now.Before(due) {
			tried, paid, err := s.autocharge(ctx, r.cfg, sc, inv, p)
			switch {
			case err != nil:
				r.fail(fmt.Sprintf("charging invoice %d", inv.ID), err)
			case paid:
				r.Counts[CountAutocharged]++
				continue
			case tried:
				r.Counts[CountAutochargeFailed]++
			}
		}
		// 3. Reminders.
		if r.now.Before(due) {
			if a.ReminderDaysBefore > 0 && !r.now.Before(days(due, -a.ReminderDaysBefore)) &&
				s.mailInvoice(ctx, r.cfg, inv, "invoice.reminder", fmt.Sprintf("invoice.reminder:%d", inv.ID), nil) {
				r.Counts[CountReminders]++
			}
			continue
		}
		for i := len(a.OverdueReminderDays) - 1; i >= 0; i-- {
			d := a.OverdueReminderDays[i]
			if r.now.Before(days(due, d)) {
				continue
			}
			// Only the latest one due: after an outage, no burst of
			// older reminders.
			if s.mailInvoice(ctx, r.cfg, inv, "invoice.overdue", fmt.Sprintf("invoice.overdue:%d:%d", inv.ID, d),
				map[string]any{"Days": d, "SuspendDate": s.suspendDate(r.cfg, inv)}) {
				r.Counts[CountOverdueReminders]++
			}
			break
		}
		// 4. Late fee.
		if a.LateFee.Type != "none" && a.LateFee.Amount > 0 && inv.LateFeeAt.IsZero() && !r.now.Before(days(due, a.LateFeeAfterDays)) {
			fee := a.LateFee.Amount
			if a.LateFee.Type == "percent" {
				fee = MulDiv(inv.Balance(), a.LateFee.Amount, 10000)
			}
			if fee > 0 {
				// Not taxed: the invoice's taxes stay as issued.
				t := inv.InvoiceTotals
				t.Subtotal, t.Total = t.Subtotal+fee, t.Total+fee
				item := store.InvoiceItem{Kind: ItemLateFee, Description: "Late payment fee", Quantity: 1, UnitPrice: fee,
					Amount: fee}
				s.payMu.Lock()
				added, err := s.Store.AddInvoiceItem(ctx, inv.ID, item, t, true, r.now)
				s.payMu.Unlock()
				switch {
				case errors.Is(err, store.ErrInvoiceState):
				case err != nil:
					r.fail(fmt.Sprintf("late fee on invoice %d", inv.ID), err)
				case added:
					r.Counts[CountLateFees]++
					s.event(ctx, acct.ID, "billing", fmt.Sprintf("Late fee of %s added to invoice %s",
						FormatMoney(fee, r.cfg.Currency), displayNumber(inv)))
				}
			}
		}
		if _, _, ok := r.invoiced(acct.ID); !ok {
			continue // the rest is for accounts billed here
		}
		// 5. Suspension.
		if acct.Status == store.AccountActive && !r.now.Before(days(due, a.SuspendAfterDays)) {
			if _, err := s.Suspend(ctx, acct.ID, ReasonBilling); err != nil {
				r.fail(fmt.Sprintf("suspending account %d", acct.ID), err)
			} else {
				r.Counts[CountSuspended]++
				s.event(ctx, acct.ID, "billing", "Suspended: invoice "+displayNumber(inv)+" is overdue")
				s.mailInvoice(ctx, r.cfg, inv, "account.suspended", fmt.Sprintf("account.suspended:%d:%d", acct.ID, inv.ID), nil)
				if cur, err := s.Store.GetAccount(ctx, acct.ID); err == nil {
					r.accounts[acct.ID] = cur
				}
			}
		}
		// 6. Termination.
		if a.TerminateAfterDays > 0 && !r.now.Before(days(due, a.TerminateAfterDays)) {
			if _, err := s.Terminate(ctx, acct.ID, a.TerminateDeletesSites); err != nil {
				r.fail(fmt.Sprintf("terminating account %d", acct.ID), err)
				continue
			}
			r.Counts[CountTerminated]++
			s.event(ctx, acct.ID, "billing", "Terminated: invoice "+displayNumber(inv)+" is unpaid")
			s.mail(ctx, acct.ID, "account.cancelled", fmt.Sprintf("account.cancelled:%d", acct.ID),
				map[string]any{"Reason": "invoice " + displayNumber(inv) + " was not paid"})
			if cur, err := s.Store.GetAccount(ctx, acct.ID); err == nil {
				r.accounts[acct.ID] = cur
			}
		}
	}
	return nil
}

// liftSuspensions brings back accounts suspended for billing once nothing
// is overdue (paid by hand, credit applied, an invoice cancelled…).
func (s *Service) liftSuspensions(ctx context.Context, r *run) error {
	for id, a := range r.accounts {
		if a.Status != store.AccountSuspended || a.SuspendReason != ReasonBilling {
			continue
		}
		if err := s.liftBillingSuspension(ctx, id); err != nil {
			r.fail(fmt.Sprintf("unsuspending account %d", id), err)
			continue
		}
		if cur, err := s.Store.GetAccount(ctx, id); err == nil && cur.Status == store.AccountActive {
			r.Counts[CountUnsuspended]++
		}
	}
	return nil
}

// dueCancellations ends the services whose cancellation date has come.
func (s *Service) dueCancellations(ctx context.Context, r *run) error {
	for id, p := range r.profiles {
		a := r.accounts[id]
		if a == nil || p.CancelAt.IsZero() || r.now.Before(p.CancelAt) || a.Status == store.AccountTerminated {
			continue
		}
		s.payMu.Lock()
		err := s.cancelNowLocked(ctx, r.cfg, a, p.CancelReason)
		s.payMu.Unlock()
		if err != nil {
			r.fail(fmt.Sprintf("cancelling account %d", id), err)
			continue
		}
		r.Counts[CountCancelled]++
	}
	return nil
}

// overageInvoices bills last month's bandwidth beyond the plan, per
// started GB, to accounts billed here whose plan has an overage price.
// The month's total is the higher of what was recorded during it and what
// the rollups say now (its last hour included).
func (s *Service) overageInvoices(ctx context.Context, r *run) error {
	month := MonthStart(r.now)
	prev := MonthStart(month.Add(-time.Hour))
	for id := range r.profiles {
		a, p, ok := r.invoiced(id)
		if !ok {
			continue
		}
		plan := r.plans[a.PlanID]
		if plan == nil || plan.OverageGBPrice <= 0 || plan.BandwidthGB <= 0 {
			continue
		}
		key := fmt.Sprintf("overage:%d:%s", a.ID, prev.Format("2006-01"))
		if _, err := s.Store.InvoiceByDedupe(ctx, key); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		used := int64(0)
		if row, err := s.Store.AccountUsageFor(ctx, a.ID, prev); err == nil {
			used = row.BandwidthBytes
		} else if !errors.Is(err, store.ErrNotFound) {
			r.fail(fmt.Sprintf("usage of account %d", a.ID), err)
			continue
		}
		ids, err := s.scopeIDs(ctx, a)
		if err != nil {
			return err
		}
		owned, err := s.Store.SiteOwners(ctx, ids...)
		if err != nil {
			return err
		}
		siteIDs := make([]string, len(owned))
		for i, o := range owned {
			siteIDs[i] = o.SiteID
		}
		if len(siteIDs) > 0 {
			bw, err := s.usage().Bandwidth(ctx, siteIDs, prev, month)
			if err != nil {
				r.fail(fmt.Sprintf("bandwidth of account %d", a.ID), err)
				continue
			}
			var sum int64
			for _, b := range bw {
				sum += b
			}
			used = max(used, sum)
		}
		over := used - plan.BandwidthGB*gb
		if over <= 0 {
			continue
		}
		gbs := (over + gb - 1) / gb
		items := []store.InvoiceItem{{Kind: ItemOverage, Description: fmt.Sprintf("Bandwidth beyond the plan's %d GB in %s (%d GB)",
			plan.BandwidthGB, prev.Format("January 2006"), gbs), Quantity: gbs, UnitPrice: plan.OverageGBPrice,
			Amount: gbs * plan.OverageGBPrice, Taxable: true}}
		s.payMu.Lock()
		inv, err := s.buildInvoice(ctx, r.cfg, newInvoice{Account: a, Profile: p, Kind: KindOverage, Items: items,
			Due: days(Day(r.now), r.cfg.Invoice.DaysBeforeDue), PeriodStart: prev, PeriodEnd: month, Dedupe: key})
		if err == nil {
			_, err = s.createInvoiceLocked(ctx, r.cfg, inv, true)
		}
		s.payMu.Unlock()
		switch {
		case errors.Is(err, store.ErrExists):
		case err != nil:
			r.fail(fmt.Sprintf("overage of account %d", a.ID), err)
		default:
			r.Counts[CountOverage]++
		}
	}
	return nil
}

// staleOrders cancels orders left unpaid for staleOrderAge.
func (s *Service) staleOrders(ctx context.Context, r *run) error {
	orders, err := s.Store.Orders(ctx, store.OrderPending, 1000)
	if err != nil {
		return err
	}
	for _, o := range orders {
		if r.now.Sub(o.CreatedAt) < staleOrderAge {
			continue
		}
		inv, err := s.Store.GetInvoice(ctx, o.InvoiceID)
		if err == nil && inv.Status != store.InvoiceUnpaid {
			continue // paid, waiting for approval
		}
		s.payMu.Lock()
		err = s.cancelOrderLocked(ctx, o, "automation: unpaid for 14 days")
		s.payMu.Unlock()
		if err != nil {
			r.fail(fmt.Sprintf("order %d", o.ID), err)
			continue
		}
		r.Counts[CountOrdersCancelled]++
	}
	return nil
}

// AutomationRuns lists the latest automation runs.
func (s *Service) AutomationRuns(ctx context.Context) ([]store.InvoicingRun, error) {
	return s.Store.InvoicingRuns(ctx, 50)
}
