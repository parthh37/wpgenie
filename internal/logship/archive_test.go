package logship

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseArchiveKey(t *testing.T) {
	good := "wpgenie/panel/access/2026/09/30/14-5b1c2d3e-aaaa-4bbb-8ccc-123456789abc.log.gz"
	k, err := parseArchiveKey("wpgenie/", good)
	if err != nil || k.Server != "panel" || k.Type != "access" || k.Name != "14-5b1c2d3e-aaaa-4bbb-8ccc-123456789abc.log.gz" ||
		!k.Day.Equal(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("parse %q = %+v, %v", good, k, err)
	}
	if k.dir() != "panel/access/2026/09/30/" {
		t.Errorf("dir = %q", k.dir())
	}
	for _, bad := range []string{
		"",
		"other/panel/access/2026/09/30/14-a.log.gz",           // outside the prefix
		"wpgenie/../secrets/access/2026/09/30/14-a.log.gz",    // escaping it
		"wpgenie/panel/access/2026/09/30/../../../../x",       // traversal in the name
		"wpgenie/panel/access/2026/09/30/..",                  // the parent itself
		"wpgenie/panel/access/2026/09/30/.hidden",             // not a name Vector writes
		"wpgenie/panel/access/2026/09/30/a/b.log.gz",          // deeper
		"wpgenie/panel/kernel/2026/09/30/14-a.log.gz",         // unknown type
		"wpgenie/Panel/access/2026/09/30/14-a.log.gz",         // not a server name
		"wpgenie/panel/access/2026/13/40/14-a.log.gz",         // not a date
		"wpgenie/panel/access/2026/09/30/14-a.log.gz\n",       // a line break
		"wpgenie/panel/access/2026/09/30/14-a.log.gz\x00.txt", // a NUL
		"wpgenie//panel/access/2026/09/30/14-a.log.gz",        // an empty segment
		"/wpgenie/panel/access/2026/09/30/14-a.log.gz",        // absolute
		"wpgenie/panel/access/2026/09/30/14..log.gz",          // dots
	} {
		if _, err := parseArchiveKey("wpgenie/", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted", bad)
		}
	}
	// An empty prefix: keys start at the server.
	if _, err := parseArchiveKey("", "web-2/jobs/2026/01/02/00-x.log.zst"); err != nil {
		t.Errorf("empty prefix: %v", err)
	}
}

func gz(t *testing.T, s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestArchives(t *testing.T) {
	ctx := context.Background()
	s, _, fs := newService(t)
	if _, err := s.Archives(ctx, "panel", TypeAccess, time.Now()); !errors.Is(err, ErrInvalid) {
		t.Errorf("no destination: %v", err)
	}
	enable(t, s, nil)
	base := "wpgenie/panel/access/2026/09/30/"
	fs.objects[base+"14-b.log.gz"] = gz(t, "{\"n\":2}\n")
	fs.objects[base+"13-a.log.gz"] = gz(t, "{\"n\":1}\n")
	fs.objects["wpgenie/panel/access/2026/09/29/23-z.log.gz"] = gz(t, "x")
	fs.objects["wpgenie/web-2/access/2026/09/30/14-c.log.gz"] = gz(t, "x")
	fs.objects[base+"plain.log"] = []byte("{\"n\":3}\n")
	fs.objects[base+"z.log.zst"] = append(append([]byte{}, zstdMagic...), 1, 2, 3)

	list, err := s.Archives(ctx, "panel", TypeAccess, time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC))
	if err != nil || len(list) != 4 || list[0].Name != "13-a.log.gz" || list[0].Key != base+"13-a.log.gz" {
		t.Fatalf("Archives = %+v, %v", list, err)
	}
	for _, bad := range [][2]string{{"../x", TypeAccess}, {"panel", "kernel"}} {
		if _, err := s.Archives(ctx, bad[0], bad[1], time.Now()); !errors.Is(err, ErrInvalid) {
			t.Errorf("Archives(%q, %q) = %v", bad[0], bad[1], err)
		}
	}

	read := func(key string) (string, bool) {
		t.Helper()
		r, err := s.OpenArchive(ctx, key)
		if err != nil {
			t.Fatalf("OpenArchive(%s): %v", key, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return string(b), r.Compressed
	}
	if got, _ := read(base + "14-b.log.gz"); got != "{\"n\":2}\n" {
		t.Errorf("gzip: %q", got)
	}
	if got, _ := read(base + "plain.log"); got != "{\"n\":3}\n" {
		t.Errorf("plain: %q", got)
	}
	if _, compressed := read(base + "z.log.zst"); !compressed {
		t.Error("zstd not reported as compressed")
	}
	if _, err := s.OpenArchive(ctx, base+"missing.log.gz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := s.OpenArchive(ctx, "elsewhere/secret.txt"); !errors.Is(err, ErrInvalid) {
		t.Errorf("outside the archive: %v", err)
	}
	// Nothing is left behind in the temporary directory.
	if left, _ := os.ReadDir(filepath.Join(s.Cfg.Dir, "tmp")); len(left) != 0 {
		t.Errorf("left in tmp: %v", left)
	}
}

// Retention deletes whole days older than the setting, of every server,
// and nothing that isn't an archive.
func TestTrimArchive(t *testing.T) {
	ctx := context.Background()
	s, _, fs := newService(t)
	set := enable(t, s, func(set *Settings) { set.ArchiveRetentionDays = 30 })
	s.Now = func() time.Time { return time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC) }
	keep := []string{
		"wpgenie/panel/access/2026/08/31/00-a.log.gz", // exactly 30 days: kept
		"wpgenie/web-2/jobs/2026/09/30/01-b.log.gz",
		"wpgenie/panel/access/2025/01/01/notes.txt.bak~", // not an archive's name
		"wpgenie/README.txt",
		"elsewhere/panel/access/2020/01/01/00-x.log.gz", // outside the prefix
	}
	drop := []string{
		"wpgenie/panel/access/2026/08/30/23-c.log.gz",
		"wpgenie/web-2/security/2025/12/31/10-d.log.zst",
	}
	for _, k := range append(append([]string{}, keep...), drop...) {
		fs.objects[k] = []byte("x")
	}
	n, err := s.trimArchive(ctx, set)
	if err != nil || n != 2 {
		t.Fatalf("trimArchive = %d, %v", n, err)
	}
	for _, k := range keep {
		if _, ok := fs.objects[k]; !ok {
			t.Errorf("%s deleted", k)
		}
	}
	for _, k := range drop {
		if _, ok := fs.objects[k]; ok {
			t.Errorf("%s kept", k)
		}
	}
	set.ArchiveRetentionDays = 0
	fs.objects[drop[0]] = []byte("x")
	if n, _ := s.trimArchive(ctx, set); n != 0 {
		t.Error("0 days deleted something")
	}
}

func TestConnectionTest(t *testing.T) {
	ctx := context.Background()
	s, _, fs := newService(t)
	res, err := s.Test(ctx, testDestination())
	if err != nil || !res.OK {
		t.Fatalf("Test = %+v, %v", res, err)
	}
	if len(fs.objects) != 0 {
		t.Errorf("test object left: %v", fs.objects)
	}
	fs.putErr = errors.New("rclone rcat: 403 AccessDenied: Access Denied.")
	res, _ = s.Test(ctx, testDestination())
	if res.OK || res.Step != "write" || res.Error != "Couldn't write a test file to the bucket: 403 AccessDenied: Access Denied." {
		t.Errorf("refused write: %+v", res)
	}
	fs.putErr, fs.delErr = nil, errors.New("rclone deletefile: 403 AccessDenied")
	if res, _ = s.Test(ctx, testDestination()); res.OK || res.Step != "delete" {
		t.Errorf("refused delete: %+v", res)
	}
	// The stored secret is reused only for the stored destination.
	fs.delErr = nil
	enable(t, s, nil)
	d := testDestination()
	d.SecretKey = ""
	if res, err := s.Test(ctx, d); err != nil || !res.OK {
		t.Errorf("stored secret: %+v %v", res, err)
	}
	d.Endpoint = "https://attacker.example.net"
	if _, err := s.Test(ctx, d); !errors.Is(err, ErrInvalid) {
		t.Errorf("stored secret sent elsewhere: %v", err)
	}
}
