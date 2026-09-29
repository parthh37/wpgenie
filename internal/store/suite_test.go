package store

// The store suite: every query of the store, run on SQLite and on
// PostgreSQL (forEachBackend). New store code should get a case here.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/axiomhq/hyperloglog"
)

func newSite(id, domain string, port int) *Site {
	return &Site{ID: id, Name: id, PrimaryDomain: domain, PHPVersion: "8.3", FPMPort: port, DBName: "wp_" + id,
		Status: StatusProvisioning, ShieldMode: "standard", BlockAIBots: true, MemoryMB: 512, CPUs: 1.5, Replicas: 1,
		WAF: true, AdminAllow: []string{"203.0.113.7/32"}, PHP: PHPSettings{MemoryLimitMB: 256}}
}

func TestStoreSites(t *testing.T) { forEachBackend(t, testSites) }

func testSites(t *testing.T, s *Store) {
	ctx := context.Background()
	a := newSite("sa", "a.test", 19000)
	a.Domains = []string{"a.test", "www.a.test"}
	if err := s.CreateSite(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSite(ctx, newSite("sb", "b.test", 19001)); err != nil {
		t.Fatal(err)
	}
	// A domain belongs to one site; a port to one site.
	if err := s.CreateSite(ctx, newSite("sc", "a.test", 19002)); err == nil || !isUnique(err) {
		t.Errorf("duplicate domain: %v", err)
	}
	if err := s.CreateSite(ctx, newSite("sc", "c.test", 19000)); err == nil || !isUnique(err) {
		t.Errorf("duplicate port: %v", err)
	}
	if _, err := s.GetSite(ctx, "sc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("failed create left a site: %v", err)
	}

	got, err := s.GetSite(ctx, "sa")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "sa" || !got.BlockAIBots || got.CPUs != 1.5 || !got.WAF || got.PageCache ||
		!slices.Equal(got.AdminAllow, []string{"203.0.113.7/32"}) || got.PHP.MemoryLimitMB != 256 ||
		!slices.Equal(got.Domains, []string{"a.test", "www.a.test"}) || !slices.Equal(got.Upstreams, []int{19000}) ||
		got.Reputation != "challenge" || got.MinReplicas != 1 || got.TargetCPU != 70 || got.CreatedAt.IsZero() {
		t.Errorf("GetSite = %+v", got)
	}
	if _, err := s.GetSite(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSite(nope) = %v", err)
	}
	list, err := s.ListSites(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListSites = %d, %v", len(list), err)
	}

	// Domains.
	if err := s.AddDomain(ctx, "sa", "old.a.test", true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDomain(ctx, "sb", "old.a.test", false); !isUnique(err) {
		t.Errorf("domain attached twice: %v", err)
	}
	if ok, err := s.DomainExists(ctx, "old.a.test"); !ok || err != nil {
		t.Errorf("DomainExists = %v, %v", ok, err)
	}
	if err := s.SetPrimaryDomain(ctx, "sa", "www.a.test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSite(ctx, "sa")
	if got.PrimaryDomain != "www.a.test" || !slices.Equal(got.RedirectDomains, []string{"a.test", "old.a.test"}) ||
		!slices.Equal(got.Domains, []string{"www.a.test"}) {
		t.Errorf("after SetPrimaryDomain: %+v", got)
	}
	if err := s.SetPrimaryDomain(ctx, "sa", "b.test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another site's domain made primary: %v", err)
	}
	if err := s.SetDomainRedirect(ctx, "sa", "www.a.test", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("primary domain set to redirect: %v", err)
	}
	if err := s.SetDomainRedirect(ctx, "sa", "old.a.test", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDomain(ctx, "sa", "www.a.test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("primary domain removed: %v", err)
	}
	if err := s.RemoveDomain(ctx, "sa", "old.a.test"); err != nil {
		t.Fatal(err)
	}
	idx, err := s.DomainIndex(ctx)
	if err != nil || idx["a.test"] != "sa" || idx["b.test"] != "sb" || len(idx) != 3 {
		t.Errorf("DomainIndex = %v, %v", idx, err)
	}

	// Settings.
	if err := s.SetResources(ctx, "sa", 1024, 2.5, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCache(ctx, "sa", true, true, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetImageFormats(ctx, "sa", []string{"avif", "webp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSiteStatus(ctx, "sa", StatusActive); err != nil {
		t.Fatal(err)
	}
	sh := got.ShieldSettings()
	sh.Mode, sh.XMLRPC, sh.RateRPS, sh.Countries, sh.CountryMode, sh.BlockAIBots = "under_attack", true, 2.5, []string{"CN", "RU"}, "block", false
	if err := s.SetShield(ctx, "sa", sh); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAutoscale(ctx, "sa", true, 1, 4, 60, 80, 900); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPHP(ctx, "sa", "8.4", PHPSettings{UploadMaxMB: 64}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSMTP(ctx, "sa", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAutoUpdate(ctx, "sa", "all"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResources(ctx, "nope", 1, 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetResources(nope) = %v", err)
	}
	got, _ = s.GetSite(ctx, "sa")
	if got.MemoryMB != 1024 || got.CPUs != 2.5 || got.Replicas != 3 || !got.PageCache || !got.ObjectCache ||
		!got.CacheMobile || !slices.Equal(got.ImageFormats, []string{"avif", "webp"}) || got.Status != StatusActive ||
		got.ShieldMode != "under_attack" || !got.XMLRPC || got.RateRPS != 2.5 || got.BlockAIBots ||
		!slices.Equal(got.Countries, []string{"CN", "RU"}) || !got.Autoscale || got.MaxReplicas != 4 ||
		got.TargetWorkers != 80 || got.TargetResponseMS != 900 || got.PHPVersion != "8.4" || got.PHP.UploadMaxMB != 64 ||
		!got.SMTP || got.AutoUpdate != "all" {
		t.Errorf("after setters: %+v", got)
	}

	// Staging.
	st := newSite("ss", "staging.a.test", 19005)
	st.ParentID = "sa"
	if err := s.CreateSite(ctx, st); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.StagingOf(ctx, "sa"); err != nil || !slices.Equal(ids, []string{"ss"}) {
		t.Errorf("StagingOf = %v, %v", ids, err)
	}

	// Upstreams.
	if err := s.SetUpstreams(ctx, "sa", []int{19010, 19011}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpstreams(ctx, "sb", []int{19010}); !isUnique(err) {
		t.Errorf("port shared by two sites: %v", err)
	}
	got, _ = s.GetSite(ctx, "sa")
	if !slices.Equal(got.Upstreams, []int{19010, 19011}) {
		t.Errorf("upstreams %v", got.Upstreams)
	}

	if err := s.DeleteSite(ctx, "sa"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSite(ctx, "sa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if idx, _ := s.DomainIndex(ctx); len(idx) != 2 {
		t.Errorf("domains not cascaded: %v", idx)
	}
}

func TestStoreUsers(t *testing.T) { forEachBackend(t, testUsers) }

func testUsers(t *testing.T, s *Store) {
	ctx := context.Background()
	if n, err := s.CountUsers(ctx); n != 0 || err != nil {
		t.Fatalf("CountUsers = %d, %v", n, err)
	}
	first, err := s.CreateFirstUser(ctx, "Alice", "h1", "admin")
	if err != nil || first.ID == 0 || first.Username != "Alice" || first.Role != "admin" {
		t.Fatalf("CreateFirstUser = %+v, %v", first, err)
	}
	if _, err := s.CreateFirstUser(ctx, "mallory", "h", "admin"); !errors.Is(err, ErrExists) {
		t.Errorf("second first user: %v", err)
	}
	// Usernames are unique and found regardless of case.
	if _, err := s.CreateUser(ctx, "ALICE", "h", "viewer"); !errors.Is(err, ErrExists) {
		t.Errorf("case-variant duplicate: %v", err)
	}
	bob, err := s.CreateUser(ctx, "bob", "h2", "operator")
	if err != nil || bob.ID <= first.ID {
		t.Fatalf("CreateUser = %+v, %v", bob, err)
	}
	if u, err := s.UserByName(ctx, "aLiCe"); err != nil || u.ID != first.ID {
		t.Errorf("UserByName(aLiCe) = %+v, %v", u, err)
	}
	if _, err := s.UserByName(ctx, "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("UserByName(carol) = %v", err)
	}
	if list, err := s.ListUsers(ctx); err != nil || len(list) != 2 || list[0].Username != "Alice" {
		t.Errorf("ListUsers = %v, %v", list, err)
	}
	if n, _ := s.CountActiveAdmins(ctx, 0); n != 1 {
		t.Errorf("CountActiveAdmins = %d", n)
	}
	if n, _ := s.CountActiveAdmins(ctx, first.ID); n != 0 {
		t.Errorf("CountActiveAdmins(except alice) = %d", n)
	}
	if err := s.SetUserRole(ctx, bob.ID, "admin", true); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountActiveAdmins(ctx, 0); n != 1 {
		t.Errorf("disabled admin counted: %d", n)
	}
	if err := s.SetUserPassword(ctx, bob.ID, "h3"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchLogin(ctx, bob.ID, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetUser(ctx, bob.ID)
	if !u.Disabled || u.Role != "admin" || u.PasswordHash != "h3" || u.LastLoginAt.Unix() != 1700000000 {
		t.Errorf("bob = %+v", u)
	}

	// TOTP and recovery codes.
	if err := s.SetTOTPPending(ctx, bob.ID, "PENDING"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTOTP(ctx, bob.ID, "SECRET", 100, []string{"r1", "r2"}); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUser(ctx, bob.ID)
	if !u.TOTPEnabled || u.TOTPPending != "" || u.TOTPLastStep != 100 || u.RecoveryLeft != 2 {
		t.Errorf("after EnableTOTP: %+v", u)
	}
	for _, c := range []struct {
		step int64
		ok   bool
	}{{100, false}, {99, false}, {101, true}, {101, false}} {
		if ok, err := s.UseTOTPStep(ctx, bob.ID, c.step); ok != c.ok || err != nil {
			t.Errorf("UseTOTPStep(%d) = %v, %v", c.step, ok, err)
		}
	}
	if ok, _ := s.UseRecoveryCode(ctx, bob.ID, "r2"); !ok {
		t.Error("recovery code refused")
	}
	if ok, _ := s.UseRecoveryCode(ctx, bob.ID, "r2"); ok {
		t.Error("recovery code used twice")
	}
	if u, _ := s.GetUser(ctx, bob.ID); !slices.Equal(u.RecoveryHashes, []string{"r1"}) {
		t.Errorf("recovery left %v", u.RecoveryHashes)
	}
	if err := s.DisableTOTP(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}

	// Sessions.
	now := time.Unix(1700000000, 0)
	for i, id := range []string{"s1", "s2"} {
		sess := &Session{ID: id, UserID: bob.ID, CreatedAt: now, LastSeenAt: now.Add(time.Duration(i) * time.Minute),
			ExpiresAt: now.Add(time.Hour), IP: "192.0.2.1", UserAgent: "test"}
		if err := s.CreateSession(ctx, sess, "hash-"+id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession(ctx, &Session{ID: "s3", UserID: first.ID, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(time.Hour)}, "hash-s1"); !isUnique(err) {
		t.Errorf("token hash reused: %v", err)
	}
	sess, err := s.SessionByToken(ctx, "hash-s1")
	if err != nil || sess.ID != "s1" || sess.Username != "bob" || sess.UserID != bob.ID {
		t.Fatalf("SessionByToken = %+v, %v", sess, err)
	}
	s.TouchSession(ctx, "s1", now.Add(10*time.Minute))
	s.TouchSession(ctx, "s1", now.Add(5*time.Minute)) // late, older: ignored
	if sess, _ := s.SessionByToken(ctx, "hash-s1"); sess.LastSeenAt.Unix() != now.Add(10*time.Minute).Unix() {
		t.Errorf("TouchSession went back: %v", sess.LastSeenAt)
	}
	if list, _ := s.Sessions(ctx, bob.ID); len(list) != 2 || list[0].ID != "s1" {
		t.Errorf("Sessions = %v", list)
	}
	if list, _ := s.Sessions(ctx, 0); len(list) != 2 {
		t.Errorf("all Sessions = %v", list)
	}
	if err := s.DeleteSession(ctx, "s2", first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted someone else's session: %v", err)
	}
	if err := s.PruneSessions(ctx, now.Add(8*time.Minute), 7*time.Minute); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Sessions(ctx, 0); len(list) != 1 || list[0].ID != "s1" {
		t.Errorf("after prune: %v", list)
	}
	if err := s.DeleteUserSessions(ctx, bob.ID, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "s1", bob.ID); err != nil {
		t.Fatal(err)
	}

	// Audit log.
	for i := range 3 {
		if err := s.AddAudit(ctx, AuditEntry{Actor: "alice", IP: "192.0.2.1", Action: "site.create",
			Target: "s" + strconv.Itoa(i), Status: 201}); err != nil {
			t.Fatal(err)
		}
	}
	s.AddAudit(ctx, AuditEntry{Actor: "bob", Action: "login"})
	if a, err := s.Audit(ctx, "alice", 2); err != nil || len(a) != 2 || a[0].Target != "s2" || a[0].Status != 201 {
		t.Errorf("Audit = %+v, %v", a, err)
	}
	if a, _ := s.Audit(ctx, "", 10); len(a) != 4 {
		t.Errorf("Audit(all) = %d", len(a))
	}

	// Plugin reports.
	if _, _, err := s.PluginReport(ctx, "sx"); !errors.Is(err, ErrNotFound) {
		t.Errorf("PluginReport = %v", err)
	}
	s.CreateSite(ctx, newSite("sx", "x.test", 19100))
	if err := s.SavePluginReport(ctx, "sx", now, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePluginReport(ctx, "sx", now.Add(time.Hour), []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if at, r, err := s.PluginReport(ctx, "sx"); err != nil || string(r) != `{"a":2}` || !at.Equal(now.Add(time.Hour)) {
		t.Errorf("PluginReport = %v %s %v", at, r, err)
	}

	if err := s.DeleteUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUser(ctx, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUser(deleted) = %v", err)
	}
	// Ids aren't reused.
	carol, err := s.CreateUser(ctx, "carol", "h", "viewer")
	if err != nil || carol.ID <= bob.ID {
		t.Errorf("CreateUser after delete = %+v, %v", carol, err)
	}
}

func sketchOf(items ...string) *hyperloglog.Sketch {
	sk := hyperloglog.New16()
	for _, it := range items {
		sk.Insert([]byte(it))
	}
	return sk
}

func TestStoreTraffic(t *testing.T) { forEachBackend(t, testTraffic) }

func testTraffic(t *testing.T, s *Store) {
	ctx := context.Background()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	h1, h2 := day.Add(time.Hour).Unix(), day.Add(2*time.Hour).Unix()
	batch := func(visitors ...string) *TrafficBatch {
		return &TrafficBatch{
			Hourly: map[HourKey]*Counters{
				{"s1", h1}: {Requests: 10, PageViews: 5, BytesOut: 1000, BotHits: 1, Blocked: 2, Errors5xx: 1},
				{"s1", h2}: {Requests: 1},
			},
			Visitors: map[DayKey]*hyperloglog.Sketch{{"s1", day.Unix()}: sketchOf(visitors...)},
			Perf: map[HourKey]*PerfCounters{{"s1", h1}: {PHPRequests: 4, PHPMS: 400, Slow: 1, CacheHits: 3, CacheMisses: 1,
				Hist: Histogram{1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}}},
			Slow:  map[SlowKey]*SlowAgg{{"s1", "GET", "/slow"}: {Count: 1, TotalMS: 1500, MaxMS: 1500, LastStatus: 200, LastSeen: h1}},
			State: IngestState{Name: "access.log", Inode: 1 << 40, Offset: 12345},
		}
	}
	if err := s.ApplyTraffic(ctx, batch("a", "b")); err != nil {
		t.Fatal(err)
	}
	b2 := batch("b", "c")
	b2.Slow[SlowKey{"s1", "GET", "/slow"}] = &SlowAgg{Count: 1, TotalMS: 3000, MaxMS: 3000, LastStatus: 504, LastSeen: h2}
	b2.State.Offset = 99999
	if err := s.ApplyTraffic(ctx, b2); err != nil {
		t.Fatal(err)
	}
	st, err := s.SiteStats(ctx, "s1", day)
	if err != nil {
		t.Fatal(err)
	}
	if st.Totals.Requests != 22 || st.Totals.BytesOut != 2000 || st.Totals.Errors5xx != 2 || len(st.Series) != 2 ||
		st.UniqueVisitors != 3 {
		t.Errorf("SiteStats = %+v", st)
	}
	is, err := s.IngestState(ctx, "access.log")
	if err != nil || is.Inode != 1<<40 || is.Offset != 99999 {
		t.Errorf("IngestState = %+v, %v", is, err)
	}
	if is, err := s.IngestState(ctx, "other"); err != nil || is.Offset != 0 {
		t.Errorf("IngestState(other) = %+v, %v", is, err)
	}

	perf, err := s.SitePerf(ctx, "s1", day)
	if err != nil {
		t.Fatal(err)
	}
	if perf.Totals.PHPRequests != 8 || perf.Totals.CacheHits != 6 || perf.Hist[1] != 4 || perf.Hist[11] != 2 ||
		perf.AvgMS != 100 || len(perf.Series) != 1 {
		t.Errorf("SitePerf = %+v", perf)
	}
	slow, err := s.SlowRequests(ctx, "s1", day, 10)
	if err != nil || len(slow) != 1 || slow[0].Count != 2 || slow[0].MaxMS != 3000 || slow[0].AvgMS != 2250 ||
		slow[0].LastStatus != 504 || slow[0].LastSeen.Unix() != h2 {
		t.Errorf("SlowRequests = %+v, %v", slow, err)
	}
	// An older occurrence arriving late doesn't move last_seen back.
	b3 := &TrafficBatch{Slow: map[SlowKey]*SlowAgg{{"s1", "GET", "/slow"}: {Count: 1, TotalMS: 1000, MaxMS: 1000,
		LastStatus: 200, LastSeen: h1}}}
	s.ApplyTraffic(ctx, b3)
	if slow, _ := s.SlowRequests(ctx, "s1", day, 10); slow[0].LastStatus != 504 || slow[0].MaxMS != 3000 {
		t.Errorf("late occurrence: %+v", slow[0])
	}
}

func TestStoreInsights(t *testing.T) { forEachBackend(t, testInsights) }

func testInsights(t *testing.T, s *Store) {
	ctx := context.Background()
	s.CreateSite(ctx, newSite("s1", "a.test", 19000))
	t0 := time.Unix(1700000000, 0).UTC()
	e := PHPError{Fingerprint: "f1", Level: "Warning", Message: "m", File: "/x.php", Line: 3, Source: "plugin:x",
		Count: 2, FirstSeen: t0, LastSeen: t0.Add(time.Minute)}
	if err := s.RecordPHPErrors(ctx, "s1", []PHPError{e}, IngestState{Name: "s1.err", Offset: 10}); err != nil {
		t.Fatal(err)
	}
	e.Count, e.FirstSeen, e.LastSeen = 3, t0.Add(-time.Hour), t0.Add(time.Hour)
	e2 := PHPError{Fingerprint: "f2", Level: "Fatal", Message: "n", File: "/y.php", Line: 1, Source: "core", Count: 1,
		FirstSeen: t0, LastSeen: t0}
	if err := s.RecordPHPErrors(ctx, "s1", []PHPError{e2, e}, IngestState{Name: "s1.err", Offset: 20}); err != nil {
		t.Fatal(err)
	}
	errs, err := s.PHPErrors(ctx, "s1", t0.Add(-24*time.Hour), 10)
	if err != nil || len(errs) != 2 || errs[0].Fingerprint != "f1" || errs[0].Count != 5 ||
		!errs[0].FirstSeen.Equal(t0.Add(-time.Hour)) || !errs[0].LastSeen.Equal(t0.Add(time.Hour)) {
		t.Errorf("PHPErrors = %+v, %v", errs, err)
	}
	if is, _ := s.IngestState(ctx, "s1.err"); is.Offset != 20 {
		t.Errorf("error log offset %d", is.Offset)
	}
	many := make([]PHPError, maxPHPErrors+20)
	for i := range many {
		many[i] = PHPError{Fingerprint: "m" + strconv.Itoa(i), Count: 1, FirstSeen: t0, LastSeen: t0.Add(time.Duration(i) * time.Second)}
	}
	if err := s.RecordPHPErrors(ctx, "s1", many, IngestState{Name: "s1.err"}); err != nil {
		t.Fatal(err)
	}
	if errs, _ := s.PHPErrors(ctx, "s1", time.Unix(0, 0), 1000); len(errs) != maxPHPErrors {
		t.Errorf("%d kinds of errors kept, want %d", len(errs), maxPHPErrors)
	}
	if err := s.ClearPHPErrors(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	s.ApplyTraffic(ctx, &TrafficBatch{
		Perf: map[HourKey]*PerfCounters{{"s1", t0.Add(-100 * 24 * time.Hour).Unix()}: {PHPRequests: 1},
			{"gone", t0.Unix()}: {PHPRequests: 1}, {"s1", t0.Unix()}: {PHPRequests: 1}},
		Slow: map[SlowKey]*SlowAgg{{"s1", "GET", "/old"}: {Count: 1, LastSeen: t0.Add(-8 * 24 * time.Hour).Unix()}},
	})
	if err := s.PruneInsights(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.SitePerf(ctx, "s1", time.Unix(0, 0)); p.Totals.PHPRequests != 1 {
		t.Errorf("pruned perf left %d", p.Totals.PHPRequests)
	}
	if p, _ := s.SitePerf(ctx, "gone", time.Unix(0, 0)); p.Totals.PHPRequests != 0 {
		t.Error("deleted site's perf kept")
	}
	if sl, _ := s.SlowRequests(ctx, "s1", time.Unix(0, 0), 10); len(sl) != 0 {
		t.Errorf("old slow URL kept: %v", sl)
	}
}

func TestStoreJobsAndOps(t *testing.T) { forEachBackend(t, testJobsAndOps) }

func testJobsAndOps(t *testing.T, s *Store) {
	ctx := context.Background()
	s.CreateSite(ctx, newSite("s1", "a.test", 19000))
	id, err := s.CreateJob(ctx, "s1", "backup", "alice")
	if err != nil || id == 0 {
		t.Fatalf("CreateJob = %d, %v", id, err)
	}
	id2, _ := s.CreateJob(ctx, "", "restore", "bob")
	if id2 <= id {
		t.Errorf("job ids %d, %d", id, id2)
	}
	if err := s.StartJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.JobProgress(ctx, id, 40, "dumping"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, id, JobFailed, "boom", `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	j, err := s.GetJob(ctx, id)
	if err != nil || j.Status != JobFailed || j.Progress != 40 || j.Error != "boom" || j.Result != `{"x":1}` ||
		j.StartedAt.IsZero() || j.FinishedAt.IsZero() || j.Actor != "alice" {
		t.Errorf("GetJob = %+v, %v", j, err)
	}
	if err := s.FinishJob(ctx, id2, JobSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.GetJob(ctx, id2); j.Progress != 100 {
		t.Errorf("succeeded job progress %d", j.Progress)
	}
	id3, _ := s.CreateJob(ctx, "s1", "clone", "alice")
	if jobs, err := s.Jobs(ctx, "s1", false, 10); err != nil || len(jobs) != 2 || jobs[0].ID != id3 {
		t.Errorf("Jobs(s1) = %+v, %v", jobs, err)
	}
	if jobs, _ := s.Jobs(ctx, "", true, 10); len(jobs) != 1 || jobs[0].ID != id3 {
		t.Errorf("Jobs(active) = %+v", jobs)
	}
	if n, err := s.FailInterruptedJobs(ctx); n != 1 || err != nil {
		t.Errorf("FailInterruptedJobs = %d, %v", n, err)
	}
	if _, err := s.GetJob(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetJob(missing) = %v", err)
	}

	// Events, bounded per site.
	for i := range keepEvents + 5 {
		if err := s.AddEvent(ctx, "s1", "scale", "event "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := s.Events(ctx, "s1", 1000)
	if err != nil || len(ev) != keepEvents || ev[0].Message != "event "+strconv.Itoa(keepEvents+4) {
		t.Errorf("Events: %d, %v", len(ev), err)
	}

	// Update runs.
	u, err := s.StartUpdate(ctx, "s1", "manual")
	if err != nil || u == 0 {
		t.Fatalf("StartUpdate = %d, %v", u, err)
	}
	u2, _ := s.StartUpdate(ctx, "s1", "auto")
	if err := s.FinishUpdate(ctx, u, "updated", "3 plugins", `[]`); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.FailInterruptedUpdates(ctx); n != 1 {
		t.Errorf("FailInterruptedUpdates = %d", n)
	}
	runs, err := s.Updates(ctx, "s1", 10)
	if err != nil || len(runs) != 2 || runs[0].ID != u2 || runs[0].Status != "failed" || runs[1].Summary != "3 plugins" ||
		runs[1].Trigger != "manual" {
		t.Errorf("Updates = %+v, %v", runs, err)
	}

	// Scans and settings.
	at := time.Unix(1700000000, 0).UTC()
	if err := s.SaveScan(ctx, "s1", at, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	s.SaveScan(ctx, "s1", at.Add(time.Hour), []byte(`{"ok":false}`))
	if when, r, err := s.Scan(ctx, "s1"); err != nil || string(r) != `{"ok":false}` || !when.Equal(at.Add(time.Hour)) {
		t.Errorf("Scan = %v %s %v", when, r, err)
	}
	if _, _, err := s.Scan(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Scan(none) = %v", err)
	}
	if v, err := s.Setting(ctx, "k"); v != "" || err != nil {
		t.Errorf("Setting(unset) = %q, %v", v, err)
	}
	s.SetSetting(ctx, "k", "v1")
	s.SetSetting(ctx, "k", "v2")
	if v, _ := s.Setting(ctx, "k"); v != "v2" {
		t.Errorf("Setting = %q", v)
	}
}

func TestStoreBackups(t *testing.T) { forEachBackend(t, testBackups) }

func testBackups(t *testing.T, s *Store) {
	ctx := context.Background()
	s.CreateSite(ctx, newSite("s1", "a.test", 19000))
	r := &BackupRepo{ID: "r1", Name: "local", Kind: "local", Location: "/backups", Password: "pw",
		Secrets: RepoSecrets{SSHPublicKey: "ssh-ed25519 AAA", KnownHosts: "host key"}}
	if err := s.CreateRepo(ctx, r); err != nil {
		t.Fatal(err)
	}
	s.CreateRepo(ctx, &BackupRepo{ID: "r2", Name: "offsite", Kind: "s3", Location: "s3:bucket", Password: "pw2"})
	if err := s.CreateRepo(ctx, r); !isUnique(err) {
		t.Errorf("repo created twice: %v", err)
	}
	p := &BackupPolicy{SiteID: "s1", RepoID: "r1", IntervalHours: 24, KeepDaily: 7}
	if err := s.SetBackupPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1700000000, 0).UTC()
	if err := s.BackupAttempted(ctx, "s1", at, ""); err != nil {
		t.Fatal(err)
	}
	s.BackupAttempted(ctx, "s1", at.Add(time.Hour), "failed")
	got, err := s.BackupPolicy(ctx, "s1")
	if err != nil || !got.LastBackupAt.Equal(at) || !got.LastAttemptAt.Equal(at.Add(time.Hour)) || got.LastError != "failed" {
		t.Errorf("BackupPolicy = %+v, %v", got, err)
	}
	// Same repository: history kept; another one: history reset.
	p.KeepDaily = 14
	s.SetBackupPolicy(ctx, p)
	if got, _ := s.BackupPolicy(ctx, "s1"); got.KeepDaily != 14 || !got.LastBackupAt.Equal(at) {
		t.Errorf("same repo: %+v", got)
	}
	p.RepoID = "r2"
	s.SetBackupPolicy(ctx, p)
	if got, _ := s.BackupPolicy(ctx, "s1"); got.RepoID != "r2" || !got.LastBackupAt.IsZero() || got.LastError != "" {
		t.Errorf("new repo: %+v", got)
	}
	if list, err := s.BackupPolicies(ctx); err != nil || len(list) != 1 {
		t.Errorf("BackupPolicies = %v, %v", list, err)
	}
	repo, err := s.GetRepo(ctx, "r2")
	if err != nil || repo.SitesUsing != 1 || repo.Password != "pw2" {
		t.Errorf("GetRepo = %+v, %v", repo, err)
	}
	if repo, _ := s.GetRepo(ctx, "r1"); repo.PublicKey != "ssh-ed25519 AAA" || repo.HostKey != "host key" {
		t.Errorf("secrets: %+v", repo)
	}
	s.RenameRepo(ctx, "r1", "here")
	s.RepoChecked(ctx, "r1", "bad")
	s.RepoPruned(ctx, "r1")
	if repos, err := s.Repos(ctx); err != nil || len(repos) != 2 || repos[0].Name != "here" || repos[0].CheckError != "bad" ||
		repos[0].PrunedAt.IsZero() {
		t.Errorf("Repos = %+v, %v", repos, err)
	}
	if err := s.DeleteRepo(ctx, "r2"); err == nil {
		t.Error("deleted a repository a policy uses")
	}
	if err := s.DeleteBackupPolicy(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRepo(ctx, "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackupPolicy(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BackupPolicy(deleted) = %v", err)
	}
}

func TestStoreMail(t *testing.T) { forEachBackend(t, testMail) }

func testMail(t *testing.T, s *Store) {
	ctx := context.Background()
	if err := s.AddMailDomain(ctx, "a.test"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMailDomain(ctx, "a.test"); !errors.Is(err, ErrExists) {
		t.Errorf("domain added twice: %v", err)
	}
	if ok, _ := s.MailDomainExists(ctx, "a.test"); !ok {
		t.Error("MailDomainExists")
	}
	if err := s.AddMailbox(ctx, Mailbox{Address: "jane@a.test", Domain: "a.test", QuotaMB: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMailbox(ctx, Mailbox{Address: "jane@a.test", Domain: "a.test"}); !errors.Is(err, ErrExists) {
		t.Errorf("mailbox added twice: %v", err)
	}
	if err := s.AddMailbox(ctx, Mailbox{Address: "x@b.test", Domain: "b.test"}); err == nil {
		t.Error("mailbox on an unknown domain")
	}
	if err := s.AddMailAlias(ctx, MailAlias{Alias: "info@a.test", Target: "jane@a.test", Domain: "a.test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMailAlias(ctx, MailAlias{Alias: "info@a.test", Target: "jane@a.test", Domain: "a.test"}); !errors.Is(err, ErrExists) {
		t.Errorf("alias added twice: %v", err)
	}
	if err := s.DeleteMailDomain(ctx, "a.test"); err == nil {
		t.Error("deleted a domain with mailboxes")
	}
	if err := s.SetMailboxQuota(ctx, "jane@a.test", 200); err != nil {
		t.Fatal(err)
	}
	if m, err := s.GetMailbox(ctx, "jane@a.test"); err != nil || m.QuotaMB != 200 {
		t.Errorf("GetMailbox = %+v, %v", m, err)
	}
	if boxes, _ := s.Mailboxes(ctx, "a.test"); len(boxes) != 1 {
		t.Errorf("Mailboxes = %v", boxes)
	}
	if doms, _ := s.MailDomains(ctx); len(doms) != 1 || doms[0].Domain != "a.test" {
		t.Errorf("MailDomains = %v", doms)
	}
	if err := s.DeleteMailbox(ctx, "jane@a.test"); err != nil {
		t.Fatal(err)
	}
	if al, _ := s.MailAliases(ctx); len(al) != 0 {
		t.Errorf("alias to a deleted mailbox kept: %v", al)
	}
	if err := s.DeleteMailbox(ctx, "jane@a.test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteMailbox twice = %v", err)
	}
	s.AddMailAlias(ctx, MailAlias{Alias: "a@a.test", Target: "x@elsewhere.test", Domain: "a.test"})
	if err := s.DeleteMailAlias(ctx, "a@a.test", "x@elsewhere.test"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMailDomain(ctx, "a.test"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCDNAndAccess(t *testing.T) { forEachBackend(t, testCDNAndAccess) }

func testCDNAndAccess(t *testing.T, s *Store) {
	ctx := context.Background()
	s.CreateSite(ctx, newSite("s1", "a.test", 19000))
	c := &CDN{SiteID: "s1", Provider: "cloudflare", APIToken: "tok", Zones: map[string]string{"a.test": "z1"}, EdgeHTML: true}
	if err := s.SetCDN(ctx, c); err != nil {
		t.Fatal(err)
	}
	started := time.Unix(1700000000, 123456789)
	if err := s.RecordCDNPurge(ctx, "s1", started, c.Zones, nil); err != nil {
		t.Fatal(err)
	}
	s.RecordCDNPurge(ctx, "s1", started.Add(time.Hour), c.Zones, errors.New("rate limited"))
	got, err := s.GetCDN(ctx, "s1")
	if err != nil || !got.EdgeHTML || got.Zones["a.test"] != "z1" || !got.PurgedAt.Equal(started) || got.LastError != "rate limited" {
		t.Errorf("GetCDN = %+v, %v", got, err)
	}
	c.Provider, c.AssetHost, c.EdgeHTML = "bunny", "cdn.a.test", false
	s.SetCDN(ctx, c)
	if list, err := s.ListCDN(ctx); err != nil || len(list) != 1 || list[0].Provider != "bunny" || list[0].EdgeHTML {
		t.Errorf("ListCDN = %+v, %v", list, err)
	}
	s.DeleteCDN(ctx, "s1")
	if _, err := s.GetCDN(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetCDN(deleted) = %v", err)
	}

	u := &SFTPUser{Username: "s1", SiteID: "s1", Password: "$6$hash", PublicKeys: []string{"ssh-ed25519 A", "ssh-rsa B"}}
	if err := s.CreateSFTPUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSFTPUser(ctx, u); !isUnique(err) {
		t.Errorf("SFTP user twice: %v", err)
	}
	if got, err := s.GetSFTPUser(ctx, "s1"); err != nil || !got.HasPass || len(got.PublicKeys) != 2 {
		t.Errorf("GetSFTPUser = %+v, %v", got, err)
	}
	s.SetSFTPCredentials(ctx, "s1", "", nil)
	if list, _ := s.SFTPUsers(ctx, "s1"); len(list) != 1 || list[0].HasPass || len(list[0].PublicKeys) != 0 {
		t.Errorf("SFTPUsers = %+v", list)
	}
	if list, _ := s.SFTPUsers(ctx, ""); len(list) != 1 {
		t.Errorf("SFTPUsers(all) = %+v", list)
	}
	if err := s.DeleteSFTPUser(ctx, "s1"); err != nil {
		t.Fatal(err)
	}

	cert := &SiteCert{SiteID: "s1", Names: []string{"a.test", "www.a.test"}, Issuer: "Test CA",
		NotAfter: time.Unix(1800000000, 0), Trusted: true}
	if err := s.SetSiteCert(ctx, cert); err != nil {
		t.Fatal(err)
	}
	cert.Trusted = false
	s.SetSiteCert(ctx, cert)
	certs, err := s.SiteCerts(ctx)
	if err != nil || len(certs) != 1 || certs["s1"].Trusted || !slices.Equal(certs["s1"].Names, cert.Names) ||
		certs["s1"].NotAfter.Unix() != 1800000000 {
		t.Errorf("SiteCerts = %+v, %v", certs, err)
	}
	if err := s.DeleteSiteCert(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SiteCert(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SiteCert(deleted) = %v", err)
	}
}
