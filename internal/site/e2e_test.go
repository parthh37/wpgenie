package site

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/parthh37/wpgenie/internal/backup"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/dbprov"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
)

type okProber struct{}

func (okProber) Probe(context.Context, string) Health { return Health{Checked: true, OK: true} }

// TestEnvironmentsEndToEnd runs Phase 2 against real WordPress, MariaDB
// and restic: create, back up, break, restore; stage, change staging, push;
// switch the primary domain; restore a backup as a new site; switch PHP.
// It needs the wpgenie/php:8.3 and :8.4 images (see newE2E).
func TestEnvironmentsEndToEnd(t *testing.T) {
	e := newE2E(t)
	ctx, svc, st, docker, raw, db := e.ctx, e.svc, e.st, e.docker, e.raw, e.db
	wp, sh, wait := e.wp, e.sh, e.wait
	titles := func(id string) []string {
		return strings.Split(wp(id, "post", "list", "--post_type=post", "--field=post_title"), "\n")
	}

	// Create (the job path, as the panel does).
	live, jobID, err := svc.StartCreate(jobs.WithOwner(jobs.WithActor(ctx, "tester"), "user:7"), CreateInput{Domain: "a.test", AdminEmail: "a@a.test"})
	wait(jobID, err)
	if creds, ok := svc.Jobs.Secret(jobID, "user:7"); !ok || creds.(*Credentials).Password == "" {
		t.Fatal("no credentials for the creator")
	}
	if _, ok := svc.Jobs.Secret(jobID, "user:8"); ok {
		t.Fatal("credentials visible to another user")
	}
	id := live.ID
	if p, err := st.BackupPolicy(ctx, id); err != nil || p.RepoID != LocalRepoID || p.IntervalHours != 24 {
		t.Fatalf("default backup policy %+v %v", p, err)
	}
	wp(id, "post", "create", "--post_title=Original", "--post_status=publish",
		`--post_content=<a href="https://a.test/about">About</a> and https://a.test.au stays`)
	sh(id, "mkdir -p wp-content/uploads/2026 && echo img > wp-content/uploads/2026/pic.jpg")

	// Back up, break, restore.
	j := wait(svc.StartBackup(ctx, id))
	if !strings.Contains(j.Result, `"kind":"manual"`) {
		t.Fatalf("backup result %s", j.Result)
	}
	list, errs, _ := svc.SiteBackups(ctx, id)
	if len(list) != 1 || len(errs) != 0 {
		t.Fatalf("backups %+v %v", list, errs)
	}
	first := list[0]
	wp(id, "post", "delete", wp(id, "post", "list", "--title=Original", "--field=ID"), "--force")
	sh(id, "echo '<?php // backdoor' > wp-content/plugins/evil.php")
	if _, err := raw.ExecContext(ctx, "CREATE TABLE wp_"+id+".wp_extra (id int)"); err != nil {
		t.Fatal(err)
	}
	wait(svc.StartRestore(ctx, id, RestoreInput{RepoID: first.RepoID, BackupID: first.ID, Files: true, Database: true}))
	if !slices.Contains(titles(id), "Original") {
		t.Fatal("restore didn't bring the post back")
	}
	if sh(id, "ls wp-content/plugins") == "" || strings.Contains(sh(id, "ls wp-content/plugins"), "evil.php") {
		t.Fatal("restore left a file the backup didn't have")
	}
	if sh(id, "cat wp-content/uploads/2026/pic.jpg") != "img" {
		t.Fatal("uploads not restored")
	}
	tables, _ := db.Tables(ctx, "wp_"+id)
	if slices.Contains(tables, "wp_extra") {
		t.Fatal("restore kept a table created after the backup")
	}
	list, _, _ = svc.SiteBackups(ctx, id)
	if len(list) != 2 || list[0].Kind != BackupSafety {
		t.Fatalf("no safety backup before the restore: %+v", list)
	}

	// Staging.
	stg, jobID, err := svc.StartStaging(ctx, id, StagingInput{})
	wait(jobID, err)
	if stg.PrimaryDomain != "staging.a.test" || wp(stg.ID, "option", "get", "home") != "https://staging.a.test" {
		t.Fatalf("staging home %s", wp(stg.ID, "option", "get", "home"))
	}
	if wp(stg.ID, "option", "get", "blog_public") != "0" ||
		wp(stg.ID, "eval", "echo wp_get_environment_type();") != "staging" {
		t.Fatal("staging isn't marked as staging")
	}
	content := wp(stg.ID, "post", "list", "--title=Original", "--field=post_content")
	if !strings.Contains(content, "https://staging.a.test/about") || !strings.Contains(content, "https://a.test.au") {
		t.Fatalf("links on staging: %s", content)
	}
	if sh(stg.ID, "cat wp-content/uploads/2026/pic.jpg") != "img" {
		t.Fatal("uploads not cloned")
	}

	// Work on staging, push code and database.
	wp(stg.ID, "post", "create", "--post_title=From staging", "--post_status=publish",
		`--post_content=See https://staging.a.test/new`)
	sh(stg.ID, "echo theme > wp-content/themes/custom.txt")
	sh(id, "echo live-upload > wp-content/uploads/2026/live.jpg") // arrives on live meanwhile
	wait(svc.StartPush(ctx, stg.ID, PushInput{Files: PushFilesCode, Database: true}))
	if !slices.Contains(titles(id), "From staging") {
		t.Fatal("pushed post missing")
	}
	if wp(id, "option", "get", "home") != "https://a.test" || wp(id, "option", "get", "blog_public") != "1" {
		t.Fatal("push changed the live site's URL or search engine setting")
	}
	if c := wp(id, "post", "list", "--title=From staging", "--field=post_content"); c != "See https://a.test/new" {
		t.Fatalf("pushed links: %s", c)
	}
	if sh(id, "cat wp-content/themes/custom.txt") != "theme" || sh(id, "cat wp-content/uploads/2026/live.jpg") != "live-upload" {
		t.Fatal("a code push must bring code and keep live uploads")
	}
	if wp(stg.ID, "option", "get", "home") != "https://staging.a.test" {
		t.Fatal("pushing changed staging")
	}

	// www becomes the primary domain.
	if _, err := svc.AddDomain(ctx, id, "www.a.test", true); err != nil {
		t.Fatal(err)
	}
	wait(svc.StartPrimaryDomain(ctx, id, "www.a.test"))
	if wp(id, "option", "get", "home") != "https://www.a.test" {
		t.Fatal("links not moved to www")
	}
	if cur, _ := st.GetSite(ctx, id); cur.PrimaryDomain != "www.a.test" || !slices.Equal(cur.RedirectDomains, []string{"a.test"}) {
		t.Fatalf("domains %s %v", cur.PrimaryDomain, cur.RedirectDomains)
	}

	// A backup as a new site.
	copySite, jobID, err := svc.StartRestoreAsNew(ctx, RestoreAsNewInput{RepoID: first.RepoID, BackupID: first.ID, Domain: "b.test"})
	wait(jobID, err)
	if wp(copySite.ID, "option", "get", "home") != "https://b.test" || !slices.Contains(titles(copySite.ID), "Original") {
		t.Fatal("restore as new site")
	}

	// PHP 8.4.
	wait(svc.StartPHPChange(ctx, id, PHPInput{Version: "8.4", Settings: store.PHPSettings{MemoryLimitMB: 300}}))
	var v bytes.Buffer
	docker.Exec(ctx, id, nil, &v, "php", "-r", "echo PHP_MAJOR_VERSION, '.', PHP_MINOR_VERSION;")
	if v.String() != "8.4" {
		t.Fatalf("PHP %s", v.String())
	}
	if out, _ := exec.CommandContext(ctx, "docker", "exec", runtime.ContainerName(id, mustSite(t, st, id).Upstreams[0]),
		"php-fpm", "-tt").CombinedOutput(); !strings.Contains(string(out), "php_value[memory_limit] = 300M") {
		t.Fatalf("setting not in FPM:\n%s", out)
	}

	// Deleting: staging first.
	if err := svc.Delete(ctx, id); err == nil {
		t.Fatal("deleted a site with a staging site")
	}
	for _, sid := range []string{stg.ID, copySite.ID, id} {
		// Replicas retired by the PHP switch drain (holding the site's lock)
		// for a while; deleting meanwhile is refused.
		for i := 0; ; i++ {
			err := svc.Delete(ctx, sid)
			if err == nil {
				break
			}
			if i > 90 || !strings.Contains(err.Error(), "try again") {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)
		}
	}
	// Backups of deleted sites stay restorable.
	if all, err := svc.RepoBackups(ctx, LocalRepoID); err != nil || len(all) < 3 {
		t.Fatalf("repository after deletes: %d %v", len(all), err)
	}
}

// e2e is a real WordPress environment: MariaDB in a container on E2E_NET,
// sites as real PHP containers, jobs, restic.
type e2e struct {
	t      *testing.T
	ctx    context.Context
	svc    *Service
	st     *store.Store
	docker *runtime.Docker
	raw    *sql.DB
	db     *dbprov.MariaDB
}

// newE2E needs Docker, the wpgenie/php images, and to run on a Docker
// network it shares with the database (see the Makefile's test-e2e: the test
// container is started with E2E_NET).
func newE2E(t *testing.T) *e2e {
	t.Helper()
	return newE2ENamed(t, t.Name(), 29000)
}

// newE2ENamed is a second environment in the same test (another server of
// a cluster): its own database and cache containers, directory and ports.
func newE2ENamed(t *testing.T, name string, portBase int) *e2e {
	t.Helper()
	net := os.Getenv("E2E_NET")
	if os.Getenv("WPGENIE_TEST_E2E") != "1" || net == "" || os.Geteuid() != 0 {
		t.Skip("set WPGENIE_TEST_E2E=1 and E2E_NET, run as root in a container on that network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	dbName, dbPass := "wpgt-db-"+strings.ToLower(name), "e2e-root-pw"
	docker := &runtime.Docker{Scope: strings.ToLower(name)}
	docker.Run(ctx, nil, "rm", "-f", dbName)
	if _, err := docker.Run(ctx, nil, "run", "-d", "--name", dbName, "--network", net,
		"-e", "MARIADB_ROOT_PASSWORD="+dbPass, "mariadb:11.4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Run(context.Background(), nil, "rm", "-f", dbName) })
	dsn := "root:" + dbPass + "@tcp(" + dbName + ":3306)/"
	raw, _ := sql.Open("mysql", dsn)
	t.Cleanup(func() { raw.Close() })
	for i := 0; raw.PingContext(ctx) != nil; i++ {
		if i > 60 {
			t.Fatal("MariaDB didn't start")
		}
		time.Sleep(time.Second)
	}
	db, err := dbprov.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// The object cache, as on a server: without it WordPress's cron runs one
	// event per pass (it re-reads its lock from the cache, skipping the
	// in-request copy).
	// As deploy/docker-compose.yml runs it: per-site users once the daemon
	// has written their ACL file.
	valkey := "wpgt-valkey-" + strings.ToLower(name)
	dataDir := t.TempDir()
	os.Chmod(dataDir, 0o755)
	aclDir := filepath.Join(dataDir, "valkey")
	os.MkdirAll(aclDir, 0o755)
	docker.Run(ctx, nil, "rm", "-f", valkey)
	if _, err := docker.Run(ctx, nil, "run", "-d", "--name", valkey, "--network", net, "-v", aclDir+":/etc/valkey:ro",
		"valkey/valkey:8-alpine", "sh", "-c",
		`exec valkey-server --save "" --appendonly no $([ -f /etc/valkey/users.acl ] && echo --aclfile /etc/valkey/users.acl)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Run(context.Background(), nil, "rm", "-f", valkey) })

	cfg := config.Default()
	cfg.RedisHost = valkey
	cfg.DataDir = dataDir
	cfg.ValkeyACLDir, cfg.ValkeyKey = aclDir, filepath.Join(dataDir, "valkey.key")
	cfg.DockerNetwork, cfg.MariaDBHost, cfg.SitePortBase = net, dbName, portBase
	cfg.CaddyfilePath = filepath.Join(cfg.DataDir, "caddy", "Caddyfile")
	st := storetest.Open(t)
	log := slog.New(slog.DiscardHandler)
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	cache := &runtime.Valkey{Docker: *docker, Container: valkey}
	svc := &Service{Cfg: cfg, Store: st, Runtime: docker, DB: db, Log: log,
		Dumper: &runtime.MariaDB{Container: dbName, Password: dbPass},
		Proxy:  &fakeProxy{log: &[]string{}}, Cache: cache, CacheACL: cache, Prober: okProber{},
		Jobs: &jobs.Queue{Store: st, Log: log},
		Backups: &backup.Restic{Docker: docker, Image: cfg.ResticImage,
			CacheDir: filepath.Join(cfg.DataDir, "backups", "cache")},
		Images: docker, Version: "test"}
	os.MkdirAll(filepath.Join(cfg.DataDir, "backups", "cache"), 0o700)
	cache.AdminPassword = svc.CacheAdminPassword
	if err := svc.UpgradeCacheUsers(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ { // restarted with the ACL file
		if out, _ := docker.Run(ctx, nil, "exec", valkey, "valkey-cli", "PING"); strings.Contains(string(out), "NOAUTH") {
			break
		}
		if i > 50 {
			t.Fatal("Valkey didn't come back with its users")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() {
		sites, _ := st.ListSites(context.Background())
		for _, s := range sites {
			docker.RemoveSite(context.Background(), s.ID)
		}
	})
	return &e2e{t: t, ctx: ctx, svc: svc, st: st, docker: docker, raw: raw, db: db}
}

func (e *e2e) wp(id string, args ...string) string {
	e.t.Helper()
	var out bytes.Buffer
	if err := e.docker.Exec(e.ctx, id, nil, &out, runtime.WPArgs(args...)...); err != nil {
		e.t.Fatalf("wp %v on %s: %v", args, id, err)
	}
	return strings.TrimSpace(out.String())
}

func (e *e2e) sh(id, script string) string {
	e.t.Helper()
	var out bytes.Buffer
	if err := e.docker.Exec(e.ctx, id, nil, &out, "sh", "-c", "cd "+e.svc.Cfg.SiteRoot(id)+" && "+script); err != nil {
		e.t.Fatalf("sh %q on %s: %v", script, id, err)
	}
	return strings.TrimSpace(out.String())
}

func (e *e2e) wait(id int64, err error) *store.Job {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
	j, err := e.svc.Jobs.WaitJob(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	if j.Status != store.JobSucceeded {
		e.t.Fatalf("job %s failed at %d%% (%s): %s", j.Kind, j.Progress, j.Step, j.Error)
	}
	return j
}

func mustSite(t *testing.T, st *store.Store, id string) *store.Site {
	t.Helper()
	s, err := st.GetSite(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
