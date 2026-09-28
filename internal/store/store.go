// Package store is the panel's own state (sites, traffic rollups), kept in
// SQLite. It is deliberately small: WordPress data lives in MariaDB.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure Go: no cgo, trivial cross-compilation
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE sites (
		id             TEXT PRIMARY KEY,
		name           TEXT NOT NULL,
		primary_domain TEXT NOT NULL,
		php_version    TEXT NOT NULL,
		fpm_port       INTEGER NOT NULL UNIQUE,
		db_name        TEXT NOT NULL,
		status         TEXT NOT NULL,
		shield_mode    TEXT NOT NULL DEFAULT 'standard',
		block_ai_bots  INTEGER NOT NULL DEFAULT 1,
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL
	);
	CREATE TABLE site_domains (
		domain  TEXT PRIMARY KEY,
		site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE
	);
	CREATE TABLE traffic_hourly (
		site_id    TEXT NOT NULL,
		hour       INTEGER NOT NULL,
		requests   INTEGER NOT NULL DEFAULT 0,
		page_views INTEGER NOT NULL DEFAULT 0,
		bytes_out  INTEGER NOT NULL DEFAULT 0,
		bot_hits   INTEGER NOT NULL DEFAULT 0,
		blocked    INTEGER NOT NULL DEFAULT 0,
		errors_5xx INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (site_id, hour)
	);
	CREATE TABLE visitors_daily (
		site_id TEXT NOT NULL,
		day     INTEGER NOT NULL,
		sketch  BLOB NOT NULL,
		PRIMARY KEY (site_id, day)
	);
	CREATE TABLE ingest_state (
		name   TEXT PRIMARY KEY,
		inode  INTEGER NOT NULL,
		offset INTEGER NOT NULL
	);`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(v), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (v) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
