package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBurstLedger(t *testing.T) {
	forEachBackend(t, testBurstLedger)
}

func testBurstLedger(t *testing.T, st *Store) {
	ctx := context.Background()
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	next := month.AddDate(0, 1, 0)

	// Per-site minutes add up; a server's report only ever raises them.
	for range 3 {
		if err := st.AddBurstMinutes(ctx, "s1", month, 1); err != nil {
			t.Fatal(err)
		}
	}
	st.AddBurstMinutes(ctx, "s2", next, 5)
	st.SetBurstReport(ctx, "r1", "n1", month, 10)
	st.SetBurstReport(ctx, "r1", "n1", month, 4) // stale report
	// r1 moved from n1 to n2 (and s1 from here to n2): each server's count adds up.
	st.SetBurstReport(ctx, "r1", "n2", month, 2)
	st.SetBurstReport(ctx, "s1", "n2", month, 1)
	got, err := st.BurstMinutes(ctx, month)
	if err != nil || got["s1"] != 4 || got["r1"] != 12 || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if got, _ := st.BurstMinutes(ctx, month, "s1"); len(got) != 1 || got["s1"] != 4 {
		t.Fatalf("filtered: %v", got)
	}
	if got, _ := st.LocalBurstMinutes(ctx, month); len(got) != 1 || got["s1"] != 3 {
		t.Fatalf("this server's own: %v", got)
	}

	if err := st.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Overage: "notify", BurstMinutes: 100}); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPlan(ctx, "p"); p.BurstMinutes != 100 {
		t.Fatalf("plan: %+v", p)
	}
	a, _, _, err := st.CreateAccount(ctx, &Account{Name: "A", Kind: AccountCustomer, PlanID: "p"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.AddBurstCredit(ctx, a.ID, 50); err != nil || n != 50 {
		t.Fatalf("credit %d %v", n, err)
	}
	if n, _ := st.AddBurstCredit(ctx, a.ID, -80); n != 0 {
		t.Fatalf("credit went below zero: %d", n)
	}
	if _, err := st.AddBurstCredit(ctx, 999, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	st.AddBurstCredit(ctx, a.ID, 30)

	// Charges are per account and site; the credit is taken once per
	// minute beyond the allowance, however often the month is recorded.
	st.AddBurstCharge(ctx, a.ID, "s1", month, 3)
	st.AddBurstCharge(ctx, a.ID, "s1", month, 2)
	st.AddBurstCharge(ctx, 7, "s1", month, 1) // the site's earlier owner
	charged, _ := st.BurstCharged(ctx, month)
	if charged["s1"] != 6 {
		t.Fatalf("charged %v", charged)
	}
	by, _ := st.AccountBurstCharges(ctx, month)
	if by[a.ID]["s1"] != 5 || by[7]["s1"] != 1 {
		t.Fatalf("by account %v", by)
	}
	for range 2 {
		credit, err := st.ChargeBurst(ctx, a.ID, month, 110, 10, 100)
		if err != nil || credit != 20 {
			t.Fatalf("credit %d %v", credit, err)
		}
	}
	// 50 beyond the plan but only 20 left: 20 are paid, the rest forgiven.
	if credit, _ := st.ChargeBurst(ctx, a.ID, month, 150, 50, 100); credit != 0 {
		t.Fatalf("credit went below zero: %d", credit)
	}
	b, err := st.AccountBurstFor(ctx, a.ID, month)
	if err != nil || b.Used != 150 || b.FromCredit != 50 || b.CreditTaken != 30 || b.Notified != 100 {
		t.Fatalf("%+v %v", b, err)
	}
	// Minutes bought later are for later bursts.
	st.AddBurstCredit(ctx, a.ID, 100)
	if credit, _ := st.ChargeBurst(ctx, a.ID, month, 150, 50, 100); credit != 100 {
		t.Fatalf("after buying minutes: %d, want 100", credit)
	}
	// A bigger plan gives back what was paid, never more.
	if credit, _ := st.ChargeBurst(ctx, a.ID, month, 150, 0, 0); credit != 130 {
		t.Fatalf("given back: %d, want 130", credit)
	}
	if credit, _ := st.ChargeBurst(ctx, a.ID, month, 150, 0, 0); credit != 130 {
		t.Fatalf("given back twice: %d", credit)
	}
	if credit, _ := st.ChargeBurst(ctx, a.ID, month, 150, 50, 0); credit != 80 {
		t.Fatalf("back on the small plan: %d, want 80", credit)
	}
	if b, _ := st.AccountBurstFor(ctx, a.ID, month); b.FromCredit != 50 || b.CreditTaken != 50 {
		t.Fatalf("%+v", b)
	}
	for _, c := range []struct{ want, settled, taken, credit, s, tk, d int64 }{
		{10, 0, 0, 100, 10, 10, -10}, {10, 0, 0, 4, 10, 4, -4}, {10, 0, 0, 0, 10, 0, 0},
		{5, 10, 3, 0, 5, 0, 3}, {5, 10, 8, 0, 5, 3, 5}, {10, 10, 7, 9, 10, 7, 0},
	} {
		if s, tk, d := SettleBurst(c.want, c.settled, c.taken, c.credit); s != c.s || tk != c.tk || d != c.d {
			t.Errorf("SettleBurst(%d, %d, %d, %d) = %d, %d, %d; want %d, %d, %d", c.want, c.settled, c.taken, c.credit,
				s, tk, d, c.s, c.tk, c.d)
		}
	}
	if b, _ := st.AccountBurstFor(ctx, a.ID, next); b != (AccountBurst{}) {
		t.Fatalf("next month: %+v", b)
	}
	// Bandwidth metering keeps the burst columns.
	if _, err := st.RecordAccountUsage(ctx, a.ID, month, 1, 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.AccountBurstFor(ctx, a.ID, month); b.Used != 150 || b.FromCredit != 50 || b.CreditTaken != 50 {
		t.Fatalf("after usage: %+v", b)
	}

	// Sites carry their burst settings.
	s := &Site{ID: "b1", Name: "b", PrimaryDomain: "b.test", PHPVersion: "8.3", FPMPort: 19500, DBName: "wp_b1",
		Status: StatusActive, MemoryMB: 256, CPUs: 1, Replicas: 1}
	if err := st.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	until := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if err := st.SetBurst(ctx, "b1", "on", until); err != nil {
		t.Fatal(err)
	}
	st.SetBurstPaused(ctx, "b1", true)
	got2, _ := st.GetSite(ctx, "b1")
	if got2.BurstMode != "on" || !got2.BurstUntil.Equal(until) || !got2.BurstPaused {
		t.Fatalf("%+v", got2)
	}
	st.SetBurst(ctx, "b1", "auto", time.Time{})
	if got2, _ = st.GetSite(ctx, "b1"); got2.BurstMode != "auto" || !got2.BurstUntil.IsZero() {
		t.Fatalf("%+v", got2)
	}
	if err := st.SetScaling(ctx, "b1", true, 2, 5, 60, 80, 0, "on", until); err != nil {
		t.Fatal(err)
	}
	if got2, _ = st.GetSite(ctx, "b1"); !got2.Autoscale || got2.MinReplicas != 2 || got2.MaxReplicas != 5 ||
		got2.TargetWorkers != 80 || got2.BurstMode != "on" || !got2.BurstUntil.Equal(until) {
		t.Fatalf("%+v", got2)
	}
}
