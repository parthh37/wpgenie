package billing

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// fakeSites records what billing asked of sites.
type fakeSites struct {
	mu        sync.Mutex
	suspended map[string]bool
	deleted   []string
	// refuse makes Delete fail for a site until its staging copy is gone.
	stagingOf map[string]string // live -> staging
	calls     []string
}

func (f *fakeSites) Suspend(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "suspend "+id)
	f.suspended[id] = true
	return nil
}

func (f *fakeSites) Unsuspend(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "unsuspend "+id)
	delete(f.suspended, id)
	return nil
}

func (f *fakeSites) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if stg, ok := f.stagingOf[id]; ok && !slices.Contains(f.deleted, stg) {
		return errors.New("conflict: delete its staging site first")
	}
	f.deleted = append(f.deleted, id)
	return nil
}

type env struct {
	t     *testing.T
	svc   *Service
	store *store.Store
	sites *fakeSites
	now   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, store: st, sites: &fakeSites{suspended: map[string]bool{}, stagingOf: map[string]string{}},
		now: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
	log := slog.New(slog.DiscardHandler)
	e.svc = &Service{Store: st, Sites: e.sites, Log: log, Now: func() time.Time { return e.now },
		Hooks: &Webhooks{Store: st, Log: log}}
	ctx := context.Background()
	for _, p := range []*store.Plan{
		{ID: "small", Name: "Small", MaxSites: 2, DiskMB: 1024, BandwidthGB: 10, MaxReplicas: 2, MaxMemoryMB: 1024,
			MaxCPUs: 1, MaxDomains: 5, Features: []string{"backups", "staging"}, BackupRepos: []string{"local"},
			Resellable: true},
		{ID: "big", Name: "Big", MaxSites: 10, DiskMB: 10240, BandwidthGB: 100, MaxReplicas: 4, MaxMemoryMB: 2048,
			MaxCPUs: 2, MaxDomains: 20, Features: []string{"backups", "staging", "sftp"}, BackupRepos: []string{"local"}},
		{ID: "reseller", Name: "Reseller", MaxSites: 3, DiskMB: 20480, BandwidthGB: 200, MaxReplicas: 2,
			MaxMemoryMB: 1024, MaxCPUs: 1, MaxDomains: 10, Features: []string{"backups", "staging"},
			BackupRepos: []string{"local"}, Overage: OverageSuspend},
	} {
		if err := NormalizePlan(p); err != nil {
			t.Fatal(err)
		}
		if err := st.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) account(in AccountInput) *store.Account {
	e.t.Helper()
	a, _, _, err := e.svc.CreateAccount(context.Background(), in, "", nil, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) status(id int64) (string, string) {
	a, err := e.store.GetAccount(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.Status, a.SuspendReason
}

func TestPlanValidationAndFit(t *testing.T) {
	p := &store.Plan{ID: "Bad ID", Name: "x"}
	if err := NormalizePlan(p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	p = &store.Plan{ID: "p", Name: "P", Features: []string{"sftp", "backups", "sftp"}}
	if err := NormalizePlan(p); err != nil || strings.Join(p.Features, ",") != "backups,sftp" || p.Overage != "notify" {
		t.Fatalf("%v %v %q", err, p.Features, p.Overage)
	}
	for _, bad := range []*store.Plan{
		{ID: "p", Name: "P", Features: []string{"root-shell"}},
		{ID: "p", Name: "P", MaxSites: -1},
		{ID: "p", Name: "P", MaxMemoryMB: 64},
		{ID: "p", Name: "P", Overage: "delete"},
		{ID: "p", Name: "line\nbreak"},
	} {
		if err := NormalizePlan(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v accepted", bad)
		}
	}
	parent := &store.Plan{ID: "r", MaxSites: 10, DiskMB: 1000, MaxReplicas: 2, Features: []string{"backups"}}
	if err := Fits(&store.Plan{ID: "c", MaxSites: 5, DiskMB: 500, MaxReplicas: 2, BandwidthGB: 7,
		Features: []string{"backups"}}, parent); err != nil {
		t.Fatalf("should fit: %v", err)
	}
	for _, c := range []*store.Plan{
		{ID: "c", MaxSites: 11, DiskMB: 500, MaxReplicas: 1},                                  // more sites
		{ID: "c", MaxSites: 0, DiskMB: 500, MaxReplicas: 1},                                   // unlimited under a limit
		{ID: "c", MaxSites: 5, DiskMB: 500, MaxReplicas: 1, Features: []string{"phpmyadmin"}}, // a feature it lacks
	} {
		if err := Fits(c, parent); !errors.Is(err, ErrForbidden) {
			t.Errorf("%+v fits", c)
		}
	}
}

func TestCreateAccountIdempotentAndAtomic(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	in := AccountInput{Name: "Acme", PlanID: "small", WHMCSServiceID: "42"}
	a, u, existed, err := e.svc.CreateAccount(ctx, in, "whmcs-42", &store.NewUser{Username: "acme", PasswordHash: "h",
		Role: "customer"}, false)
	if err != nil || existed || u == nil || u.AccountID != a.ID || a.Kind != "customer" {
		t.Fatalf("%+v %+v %v %v", a, u, existed, err)
	}
	// A billing system retrying the same request gets the same account.
	b, u2, existed, err := e.svc.CreateAccount(ctx, in, "whmcs-42", &store.NewUser{Username: "acme", PasswordHash: "h",
		Role: "customer"}, false)
	if err != nil || !existed || b.ID != a.ID || u2 != nil {
		t.Fatalf("retry: %+v %v %v", b, existed, err)
	}
	// A taken username leaves no account behind.
	_, _, _, err = e.svc.CreateAccount(ctx, AccountInput{Name: "Other", PlanID: "small"}, "k2",
		&store.NewUser{Username: "ACME", PasswordHash: "h", Role: "customer"}, false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("taken username: %v", err)
	}
	all, _ := e.store.ListAccounts(ctx, store.AccountFilter{})
	if len(all) != 1 {
		t.Fatalf("%d accounts", len(all))
	}
	if _, _, _, err := e.svc.CreateAccount(ctx, AccountInput{Name: "X", PlanID: "nope"}, "", nil, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown plan: %v", err)
	}
	// Account creation is announced to webhook endpoints.
	if _, err := e.store.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{URL: "https://hooks.example/x", Secret: "s",
		Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.account(AccountInput{Name: "Hooked", PlanID: "small"})
	ds, _ := e.store.WebhookDeliveries(ctx, 0, 10)
	if len(ds) != 1 || ds[0].Event != EventAccountCreated || !strings.Contains(ds[0].Payload, `"name":"Hooked"`) {
		t.Fatalf("deliveries %+v", ds)
	}
}

func TestResellerHierarchy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.account(AccountInput{Name: "Reseller", Kind: "reseller", PlanID: "reseller"})
	cust := e.account(AccountInput{Name: "Cust", PlanID: "big"})
	cases := []struct {
		in         AccountInput
		byReseller bool
		want       error
	}{
		{AccountInput{Name: "C", PlanID: "small", ParentID: r.ID}, true, nil},
		{AccountInput{Name: "C", PlanID: "big", ParentID: r.ID}, true, ErrForbidden},                   // not resellable
		{AccountInput{Name: "C", PlanID: "small", ParentID: cust.ID}, false, ErrInvalid},               // parent not a reseller
		{AccountInput{Name: "C", Kind: "reseller", PlanID: "small", ParentID: r.ID}, true, ErrInvalid}, // resellers are top-level
		{AccountInput{Name: "C", PlanID: "big", ParentID: r.ID}, false, nil},                           // an admin may
	}
	for i, c := range cases {
		_, _, _, err := e.svc.CreateAccount(ctx, c.in, "", nil, c.byReseller)
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("case %d: %v, want %v", i, err, c.want)
		}
	}
	// Limits of a customer are narrowed by its reseller's plan.
	kid := e.account(AccountInput{Name: "Kid", PlanID: "big", ParentID: r.ID})
	l, err := e.svc.LimitsFor(ctx, kid)
	if err != nil {
		t.Fatal(err)
	}
	if l.MaxReplicas != 2 || l.MaxMemoryMB != 1024 || l.Has("sftp") || !l.Has("backups") {
		t.Fatalf("limits %+v", l)
	}
}

func TestSuspensionCascadeAndAuthority(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.account(AccountInput{Name: "Reseller", Kind: "reseller", PlanID: "reseller"})
	kid := e.account(AccountInput{Name: "Kid", PlanID: "small", ParentID: r.ID})
	e.store.AssignSite(ctx, "sres", r.ID)
	e.store.AssignSite(ctx, "skid", kid.ID)

	// A reseller suspends its customer; an administrator's suspension of
	// the same account outranks it, and the reseller can't lift that.
	if _, err := e.svc.Suspend(ctx, kid.ID, ReasonReseller); err != nil {
		t.Fatal(err)
	}
	if !e.sites.suspended["skid"] || e.sites.suspended["sres"] {
		t.Fatalf("suspended %v", e.sites.suspended)
	}
	e.svc.Suspend(ctx, kid.ID, ReasonAdmin)
	if _, reason := e.status(kid.ID); reason != ReasonAdmin {
		t.Fatalf("reason %s", reason)
	}
	if _, err := e.svc.Unsuspend(ctx, kid.ID, ReasonReseller); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reseller lifted an admin suspension: %v", err)
	}
	if _, err := e.svc.Unsuspend(ctx, kid.ID, ReasonAdmin); err != nil {
		t.Fatal(err)
	}
	if e.sites.suspended["skid"] {
		t.Fatal("site still suspended")
	}

	// Suspending the reseller takes its customers' sites down too, without
	// changing the customers' own status; lifting it brings them back.
	e.svc.Suspend(ctx, r.ID, ReasonBilling)
	if !e.sites.suspended["skid"] || !e.sites.suspended["sres"] {
		t.Fatalf("cascade: %v", e.sites.suspended)
	}
	if st, _ := e.status(kid.ID); st != store.AccountActive {
		t.Fatalf("customer status %s", st)
	}
	if err := e.svc.CheckNewSiteLocked(ctx, kid.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("new site under a suspended reseller: %v", err)
	}
	if _, err := e.svc.Unsuspend(ctx, r.ID, ReasonOverage); !errors.Is(err, ErrForbidden) {
		t.Fatalf("overage lifted a billing suspension: %v", err)
	}
	e.svc.Unsuspend(ctx, r.ID, ReasonBilling)
	if len(e.sites.suspended) != 0 {
		t.Fatalf("still suspended: %v", e.sites.suspended)
	}
	// A site given to a suspended account is suspended at once.
	e.svc.Suspend(ctx, kid.ID, ReasonAdmin)
	if err := e.svc.AssignSite(ctx, "snew", kid.ID); err != nil {
		t.Fatal(err)
	}
	if !e.sites.suspended["snew"] {
		t.Fatal("assigned site not suspended")
	}
}

func TestQuotas(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.account(AccountInput{Name: "Reseller", Kind: "reseller", PlanID: "reseller"}) // 3 sites in total
	a := e.account(AccountInput{Name: "A", PlanID: "small", ParentID: r.ID})             // 2 sites
	b := e.account(AccountInput{Name: "B", PlanID: "small", ParentID: r.ID})
	check := func(id int64) error {
		unlock := e.svc.LockQuota()
		defer unlock()
		return e.svc.CheckNewSiteLocked(ctx, id)
	}
	e.store.AssignSite(ctx, "a1", a.ID)
	if err := check(a.ID); err != nil {
		t.Fatal(err)
	}
	e.store.AssignSite(ctx, "a2", a.ID)
	if err := check(a.ID); !errors.Is(err, ErrQuota) {
		t.Fatalf("own plan: %v", err)
	}
	e.store.AssignSite(ctx, "b1", b.ID)
	// B has room in its plan, but the reseller's 3 sites are used.
	if err := check(b.ID); !errors.Is(err, ErrQuota) || !strings.Contains(err.Error(), "reseller") {
		t.Fatalf("reseller total: %v", err)
	}
	// Disk: a plan's space used up blocks new sites.
	c := e.account(AccountInput{Name: "C", PlanID: "small"})
	e.store.AssignSite(ctx, "c1", c.ID)
	e.store.SetSiteUsage(ctx, store.SiteUsage{SiteID: "c1", FilesBytes: 900 << 20, DBBytes: 200 << 20, MeasuredAt: e.now})
	if err := check(c.ID); !errors.Is(err, ErrQuota) || !strings.Contains(err.Error(), "disk") {
		t.Fatalf("disk: %v", err)
	}
	l, _ := e.svc.LimitsFor(ctx, c)
	if err := CheckResources(l, 3, 512, 1); !errors.Is(err, ErrQuota) {
		t.Fatal("3 replicas allowed")
	}
	if err := CheckResources(l, 2, 2048, 1); !errors.Is(err, ErrQuota) {
		t.Fatal("2 GB allowed")
	}
	if err := CheckResources(l, 2, 1024, 1); err != nil {
		t.Fatal(err)
	}
}

func traffic(t *testing.T, st *store.Store, site string, at time.Time, bytes int64) {
	t.Helper()
	err := st.ApplyTraffic(context.Background(), &store.TrafficBatch{Hourly: map[store.HourKey]*store.Counters{
		{SiteID: site, Hour: at.Truncate(time.Hour).Unix()}: {BytesOut: bytes, Requests: 1}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBandwidthThresholdsAndOverage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.account(AccountInput{Name: "Reseller", Kind: "reseller", PlanID: "reseller"}) // 200 GB, suspend
	kid := e.account(AccountInput{Name: "Kid", PlanID: "small", ParentID: r.ID})         // 10 GB, notify
	e.store.AssignSite(ctx, "s1", kid.ID)
	e.store.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{URL: "https://hooks.example/x", Secret: "s", Enabled: true,
		Events: []string{EventUsageThreshold}})

	// Last month's traffic doesn't count.
	traffic(t, e.store, "s1", e.now.AddDate(0, -1, 0), 500*gb)
	traffic(t, e.store, "s1", e.now.Add(-2*time.Hour), 8*gb+1)
	if err := e.svc.EvaluateUsage(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ := e.store.WebhookDeliveries(ctx, 0, 10)
	if len(ds) != 1 || !strings.Contains(ds[0].Payload, `"percent":80`) || !strings.Contains(ds[0].Payload, `"metric":"bandwidth"`) {
		t.Fatalf("deliveries %+v", ds)
	}
	// Notified once per level and month.
	e.svc.EvaluateUsage(ctx)
	if ds, _ := e.store.WebhookDeliveries(ctx, 0, 10); len(ds) != 1 {
		t.Fatalf("notified again: %d", len(ds))
	}
	// Past 100% on a notify plan: notified, not suspended.
	traffic(t, e.store, "s1", e.now.Add(-time.Hour), 3*gb)
	e.svc.EvaluateUsage(ctx)
	if st, _ := e.status(kid.ID); st != store.AccountActive {
		t.Fatalf("notify plan suspended the account")
	}
	if ds, _ := e.store.WebhookDeliveries(ctx, 0, 10); len(ds) != 2 || !strings.Contains(ds[0].Payload, `"percent":100`) {
		t.Fatalf("100%%: %+v", ds)
	}

	// The reseller's plan suspends: its total (every customer's sites) is
	// what counts.
	traffic(t, e.store, "s1", e.now, 190*gb)
	e.svc.EvaluateUsage(ctx)
	if st, reason := e.status(r.ID); st != store.AccountSuspended || reason != ReasonOverage {
		t.Fatalf("reseller %s %s", st, reason)
	}
	if !e.sites.suspended["s1"] {
		t.Fatal("customer site not suspended with its reseller")
	}
	// Next month: the allowance is back, and so is the reseller.
	e.now = e.now.AddDate(0, 1, 0)
	e.svc.EvaluateUsage(ctx)
	if st, _ := e.status(r.ID); st != store.AccountActive || e.sites.suspended["s1"] {
		t.Fatalf("new month: %s, sites %v", st, e.sites.suspended)
	}
	u, err := e.svc.UsageOf(ctx, r)
	if err != nil || u.BandwidthBytes != 0 || !u.IncludesCustomers || u.Sites != 1 {
		t.Fatalf("usage %+v %v", u, err)
	}
}

func TestTerminateDeletesSitesStagingFirst(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.account(AccountInput{Name: "A", PlanID: "small"})
	u, err := e.store.CreateAccountUser(ctx, a.ID, "ann", "h", "customer")
	if err != nil {
		t.Fatal(err)
	}
	e.store.CreateSession(ctx, &store.Session{ID: "x", UserID: u.ID, CreatedAt: e.now, LastSeenAt: e.now,
		ExpiresAt: e.now.Add(time.Hour)}, "hash")
	e.store.AssignSite(ctx, "live", a.ID)
	e.store.AssignSite(ctx, "stg", a.ID)
	e.sites.stagingOf["live"] = "stg"
	res, err := e.svc.Terminate(ctx, a.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Account.Status != store.AccountTerminated || len(res.Deleted) != 2 {
		t.Fatalf("%+v", res)
	}
	if owned, _ := e.store.SiteOwners(ctx, a.ID); len(owned) != 0 {
		t.Fatalf("ownership left: %v", owned)
	}
	if ss, _ := e.store.Sessions(ctx, u.ID); len(ss) != 0 {
		t.Fatal("sessions survived termination")
	}
	if _, err := e.svc.Suspend(ctx, a.ID, ReasonAdmin); !errors.Is(err, ErrConflict) {
		t.Fatalf("suspending a terminated account: %v", err)
	}
	if _, err := e.svc.Unsuspend(ctx, a.ID, ReasonBilling); !errors.Is(err, ErrForbidden) {
		t.Fatalf("billing reactivated a terminated account: %v", err)
	}
	if err := e.svc.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GetUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("users of a deleted account remain")
	}
}
