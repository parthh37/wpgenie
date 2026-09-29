package site

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/backup"
	"github.com/parthh37/wpgenie/internal/store"
)

func TestBackupDue(t *testing.T) {
	now := time.Date(2026, 9, 29, 3, 10, 0, 0, time.Local)
	h := func(n float64) time.Time { return now.Add(-time.Duration(n * float64(time.Hour))) }
	cases := []struct {
		name   string
		p      store.BackupPolicy
		window bool
		want   bool
	}{
		{"manual only", store.BackupPolicy{IntervalHours: 0}, true, false},
		{"hourly, due", store.BackupPolicy{IntervalHours: 1, LastBackupAt: h(1.1)}, false, true},
		{"hourly, not yet", store.BackupPolicy{IntervalHours: 1, LastBackupAt: h(0.5)}, false, false},
		{"daily, never backed up: at once", store.BackupPolicy{IntervalHours: 24}, false, true},
		{"daily, first attempt failed: wait for the retry", store.BackupPolicy{IntervalHours: 24, LastAttemptAt: h(0.2), LastError: "x"}, true, false},
		{"daily, in the window", store.BackupPolicy{IntervalHours: 24, LastBackupAt: h(23)}, true, true},
		{"daily, outside the window", store.BackupPolicy{IntervalHours: 24, LastBackupAt: h(23)}, false, false},
		{"daily, window missed for a day", store.BackupPolicy{IntervalHours: 24, LastBackupAt: h(31)}, false, true},
		{"daily, already done tonight", store.BackupPolicy{IntervalHours: 24, LastBackupAt: h(0.1)}, true, false},
		{"failed an hour ago: retry", store.BackupPolicy{IntervalHours: 6, LastBackupAt: h(7), LastAttemptAt: h(1.2), LastError: "x"}, false, true},
		{"failed just now: wait", store.BackupPolicy{IntervalHours: 6, LastBackupAt: h(7), LastAttemptAt: h(0.3), LastError: "x"}, false, false},
		{"weekly", store.BackupPolicy{IntervalHours: 168, LastBackupAt: h(166)}, true, true},
	}
	for _, c := range cases {
		if got := backupDue(&c.p, now, c.window); got != c.want {
			t.Errorf("%s: due=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestSearchReplaceArgs(t *testing.T) {
	args := searchReplaceArgs("a.example.com", "staging.a.example.com", nil, false)
	if !slices.Equal(args[:3], []string{"wp", "--skip-themes", "--skip-plugins"}) {
		t.Fatalf("WP-CLI must never load plugins: %v", args)
	}
	if args[4] != `(//|\\/\\/)a\.example\.com(?![A-Za-z0-9.-])` || args[5] != "${1}staging.a.example.com" {
		t.Errorf("pattern %q replacement %q", args[4], args[5])
	}
	for _, want := range []string{"--regex", "--skip-columns=guid", "--all-tables"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s: %v", want, args)
		}
	}
	args = searchReplaceArgs("s.test", "a.test", []string{"wp_posts"}, true)
	if slices.Contains(args, "--all-tables") || !slices.Contains(args, "wp_posts") || !slices.Contains(args, "--export") {
		t.Errorf("table push: %v", args)
	}
}

func TestInstallTarExcludes(t *testing.T) {
	code := strings.Join(installTar("/srv/s1/public", false), " ")
	all := strings.Join(installTar("/srv/s1/public", true), " ")
	for _, want := range []string{"--exclude=./wp-content/cache", "--exclude=./.maintenance", "--exclude=./" + restoreTmp} {
		if !strings.Contains(code, want) || !strings.Contains(all, want) {
			t.Errorf("missing %s", want)
		}
	}
	if !strings.Contains(code, "--exclude=./wp-content/uploads") || strings.Contains(all, "uploads") {
		t.Error("uploads: excluded from code copies only")
	}
}

func TestPipeReportsTheProducerFirst(t *testing.T) {
	boom := errors.New("source container died")
	err := pipe(func(w io.Writer) error {
		w.Write([]byte("partial"))
		return boom
	}, func(r io.Reader) error {
		_, err := io.ReadAll(r)
		return err
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	// A consumer that stops early must not leave the producer stuck.
	done := make(chan error)
	go func() {
		done <- pipe(func(w io.Writer) error {
			for {
				if _, err := w.Write(make([]byte, 1<<16)); err != nil {
					return err
				}
			}
		}, func(io.Reader) error { return errors.New("tar: bad header") })
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "tar: bad header") {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked after the consumer failed")
	}
	var got bytes.Buffer
	if err := pipe(func(w io.Writer) error { _, err := io.WriteString(w, "ok"); return err },
		func(r io.Reader) error { _, err := io.Copy(&got, r); return err }); err != nil || got.String() != "ok" {
		t.Fatal(err, got.String())
	}
}

func selfSigned(t *testing.T, names []string, notAfter time.Time) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func TestParseCert(t *testing.T) {
	now := time.Now()
	cert, key := selfSigned(t, []string{"example.com", "*.example.com"}, now.Add(90*24*time.Hour))
	c, err := parseCert(CertInput{cert, key}, []string{"example.com", "www.example.com"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Trusted || len(c.Names) != 2 {
		t.Errorf("%+v (a self-signed certificate isn't publicly trusted)", c)
	}
	if _, err := parseCert(CertInput{cert, key}, []string{"example.com", "shop.test"}, now); err == nil ||
		!strings.Contains(err.Error(), "shop.test") {
		t.Errorf("uncovered domain: %v", err)
	}
	_, otherKey := selfSigned(t, []string{"example.com"}, now.Add(time.Hour))
	if _, err := parseCert(CertInput{cert, otherKey}, []string{"example.com"}, now); err == nil {
		t.Error("a key that doesn't match the certificate was accepted")
	}
	if _, err := parseCert(CertInput{cert, key}, []string{"example.com"}, now.Add(100*24*time.Hour)); err == nil {
		t.Error("an expired certificate was accepted")
	}
	leaf, _ := firstCert([]byte(cert))
	if certCovers(leaf, []string{"a.b.example.com"}) == nil {
		t.Error("a wildcard covers one level only")
	}
}

func TestPHPSettings(t *testing.T) {
	h := newHarness(t)
	st, _ := h.svc.Store.GetSite(context.Background(), "s1")
	for _, in := range []PHPInput{
		{Version: "7.4"},
		{Version: "8.3", Settings: store.PHPSettings{MemoryLimitMB: 4096}}, // above the replica's 512 MB
		{Version: "8.3", Settings: store.PHPSettings{MaxExecutionTime: 5}},
		{Version: "8.3", Settings: store.PHPSettings{MaxInputVars: 10}},
	} {
		if err := h.svc.validatePHP(st, in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	if err := h.svc.validatePHP(st, PHPInput{Version: "8.4", Settings: store.PHPSettings{MemoryLimitMB: 384}}); err != nil {
		t.Fatal(err)
	}
	env := phpEnv(store.PHPSettings{MemoryLimitMB: 384, UploadMaxMB: 256, MaxExecutionTime: 300})
	for _, want := range []string{"WPG_MEMORY_LIMIT=384M", "WPG_UPLOAD_MAX=256M", "WPG_POST_MAX=264M",
		"WPG_MAX_EXECUTION_TIME=300", "WPG_REQUEST_TIMEOUT=330s"} {
		if !slices.Contains(env, want) {
			t.Errorf("env %v lacks %s", env, want)
		}
	}
	if phpEnv(store.PHPSettings{}) != nil {
		t.Error("default settings must add no variables (existing sites' replicas keep their spec)")
	}
	if h.svc.Cfg.PHPImageFor("8.4") != "wpgenie/php:8.4" || h.svc.Cfg.PHPImageFor("8.3") != h.svc.Cfg.PHPImage {
		t.Error("image per version")
	}
}

func TestPHPChangeRollsReplicasAndBackOnFailure(t *testing.T) {
	u := newUpdateHarness(t, true)
	ctx := context.Background()
	// The new PHP version breaks the site.
	u.rt.imageID = "sha256:v1"
	h := u.harness
	probe := &switchProbe{ok: true}
	h.svc.Prober = probe
	h.rt.onStart = func(image string) { probe.ok = !strings.HasSuffix(image, ":8.4") }
	if err := h.svc.changePHP(ctx, "s1", PHPInput{Version: "8.4"}, testTask(h)); err == nil ||
		!strings.Contains(err.Error(), "Switched back to PHP 8.3") {
		t.Fatalf("err %v", err)
	}
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	if st.PHPVersion != "8.3" {
		t.Fatalf("version %s after a failed switch", st.PHPVersion)
	}
	// Settings only: no health gate, replicas rolled with the new env.
	if err := h.svc.changePHP(ctx, "s1", PHPInput{Version: "8.3", Settings: store.PHPSettings{MemoryLimitMB: 384}}, testTask(h)); err != nil {
		t.Fatal(err)
	}
	st, _ = h.svc.Store.GetSite(ctx, "s1")
	if st.PHP.MemoryLimitMB != 384 || !slices.Contains(h.rt.lastSpec.PHPEnv, "WPG_MEMORY_LIMIT=384M") {
		t.Fatalf("settings %+v, env %v", st.PHP, h.rt.lastSpec.PHPEnv)
	}
}

type switchProbe struct{ ok bool }

func (p *switchProbe) Probe(context.Context, string) Health {
	if p.ok {
		return Health{Checked: true, OK: true}
	}
	return Health{Checked: true, Detail: "critical error"}
}

func TestDomains(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, err := h.svc.AddDomain(ctx, "s1", "WWW.A.test", true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.RedirectDomains, []string{"www.a.test"}) {
		t.Fatalf("redirects %v", st.RedirectDomains)
	}
	last := h.proxy.last[0]
	if last.Domains[0] != "a.test" || !slices.Equal(last.Redirects, []string{"www.a.test"}) {
		t.Fatalf("proxy got %+v", last)
	}
	if _, err := h.svc.AddDomain(ctx, "s1", "www.a.test", false); !errors.Is(err, ErrDomainTaken) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := h.svc.RemoveDomain(ctx, "s1", "a.test"); err == nil {
		t.Error("removed the primary domain")
	}
	h.proxy.rejects = 1
	if _, err := h.svc.AddDomain(ctx, "s1", "shop.test", false); err == nil {
		t.Fatal("proxy refused but the domain was added")
	}
	if taken, _ := h.svc.Store.DomainExists(ctx, "shop.test"); taken {
		t.Error("a domain Caddy refused stayed attached")
	}
	if _, err := h.svc.StartPrimaryDomain(ctx, "s1", "other.test"); err == nil {
		t.Error("made an unattached domain primary")
	}
	st, err = h.svc.RemoveDomain(ctx, "s1", "www.a.test")
	if err != nil || len(st.RedirectDomains) != 0 {
		t.Fatalf("%v %v", st, err)
	}
}

func TestBackupPolicyValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.Backups = &nilEngine{}
	for _, in := range []PolicyInput{
		{RepoID: LocalRepoID, IntervalHours: 5, KeepDaily: 7},
		{RepoID: LocalRepoID, IntervalHours: 24}, // keeps nothing: scheduled backups would pile up
		{RepoID: "nope", IntervalHours: 24, KeepDaily: 7},
		{RepoID: LocalRepoID, IntervalHours: 24, KeepDaily: -1},
	} {
		if _, err := h.svc.SetBackupPolicy(ctx, "s1", in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	p, err := h.svc.SetBackupPolicy(ctx, "s1", PolicyInput{RepoID: LocalRepoID, IntervalHours: 24, KeepDaily: 7, KeepWeekly: 4})
	if err != nil || p.KeepWeekly != 4 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := h.svc.SetBackupPolicy(ctx, "s1", PolicyInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Store.BackupPolicy(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Error("policy not removed")
	}
	if err := h.svc.localPathOK(h.svc.Cfg.SitesDir() + "/x"); err == nil {
		t.Error("a repository inside the sites directory would be readable by sites")
	}
}

func TestDeleteRefusesLiveSiteWithStaging(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.Store.CreateSite(ctx, &store.Site{ID: "s2", Name: "stg", PrimaryDomain: "staging.a.test", PHPVersion: "8.3",
		FPMPort: 19001, DBName: "wp_s2", Status: store.StatusActive, ParentID: "s1", MemoryMB: 512, CPUs: 1, Replicas: 1})
	if err := h.svc.Delete(ctx, "s1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v", err)
	}
	if _, _, err := h.svc.StartStaging(ctx, "s2", StagingInput{}); err == nil {
		t.Error("staged a staging site")
	}
	if _, err := h.svc.StartPush(ctx, "s1", PushInput{Database: true}); err == nil {
		t.Error("pushed from a live site")
	}
	if _, err := h.svc.StartPush(ctx, "s2", PushInput{}); err == nil {
		t.Error("pushed nothing")
	}
}

type fakeTask struct{ result any }

func (*fakeTask) Progress(int, string) {}
func (f *fakeTask) SetResult(v any)    { f.result = v }

func testTask(*harness) *fakeTask { return &fakeTask{} }

// nilEngine is restic that succeeds without doing anything.
type nilEngine struct{}

func (nilEngine) Init(context.Context, *store.BackupRepo) error { return nil }
func (nilEngine) Backup(context.Context, *store.BackupRepo, backup.BackupInput) (string, error) {
	return "0123456789abcdef", nil
}
func (nilEngine) Snapshots(context.Context, *store.BackupRepo, ...string) ([]backup.Snapshot, error) {
	return nil, nil
}
func (nilEngine) Dump(context.Context, *store.BackupRepo, string, string, io.Writer) error {
	return nil
}
func (nilEngine) Apply(context.Context, *store.BackupRepo, []string, backup.Keep) error { return nil }
func (nilEngine) Forget(context.Context, *store.BackupRepo, []string) error             { return nil }
func (nilEngine) Prune(context.Context, *store.BackupRepo) error                        { return nil }
func (nilEngine) Check(context.Context, *store.BackupRepo) error                        { return nil }
func (nilEngine) KeyScan(context.Context, string, int) (string, error)                  { return "", nil }
