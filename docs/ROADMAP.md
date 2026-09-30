# Roadmap

Phases are ordered by what makes WPGenie safe to run real sites on. Each item is a good first
issue candidate once its phase starts.

## Phase 0 — Foundation ✅ (v0.1)
- [x] Installer, compose stack, systemd unit
- [x] Site create/delete with full rollback on failure
- [x] Hardened per-site PHP-FPM containers + WP-CLI image
- [x] Caddy auto-TLS, generated config, live reload
- [x] Shield: bot/AI classification, FCrDNS crawler verification, rate limiting, PoW challenge
- [x] Shield policy (`shield.Decide`)
- [x] Analytics: visitors (HLL), page views, bandwidth, bot hits, shield blocks
- [x] Dashboard, REST API, CLI

## Phase 1 — Security & maintenance ✅
- [x] **WAF**: WordPress-tuned request inspection in the shield (URI/headers), automatic bans
- [x] Body inspection: custom Caddy build with Coraza + OWASP CRS and the CRS WordPress exclusions,
      per site (log-only or block), matches in the security log
- [x] **Vulnerability checks**: nightly `wp plugin/theme/core list` → match against
      [WPVulnerability.net](https://www.wpvulnerability.net/) (free, no key) and optionally
      Wordfence Intelligence / Patchstack; alert + dashboard badges
- [x] **Plugin analyser**: inventory, abandoned plugins (not updated in 2y / closed on wp.org),
      nulled/modified detection via `wp plugin verify-checksums` (+ nulled/WP-VCD markers),
      performance cost per plugin (jailed front-page profile: load, hooks, queries)
- [x] **Update manager**: scheduled updates → snapshot → update → HTTP + visual health check →
      auto-rollback on failure (WP Engine Smart Plugin Manager equivalent)
- [x] **Malware / integrity scanning**: `wp core verify-checksums`, new-PHP-file alerts in uploads
- [x] Panel users with roles, TOTP 2FA (+ recovery codes, optional requirement), sessions, audit log
- [x] Global IP reputation (Spamhaus DROP, blocklist.de), country rules (DB-IP), per-site and
      server-wide allow/deny lists
- [x] Per-site WAF toggle, wp-admin IP allowlist, trusted IPs
- [x] Shield settings per site: xmlrpc toggle, custom rate limits, challenge difficulty
- [ ] Follow-ups (not needed for the phase): body-WAF matches towards automatic bans, CrowdSec/AbuseIPDB feeds (need accounts),
      WebAuthn/passkeys (per-user API tokens: done with Phase 5's accounts)

## Phase 2 — Backups & environments ✅
- [x] Backups with restic (dedup + encryption): files + `mariadb-dump`, to local/S3/B2/SFTP; restic runs in a
      throwaway container that only sees the site, secrets on stdin; one repository shared by every site (WordPress
      deduplicates across them)
- [x] Retention policies (restic keep rules per site, weekly prune + check), one-click restore (files and/or database,
      safety backup first), download backup (streamed `.tar.gz`), restore any backup (deleted sites too) as a new site
- [x] Automatic pre-update snapshots (used by the update manager)
- [x] Staging environments: clone, search-replace URLs (regex: plain and JSON-escaped links, never a longer hostname),
      push staging → live (code / all files / database or chosen tables; exported with links rewritten, live backed
      up first); staging is `WP_ENVIRONMENT_TYPE=staging`, noindex, no cron, no mail
- [x] Per-site SFTP (one OpenSSH container, every login chrooted to its site, SFTP only), Adminer on demand on the
      site's own domain with one-time tokens and temporary database accounts
- [x] File manager in the dashboard (browse, upload, edit, download as zip, move, copy, permissions, extract), jailed
      to the docroot with `os.Root` and acting as the site user
- [x] Domain aliases (served or redirecting), www ↔ apex redirects, primary domain change (links rewritten),
      custom SSL certificates (validated, expiry warnings)
- [x] PHP version switching (8.2 / 8.3 / 8.4, image built on first use, health-checked with automatic switch back),
      per-site PHP settings (memory_limit, uploads, max_execution_time, max_input_vars)
- [x] Job queue for long operations (create/backup/restore/clone/push/PHP) with progress in the UI and CLI
- [ ] Follow-ups (not needed for the phase): refresh a staging site from live in place, per-table diff before a push,
      SFTP rate limits beyond OpenSSH's PerSourcePenalties, backup encryption keys per site, restore to a point in
      time between backups (binary logs)

## Phase 3 — Performance ✅
- [x] Full-page cache: WordPress writes static HTML, Caddy serves it; purge on content change
      (disk-based rather than Souin: no custom Caddy build, and every replica shares it)
- [x] Redis object cache drop-in, graceful + selective flush on the shared Valkey
- [x] Per-site resources (memory/CPU), FPM workers sized from memory
- [x] Replicas with zero-downtime blue/green rollouts; rolling PHP image upgrades
- [x] System cron (jailed) instead of page-view WP-Cron
- [x] Per-site MariaDB connection limits
- [x] CPU autoscaling (HPA-style, stabilization window, host-memory cap)
- [x] Burst: autoscaling as customers see it (off / automatic / on now), ceiling computed from the plan,
  the database and server load, billed in burst minutes (plan allowance + credit, paused at zero)
- [x] Simple security: protection levels, automatic Under attack on floods
- [x] Autoscale on PHP-FPM load (requests per worker, the listen queue included, read from the replica's socket
      table) and on the 95th percentile of PHP response times (from Caddy's log, only under load); the highest
      proposal of the three metrics wins
- [x] Admin-bar "Purge cache" button (editors and up; page cache, object cache and CDN); content changed with
      WP-CLI purges too (mu-plugins load despite `--skip-plugins`: covered end to end)
- [x] Mobile/desktop page cache copies for pages that ask `wp_is_mobile()` (Caddy picks with the same rule, client
      hint first), or for every page when the theme detects phones itself
- [x] WebP/AVIF copies of uploads (GD, in the site's container; new uploads by cron, the rest by a job and nightly),
      negotiated by Caddy at the same URL; Brotli and gzip copies of cached pages served precompressed (zstd/gzip on
      the fly for the rest); lazy-loading is WordPress core's (`loading="lazy"`, `fetchpriority`), left on
- [x] Pull-zone CDNs: bunny.net (checked, purged with the site) or any other on its own hostname (static links
      rewritten, fonts get CORS); optional HTML edge caching on Cloudflare (a Cache Rule per zone that only keeps
      what Caddy marks cacheable: page-cache hits)
- [x] Per-site insights: PHP response time percentiles and histogram, page cache hit rate, slowest URLs, PHP errors
      grouped by message and place with the plugin or theme responsible
- [ ] Follow-ups (not needed for the phase): Brotli for dynamic responses and static CSS/JS (a Caddy encoder module
      or precompressed assets), stack traces of slow requests (FPM's slowlog needs ptrace, which the hardened
      containers don't allow), GIF/animated image conversion, edge HTML caching on Bunny, alerts on error spikes

## Phase 4 — Email
- [x] docker-mailserver (Postfix, Dovecot, Rspamd) as an optional, panel-managed component
- [x] Mailboxes with quotas, aliases, Roundcube webmail
- [x] Automatic DKIM keys, SPF/DKIM/DMARC record guidance and live DNS verification
- [x] Outbound relay for everything, `wp_mail` through per-site SMTP mailboxes
- [ ] Per-mailbox sending rate limits, mailbox usage in the panel, fail2ban for IMAP/SMTP brute force
- [ ] Autoconfig/autodiscover for mail clients; ManageSieve (filters, vacation replies)

## Platform
- [x] One-click self-update from signed releases with automatic rollback

## Phase 5 — Multi-server ✅
- [x] `wpgenie agent` + mTLS control channel: every server a full data plane run by the same daemon; key-pinned
      one-time pairing, a private CA on the panel, identities checked by name; the panel forwards site operations as
      the signed-in user; HTTP/2 tunnels for everything that crosses servers (nothing published)
- [x] Uploads offload to S3-compatible storage (prerequisite for replicas across nodes): rclone in a throwaway
      container (keys on stdin; it only sees files the daemon copied out through `os.Root`), new uploads within a
      minute and a nightly full comparison, WordPress deletes propagated through a locked queue, missing uploads
      served by Caddy from the bucket's public URL (also for staging clones), local copies optionally removed after
      N days once the bucket is confirmed to hold them (and copied back on demand)
- [x] Node placement (memory promised vs. capacity, disk, draining), site migration between nodes (two-pass copy,
      seconds of maintenance, visitors and ACME challenges forwarded from the old server with their addresses until
      DNS moves), drain; replicas spread over several servers (writes stay home, code pushed on change, database and
      cache through mTLS links)
- [x] Postgres store for multi-node control plane (`database_url`; SQLite stays the default; `wpgenie store migrate-to-postgres`)
- [x] Prometheus metrics, alerting (uptime, cert expiry, disk)
- [x] Bandwidth quotas and billing hooks (WHMCS/Stripe), reseller accounts: customer and reseller accounts on
      plans (sites, disk, monthly bandwidth, per-site resources, features) enforced on every tenant route (default
      deny, ownership checked centrally), usage metering (bandwidth from the rollups, disk nightly and on demand),
      80/100% notifications and overage suspension, suspension as a static 503 with PHP stopped, per-user API
      tokens, provisioning API with idempotent creation and one-time sign-on links, WHMCS module, Stripe
      webhooks and metered bandwidth, signed outgoing webhooks with a retry queue
- [x] Object cache isolation: a Valkey ACL user per site, limited to its own keys (tenants' PHP can't touch
      another site's cache), default user off
- [ ] Follow-ups (not needed for the phase): DNS ownership checks for domains tenants add, backups in the disk quota,
      tenant-managed mailboxes within a plan, usage history beyond the current month, per-account audit log;
      several panel processes behind one database (leader election for the startup sweeps and the loops); a
      shared rate limiter across servers; spreading a site's staging copy with it; incremental (rsync-style) copies
      for moves and spread pushes