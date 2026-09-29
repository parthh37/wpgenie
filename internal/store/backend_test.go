package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/parthh37/wpgenie/internal/store/pgtest"
)

// PostgreSQL tests run with WPGENIE_TEST_POSTGRES=<url> or
// WPGENIE_TEST_DOCKER=1 (see pgtest); without either they are skipped.
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// forEachBackend runs fn against a fresh, fully migrated store on SQLite
// and on PostgreSQL (skipped unless enabled).
func forEachBackend(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, openSQLite(t)) })
	t.Run("postgres", func(t *testing.T) { fn(t, openPostgresStore(t)) })
}

func openSQLite(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openPostgresStore(t *testing.T) *Store {
	t.Helper()
	return openURL(t, pgDSN(t))
}

// pgDSN is a URL for a new, empty schema; it skips the test when
// PostgreSQL tests are off.
func pgDSN(t *testing.T) string {
	t.Helper()
	return pgtest.DSN(t)
}

func openURL(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := OpenURL(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
