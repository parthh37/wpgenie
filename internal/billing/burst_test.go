package billing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// fakeBurst plays the servers sites run on: their minutes, and the pauses
// billing sends.
type fakeBurst struct {
	mu sync.Mutex
	// minutes are the current month's (month, zero: any month's), older
	// months' in past.
	minutes map[string]int64
	month   time.Time
	past    map[time.Time]map[string]int64
	paused  map[string]bool
	sent    int
}

func (f *fakeBurst) BurstMinutes(_ context.Context, month time.Time) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.minutes
	if !f.month.IsZero() && !month.Equal(f.month) {
		src = f.past[month]
	}
	out := map[string]int64{}
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

func (f *fakeBurst) SetBurstPaused(_ context.Context, id string, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	f.paused[id] = paused
	return nil
}

func (f *fakeBurst) BurstPaused(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k, v := range f.paused {
		if v {
			out[k] = true
		}
	}
	return out, nil
}

func (f *fakeBurst) add(site string, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minutes[site] += n
}

func (f *fakeBurst) isPaused(site string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused[site]
}

func TestBurstMinutes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fb := &fakeBurst{minutes: map[string]int64{}, paused: map[string]bool{}}
	e.svc.Burst = fb
	p := &store.Plan{ID: "burst", Name: "Burst", BurstMinutes: 100, Features: []string{FeatureBurst}}
	if err := NormalizePlan(p); err != nil {
		t.Fatal(err)
	}
	e.store.CreatePlan(ctx, p)
	unl := &store.Plan{ID: "unl", Name: "Unlimited burst", Features: []string{FeatureBurst}}
	NormalizePlan(unl)
	e.store.CreatePlan(ctx, unl)
	a := e.account(AccountInput{Name: "A", Kind: store.AccountCustomer, PlanID: "burst"})
	b := e.account(AccountInput{Name: "B", Kind: store.AccountCustomer, PlanID: "unl"})
	e.store.AssignSite(ctx, "a1", a.ID)
	e.store.AssignSite(ctx, "a2", a.ID)
	e.store.AssignSite(ctx, "b1", b.ID)
	meter := func() {
		t.Helper()
		if err := e.svc.MeterBurst(ctx); err != nil {
			t.Fatal(err)
		}
	}
	balance := func(acct *store.Account) *BurstBalance {
		t.Helper()
		bal, err := e.svc.BurstBalanceOf(ctx, acct)
		if err != nil {
			t.Fatal(err)
		}
		return bal
	}

	// A staff site bursts before it is given to A: A only pays for later.
	fb.add("staff", 30)
	fb.add("a1", 50)
	fb.add("b1", 5000)
	meter()
	if bal := balance(a); bal.Used != 50 || bal.Remaining != 50 || bal.Unlimited || !bal.Allowed || bal.PerSite["a1"] != 50 {
		t.Fatalf("A: %+v", bal)
	}
	if bal := balance(b); !bal.Unlimited || bal.Used != 5000 || !bal.Allowed {
		t.Fatalf("B: %+v", bal)
	}
	e.store.AssignSite(ctx, "staff", a.ID)
	fb.add("staff", 10)
	fb.add("a1", 22) // 82 of 100: the 80% notice
	meter()
	if bal := balance(a); bal.Used != 82 || fb.isPaused("a1") {
		t.Fatalf("A at 82: %+v paused %v", bal, fb.isPaused("a1"))
	}

	// Past the month's minutes: credit is used, then burst pauses on every
	// site of the account and nowhere else.
	if _, err := e.svc.AddBurstCredit(ctx, a.ID, 30); err != nil {
		t.Fatal(err)
	}
	fb.add("a2", 38) // 120: 20 from the credit
	meter()
	if bal := balance(a); bal.Used != 120 || bal.Credit != 10 || bal.Remaining != 10 || fb.isPaused("a2") {
		t.Fatalf("A on credit: %+v", bal)
	}
	meter() // metering again takes nothing more
	if bal := balance(a); bal.Credit != 10 {
		t.Fatalf("credit taken twice: %+v", bal)
	}
	fb.add("a1", 15)
	meter()
	if bal := balance(a); bal.Remaining != 0 || bal.Credit != 0 || !fb.isPaused("a1") || !fb.isPaused("a2") ||
		!fb.isPaused("staff") || fb.isPaused("b1") {
		t.Fatalf("A used up: %+v paused %v", bal, fb.paused)
	}
	// Giving a site away or deleting it gives nothing back; a site that
	// left the account isn't paused any more.
	e.store.UnassignSite(ctx, "a1")
	meter()
	if bal := balance(a); bal.Used != 135 || !fb.isPaused("a2") {
		t.Fatalf("after giving a site away: %+v", bal)
	}
	if fb.isPaused("a1") {
		t.Fatal("a site that left the account stays paused")
	}
	fb.mu.Lock()
	delete(fb.minutes, "a1")
	fb.mu.Unlock()
	meter()
	if bal := balance(a); bal.Used != 135 {
		t.Fatalf("after deleting a site: %+v", bal)
	}

	// Buying minutes resumes the sites; running out again is notified again.
	sent := fb.sent
	if _, err := e.svc.AddBurstCredit(ctx, a.ID, 5); err != nil {
		t.Fatal(err)
	}
	meter()
	if fb.isPaused("a2") || fb.sent == sent {
		t.Fatal("not resumed after buying minutes")
	}
	fb.add("a2", 5)
	meter()
	if !fb.isPaused("a2") {
		t.Fatal("not paused after using the bought minutes")
	}
	evs, _ := e.store.AccountEvents(ctx, a.ID, 50)
	var notes []string
	for _, ev := range evs {
		if ev.Kind == "burst" {
			notes = append(notes, ev.Message)
		}
	}
	joined := strings.Join(notes, "\n")
	if strings.Count(joined, "used up (") != 2 || !strings.Contains(joined, "80%") ||
		!strings.Contains(joined, "bought minutes are used now") {
		t.Fatalf("notifications:\n%s", joined)
	}

	// Pauses aren't sent again every minute.
	sent = fb.sent
	meter()
	meter()
	if fb.sent != sent {
		t.Fatalf("%d pauses re-sent", fb.sent-sent)
	}

	// A new month starts afresh.
	e.now = time.Date(2026, 10, 1, 0, 1, 0, 0, time.UTC)
	fb.mu.Lock()
	fb.minutes = map[string]int64{}
	fb.mu.Unlock()
	meter()
	if bal := balance(a); bal.Used != 0 || bal.Remaining != 100 || fb.isPaused("a2") {
		t.Fatalf("new month: %+v paused %v", bal, fb.paused)
	}
}

func TestBurstCreditAndPlanValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.account(AccountInput{Name: "A", Kind: store.AccountCustomer, PlanID: "small"})
	for _, n := range []int64{0, maxBurstCredit + 1, -maxBurstCredit - 1} {
		if _, err := e.svc.AddBurstCredit(ctx, a.ID, n); !errors.Is(err, ErrInvalid) {
			t.Errorf("AddBurstCredit(%d) = %v", n, err)
		}
	}
	if got, err := e.svc.AddBurstCredit(ctx, a.ID, 500); err != nil || got.BurstCredit != 500 {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ := e.svc.AddBurstCredit(ctx, a.ID, -800); got.BurstCredit != 0 {
		t.Fatalf("credit below zero: %+v", got)
	}
	if _, err := e.svc.AddBurstCredit(ctx, 999, 5); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if err := NormalizePlan(&store.Plan{ID: "x", Name: "X", BurstMinutes: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative minutes: %v", err)
	}
	// A reseller can't hand out more minutes than they have.
	parent := &store.Plan{BurstMinutes: 1000}
	if err := Fits(&store.Plan{ID: "c", BurstMinutes: 2000}, parent); !errors.Is(err, ErrForbidden) {
		t.Fatalf("more minutes than the reseller: %v", err)
	}
	if err := Fits(&store.Plan{ID: "c"}, parent); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unlimited minutes from a limited reseller: %v", err)
	}
	if err := Fits(&store.Plan{ID: "c", BurstMinutes: 500}, parent); err != nil {
		t.Fatal(err)
	}
	// Without burst metering nothing happens.
	e.svc.Burst = nil
	if err := e.svc.MeterBurst(ctx); err != nil {
		t.Fatal(err)
	}
}

// The review's cases: switching plans can't make credit, a plan without
// burst pauses it, a month's last minutes are still charged, and a pause
// found after a restart is lifted from a site no account owns.
func TestBurstEdges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fb := &fakeBurst{minutes: map[string]int64{}, paused: map[string]bool{}}
	e.svc.Burst = fb
	for _, p := range []*store.Plan{
		{ID: "b600", Name: "600", BurstMinutes: 600, Features: []string{FeatureBurst}},
		{ID: "bunl", Name: "Unlimited", Features: []string{FeatureBurst}},
	} {
		NormalizePlan(p)
		e.store.CreatePlan(ctx, p)
	}
	a := e.account(AccountInput{Name: "A", Kind: store.AccountCustomer, PlanID: "bunl"})
	e.store.AssignSite(ctx, "a1", a.ID)
	meter := func() {
		t.Helper()
		if err := e.svc.MeterBurst(ctx); err != nil {
			t.Fatal(err)
		}
	}
	credit := func() int64 {
		got, _ := e.store.GetAccount(ctx, a.ID)
		return got.BurstCredit
	}
	fb.add("a1", 5000)
	meter()
	for _, plan := range []string{"b600", "bunl", "b600", "bunl"} {
		if _, err := e.svc.UpdateAccount(ctx, a.ID, AccountInput{Name: "A", Kind: store.AccountCustomer, PlanID: plan}, false); err != nil {
			t.Fatal(err)
		}
		meter()
		if c := credit(); c != 0 {
			t.Fatalf("on %s: %d minutes of credit from nothing", plan, c)
		}
	}
	if fb.isPaused("a1") {
		t.Fatal("paused on an unlimited plan")
	}

	// A plan without burst pauses it.
	e.store.AssignSite(ctx, "b1", e.account(AccountInput{Name: "B", Kind: store.AccountCustomer, PlanID: "small"}).ID)
	fb.add("b1", 1)
	meter()
	if !fb.isPaused("b1") {
		t.Fatal("bursting on a plan without burst")
	}

	// The last minutes of September are charged early in October.
	if _, err := e.svc.UpdateAccount(ctx, a.ID, AccountInput{Name: "A", Kind: store.AccountCustomer, PlanID: "b600"}, false); err != nil {
		t.Fatal(err)
	}
	e.svc.AddBurstCredit(ctx, a.ID, 10000)
	meter()
	before := credit()
	fb.add("a1", 7) // counted in September, reported late
	sept, oct := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	fb.mu.Lock()
	fb.month, fb.past, fb.minutes = oct, map[time.Time]map[string]int64{sept: fb.minutes}, map[string]int64{}
	fb.mu.Unlock()
	e.now = oct.Add(20 * time.Minute)
	meter()
	if b, _ := e.store.AccountBurstFor(ctx, a.ID, sept); b.Used != 5007 || credit() != before-7 {
		t.Fatalf("September's last minutes: %+v, credit %d (was %d)", b, credit(), before)
	}

	// After a restart, a stored pause on a site nobody owns is lifted.
	fb.mu.Lock()
	fb.paused["gone"] = true
	fb.mu.Unlock()
	e.svc = &Service{Store: e.store, Sites: e.sites, Log: e.svc.Log, Now: e.svc.Now, Burst: fb}
	meter()
	if fb.isPaused("gone") {
		t.Fatal("a staff site stays paused after a restart")
	}
}
