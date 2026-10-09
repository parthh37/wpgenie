package site

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/parthh37/wpgenie/internal/backup"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// snapEngine is restic over an in-memory list of snapshots: Forget removes
// them, Prune counts.
type snapEngine struct {
	nilEngine
	mu      sync.Mutex
	snaps   map[string][]backup.Snapshot // repo ID → snapshots
	forgot  []string
	pruned  []string
	backups []string // repo IDs backed up to
}

func (e *snapEngine) Backup(_ context.Context, r *store.BackupRepo, _ backup.BackupInput) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backups = append(e.backups, r.ID)
	return "0123456789abcdef", nil
}

func (e *snapEngine) Snapshots(_ context.Context, r *store.BackupRepo, tags ...string) ([]backup.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []backup.Snapshot
	for _, sn := range e.snaps[r.ID] {
		if !slices.ContainsFunc(tags, func(t string) bool { return !sn.HasTag(t) }) {
			out = append(out, sn)
		}
	}
	return out, nil
}

func (e *snapEngine) Forget(_ context.Context, r *store.BackupRepo, ids []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgot = append(e.forgot, ids...)
	e.snaps[r.ID] = slices.DeleteFunc(e.snaps[r.ID], func(sn backup.Snapshot) bool { return slices.Contains(ids, sn.ID) })
	return nil
}

func (e *snapEngine) Prune(_ context.Context, r *store.BackupRepo) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruned = append(e.pruned, r.ID)
	return nil
}

func snap(id, site, kind string) backup.Snapshot {
	return backup.Snapshot{ID: id, Tags: []string{"wpgenie", "site=" + site, kind}}
}

func addRepo(t *testing.T, h *harness, id, kind string) {
	t.Helper()
	if err := h.svc.Store.CreateRepo(context.Background(), &store.BackupRepo{ID: id, Name: id, Kind: kind,
		Location: "s3:https://x/" + id, Password: "pw"}); err != nil {
		t.Fatal(err)
	}
}

func TestNoBackupsUntilADestinationIsChosen(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.Backups = &snapEngine{snaps: map[string][]backup.Snapshot{}}
	h.svc.Jobs = &jobs.Queue{Store: h.svc.Store, Log: slog.New(slog.DiscardHandler)}
	st, _ := h.svc.Store.GetSite(ctx, "s1")

	// Only this server's disk: nothing is set up, nothing is backed up.
	h.svc.defaultBackupPolicy(ctx, st)
	if p, err := h.svc.Store.BackupPolicy(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a schedule without a choice: %+v %v", p, err)
	}
	if _, err := h.svc.StartBackup(ctx, "s1"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("backed up with no destination: %v", err)
	}
	if _, err := h.svc.StartSearchReplace(ctx, "s1", SearchReplaceInput{Search: "a.test", Replace: "b.test"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("search & replace without a backup or the person's word: %v", err)
	}
	if c, err := h.svc.BackupCoverage(ctx); err != nil || c.WithoutDestination != 1 || c.Preferred != "" {
		t.Fatalf("coverage %+v %v", c, err)
	}

	// Choosing this server's disk is allowed, explicitly.
	if _, err := h.svc.SetBackupPolicy(ctx, "s1", defaultSchedule(LocalRepoID)); err != nil {
		t.Fatal(err)
	}
	if r, err := h.svc.siteRepo(ctx, "s1"); err != nil || r.ID != LocalRepoID {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestS3IsPreferred(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.Backups = &snapEngine{snaps: map[string][]backup.Snapshot{}}
	h.svc.Jobs = &jobs.Queue{Store: h.svc.Store, Log: slog.New(slog.DiscardHandler)}
	h.svc.ensureLocalRepo(ctx)
	addRepo(t, h, "rb2", backup.KindB2)
	addRepo(t, h, "rsftp", backup.KindSFTP)
	if r, _ := h.svc.preferredRepo(ctx); r == nil || r.ID != "rb2" {
		t.Fatalf("without S3, the first off-server kind: %+v", r)
	}
	addRepo(t, h, "rs3", backup.KindS3)
	addRepo(t, h, "rs3b", backup.KindS3)
	h.svc.Store.RepoChecked(ctx, "rs3", "unreachable")
	if r, _ := h.svc.preferredRepo(ctx); r == nil || r.ID != "rs3b" {
		t.Fatalf("a reachable S3 destination first: %+v", r)
	}
	h.svc.Store.RepoChecked(ctx, "rs3", "")
	if r, _ := h.svc.preferredRepo(ctx); r == nil || r.ID != "rs3" {
		t.Fatalf("the oldest S3 destination: %+v", r)
	}

	st, _ := h.svc.Store.GetSite(ctx, "s1")
	h.svc.defaultBackupPolicy(ctx, st)
	if p, err := h.svc.Store.BackupPolicy(ctx, "s1"); err != nil || p.RepoID != "rs3" || p.IntervalHours != 24 {
		t.Fatalf("new site's schedule %+v %v", p, err)
	}
}

func TestAdoptRepo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.Backups = &snapEngine{snaps: map[string][]backup.Snapshot{}}
	for i, id := range []string{"s2", "s3"} {
		if err := h.svc.Store.CreateSite(ctx, &store.Site{ID: id, Name: id, PrimaryDomain: id + ".test", PHPVersion: "8.3",
			FPMPort: 19001 + i, DBName: "wp_" + id, Status: store.StatusActive, MemoryMB: 512, CPUs: 1, Replicas: 1,
			ParentID: map[string]string{"s3": "s1"}[id]}); err != nil {
			t.Fatal(err)
		}
	}
	// s2 chose this server's disk, weekly; s3 is a staging copy (stays
	// as it is); s1 has nothing.
	if _, err := h.svc.SetBackupPolicy(ctx, "s2", PolicyInput{RepoID: LocalRepoID, IntervalHours: 168, KeepWeekly: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AdoptRepo(ctx, LocalRepoID, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("this server's disk took sites over: %v", err)
	}
	addRepo(t, h, "rs3", backup.KindS3)
	res, err := h.svc.AdoptRepo(ctx, "rs3", false)
	if err != nil || !slices.Equal(res.Started, []string{"s1"}) || len(res.Moved) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	res, err = h.svc.AdoptRepo(ctx, "rs3", true)
	if err != nil || len(res.Started) != 0 || !slices.Equal(res.Moved, []string{"s2"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if p, _ := h.svc.Store.BackupPolicy(ctx, "s2"); p.RepoID != "rs3" || p.IntervalHours != 168 || p.KeepWeekly != 3 {
		t.Fatalf("moved without its schedule: %+v", p)
	}
	if _, err := h.svc.Store.BackupPolicy(ctx, "s3"); !errors.Is(err, store.ErrNotFound) {
		t.Error("a staging copy got a schedule")
	}
}

func TestBackupCleanup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	e := &snapEngine{snaps: map[string][]backup.Snapshot{
		"rs3": {snap("a1", "s1", BackupManual), snap("a2", "s1", BackupScheduled), snap("a3", "s1", BackupSafety),
			snap("b1", "gone", BackupScheduled)},
		"rb2": {snap("c1", "s1", BackupManual)},
	}}
	h.svc.Backups = e
	h.svc.Jobs = &jobs.Queue{Store: h.svc.Store, Log: slog.New(slog.DiscardHandler)}
	addRepo(t, h, "rs3", backup.KindS3)
	addRepo(t, h, "rb2", backup.KindB2)
	wait := func(id int64, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if j, err := h.svc.Jobs.WaitJob(ctx, id); err != nil || j.Status != store.JobSucceeded {
			t.Fatalf("job %+v %v", j, err)
		}
	}

	if _, err := h.svc.StartSiteCleanup(ctx, "s1", BackupCleanupInput{Kinds: []string{"daily"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown kind: %v", err)
	}
	// Only the manual ones, everywhere.
	wait(h.svc.StartSiteCleanup(ctx, "s1", BackupCleanupInput{Kinds: []string{BackupManual}}))
	slices.Sort(e.forgot)
	if !slices.Equal(e.forgot, []string{"a1", "c1"}) || len(e.pruned) != 2 {
		t.Fatalf("forgot %v, pruned %v", e.forgot, e.pruned)
	}
	// Everything of the site in one destination: another site's stay.
	e.forgot, e.pruned = nil, nil
	wait(h.svc.StartSiteCleanup(ctx, "s1", BackupCleanupInput{RepoID: "rs3"}))
	slices.Sort(e.forgot)
	if !slices.Equal(e.forgot, []string{"a2", "a3"}) || !slices.Equal(e.pruned, []string{"rs3"}) {
		t.Fatalf("forgot %v, pruned %v", e.forgot, e.pruned)
	}
	// The whole destination, deleted sites' backups included.
	e.forgot, e.pruned = nil, nil
	wait(h.svc.StartRepoCleanup(ctx, "rs3", BackupCleanupInput{}))
	if !slices.Equal(e.forgot, []string{"b1"}) || len(e.snaps["rs3"]) != 0 {
		t.Fatalf("forgot %v, left %v", e.forgot, e.snaps["rs3"])
	}
	// Nothing left to delete: no prune.
	e.pruned = nil
	wait(h.svc.StartRepoCleanup(ctx, "rs3", BackupCleanupInput{}))
	if len(e.pruned) != 0 {
		t.Fatalf("pruned an untouched destination: %v", e.pruned)
	}
}
