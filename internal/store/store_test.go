package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	_ "modernc.org/sqlite"
)

// A v0.1 panel database must upgrade in place: every existing site keeps
// its port as its single upstream and gets the old fixed resources.
func TestMigrateFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE schema_version (v INTEGER NOT NULL)`,
		migrations[0],
		`INSERT INTO schema_version (v) VALUES (1)`,
		`INSERT INTO sites (id, name, primary_domain, php_version, fpm_port, db_name, status, created_at, updated_at)
		 VALUES ('sold', 'Old', 'old.test', '8.3', 19004, 'wp_sold', 'active', 1, 1)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.GetSite(context.Background(), "sold")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Upstreams, []int{19004}) || s.Replicas != 1 || s.MemoryMB != 512 || s.CPUs != 1 {
		t.Errorf("migrated site = %+v", s)
	}
	if s.PageCache || s.ObjectCache {
		t.Error("existing sites must keep caching off until the owner turns it on")
	}
}

func TestAllocatePortsSkipsUsedAndReserved(t *testing.T) {
	forEachBackend(t, testAllocatePorts)
}

func testAllocatePorts(t *testing.T, st *Store) {
	ctx := context.Background()
	// Reserved 19000, but currently served on 19002 only.
	if err := st.CreateSite(ctx, &Site{ID: "a", Name: "a", PrimaryDomain: "a.test", PHPVersion: "8.3",
		FPMPort: 19000, DBName: "wp_a", Status: StatusActive, ShieldMode: "standard"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUpstreams(ctx, "a", []int{19002}); err != nil {
		t.Fatal(err)
	}
	got, err := st.AllocatePorts(ctx, 19000, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{19001, 19003, 19004}; !slices.Equal(got, want) {
		t.Errorf("AllocatePorts = %v, want %v", got, want)
	}
	if got, _ := st.AllocatePorts(ctx, 19000, 2, 19001); !slices.Equal(got, []int{19003, 19004}) {
		t.Errorf("excluded port handed out: %v", got)
	}
	if _, err := st.AllocatePorts(ctx, 65535, 2); err == nil {
		t.Error("expected an error when the port range is exhausted")
	}
	// Deleting a site frees its ports (ON DELETE CASCADE).
	st.DeleteSite(ctx, "a")
	if got, _ := st.AllocatePorts(ctx, 19000, 1); got[0] != 19000 {
		t.Errorf("ports not released on delete: got %v", got)
	}
}
