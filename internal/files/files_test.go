package files

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/store"
)

type env struct {
	t       *testing.T
	svc     *Service
	st      *store.Store
	docroot string
	outside string // a directory next to the docroot: another site, the host
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	base := t.TempDir()
	e := &env{t: t, st: st, docroot: filepath.Join(base, "s1", "public"), outside: filepath.Join(base, "s2")}
	for _, d := range []string{e.docroot, e.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "s1", "wp-config.php"), []byte("<?php // secrets"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.outside, "secret.txt"), []byte("other site"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSite(context.Background(), &store.Site{ID: "s1", Name: "s1", PrimaryDomain: "s1.test",
		PHPVersion: "8.3", FPMPort: 19001, DBName: "wp_s1", Status: store.StatusActive, ShieldMode: "standard",
		MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	// The test's own user plays the site user.
	e.svc = &Service{Root: func(id string) string { return filepath.Join(base, id, "public") }, Store: st,
		UID: os.Getuid(), GID: os.Getgid()}
	return e
}

func (e *env) write(name, content string) {
	e.t.Helper()
	p := filepath.Join(e.docroot, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.docroot, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(e.docroot, name))
	return err == nil
}

var ctx = context.Background()

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"": ".", "/": ".", "/wp-content/": "wp-content", "a/../b": "b", "../../etc/passwd": "etc/passwd",
		"/./a//b/": "a/b", "..": ".",
	} {
		if got, err := Clean(in); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"a\x00b", strings.Repeat("x", 256), strings.Repeat("a/", 2100)} {
		if _, err := Clean(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Clean(%.20q): %v", bad, err)
		}
	}
}

func TestListSortsFoldersFirstAndShowsLinks(t *testing.T) {
	e := newEnv(t)
	e.write("b.txt", "b")
	e.write("A.php", "<?php")
	e.write("wp-content/index.php", "")
	os.Symlink("wp-content", filepath.Join(e.docroot, "link"))
	l, err := e.svc.List(ctx, "s1", "/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range l.Entries {
		names = append(names, x.Name+":"+x.Type)
	}
	if got := strings.Join(names, " "); got != "wp-content:dir A.php:file b.txt:file link:link" {
		t.Errorf("listing: %s", got)
	}
	if !l.Writable || l.Path != "/" {
		t.Errorf("listing: %+v", l)
	}
	if l.Entries[3].Target != "wp-content" || !l.Entries[1].Owned || l.Entries[1].Mode != "0644" {
		t.Errorf("entries: %+v", l.Entries)
	}
	if _, err := e.svc.List(ctx, "s1", "/b.txt"); !errors.Is(err, ErrInvalid) {
		t.Errorf("listing a file: %v", err)
	}
	if _, err := e.svc.List(ctx, "s1", "/nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("listing nothing: %v", err)
	}
	e.svc.Limits.List = 2
	if l, _ := e.svc.List(ctx, "s1", "/"); !l.Truncated || len(l.Entries) != 2 {
		t.Errorf("truncated listing: %+v", l)
	}
}

// Nothing reaches outside the docroot: not "..", not a link the site
// planted, not wp-config.php one level up.
func TestNothingEscapesTheDocroot(t *testing.T) {
	e := newEnv(t)
	os.Symlink(e.outside, filepath.Join(e.docroot, "escape"))
	os.Symlink(filepath.Join(e.outside, "secret.txt"), filepath.Join(e.docroot, "secret.txt"))
	os.Symlink("../wp-config.php", filepath.Join(e.docroot, "config.php"))

	if _, err := e.svc.List(ctx, "s1", "/escape"); err == nil {
		t.Error("listed a directory outside the docroot through a link")
	}
	if _, err := e.svc.List(ctx, "s1", "/../"); err != nil {
		t.Errorf(`".." is the docroot itself: %v`, err)
	}
	for _, p := range []string{"/secret.txt", "/config.php", "/escape/secret.txt", "/../wp-config.php"} {
		if txt, err := e.svc.Read(ctx, "s1", p); err == nil {
			t.Errorf("read %s: %q", p, txt.Content)
		}
		if f, _, err := e.svc.Download(ctx, "s1", p); err == nil {
			f.Close()
			t.Errorf("downloaded %s", p)
		}
	}
	for _, p := range []string{"/escape/new.php", "/escape/secret.txt"} {
		if _, err := e.svc.Write(ctx, "s1", p, strings.NewReader("pwned"), WriteOptions{Overwrite: true}); err == nil {
			t.Errorf("wrote %s", p)
		}
	}
	if err := e.svc.Mkdir(ctx, "s1", "/escape/dir"); err == nil {
		t.Error("made a directory through a link")
	}
	if err := e.svc.Chmod(ctx, "s1", "/secret.txt", 0o777); err == nil {
		t.Error("changed the mode of a link's target")
	}
	var buf bytes.Buffer
	if err := e.svc.Zip(ctx, "s1", "/escape", &buf); err == nil {
		t.Error("zipped a directory outside the docroot")
	}
	// Deleting the link removes the link, not what it points at.
	if err := e.svc.Delete(ctx, "s1", "/escape"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(e.outside, "secret.txt")); err != nil || string(b) != "other site" {
		t.Errorf("the other site's file: %q %v", b, err)
	}
}

func TestReadAndSaveWithVersions(t *testing.T) {
	e := newEnv(t)
	e.write("wp-content/themes/t/functions.php", "<?php\n// v1\n")
	os.Chmod(filepath.Join(e.docroot, "wp-content/themes/t/functions.php"), 0o640)
	txt, err := e.svc.Read(ctx, "s1", "wp-content/themes/t/functions.php")
	if err != nil {
		t.Fatal(err)
	}
	if txt.Content != "<?php\n// v1\n" || !txt.Writable || txt.Path != "/wp-content/themes/t/functions.php" {
		t.Fatalf("read: %+v", txt)
	}
	ent, err := e.svc.Write(ctx, "s1", txt.Path, strings.NewReader("<?php\n// v2\n"), WriteOptions{Version: txt.Version})
	if err != nil {
		t.Fatal(err)
	}
	if e.read("wp-content/themes/t/functions.php") != "<?php\n// v2\n" || ent.Mode != "0640" {
		t.Errorf("saved: %q, mode %s (kept?)", e.read("wp-content/themes/t/functions.php"), ent.Mode)
	}
	// The editor saves again with the version the save returned.
	if _, err := e.svc.Write(ctx, "s1", txt.Path, strings.NewReader("<?php\n// v3\n"), WriteOptions{Version: ent.Version}); err != nil {
		t.Errorf("second save: %v", err)
	}
	// A second save with the version that was read first: someone (this
	// save) changed the file meanwhile.
	if _, err := e.svc.Write(ctx, "s1", txt.Path, strings.NewReader("stale"), WriteOptions{Version: txt.Version}); !errors.Is(err, ErrConflict) {
		t.Errorf("stale save: %v", err)
	}
	os.Remove(filepath.Join(e.docroot, "wp-content/themes/t/functions.php"))
	if _, err := e.svc.Write(ctx, "s1", txt.Path, strings.NewReader("x"), WriteOptions{Version: txt.Version}); !errors.Is(err, ErrConflict) {
		t.Errorf("save of a deleted file: %v", err)
	}
	// No temporary files left behind.
	des, _ := os.ReadDir(filepath.Join(e.docroot, "wp-content/themes/t"))
	if len(des) != 0 {
		t.Errorf("left behind: %v", des)
	}
}

func TestReadRefusesBinaryAndHugeFiles(t *testing.T) {
	e := newEnv(t)
	e.write("logo.png", "\x89PNG\r\n\x1a\n\x00\x00")
	e.write("latin1.txt", "caf\xe9")
	e.write("big.txt", strings.Repeat("x", 2048))
	e.svc.Limits.Edit = 1024
	for _, p := range []string{"logo.png", "latin1.txt"} {
		if _, err := e.svc.Read(ctx, "s1", p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := e.svc.Read(ctx, "s1", "big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big: %v", err)
	}
	if _, err := e.svc.Read(ctx, "s1", "/"); !errors.Is(err, ErrInvalid) {
		t.Errorf("the docroot: %v", err)
	}
}

func TestUploads(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Write(ctx, "s1", "/a.txt", strings.NewReader("one"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(ctx, "s1", "/a.txt", strings.NewReader("two"), WriteOptions{}); !errors.Is(err, ErrConflict) {
		t.Errorf("replaced without overwrite: %v", err)
	}
	if _, err := e.svc.Write(ctx, "s1", "/a.txt", strings.NewReader("two"), WriteOptions{Overwrite: true}); err != nil || e.read("a.txt") != "two" {
		t.Errorf("overwrite: %v %q", err, e.read("a.txt"))
	}
	e.svc.Limits.Upload = 10
	if _, err := e.svc.Write(ctx, "s1", "/big.bin", strings.NewReader(strings.Repeat("x", 11)), WriteOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	if e.exists("big.bin") {
		t.Error("a refused upload was kept")
	}
	if _, err := e.svc.Write(ctx, "s1", "/missing/x.txt", strings.NewReader(""), WriteOptions{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("into a missing folder: %v", err)
	}
	e.write("dir/x", "")
	if _, err := e.svc.Write(ctx, "s1", "/dir", strings.NewReader(""), WriteOptions{Overwrite: true}); !errors.Is(err, ErrConflict) {
		t.Errorf("over a folder: %v", err)
	}
	if _, err := e.svc.Write(ctx, "s1", "/", strings.NewReader(""), WriteOptions{Overwrite: true}); !errors.Is(err, ErrInvalid) {
		t.Errorf("over the docroot: %v", err)
	}
}

// A hard link in the docroot to a file elsewhere: saving it replaces the
// link, and the other file stays as it was.
func TestSaveNeverWritesThroughAHardLink(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(e.outside, "secret.txt")
	if err := os.Link(other, filepath.Join(e.docroot, "hard.txt")); err != nil {
		t.Skip("no hard links here:", err)
	}
	if _, err := e.svc.Write(ctx, "s1", "/hard.txt", strings.NewReader("changed"), WriteOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(other); string(b) != "other site" {
		t.Errorf("the linked file was written: %q", b)
	}
	if e.read("hard.txt") != "changed" {
		t.Error("the docroot's copy wasn't replaced")
	}
}

// What the site doesn't own (WPGenie's drop-ins, root's in production) is
// read-only.
func TestFilesTheSiteDoesNotOwnAreReadOnly(t *testing.T) {
	e := newEnv(t)
	e.write("wp-content/object-cache.php", "<?php // managed")
	e.svc.UID = os.Getuid() + 1 // now nothing here is the site's
	if _, err := e.svc.Read(ctx, "s1", "wp-content/object-cache.php"); err != nil {
		t.Errorf("reading is fine: %v", err)
	}
	l, _ := e.svc.List(ctx, "s1", "/wp-content")
	if l.Writable || l.Entries[0].Owned {
		t.Errorf("listing: %+v", l)
	}
	checks := map[string]error{
		"write": func() error {
			_, err := e.svc.Write(ctx, "s1", "wp-content/object-cache.php", strings.NewReader("x"), WriteOptions{Overwrite: true})
			return err
		}(),
		"new": func() error {
			_, err := e.svc.Write(ctx, "s1", "wp-content/new.php", strings.NewReader("x"), WriteOptions{})
			return err
		}(),
		"delete": e.svc.Delete(ctx, "s1", "wp-content/object-cache.php"),
		"move":   e.svc.Move(ctx, "s1", "wp-content/object-cache.php", "x.php"),
		"chmod":  e.svc.Chmod(ctx, "s1", "wp-content/object-cache.php", 0o666),
		"mkdir":  e.svc.Mkdir(ctx, "s1", "wp-content/d"),
	}
	for op, err := range checks {
		if !errors.Is(err, ErrPermission) {
			t.Errorf("%s: %v", op, err)
		}
	}
	if e.read("wp-content/object-cache.php") != "<?php // managed" {
		t.Error("the managed file changed")
	}
}

func TestReadOnlyModeNeedsAChmodFirst(t *testing.T) {
	e := newEnv(t)
	e.write("locked.txt", "v1")
	os.Chmod(filepath.Join(e.docroot, "locked.txt"), 0o444)
	txt, _ := e.svc.Read(ctx, "s1", "locked.txt")
	if txt.Writable {
		t.Error("0444 shown as writable")
	}
	if _, err := e.svc.Write(ctx, "s1", "locked.txt", strings.NewReader("v2"), WriteOptions{Version: txt.Version}); !errors.Is(err, ErrPermission) {
		t.Errorf("wrote a 0444 file: %v", err)
	}
	if err := e.svc.Chmod(ctx, "s1", "locked.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(ctx, "s1", "locked.txt", strings.NewReader("v2"), WriteOptions{Overwrite: true}); err != nil {
		t.Error(err)
	}
}

func TestFoldersMovesCopiesDeletesModes(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Mkdir(ctx, "s1", "/new"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Mkdir(ctx, "s1", "/new"); !errors.Is(err, ErrConflict) {
		t.Errorf("mkdir twice: %v", err)
	}
	e.write("new/a.txt", "a")
	e.write("new/sub/b.txt", "b")
	os.Symlink("/etc/passwd", filepath.Join(e.docroot, "new/sub/link"))

	res, err := e.svc.Copy(ctx, "s1", "/new", "/copy")
	if err != nil {
		t.Fatal(err)
	}
	if *res != (Extracted{Files: 2, Folders: 2, Skipped: 1}) || e.read("copy/sub/b.txt") != "b" || e.exists("copy/sub/link") {
		t.Errorf("copy: %+v", res)
	}
	if _, err := e.svc.Copy(ctx, "s1", "/new", "/new/inside"); !errors.Is(err, ErrInvalid) {
		t.Errorf("copy into itself: %v", err)
	}
	if _, err := e.svc.Copy(ctx, "s1", "/new/a.txt", "/copy/a.txt"); !errors.Is(err, ErrConflict) {
		t.Errorf("copy over a file: %v", err)
	}
	e.svc.Limits.ExtractFiles = 1
	if _, err := e.svc.Copy(ctx, "s1", "/new", "/copy2"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("copy beyond the limit: %v", err)
	}
	e.svc.Limits.ExtractFiles = 0

	if err := e.svc.Move(ctx, "s1", "/new/a.txt", "/copy/a.txt"); !errors.Is(err, ErrConflict) {
		t.Errorf("move over a file: %v", err)
	}
	if err := e.svc.Move(ctx, "s1", "/new", "/new/sub/x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("move into itself: %v", err)
	}
	if err := e.svc.Move(ctx, "s1", "/new/a.txt", "/renamed.txt"); err != nil || e.read("renamed.txt") != "a" {
		t.Errorf("move: %v", err)
	}

	if err := e.svc.Chmod(ctx, "s1", "/renamed.txt", 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(e.docroot, "renamed.txt")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v", fi.Mode())
	}
	for _, m := range []fs.FileMode{0o4755, 0o1777} {
		if err := e.svc.Chmod(ctx, "s1", "/renamed.txt", m); !errors.Is(err, ErrInvalid) {
			t.Errorf("chmod %o: %v", m, err)
		}
	}
	if err := e.svc.Chmod(ctx, "s1", "/copy", 0o055); !errors.Is(err, ErrInvalid) {
		t.Errorf("a folder the site can't use: %v", err)
	}

	if err := e.svc.Delete(ctx, "s1", "/copy"); err != nil || e.exists("copy") {
		t.Errorf("delete a folder: %v", err)
	}
	if err := e.svc.Delete(ctx, "s1", "/"); !errors.Is(err, ErrInvalid) {
		t.Errorf("delete the docroot: %v", err)
	}
	if err := e.svc.Delete(ctx, "s1", "/nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete nothing: %v", err)
	}
}

func makeZip(t *testing.T, entries map[string]string, links ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	for _, l := range links {
		hdr := &zip.FileHeader{Name: l}
		hdr.SetMode(fs.ModeSymlink | 0o777)
		w, _ := zw.CreateHeader(hdr)
		io.WriteString(w, "/etc")
	}
	zw.Close()
	return buf.Bytes()
}

func (e *env) zip(name string, data []byte) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.docroot, name), data, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func TestExtract(t *testing.T) {
	e := newEnv(t)
	e.zip("plugin.zip", makeZip(t, map[string]string{"my-plugin/": "", "my-plugin/my-plugin.php": "<?php", "my-plugin/inc/a.php": "a"}, "my-plugin/evil"))
	res, err := e.svc.Extract(ctx, "s1", "/plugin.zip", "/wp-content/plugins", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Skipped != 1 || e.read("wp-content/plugins/my-plugin/inc/a.php") != "a" || e.exists("wp-content/plugins/my-plugin/evil") {
		t.Errorf("extracted: %+v", res)
	}
	// Again: the files exist; nothing is written without overwrite.
	e.write("wp-content/plugins/my-plugin/inc/a.php", "changed")
	if _, err := e.svc.Extract(ctx, "s1", "/plugin.zip", "/wp-content/plugins", false); !errors.Is(err, ErrConflict) {
		t.Errorf("extract over existing files: %v", err)
	}
	if e.read("wp-content/plugins/my-plugin/inc/a.php") != "changed" {
		t.Error("a refused extraction wrote something")
	}
	if _, err := e.svc.Extract(ctx, "s1", "/plugin.zip", "/wp-content/plugins", true); err != nil || e.read("wp-content/plugins/my-plugin/inc/a.php") != "a" {
		t.Errorf("overwrite: %v", err)
	}
}

func TestExtractRefusesUnsafeArchives(t *testing.T) {
	e := newEnv(t)
	for name, entries := range map[string]map[string]string{
		"slip.zip":     {"../../s2/pwned.php": "x"},
		"abs.zip":      {"/etc/cron.d/x": "x"},
		"windows.zip":  {`..\..\x.php`: "x"},
		"drive.zip":    {"C:/x.php": "x"},
		"dotdot.zip":   {"a/../../x.php": "x"},
		"harmless.zip": {"ok.txt": "ok"},
	} {
		e.zip(name, makeZip(t, entries))
		_, err := e.svc.Extract(ctx, "s1", name, "/", false)
		if name == "harmless.zip" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.outside, "pwned.php")); err == nil {
		t.Error("zip slip")
	}
	e.write("not.zip", "hello")
	if _, err := e.svc.Extract(ctx, "s1", "not.zip", "/", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("not a zip: %v", err)
	}
	// A link in the docroot leading outside: extracting "into" it fails.
	os.Symlink(e.outside, filepath.Join(e.docroot, "out"))
	if _, err := e.svc.Extract(ctx, "s1", "harmless.zip", "/out", true); err == nil {
		t.Error("extracted through a link")
	}
	if _, err := os.Stat(filepath.Join(e.outside, "ok.txt")); err == nil {
		t.Error("wrote outside the docroot")
	}
}

// A zip bomb: the archive is small, what it holds isn't. The declared size
// is checked first; the bytes actually written are counted as well.
func TestExtractLimits(t *testing.T) {
	e := newEnv(t)
	e.zip("bomb.zip", makeZip(t, map[string]string{"a": strings.Repeat("0", 4000), "b": strings.Repeat("0", 4000)}))
	e.svc.Limits.ExtractBytes = 5000
	if _, err := e.svc.Extract(ctx, "s1", "bomb.zip", "/x", false); !errors.Is(err, ErrTooLarge) {
		t.Errorf("declared size: %v", err)
	}
	if e.exists("x") {
		t.Error("wrote before the checks")
	}
	// An archive that understates its sizes.
	data := makeZip(t, map[string]string{"a": strings.Repeat("0", 4000), "b": strings.Repeat("0", 4000)})
	zr, _ := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	var lied bytes.Buffer
	zw := zip.NewWriter(&lied)
	for _, f := range zr.File {
		hdr := f.FileHeader
		raw, _ := f.OpenRaw()
		hdr.UncompressedSize64 = 10
		w, _ := zw.CreateRaw(&hdr)
		io.Copy(w, raw)
	}
	zw.Close()
	e.zip("liar.zip", lied.Bytes())
	if _, err := e.svc.Extract(ctx, "s1", "liar.zip", "/y", false); err == nil {
		t.Error("extracted more than the limit from a lying archive")
	}
	e.svc.Limits = Limits{ExtractFiles: 1}
	if _, err := e.svc.Extract(ctx, "s1", "bomb.zip", "/z", false); !errors.Is(err, ErrTooLarge) {
		t.Errorf("entry count: %v", err)
	}
}

func TestZipDownloadRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.write("wp-content/themes/t/style.css", "body{}")
	e.write("wp-content/themes/t/inc/x.php", "<?php")
	os.Symlink(e.outside, filepath.Join(e.docroot, "wp-content/themes/t/escape"))
	var buf bytes.Buffer
	if err := e.svc.Zip(ctx, "s1", "/wp-content/themes/t", &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, " "); got != "inc/ inc/x.php style.css" {
		t.Errorf("archive: %s", got)
	}
	if dir, _ := e.svc.IsDir(ctx, "s1", "/wp-content/themes/t"); !dir {
		t.Error("IsDir")
	}
}

func TestSiteStatusGatesChanges(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	write := func() error { return e.svc.Mkdir(ctx, "s1", "/d") }
	e.st.FreezeSite(ctx, "s1", true)
	if err := write(); !errors.Is(err, ErrConflict) {
		t.Errorf("frozen for a move: %v", err)
	}
	if _, err := e.svc.List(ctx, "s1", "/"); err != nil {
		t.Errorf("reading while frozen: %v", err)
	}
	e.st.FreezeSite(ctx, "s1", false)
	e.st.SetSiteStatus(ctx, "s1", store.StatusSuspended)
	if err := write(); !errors.Is(err, ErrConflict) {
		t.Errorf("suspended: %v", err)
	}
	e.st.SetSiteStatus(ctx, "s1", store.StatusMoved)
	if _, err := e.svc.List(ctx, "s1", "/"); !errors.Is(err, ErrConflict) {
		t.Errorf("reading a moved site: %v", err)
	}
	if _, err := e.svc.List(ctx, "nosuch", "/"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no site: %v", err)
	}
}
