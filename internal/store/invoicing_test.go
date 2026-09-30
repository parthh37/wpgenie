package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestStoreInvoicing(t *testing.T) { forEachBackend(t, testInvoicing) }

func testInvoicing(t *testing.T, s *Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	plan := &Plan{ID: "pro", Name: "Pro", Features: []string{}, BackupRepos: []string{}, Overage: "notify",
		Description: "For shops", Public: true, Sort: 5, AccountKind: AccountReseller, OverageGBPrice: 20,
		Prices: map[string]PlanPrice{"monthly": {Price: 1500}, "annually": {Price: 15000, SetupFee: 500}}}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPlan(ctx, "pro")
	if err != nil || got.Description != "For shops" || !got.Public || got.Sort != 5 || got.AccountKind != AccountReseller ||
		got.OverageGBPrice != 20 || got.Prices["annually"] != (PlanPrice{15000, 500}) || len(got.Prices) != 2 {
		t.Fatalf("plan %+v %v", got, err)
	}
	got.Prices, got.Public, got.AccountKind = nil, false, ""
	if err := s.UpdatePlan(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetPlan(ctx, "pro"); len(got.Prices) != 0 || got.Public || got.AccountKind != AccountCustomer {
		t.Fatalf("updated plan %+v", got)
	}
	a, _, _, err := s.CreateAccount(ctx, &Account{Name: "Acme", Kind: AccountCustomer, PlanID: "pro"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Profiles: none, then saved and updated; the credit isn't saved.
	if _, err := s.GetBillingProfile(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no profile: %v", err)
	}
	over := int64(1200)
	p := &BillingProfile{AccountID: a.ID, Mode: "invoice", Cycle: "monthly", PriceOverride: &over, NextDueAt: at,
		AnchorDay: 1, AutoPay: true, CardPM: "pm_1", CardLast4: "4242", Credit: 999, TaxExempt: true,
		Contact: BillingContact{Email: "a@b.test", Country: "IN"}}
	if err := s.SaveBillingProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
	gp, err := s.GetBillingProfile(ctx, a.ID)
	if err != nil || gp.Mode != "invoice" || *gp.PriceOverride != 1200 || !gp.NextDueAt.Equal(at) || !gp.AutoPay ||
		gp.CardLast4 != "4242" || gp.Credit != 0 || !gp.TaxExempt || gp.Contact.Country != "IN" || !gp.CancelAt.IsZero() {
		t.Fatalf("profile %+v %v", gp, err)
	}
	gp.PriceOverride = nil
	s.SaveBillingProfile(ctx, gp)
	if gp, _ = s.GetBillingProfile(ctx, a.ID); gp.PriceOverride != nil {
		t.Fatal("override not cleared")
	}
	if all, err := s.BillingProfiles(ctx); err != nil || len(all) != 1 {
		t.Fatalf("profiles %d %v", len(all), err)
	}

	// Credit: added, removed, never below zero, once per ref.
	if _, dup, err := s.AddCredit(ctx, a.ID, 1000, "Goodwill", "admin", "", at); err != nil || dup {
		t.Fatal(err)
	}
	if _, _, err := s.AddCredit(ctx, a.ID, -1500, "Too much", "admin", "", at); !errors.Is(err, ErrInsufficientCredit) {
		t.Fatalf("credit below zero: %v", err)
	}
	if e, _, err := s.AddCredit(ctx, a.ID, -200, "Fix", "admin", "k1", at); err != nil || e.Balance != 800 {
		t.Fatalf("remove: %+v %v", e, err)
	}
	if _, dup, _ := s.AddCredit(ctx, a.ID, -200, "Fix", "admin", "k1", at); !dup {
		t.Fatal("ref applied twice")
	}
	if list, _ := s.CreditEntries(ctx, a.ID, 10); len(list) != 2 || list[0].Amount != -200 || list[1].Balance != 1000 ||
		list[1].By != "admin" {
		t.Fatalf("ledger %+v", list)
	}
	// A credit without a profile makes one.
	b, _, _, _ := s.CreateAccount(ctx, &Account{Name: "Bee", Kind: AccountCustomer, PlanID: "pro"}, "", nil)
	if _, _, err := s.AddCredit(ctx, b.ID, 50, "x", "", "", at); err != nil {
		t.Fatal(err)
	}
	if bp, err := s.GetBillingProfile(ctx, b.ID); err != nil || bp.Credit != 50 || bp.Mode != "" {
		t.Fatalf("implicit profile %+v %v", bp, err)
	}

	// Invoices: numbered when issued, gapless.
	mk := func(status, key string) *Invoice {
		t.Helper()
		inv := &Invoice{AccountID: a.ID, AccountName: "Acme", Kind: "manual", Status: status, Currency: "USD", IssuedAt: at,
			DueAt: at.Add(7 * 24 * time.Hour), DedupeKey: key, InvoiceTotals: InvoiceTotals{Subtotal: 1000, Tax: 180,
				Total: 1180, TaxLines: []TaxLine{{"GST", 1800, 180}}},
			BillingAddress: BillingAddress{Name: "Jo", Lines: []string{"1 Road"}, Country: "IN"},
			Items:          []InvoiceItem{{Kind: "custom", Description: "Work", Quantity: 2, UnitPrice: 500, Amount: 1000, Taxable: true}}}
		var num *Numbering
		if status != InvoiceDraft {
			num = &Numbering{Prefix: "INV-"}
		}
		out, err := s.CreateInvoice(ctx, inv, num)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	i1 := mk(InvoiceUnpaid, "renewal:1")
	if i1.Number != "INV-000001" || len(i1.Items) != 1 || i1.Items[0].Amount != 1000 || i1.TaxLines[0].Amount != 180 ||
		i1.BillingAddress.Lines[0] != "1 Road" || i1.Balance() != 1180 || i1.DedupeKey != "renewal:1" {
		t.Fatalf("invoice %+v", i1)
	}
	inv := *i1
	inv.ID = 0
	if _, err := s.CreateInvoice(ctx, &inv, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("dedupe key: %v", err)
	}
	if got, err := s.InvoiceByDedupe(ctx, "renewal:1"); err != nil || got.ID != i1.ID {
		t.Fatalf("by dedupe %v", err)
	}
	d := mk(InvoiceDraft, "")
	if d.Number != "" {
		t.Fatal("draft numbered")
	}
	// Drafts are edited whole, then issued with the next number.
	d.Items = []InvoiceItem{{Kind: "custom", Description: "A", Quantity: 1, UnitPrice: 300, Amount: 300}}
	d.InvoiceTotals = InvoiceTotals{Subtotal: 300, Total: 300}
	d.Notes = "hello"
	if err := s.UpdateDraftInvoice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDraftInvoice(ctx, i1); !errors.Is(err, ErrInvoiceState) {
		t.Fatalf("edit unpaid: %v", err)
	}
	if err := s.IssueInvoice(ctx, d.ID, at, &Numbering{Prefix: "INV-"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueInvoice(ctx, d.ID, at, &Numbering{Prefix: "INV-"}); !errors.Is(err, ErrInvoiceState) {
		t.Fatalf("issued twice: %v", err)
	}
	d, _ = s.GetInvoice(ctx, d.ID)
	if d.Number != "INV-000002" || d.Status != InvoiceUnpaid || d.Total != 300 || len(d.Items) != 1 || d.Notes != "hello" {
		t.Fatalf("issued draft %+v", d)
	}
	if err := s.UpdateInvoiceNotes(ctx, d.ID, "later", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.NextInvoiceNumber(ctx); n != 3 {
		t.Fatalf("next number %d", n)
	}
	if err := s.SetNextInvoiceNumber(ctx, 2, "INV-"); !errors.Is(err, ErrConflict) {
		t.Fatalf("number reused: %v", err)
	}
	if err := s.SetNextInvoiceNumber(ctx, 100, "INV-"); err != nil {
		t.Fatal(err)
	}

	// Late fee: once.
	tot := InvoiceTotals{Subtotal: 1100, Tax: 180, Total: 1280, TaxLines: i1.TaxLines}
	fee := InvoiceItem{Kind: "late_fee", Description: "Late fee", Quantity: 1, UnitPrice: 100, Amount: 100}
	if ok, err := s.AddInvoiceItem(ctx, i1.ID, fee, tot, true, at); !ok || err != nil {
		t.Fatalf("late fee %v %v", ok, err)
	}
	if ok, _ := s.AddInvoiceItem(ctx, i1.ID, fee, tot, true, at); ok {
		t.Fatal("late fee twice")
	}
	if i1, _ = s.GetInvoice(ctx, i1.ID); i1.Total != 1280 || len(i1.Items) != 2 || i1.LateFeeAt.IsZero() {
		t.Fatalf("after late fee %+v", i1)
	}

	// Payments: partial, deduplicated, then the rest plus extra (credit),
	// numbered on payment when it had no number.
	pay := func(amount int64, dedupe string) *PaymentResult {
		t.Helper()
		r, err := s.RecordPayment(ctx, PaymentInput{InvoiceID: i1.ID, Gateway: "stripe", Reference: dedupe, Amount: amount,
			At: at, Dedupe: dedupe, Numbering: &Numbering{Prefix: "INV-"}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := pay(500, "stripe:pi_1"); r.Paid || r.Duplicate || r.Credited != 0 {
		t.Fatalf("partial %+v", r)
	}
	if r := pay(500, "stripe:pi_1"); !r.Duplicate || r.Payment.Amount != 500 {
		t.Fatalf("replay %+v", r)
	}
	if r := pay(1000, "stripe:pi_2"); !r.Paid || r.Credited != 220 || r.Payment.InvoiceNumber != "INV-000001" {
		t.Fatalf("rest %+v", r)
	}
	i1, _ = s.GetInvoice(ctx, i1.ID)
	if i1.Status != InvoicePaid || i1.AmountPaid != 1280 || i1.PaidAt.IsZero() || len(i1.Payments) != 2 || i1.EffectsDone {
		t.Fatalf("paid %+v", i1)
	}
	if bp, _ := s.GetBillingProfile(ctx, a.ID); bp.Credit != 1020 {
		t.Fatalf("credit %d", bp.Credit)
	}
	if list, _ := s.PaidInvoicesPending(ctx); len(list) != 1 || list[0].ID != i1.ID {
		t.Fatalf("pending effects %+v", list)
	}
	s.SetInvoiceEffectsDone(ctx, i1.ID)
	if list, _ := s.PaidInvoicesPending(ctx); len(list) != 0 {
		t.Fatal("effects still pending")
	}
	if _, err := s.RecordPayment(ctx, PaymentInput{InvoiceID: i1.ID, Gateway: "bank", Amount: 0, At: at}); err == nil {
		t.Fatal("zero payment")
	}

	// Credit applied to the other invoice pays it.
	applied, paid, err := s.ApplyCredit(ctx, d.ID, 0, "jo", at, nil)
	if err != nil || applied != 300 || !paid {
		t.Fatalf("apply credit %d %v %v", applied, paid, err)
	}
	if _, _, err := s.ApplyCredit(ctx, d.ID, 0, "jo", at, nil); !errors.Is(err, ErrInvoiceState) {
		t.Fatalf("credit on a paid invoice: %v", err)
	}
	i3 := mk(InvoiceUnpaid, "")
	if _, _, err := s.ApplyCredit(ctx, i3.ID, 5000, "jo", at, nil); err == nil {
		t.Fatal("more than the balance")
	}
	if applied, paid, _ := s.ApplyCredit(ctx, i3.ID, 100, "jo", at, nil); applied != 100 || paid {
		t.Fatalf("partial credit %d %v", applied, paid)
	}
	// Cancelling gives what was paid back.
	if err := s.CancelInvoice(ctx, i3.ID, "admin", at, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelInvoice(ctx, i3.ID, "admin", at, false); !errors.Is(err, ErrInvoiceState) {
		t.Fatalf("cancelled twice: %v", err)
	}
	if bp, _ := s.GetBillingProfile(ctx, a.ID); bp.Credit != 1020-300 {
		t.Fatalf("credit after cancel %d", bp.Credit)
	}
	// A cancelled invoice keeps its dedupe key, unless released.
	kept := mk(InvoiceUnpaid, "renewal:kept")
	s.CancelInvoice(ctx, kept.ID, "admin", at, false)
	again := *kept
	if _, err := s.CreateInvoice(ctx, &again, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("kept key reused: %v", err)
	}
	freed := mk(InvoiceUnpaid, "renewal:freed")
	s.CancelInvoice(ctx, freed.ID, "plan change", at, true)
	again = *freed
	remade, err := s.CreateInvoice(ctx, &again, nil)
	if err != nil {
		t.Fatalf("released key: %v", err)
	}
	s.CancelInvoice(ctx, remade.ID, "admin", at, false)

	// Refunds: partial, to credit, too much, deduplicated.
	p1 := i1.Payments[0]
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p1.ID, Amount: 600, At: at}); !errors.Is(err, ErrOverRefund) {
		t.Fatalf("over-refund: %v", err)
	}
	if r, dup, err := s.RecordRefund(ctx, RefundInput{PaymentID: p1.ID, Amount: 200, At: at, Dedupe: "razorpay:rfnd_1"}); err != nil ||
		dup || r.Amount != 200 {
		t.Fatalf("refund %+v %v", r, err)
	}
	if _, dup, _ := s.RecordRefund(ctx, RefundInput{PaymentID: p1.ID, Amount: 200, At: at, Dedupe: "razorpay:rfnd_1"}); !dup {
		t.Fatal("refund replayed")
	}
	if i1, _ = s.GetInvoice(ctx, i1.ID); i1.Status != InvoicePartiallyRefunded || i1.AmountRefunded != 200 ||
		i1.Payments[0].Refunded != 200 {
		t.Fatalf("partially refunded %+v", i1)
	}
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: i1.Payments[1].ID, Amount: 1000, ToCredit: true, At: at}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p1.ID, Amount: 300, At: at}); err != nil {
		t.Fatal(err)
	}
	if i1, _ = s.GetInvoice(ctx, i1.ID); i1.Status != InvoiceRefunded {
		t.Fatalf("refunded %+v", i1.Status)
	}
	if bp, _ := s.GetBillingProfile(ctx, a.ID); bp.Credit != 720+1000 {
		t.Fatalf("credit after refund to credit %d", bp.Credit)
	}
	if list, _ := s.Refunds(ctx, at, at.Add(time.Hour)); len(list) != 3 {
		t.Fatalf("refunds %d", len(list))
	}

	// Lists, filters and stats.
	i4 := mk(InvoiceUnpaid, "")
	mk(InvoiceDraft, "")
	list, _ := s.ListInvoices(ctx, InvoiceFilter{AccountID: a.ID, NoDrafts: true})
	if len(list) != 7 || list[0].ID != i4.ID {
		t.Fatalf("list %d", len(list))
	}
	if list, _ := s.ListInvoices(ctx, InvoiceFilter{Status: InvoiceUnpaid}); len(list) != 1 {
		t.Fatalf("unpaid %d", len(list))
	}
	if list, _ := s.ListInvoices(ctx, InvoiceFilter{Q: "000001"}); len(list) != 1 {
		t.Fatalf("search %d", len(list))
	}
	if list, _ := s.ListInvoices(ctx, InvoiceFilter{Q: "acm", Limit: 2, BeforeID: i4.ID}); len(list) != 2 || list[0].ID >= i4.ID {
		t.Fatalf("page %d", len(list))
	}
	if list, _ := s.ListInvoices(ctx, InvoiceFilter{OverdueAt: at.Add(8 * 24 * time.Hour)}); len(list) != 1 || list[0].ID != i4.ID {
		t.Fatalf("overdue %d", len(list))
	}
	if list, _ := s.ListInvoices(ctx, InvoiceFilter{From: at.Add(time.Hour)}); len(list) != 0 {
		t.Fatalf("from %d", len(list))
	}
	if list, _ := s.ListInvoices(ctx, InvoiceFilter{Statuses: []string{InvoicePaid, InvoiceRefunded}}); len(list) != 2 {
		t.Fatalf("statuses %d", len(list))
	}
	st, err := s.InvoiceStats(ctx, at.Add(8*24*time.Hour))
	if err != nil || st.UnpaidCount != 1 || st.Outstanding != 1180 || st.OverdueCount != 1 || st.OverdueTotal != 1180 {
		t.Fatalf("stats %+v %v", st, err)
	}
	pays, _ := s.Payments(ctx, PaymentFilter{AccountID: a.ID, Gateway: "stripe", From: at, To: at.Add(time.Hour)})
	if len(pays) != 2 || pays[0].ID < pays[1].ID || pays[0].AccountName != "Acme" {
		t.Fatalf("payments %+v", pays)
	}
	if p, err := s.PaymentByDedupe(ctx, "stripe:pi_2"); err != nil || p.Amount != 1000 {
		t.Fatalf("by dedupe %v", err)
	}
	if p, err := s.GetPayment(ctx, pays[0].ID); err != nil || p.Gateway != "stripe" {
		t.Fatalf("get payment %v", err)
	}

	// Autocharge once a day; payment attempts count.
	if ok, _ := s.SetInvoiceAutocharge(ctx, i4.ID, "2026-10-08"); !ok {
		t.Fatal("first autocharge")
	}
	if ok, _ := s.SetInvoiceAutocharge(ctx, i4.ID, "2026-10-08"); ok {
		t.Fatal("second autocharge the same day")
	}
	if n, _ := s.NextPayAttempt(ctx, i4.ID); n != 1 {
		t.Fatalf("attempt %d", n)
	}
	if n, _ := s.NextPayAttempt(ctx, i4.ID); n != 2 {
		t.Fatalf("attempt %d", n)
	}
	// A zero invoice settles.
	z, _ := s.CreateInvoice(ctx, &Invoice{AccountID: a.ID, Kind: "manual", Status: InvoiceUnpaid, Currency: "USD"}, nil)
	if ok, err := s.SettleInvoice(ctx, z.ID, at, &Numbering{Prefix: "INV-"}); !ok || err != nil {
		t.Fatalf("settle %v %v", ok, err)
	}
	if ok, _ := s.SettleInvoice(ctx, i4.ID, at, nil); ok {
		t.Fatal("settled an invoice with a balance")
	}

	// Promotions.
	pr, err := s.CreatePromotion(ctx, &Promotion{Code: "SAVE10", Type: "percent", Value: 1000, Plans: []string{"pro"},
		MaxUses: 1, Enabled: true, ExpiresAt: at})
	if err != nil || pr.Code != "SAVE10" || !slices.Equal(pr.Plans, []string{"pro"}) || !pr.ExpiresAt.Equal(at) {
		t.Fatalf("promo %+v %v", pr, err)
	}
	if _, err := s.CreatePromotion(ctx, &Promotion{Code: "SAVE10", Type: "fixed", Value: 1}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate code: %v", err)
	}
	if got, err := s.PromotionByCode(ctx, "save10"); err != nil || got.ID != pr.ID {
		t.Fatalf("by code %v", err)
	}
	if err := s.UsePromotion(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UsePromotion(ctx, pr.ID); !errors.Is(err, ErrPromoUsedUp) {
		t.Fatalf("used up: %v", err)
	}
	pr.MaxUses, pr.Description = 5, "Ten off"
	if err := s.UpdatePromotion(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Promotions(ctx); len(list) != 1 || list[0].Uses != 1 || list[0].Description != "Ten off" {
		t.Fatalf("promotions %+v", list)
	}

	// Tax rules.
	tr, err := s.CreateTaxRule(ctx, &TaxRule{Name: "CGST", Country: "IN", Rate: 900, Level: 1})
	if err != nil {
		t.Fatal(err)
	}
	tr.State, tr.Compound = "MH", true
	if err := s.UpdateTaxRule(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTaxRule(ctx, tr.ID); got.State != "MH" || !got.Compound {
		t.Fatalf("tax rule %+v", got)
	}
	if list, _ := s.TaxRules(ctx); len(list) != 1 {
		t.Fatal("tax rules")
	}
	if err := s.DeleteTaxRule(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTaxRule(ctx, tr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted twice")
	}

	// Orders: all or nothing.
	order := func(user string, promo int64) (*Order, error) {
		return s.CreateOrder(ctx, &NewOrder{
			Account:   &Account{Name: "Shop", Kind: AccountCustomer, PlanID: "pro", Email: "s@b.test"},
			User:      &NewUser{Username: user, PasswordHash: "x", Role: "customer"},
			Profile:   &BillingProfile{Mode: "invoice", Cycle: "monthly", AnchorDay: 1, Contact: BillingContact{Email: "s@b.test"}},
			Invoice:   &Invoice{Kind: "order", Status: InvoiceUnpaid, Currency: "USD", InvoiceTotals: InvoiceTotals{Total: 1500}},
			Numbering: &Numbering{Prefix: "INV-"}, PromoID: promo,
			Order: &Order{PlanID: "pro", Cycle: "monthly", Total: 1500, IP: "203.0.113.9"}, At: at})
	}
	o, err := order("shopper", 0)
	if err != nil {
		t.Fatal(err)
	}
	oa, _ := s.GetAccount(ctx, o.AccountID)
	oi, _ := s.GetInvoice(ctx, o.InvoiceID)
	if oa.Status != AccountPending || o.Status != OrderPending || oi.AccountID != oa.ID || oi.Number == "" || o.IP != "203.0.113.9" {
		t.Fatalf("order %+v %+v %+v", o, oa, oi)
	}
	if u, err := s.UserByName(ctx, "shopper"); err != nil || u.AccountID != oa.ID {
		t.Fatalf("order user %v", err)
	}
	if op, err := s.GetBillingProfile(ctx, oa.ID); err != nil || op.Mode != "invoice" {
		t.Fatalf("order profile %v", err)
	}
	n0, _ := s.ListAccounts(ctx, AccountFilter{})
	if _, err := order("shopper", 0); !errors.Is(err, ErrExists) {
		t.Fatalf("taken username: %v", err)
	}
	if _, err := order("other", pr.ID); err != nil {
		t.Fatal(err)
	}
	pr.MaxUses = 2
	s.UpdatePromotion(ctx, pr)
	if _, err := order("third", pr.ID); !errors.Is(err, ErrPromoUsedUp) {
		t.Fatalf("promotion used up: %v", err)
	}
	if n1, _ := s.ListAccounts(ctx, AccountFilter{}); len(n1) != len(n0)+1 {
		t.Fatalf("failed orders left accounts: %d -> %d", len(n0), len(n1))
	}
	if got, err := s.OrderByInvoice(ctx, o.InvoiceID); err != nil || got.ID != o.ID {
		t.Fatalf("by invoice %v", err)
	}
	if ok, _ := s.SetOrderStatus(ctx, o.ID, OrderPending, OrderActive); !ok {
		t.Fatal("activate")
	}
	if ok, _ := s.SetOrderStatus(ctx, o.ID, OrderPending, OrderCancelled); ok {
		t.Fatal("moved from the wrong status")
	}
	if list, _ := s.Orders(ctx, OrderPending, 10); len(list) != 1 {
		t.Fatalf("pending orders %d", len(list))
	}
	if list, _ := s.Orders(ctx, "", 10); len(list) != 2 {
		t.Fatalf("orders %d", len(list))
	}
	if err := s.DeletePromotion(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}

	// Automation runs.
	for i := range 3 {
		if err := s.AddInvoicingRun(ctx, &InvoicingRun{StartedAt: at, FinishedAt: at.Add(time.Duration(i) * time.Second),
			Counts: map[string]int{"renewals": i}, Errors: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if runs, err := s.InvoicingRuns(ctx, 2); err != nil || len(runs) != 2 || runs[0].Counts["renewals"] != 2 || runs[0].Errors[0] != "x" {
		t.Fatalf("runs %+v %v", runs, err)
	}
}

func TestStoreBurstTopups(t *testing.T) { forEachBackend(t, testBurstTopups) }

func testBurstTopups(t *testing.T, s *Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Features: []string{}, BackupRepos: []string{}, Overage: "notify"})
	a, _, _, err := s.CreateAccount(ctx, &Account{Name: "A", Kind: AccountCustomer, PlanID: "p"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateInvoice(ctx, &Invoice{AccountID: a.ID, Kind: "burst_topup", Status: InvoiceUnpaid, Currency: "USD",
		Units: 500, InvoiceTotals: InvoiceTotals{Subtotal: 500, Total: 500},
		Items: []InvoiceItem{{Kind: "burst", Description: "500 burst minutes", Quantity: 1, UnitPrice: 500, Amount: 500}}}, nil)
	if err != nil || inv.Units != 500 || inv.UnitsGranted {
		t.Fatalf("top-up %+v %v", inv, err)
	}
	if _, err := s.GrantBurstMinutes(ctx, inv.ID); !errors.Is(err, ErrInvoiceState) {
		t.Fatalf("granted before payment: %v", err)
	}
	if _, err := s.RecordPayment(ctx, PaymentInput{InvoiceID: inv.ID, Gateway: "bank", Amount: 500, At: at}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{500, 0, 0} {
		if n, err := s.GrantBurstMinutes(ctx, inv.ID); err != nil || n != want {
			t.Fatalf("grant %d: %d %v", i, n, err)
		}
	}
	if a, _ = s.GetAccount(ctx, a.ID); a.BurstCredit != 500 {
		t.Fatalf("burst credit %d", a.BurstCredit)
	}
	if inv, _ = s.GetInvoice(ctx, inv.ID); !inv.UnitsGranted {
		t.Fatal("not marked granted")
	}
}
