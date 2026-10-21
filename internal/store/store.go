// Package store is the panel's own state (sites, users, traffic rollups).
// It runs on SQLite by default (one file, nothing to operate) or on
// PostgreSQL, which several control-plane nodes can share. It is
// deliberately small: WordPress data lives in MariaDB.
//
// Queries and migrations are written once, in the SQL both understand;
// dialect.go and translate.go hold the rules and what gets rewritten.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // pure Go: no cgo, trivial cross-compilation
)

type Store struct {
	db *DB
}

// Open opens (creating it if needed) the SQLite database at path and
// brings its schema up to date.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY churn.
	// It also runs one transaction at a time, which stands in for the row
	// and table locks the store takes on PostgreSQL.
	db.SetMaxOpenConns(1)
	s := &Store{db: &DB{sql: db}}
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

// OpenURL opens the PostgreSQL database named by a postgres:// URL (the
// database_url setting) and brings its schema up to date. PostgresConfig
// has the TLS rules.
func OpenURL(ctx context.Context, databaseURL string) (*Store, error) {
	db, err := openPostgres(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := migratePostgres(ctx, db, len(migrations)); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: &DB{sql: db, postgres: true}}, nil
}

func (s *Store) Close() error { return s.db.sql.Close() }

// Postgres reports whether the store runs on PostgreSQL.
func (s *Store) Postgres() bool { return s.db.postgres }

// migrations are SQLite DDL, only ever appended to: a database records how
// many it has run (schema_version). On PostgreSQL each one is translated
// (translate.go) unless postgresMigrations overrides its version. The
// store tests run them all on both backends and compare the tables and
// columns they produce. For both to work, a new migration uses:
//
//   - CREATE TABLE / CREATE [UNIQUE] INDEX / ALTER TABLE ADD COLUMN, RENAME,
//     DROP COLUMN / DROP TABLE|INDEX, and plain INSERT/UPDATE/DELETE;
//   - types INTEGER (also for booleans and unix times), REAL, TEXT (COLLATE
//     NOCASE for case-insensitive names) and BLOB; INTEGER PRIMARY KEY
//     AUTOINCREMENT for generated ids;
//   - literal DEFAULTs (no CURRENT_TIMESTAMP), and NOT NULL columns added
//     with a DEFAULT.
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
	// Cluster: servers running `wpgenie agent` (on the control plane), the
	// registry of sites living on them (the node keeps the full record;
	// data is the control plane's copy for listings and access checks),
	// sites whose replicas also run on other nodes (site_upstreams rows
	// with a node_id are local tunnel ports to a replica there), replicas
	// a node runs for another node's sites, and sites that moved away from
	// this server whose domains are forwarded until DNS follows.
	`CREATE TABLE nodes (
		id             TEXT PRIMARY KEY,
		num            INTEGER NOT NULL UNIQUE,
		name           TEXT NOT NULL,
		address        TEXT NOT NULL,
		public_ip      TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'active',
		info           TEXT NOT NULL DEFAULT '{}',
		cert_not_after INTEGER NOT NULL DEFAULT 0,
		last_seen      INTEGER NOT NULL DEFAULT 0,
		last_error     TEXT NOT NULL DEFAULT '',
		key_hash       TEXT NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL
	);
	CREATE TABLE cluster_sites (
		site_id        TEXT PRIMARY KEY,
		node_id        TEXT NOT NULL REFERENCES nodes(id),
		primary_domain TEXT NOT NULL,
		data           TEXT NOT NULL DEFAULT '{}',
		updated_at     INTEGER NOT NULL
	);
	CREATE INDEX cluster_sites_by_node ON cluster_sites (node_id);
	ALTER TABLE sites ADD COLUMN spread_nodes TEXT NOT NULL DEFAULT '';
	ALTER TABLE site_upstreams ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE site_upstreams ADD COLUMN remote_port INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE guest_replicas (
		port       INTEGER PRIMARY KEY,
		site_id    TEXT NOT NULL,
		home_node  TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE site_forwards (
		site_id    TEXT PRIMARY KEY,
		node_id    TEXT NOT NULL,
		address    TEXT NOT NULL,
		domains    TEXT NOT NULL,
		port       INTEGER NOT NULL,
		http_port  INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	);`,
	// Monitoring: alert state, one row per watched target (a site's
	// uptime, a domain's certificate, a filesystem, a site's backups) once
	// it has been judged, firing or resolved; and a bounded history of
	// transitions. No foreign keys: an alert about a site outlives it long
	// enough to be resolved and notified.
	`CREATE TABLE alerts (
		alert_key   TEXT PRIMARY KEY,
		kind        TEXT NOT NULL,
		site_id     TEXT NOT NULL DEFAULT '',
		target      TEXT NOT NULL,
		severity    TEXT NOT NULL DEFAULT '',
		state       TEXT NOT NULL,
		message     TEXT NOT NULL DEFAULT '',
		since       INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL,
		notified_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE alert_history (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		alert_key   TEXT NOT NULL,
		kind        TEXT NOT NULL,
		site_id     TEXT NOT NULL DEFAULT '',
		target      TEXT NOT NULL,
		severity    TEXT NOT NULL DEFAULT '',
		state       TEXT NOT NULL,
		message     TEXT NOT NULL DEFAULT '',
		happened_at INTEGER NOT NULL
	);`,
	// Uploads offload: a site's uploads copied to S3-compatible storage and
	// served from its public URL when missing on disk. Its own table so the
	// secret key never rides along with the site record the API returns.
	// incremental_at / full_at: start of the last successful sync of each
	// kind (unix seconds; an incremental covers files changed since the
	// previous one started). offload_deletes: objects to delete (uploads
	// WordPress deleted), kept until the bucket confirms.
	`CREATE TABLE site_offload (
		site_id        TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		endpoint       TEXT NOT NULL,
		region         TEXT NOT NULL DEFAULT '',
		bucket         TEXT NOT NULL,
		key_prefix     TEXT NOT NULL,
		access_key_id  TEXT NOT NULL,
		secret_key     TEXT NOT NULL,
		public_url     TEXT NOT NULL,
		object_acl     TEXT NOT NULL DEFAULT '',
		local_days     INTEGER NOT NULL DEFAULT 0,
		incremental_at INTEGER NOT NULL DEFAULT 0,
		full_at        INTEGER NOT NULL DEFAULT 0,
		attempt_at     INTEGER NOT NULL DEFAULT 0,
		failures       INTEGER NOT NULL DEFAULT 0,
		last_error     TEXT NOT NULL DEFAULT '',
		last_objects   INTEGER NOT NULL DEFAULT 0,
		last_bytes     INTEGER NOT NULL DEFAULT 0,
		total_objects  INTEGER NOT NULL DEFAULT 0,
		total_bytes    INTEGER NOT NULL DEFAULT 0,
		total_deleted  INTEGER NOT NULL DEFAULT 0,
		removed_local  INTEGER NOT NULL DEFAULT 0,
		removed_bytes  INTEGER NOT NULL DEFAULT 0,
		cleaned_at     INTEGER NOT NULL DEFAULT 0,
		created_at     INTEGER NOT NULL
	);
	CREATE TABLE offload_deletes (
		site_id     TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		object_path TEXT NOT NULL,
		queued_at   INTEGER NOT NULL,
		PRIMARY KEY (site_id, object_path)
	);`,
	// Accounts, plans and billing: tenant accounts (customers and resellers)
	// on admin-defined plans, users belonging to an account (account_id 0:
	// staff), per-user API tokens (SHA-256 only) and one-time sign-on tokens.
	// Site ownership is its own table without a foreign key to sites: a site
	// may live on another node. A WHMCS service ID is unique per billing
	// owner (parent_id: 0 is the panel's own WHMCS, else the reseller's). site_usage holds the last disk measurement
	// per site, account_usage the month's totals and which thresholds were
	// notified (month_start: unix time of the UTC month). Stripe event IDs
	// make webhook processing idempotent; outgoing webhooks are a persistent
	// delivery queue. jobs.owner lets a tenant follow the jobs they started.
	`CREATE TABLE plans (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		max_sites    INTEGER NOT NULL DEFAULT 0,
		disk_mb      INTEGER NOT NULL DEFAULT 0,
		bandwidth_gb INTEGER NOT NULL DEFAULT 0,
		max_replicas INTEGER NOT NULL DEFAULT 0,
		max_memory_mb INTEGER NOT NULL DEFAULT 0,
		max_cpus     REAL NOT NULL DEFAULT 0,
		max_domains  INTEGER NOT NULL DEFAULT 0,
		features     TEXT NOT NULL DEFAULT '',
		backup_repos TEXT NOT NULL DEFAULT '',
		overage      TEXT NOT NULL DEFAULT 'notify',
		resellable   INTEGER NOT NULL DEFAULT 0,
		created_at   INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL
	);
	CREATE TABLE accounts (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		name                   TEXT NOT NULL,
		kind                   TEXT NOT NULL,
		status                 TEXT NOT NULL DEFAULT 'active',
		suspend_reason         TEXT NOT NULL DEFAULT '',
		plan_id                TEXT NOT NULL REFERENCES plans(id),
		parent_id              INTEGER NOT NULL DEFAULT 0,
		email                  TEXT NOT NULL DEFAULT '',
		whmcs_service_id       TEXT NOT NULL DEFAULT '',
		stripe_customer_id     TEXT NOT NULL DEFAULT '',
		stripe_subscription_id TEXT NOT NULL DEFAULT '',
		idempotency_key        TEXT UNIQUE,
		created_at             INTEGER NOT NULL,
		updated_at             INTEGER NOT NULL,
		suspended_at           INTEGER NOT NULL DEFAULT 0,
		stripe_event_at        INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX accounts_by_parent ON accounts (parent_id);
	CREATE INDEX accounts_by_stripe ON accounts (stripe_customer_id);
	CREATE UNIQUE INDEX accounts_by_whmcs ON accounts (parent_id, whmcs_service_id) WHERE whmcs_service_id <> '';
	ALTER TABLE users ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX users_by_account ON users (account_id);
	CREATE TABLE site_accounts (
		site_id    TEXT PRIMARY KEY,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		suspended  INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX site_accounts_by_account ON site_accounts (account_id);
	CREATE TABLE account_events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL,
		at         INTEGER NOT NULL,
		kind       TEXT NOT NULL,
		message    TEXT NOT NULL
	);
	CREATE INDEX account_events_by_account ON account_events (account_id, id);
	CREATE TABLE api_tokens (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name         TEXT NOT NULL,
		token_hash   TEXT NOT NULL UNIQUE,
		hint         TEXT NOT NULL,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL DEFAULT 0,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		last_used_ip TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX api_tokens_by_user ON api_tokens (user_id);
	CREATE TABLE sso_tokens (
		token_hash TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);
	CREATE TABLE site_usage (
		site_id     TEXT PRIMARY KEY,
		files_bytes INTEGER NOT NULL DEFAULT 0,
		db_bytes    INTEGER NOT NULL DEFAULT 0,
		measured_at INTEGER NOT NULL
	);
	CREATE TABLE account_usage (
		account_id      INTEGER NOT NULL,
		month_start     INTEGER NOT NULL,
		bandwidth_bytes INTEGER NOT NULL DEFAULT 0,
		disk_bytes      INTEGER NOT NULL DEFAULT 0,
		bw_notified     INTEGER NOT NULL DEFAULT 0,
		disk_notified   INTEGER NOT NULL DEFAULT 0,
		reported_mb     INTEGER NOT NULL DEFAULT 0,
		updated_at      INTEGER NOT NULL,
		PRIMARY KEY (account_id, month_start)
	);
	CREATE TABLE stripe_events (
		id          TEXT PRIMARY KEY,
		kind        TEXT NOT NULL,
		received_at INTEGER NOT NULL
	);
	CREATE TABLE webhook_endpoints (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		url        TEXT NOT NULL,
		secret     TEXT NOT NULL,
		events     TEXT NOT NULL DEFAULT '',
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE webhook_deliveries (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		endpoint_id     INTEGER NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
		event_id        TEXT NOT NULL,
		event           TEXT NOT NULL,
		payload         TEXT NOT NULL,
		status          TEXT NOT NULL,
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL,
		last_status     INTEGER NOT NULL DEFAULT 0,
		last_error      TEXT NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		delivered_at    INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX webhook_deliveries_due ON webhook_deliveries (status, next_attempt_at);
	ALTER TABLE jobs ADD COLUMN owner TEXT NOT NULL DEFAULT '';`,
	// WordPress performance tweaks (the optimize mu-plugin): a list of
	// site.Optimizations keys.
	`ALTER TABLE sites ADD COLUMN optimize TEXT NOT NULL DEFAULT '';`,
	// The file manager ("files") reaches what SFTP does: plans that
	// already include SFTP get it too.
	`UPDATE plans SET features = features || ',files'
	WHERE ',' || features || ',' LIKE '%,sftp,%' AND ',' || features || ',' NOT LIKE '%,files,%';`,
	// Database access moved from Adminer to phpMyAdmin: plans keep it.
	`UPDATE plans SET features = REPLACE(features, 'adminer', 'phpmyadmin');`,
	// Burst: a site's extra instances under load, paid for in minutes.
	// Autoscaled sites become automatic burst; existing plans get the
	// feature with unlimited minutes (0), so nobody loses what they had.
	`ALTER TABLE sites ADD COLUMN burst_mode TEXT NOT NULL DEFAULT 'off';
	ALTER TABLE sites ADD COLUMN burst_until INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN burst_paused INTEGER NOT NULL DEFAULT 0;
	UPDATE sites SET burst_mode = 'auto' WHERE autoscale = 1;
	CREATE TABLE burst_usage (
		site_id     TEXT NOT NULL,
		month_start INTEGER NOT NULL,
		minutes     INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (site_id, month_start)
	);
	CREATE TABLE burst_reports (
		site_id     TEXT NOT NULL,
		node_id     TEXT NOT NULL,
		month_start INTEGER NOT NULL,
		minutes     INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (site_id, node_id, month_start)
	);
	CREATE TABLE burst_charges (
		account_id  INTEGER NOT NULL,
		site_id     TEXT NOT NULL,
		month_start INTEGER NOT NULL,
		minutes     INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, site_id, month_start)
	);
	ALTER TABLE plans ADD COLUMN burst_minutes INTEGER NOT NULL DEFAULT 0;
	UPDATE plans SET features = CASE WHEN features = '' THEN 'burst' ELSE features || ',burst' END
	WHERE ',' || features || ',' NOT LIKE '%,burst,%';
	ALTER TABLE accounts ADD COLUMN burst_credit INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE account_usage ADD COLUMN burst_minutes INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE account_usage ADD COLUMN burst_from_credit INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE account_usage ADD COLUMN burst_credit_taken INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE account_usage ADD COLUMN burst_notified INTEGER NOT NULL DEFAULT 0;`,
	// Outgoing e-mail: see mailer.go.
	mailerSchema,
	// Built-in billing (products' prices, invoices, payments, credit,
	// promotions, taxes): see invoicing.go.
	invoicingSchema,
	// Support tickets: see support.go.
	supportSchema,
	// Log shipping to object storage: see logship.go.
	logshipSchema,
	// Sites shared with users of other accounts: see grants.go.
	grantsSchema,
	// WordPress hardening (the hardening mu-plugin): a list of
	// site.HardeningOptions keys. Sites that exist keep none until their
	// owner chooses (the analyser suggests them): turning off application
	// passwords or shortening sessions under a live site could break an
	// integration nobody here knows about. New sites get the defaults.
	`ALTER TABLE sites ADD COLUMN harden TEXT NOT NULL DEFAULT '';`,
	// Site locks and redirects: see edge.go.
	edgeSchema,
	// AI assistants connected over MCP (OAuth clients and their tokens):
	// see oauth.go.
	oauthSchema,
}

// postgresMigrations holds PostgreSQL versions of the migrations the
// translator can't express, by version (1 is migrations[0]). Their
// statements run as written, one at a time.
var postgresMigrations = map[int]string{}

func (s *Store) migrate(ctx context.Context) error {
	if s.db.postgres {
		return migratePostgres(ctx, s.db.sql, len(migrations))
	}
	// Migration scripts hold several statements: straight to the driver.
	db := s.db.sql
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(v), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
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
