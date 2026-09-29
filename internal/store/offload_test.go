package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestOffloadRecords(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSite(ctx, &Site{ID: "s1", Name: "s1", PrimaryDomain: "a.test", PHPVersion: "8.3", FPMPort: 19000,
		DBName: "wp_s1", Status: StatusActive, MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	o := &Offload{SiteID: "s1", Endpoint: "https://s3.example.com", Bucket: "media", Prefix: "s1/uploads/",
		AccessKeyID: "AKIA", SecretKey: "secret", PublicURL: "https://media.example.com", LocalDays: 7}
	if err := st.SetOffload(ctx, o, true); err != nil {
		t.Fatal(err)
	}
	full := time.Unix(1_800_000_000, 0)
	st.RecordOffloadRun(ctx, "s1", OffloadRun{Full: true, Started: full, Objects: 3, Bytes: 300, Deleted: 1})
	st.RecordOffloadRun(ctx, "s1", OffloadRun{Started: full.Add(time.Minute), Objects: 1, Bytes: 10})
	st.RecordOffloadRun(ctx, "s1", OffloadRun{Started: full.Add(2 * time.Minute), Err: errors.New("503 SlowDown")})
	st.RecordOffloadCleanup(ctx, "s1", full.Add(time.Hour), 2, 200)
	got, err := st.GetOffload(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.FullAt.Equal(full) || !got.IncrementalAt.Equal(full.Add(time.Minute)) || !got.AttemptAt.Equal(full.Add(2*time.Minute)) ||
		got.Failures != 1 || got.LastError != "503 SlowDown" || got.TotalObjects != 4 || got.TotalBytes != 310 ||
		got.LastObjects != 1 || got.TotalDeleted != 1 || got.RemovedLocal != 2 || got.RemovedBytes != 200 || got.LocalDays != 7 {
		t.Errorf("%+v", got)
	}
	// A settings change keeps the history; a new location forgets it.
	o.LocalDays = 0
	st.SetOffload(ctx, o, false)
	if got, _ := st.GetOffload(ctx, "s1"); got.FullAt.IsZero() || got.LocalDays != 0 {
		t.Errorf("history lost on a settings change: %+v", got)
	}

	if refused, err := st.QueueOffloadDeletes(ctx, "s1", []string{"a.jpg", "b.jpg", "a.jpg", "c.jpg"}, 2); err != nil || refused != 1 {
		t.Fatalf("queue: %d %v", refused, err)
	}
	pending, _ := st.PendingOffloadDeletes(ctx, "s1", 10)
	if !slices.Equal(pending, []string{"a.jpg", "b.jpg"}) {
		t.Fatalf("pending %v", pending)
	}
	st.DoneOffloadDeletes(ctx, "s1", []string{"a.jpg"})
	if n, _ := st.CountOffloadDeletes(ctx, "s1"); n != 1 {
		t.Errorf("%d pending", n)
	}
	st.SetOffload(ctx, o, true)
	if n, _ := st.CountOffloadDeletes(ctx, "s1"); n != 0 {
		t.Error("deletes for the old location kept")
	}
	if got, _ := st.GetOffload(ctx, "s1"); !got.FullAt.IsZero() || got.TotalObjects != 0 {
		t.Errorf("history kept for a new location: %+v", got)
	}

	// Deleting the site takes its settings (and secret) along.
	st.QueueOffloadDeletes(ctx, "s1", []string{"x.jpg"}, 10)
	if err := st.DeleteSite(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetOffload(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("settings outlived the site: %v", err)
	}
	if n, _ := st.CountOffloadDeletes(ctx, "s1"); n != 0 {
		t.Error("queued deletes outlived the site")
	}
}
