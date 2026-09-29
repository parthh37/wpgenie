package store

// Read-modify-write sequences that SQLite's single connection made atomic
// must stay atomic on PostgreSQL, where transactions really run at once.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/axiomhq/hyperloglog"
)

const racers = 8

// race runs fn on racers goroutines released together. On PostgreSQL the
// pool first gets a connection per goroutine: opening connections would
// otherwise stagger them enough to hide races.
func race(s *Store, fn func(i int)) {
	if s.db.postgres {
		s.db.sql.SetMaxIdleConns(racers)
		var wg sync.WaitGroup
		for range racers {
			wg.Go(func() { s.db.sql.Exec(`SELECT pg_sleep(0.05)`) })
		}
		wg.Wait()
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			<-start
			fn(i)
		})
	}
	close(start)
	wg.Wait()
}

func TestConcurrentSecondFactors(t *testing.T) { forEachBackend(t, testConcurrentSecondFactors) }

func testConcurrentSecondFactors(t *testing.T, s *Store) {
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "alice", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	codes := make([]string, racers)
	for i := range codes {
		codes[i] = "code" + strconv.Itoa(i)
	}
	if err := s.EnableTOTP(ctx, u.ID, "SECRET", 1, codes); err != nil {
		t.Fatal(err)
	}
	// The same TOTP code, replayed at once: accepted once.
	var won atomic.Int32
	race(s, func(int) {
		if ok, err := s.UseTOTPStep(ctx, u.ID, 2); err != nil {
			t.Error(err)
		} else if ok {
			won.Add(1)
		}
	})
	if won.Load() != 1 {
		t.Errorf("TOTP step accepted %d times", won.Load())
	}
	// The same recovery code at once: accepted once.
	won.Store(0)
	race(s, func(int) {
		if ok, err := s.UseRecoveryCode(ctx, u.ID, "code0"); err != nil {
			t.Error(err)
		} else if ok {
			won.Add(1)
		}
	})
	if won.Load() != 1 {
		t.Errorf("recovery code accepted %d times", won.Load())
	}
	// Different codes at once: each works, none is lost or resurrected.
	race(s, func(i int) {
		if i == 0 {
			return
		}
		if ok, err := s.UseRecoveryCode(ctx, u.ID, codes[i]); !ok || err != nil {
			t.Errorf("code %d: %v, %v", i, ok, err)
		}
	})
	if u, _ := s.GetUser(ctx, u.ID); u.RecoveryLeft != 0 {
		t.Errorf("%d recovery codes left: %v", u.RecoveryLeft, u.RecoveryHashes)
	}
}

func TestConcurrentFirstUser(t *testing.T) { forEachBackend(t, testConcurrentFirstUser) }

func testConcurrentFirstUser(t *testing.T, s *Store) {
	ctx := context.Background()
	for round := range 10 {
		var created atomic.Int32
		race(s, func(i int) {
			_, err := s.CreateFirstUser(ctx, "admin"+strconv.Itoa(i), "h", "admin")
			switch {
			case err == nil:
				created.Add(1)
			case !errors.Is(err, ErrExists):
				t.Error(err)
			}
		})
		if n, _ := s.CountUsers(ctx); created.Load() != 1 || n != 1 {
			t.Fatalf("round %d: %d first users created, %d users", round, created.Load(), n)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM users`); err != nil {
			t.Fatal(err)
		}
	}
}

// Batches from several ingesters (nodes) for the same site and hours.
func TestConcurrentTraffic(t *testing.T) { forEachBackend(t, testConcurrentTraffic) }

func testConcurrentTraffic(t *testing.T, s *Store) {
	ctx := context.Background()
	const day, h1, h2 = 1788220800, 1788220800 + 3600, 1788220800 + 7200
	race(s, func(i int) {
		sk := hyperloglog.New16()
		for v := range 50 {
			sk.Insert([]byte("visitor-" + strconv.Itoa(i) + "-" + strconv.Itoa(v)))
		}
		var h Histogram
		h[0] = 1
		b := &TrafficBatch{
			Hourly:   map[HourKey]*Counters{{"s1", h1}: {Requests: 1}, {"s1", h2}: {Requests: 2}},
			Visitors: map[DayKey]*hyperloglog.Sketch{{"s1", day}: sk},
			Perf:     map[HourKey]*PerfCounters{{"s1", h1}: {PHPRequests: 1, Hist: h}, {"s1", h2}: {PHPRequests: 1, Hist: h}},
			Slow:     map[SlowKey]*SlowAgg{{"s1", "GET", "/x"}: {Count: 1, TotalMS: 1000, MaxMS: int64(1000 + i), LastSeen: h1}},
			State:    IngestState{Name: "node" + strconv.Itoa(i), Offset: int64(i)},
		}
		if err := s.ApplyTraffic(ctx, b); err != nil {
			t.Error(err)
		}
	})
	st, err := s.SiteStats(ctx, "s1", unixTime(day))
	if err != nil {
		t.Fatal(err)
	}
	// HyperLogLog: 400 distinct visitors, ~1% error at this size.
	if st.Totals.Requests != 3*racers || st.UniqueVisitors < 390 || st.UniqueVisitors > 410 {
		t.Errorf("requests %d (want %d), visitors %d (want ~%d)", st.Totals.Requests, 3*racers, st.UniqueVisitors, 50*racers)
	}
	perf, _ := s.SitePerf(ctx, "s1", unixTime(day))
	if perf.Totals.PHPRequests != 2*racers || perf.Hist[0] != 2*racers {
		t.Errorf("perf %d requests, histogram %v", perf.Totals.PHPRequests, perf.Hist)
	}
	slow, _ := s.SlowRequests(ctx, "s1", unixTime(day), 10)
	if len(slow) != 1 || slow[0].Count != racers || slow[0].MaxMS != 1000+racers-1 {
		t.Errorf("slow %+v", slow)
	}

	// Sketches and histograms alone (above, the hourly rows' locks happen
	// to serialise the batches first): each merge must see the others.
	const day2, h3 = day + 86400, day + 86400 + 3600
	for round := range 3 {
		race(s, func(i int) {
			sk := hyperloglog.New16()
			for v := range 50 {
				sk.Insert([]byte("r" + strconv.Itoa(round) + "-" + strconv.Itoa(i) + "-" + strconv.Itoa(v)))
			}
			var h Histogram
			h[1] = 1
			if err := s.ApplyTraffic(ctx, &TrafficBatch{Visitors: map[DayKey]*hyperloglog.Sketch{{"s2", day2}: sk}}); err != nil {
				t.Error(err)
			}
			if err := s.ApplyTraffic(ctx, &TrafficBatch{Perf: map[HourKey]*PerfCounters{{"s2", h3}: {PHPRequests: 1, Hist: h}}}); err != nil {
				t.Error(err)
			}
		})
	}
	st, _ = s.SiteStats(ctx, "s2", unixTime(day2))
	if want := uint64(3 * 50 * racers); st.UniqueVisitors < want*97/100 || st.UniqueVisitors > want*103/100 {
		t.Errorf("visitors %d, want ~%d: concurrent merges lost some", st.UniqueVisitors, want)
	}
	perf, _ = s.SitePerf(ctx, "s2", unixTime(day2))
	if perf.Totals.PHPRequests != 3*racers || perf.Hist[1] != 3*racers {
		t.Errorf("perf %d requests, histogram %v: concurrent merges lost some", perf.Totals.PHPRequests, perf.Hist)
	}
}

func TestConcurrentUpstreams(t *testing.T) { forEachBackend(t, testConcurrentUpstreams) }

func testConcurrentUpstreams(t *testing.T, s *Store) {
	ctx := context.Background()
	if err := s.CreateSite(ctx, newSite("s1", "a.test", 19000)); err != nil {
		t.Fatal(err)
	}
	sets := make([][]int, racers)
	for i := range sets {
		sets[i] = []int{20000 + 10*i, 20001 + 10*i}
	}
	race(s, func(i int) {
		if err := s.SetUpstreams(ctx, "s1", sets[i]); err != nil {
			t.Error(err)
		}
	})
	got, _ := s.GetSite(ctx, "s1")
	if !slices.ContainsFunc(sets, func(set []int) bool { return slices.Equal(set, got.Upstreams) }) {
		t.Errorf("upstreams %v: a mix of concurrent replacements", got.Upstreams)
	}
}

func TestConcurrentJobsAndEvents(t *testing.T) { forEachBackend(t, testConcurrentJobsAndEvents) }

func testConcurrentJobsAndEvents(t *testing.T, s *Store) {
	ctx := context.Background()
	s.CreateSite(ctx, newSite("s1", "a.test", 19000))
	ids := make([]int64, racers)
	race(s, func(i int) {
		id, err := s.CreateJob(ctx, "s1", "backup", "system")
		if err != nil {
			t.Error(err)
		}
		ids[i] = id
		if err := s.AddEvent(ctx, "s1", "scale", "e"); err != nil {
			t.Error(err)
		}
		if err := s.AddAudit(ctx, AuditEntry{Actor: "a", Action: "x"}); err != nil {
			t.Error(err)
		}
	})
	slices.Sort(ids)
	if len(slices.Compact(ids)) != racers || ids[0] == 0 {
		t.Errorf("job ids %v", ids)
	}
	if ev, _ := s.Events(ctx, "s1", 100); len(ev) != racers {
		t.Errorf("%d events", len(ev))
	}
}
