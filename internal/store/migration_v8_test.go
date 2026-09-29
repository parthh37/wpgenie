package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
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
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	slow := map[SlowKey]*SlowAgg{}
	for i := range maxSlowRequests + 100 {
		slow[SlowKey{"s1", "GET", "/made-up-" + strconv.Itoa(i) + "/"}] = &SlowAgg{Count: 1, TotalMS: 1500, MaxMS: 1500, LastStatus: 404, LastSeen: int64(i)}
	}
	if err := st.ApplyTraffic(context.Background(), &TrafficBatch{Slow: slow}); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM slow_requests WHERE site_id = 's1'`).Scan(&n)
	if n != maxSlowRequests {
		t.Fatalf("%d slow URLs kept, want %d", n, maxSlowRequests)
	}
}
