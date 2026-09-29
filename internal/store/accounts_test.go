package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAccountsAndOwnership(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Overage: "notify", Features: []string{"backups"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlan(ctx, &Plan{ID: "p", Name: "dup", Overage: "notify"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate plan: %v", err)
	}
	a, u, existed, err := st.CreateAccount(ctx, &Account{Name: "A", Kind: AccountCustomer, PlanID: "p"}, "k",
		&NewUser{Username: "ann", PasswordHash: "h", Role: "customer"})
	if err != nil || existed || u.AccountID != a.ID || a.Status != AccountActive {
		t.Fatalf("%+v %+v %v %v", a, u, existed, err)
	}
	// Concurrent retries with one key make one account.
	var wg sync.WaitGroup
	ids := make([]int64, 8)
	for i := range ids {
		wg.Go(func() {
			b, _, _, err := st.CreateAccount(ctx, &Account{Name: "B", Kind: AccountCustomer, PlanID: "p"}, "same", nil)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = b.ID
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("idempotency key made several accounts: %v", ids)
		}
	}
	// Accounts without a key never clash (NULL is not a duplicate).
	for range 2 {
		if _, _, _, err := st.CreateAccount(ctx, &Account{Name: "C", Kind: AccountCustomer, PlanID: "p"}, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeletePlan(ctx, "p"); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting a plan in use: %v", err)
	}

	// Ownership has no foreign key to sites (a site may be elsewhere), and
	// deleting a local site takes it along.
	if err := st.AssignSite(ctx, "remote1", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSite(ctx, &Site{ID: "s1", Name: "s1", PrimaryDomain: "a.test", PHPVersion: "8.3", FPMPort: 19000,
		DBName: "wp_s1", Status: StatusActive, ShieldMode: "standard"}); err != nil {
		t.Fatal(err)
	}
	st.AssignSite(ctx, "s1", a.ID)
	st.SetSiteUsage(ctx, SiteUsage{SiteID: "s1", FilesBytes: 5, MeasuredAt: time.Now()})
	if err := st.DeleteSite(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SiteOwnerOf(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("ownership outlived the site")
	}
	if u, _ := st.SiteUsages(ctx, []string{"s1"}); len(u) != 0 {
		t.Fatal("usage outlived the site")
	}
	if owners, _ := st.SiteOwners(ctx, a.ID); len(owners) != 1 || owners[0].SiteID != "remote1" {
		t.Fatalf("owners %+v", owners)
	}
	if err := st.DeleteAccount(ctx, a.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting an account that owns a site: %v", err)
	}
	st.UnassignSite(ctx, "remote1")
	if err := st.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("the account's user remains")
	}
}

func TestSSOTokensAreSingleUse(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "ann", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.CreateSSOToken(ctx, "hash", u.ID, now.Add(time.Minute))
	if id, err := st.SSOTokenUser(ctx, "hash", now); err != nil || id != u.ID {
		t.Fatalf("peek: %d %v", id, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 8 {
		wg.Go(func() {
			if _, err := st.ConsumeSSOToken(ctx, "hash", now); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d requests used one token", wins)
	}
	st.CreateSSOToken(ctx, "old", u.ID, now.Add(-time.Second))
	if _, err := st.ConsumeSSOToken(ctx, "old", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired token used")
	}
}

func TestVisibleJobs(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a, _ := st.CreateJob(ctx, "sa", "backup", "x")
	st.CreateJob(ctx, "sb", "backup", "x")
	gone, _ := st.CreateJob(ctx, "sgone", "create", "ann")
	st.SetJobOwner(ctx, gone, "user:1")
	list, err := st.VisibleJobs(ctx, []string{"sa"}, "user:1", false, 10)
	if err != nil || len(list) != 2 || list[0].ID != gone || list[1].ID != a {
		t.Fatalf("%+v %v", list, err)
	}
	if list, _ := st.VisibleJobs(ctx, nil, "", false, 10); len(list) != 0 {
		t.Fatalf("no scope sees %d jobs", len(list))
	}
}
