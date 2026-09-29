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

## Phase 1 — Security & maintenance
- [x] **WAF**: WordPress-tuned request inspection in the shield (URI/headers), automatic bans
- [ ] Body inspection: custom Caddy build with Coraza + OWASP CRS (needs WordPress exclusions)
- [x] **Vulnerability checks**: nightly `wp plugin/theme/core list` → match against
      [WPVulnerability.net](https://www.wpvulnerability.net/) (free, no key) and optionally
      Wordfence Intelligence / Patchstack; alert + dashboard badges
- [ ] **Plugin analyser**: inventory, abandoned plugins (not updated in 2y / closed on wp.org),
      nulled/modified detection via `wp plugin verify-checksums`, performance cost per plugin
- [x] **Update manager**: scheduled updates → snapshot → update → HTTP + visual health check →
      auto-rollback on failure (WP Engine Smart Plugin Manager equivalent)
- [x] **Malware / integrity scanning**: `wp core verify-checksums`, new-PHP-file alerts in uploads
- [ ] Panel users with roles, TOTP 2FA, sessions, audit log
- [ ] Global IP reputation (crowd-sourced blocklists), country rules, per-site allow/deny lists
- [x] Per-site WAF toggle, wp-admin IP allowlist, trusted IPs
- [ ] Shield settings per site: xmlrpc toggle, custom rate limits, challenge difficulty

## Phase 2 — Backups & environments
- [ ] Backups with restic (dedup + encryption): files + `mysqldump`, to local/S3/B2/SFTP
- [ ] Retention policies, one-click restore, download backup
- [x] Automatic pre-update snapshots (used by the update manager)
- [ ] Staging environments: clone, search-replace URLs, push staging → live (files/DB selective)
- [ ] Per-site SFTP (chrooted), Adminer on demand with short-lived tokens
- [ ] Domain aliases, www ↔ apex redirects, custom SSL certificates
- [ ] PHP version switching (8.2 / 8.3 / 8.4), per-site PHP settings
- [ ] Job queue for long operations (create/backup/restore) with progress in the UI

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
- [ ] CDN integration (Cloudflare/Bunny) with automatic purge
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
