// Package jobs runs long operations (creating a site, backups, restores,
// clones) in the background and records their progress, so the panel can
// show it and an API call never has to stay open for minutes.
//
// A job waits (status "queued") until it holds its lock, typically the
// site's maintenance lock, so jobs on one site run one after another and
// never overlap updates or scans. Heavy jobs (copying a whole site) also
// share a small concurrency limit: ten scheduled backups starting at the
// same minute take turns instead of saturating the disk.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Spec describes a job to run.
type Spec struct {
	SiteID string
	Kind   string
	// Heavy jobs take a slot of Queue.Heavy while they run.
	Heavy bool
	// Timeout bounds the run once it has started (default 2 hours).
	Timeout time.Duration
	// Lock, if set, is taken before the job starts and released when it
	// ends; until then the job is queued. It must return when ctx ends.
	Lock func(ctx context.Context) (unlock func(), err error)
}

// Task is a running job, as its function sees it.
type Task struct {
	ID     int64
	q      *Queue
	mu     sync.Mutex
	pct    int
	step   string
	wrote  time.Time
	result any
}

// Progress reports how far the job is (0-100) and what it is doing. Writes
// to the store are throttled; a new step is always written.
func (t *Task) Progress(pct int, step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pct = min(max(pct, t.pct), 99) // never backwards; 100 is "finished"
	if pct == t.pct && step == t.step {
		return
	}
	changed := step != t.step
	t.pct, t.step = pct, step
	if !changed && time.Since(t.wrote) < time.Second {
		return
	}
	t.wrote = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.q.Store.JobProgress(ctx, t.ID, pct, step); err != nil {
		t.q.Log.Warn("recording job progress", "job", t.ID, "err", err)
	}
}

// Stepper returns a progress function for a sub-task that maps its own
// 0-100 onto from-to of the job.
func (t *Task) Stepper(from, to int, step string) func(pct int) {
	return func(pct int) { t.Progress(from+(to-from)*min(max(pct, 0), 100)/100, step) }
}

// SetResult stores v (JSON) with the job when it finishes. Never secrets:
// results are shown to every panel user.
func (t *Task) SetResult(v any) {
	t.mu.Lock()
	t.result = v
	t.mu.Unlock()
}

// SetSecret keeps v in memory for the owner who started the job (see
// WithOwner; e.g. a new site's admin password): never written to disk,
// gone after secretTTL or when they dismiss it. Without an owner nobody
// can read it.
func (t *Task) SetSecret(v any, owner string) {
	if owner == "" {
		return
	}
	t.q.secrets.Store(t.ID, &secret{value: v, owner: owner, expires: time.Now().Add(secretTTL)})
}

type secret struct {
	value   any
	owner   string
	expires time.Time
}

const secretTTL = 30 * time.Minute

type Queue struct {
	Store *store.Store
	Log   *slog.Logger
	// Heavy is how many heavy jobs may run at the same time (default 2).
	Heavy int

	once    sync.Once
	sem     chan struct{}
	wg      sync.WaitGroup
	secrets sync.Map // job ID -> *secret
	done    sync.Map // job ID -> chan struct{}, closed when it ends
}

func (q *Queue) init() {
	q.once.Do(func() {
		n := q.Heavy
		if n < 1 {
			n = 2
		}
		q.sem = make(chan struct{}, n)
	})
}

// ErrNoJob means a job isn't known (or has been cleaned up).
var ErrNoJob = errors.New("no such job")

type ctxKey struct{}
type ownerKey struct{}

// WithOwner records a stable identity (a user ID, not a name that can be
// re-created) that secret results are bound to.
func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

// OwnerFrom returns the identity recorded with WithOwner, or "".
func OwnerFrom(ctx context.Context) string {
	o, _ := ctx.Value(ownerKey{}).(string)
	return o
}

// WithActor records who asked for the work (a panel user, "api-token",
// "scheduler") so the jobs it starts are attributed to them.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, ctxKey{}, actor)
}

// ActorFrom returns the actor recorded with WithActor, or "system".
func ActorFrom(ctx context.Context) string {
	if a, ok := ctx.Value(ctxKey{}).(string); ok && a != "" {
		return a
	}
	return "system"
}

// Submit records a job and runs fn in the background. The job keeps
// running if ctx (typically an API request) ends: only the job's own
// timeout stops it.
func (q *Queue) Submit(ctx context.Context, spec Spec, fn func(ctx context.Context, t *Task) error) (int64, error) {
	q.init()
	q.secrets.Range(func(k, v any) bool { // drop expired secrets nobody fetched
		if time.Now().After(v.(*secret).expires) {
			q.secrets.Delete(k)
		}
		return true
	})
	id, err := q.Store.CreateJob(ctx, spec.SiteID, spec.Kind, ActorFrom(ctx))
	if err != nil {
		return 0, err
	}
	done := make(chan struct{})
	q.done.Store(id, done)
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		defer close(done)
		q.run(context.WithoutCancel(ctx), id, spec, fn)
	}()
	return id, nil
}

func (q *Queue) run(ctx context.Context, id int64, spec Spec, fn func(context.Context, *Task) error) {
	t := &Task{ID: id, q: q}
	status, msg := store.JobSucceeded, ""
	defer func() {
		if r := recover(); r != nil {
			q.Log.Error("job panicked", "job", id, "kind", spec.Kind, "panic", r)
			status, msg = store.JobFailed, fmt.Sprintf("internal error: %v", r)
		}
		q.finish(id, spec, status, msg, t)
	}()
	// Waiting for the lock and a slot is bounded too: a job queued behind a
	// stuck one must not wait forever.
	wctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	if spec.Lock != nil {
		unlock, err := spec.Lock(wctx)
		if err != nil {
			status, msg = store.JobFailed, "never started: "+err.Error()
			return
		}
		defer unlock()
	}
	if spec.Heavy {
		select {
		case q.sem <- struct{}{}:
			defer func() { <-q.sem }()
		case <-wctx.Done():
			status, msg = store.JobFailed, "never started: timed out waiting for other jobs to finish"
			return
		}
	}
	if err := q.Store.StartJob(ctx, id); err != nil {
		status, msg = store.JobFailed, err.Error()
		return
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 2 * time.Hour
	}
	rctx, rcancel := context.WithTimeout(ctx, timeout)
	defer rcancel()
	if err := fn(rctx, t); err != nil {
		status, msg = store.JobFailed, err.Error()
	}
}

func (q *Queue) finish(id int64, spec Spec, status, msg string, t *Task) {
	if len(msg) > 4000 {
		msg = msg[:4000] + "…"
	}
	var result string
	t.mu.Lock()
	if t.result != nil {
		if b, err := json.Marshal(t.result); err == nil {
			result = string(b)
		}
	}
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := q.Store.FinishJob(ctx, id, status, msg, result); err != nil {
		q.Log.Error("recording job result", "job", id, "err", err)
	}
	lvl := slog.LevelInfo
	if status != store.JobSucceeded {
		lvl = slog.LevelWarn
	}
	q.Log.Log(ctx, lvl, "job finished", "job", id, "kind", spec.Kind, "site", spec.SiteID, "status", status, "err", msg)
	// Keep the done channel briefly for late waiters, then drop it.
	time.AfterFunc(time.Minute, func() { q.done.Delete(id) })
}

// WaitJob waits for a job to end and returns its final record.
func (q *Queue) WaitJob(ctx context.Context, id int64) (*store.Job, error) {
	if d, ok := q.done.Load(id); ok {
		select {
		case <-d.(chan struct{}):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return q.Store.GetJob(ctx, id)
}

// Secret returns a job's secret result if owner started it.
func (q *Queue) Secret(id int64, owner string) (any, bool) {
	v, ok := q.secrets.Load(id)
	if !ok {
		return nil, false
	}
	s := v.(*secret)
	if time.Now().After(s.expires) {
		q.secrets.Delete(id)
		return nil, false
	}
	if owner == "" || s.owner != owner {
		return nil, false
	}
	return s.value, true
}

// DropSecret forgets a job's secret result (the user has saved it).
func (q *Queue) DropSecret(id int64, owner string) {
	if _, ok := q.Secret(id, owner); ok {
		q.secrets.Delete(id)
	}
}

// Wait waits up to timeout for running jobs, so a daemon stop doesn't cut
// a restore in half.
func (q *Queue) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// LockFunc adapts mutexes to Spec.Lock, polling so waiting ends with ctx.
// With several (a staging site and its live site), they are taken all at
// once or not at all: a job never holds one while it waits for another,
// so two jobs wanting the same pair can't deadlock.
func LockFunc(mus ...*sync.Mutex) func(ctx context.Context) (func(), error) {
	return func(ctx context.Context) (func(), error) {
		for {
			if unlock, ok := tryAll(mus); ok {
				return unlock, nil
			}
			select {
			case <-ctx.Done():
				return nil, errors.New("timed out waiting for another operation on this site to finish")
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

func tryAll(mus []*sync.Mutex) (func(), bool) {
	for i, mu := range mus {
		if !mu.TryLock() {
			for _, held := range mus[:i] {
				held.Unlock()
			}
			return nil, false
		}
	}
	return func() {
		for _, mu := range mus {
			mu.Unlock()
		}
	}, true
}
