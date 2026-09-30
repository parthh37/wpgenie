package offload

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/offload/offloadtest"
	"github.com/parthh37/wpgenie/internal/runtime"
)

var testTarget = Target{Endpoint: "https://s3.eu-central-1.amazonaws.com", Region: "eu-central-1", Bucket: "media-bucket",
	Prefix: "s1/uploads/", AccessKeyID: "AKIAEXAMPLE123", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

type fakeDocker struct {
	args  []string
	stdin string
	out   string
	err   error
}

func (f *fakeDocker) Stream(_ context.Context, stdin io.Reader, w io.Writer, args ...string) error {
	f.args = args
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.stdin = string(b)
	}
	io.WriteString(w, f.out)
	return f.err
}

// Keys go in on stdin, never in argv (ps, docker inspect); rclone gets no
// capabilities and only the daemon's own directory.
func TestSecretsOnStdinOnly(t *testing.T) {
	d := &fakeDocker{out: `{"level":"notice","msg":"x","stats":{"bytes":42,"transfers":3,"errors":0}}` + "\n"}
	r := &Rclone{Docker: d, Image: "rclone/rclone:1.75.1"}
	tg := testTarget
	tg.ACL = "public-read"
	st, err := r.Upload(context.Background(), tg, "/var/lib/wpgenie/offload/s1/stage-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Objects != 3 || st.Bytes != 42 {
		t.Errorf("stats %+v", st)
	}
	argv := strings.Join(d.args, " ")
	if strings.Contains(argv, tg.SecretKey) || strings.Contains(argv, tg.AccessKeyID) {
		t.Fatalf("credentials in argv: %s", argv)
	}
	if !strings.HasPrefix(d.stdin, "RCLONE_S3_ACCESS_KEY_ID="+tg.AccessKeyID+"\nRCLONE_S3_SECRET_ACCESS_KEY="+tg.SecretKey+"\n\n") {
		t.Errorf("stdin %q", d.stdin)
	}
	for _, want := range []string{"--cap-drop ALL", "--read-only", "--security-opt no-new-privileges",
		"-v /var/lib/wpgenie/offload/s1/stage-1:/src:ro", ":s3:media-bucket/s1/uploads", "--no-check-dest",
		"--s3-provider AWS", "--s3-region eu-central-1", "--s3-no-check-bucket", "--s3-acl public-read",
		"--network bridge", "Cache-Control: public, max-age=2592000"} {
		if !strings.Contains(argv, want) {
			t.Errorf("missing %q in %s", want, argv)
		}
	}
	// The delete list follows the secret block on stdin.
	if _, err := r.Delete(context.Background(), tg, []string{"2024/01/a.jpg", "b c.png"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(d.stdin, "\n\n2024/01/a.jpg\nb c.png\n") || !slices.Contains(d.args, "--files-from-raw") {
		t.Errorf("delete stdin %q args %v", d.stdin, d.args)
	}
	for _, bad := range [][]string{{"../x"}, {"/etc/passwd"}, {"a/../../b"}, {"a\nb"}, {""}, {`a\..\b`}} {
		if _, err := r.Delete(context.Background(), tg, bad); err == nil {
			t.Errorf("delete of %q accepted", bad)
		}
	}
	bad := tg
	bad.SecretKey = "abc\nRCLONE_S3_ENDPOINT=http://evil"
	if _, err := r.Upload(context.Background(), bad, "/x"); err == nil {
		t.Error("a line break in the secret must be refused (it would inject another variable)")
	}
	if _, err := r.Upload(context.Background(), tg, "/var/lib/x:/etc"); err == nil {
		t.Error("unsafe mount accepted")
	}
}

func TestErrorsAreReadable(t *testing.T) {
	d := &fakeDocker{err: errors.New("docker run: exit status 1: "), out: strings.Join([]string{
		`{"level":"error","msg":"Failed to copy: operation error S3: PutObject, https response error StatusCode: 403, RequestID: 1, HostID: 2, api error AccessDenied: Access Denied.","object":"2024/a.jpg"}`,
		`{"level":"error","msg":"Failed to copy: operation error S3: PutObject, https response error StatusCode: 403, RequestID: 1, HostID: 2, api error AccessDenied: Access Denied.","object":"2024/a.jpg"}`,
		`{"level":"notice","msg":"Failed to copy with 2 errors: last error was: operation error S3: PutObject, https response error StatusCode: 403, RequestID: 3, api error AccessDenied: Access Denied."}`,
	}, "\n")}
	r := &Rclone{Docker: d, Image: "x"}
	_, err := r.Upload(context.Background(), testTarget, "/x")
	if err == nil || !strings.Contains(err.Error(), "2024/a.jpg: 403 AccessDenied: Access Denied.") ||
		strings.Count(err.Error(), "2024/a.jpg") != 1 || strings.Contains(err.Error(), "RequestID") {
		t.Errorf("err %v", err)
	}
}

func TestValidate(t *testing.T) {
	if err := testTarget.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Target){
		"endpoint path":   func(t *Target) { t.Endpoint = "https://s3.example.com/bucket" },
		"endpoint scheme": func(t *Target) { t.Endpoint = "ftp://s3.example.com" },
		"endpoint user":   func(t *Target) { t.Endpoint = "https://a:b@s3.example.com" },
		"bucket":          func(t *Target) { t.Bucket = "Bad_Bucket" },
		"bucket dots":     func(t *Target) { t.Bucket = "a..b" },
		"prefix slash":    func(t *Target) { t.Prefix = "/s1/" },
		"prefix dotdot":   func(t *Target) { t.Prefix = "s1/../x/" },
		"prefix no slash": func(t *Target) { t.Prefix = "s1" },
		"key id":          func(t *Target) { t.AccessKeyID = "a b" },
		"secret space":    func(t *Target) { t.SecretKey = "a b" },
		"acl":             func(t *Target) { t.ACL = "authenticated-read" },
		"region":          func(t *Target) { t.Region = "EU WEST" },
	} {
		tg := testTarget
		mut(&tg)
		if tg.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for host, want := range map[string]string{"https://s3.amazonaws.com": "AWS", "https://abc.r2.cloudflarestorage.com": "Cloudflare",
		"https://minio.example.com:9000": "Other"} {
		tg := testTarget
		tg.Endpoint = host
		if got := tg.provider(); got != want {
			t.Errorf("%s: provider %s, want %s", host, got, want)
		}
	}
}

func TestParseListing(t *testing.T) {
	in := "2024/a.jpg,12,2026-09-29T10:00:00.5Z,0cc175b9c0f1b6a831c399e269772661\n" +
		`"x,y.png",3,2026-09-29T10:00:00Z,` + "\n" +
		"big.mp4,999,2026-09-29T10:00:00Z,abc-12\n"
	objs, err := parseListing(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if o := objs["2024/a.jpg"]; o.Size != 12 || o.MD5 != "0cc175b9c0f1b6a831c399e269772661" ||
		!o.Modified.Equal(time.Date(2026, 9, 29, 10, 0, 0, 5e8, time.UTC)) {
		t.Errorf("%+v", o)
	}
	if o := objs["x,y.png"]; o.Size != 3 || o.MD5 != "" {
		t.Errorf("%+v", o)
	}
	if o := objs["big.mp4"]; o.MD5 != "" {
		t.Error("a multipart ETag is no MD5")
	}
	if _, err := parseListing(strings.NewReader("a,notanumber,x,y\n")); err == nil {
		t.Error("garbage accepted")
	}
}

// TestRcloneAgainstMinIO runs every command for real. Needs Docker:
// WPGENIE_TEST_DOCKER=1.
func TestRcloneAgainstMinIO(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run rclone against MinIO via Docker")
	}
	m := offloadtest.Start(t, "s1/uploads/")
	r := &Rclone{Docker: &runtime.Docker{}, Image: "rclone/rclone:1.75.1", Network: m.Network, User: offloadtest.RunAs()}
	tg := Target{Endpoint: m.Endpoint, Bucket: m.Bucket, Prefix: "s1/uploads/", AccessKeyID: m.AccessKey, SecretKey: m.SecretKey}
	ctx := context.Background()

	src := t.TempDir()
	mtime := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	for name, body := range map[string]string{"2024/01/a.jpg": "jpeg", "2024/01/sp ace é.png": "png!", "doc.pdf": "%PDF"} {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		os.Chtimes(p, mtime, mtime)
	}
	st, err := r.Upload(ctx, tg, src)
	if err != nil {
		t.Fatal(err)
	}
	if st.Objects != 3 || st.Bytes != 12 {
		t.Errorf("upload stats %+v", st)
	}
	if code, body := m.Get(t, "s1/uploads/2024/01/sp%20ace%20%C3%A9.png"); code != 200 || body != "png!" {
		t.Errorf("public read: %d %q", code, body)
	}

	objs, err := r.List(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	if o := objs["2024/01/a.jpg"]; len(objs) != 3 || o.Size != 4 || o.MD5 != fmt.Sprintf("%x", md5.Sum([]byte("jpeg"))) ||
		time.Since(o.Modified) > time.Hour {
		t.Errorf("listing %+v", objs)
	}

	if err := r.Put(ctx, tg, "probe.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if code, body := m.Get(t, "s1/uploads/probe.txt"); code != 200 || body != "hello" {
		t.Errorf("probe: %d %q", code, body)
	}
	if err := r.DeleteFile(ctx, tg, "probe.txt"); err != nil {
		t.Fatal(err)
	}
	if code, _ := m.Get(t, "s1/uploads/probe.txt"); code != 404 {
		t.Errorf("probe still there: %d", code)
	}

	st, err = r.Delete(ctx, tg, []string{"2024/01/a.jpg", "never/existed.jpg"})
	if err != nil || st.Deletes != 1 {
		t.Fatalf("delete: %+v %v", st, err)
	}
	if code, _ := m.Get(t, "s1/uploads/2024/01/a.jpg"); code != 404 {
		t.Error("deleted object still served")
	}

	dst := t.TempDir()
	if _, err := r.Download(ctx, tg, []string{"2024/01/sp ace é.png"}, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "2024", "01", "sp ace é.png"))
	if err != nil || !fi.ModTime().Equal(mtime) {
		t.Errorf("download: %v %v (want the original mtime back)", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "doc.pdf")); err == nil {
		t.Error("downloaded more than asked for")
	}

	bad := tg
	bad.SecretKey = "wrong-secret-key"
	err = r.Put(ctx, bad, "probe.txt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("wrong key: %v", err)
	}
}

// Addressing is rclone's per-provider default unless the target says.
func TestForcePathStyle(t *testing.T) {
	tg := Target{Endpoint: "https://s3.eu-central-1.amazonaws.com", Bucket: "b", AccessKeyID: "AKIAX", SecretKey: "s"}
	has := func(f string) bool { return slices.Contains(tg.Flags(), f) }
	if has("--s3-force-path-style=true") || has("--s3-force-path-style=false") {
		t.Error("a flag without ForcePathStyle")
	}
	for _, v := range []bool{true, false} {
		tg.ForcePathStyle = &v
		if !has("--s3-force-path-style=" + strconv.FormatBool(v)) {
			t.Errorf("ForcePathStyle %v: %q", v, tg.Flags())
		}
	}
}
