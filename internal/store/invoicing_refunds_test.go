package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Money a payment put in the account's credit (paid beyond the balance,
// or returned when its invoice was cancelled) is refunded once: refunding
// it takes the credit back, and can't once the credit is spent.
func TestStoreRefundsOfCredit(t *testing.T) { forEachBackend(t, testRefundsOfCredit) }

func testRefundsOfCredit(t *testing.T, s *Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Features: []string{}, BackupRepos: []string{}, Overage: "notify"})
	acct := func(name string) int64 {
		t.Helper()
		a, _, _, err := s.CreateAccount(ctx, &Account{Name: name, Kind: AccountCustomer, PlanID: "p", Email: name + "@x.test"}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	credit := func(id int64) int64 {
		p, err := s.GetBillingProfile(ctx, id)
		if err != nil {
			return 0
		}
		return p.Credit
	}
	invoice := func(id, total int64) *Invoice {
		t.Helper()
		inv, err := s.CreateInvoice(ctx, &Invoice{AccountID: id, Kind: "manual", Status: InvoiceUnpaid, Currency: "USD",
			InvoiceTotals: InvoiceTotals{Subtotal: total, Total: total}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return inv
	}
	pay := func(inv *Invoice, amount int64) *Payment {
		t.Helper()
		r, err := s.RecordPayment(ctx, PaymentInput{InvoiceID: inv.ID, Gateway: "stripe", Amount: amount, At: at})
		if err != nil {
			t.Fatal(err)
		}
		return r.Payment
	}
	// 1000 paid 1500, all refunded to credit: 1500 of credit, not 2000.
	a := acct("a")
	p := pay(invoice(a, 1000), 1500)
	if p.Credited != 500 || credit(a) != 500 {
		t.Fatalf("overpaid: %+v, credit %d", p, credit(a))
	}
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p.ID, Amount: 1500, ToCredit: true, At: at}); err != nil {
		t.Fatal(err)
	}
	inv, _ := s.GetInvoice(ctx, p.InvoiceID)
	if credit(a) != 1500 || inv.Status != InvoiceRefunded || inv.AmountRefunded != 1000 {
		t.Fatalf("refund to credit: credit %d, %s %d", credit(a), inv.Status, inv.AmountRefunded)
	}
	// Refunded to the payer: the credit it gave goes too.
	b := acct("b")
	p = pay(invoice(b, 1000), 1500)
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p.ID, Amount: 1500, At: at}); err != nil {
		t.Fatal(err)
	}
	if credit(b) != 0 {
		t.Fatalf("credit left after refunding it to the payer: %d", credit(b))
	}
	if n, _ := s.GatewayRefunded(ctx, p.ID); n != 1500 {
		t.Fatalf("gateway refunds %d", n)
	}
	// Spent credit can't be refunded again (the credit part goes first).
	c := acct("c")
	p = pay(invoice(c, 1000), 1500)
	if _, _, err := s.ApplyCredit(ctx, invoice(c, 500).ID, 0, "", at, nil); err != nil {
		t.Fatal(err)
	}
	for _, amount := range []int64{1500, 400} {
		if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p.ID, Amount: amount, At: at}); !errors.Is(err, ErrCreditSpent) {
			t.Fatalf("spent credit refunded (%d): %v", amount, err)
		}
	}
	if got, _ := s.GetPayment(ctx, p.ID); got.Refunded != 0 || got.Credited != 500 || credit(c) != 0 {
		t.Fatalf("a refused refund changed things: %+v, credit %d", got, credit(c))
	}
	// 400 paid, invoice cancelled (400 to credit), then 400 refunded: once.
	d := acct("d")
	inv = invoice(d, 1000)
	p = pay(inv, 400)
	if err := s.CancelInvoice(ctx, inv.ID, "admin", at, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetPayment(ctx, p.ID); got.Credited != 400 || credit(d) != 400 {
		t.Fatalf("cancelled: %+v, credit %d", got, credit(d))
	}
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p.ID, Amount: 400, At: at}); err != nil {
		t.Fatal(err)
	}
	if credit(d) != 0 {
		t.Fatalf("refunded and still in credit: %d", credit(d))
	}
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p.ID, Amount: 1, At: at}); !errors.Is(err, ErrOverRefund) {
		t.Fatalf("refunded twice: %v", err)
	}
	e := acct("e")
	inv = invoice(e, 1000)
	p = pay(inv, 400)
	s.CancelInvoice(ctx, inv.ID, "admin", at, false)
	if _, _, err := s.RecordRefund(ctx, RefundInput{PaymentID: p.ID, Amount: 400, ToCredit: true, At: at}); err != nil {
		t.Fatal(err)
	}
	if credit(e) != 400 {
		t.Fatalf("a refund to credit of what is credit already: %d", credit(e))
	}

	// Cancelling an order gives its promotion's use back; e-mails are found.
	pr, _ := s.CreatePromotion(ctx, &Promotion{Code: "ONE", Type: "fixed", Value: 1, MaxUses: 1, Enabled: true})
	o, err := s.CreateOrder(ctx, &NewOrder{Account: &Account{Name: "Shop", Kind: AccountCustomer, PlanID: "p", Email: "Shop@X.test"},
		Profile: &BillingProfile{Mode: "invoice", Cycle: "monthly"},
		Invoice: &Invoice{Kind: "order", Status: InvoiceUnpaid, Currency: "USD", PromoID: pr.ID, CreditThrough: at,
			InvoiceTotals: InvoiceTotals{Total: 100}},
		PromoID: pr.ID, Order: &Order{PlanID: "p", Cycle: "monthly"}, At: at})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetInvoice(ctx, o.InvoiceID); !got.CreditThrough.Equal(at) {
		t.Fatalf("credit through %v", got.CreditThrough)
	}
	if ok, _ := s.AccountEmailExists(ctx, "shop@x.TEST"); !ok {
		t.Fatal("e-mail not found")
	}
	if ok, _ := s.AccountEmailExists(ctx, "nobody@x.test"); ok {
		t.Fatal("unknown e-mail found")
	}
	if err := s.UsePromotion(ctx, pr.ID); !errors.Is(err, ErrPromoUsedUp) {
		t.Fatalf("max uses: %v", err)
	}
	s.CancelInvoice(ctx, o.InvoiceID, "automation", at, false)
	if err := s.UsePromotion(ctx, pr.ID); err != nil {
		t.Fatalf("use not given back: %v", err)
	}
}
