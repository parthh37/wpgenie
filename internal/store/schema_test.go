package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
)

// column is what the schemas must agree on: name, storage class and
// nullability.
type column struct {
	Name    string
	Class   string // integer | real | text | blob
	NotNull bool
}

// sqliteClass maps a declared SQLite type the way SQLite's affinity rules
// (and the translator) do.
func sqliteClass(decl string) string {
	d := strings.ToUpper(decl)
	switch {
	case d == "BOOLEAN" || d == "BOOL" || strings.Contains(d, "INT"):
		return "integer"
	case strings.Contains(d, "CHAR") || strings.Contains(d, "CLOB") || strings.Contains(d, "TEXT"):
		return "text"
	case d == "BLOB":
		return "blob"
	case strings.Contains(d, "REAL") || strings.Contains(d, "FLOA") || strings.Contains(d, "DOUB"):
		return "real"
	}
	return "?" + decl
}

func pgClass(t string) string {
	return map[string]string{"bigint": "integer", "double precision": "real", "text": "text", "bytea": "blob"}[t]
}

type schemaInfo struct {
	Tables  map[string][]column
	Indexes []string // named indexes (not those behind constraints)
}

func sqliteSchema(t *testing.T, s *Store) schemaInfo {
	ctx := context.Background()
	db := s.db.sql
	info := schemaInfo{Tables: map[string][]column{}}
	rows, err := db.QueryContext(ctx, `SELECT type, name FROM sqlite_master
		WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var typ, name string
		rows.Scan(&typ, &name)
		if typ == "table" {
			tables = append(tables, name)
		} else {
			info.Indexes = append(info.Indexes, name)
		}
	}
	rows.Close()
	for _, tb := range tables {
		cols, err := db.QueryContext(ctx, `SELECT name, type, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, tb)
		if err != nil {
			t.Fatal(err)
		}
		for cols.Next() {
			var c column
			var decl string
			var pk int
			if err := cols.Scan(&c.Name, &decl, &c.NotNull, &pk); err != nil {
				t.Fatal(err)
			}
			c.Class = sqliteClass(decl)
			// PostgreSQL makes primary key columns NOT NULL; SQLite only
			// does for INTEGER PRIMARY KEY (legacy quirk).
			c.NotNull = c.NotNull || pk > 0
			info.Tables[tb] = append(info.Tables[tb], c)
		}
		cols.Close()
	}
	return info
}

func postgresSchema(t *testing.T, s *Store) schemaInfo {
	ctx := context.Background()
	db := s.db.sql
	info := schemaInfo{Tables: map[string][]column{}}
	rows, err := db.QueryContext(ctx, `SELECT table_name, column_name, data_type, is_nullable = 'NO'
		FROM information_schema.columns WHERE table_schema = current_schema() ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tb, typ string
		var c column
		if err := rows.Scan(&tb, &c.Name, &typ, &c.NotNull); err != nil {
			t.Fatal(err)
		}
		c.Class = pgClass(typ)
		if c.Class == "" {
			c.Class = "?" + typ
		}
		info.Tables[tb] = append(info.Tables[tb], c)
	}
	rows.Close()
	// Indexes that don't back a constraint (PRIMARY KEY/UNIQUE ones are
	// named differently by each database).
	irows, err := db.QueryContext(ctx, `SELECT i.indexname FROM pg_indexes i
		WHERE i.schemaname = current_schema() AND NOT EXISTS (SELECT 1 FROM pg_constraint c
			WHERE c.conname = i.indexname AND c.connamespace = to_regnamespace(current_schema()))
		ORDER BY i.indexname`)
	if err != nil {
		t.Fatal(err)
	}
	for irows.Next() {
		var n string
		irows.Scan(&n)
		info.Indexes = append(info.Indexes, n)
	}
	irows.Close()
	return info
}

// Every migration, translated, must give PostgreSQL the same tables,
// columns (names, storage classes, NOT NULL) and named indexes as SQLite:
// a future migration the translator gets wrong fails here.
func TestPostgresSchemaMatchesSQLite(t *testing.T) {
	pg := openPostgresStore(t)
	lite := openSQLite(t)
	want, got := sqliteSchema(t, lite), postgresSchema(t, pg)
	for _, tb := range slices.Sorted(maps.Keys(want.Tables)) {
		if !slices.Equal(want.Tables[tb], got.Tables[tb]) {
			t.Errorf("table %s:\n SQLite     %v\n PostgreSQL %v", tb, want.Tables[tb], got.Tables[tb])
		}
	}
	for tb := range got.Tables {
		if _, ok := want.Tables[tb]; !ok {
			t.Errorf("table %s exists only on PostgreSQL", tb)
		}
	}
	if !slices.Equal(want.Indexes, got.Indexes) {
		t.Errorf("indexes:\n SQLite     %v\n PostgreSQL %v", want.Indexes, got.Indexes)
	}
	var v int
	pg.db.QueryRowContext(context.Background(), `SELECT MAX(v) FROM schema_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("schema_version = %d, want %d", v, len(migrations))
	}
}

// Two control-plane nodes starting at once on a new database: the
// advisory lock lets one migrate; the other finds nothing left to do.
func TestPostgresConcurrentMigrations(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	dsn := pgDSN(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			s, err := OpenURL(context.Background(), dsn)
			if err == nil {
				s.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("node %d: %v", i, err)
		}
	}
	s := openURL(t, dsn)
	var n, v int
	s.db.QueryRowContext(context.Background(), `SELECT COUNT(*), MAX(v) FROM schema_version`).Scan(&n, &v)
	if n != len(migrations) || v != len(migrations) {
		t.Errorf("schema_version has %d rows up to %d, want %d", n, v, len(migrations))
	}
}

// An existing PostgreSQL database upgrades in place from any version, like
// SQLite: data survives and new columns get their defaults.
func TestPostgresMigrateFromEachVersion(t *testing.T) {
	for from := 1; from < len(migrations); from++ {
		t.Run(fmt.Sprintf("v%d", from), func(t *testing.T) {
			dsn := pgDSN(t)
			db, err := openPostgres(context.Background(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			if err := migratePostgres(context.Background(), db, from); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO sites (id, name, primary_domain, php_version, fpm_port, db_name, status,
				created_at, updated_at) VALUES ('s1', 'x', 'a.test', '8.3', 19000, 'wp_s1', 'active', 1, 1)`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			s := openURL(t, dsn)
			site, err := s.GetSite(context.Background(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			if site.MemoryMB != 512 || site.Replicas != 1 || site.Reputation != "challenge" || site.PageCache || !site.WAF {
				t.Errorf("%+v", site)
			}
			// Migration 2 turns the reserved port into the first upstream.
			if (from == 1) != slices.Equal(site.Upstreams, []int{19000}) {
				t.Errorf("upstreams %v", site.Upstreams)
			}
		})
	}
}
