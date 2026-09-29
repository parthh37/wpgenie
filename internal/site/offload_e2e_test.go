package site

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/offload"
	"github.com/parthh37/wpgenie/internal/offload/offloadtest"
)

// TestOffloadEndToEnd runs uploads offload against real WordPress, rclone
// and MinIO: uploads (every size) reach the bucket; deleting an attachment
// in WordPress deletes all its objects, also once its local copies are gone
// (WordPress then skips wp_delete_file); a staging clone doesn't queue
// deletes for the live bucket. Needs the wpgenie/php:8.3 image (see newE2E).
func TestOffloadEndToEnd(t *testing.T) {
	e := newE2E(t)
	ctx, svc := e.ctx, e.svc
	m := offloadtest.Start(t, "")
	svc.Offload = &offload.Rclone{Docker: e.docker, Image: svc.Cfg.RcloneImage, Network: m.Network}
	svc.offload.insecure = true // MinIO without TLS

	st, jobID, err := svc.StartCreate(ctx, CreateInput{Domain: "offload.test", AdminEmail: "a@offload.test"})
	e.wait(jobID, err)
	id := st.ID
	st = mustSite(t, e.st, id)
	if _, err := svc.SetOffload(ctx, id, OffloadInput{Enabled: true, Endpoint: m.Endpoint, Bucket: m.Bucket,
		AccessKeyID: m.AccessKey, SecretKey: m.SecretKey, PublicURL: m.HostURL + "/" + m.Bucket + "/" + id + "/uploads"}); err != nil {
		t.Fatal(err)
	}
	prefix := id + "/uploads/"
	objects := func() []string {
		t.Helper()
		out := m.MC(t, "", "ls", "--recursive", "m/"+m.Bucket+"/"+prefix)
		var keys []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if f := strings.Fields(line); len(f) > 0 {
				keys = append(keys, f[len(f)-1])
			}
		}
		slices.Sort(keys)
		return keys
	}

	// An upload with its sizes; PHP pokes the daemon.
	e.sh(id, `php -r '$i = imagecreatetruecolor(1200, 800); imagefilledrectangle($i, 0, 0, 600, 400, 0xff0000); imagejpeg($i, "/tmp/photo.jpg", 90);'`)
	first := e.wp(id, "media", "import", "/tmp/photo.jpg", "--porcelain")
	if _, err := os.Stat(filepath.Join(svc.Cfg.SiteDir(id), offloadPokePath)); err != nil {
		t.Fatal("no poke after an upload")
	}
	if _, err := svc.offloadPass(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	file := e.wp(id, "post", "meta", "get", first, "_wp_attached_file")
	dir := file[:strings.LastIndex(file, "/")+1]
	keys := objects()
	if !slices.Contains(keys, file) || !slices.ContainsFunc(keys, func(k string) bool { return strings.HasSuffix(k, "-300x200.jpg") }) {
		t.Fatalf("bucket after the upload: %v", keys)
	}

	// Deleted in WordPress: every object goes.
	e.wp(id, "post", "delete", first, "--force")
	queue, _ := os.ReadFile(filepath.Join(svc.Cfg.SiteDir(id), offloadQueuePath))
	if !strings.Contains(string(queue), file+"\n") || strings.Contains(string(queue), "/var/") {
		t.Fatalf("queue: %q", queue)
	}
	if _, err := svc.offloadPass(ctx, st, false); err != nil {
		t.Fatal(err)
	}
	if keys := objects(); len(keys) != 0 {
		t.Fatalf("objects of a deleted attachment remain: %v", keys)
	}

	// Once local copies are gone (local_days), WordPress can't find the
	// files and never calls wp_delete_file: the attachment's own list counts.
	second := e.wp(id, "media", "import", "/tmp/photo.jpg", "--porcelain")
	if _, err := svc.offloadPass(ctx, st, false); err != nil {
		t.Fatal(err)
	}
	n := len(objects())
	e.sh(id, "rm -f wp-content/uploads/"+dir+"photo*")
	e.wp(id, "post", "delete", second, "--force")
	if _, err := svc.offloadPass(ctx, st, false); err != nil {
		t.Fatal(err)
	}
	if keys := objects(); n < 2 || len(keys) != 0 {
		t.Fatalf("%d objects uploaded; left after deleting the attachment without local copies: %v", n, keys)
	}

	// A staging clone serves the live bucket but never deletes from it.
	stg, jobID, err := svc.StartStaging(ctx, id, StagingInput{})
	e.wait(jobID, err)
	if _, err := os.Stat(filepath.Join(svc.Cfg.SiteRoot(stg.ID), offloadWrapperPath)); err == nil {
		t.Fatal("the staging copy queues deletes for the live bucket")
	}
	if on, _ := svc.OffloadEnabled(ctx, stg.ID); on {
		t.Fatal("staging offloads")
	}
}
