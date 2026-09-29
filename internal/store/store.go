// Package store is the panel's own state (sites, traffic rollups), kept in
// SQLite. It is deliberately small: WordPress data lives in MariaDB.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"

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
	// Settings hold secrets (the mail relay password): root only. SQLite
	// gives the -wal/-shm files the same mode as the database.
	if err := os.Chmod(path, 0o600); err != nil {
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
	// v2: per-site resources, replicas and caching. A site now has one
	// loopback port per PHP-FPM replica (site_upstreams); sites.fpm_port stays
	// as a reserved port so its UNIQUE constraint keeps holding.
	`ALTER TABLE sites ADD COLUMN memory_mb INTEGER NOT NULL DEFAULT 512;
	ALTER TABLE sites ADD COLUMN cpus REAL NOT NULL DEFAULT 1;
	ALTER TABLE sites ADD COLUMN replicas INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE sites ADD COLUMN page_cache INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN object_cache INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE site_upstreams (
		port    INTEGER PRIMARY KEY,
		site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE
	);
	INSERT INTO site_upstreams (port, site_id) SELECT fpm_port, id FROM sites;`,
	// v3: shield settings, CPU autoscaling, WordPress update manager and
	// security scans. IP lists are comma-separated normalised prefixes.
	`ALTER TABLE sites ADD COLUMN waf INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE sites ADD COLUMN admin_allow TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN trusted_ips TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN autoscale INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN min_replicas INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE sites ADD COLUMN max_replicas INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE sites ADD COLUMN target_cpu INTEGER NOT NULL DEFAULT 70;
	ALTER TABLE sites ADD COLUMN auto_update TEXT NOT NULL DEFAULT 'security';
	CREATE TABLE site_events (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		time    INTEGER NOT NULL,
		kind    TEXT NOT NULL,
		message TEXT NOT NULL
	);
	CREATE INDEX site_events_by_site ON site_events (site_id, id);
	CREATE TABLE site_updates (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id     TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		trigger     TEXT NOT NULL,
		status      TEXT NOT NULL,
		summary     TEXT NOT NULL DEFAULT '',
		details     TEXT NOT NULL DEFAULT '[]',
		started_at  INTEGER NOT NULL,
		finished_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX site_updates_by_site ON site_updates (site_id, id);
	CREATE TABLE site_scans (
		site_id    TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		scanned_at INTEGER NOT NULL,
		report     TEXT NOT NULL
	);
	CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
	// v4: mail. The mail server's own files are the source of truth for
	// accounts; these tables are what the panel shows and validates against.
	// A mailbox with a site_id is that site's WordPress sender (managed).
	`CREATE TABLE mail_domains (
		domain     TEXT PRIMARY KEY,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE mailboxes (
		address    TEXT PRIMARY KEY,
		domain     TEXT NOT NULL REFERENCES mail_domains(domain),
		quota_mb   INTEGER NOT NULL DEFAULT 0,
		site_id    TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	);
	CREATE TABLE mail_aliases (
		alias  TEXT NOT NULL,
		target TEXT NOT NULL,
		domain TEXT NOT NULL REFERENCES mail_domains(domain),
		PRIMARY KEY (alias, target)
	);
	ALTER TABLE sites ADD COLUMN smtp INTEGER NOT NULL DEFAULT 0;`,
	// v5: CDN in front of a site. Its own table so the API token never rides
	// along with the site record the API returns. zones is JSON (domain ->
	// zone ID); purged_at is when the last successful purge started.
	`CREATE TABLE site_cdn (
		site_id    TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		provider   TEXT NOT NULL,
		api_token  TEXT NOT NULL,
		zones      TEXT NOT NULL DEFAULT '{}',
		purged_at  INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	);`,
	// v6: per-site shield tuning (XML-RPC, rate limits, challenge
	// difficulty), deny lists, IP reputation, country rules and request-body
	// inspection; panel users, their sessions and the audit log; plugin
	// analysis reports. Zero rate limits and difficulty mean the server
	// defaults. Existing sites keep body inspection off: switching it on can
	// change what a live site accepts.
	`ALTER TABLE sites ADD COLUMN xmlrpc INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN rate_rps REAL NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN rate_burst INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN login_per_min REAL NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN challenge_bits INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN deny_ips TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN reputation TEXT NOT NULL DEFAULT 'challenge';
	ALTER TABLE sites ADD COLUMN country_mode TEXT NOT NULL DEFAULT 'off';
	ALTER TABLE sites ADD COLUMN countries TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN country_action TEXT NOT NULL DEFAULT 'block';
	ALTER TABLE sites ADD COLUMN body_waf TEXT NOT NULL DEFAULT 'off';
	CREATE TABLE users (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		username       TEXT NOT NULL UNIQUE COLLATE NOCASE,
		password       TEXT NOT NULL,
		role           TEXT NOT NULL,
		totp_secret    TEXT NOT NULL DEFAULT '',
		totp_pending   TEXT NOT NULL DEFAULT '',
		totp_last_step INTEGER NOT NULL DEFAULT 0,
		recovery       TEXT NOT NULL DEFAULT '[]',
		disabled       INTEGER NOT NULL DEFAULT 0,
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL,
		last_login_at  INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE sessions (
		id           TEXT PRIMARY KEY,
		token_hash   TEXT NOT NULL UNIQUE,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		ip           TEXT NOT NULL,
		user_agent   TEXT NOT NULL
	);
	CREATE INDEX sessions_by_user ON sessions (user_id);
	CREATE TABLE audit_log (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		time   INTEGER NOT NULL,
		actor  TEXT NOT NULL,
		ip     TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		status INTEGER NOT NULL DEFAULT 0,
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE site_plugin_reports (
		site_id     TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		analysed_at INTEGER NOT NULL,
		report      TEXT NOT NULL
	);`,
	// v7: backups and environments. Jobs are long operations with progress
	// (no foreign key: the record of deleting or failing to create a site
	// outlives it). Backup snapshots live in the restic repositories; the
	// panel stores where the repositories are and each site's policy.
	// parent_id links a staging site to the live site it was cloned from.
	// Redirect domains answer with a permanent redirect to the primary.
	`CREATE TABLE jobs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id     TEXT NOT NULL DEFAULT '',
		kind        TEXT NOT NULL,
		status      TEXT NOT NULL,
		progress    INTEGER NOT NULL DEFAULT 0,
		step        TEXT NOT NULL DEFAULT '',
		error       TEXT NOT NULL DEFAULT '',
		result      TEXT NOT NULL DEFAULT '',
		actor       TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		started_at  INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX jobs_by_site ON jobs (site_id, id);
	CREATE TABLE backup_repos (
		id               TEXT PRIMARY KEY,
		name             TEXT NOT NULL,
		kind             TEXT NOT NULL,
		location         TEXT NOT NULL,
		password         TEXT NOT NULL,
		secrets          TEXT NOT NULL DEFAULT '{}',
		created_at       INTEGER NOT NULL,
		checked_at       INTEGER NOT NULL DEFAULT 0,
		check_error      TEXT NOT NULL DEFAULT '',
		pruned_at        INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE site_backup_policy (
		site_id         TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		repo_id         TEXT NOT NULL REFERENCES backup_repos(id),
		interval_hours  INTEGER NOT NULL DEFAULT 24,
		keep_last       INTEGER NOT NULL DEFAULT 0,
		keep_daily      INTEGER NOT NULL DEFAULT 7,
		keep_weekly     INTEGER NOT NULL DEFAULT 4,
		keep_monthly    INTEGER NOT NULL DEFAULT 6,
		last_backup_at  INTEGER NOT NULL DEFAULT 0,
		last_attempt_at INTEGER NOT NULL DEFAULT 0,
		last_error      TEXT NOT NULL DEFAULT ''
	);
	ALTER TABLE sites ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN php_settings TEXT NOT NULL DEFAULT '{}';
	ALTER TABLE site_domains ADD COLUMN redirect INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE site_certs (
		site_id    TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		names      TEXT NOT NULL,
		issuer     TEXT NOT NULL,
		not_after  INTEGER NOT NULL,
		trusted    INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE sftp_users (
		username    TEXT PRIMARY KEY,
		site_id     TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		password    TEXT NOT NULL DEFAULT '',
		public_keys TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	);`,
	// v8: performance. Separate mobile page cache for every page, image
	// formats to convert uploads to, autoscaling on PHP workers and response
	// time (0: off), pull-zone CDNs (asset_host, pull_zone) and HTML edge
	// caching; insights: PHP response times per hour (hist: counts per
	// store.LatencyBuckets, JSON), slow URLs and PHP errors grouped by
	// fingerprint (level, message, file, line).
	`ALTER TABLE sites ADD COLUMN cache_mobile INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN image_formats TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN target_workers INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN target_response_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE site_cdn ADD COLUMN asset_host TEXT NOT NULL DEFAULT '';
	ALTER TABLE site_cdn ADD COLUMN pull_zone TEXT NOT NULL DEFAULT '';
	ALTER TABLE site_cdn ADD COLUMN edge_html INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE perf_hourly (
		site_id      TEXT NOT NULL,
		hour         INTEGER NOT NULL,
		php_requests INTEGER NOT NULL DEFAULT 0,
		php_ms       INTEGER NOT NULL DEFAULT 0,
		slow         INTEGER NOT NULL DEFAULT 0,
		cache_hits   INTEGER NOT NULL DEFAULT 0,
		cache_misses INTEGER NOT NULL DEFAULT 0,
		hist         TEXT NOT NULL DEFAULT '[]',
		PRIMARY KEY (site_id, hour)
	);
	CREATE TABLE slow_requests (
		site_id     TEXT NOT NULL,
		method      TEXT NOT NULL,
		path        TEXT NOT NULL,
		count       INTEGER NOT NULL,
		total_ms    INTEGER NOT NULL,
		max_ms      INTEGER NOT NULL,
		last_status INTEGER NOT NULL,
		last_seen   INTEGER NOT NULL,
		PRIMARY KEY (site_id, method, path)
	);
	CREATE TABLE php_errors (
		site_id     TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		fingerprint TEXT NOT NULL,
		level       TEXT NOT NULL,
		message     TEXT NOT NULL,
		file        TEXT NOT NULL,
		line        INTEGER NOT NULL,
		source      TEXT NOT NULL,
		count       INTEGER NOT NULL,
		first_seen  INTEGER NOT NULL,
		last_seen   INTEGER NOT NULL,
		PRIMARY KEY (site_id, fingerprint)
	);
	CREATE INDEX php_errors_by_time ON php_errors (site_id, last_seen);`,
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
