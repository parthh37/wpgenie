package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

func TestTargetKeepsSecretsOutOfArgs(t *testing.T) {
	r := &Restic{Image: "restic/restic:test", CacheDir: "/var/lib/wpgenie/backups/cache"}
	repos := []*store.BackupRepo{
		{Kind: KindLocal, Location: "/var/lib/wpgenie/backups/local", Password: "pw-local"},
		{Kind: KindS3, Location: "s3:https://s3.example.com/bucket/wp", Password: "pw-s3",
			Secrets: store.RepoSecrets{AccessKeyID: "AKIA1", SecretAccessKey: "s3cret", Region: "eu-west-1"}},
		{Kind: KindB2, Location: "b2:bucket:wp", Password: "pw-b2",
			Secrets: store.RepoSecrets{AccessKeyID: "k1", SecretAccessKey: "b2secret"}},
		{Kind: KindSFTP, Location: SFTPLocation("backup", "nas.example.com", 2222, "/srv/restic"), Password: "pw-sftp",
			Secrets: store.RepoSecrets{SSHPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n",
				KnownHosts: "[nas.example.com]:2222 ssh-ed25519 AAAA"}},
	}
	for _, repo := range repos {
		tg, err := targetFor(repo)
		if err != nil {
			t.Fatalf("%s: %v", repo.Kind, err)
		}
		args := strings.Join(r.dockerArgs("n", tg, nil, []string{"snapshots"}), " ")
		for _, secret := range []string{repo.Password, repo.Secrets.SecretAccessKey, "BEGIN OPENSSH"} {
			if secret != "" && strings.Contains(args, secret) {
				t.Errorf("%s: secret %q in docker args: %s", repo.Kind, secret, args)
			}
		}
		env := strings.Join(tg.env, "\n")
		if !strings.Contains(env, "RESTIC_PASSWORD="+repo.Password) {
			t.Errorf("%s: password not in the stdin environment", repo.Kind)
		}
		wantNet := "bridge"
		if repo.Kind == KindLocal {
			wantNet = "none"
		}
		if tg.network != wantNet {
			t.Errorf("%s: network %s, want %s", repo.Kind, tg.network, wantNet)
		}
	}
	tg, _ := targetFor(repos[3])
	if cmd := strings.Join(tg.opts, " "); !strings.Contains(cmd, "-p 2222 backup@nas.example.com -s sftp") ||
		!strings.Contains(cmd, "StrictHostKeyChecking=yes") {
		t.Errorf("sftp command %q", cmd)
	}
	if _, err := targetFor(&store.BackupRepo{Kind: KindS3, Location: "s3:x/y", Password: "p",
		Secrets: store.RepoSecrets{SecretAccessKey: "a\nRESTIC_PASSWORD=evil"}}); err == nil {
		t.Error("a line break in a secret was accepted (it would inject environment variables)")
	}
}

func TestParseSFTP(t *testing.T) {
	user, host, port, path, err := ParseSFTP(SFTPLocation("u", "2001:db8::1", 22, "/srv/b"))
	if err != nil || user != "u" || host != "2001:db8::1" || port != 22 || path != "//srv/b" {
		t.Fatalf("%q %q %d %q %v", user, host, port, path, err)
	}
	for _, bad := range []string{"sftp://host//p", "sftp://u@h:0//p", "sftp://u;x@h//p", "s3://u@h//p", "sftp://u@h"} {
		if _, _, _, _, err := ParseSFTP(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPickHostKey(t *testing.T) {
	scan := "# nas:22 SSH-2.0-OpenSSH_9.6\nnas ssh-rsa AAAArsa\nnas ssh-ed25519 AAAAed\nnas ecdsa-sha2-nistp256 AAAAec\n"
	if k, err := pickHostKey(scan); err != nil || k != "nas ssh-ed25519 AAAAed" {
		t.Fatalf("%q %v", k, err)
	}
	if _, err := pickHostKey("# nothing\n"); err == nil {
		t.Fatal("no key accepted")
	}
}

func TestKeepArgs(t *testing.T) {
	got := Keep{Daily: 7, Monthly: 6, Within: "7d"}.args()
	want := []string{"--keep-daily", "7", "--keep-monthly", "6", "--keep-within", "7d"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

func dockerRestic(t *testing.T) *Restic {
	t.Helper()
	if os.Getenv("WPGENIE_TEST_DOCKER") == "" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run restic in Docker")
	}
	return &Restic{Docker: &runtime.Docker{}, Image: "restic/restic:0.18.1", CacheDir: writableDir(t)}
}

// writableDir is a temporary directory restic's container can write. It
// runs as root without DAC_OVERRIDE (on a server the repository and cache
// are root's), so a directory owned by a non-root test runner must be
// opened up.
func writableDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if os.Geteuid() != 0 {
		os.Chmod(d, 0o777)
		// restic leaves root-owned files the test can't remove itself; this
		// runs before TempDir's own cleanup (cleanups run last-in first-out).
		t.Cleanup(func() {
			exec.Command("docker", "run", "--rm", "-v", d+":/d", "--entrypoint", "sh", "restic/restic:0.18.1",
				"-c", "rm -rf /d/* /d/.[!.]*").Run()
		})
	}
	return d
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResticRoundTrip(t *testing.T) {
	r := dockerRestic(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repo := &store.BackupRepo{ID: "local", Kind: KindLocal, Location: writableDir(t), Password: "correct horse"}
	if err := r.Init(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := r.Init(ctx, repo); err != nil { // idempotent
		t.Fatalf("second init: %v", err)
	}
	site, db := t.TempDir(), t.TempDir()
	writeTree(t, site, map[string]string{"index.php": "<?php // v1", "wp-content/uploads/a.jpg": "jpeg",
		"wp-content/cache/wpgenie/index.html": "cached page"})
	os.Symlink("/etc/shadow", filepath.Join(site, "evil"))
	writeTree(t, db, map[string]string{DumpFile: "CREATE TABLE wp_posts (id int);", MetaFile: `{"site_id":"s1"}`})

	var progress []int
	id, err := r.Backup(ctx, repo, BackupInput{SiteID: "s1", Files: site, DB: db, Tags: []string{"manual"},
		Exclude: []string{"wp-content/cache"}, Progress: func(p int) { progress = append(progress, p) }})
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.Snapshots(ctx, repo, "wpgenie", "site=s1")
	if err != nil || len(snaps) != 1 || !strings.HasPrefix(id, snaps[0].ID[:8]) || !snaps[0].HasTag("manual") ||
		snaps[0].Tag("site") != "s1" {
		t.Fatalf("snapshots %+v, %v (backup %s)", snaps, err, id)
	}
	if other, _ := r.Snapshots(ctx, repo, "wpgenie", "site=s2"); len(other) != 0 {
		t.Fatalf("site filter leaked: %+v", other)
	}

	var dump bytes.Buffer
	if err := r.Dump(ctx, repo, id, DBPath+"/"+DumpFile, &dump); err != nil {
		t.Fatal(err)
	}
	if dump.String() != "CREATE TABLE wp_posts (id int);" { // a file comes out as is
		t.Fatalf("database dump %q", dump.String())
	}
	var files bytes.Buffer
	if err := r.Dump(ctx, repo, id, FilesPath, &files); err != nil {
		t.Fatal(err)
	}
	got := tarFiles(t, &files)
	if got["backup/files/index.php"] != "<?php // v1" || got["backup/files/evil"] != "-> /etc/shadow" {
		t.Fatalf("files %v", got)
	}
	if _, ok := got["backup/files/wp-content/cache/wpgenie/index.html"]; ok {
		t.Fatal("page cache was backed up")
	}

	// Retention: two scheduled backups, keep the last one; the manual one
	// isn't touched by a policy for scheduled backups.
	for range 2 {
		if _, err := r.Backup(ctx, repo, BackupInput{SiteID: "s1", Files: site, DB: db, Tags: []string{"scheduled"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Apply(ctx, repo, []string{"site=s1", "scheduled"}, Keep{Last: 1}); err != nil {
		t.Fatal(err)
	}
	snaps, _ = r.Snapshots(ctx, repo, "site=s1")
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots after retention, want 2 (1 manual + 1 scheduled)", len(snaps))
	}
	if err := r.Forget(ctx, repo, []string{id}); err != nil {
		t.Fatal(err)
	}
	if err := r.Prune(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(ctx, repo); err != nil {
		t.Fatal(err)
	}
	wrong := *repo
	wrong.Password = "nope"
	if err := r.Init(ctx, &wrong); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
}

func tarFiles(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		switch h.Typeflag {
		case tar.TypeReg:
			b, _ := io.ReadAll(tr)
			out[h.Name] = string(b)
		case tar.TypeSymlink:
			out[h.Name] = "-> " + h.Linkname
		}
	}
}
