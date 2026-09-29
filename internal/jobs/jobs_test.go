package jobs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
)

func newQueue(t *testing.T, heavy int) *Queue {
	t.Helper()
	st := storetest.Open(t)
	return &Queue{Store: st, Log: slog.New(slog.DiscardHandler), Heavy: heavy}
}

func wait(t *testing.T, q *Queue, id int64) *store.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	j, err := q.WaitJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestJobRecordsProgressResultAndActor(t *testing.T) {
	q := newQueue(t, 1)
	ctx := WithActor(context.Background(), "alice")
	id, err := q.Submit(ctx, Spec{SiteID: "s1", Kind: "backup"}, func(ctx context.Context, t *Task) error {
		t.Progress(40, "dumping the database")
		t.Progress(10, "going backwards is ignored")
		t.SetResult(map[string]string{"snapshot": "abc"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	j := wait(t, q, id)
	if j.Status != store.JobSucceeded || j.Progress != 100 || j.Actor != "alice" || j.Result != `{"snapshot":"abc"}` {
		t.Fatalf("job %+v", j)
	}
	if j.Step != "going backwards is ignored" {
		t.Errorf("step %q", j.Step)
	}
}

func TestFailedJobKeepsProgressAndError(t *testing.T) {
	q := newQueue(t, 1)
	id, _ := q.Submit(context.Background(), Spec{Kind: "restore"}, func(ctx context.Context, t *Task) error {
		t.Progress(60, "restoring files")
		return errors.New("disk full")
	})
	j := wait(t, q, id)
	if j.Status != store.JobFailed || j.Error != "disk full" || j.Progress != 60 {
		t.Fatalf("job %+v", j)
	}
	id, _ = q.Submit(context.Background(), Spec{Kind: "x"}, func(context.Context, *Task) error { panic("boom") })
	if j := wait(t, q, id); j.Status != store.JobFailed || j.Error != "internal error: boom" {
		t.Fatalf("panicking job %+v", j)
	}
}

func TestJobsWaitForTheirLock(t *testing.T) {
	q := newQueue(t, 4)
	var mu sync.Mutex
	mu.Lock() // e.g. an update is running on the site
	ran := make(chan struct{})
	id, _ := q.Submit(context.Background(), Spec{Kind: "backup", Lock: LockFunc(&mu)}, func(context.Context, *Task) error {
		close(ran)
		return nil
	})
	time.Sleep(600 * time.Millisecond)
	if j, _ := q.Store.GetJob(context.Background(), id); j.Status != store.JobQueued {
		t.Fatalf("status %s while the lock is held, want queued", j.Status)
	}
	mu.Unlock()
	<-ran
	if j := wait(t, q, id); j.Status != store.JobSucceeded {
		t.Fatalf("job %+v", j)
	}
	if !mu.TryLock() {
		t.Fatal("job didn't release the lock")
	}
}

func TestHeavyJobsShareSlots(t *testing.T) {
	q := newQueue(t, 2)
	var running, peak atomic.Int32
	var ids []int64
	for range 6 {
		id, _ := q.Submit(context.Background(), Spec{Kind: "backup", Heavy: true}, func(context.Context, *Task) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			running.Add(-1)
			return nil
		})
		ids = append(ids, id)
	}
	for _, id := range ids {
		wait(t, q, id)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak.Load())
	}
}

func TestSecretsOnlyForTheirActor(t *testing.T) {
	q := newQueue(t, 1)
	ctx := WithOwner(WithActor(context.Background(), "alice"), "user:1")
	id, _ := q.Submit(ctx, Spec{Kind: "create"}, func(ctx context.Context, t *Task) error {
		t.SetSecret("hunter2", OwnerFrom(ctx))
		return nil
	})
	wait(t, q, id)
	for _, other := range []string{"user:2", "alice", ""} { // a name is not an owner
		if _, ok := q.Secret(id, other); ok {
			t.Fatalf("%q got the secret", other)
		}
	}
	if v, ok := q.Secret(id, "user:1"); !ok || v != "hunter2" {
		t.Fatalf("secret %v %v", v, ok)
	}
	q.DropSecret(id, "user:1")
	if _, ok := q.Secret(id, "user:1"); ok {
		t.Fatal("secret survived being dismissed")
	}
	if j, _ := q.Store.GetJob(context.Background(), id); j.Result != "" {
		t.Fatalf("secret leaked into the stored result: %q", j.Result)
	}
}

func TestInterruptedJobsFailAtStartup(t *testing.T) {
	q := newQueue(t, 1)
	ctx := context.Background()
	id, _ := q.Store.CreateJob(ctx, "s1", "restore", "alice")
	q.Store.StartJob(ctx, id)
	if n, err := q.Store.FailInterruptedJobs(ctx); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if j, _ := q.Store.GetJob(ctx, id); j.Status != store.JobFailed || j.Error == "" {
		t.Fatalf("job %+v", j)
	}
}

// Two jobs locking the same two sites in opposite orders, with a single
// heavy slot: neither may hold one lock while waiting for the other.
func TestLockFuncTakesAllOrNothing(t *testing.T) {
	q := newQueue(t, 1)
	var a, b sync.Mutex
	b.Lock() // something else holds b for a moment
	var ids []int64
	for _, order := range [][]*sync.Mutex{{&a, &b}, {&b, &a}} {
		id, _ := q.Submit(context.Background(), Spec{Kind: "push", Heavy: true, Lock: LockFunc(order...)},
			func(context.Context, *Task) error { return nil })
		ids = append(ids, id)
	}
	time.Sleep(400 * time.Millisecond)
	if !a.TryLock() {
		t.Fatal("a job holds a while waiting for b")
	}
	a.Unlock()
	b.Unlock()
	for _, id := range ids {
		if j := wait(t, q, id); j.Status != store.JobSucceeded {
			t.Fatalf("job %+v", j)
		}
	}
}
