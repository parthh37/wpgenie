# WPGenie

**Open-source, self-hosted managed WordPress hosting — the WP Engine experience on your own VPS.**

WPGenie turns a plain Linux server into a secure, efficient WordPress host: every site in its own
hardened container, automatic HTTPS, a built-in bot shield with AI-crawler blocking and a
privacy-friendly CAPTCHA, real visitor and bandwidth analytics, and a clean dashboard — installed
with one command.

> **Status: early development (v0.1 foundation).** The core hosting path works end to end.
> Many WP Engine–class features are on the [roadmap](docs/ROADMAP.md). Not production-ready yet —
> contributions welcome!

## Features

| | Feature | Status |
|---|---|---|
| 🚀 | One-command install on Ubuntu/Debian VPS | ✅ |
| 🔒 | Automatic SSL (Let's Encrypt / ZeroSSL) & reverse proxy via Caddy, HTTP/3 | ✅ |
| 📦 | Per-site isolation: unprivileged, read-only, capability-less PHP-FPM containers | ✅ |
| 🛡️ | **Shield**: bot classification, AI-crawler blocking, per-IP rate limiting, login brute-force limits | ✅ |
| 🧱 | **WAF**: WordPress-tuned request inspection (SQLi, XSS, traversal, code injection, scanner probes, user enumeration) | ✅ |
| 🧬 | **Request-body WAF**: Coraza + OWASP Core Rule Set with the CRS WordPress exclusions, per site (log-only or block) | ✅ |
| 🌐 | **IP reputation**: Spamhaus DROP + blocklist.de, per-site country rules (DB-IP), server-wide allow/deny lists | ✅ |
| 🎛️ | Per-site shield tuning: XML-RPC toggle, rate limits, login limits, challenge difficulty, deny lists | ✅ |
| 🚫 | Automatic server-wide bans for attackers (escalating), manual bans, security log | ✅ |
| 🔑 | Per-site wp-admin IP allowlist, trusted IPs | ✅ |
| 👥 | **Panel users** with roles (admin / operator / viewer), TOTP two-factor with recovery codes, sessions, **audit log** | ✅ |
| 🧩 | Self-hosted proof-of-work challenge (CAPTCHA without Google/Cloudflare, no tracking) | ✅ |
| ✔️ | Search-engine verification (forward-confirmed reverse DNS) — fake "Googlebots" are caught | ✅ |
| 📊 | Visitor counts (HyperLogLog, no raw IPs stored), page views, **bandwidth per site** | ✅ |
| 🔐 | WordPress hardening: `wp-config.php` outside docroot & read-only, file editor disabled, PHP jailed with `open_basedir`, uploads can't execute PHP | ✅ |
| 🔎 | Nightly security scans: known vulnerabilities ([WPVulnerability](https://www.wpvulnerability.net/)), modified core/plugin files, PHP in uploads | ✅ |
| 🧩 | **Plugin analyser**: closed and abandoned plugins (wordpress.org), modified or nulled copies, load and hook time per plugin | ✅ |
| 🔄 | **WordPress updates**: snapshot → update → health check → automatic rollback; nightly security auto-updates | ✅ |
| ⬆️ | **One-click WPGenie updates**: signed releases, automatic rollback if the new version doesn't start | ✅ |
| 🖥️ | Dashboard + REST API + CLI | ✅ |
| 💾 | **Backups** (restic: deduplicated, encrypted) of files + database to this server, S3, B2 or SFTP; retention rules, one-click restore, downloads, restore as a new site | ✅ |
| 🧪 | **Staging**: clone a site, push code / files / database (or chosen tables) back with links rewritten | ✅ |
| 🌐 | Domain aliases, www ↔ bare-domain redirects, primary domain changes, your own TLS certificates | ✅ |
| 🐘 | PHP 8.2 / 8.3 / 8.4 per site (health-checked, switched back if the site breaks), per-site PHP limits | ✅ |
| 📂 | Per-site **SFTP** (chrooted, keys or password) and **Adminer** on demand (one-time link, temporary DB account) | ✅ |
| ⏳ | Job queue: long operations run in the background with progress in the dashboard and CLI | ✅ |
| ⚡ | Full-page cache served by Caddy, Redis object cache, system cron | ✅ |
| 📈 | Scaling: per-site memory/CPU, replicas with zero-downtime rollouts, per-site DB connection limits | ✅ |
| 🌡️ | **CPU autoscaling**: replicas follow traffic between a min and max, capped by server memory | ✅ |
| ✉️ | **Mail**: mailboxes & aliases (docker-mailserver: Postfix, Dovecot, Rspamd), Roundcube webmail, automatic DKIM, DNS checks, WordPress mail via SMTP, outbound relay | ✅ |
| 🌍 | **Free CDN (Cloudflare)**: real visitor IPs behind the proxy, automatic cache purges, SSL/DNS checks, cache headers on static files | ✅ |
| 🖼️ | Image optimisation | 🚧 Phase 3 |
| 🌐 | Multi-server clusters | 🚧 Phase 5 |

## Install

On a fresh **Ubuntu 22.04/24.04 or Debian 12** server (1 GB+ RAM), as root:

```bash
curl -fsSL https://raw.githubusercontent.com/parthh37/wpgenie/main/deploy/install.sh | \
  PANEL_DOMAIN=panel.example.com ACME_EMAIL=you@example.com bash
```

Both variables are optional. Without `PANEL_DOMAIN` the dashboard is only reachable through an SSH
tunnel (`ssh -L 8088:127.0.0.1:8088 root@server`, then open http://localhost:8088) — the most
secure default.

Create a site (point the domain's DNS at the server first, so the certificate can be issued):

```bash
wpgenie site create example.com you@example.com
```

Scale it when it gets busy — no downtime, the old containers drain before they are removed:

```bash
wpgenie site scale <site-id> --replicas 3 --memory 1024 --cpus 2
wpgenie site cache <site-id> --page on --object on
wpgenie site purge <site-id>
```

Or let it scale itself: replicas are added when CPU use passes the target and removed after five
quiet minutes:

```bash
wpgenie site autoscale <site-id> --on --min 1 --max 4 --target 70
```

Keep WordPress patched. Every update takes a snapshot first and is rolled back automatically if the
site stops working; by default, updates that fix a known vulnerability are applied every night:

```bash
wpgenie site updates <site-id>                 # what's outdated
wpgenie site update <site-id> --all
wpgenie site auto-update <site-id> security    # off | security | all
wpgenie site scan <site-id>                    # vulnerabilities + file integrity
```

Lock down wp-admin, inspect request bodies, keep out known-bad networks and see who the shield
stopped:

```bash
wpgenie site shield <site-id> --admin-allow 203.0.113.7,198.51.100.0/24
wpgenie site shield <site-id> --body-waf block --reputation challenge --country-mode block --countries CN,RU
wpgenie site plugins <site-id> --now           # closed/abandoned/nulled plugins, cost per plugin
wpgenie security bans
wpgenie security reputation
```

The dashboard asks for the installer's API token once, to create the first administrator. Add more
people with roles, and turn on two-factor authentication under Account:

```bash
wpgenie user add jane@example.com --role operator   # prints a generated password
wpgenie user require-2fa on
wpgenie audit                                       # who changed what, from where
```

Mail (point `mail.example.com`'s A record here and open ports 25, 465, 587, 993 first):

```bash
wpgenie mail enable mail.example.com
wpgenie mail domain add example.com            # prints the MX/SPF/DKIM/DMARC records to publish
wpgenie mail box add jane@example.com --quota 2048
wpgenie site smtp <site-id> on                 # WordPress sends its mail through it
```

Backups, staging and access (every site is backed up daily to this server by default; add an
off-server destination for when the server itself is lost):

```bash
echo "$SECRET_KEY" | wpgenie backup repo add b2 offsite --bucket my-backups --key-id 004abc…
wpgenie site backup <site-id> policy --repo <repo-id> --every 24 --keep-daily 7 --keep-weekly 4
wpgenie site backup <site-id> now | ls | restore <repo> <backup>
wpgenie site staging <site-id>                          # staging.<domain>, a full copy
wpgenie site push <staging-id> --files code --db        # live is backed up first
wpgenie site domain <site-id> add www.example.com --redirect
wpgenie site php <site-id> --version 8.4 --memory-limit 512
wpgenie site sftp <site-id> add --password              # sftp -P 2222 <site-id>@example.com
wpgenie site adminer <site-id>                          # one-time link to the database
```

Webmail is at `https://mail.example.com/`. Update WPGenie itself from the dashboard (System tab) or
with `wpgenie update`: releases are signature-checked, and the previous version is restored if the
new one doesn't come up.

## How it works

```
                 Internet  :80 / :443 (TCP+UDP)
                    │
          ┌─────────▼──────────┐     forward_auth      ┌───────────────────────────┐
          │ Caddy (host net)   │ ────────────────────▶ │ wpgenie daemon (loopback) │
          │ auto-TLS, HTTP/3,  │ ◀── allow/challenge ─ │  • Shield  • API/UI       │
          │ static files, logs │                       │  • Analytics  • Orchestr. │
          └──┬──────────────┬──┘                       └──┬──────────┬─────────────┘
   FastCGI   │              │ JSON access log ──────────▶ │          │ docker / SQL
   127.0.0.1 │              │                             │          │
   ┌─────────▼───┐  ┌───────▼─────┐                ┌──────▼─────┐ ┌──▼──────┐
   │ wpg-site-a  │  │ wpg-site-b  │   … one per    │  MariaDB   │ │ Valkey  │
   │ PHP-FPM     │  │ PHP-FPM     │   site         │ (per-site  │ │ (object │
   │ uid 82, ro  │  │ uid 82, ro  │                │  DB+user)  │ │  cache) │
   └─────────────┘  └─────────────┘                └────────────┘ └─────────┘
```

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full design, security model and scaling plan.

## Development

Requirements: Go 1.26+, Docker (for the integration tests and PHP image).

```bash
make test               # unit tests
make test-integration   # + validates generated Caddy config with real Caddy
make test-e2e           # backups, staging, SFTP, Adminer against real WordPress/MariaDB/restic
make php-image          # build the hardened PHP runtime image
make caddy-image        # build Caddy with the Coraza WAF
make build              # ./bin/wpgenie
```

Layout:

```
cmd/wpgenie/        daemon + CLI entrypoint
internal/shield/    bot classification, WAF rules, bans, rate limiting, PoW challenge, policy
internal/iprep/     IP blocklists and the country database for the shield
internal/auth/      panel passwords, TOTP, recovery codes, roles
internal/analytics/ Caddy log tailing → visitors / bandwidth rollups
internal/site/      site lifecycle (with rollback), autoscaling, WordPress updates, scans, plugin analysis,
                    backups, staging, domains and certificates, PHP versions
internal/jobs/      background jobs with progress (create, backup, restore, clone, push)
internal/backup/    restic in throwaway containers (local, S3, B2, SFTP repositories)
internal/sftp/      the chrooted SFTP server's accounts (SHA-512 crypt, authorized_keys)
internal/adminer/   Adminer sessions: one-time tokens, temporary DB accounts, proxy
internal/mail/      mail server + webmail containers, domains, mailboxes, DKIM
internal/updater/   WPGenie self-update (signed releases, applier with rollback)
internal/proxy/     Caddyfile rendering + live reload, Coraza WAF rules and audit log
internal/runtime/   container runtime (the seam for multi-node)
internal/store/     panel state (SQLite)
internal/web/       embedded dashboard
images/php/         hardened PHP-FPM + WP-CLI image (page cache, SMTP, plugin profiler)
images/caddy/       Caddy with the Coraza WAF module
images/sftp/        OpenSSH, SFTP only, every login chrooted (built by the daemon on first use)
images/adminer/     Adminer behind WPGenie's session front (built by the daemon on first use)
deploy/             installer, compose stack, systemd unit
```

## Contributing

Issues and PRs are welcome — see the [roadmap](docs/ROADMAP.md) for where help is most useful.
Please report security issues privately as described in [SECURITY.md](SECURITY.md).

## License

[AGPL-3.0](LICENSE). You can use, modify and self-host WPGenie freely; if you offer a modified
version as a network service, you must share your changes.
