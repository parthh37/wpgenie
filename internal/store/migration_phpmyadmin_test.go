package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	_ "modernc.org/sqlite"
)

// Plans that included Adminer include phpMyAdmin after the upgrade.
func TestMigrationAdminerToPHPMyAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	raw, _ := sql.Open("sqlite", "file:"+path)
	raw.Exec(`CREATE TABLE schema_version (v INTEGER NOT NULL)`)
	for i := 0; i < migrationIndex(t, "'phpmyadmin'"); i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(i, err)
		}
		raw.Exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1)
	}
	raw.Exec(`INSERT INTO plans (id, name, features, created_at, updated_at) VALUES ('a','A','staging,adminer,sftp',0,0)`)
	raw.Exec(`INSERT INTO plans (id, name, features, created_at, updated_at) VALUES ('b','B','backups',0,0)`)
	raw.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.GetPlan(context.Background(), "a")
	if err != nil || !slices.Equal(a.Features, []string{"staging", "phpmyadmin", "sftp"}) {
		t.Fatalf("%+v %v", a, err)
	}
	b, err := st.GetPlan(context.Background(), "b")
	if err != nil || !slices.Equal(b.Features, []string{"backups"}) {
		t.Fatalf("%+v %v", b, err)
	}
}
