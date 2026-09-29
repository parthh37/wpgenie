// Package pgtest gives tests an empty PostgreSQL schema of their own.
//
// PostgreSQL tests run when either is set:
//
//	WPGENIE_TEST_POSTGRES=postgres://user:pass@host/db   an existing server
//	WPGENIE_TEST_DOCKER=1                                 a throwaway postgres:17-alpine
//	                                                      container (removed by Main)
//
// and are skipped otherwise. Each test gets a fresh schema (dropped at the
// end) through search_path, so tests can run in parallel on one server.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// Image is the PostgreSQL version the Docker tests run.
const Image = "postgres:17-alpine"

var (
	once      sync.Once
	baseDSN   string
	container string
	startErr  error
)

// Main runs the tests and removes the container they started, if any:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }
func Main(m *testing.M) int {
	code := m.Run()
	if container != "" {
		exec.Command("docker", "rm", "-f", "-v", container).Run()
	}
	return code
}

// Enabled reports whether PostgreSQL tests run.
func Enabled() bool {
	return os.Getenv("WPGENIE_TEST_POSTGRES") != "" || os.Getenv("WPGENIE_TEST_DOCKER") == "1"
}

// DSN returns a URL for a new, empty schema (dropped when the test ends),
// or skips the test when PostgreSQL tests aren't enabled.
func DSN(t testing.TB) string {
	t.Helper()
	if !Enabled() {
		t.Skip("PostgreSQL tests: set WPGENIE_TEST_POSTGRES=<url> or WPGENIE_TEST_DOCKER=1")
	}
	once.Do(start)
	if startErr != nil {
		t.Fatal(startErr)
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "wpgenie_test_" + randHex(6)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		if _, err := db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	return withSearchPath(baseDSN, schema)
}

// withSearchPath adds search_path (sent as a startup parameter) to a URL
// or keyword/value connection string.
func withSearchPath(dsn, schema string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

func start() {
	if dsn := os.Getenv("WPGENIE_TEST_POSTGRES"); dsn != "" {
		baseDSN = dsn
		startErr = waitReady(dsn, 30*time.Second)
		return
	}
	name := "wpgenie-test-pg-" + randHex(6)
	pass := randHex(16)
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD="+pass, "-p", "127.0.0.1::5432", Image).CombinedOutput()
	if err != nil {
		startErr = fmt.Errorf("starting %s: %v: %s", Image, err, out)
		return
	}
	container = name
	out, err = exec.Command("docker", "port", name, "5432/tcp").Output()
	if err != nil {
		startErr = fmt.Errorf("docker port: %v", err)
		return
	}
	addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	baseDSN = fmt.Sprintf("postgres://postgres:%s@%s/postgres?sslmode=disable", pass, addr)
	startErr = waitReady(baseDSN, 90*time.Second)
}

// waitReady waits for the server to accept queries. The image's first
// start runs initdb on a server listening only on its socket, then
// restarts: TCP connections fail until the real server is up.
func waitReady(dsn string, timeout time.Duration) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = db.QueryRowContext(ctx, `SELECT 1`).Scan(new(int))
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PostgreSQL not ready: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
