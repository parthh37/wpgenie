package billing

import (
	"context"
	"slices"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Overview is the staff's billing dashboard.
type Overview struct {
	Currency         Currency        `json:"currency"`
	MRR              int64           `json:"mrr"`
	IncomeThisMonth  int64           `json:"income_this_month"`
	IncomeLastMonth  int64           `json:"income_last_month"`
	Outstanding      int64           `json:"outstanding"`
	OverdueTotal     int64           `json:"overdue_total"`
	OverdueCount     int             `json:"overdue_count"`
	UnpaidCount      int             `json:"unpaid_count"`
	BilledAccounts   int             `json:"billed_accounts"`
	PendingOrders    int             `json:"pending_orders"`
	CreditTotal      int64           `json:"credit_total"`
	IncomeByMonth    []MonthIncome   `json:"income_by_month"`
	RecentPayments   []store.Payment `json:"recent_payments"`
	OverdueInvoices  []InvoiceView   `json:"overdue_invoices"`
	UpcomingRenewals []Renewal       `json:"upcoming_renewals"`
}

// MonthIncome is a month's payments less refunds paid out ("2026-09").
type MonthIncome struct {
	Month  string `json:"month"`
	Amount int64  `json:"amount"`
}

// Renewal is an account's next renewal.
type Renewal struct {
	AccountID   int64     `json:"account_id"`
	AccountName string    `json:"account_name"`
	PlanID      string    `json:"plan_id"`
	PlanName    string    `json:"plan_name"`
	Cycle       string    `json:"cycle"`
	DueAt       time.Time `json:"due_at"`
	Amount      int64     `json:"amount"`
}

// Overview computes the dashboard. MRR is each active invoice-billed
// account's recurring price divided by its cycle's months; income is money
// received less refunds paid back (refunds to credit stay income).
func (s *Service) Overview(ctx context.Context) (*Overview, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	o := &Overview{Currency: cfg.Currency, IncomeByMonth: []MonthIncome{}, UpcomingRenewals: []Renewal{}}
	st, err := s.Store.InvoiceStats(ctx, OverdueCutoff(now))
	if err != nil {
		return nil, err
	}
	o.Outstanding, o.OverdueTotal, o.OverdueCount, o.UnpaidCount = st.Outstanding, st.OverdueTotal, st.OverdueCount, st.UnpaidCount

	// Income by month, the last 12 (this one included).
	month := MonthStart(now)
	from := month.AddDate(0, -11, 0)
	byMonth := map[string]int64{}
	pays, err := s.Store.Payments(ctx, store.PaymentFilter{From: from})
	if err != nil {
		return nil, err
	}
	for _, p := range pays {
		byMonth[p.At.Format("2006-01")] += p.Amount
	}
	refunds, err := s.Store.Refunds(ctx, from, time.Time{})
	if err != nil {
		return nil, err
	}
	for _, r := range refunds {
		if !r.ToCredit {
			byMonth[r.At.Format("2006-01")] -= r.Amount
		}
	}
	for m := from; !m.After(month); m = m.AddDate(0, 1, 0) {
		o.IncomeByMonth = append(o.IncomeByMonth, MonthIncome{Month: m.Format("2006-01"), Amount: byMonth[m.Format("2006-01")]})
	}
	o.IncomeThisMonth = byMonth[month.Format("2006-01")]
	o.IncomeLastMonth = byMonth[month.AddDate(0, -1, 0).Format("2006-01")]

	if o.RecentPayments, err = s.Store.Payments(ctx, store.PaymentFilter{Limit: 10}); err != nil {
		return nil, err
	}
	overdue, err := s.Store.ListInvoices(ctx, store.InvoiceFilter{OverdueAt: OverdueCutoff(now), Limit: 10})
	if err != nil {
		return nil, err
	}
	o.OverdueInvoices = make([]InvoiceView, len(overdue))
	for i, inv := range overdue {
		o.OverdueInvoices[i] = s.invoiceView(cfg, inv, now)
	}
	orders, err := s.Store.Orders(ctx, store.OrderPending, 1000)
	if err != nil {
		return nil, err
	}
	o.PendingOrders = len(orders)

	profiles, err := s.Store.BillingProfiles(ctx)
	if err != nil {
		return nil, err
	}
	plans := map[string]*store.Plan{}
	if list, err := s.Store.ListPlans(ctx); err == nil {
		for _, p := range list {
			plans[p.ID] = p
		}
	}
	soon := now.AddDate(0, 0, 30)
	for _, p := range profiles {
		o.CreditTotal += p.Credit
		a, err := s.Store.GetAccount(ctx, p.AccountID)
		if err != nil || ModeOf(a, p) != ModeInvoice || a.Status == store.AccountTerminated || a.Status == store.AccountPending {
			continue
		}
		o.BilledAccounts++
		plan := plans[a.PlanID]
		if plan == nil {
			continue
		}
		if p.Cycle == "" {
			p.Cycle = "monthly"
		}
		price, ok := recurringPrice(plan, p)
		if !ok {
			continue
		}
		if a.Status == store.AccountActive {
			o.MRR += MulDiv(price, 1, int64(CycleMonths(p.Cycle)))
		}
		if !p.NextDueAt.Before(now) && p.NextDueAt.Before(soon) && (p.CancelAt.IsZero() || p.CancelAt.After(p.NextDueAt)) {
			o.UpcomingRenewals = append(o.UpcomingRenewals, Renewal{AccountID: a.ID, AccountName: a.Name, PlanID: plan.ID,
				PlanName: plan.Name, Cycle: p.Cycle, DueAt: p.NextDueAt, Amount: price})
		}
	}
	slices.SortFunc(o.UpcomingRenewals, func(a, b Renewal) int { return a.DueAt.Compare(b.DueAt) })
	if len(o.UpcomingRenewals) > 10 {
		o.UpcomingRenewals = o.UpcomingRenewals[:10]
	}
	return o, nil
}
