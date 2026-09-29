package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// IsPostgresURL reports whether a database_url names PostgreSQL.
func IsPostgresURL(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "postgres://") || strings.HasPrefix(l, "postgresql://")
}

// PostgresConfig parses a database_url. The panel database holds password
// hashes, TOTP secrets and backup credentials, so a server that isn't on
// this machine (loopback address or Unix socket) must be reached over
// TLS: without sslmode in the URL the connection verifies the server's
// certificate and name (verify-full, system roots unless sslrootcert is
// given); sslmode=require or verify-ca are respected; disable, allow and
// prefer, which may fall back to plaintext, are refused.
func PostgresConfig(databaseURL string) (*pgx.ConnConfig, error) {
	if !IsPostgresURL(databaseURL) {
		return nil, errors.New("database_url must start with postgres:// or postgresql://")
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		// url's errors quote the input, password included.
		return nil, errors.New("database_url is not a valid URL")
	}
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, redactURLError(err, u)
	}
	if allLocal(cfg) {
		return cfg, nil
	}
	q := u.Query()
	switch mode := q.Get("sslmode"); mode {
	case "":
		q.Set("sslmode", "verify-full")
		u.RawQuery = q.Encode()
		if cfg, err = pgx.ParseConfig(u.String()); err != nil {
			return nil, redactURLError(err, u)
		}
	case "disable", "allow", "prefer":
		return nil, fmt.Errorf("database_url: sslmode=%s may send the panel's data and password unencrypted to a remote "+
			"server; use sslmode=verify-full (or require)", mode)
	}
	// Whatever the URL and PG* environment said: every host pgx may try
	// must be reached over TLS.
	if cfg.TLSConfig == nil {
		return nil, errors.New("database_url: a remote PostgreSQL server must be reached over TLS")
	}
	for _, fb := range cfg.Fallbacks {
		if fb.TLSConfig == nil {
			return nil, errors.New("database_url: a remote PostgreSQL server must be reached over TLS")
		}
	}
	return cfg, nil
}

// allLocal: every host pgx may connect to is a Unix socket or loopback.
func allLocal(cfg *pgx.ConnConfig) bool {
	hosts := []string{cfg.Host}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, fb.Host)
	}
	for _, h := range hosts {
		if strings.HasPrefix(h, "/") || strings.EqualFold(h, "localhost") {
			continue
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			continue
		}
		return false
	}
	return true
}

// redactURLError keeps the password out of parse errors (pgx redacts on a
// best-effort basis only).
func redactURLError(err error, u *url.URL) error {
	msg := err.Error()
	if pw, ok := u.User.Password(); ok && pw != "" {
		msg = strings.ReplaceAll(msg, pw, "xxxxx")
		msg = strings.ReplaceAll(msg, url.QueryEscape(pw), "xxxxx")
		msg = strings.ReplaceAll(msg, url.PathEscape(pw), "xxxxx")
	}
	return fmt.Errorf("database_url: %s", msg)
}

func openPostgres(ctx context.Context, databaseURL string) (*sql.DB, error) {
	cfg, err := PostgresConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "wpgenie"
	}
	db := stdlib.OpenDB(*cfg)
	// The daemon's goroutines (API, ingestion, autoscaler, jobs) each hold
	// a connection briefly; a small pool leaves room for other nodes.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	return db, nil
}

// migrationLock names the advisory lock that serialises migrations (and
// copies into an empty database) per schema: two control-plane nodes
// starting at once must not both run a migration.
const migrationLock = `SELECT pg_advisory_xact_lock(hashtext('wpgenie-store-migrate:' || COALESCE(current_schema(), '')))`

// postgresStatements returns migration i (0-based) for PostgreSQL.
func postgresStatements(i int) ([]string, error) {
	if o, ok := postgresMigrations[i+1]; ok {
		return splitStatements(o)
	}
	return translateMigration(migrations[i])
}

// schemaVersion reads the migration count of a PostgreSQL database (0
// when it has never been migrated).
func schemaVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('schema_version') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return 0, err
	}
	var v int
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(v), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// migratePostgres brings the schema to version target, all pending
// migrations in one transaction (PostgreSQL DDL is transactional: a
// failed migration leaves nothing behind) under the migration lock.
func migratePostgres(ctx context.Context, db *sql.DB, target int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, migrationLock); err != nil {
		return err
	}
	v, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if v >= target {
		return nil // up to date (or migrated by a newer binary, like SQLite)
	}
	for _, q := range []string{nocaseCollation, `CREATE TABLE IF NOT EXISTS schema_version (v BIGINT NOT NULL)`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			if strings.Contains(err.Error(), "ICU") {
				return fmt.Errorf("PostgreSQL must be built with ICU (every mainstream package and managed service is): %w", err)
			}
			return err
		}
	}
	for i := v; i < target; i++ {
		stmts, err := postgresStatements(i)
		if err != nil {
			return fmt.Errorf("migration %d (PostgreSQL): %w", i+1, err)
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st); err != nil {
				return fmt.Errorf("migration %d (PostgreSQL): %w\n\tin: %s", i+1, err, compact(st))
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (v) VALUES ($1)`, i+1); err != nil {
			return err
		}
	}
	// A migration that copies rows with their ids (rebuilding a table)
	// leaves the identity behind them.
	if err := resetIdentities(ctx, tx, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// resetIdentities moves every identity sequence past the largest id in
// its table (and past floor[table], SQLite's AUTOINCREMENT high-water
// mark when copying), never backwards: ids are not handed out twice.
func resetIdentities(ctx context.Context, tx *sql.Tx, floor map[string]int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT table_name, column_name,
		pg_get_serial_sequence(quote_ident(table_name), column_name)
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND is_identity = 'YES'`)
	if err != nil {
		return err
	}
	type ident struct{ table, column, seq string }
	var ids []ident
	for rows.Next() {
		var i ident
		if err := rows.Scan(&i.table, &i.column, &i.seq); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, i := range ids {
		var maxID, last int64
		var called bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(`+quoteIdent(i.column)+`), 0) FROM `+
			quoteIdent(i.table)).Scan(&maxID); err != nil {
			return err
		}
		// seq comes from the catalog, already quoted and qualified.
		if err := tx.QueryRowContext(ctx, `SELECT last_value, is_called FROM `+i.seq).Scan(&last, &called); err != nil {
			return err
		}
		next := max(maxID, floor[i.table]) + 1
		if called {
			next = max(next, last+1)
		} else {
			next = max(next, last)
		}
		if _, err := tx.ExecContext(ctx, `SELECT setval($1::regclass, $2, false)`, i.seq, next); err != nil {
			return err
		}
	}
	return nil
}
