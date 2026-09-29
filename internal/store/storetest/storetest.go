// Package storetest opens a panel store for the tests of other packages:
// SQLite in a temporary directory by default or, when
// WPGENIE_TEST_POSTGRES holds a PostgreSQL URL, a fresh schema on that
// server. So
//
//	WPGENIE_TEST_POSTGRES=postgres://postgres:pw@127.0.0.1:5432/postgres?sslmode=disable go test ./...
//
// runs every store-backed test on PostgreSQL as well.
package storetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/pgtest"
)

// Open returns an empty, migrated store, closed when the test ends.
func Open(t testing.TB) *store.Store {
	t.Helper()
	var s *store.Store
	var err error
	if os.Getenv("WPGENIE_TEST_POSTGRES") != "" {
		s, err = store.OpenURL(context.Background(), pgtest.DSN(t))
	} else {
		s, err = store.Open(filepath.Join(t.TempDir(), "db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
