package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPostgresConfigTLS(t *testing.T) {
	for _, c := range []struct {
		url    string
		ok     bool
		verify bool // certificate and name checked
	}{
		{"postgres://u:p@localhost/db", true, false},
		{"postgres://u:p@127.0.0.1:5433/db?sslmode=disable", true, false},
		{"postgresql://u:p@[::1]/db", true, false},
		{"postgres://u:p@/db?host=/var/run/postgresql", true, false},
		{"postgres://u:p@db.example.com/db", true, true}, // verify-full by default
		{"postgres://u:p@db.example.com/db?sslmode=verify-full", true, true},
		{"postgres://u:p@db.example.com/db?sslmode=require", true, false},
		{"postgres://u:p@db.example.com/db?sslmode=disable", false, false},
		{"postgres://u:p@db.example.com/db?sslmode=prefer", false, false},
		{"postgres://u:p@db.example.com/db?sslmode=allow", false, false},
		{"postgres://u:p@localhost,db.example.com/db?sslmode=disable", false, false},
		{"mysql://u:p@localhost/db", false, false},
		{"host=localhost dbname=x", false, false},
	} {
		cfg, err := PostgresConfig(c.url)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.url, err)
			continue
		}
		if err == nil && c.verify && (cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName == "") {
			t.Errorf("%s: certificate not verified", c.url)
		}
	}
	// Errors never show the password.
	_, err := PostgresConfig("postgres://u:s3cr3t-pw@db.example.com:notaport/db")
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Errorf("err = %v", err)
	}
	_, err = PostgresConfig("postgres://u:s3cr3t-pw@db.example.com/db?sslmode=bogus")
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Errorf("err = %v", err)
	}
}

// A panel database with data moves to PostgreSQL intact: every row, the
// ids, and ids handed out later don't collide with deleted ones.
func TestCopyToPostgres(t *testing.T) {
	dsn := pgDSN(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "wpgenie.db")
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Fill every table the suite knows how to fill.
	testSites(t, src)
	testUsers(t, src)
	testTraffic(t, src)
	testJobsAndOps(t, src)
	testBackups(t, src)
	testCDNAndAccess(t, src)
	src.AddMailDomain(ctx, "m.test")
	src.AddMailbox(ctx, Mailbox{Address: "a@m.test", Domain: "m.test", QuotaMB: 5})
	src.AddMailAlias(ctx, MailAlias{Alias: "b@m.test", Target: "a@m.test", Domain: "m.test"})
	src.SaveScan(ctx, "sb", time.Unix(1700000000, 0), []byte(`{}`))
	src.SetCDN(ctx, &CDN{SiteID: "sb", Provider: "cloudflare", APIToken: "t", Zones: map[string]string{}})
	// The newest user is deleted: its id must not come back.
	last, _ := src.CreateUser(ctx, "last", "h", "viewer")
	src.DeleteUser(ctx, last.ID)
	users, _ := src.ListUsers(ctx)
	sites, _ := src.ListSites(ctx)
	stats, _ := src.SiteStats(ctx, "s1", time.Unix(0, 0))
	src.Close()
	before, _ := os.ReadFile(path)

	rep, err := CopyToPostgres(ctx, path, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Version != len(migrations) || len(rep.Tables) < 20 {
		t.Errorf("report %+v", rep)
	}
	var total int64
	for _, tc := range rep.Tables {
		total += tc.Rows
	}
	if total < 500 {
		t.Errorf("only %d rows copied", total)
	}
	if after, _ := os.ReadFile(path); !slices.Equal(before, after) {
		t.Error("the SQLite file changed")
	}

	pg := openURL(t, dsn)
	pgUsers, _ := pg.ListUsers(ctx)
	if len(pgUsers) != len(users) {
		t.Fatalf("users %d, want %d", len(pgUsers), len(users))
	}
	for i := range users {
		a, b := users[i], pgUsers[i]
		if a.ID != b.ID || a.Username != b.Username || a.PasswordHash != b.PasswordHash || a.Disabled != b.Disabled ||
			!slices.Equal(a.RecoveryHashes, b.RecoveryHashes) {
			t.Errorf("user %d: %+v != %+v", i, a, b)
		}
	}
	pgSites, _ := pg.ListSites(ctx)
	if len(pgSites) != len(sites) || pgSites[0].ID != sites[0].ID || pgSites[0].CPUs != sites[0].CPUs ||
		!slices.Equal(pgSites[0].Upstreams, sites[0].Upstreams) || !slices.Equal(pgSites[0].Domains, sites[0].Domains) {
		t.Errorf("sites differ: %+v / %+v", pgSites, sites)
	}
	if st, _ := pg.SiteStats(ctx, "s1", time.Unix(0, 0)); st.UniqueVisitors != stats.UniqueVisitors ||
		st.Totals != stats.Totals {
		t.Errorf("stats %+v, want %+v", st, stats)
	}
	u, err := pg.CreateUser(ctx, "new", "h", "viewer")
	if err != nil || u.ID <= last.ID {
		t.Errorf("new user id %d after copy (deleted max %d): %v", u.ID, last.ID, err)
	}
	if _, err := pg.CreateJob(ctx, "", "x", "y"); err != nil {
		t.Errorf("CreateJob after copy: %v", err)
	}

	// Never into a database that has data.
	if _, err := CopyToPostgres(ctx, path, dsn); err == nil || !strings.Contains(err.Error(), "already has data") {
		t.Errorf("second copy: %v", err)
	}
}

// A database from an older version is copied at its version; the daemon
// then migrates PostgreSQL the rest of the way.
func TestCopyToPostgresOlderVersion(t *testing.T) {
	dsn := pgDSN(t)
	path := filepath.Join(t.TempDir(), "db")
	ctx := context.Background()
	// A database one migration behind, as a daemon that hasn't started on
	// the new version leaves it: every migration but the last, as Open runs
	// them.
	older := len(migrations) - 1
	rawSQL := openRawSQLite(t, path)
	if _, err := rawSQL.Exec(`CREATE TABLE schema_version (v INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := range older {
		if _, err := rawSQL.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
		if _, err := rawSQL.Exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawSQL.Exec(`INSERT INTO sites (id, name, primary_domain, php_version, fpm_port, db_name, status,
		created_at, updated_at) VALUES ('s1', 'x', 'a.test', '8.3', 19000, 'wp_s1', 'active', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	rawSQL.Close()
	rep, err := CopyToPostgres(ctx, path, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Version != older {
		t.Errorf("copied at version %d, want %d", rep.Version, older)
	}
	pg := openURL(t, dsn)
	if s, err := pg.GetSite(ctx, "s1"); err != nil || s.CacheMobile {
		t.Errorf("GetSite after upgrade = %+v, %v", s, err)
	}
}
