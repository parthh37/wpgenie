package site

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/offload"
	"github.com/parthh37/wpgenie/internal/store"
)

// fakeBucket is object storage: the engine side (what rclone would do) and
// the public side (an HTTPS server answering for example.com).
type fakeBucket struct {
	mu        sync.Mutex
	objs      map[string]fakeObject // path under the prefix
	targets   []offload.Target
	uploads   int
	putErr    error
	delErr    error
	uploadErr error
	status    int // forced public status (0: serve)
	body      string
	srv       *httptest.Server
}

type fakeObject struct {
	data  []byte
	mtime time.Time // the file's, as rclone stores it
	up    time.Time // upload time
}

func (b *fakeBucket) Upload(_ context.Context, t offload.Target, dir string) (offload.Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.targets = append(b.targets, t)
	if b.uploadErr != nil {
		return offload.Stats{}, b.uploadErr
	}
	b.uploads++
	var st offload.Stats
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return errors.New("the staging directory holds something else than files: " + p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		rel, _ := filepath.Rel(dir, p)
		b.objs[filepath.ToSlash(rel)] = fakeObject{data, fi.ModTime(), time.Now()}
		st.Objects++
		st.Bytes += int64(len(data))
		return nil
	})
	return st, err
}

func (b *fakeBucket) Download(_ context.Context, _ offload.Target, paths []string, dir string) (offload.Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var st offload.Stats
	for _, p := range paths {
		o, ok := b.objs[p]
		if !ok {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.WriteFile(dst, o.data, 0o644)
		os.Chtimes(dst, o.mtime, o.mtime)
		st.Objects++
	}
	return st, nil
}

func (b *fakeBucket) Delete(_ context.Context, _ offload.Target, paths []string) (offload.Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var st offload.Stats
	for _, p := range paths {
		if _, ok := b.objs[p]; ok {
			delete(b.objs, p)
			st.Deletes++
		}
	}
	return st, nil
}

func (b *fakeBucket) List(context.Context, offload.Target) (map[string]offload.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]offload.Object{}
	for p, o := range b.objs {
		sum := md5.Sum(o.data)
		out[p] = offload.Object{Size: int64(len(o.data)), MD5: hex.EncodeToString(sum[:]), Modified: o.up}
	}
	return out, nil
}

func (b *fakeBucket) Put(_ context.Context, t offload.Target, name string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.targets = append(b.targets, t)
	if b.putErr != nil {
		return b.putErr
	}
	b.objs[name] = fakeObject{data, time.Now(), time.Now()}
	return nil
}

func (b *fakeBucket) DeleteFile(_ context.Context, _ offload.Target, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objs, name)
	return b.delErr
}

func (b *fakeBucket) has(p string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objs[p]
	return ok
}

func (b *fakeBucket) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k := range b.objs {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ServeHTTP is the bucket's public URL, https://example.com/pub.
func (b *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status != 0 {
		w.WriteHeader(b.status)
		w.Write([]byte(b.body))
		return
	}
	o, ok := b.objs[strings.TrimPrefix(r.URL.Path, "/pub/")]
	if !ok || !strings.HasPrefix(r.URL.Path, "/pub/") {
		http.NotFound(w, r)
		return
	}
	w.Write(o.data)
}

const offloadSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"

func offloadHarness(t *testing.T) (*harness, *fakeBucket) {
	h := newHarness(t)
	b := &fakeBucket{objs: map[string]fakeObject{}}
	b.srv = httptest.NewTLSServer(b)
	t.Cleanup(b.srv.Close)
	client := b.srv.Client()
	// Every hostname reaches the test server (its certificate is for
	// example.com), as the bucket's public URL would.
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, b.srv.Listener.Addr().String())
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	h.svc.Offload, h.svc.offload.http = b, client
	return h, b
}

func offloadOn() OffloadInput {
	return OffloadInput{Enabled: true, Endpoint: "https://s3.eu-central-1.amazonaws.com", Region: "eu-central-1", Bucket: "media",
		AccessKeyID: "AKIAEXAMPLE123", SecretKey: offloadSecret, PublicURL: "https://example.com/pub/"}
}

// writeUpload puts a file in s1's uploads with the given age.
func writeUpload(t *testing.T, h *harness, rel, body string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content", "uploads", filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	os.Chtimes(p, mt, mt)
	return p
}

func TestSetOffloadValidates(t *testing.T) {
	h, b := offloadHarness(t)
	ctx := context.Background()
	for name, mut := range map[string]func(*OffloadInput){
		"http endpoint":         func(in *OffloadInput) { in.Endpoint = "http://s3.example.com" },
		"endpoint with path":    func(in *OffloadInput) { in.Endpoint = "https://s3.example.com/media" },
		"http public URL":       func(in *OffloadInput) { in.PublicURL = "http://example.com/pub" },
		"public URL on an IP":   func(in *OffloadInput) { in.PublicURL = "https://10.0.0.5/pub" },
		"public URL localhost":  func(in *OffloadInput) { in.PublicURL = "https://localhost/pub" },
		"public URL query":      func(in *OffloadInput) { in.PublicURL = "https://example.com/pub?x=1" },
		"public URL bad chars":  func(in *OffloadInput) { in.PublicURL = "https://example.com/p ub" },
		"public URL caddyfile":  func(in *OffloadInput) { in.PublicURL = "https://example.com/pub\n}" },
		"bucket":                func(in *OffloadInput) { in.Bucket = "Media_Bucket" },
		"prefix traversal":      func(in *OffloadInput) { in.Prefix = "s1/../s2/" },
		"no secret":             func(in *OffloadInput) { in.SecretKey = "" },
		"secret with a newline": func(in *OffloadInput) { in.SecretKey = "abc\nRCLONE_S3_ENDPOINT=https://evil.test" },
		"negative days":         func(in *OffloadInput) { in.LocalDays = -1 },
		"acl":                   func(in *OffloadInput) { in.ACL = "public-read-write" },
	} {
		in := offloadOn()
		mut(&in)
		if _, err := h.svc.SetOffload(ctx, "s1", in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v, want ErrInvalidInput", name, err)
		}
	}
	if len(b.targets) != 0 {
		t.Error("invalid settings reached the storage")
	}

	// Another site's prefix may not contain this one's (its delete queue
	// would reach these objects), nor the other way round.
	other := &store.Site{ID: "s2", Name: "s2", PrimaryDomain: "b.test", PHPVersion: "8.3", FPMPort: 19001,
		DBName: "wp_s2", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1, Upstreams: []int{19001}}
	h.svc.Store.CreateSite(ctx, other)
	h.svc.Store.SetOffload(ctx, &store.Offload{SiteID: "s2", Endpoint: "https://S3.eu-central-1.amazonaws.com", Bucket: "media",
		Prefix: "shared/", AccessKeyID: "x", SecretKey: "y", PublicURL: "https://example.com/x"}, true)
	for _, prefix := range []string{"shared/", "shared/s1/", ""} {
		in := offloadOn()
		in.Prefix = prefix
		if prefix == "" {
			h.svc.Store.SetOffload(ctx, &store.Offload{SiteID: "s2", Endpoint: "https://s3.eu-central-1.amazonaws.com", Bucket: "media",
				Prefix: "s1/", AccessKeyID: "x", SecretKey: "y", PublicURL: "https://example.com/x"}, true)
		}
		if _, err := h.svc.SetOffload(ctx, "s1", in); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "s2") {
			t.Errorf("prefix %q overlapping s2's: %v", prefix, err)
		}
	}
}

func TestSetOffloadChecksEndToEnd(t *testing.T) {
	h, b := offloadHarness(t)
	ctx := context.Background()

	b.status = http.StatusForbidden
	_, err := h.svc.SetOffload(ctx, "s1", offloadOn())
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "isn't publicly readable") || !strings.Contains(err.Error(), "403") {
		t.Errorf("private bucket: %v", err)
	}
	b.status, b.body = http.StatusOK, "something else"
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "doesn't serve the test object") {
		t.Errorf("wrong public URL: %v", err)
	}
	b.status = http.StatusMovedPermanently
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("redirect: %v", err)
	}
	b.status, b.delErr = 0, errors.New("403 AccessDenied: Access Denied.")
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "allowed to delete") {
		t.Errorf("key that can't delete: %v", err)
	}
	b.delErr, b.putErr = nil, errors.New("SignatureDoesNotMatch: The request signature we calculated does not match")
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("wrong key: %v", err)
	}
	b.putErr = nil
	if _, err := h.svc.Store.GetOffload(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("settings that failed their check were stored")
	}
	if len(b.keys()) != 0 {
		t.Errorf("probe objects left behind: %v", b.keys())
	}

	st, err := h.svc.SetOffload(ctx, "s1", offloadOn())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.Prefix != "s1/uploads/" || st.PublicURL != "https://example.com/pub" || st.AccessKeyID != "AKIA…E123" {
		t.Errorf("status %+v", st)
	}
	if last := b.targets[len(b.targets)-1]; last.Prefix != "s1/uploads/" || last.SecretKey != offloadSecret {
		t.Errorf("target %+v", last)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), offloadSecret) || strings.Contains(string(raw), "AKIAEXAMPLE123") {
		t.Fatalf("the status leaks a key: %s", raw)
	}
	if on, err := h.svc.OffloadEnabled(ctx, "s1"); !on || err != nil {
		t.Error("OffloadEnabled", on, err)
	}
	// Caddy serves missing uploads from the public URL; PHP queues deletes.
	if p := h.proxy.last[0]; p.Offload != "https://example.com/pub" {
		t.Errorf("proxy offload %q", p.Offload)
	}
	wrapper, err := os.ReadFile(filepath.Join(h.svc.Cfg.SiteRoot("s1"), offloadWrapperPath))
	if err != nil || !strings.Contains(string(wrapper), "define( 'WPGENIE_OFFLOAD_QUEUE', '"+filepath.Join(h.svc.Cfg.SiteDir("s1"), "logs", "offload-deletes.queue")+"' );") ||
		strings.Contains(string(wrapper), offloadSecret) {
		t.Fatalf("wrapper %s %v", wrapper, err)
	}

	// Changing only local_days needs no new check; an empty secret keeps
	// the stored one, but not for another endpoint.
	b.putErr = errors.New("must not be called")
	in := offloadOn()
	in.SecretKey, in.AccessKeyID, in.LocalDays = "", "", 30
	if st, err := h.svc.SetOffload(ctx, "s1", in); err != nil || st.LocalDays != 30 {
		t.Fatalf("local_days change: %v", err)
	}
	in.Endpoint = "https://s3.us-east-1.amazonaws.com"
	if _, err := h.svc.SetOffload(ctx, "s1", in); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "secret_key") {
		t.Errorf("the stored secret went to another endpoint: %v", err)
	}
	b.putErr = nil

	// Off: the wrapper and the fallback go, the bucket's objects stay.
	if _, err := h.svc.SetOffload(ctx, "s1", OffloadInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteRoot("s1"), offloadWrapperPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("wrapper left behind")
	}
	if h.proxy.last[0].Offload != "" {
		t.Error("fallback left in Caddy")
	}
}

func TestOffloadSync(t *testing.T) {
	h, b := offloadHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); err != nil {
		t.Fatal(err)
	}
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	writeUpload(t, h, "2024/01/a.jpg", "jpeg", 48*time.Hour)
	writeUpload(t, h, "2024/01/sp ace é.png", "png", 48*time.Hour)
	writeUpload(t, h, "2024/01/a.jpg.webp", "copy", time.Hour)
	for _, never := range []string{"2024/01/shell.php", "2024/01/x.PHP5", "2024/01/y.php.jpg", ".htaccess", "2024/.user.ini",
		".hidden/secret.jpg", "woocommerce_uploads/paid.zip", "backwpup-abc-backups/site.zip", "dump.sql", "debug.log", "db.sql.gz"} {
		writeUpload(t, h, never, "private", time.Hour)
	}
	uploads := filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content", "uploads")
	os.Symlink("/etc/passwd", filepath.Join(uploads, "2024", "01", "link.jpg"))
	syscall.Mkfifo(filepath.Join(uploads, "2024", "01", "pipe.jpg"), 0o644)

	res, err := h.svc.offloadPass(ctx, st, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"2024/01/a.jpg", "2024/01/sp ace é.png"}; !slices.Equal(b.keys(), want) || res.Objects != 2 {
		t.Fatalf("bucket %v (%+v), want %v", b.keys(), res, want)
	}
	o, _ := h.svc.Store.GetOffload(ctx, "s1")
	if o.FullAt.IsZero() || o.IncrementalAt.IsZero() || o.TotalObjects != 2 || o.LastError != "" {
		t.Errorf("recorded %+v", o)
	}
	// Staged copies keep the file's mtime (rclone stores it); scratch goes.
	if got := b.objs["2024/01/a.jpg"].mtime; time.Since(got) < 47*time.Hour {
		t.Errorf("mtime not kept: %v", got)
	}
	if entries, _ := os.ReadDir(h.svc.offloadWorkDir("s1")); len(entries) != 0 {
		t.Errorf("scratch left behind: %v", entries)
	}

	// A full copy with nothing new uploads nothing; an incremental takes
	// what changed since the last one started.
	if res, _ := h.svc.offloadPass(ctx, st, true); res.Objects != 0 {
		t.Errorf("unchanged files uploaded again: %+v", res)
	}
	writeUpload(t, h, "2024/02/new.gif", "gif", 0)
	res, err = h.svc.offloadPass(ctx, st, false)
	if err != nil || res.Objects != 1 || !b.has("2024/02/new.gif") {
		t.Fatalf("incremental: %+v %v %v", res, err, b.keys())
	}
	if res, _ := h.svc.offloadPass(ctx, st, false); res.Objects != 0 {
		t.Errorf("the overlap between incremental copies was uploaded twice: %+v", res)
	}
	// Changed in place: uploaded again.
	writeUpload(t, h, "2024/02/new.gif", "gif v2", 0)
	if res, _ := h.svc.offloadPass(ctx, st, false); res.Objects != 1 || string(b.objs["2024/02/new.gif"].data) != "gif v2" {
		t.Errorf("changed file: %+v", res)
	}

	// A failure is recorded (with backoff) and the next copy starts from
	// the same point.
	b.uploadErr = errors.New("503 SlowDown")
	writeUpload(t, h, "2024/02/later.jpg", "later", 0)
	if _, err := h.svc.offloadPass(ctx, st, false); err == nil {
		t.Fatal("upload error swallowed")
	}
	o, _ = h.svc.Store.GetOffload(ctx, "s1")
	if o.Failures != 1 || !strings.Contains(o.LastError, "SlowDown") {
		t.Errorf("failure not recorded: %+v", o)
	}
	if run, _ := offloadDue(o, time.Now(), time.Time{}, time.Time{}, false); run {
		t.Error("retried without backoff")
	}
	b.uploadErr = nil
	if _, err := h.svc.offloadPass(ctx, st, false); err != nil || !b.has("2024/02/later.jpg") {
		t.Fatalf("retry: %v", err)
	}
	if ev, _ := h.svc.Store.Events(ctx, "s1", 10); !slices.ContainsFunc(ev, func(e store.Event) bool {
		return strings.Contains(e.Message, "works again")
	}) {
		t.Error("recovery not logged")
	}
}

// The site controls wp-content: a symlinked uploads directory must not be
// followed anywhere (here to another site's files).
func TestOffloadRefusesSymlinkedUploads(t *testing.T) {
	h, b := offloadHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); err != nil {
		t.Fatal(err)
	}
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	secret := filepath.Join(h.svc.Cfg.SitesDir(), "s2", "public", "wp-content", "uploads")
	os.MkdirAll(secret, 0o755)
	os.WriteFile(filepath.Join(secret, "private.jpg"), []byte("s2"), 0o644)
	os.MkdirAll(filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content"), 0o755)
	if err := os.Symlink(secret, filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content", "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.offloadPass(ctx, st, true); err == nil {
		t.Error("a symlinked uploads directory was accepted")
	}
	// wp-content itself swapped for a symlink inside the docroot.
	os.Remove(filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content", "uploads"))
	os.RemoveAll(filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content"))
	os.MkdirAll(filepath.Join(h.svc.Cfg.SiteRoot("s1"), "elsewhere", "uploads"), 0o755)
	os.WriteFile(filepath.Join(h.svc.Cfg.SiteRoot("s1"), "elsewhere", "uploads", "x.jpg"), []byte("x"), 0o644)
	os.Symlink("elsewhere", filepath.Join(h.svc.Cfg.SiteRoot("s1"), "wp-content"))
	if _, err := h.svc.offloadPass(ctx, st, true); err == nil {
		t.Error("a symlinked wp-content was accepted")
	}
	if len(b.keys()) != 0 {
		t.Fatalf("uploaded through a symlink: %v", b.keys())
	}
}

func TestOffloadDeletes(t *testing.T) {
	h, b := offloadHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); err != nil {
		t.Fatal(err)
	}
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	a := writeUpload(t, h, "2024/01/a.jpg", "a", time.Hour)
	writeUpload(t, h, "2024/01/a-300x200.jpg", "a small", time.Hour)
	writeUpload(t, h, "2024/01/reused.jpg", "old", time.Hour)
	writeUpload(t, h, "2024/01/keep.jpg", "keep", time.Hour)
	if _, err := h.svc.offloadPass(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	// WordPress deleted a.jpg and its size, and reused.jpg, whose name was
	// then taken by a new upload. The queue is the site's: untrusted.
	os.Remove(a)
	os.Remove(filepath.Join(filepath.Dir(a), "a-300x200.jpg"))
	writeUpload(t, h, "2024/01/reused.jpg", "new", 0)
	queue := filepath.Join(h.svc.Cfg.SiteDir("s1"), offloadQueuePath)
	os.WriteFile(queue, []byte("2024/01/a.jpg\n2024/01/a-300x200.jpg\n2024/01/reused.jpg\n../s2/uploads/x.jpg\n/etc/passwd\n"+
		"2024/../../keep.jpg\n2024/01/keep.jpg/..\n\n2024/01/a.jpg\r\n"), 0o644)
	res, err := h.svc.offloadPass(ctx, st, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"2024/01/keep.jpg", "2024/01/reused.jpg"}; !slices.Equal(b.keys(), want) || res.Deleted != 2 {
		t.Fatalf("bucket %v (%+v), want %v", b.keys(), res, want)
	}
	if string(b.objs["2024/01/reused.jpg"].data) != "new" {
		t.Error("the upload that took a deleted file's name was deleted")
	}
	if fi, _ := os.Stat(queue); fi.Size() != 0 {
		t.Error("queue not emptied")
	}
	if n, _ := h.svc.Store.CountOffloadDeletes(ctx, "s1"); n != 0 {
		t.Errorf("%d deletes still pending", n)
	}
}

func TestReadOffloadQueueDefensively(t *testing.T) {
	h, _ := offloadHarness(t)
	dir := h.svc.Cfg.SiteDir("s1")
	os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	queue := filepath.Join(dir, offloadQueuePath)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("2024/secret.jpg\n"), 0o644)

	os.Symlink(secret, queue)
	if _, _, err := h.svc.readOffloadQueue("s1"); err == nil {
		t.Error("a symlinked queue was read")
	}
	if b, _ := os.ReadFile(secret); len(b) == 0 {
		t.Fatal("the symlink's target was truncated")
	}
	os.Remove(queue)
	syscall.Mkfifo(queue, 0o644)
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.readOffloadQueue("s1"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a named pipe was read as the queue")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a named pipe blocked the daemon")
	}
	os.Remove(queue)

	// A site holding the lock only delays its own deletes.
	os.WriteFile(queue, []byte("2024/a.jpg\n"), 0o644)
	f, _ := os.Open(queue)
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	if paths, _, err := h.svc.readOffloadQueue("s1"); err != nil || len(paths) != 0 {
		t.Errorf("read under someone else's lock: %v %v", paths, err)
	}
	f.Close()
	if paths, _, err := h.svc.readOffloadQueue("s1"); err != nil || !slices.Equal(paths, []string{"2024/a.jpg"}) {
		t.Errorf("after the lock: %v %v", paths, err)
	}
}

func TestQueuePathsAndFilters(t *testing.T) {
	for in, want := range map[string]string{
		"2024/01/a.jpg": "2024/01/a.jpg", "a b é.png": "a b é.png", "x.pdf\r": "x.pdf",
		"": "", "/etc/passwd": "", "../x.jpg": "", "a/../../x.jpg": "", "a//b.jpg": "", "./a.jpg": "", "a/.": "",
		"a/b/..": "", "..": "", `a\b.jpg`: "", "a\x00.jpg": "", "shell.php": "", "x.PhTmL": "", ".htaccess": "",
		"a/.hidden.jpg": "", "woocommerce_uploads/paid.zip": "", "WooCommerce_Uploads/x.jpg": "", "backwpup-1a2b-logs/x.html": "",
		"edd/2024/file.pdf": "", "edd.jpg": "edd.jpg", "2024/woocommerce_uploads/x.jpg": "2024/woocommerce_uploads/x.jpg",
		"a.jpg.avif": "", "b.PNG.webp": "", "c.webp": "c.webp", "dump.sql.gz": "", "x.bak": "",
	} {
		if got := cleanQueuePath(in); got != want {
			t.Errorf("cleanQueuePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOffloadDue(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	ok := &store.Offload{FullAt: ago(10 * time.Hour), IncrementalAt: ago(time.Minute)}
	for _, c := range []struct {
		name          string
		o             *store.Offload
		last, poked   time.Time
		window        bool
		run, wantFull bool
	}{
		{"never synced: full at once", &store.Offload{}, time.Time{}, time.Time{}, false, true, true},
		{"a minute since the last: incremental", ok, ago(61 * time.Second), time.Time{}, false, true, false},
		{"just ran", ok, ago(20 * time.Second), time.Time{}, false, false, false},
		{"poked: sooner", ok, ago(20 * time.Second), ago(5 * time.Second), false, true, false},
		{"poked, but only just ran", ok, ago(5 * time.Second), ago(time.Second), false, false, false},
		{"poke before the last run", ok, ago(30 * time.Second), ago(40 * time.Second), false, false, false},
		{"nightly in the window", &store.Offload{FullAt: ago(21 * time.Hour), IncrementalAt: ago(time.Minute)}, ago(2 * time.Minute), time.Time{}, true, true, true},
		{"already full tonight", ok, ago(2 * time.Minute), time.Time{}, true, true, false},
		{"window missed for two days", &store.Offload{FullAt: ago(49 * time.Hour), IncrementalAt: ago(time.Minute)}, ago(time.Second), time.Time{}, false, true, true},
		{"failed just now: back off", &store.Offload{FullAt: ago(time.Hour), IncrementalAt: ago(time.Hour), Failures: 3, AttemptAt: ago(time.Minute)},
			ago(time.Minute), ago(time.Second), false, false, false},
		{"failed a while ago: retry", &store.Offload{FullAt: ago(time.Hour), IncrementalAt: ago(time.Hour), Failures: 3, AttemptAt: ago(5 * time.Minute)},
			ago(5 * time.Minute), time.Time{}, false, true, false},
	} {
		run, full := offloadDue(c.o, now, c.last, c.poked, c.window)
		if run != c.run || (run && full != c.wantFull) {
			t.Errorf("%s: run=%v full=%v", c.name, run, full)
		}
	}
	if offloadBackoff(50) != offloadMaxBackoff || offloadBackoff(1) != time.Minute {
		t.Error("backoff bounds")
	}
}

func TestOffloadRemovesLocalCopies(t *testing.T) {
	h, b := offloadHarness(t)
	ctx := context.Background()
	in := offloadOn()
	in.LocalDays = 7
	if _, err := h.svc.SetOffload(ctx, "s1", in); err != nil {
		t.Fatal(err)
	}
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	old := writeUpload(t, h, "2023/old.jpg", "old", 30*24*time.Hour)
	changed := writeUpload(t, h, "2023/changed.jpg", "v1", 30*24*time.Hour)
	recent := writeUpload(t, h, "2026/recent.jpg", "recent", 24*time.Hour)
	// The first full copy uploads them: none was confirmed in the bucket
	// before, so nothing is removed yet.
	if _, err := h.svc.offloadPass(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("removed before the bucket was seen to hold it")
	}
	// The bucket's copy of changed.jpg differs (same size): kept.
	b.mu.Lock()
	o := b.objs["2023/changed.jpg"]
	o.data = []byte("v2")
	b.objs["2023/changed.jpg"] = o
	b.mu.Unlock()
	// A public URL that stopped working: nothing is removed.
	b.status = http.StatusForbidden
	if _, err := h.svc.offloadPass(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("removed while the public URL doesn't serve it")
	}
	b.status = 0
	if _, err := h.svc.offloadPass(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Error("an old file the bucket holds was kept")
	}
	for _, p := range []string{changed, recent} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
	off, _ := h.svc.Store.GetOffload(ctx, "s1")
	if off.RemovedLocal != 1 || off.RemovedBytes != 3 {
		t.Errorf("recorded %+v", off)
	}

	// Offload can't be turned off (or moved) while uploads only exist in
	// the bucket, until they're copied back.
	if _, err := h.svc.SetOffload(ctx, "s1", OffloadInput{}); !errors.Is(err, ErrConflict) {
		t.Errorf("turned off with uploads only in the bucket: %v", err)
	}
	moved := in
	moved.Prefix = "elsewhere/"
	if _, err := h.svc.SetOffload(ctx, "s1", moved); !errors.Is(err, ErrConflict) {
		t.Errorf("moved with uploads only in the bucket: %v", err)
	}
	res, err := h.svc.offloadDownload(ctx, "s1", noProgress)
	if err != nil || res.Files != 1 {
		t.Fatalf("download: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(old); string(got) != "old" {
		t.Errorf("copied back: %q", got)
	}
	if fi, _ := os.Stat(old); time.Since(fi.ModTime()) < 29*24*time.Hour {
		t.Error("copied back without its mtime")
	}
	if got, _ := os.ReadFile(changed); string(got) != "v1" {
		t.Error("a local file was overwritten by the bucket's copy")
	}
	if _, err := h.svc.SetOffload(ctx, "s1", OffloadInput{}); err != nil {
		t.Fatalf("off after copying back: %v", err)
	}
}

// Staging sites don't offload (a copy of a shop must not touch the live
// bucket) but serve uploads they lack from the live site's public URL.
func TestStagingUsesParentBucketReadOnly(t *testing.T) {
	h, _ := offloadHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); err != nil {
		t.Fatal(err)
	}
	stg := &store.Site{ID: "s9", Name: "Staging", PrimaryDomain: "staging.a.test", PHPVersion: "8.3", FPMPort: 19002,
		DBName: "wp_s9", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1,
		Upstreams: []int{19002}, ParentID: "s1"}
	if err := h.svc.Store.CreateSite(ctx, stg); err != nil {
		t.Fatal(err)
	}
	// The clone carries the live site's wrapper; rewriting WPGenie's files
	// removes it.
	os.MkdirAll(filepath.Join(h.svc.Cfg.SiteRoot("s9"), "wp-content", "mu-plugins"), 0o755)
	live, _ := os.ReadFile(filepath.Join(h.svc.Cfg.SiteRoot("s1"), offloadWrapperPath))
	os.WriteFile(filepath.Join(h.svc.Cfg.SiteRoot("s9"), offloadWrapperPath), live, 0o644)
	if err := h.svc.rewriteManagedFiles(ctx, "s9"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.svc.Cfg.SiteRoot("s9"), offloadWrapperPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the staging site queues deletes for the live bucket")
	}
	if err := h.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range h.proxy.last {
		if p.ID == "s9" && p.Offload != "https://example.com/pub" {
			t.Errorf("staging fallback %q", p.Offload)
		}
	}
	st, err := h.svc.OffloadStatus(ctx, "s9")
	if err != nil || st.Enabled || st.ParentPublicURL != "https://example.com/pub" {
		t.Errorf("staging status %+v %v", st, err)
	}
	if on, _ := h.svc.OffloadEnabled(ctx, "s9"); on {
		t.Error("staging reported as offloaded")
	}
}

// The loop copies due sites in the background, each once at a time.
func TestOffloadTick(t *testing.T) {
	h, b := offloadHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetOffload(ctx, "s1", offloadOn()); err != nil {
		t.Fatal(err)
	}
	writeUpload(t, h, "2024/a.jpg", "a", time.Hour)
	slots := offloadSlots{inc: make(chan struct{}, 1), full: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	h.svc.offloadTick(ctx, time.Now(), slots, &wg)
	wg.Wait()
	if !b.has("2024/a.jpg") {
		t.Fatal("the first (full) copy didn't run")
	}
	// Just ran: nothing to do until a minute passes or PHP pokes.
	writeUpload(t, h, "2024/b.jpg", "b", 0)
	h.svc.offloadTick(ctx, time.Now(), slots, &wg)
	wg.Wait()
	if b.has("2024/b.jpg") {
		t.Fatal("copied again at once")
	}
	h.svc.offloadTick(ctx, time.Now().Add(offloadEvery), slots, &wg)
	wg.Wait()
	if !b.has("2024/b.jpg") {
		t.Fatal("the incremental copy didn't run after a minute")
	}
	// Turned off: never copied again.
	if _, err := h.svc.SetOffload(ctx, "s1", OffloadInput{}); err != nil {
		t.Fatal(err)
	}
	writeUpload(t, h, "2024/c.jpg", "c", 0)
	h.svc.offloadTick(ctx, time.Now().Add(time.Hour), slots, &wg)
	wg.Wait()
	if b.has("2024/c.jpg") {
		t.Fatal("copied after offload was turned off")
	}
}
