package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		release, current string
		want             bool
	}{
		{"v0.3.0", "v0.2.9", true},
		{"v0.10.0", "v0.9.0", true}, // numeric, not lexical
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "v1.0.1", false}, // never downgrade
		{"v1.0.0", "v1.0.0-rc.1", true},
		{"v1.0.0-rc.2", "v1.0.0", false},
		{"v2.0.0", "dev", false},                      // development builds don't self-update
		{"v2.0.0", "v0.1.0-12-gabc1234-dirty", false}, // git describe output isn't a release either
		{"latest", "v0.1.0", false},
		{"v9.0.0-../../etc", "v0.1.0", false}, // tags become paths
		{"v9.0.0-rc/1", "v0.1.0", false},
	} {
		if got := Newer(c.release, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.release, c.current, got, c.want)
		}
	}
}

type fakeRelease struct {
	priv    ed25519.PrivateKey
	files   map[string][]byte // asset name -> content
	tamper  func(map[string][]byte)
	version string
}

func newKey(t *testing.T) (ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(pub)
	return priv, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
}

func makeTarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: e.typeflag}
		if e.typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if e.typeflag == tar.TypeSymlink {
			h.Linkname, h.Size = "/etc/passwd", 0
		}
		if e.name == "wpgenie" {
			h.Mode = 0o755
		}
		tw.WriteHeader(h)
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

var goodEntries = []tarEntry{
	{name: "wpgenie", body: "#!new-binary"},
	{name: "LICENSE", body: "AGPL"},
	{name: "deploy/", typeflag: tar.TypeDir},
	{name: "deploy/docker-compose.yml", body: "services: {}"},
	{name: "deploy/wpgenie.service", body: "[Service]"},
	{name: "images/php/Dockerfile", body: "FROM x"},
}

// serve publishes a signed release the way the release workflow does.
func (f *fakeRelease) serve(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()
	name := "wpgenie_" + f.version + "_linux_" + goruntime.GOARCH + ".tar.gz"
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  other.tar.gz\n")
	f.files = map[string][]byte{name: archive, "checksums.txt": sums, "checksums.txt.sig": ed25519.Sign(f.priv, sums)}
	if f.tamper != nil {
		f.tamper(f.files)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/releases/latest" {
			var assets []map[string]string
			for n := range f.files {
				assets = append(assets, map[string]string{"name": n, "browser_download_url": srv.URL + "/dl/" + n})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": f.version, "body": "notes", "assets": assets})
			return
		}
		if b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/dl/")]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withKey(t *testing.T, pub []byte) {
	old := releasePub
	releasePub = pub
	t.Cleanup(func() { releasePub = old })
}

func newUpdater(t *testing.T, srv *httptest.Server, launched *string) *Updater {
	return &Updater{Current: "v0.1.0", Repo: "o/r", StateDir: t.TempDir(), APIBase: srv.URL,
		Launch: func(_ context.Context, staged string) error { *launched = staged; return nil }}
}

func TestUpdateStagesVerifiedRelease(t *testing.T) {
	priv, pub := newKey(t)
	withKey(t, pub)
	f := &fakeRelease{priv: priv, version: "v0.2.0"}
	srv := f.serve(t, makeTarball(t, goodEntries))
	var staged string
	u := newUpdater(t, srv, &staged)

	if !u.Info().Signed {
		t.Fatal("Info must report the signing key")
	}
	v, err := u.Update(context.Background())
	if err != nil || v != "v0.2.0" {
		t.Fatalf("Update = %q, %v", v, err)
	}
	b, _ := os.ReadFile(filepath.Join(staged, "wpgenie"))
	fi, _ := os.Stat(filepath.Join(staged, "wpgenie"))
	if string(b) != "#!new-binary" || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("staged binary %q mode %v", b, fi.Mode())
	}
	if st, _ := ReadStatus(u.StateDir); st == nil || st.Phase != PhaseStaged || st.To != "v0.2.0" {
		t.Errorf("status = %+v", st)
	}
	if in := u.Info(); !in.Available || in.Latest.Version != "v0.2.0" {
		t.Errorf("info = %+v", in)
	}
	if _, err := u.Update(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("second update while the first is staged: %v", err)
	}
}

func TestUpdateRefusesTamperedReleases(t *testing.T) {
	priv, pub := newKey(t)
	otherPriv, _ := newKey(t)
	cases := map[string]struct {
		tamper  func(map[string][]byte)
		entries []tarEntry
		want    string
	}{
		"archive swapped": {tamper: func(f map[string][]byte) {
			for n := range f {
				if strings.HasSuffix(n, ".tar.gz") {
					f[n] = makeTarball(t, append(slices.Clone(goodEntries), tarEntry{name: "images/evil", body: "x"}))
				}
			}
		}, want: "checksum mismatch"},
		"checksums rewritten to match": {tamper: func(f map[string][]byte) {
			f["checksums.txt"] = append(f["checksums.txt"], '\n')
		}, want: "signature"},
		"signed with another key": {tamper: func(f map[string][]byte) {
			f["checksums.txt.sig"] = ed25519.Sign(otherPriv, f["checksums.txt"])
		}, want: "signature"},
		"unsigned release": {tamper: func(f map[string][]byte) { delete(f, "checksums.txt.sig") }, want: "no checksums.txt.sig"},
		"symlink in archive": {entries: append(slices.Clone(goodEntries), tarEntry{name: "deploy/link", typeflag: tar.TypeSymlink}),
			want: "unsupported entry"},
		"path traversal": {entries: append(slices.Clone(goodEntries), tarEntry{name: "deploy/../../etc/cron.d/x", body: "x"}),
			want: "unsafe path"},
		"unexpected file": {entries: append(slices.Clone(goodEntries), tarEntry{name: "etc/passwd", body: "x"}),
			want: "unexpected entry"},
		"missing binary": {entries: goodEntries[1:], want: "missing wpgenie"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			withKey(t, pub)
			entries := c.entries
			if entries == nil {
				entries = goodEntries
			}
			f := &fakeRelease{priv: priv, version: "v0.2.0", tamper: c.tamper}
			srv := f.serve(t, makeTarball(t, entries))
			var launched string
			u := newUpdater(t, srv, &launched)
			_, err := u.Update(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Update = %v, want error containing %q", err, c.want)
			}
			if launched != "" {
				t.Fatal("launched the installer for a release that failed verification")
			}
			if entries, _ := os.ReadDir(u.StateDir); len(entries) != 1 { // only status.json
				t.Fatalf("left files behind: %v", entries)
			}
			if st, _ := ReadStatus(u.StateDir); st.Phase != PhaseFailed || st.Active() {
				t.Errorf("status = %+v", st)
			}
		})
	}
}

func TestUpdateNeedsKeyAndNewerRelease(t *testing.T) {
	withKey(t, nil)
	if _, err := (&Updater{}).Update(context.Background()); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("no key: %v", err)
	}
	priv, pub := newKey(t)
	withKey(t, pub)
	f := &fakeRelease{priv: priv, version: "v0.1.0"}
	srv := f.serve(t, makeTarball(t, goodEntries))
	var launched string
	if _, err := newUpdater(t, srv, &launched).Update(context.Background()); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("same version: %v", err)
	}
}

// applierHarness is an installation in temp dirs with fake commands.
type applierHarness struct {
	a       *Applier
	cmds    []string
	failCmd string // fail the first command containing this
	running string // version the "daemon" answers with after a restart
	next    string // version a restart brings up
}

func newApplier(t *testing.T) *applierHarness {
	t.Helper()
	root := t.TempDir()
	p := Paths{Bin: filepath.Join(root, "bin", "wpgenie"), Share: filepath.Join(root, "opt"),
		Unit: filepath.Join(root, "wpgenie.service"), EnvFile: filepath.Join(root, "infra.env"), StateDir: filepath.Join(root, "updates")}
	os.MkdirAll(filepath.Dir(p.Bin), 0o755)
	os.WriteFile(p.Bin, []byte("#!old-binary"), 0o755)
	os.MkdirAll(filepath.Join(p.Share, "deploy"), 0o755)
	os.WriteFile(filepath.Join(p.Share, "deploy", "docker-compose.yml"), []byte("old compose"), 0o644)
	os.WriteFile(p.Unit, []byte("old unit"), 0o644)

	staged := filepath.Join(root, "updates", "v0.2.0")
	for name, body := range map[string]string{
		"wpgenie": "#!new-binary", "VERSION": "v0.2.0", "deploy/docker-compose.yml": "new compose",
		"deploy/wpgenie.service": "new unit", "images/php/Dockerfile": "FROM new",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(staged, name)), 0o755)
		os.WriteFile(filepath.Join(staged, name), []byte(body), 0o755)
	}
	h := &applierHarness{running: "v0.1.0", next: "v0.2.0"}
	h.a = &Applier{Staged: staged, From: "v0.1.0", Paths: p, PHPImage: "wpgenie/php:8.3", HealthTimeout: 1,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			cmd := name + " " + strings.Join(args, " ")
			h.cmds = append(h.cmds, cmd)
			if h.failCmd != "" && strings.Contains(cmd, h.failCmd) {
				h.failCmd = ""
				return []byte("boom"), errors.New("exit status 1")
			}
			switch {
			case strings.HasPrefix(cmd, "docker image inspect"):
				return []byte("sha256:old\n"), nil
			case cmd == "systemctl restart wpgenie":
				// Restarting runs whatever binary is installed now.
				b, _ := os.ReadFile(p.Bin)
				h.running = map[string]string{"#!old-binary": "v0.1.0", "#!new-binary": h.next}[string(b)]
			}
			return nil, nil
		},
		Version: func(context.Context) (string, error) { return h.running, nil },
	}
	return h
}

func (h *applierHarness) read(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplySucceeds(t *testing.T) {
	h := newApplier(t)
	rolled := false
	h.a.RollSites = func(context.Context) error { rolled = true; return nil }
	if err := h.a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.a.Paths
	if h.read(t, p.Bin) != "#!new-binary" || h.read(t, filepath.Join(p.Share, "deploy", "docker-compose.yml")) != "new compose" ||
		h.read(t, p.Unit) != "new unit" || h.read(t, p.Bin+".prev") != "#!old-binary" {
		t.Fatal("new release not installed (or previous binary not kept)")
	}
	build := slices.IndexFunc(h.cmds, func(c string) bool { return strings.HasPrefix(c, "docker build") })
	restart := slices.Index(h.cmds, "systemctl restart wpgenie")
	if build < 0 || restart < build || !strings.Contains(h.cmds[build], "PHP_VERSION=8.3") {
		t.Fatalf("commands %v: the image must be built before anything restarts", h.cmds)
	}
	if st, _ := ReadStatus(p.StateDir); st.Phase != PhaseDone || !rolled {
		t.Fatalf("status %+v, rolled sites %v", st, rolled)
	}
}

func TestRollbackNeverUsesStaleCopies(t *testing.T) {
	h := newApplier(t)
	p := h.a.Paths
	// Left over from an update two versions ago.
	os.WriteFile(p.Bin+".prev", []byte("#!ancient-binary"), 0o755)
	os.WriteFile(p.Unit+".prev", []byte("ancient unit"), 0o644)
	h.next = "" // the new version won't start
	h.a.Apply(context.Background())
	if h.read(t, p.Bin) != "#!old-binary" || h.read(t, p.Unit) != "old unit" {
		t.Fatalf("rolled back to %q / %q, want the version that was running", h.read(t, p.Bin), h.read(t, p.Unit))
	}
}

func TestApplyBuildFailureChangesNothing(t *testing.T) {
	h := newApplier(t)
	h.failCmd = "docker build"
	if err := h.a.Apply(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if h.read(t, h.a.Paths.Bin) != "#!old-binary" || slices.Contains(h.cmds, "systemctl restart wpgenie") {
		t.Fatalf("a failed image build must not touch the installation: %v", h.cmds)
	}
	if st, _ := ReadStatus(h.a.Paths.StateDir); st.Phase != PhaseFailed || !strings.Contains(st.Message, "nothing was changed") {
		t.Fatalf("status %+v", st)
	}
}

func TestApplyRollsBackWhenNewVersionDoesNotStart(t *testing.T) {
	for name, setup := range map[string]func(*applierHarness){
		"new version crashes":  func(h *applierHarness) { h.next = "" },
		"compose fails":        func(h *applierHarness) { h.failCmd = "docker compose" },
		"answers old version?": func(h *applierHarness) { h.next = "v0.1.0" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newApplier(t)
			setup(h)
			err := h.a.Apply(context.Background())
			if err == nil {
				t.Fatal("expected failure")
			}
			p := h.a.Paths
			if h.read(t, p.Bin) != "#!old-binary" || h.read(t, filepath.Join(p.Share, "deploy", "docker-compose.yml")) != "old compose" ||
				h.read(t, p.Unit) != "old unit" {
				t.Fatal("previous installation not restored")
			}
			if !slices.Contains(h.cmds, "docker tag sha256:old wpgenie/php:8.3") {
				t.Errorf("previous PHP image not re-tagged: %v", h.cmds)
			}
			st, _ := ReadStatus(p.StateDir)
			if st.Phase != PhaseRolledBack || h.running != "v0.1.0" {
				t.Fatalf("status %+v, running %q", st, h.running)
			}
		})
	}
}
