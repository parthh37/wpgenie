package site

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/parthh37/wpgenie/internal/store"
)

func TestSuspendAndUnsuspend(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var hooks []bool
	h.svc.SiteSuspended = func(_ context.Context, id string, suspended bool) { hooks = append(hooks, suspended) }

	if err := h.svc.Suspend(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	if st.Status != store.StatusSuspended {
		t.Fatalf("status %s", st.Status)
	}
	if len(h.rt.containers) != 0 {
		t.Fatalf("replicas still running: %v", h.names())
	}
	if len(st.Upstreams) != 0 {
		t.Fatalf("ports kept: %v", st.Upstreams)
	}
	last := h.proxy.last
	if len(last) != 1 || !last[0].Suspended || len(last[0].Upstreams) != 0 || last[0].Domains[0] != "a.test" {
		t.Fatalf("proxy sites %+v", last)
	}
	// Idempotent, and everything that needs an active site refuses.
	if err := h.svc.Suspend(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Scale(ctx, "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 2}); err == nil {
		t.Fatal("scaled a suspended site")
	}

	if err := h.svc.Unsuspend(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	st, _ = h.svc.Store.GetSite(ctx, "s1")
	if st.Status != store.StatusActive || len(st.Upstreams) != 1 || len(h.rt.containers) != 1 {
		t.Fatalf("after unsuspend: %s %v %v", st.Status, st.Upstreams, h.names())
	}
	last = h.proxy.last
	if len(last) != 1 || last[0].Suspended || len(last[0].Upstreams) != 1 {
		t.Fatalf("proxy sites %+v", last)
	}
	if err := h.svc.Unsuspend(ctx, "s1"); err != nil { // a no-op now
		t.Fatal(err)
	}
	if len(hooks) != 2 || !hooks[0] || hooks[1] {
		t.Fatalf("hooks %v", hooks)
	}
}

// A failed proxy switch leaves the site suspended with nothing running.
func TestUnsuspendRollsBackWhenTheProxyRefuses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Suspend(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	for _, rejects := range []int{1, 2} {
		h.proxy.rejects = rejects
		if err := h.svc.Unsuspend(ctx, "s1"); err == nil {
			t.Fatal("unsuspend succeeded")
		}
		st, _ := h.svc.Store.GetSite(ctx, "s1")
		if st.Status != store.StatusSuspended || len(h.rt.containers) != 0 {
			t.Fatalf("rejects %d: status %s, running %v", rejects, st.Status, h.names())
		}
	}
	h.proxy.rejects = 0
	if err := h.svc.Unsuspend(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
}

// The disk walk counts regular files only and never follows a symlink,
// whether it points into another site or at a huge directory.
func TestDirSizeDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	site, other := filepath.Join(dir, "s1"), filepath.Join(dir, "s2")
	for _, d := range []string{filepath.Join(site, "public", "wp-content"), other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(site, "wp-config.php"), make([]byte, 100), 0o644)
	os.WriteFile(filepath.Join(site, "public", "wp-content", "a.jpg"), make([]byte, 1000), 0o644)
	os.WriteFile(filepath.Join(other, "big.sql"), make([]byte, 1<<20), 0o644)
	os.Symlink(other, filepath.Join(site, "public", "other-site"))
	os.Symlink(filepath.Join(other, "big.sql"), filepath.Join(site, "public", "dump.sql"))
	os.Symlink("/", filepath.Join(site, "public", "root"))
	n, err := dirSize(context.Background(), site)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1100 {
		t.Fatalf("size %d, want 1100", n)
	}
	if n, err := dirSize(context.Background(), filepath.Join(dir, "missing")); err != nil || n != 0 {
		t.Fatalf("missing dir: %d %v", n, err)
	}
}
