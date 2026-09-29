package site

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/offload"
	"github.com/parthh37/wpgenie/internal/offload/offloadtest"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// TestOffloadAgainstMinIO runs uploads offload with real rclone against a
// real MinIO: the end-to-end check on enabling, full and incremental
// copies, deletes from the queue, local copies removed after local_days and
// copied back. Needs Docker: WPGENIE_TEST_DOCKER=1.
func TestOffloadAgainstMinIO(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run rclone against MinIO via Docker")
	}
	m := offloadtest.Start(t, "s1/uploads/")
	h := newHarness(t)
	h.svc.Offload = &offload.Rclone{Docker: &runtime.Docker{}, Image: h.svc.Cfg.RcloneImage, Network: m.Network,
		User: offloadtest.RunAs()}
	h.svc.offload.insecure = true // MinIO without TLS
	ctx := context.Background()
	in := OffloadInput{Enabled: true, Endpoint: m.Endpoint, Bucket: m.Bucket, AccessKeyID: m.AccessKey, SecretKey: m.SecretKey,
		PublicURL: m.HostURL + "/" + m.Bucket + "/s1/uploads"}

	// A public URL that isn't this bucket's prefix is refused, and nothing
	// is left in the bucket.
	bad := in
	bad.PublicURL = m.HostURL + "/" + m.Bucket + "/other"
	if _, err := h.svc.SetOffload(ctx, "s1", bad); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong public URL: %v", err)
	}
	wrongKey := in
	wrongKey.SecretKey = "not-the-secret-key"
	if _, err := h.svc.SetOffload(ctx, "s1", wrongKey); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := h.svc.SetOffload(ctx, "s1", in); err != nil {
		t.Fatal(err)
	}
	remote, err := h.svc.Offload.List(ctx, offload.Target{Endpoint: m.Endpoint, Bucket: m.Bucket, Prefix: "s1/uploads/",
		AccessKeyID: m.AccessKey, SecretKey: m.SecretKey})
	if err != nil || len(remote) != 0 {
		t.Fatalf("probe objects left behind: %v %v", remote, err)
	}

	st, _ := h.svc.Store.GetSite(ctx, "s1")
	writeUpload(t, h, "2023/05/old.jpg", "an old photo", 30*24*time.Hour)
	writeUpload(t, h, "2026/09/sp ace é.png", "png", time.Hour)
	writeUpload(t, h, "2026/09/shell.php", "<?php", time.Hour)
	writeUpload(t, h, "woocommerce_uploads/paid.zip", "paid", time.Hour)
	if _, err := h.svc.offloadPass(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{
		"s1/uploads/2023/05/old.jpg": 200, "s1/uploads/2026/09/sp%20ace%20%C3%A9.png": 200,
		"s1/uploads/2026/09/shell.php": 404, "s1/uploads/woocommerce_uploads/paid.zip": 404,
	} {
		if code, _ := m.Get(t, key); code != want {
			t.Errorf("%s: %d, want %d", key, code, want)
		}
	}

	// A new upload (incremental), then WordPress deletes one.
	writeUpload(t, h, "2026/09/new.gif", "gif", 0)
	if res, err := h.svc.offloadPass(ctx, st, false); err != nil || res.Objects != 1 {
		t.Fatalf("incremental: %+v %v", res, err)
	}
	if code, body := m.Get(t, "s1/uploads/2026/09/new.gif"); code != 200 || body != "gif" {
		t.Errorf("new upload: %d %q", code, body)
	}
	os.Remove(filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content", "uploads", "2026", "09", "new.gif"))
	os.WriteFile(filepath.Join(h.svc.Cfg.SiteDir("s1"), offloadQueuePath), []byte("2026/09/new.gif\n../../etc/passwd\n"), 0o644)
	if res, err := h.svc.offloadPass(ctx, st, false); err != nil || res.Deleted != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if code, _ := m.Get(t, "s1/uploads/2026/09/new.gif"); code != 404 {
		t.Error("a deleted upload is still in the bucket")
	}

	// Local copies older than local_days go once the bucket holds them.
	in.SecretKey, in.LocalDays = "", 7
	if _, err := h.svc.SetOffload(ctx, "s1", in); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.offloadPass(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content", "uploads", "2023", "05", "old.jpg")
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the old upload's local copy was kept")
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content", "uploads", "2026", "09", "sp ace é.png")); err != nil {
		t.Fatal("a recent upload's local copy was removed")
	}
	if code, body := m.Get(t, "s1/uploads/2023/05/old.jpg"); code != 200 || body != "an old photo" {
		t.Fatalf("removed locally but not served: %d %q", code, body)
	}
	status, _ := h.svc.OffloadStatus(ctx, "s1")
	if status.RemovedLocal != 1 || status.LastFull == nil || status.LastError != "" {
		t.Errorf("status %+v", status)
	}

	// Copied back, with its mtime.
	if res, err := h.svc.offloadDownload(ctx, "s1", noProgress); err != nil || res.Files != 1 {
		t.Fatalf("download: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(old); string(b) != "an old photo" {
		t.Errorf("copied back: %q", b)
	}
	if entries, _ := os.ReadDir(h.svc.offloadWorkDir("s1")); len(entries) != 0 {
		t.Errorf("scratch left behind: %v", entries)
	}
	ev, _ := h.svc.Store.Events(ctx, "s1", 20)
	if !slices.ContainsFunc(ev, func(e store.Event) bool { return strings.Contains(e.Message, "copied back") }) {
		t.Errorf("events %+v", ev)
	}
}
