package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// A v7 database with a Cloudflare site upgrades with every new setting off.
func TestMigrationV8FromV7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	raw, _ := sql.Open("sqlite", "file:"+path)
	raw.Exec(`CREATE TABLE schema_version (v INTEGER NOT NULL)`)
	for i := 0; i < 7; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(i, err)
		}
		raw.Exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1)
	}
	raw.Exec(`INSERT INTO sites (id, name, primary_domain, php_version, fpm_port, db_name, status, created_at, updated_at) VALUES ('s1','x','a.test','8.3',19000,'wp_s1','active',0,0)`)
	raw.Exec(`INSERT INTO site_cdn (site_id, provider, api_token, created_at) VALUES ('s1','cloudflare','tok',0)`)
	raw.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.GetSite(context.Background(), "s1")
	if err != nil || s.CacheMobile || len(s.ImageFormats) != 0 || s.TargetWorkers != 0 || s.TargetResponseMS != 0 {
		t.Fatalf("%+v %v", s, err)
	}
	c, err := st.GetCDN(context.Background(), "s1")
	if err != nil || c.EdgeHTML || c.AssetHost != "" || c.Provider != "cloudflare" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestSlowRequestsAreCapped(t *testing.T) {
	forEachBackend(t, testSlowRequestsAreCapped)
}

func testSlowRequestsAreCapped(t *testing.T, st *Store) {
	slow := map[SlowKey]*SlowAgg{}
	for i := range maxSlowRequests + 100 {
		slow[SlowKey{"s1", "GET", "/made-up-" + strconv.Itoa(i) + "/"}] = &SlowAgg{Count: 1, TotalMS: 1500, MaxMS: 1500, LastStatus: 404, LastSeen: int64(i)}
	}
	if err := st.ApplyTraffic(context.Background(), &TrafficBatch{Slow: slow}); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM slow_requests WHERE site_id = 's1'`).Scan(&n)
	if n != maxSlowRequests {
		t.Fatalf("%d slow URLs kept, want %d", n, maxSlowRequests)
	}
}

// Plans that included SFTP before the file manager existed get it too;
// others don't, and running the migration can't add it twice.
func TestFilesFeatureMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	raw, _ := sql.Open("sqlite", "file:"+path)
	raw.Exec(`CREATE TABLE schema_version (v INTEGER NOT NULL)`)
	last := migrationIndex(t, "',files'")
	for i := 0; i < last; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(i, err)
		}
		raw.Exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1)
	}
	for id, feats := range map[string]string{"a": "sftp", "b": "backups,sftp,cdn", "c": "backups", "d": "", "e": "sftp,files", "f": "sftpx"} {
		if _, err := raw.Exec(`INSERT INTO plans (id, name, features, created_at, updated_at) VALUES (?, ?, ?, 0, 0)`, id, id, feats); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// (Every plan also gets burst, a later migration.)
	for id, want := range map[string]string{"a": "sftp,files,burst", "b": "backups,sftp,cdn,files,burst", "c": "backups,burst",
		"d": "burst", "e": "sftp,files,burst", "f": "sftpx,burst"} {
		p, err := st.GetPlan(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(p.Features, ","); got != want {
			t.Errorf("plan %s: %q, want %q", id, got, want)
		}
	}
}

// migrationIndex finds the migration containing marker: tests of one
// migration run those before it, whatever is appended after.
func migrationIndex(t *testing.T, marker string) int {
	t.Helper()
	for i, m := range migrations {
		if strings.Contains(m, marker) {
			return i
		}
	}
	t.Fatalf("no migration contains %q", marker)
	return 0
}

// Autoscaled sites become automatic burst and every plan keeps them
// scaling (the feature, unlimited minutes); the others start with burst off.
func TestBurstMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	raw, _ := sql.Open("sqlite", "file:"+path)
	raw.Exec(`CREATE TABLE schema_version (v INTEGER NOT NULL)`)
	last := migrationIndex(t, "burst_mode")
	for i := 0; i < last; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(i, err)
		}
		raw.Exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1)
	}
	for i, id := range []string{"s1", "s2"} {
		if _, err := raw.Exec(`INSERT INTO sites (id, name, primary_domain, php_version, fpm_port, db_name, status, autoscale,
			min_replicas, max_replicas, created_at, updated_at) VALUES (?, ?, ?, '8.3', ?, ?, 'active', ?, 1, 3, 0, 0)`,
			id, id, id+".test", 19000+i, "wp_"+id, 1-i); err != nil {
			t.Fatal(err)
		}
	}
	raw.Exec(`INSERT INTO plans (id, name, features, created_at, updated_at) VALUES ('p', 'p', 'burst,sftp', 0, 0)`)
	raw.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for id, want := range map[string]string{"s1": "auto", "s2": "off"} {
		s, err := st.GetSite(context.Background(), id)
		if err != nil || s.BurstMode != want || s.BurstPaused || !s.BurstUntil.IsZero() {
			t.Errorf("%s: %+v %v, want burst %s", id, s, err, want)
		}
	}
	if p, _ := st.GetPlan(context.Background(), "p"); strings.Join(p.Features, ",") != "burst,sftp" || p.BurstMinutes != 0 {
		t.Errorf("plan: %+v", p)
	}
}
