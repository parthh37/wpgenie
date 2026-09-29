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
      WebAuthn/passkeys, per-user API tokens

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
- [x] Domain aliases (served or redirecting), www ↔ apex redirects, primary domain change (links rewritten),
      custom SSL certificates (validated, expiry warnings)
- [x] PHP version switching (8.2 / 8.3 / 8.4, image built on first use, health-checked with automatic switch back),
      per-site PHP settings (memory_limit, uploads, max_execution_time, max_input_vars)
- [x] Job queue for long operations (create/backup/restore/clone/push/PHP) with progress in the UI and CLI
- [ ] Follow-ups (not needed for the phase): refresh a staging site from live in place, per-table diff before a push,
      SFTP rate limits beyond OpenSSH's PerSourcePenalties, backup encryption keys per site, restore to a point in
      time between backups (binary logs)

## Phase 3 — Performance
- [x] Full-page cache: WordPress writes static HTML, Caddy serves it; purge on content change
      (disk-based rather than Souin: no custom Caddy build, and every replica shares it)
- [x] Redis object cache drop-in, graceful + selective flush on the shared Valkey
- [x] Per-site resources (memory/CPU), FPM workers sized from memory
- [x] Replicas with zero-downtime blue/green rollouts; rolling PHP image upgrades
- [x] System cron (jailed) instead of page-view WP-Cron
- [x] Per-site MariaDB connection limits
- [x] CPU autoscaling (HPA-style, stabilization window, host-memory cap)
- [ ] Autoscale on PHP-FPM queue length / response time, not just CPU
- [ ] Admin-bar "purge cache" button; purge on WP-CLI content changes (runs with `--skip-plugins`)
- [ ] Mobile/device cache variants for themes that serve different markup
- [ ] WebP/AVIF conversion, lazy-loading, Brotli/zstd (zstd done)
- [x] CDN integration (Cloudflare free plan): real client IPs, automatic hostname purges, config checks
- [ ] Bunny / generic pull-zone CDNs; optional HTML edge caching (Cloudflare Cache Rules)
- [ ] Per-site slow-request / PHP error insights

## Phase 4 — Email
- [x] docker-mailserver (Postfix, Dovecot, Rspamd) as an optional, panel-managed component
- [x] Mailboxes with quotas, aliases, Roundcube webmail
- [x] Automatic DKIM keys, SPF/DKIM/DMARC record guidance and live DNS verification
- [x] Outbound relay for everything, `wp_mail` through per-site SMTP mailboxes
- [ ] Per-mailbox sending rate limits, mailbox usage in the panel, fail2ban for IMAP/SMTP brute force
- [ ] Autoconfig/autodiscover for mail clients; ManageSieve (filters, vacation replies)

## Platform
- [x] One-click self-update from signed releases with automatic rollback

## Phase 5 — Multi-server
- [ ] `wpgenie agent` + mTLS control channel; `runtime.RemoteAgent`
- [ ] Uploads offload to S3-compatible storage (prerequisite for replicas across nodes)
- [ ] Node placement, site migration between nodes
- [ ] Postgres store for multi-node control plane
- [ ] Prometheus metrics, alerting (uptime, cert expiry, disk)
- [ ] Bandwidth quotas and billing hooks (WHMCS/Stripe), reseller accounts
