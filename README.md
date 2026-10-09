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
| 🩺 | **Site analyser**: one scored report (A–F) on security, performance and upkeep — plugin/theme versions against known vulnerabilities, risky WordPress settings, database bloat — with one-click fixes | ✅ |
| 🗝️ | **wp-admin without a password**: one click in the panel signs you in as any administrator (a one-time link that opens a normal WordPress session); reset administrators' passwords from the panel | ✅ |
| 🏷️ | **Branding**: your logo and name instead of WordPress's on the login page, admin bar, footer and titles of every site | ✅ |
| 🎨 | **Divi preinstalled**: with your Elegant Themes license saved once, new sites get Divi installed and activated (skippable per site); every site with Divi gets updates and premade layouts while the key stays out of its database and out of Divi's settings | ✅ |
| 🪶 | **WordPress tweaks**: emoji/embed scripts, `<head>` clutter, Heartbeat polling, Dashicons for visitors, nightly database cleanup — applied by a must-use plugin, on by default for new sites | ✅ |
| ⬆️ | **One-click WPGenie updates**: signed releases, automatic rollback if the new version doesn't start | ✅ |
| 🖥️ | Dashboard + REST API + CLI | ✅ |
| 💾 | **Backups** (restic: deduplicated, encrypted) of files + database to this server, S3, B2 or SFTP; retention rules, one-click restore, downloads, restore as a new site | ✅ |
| 🧪 | **Staging**: clone a site, push code / files / database (or chosen tables) back with links rewritten | ✅ |
| 🌐 | Domain aliases, www ↔ bare-domain redirects, primary domain changes, your own TLS certificates | ✅ |
| 🐘 | PHP 8.2 / 8.3 / 8.4 per site (health-checked, switched back if the site breaks), per-site PHP limits | ✅ |
| 📂 | Per-site **SFTP** (chrooted, keys or password) and **phpMyAdmin** on demand (one-time link, temporary DB account) | ✅ |
| 🗂️ | **File manager** in the dashboard: browse, drag-and-drop uploads, edit text files (conflict-safe saves), download files or folders as zip, rename, copy, permissions, extract zip archives; jailed to the site's files | ✅ |
| ⏳ | Job queue: long operations run in the background with progress in the dashboard and CLI | ✅ |
| ⚡ | Full-page cache served by Caddy (Brotli/gzip precompressed, mobile copies when a theme needs them, admin-bar purge), Redis object cache, system cron | ✅ |
| 📈 | Scaling: per-site memory/CPU, replicas with zero-downtime rollouts, per-site DB connection limits | ✅ |
| 🌡️ | **Burst**: off, automatic or on now (for a launch or a sale); a busy site gets extra instances by itself, as many as its plan, the database and the server's free memory and CPU allow, billed in burst minutes (plan allowance + bought top-ups; paused when they run out). Expert targets (CPU, busy PHP workers, response times) under Advanced | ✅ |
| 🛡️ | **Simple security**: protection levels (Basic, Recommended, Strict) instead of firewall knobs; a browser check instead of a CAPTCHA, switched on for every visitor **automatically** while a flood is detected (and alerted), off again when it's over | ✅ |
| 🖼️ | **Image optimisation**: AVIF/WebP copies of uploads, served at the same URL to browsers that accept them | ✅ |
| 🔬 | **Performance insights**: response time percentiles, cache hit rate, slowest URLs, PHP errors by plugin/theme | ✅ |
| ✉️ | **Mail**: mailboxes & aliases (docker-mailserver: Postfix, Dovecot, Rspamd), Roundcube webmail, automatic DKIM, DNS checks, WordPress mail via SMTP, outbound relay | ✅ |
| 🌍 | **CDN**: Cloudflare (real visitor IPs, automatic purges, optional edge caching of pages, SSL/DNS checks) or a bunny.net / any pull zone for static files | ✅ |
| 🚨 | **Monitoring**: Prometheus metrics, alerts for sites down, certificates expiring or invalid, disks filling up and failing backups, by e-mail or signed webhooks (Slack, Discord, Mattermost) | ✅ |
| 🪣 | **Uploads offload**: the media library copied to S3-compatible storage (S3, R2, B2, MinIO), served from there when missing locally; optional removal of old local copies | ✅ |
| 🌐 | **Several servers**: `install.sh --agent` on another VPS, pair it with a one-time code; new sites placed by free memory and disk, moved between servers with seconds of downtime (visitors forwarded until DNS follows), servers drained, a busy site's replicas spread over servers; mutual TLS between servers, nothing else exposed | ✅ |
| 🏢 | **Accounts & billing**: customer and reseller accounts on plans (sites, disk, monthly bandwidth, per-site resources, features), usage metering, suspension (static 503, PHP stopped, nothing deleted), per-user API tokens, provisioning API with single sign-on, **WHMCS** module (also for resellers billing their own customers), **Stripe** subscriptions and metered bandwidth, signed outgoing webhooks | ✅ |
| 🧾 | **Built-in billing** (WHMCS-style): prices per billing cycle and setup fees, a public order page, invoices (gapless numbers, printable), cards with **Stripe** (saved cards charged automatically), UPI and more with **Razorpay**, bank transfer; taxes by country and state (two levels: GST, VAT…), promo codes, credit, prorated plan changes, cancellation, reminders → late fee → suspension → optional closing, bandwidth overage and burst minute invoices, client area, CSV exports. Resellers are billed; their customers are theirs to bill ([guide](docs/BILLING.md)) | ✅ |
| 🛟 | **Support tickets**: departments, priorities, internal notes, attachments, canned replies, auto-close, e-mail notifications; resellers answer their customers' tickets and can escalate | ✅ |
| 📨 | **E-mail to clients**: your SMTP (TLS required with a password), editable templates with previews, an outbox with retries and a log of everything sent | ✅ |
| 🗄️ | **Log shipping**: access, PHP, WAF, security, audit and other logs to S3-compatible storage with [Vector](https://vector.dev) (hardened container, disk spool), bucket retention, smaller local logs, archive browser | ✅ |

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

Using Divi? Save your Elegant Themes account once (a dedicated API key from Account → Username & API
Key, so it can be revoked on its own) and new sites come with Divi installed, activated and licensed:

```bash
wpgenie divi set --username acme < divi-api-key.txt   # the key on stdin, never the command line
wpgenie divi check                                    # does Elegant Themes accept it?
wpgenie site create shop.example.com you@example.com --no-divi   # skip it for one site
wpgenie site divi <site-id>                           # add it to an existing site
```

Scale it when it gets busy — no downtime, the old containers drain before they are removed:

```bash
wpgenie site scale <site-id> --replicas 3 --memory 1024 --cpus 2
wpgenie site cache <site-id> --page on --object on
wpgenie site purge <site-id>
```

Or let it scale itself with burst: extra instances while traffic needs them (automatic), or right
away for a few hours (on); every minute above the normal size is a burst minute:

```bash
wpgenie site burst <site-id> auto
wpgenie site burst <site-id> on --hours 3      # a launch: then back to automatic
wpgenie site burst <site-id>                   # status and minutes used
```

Experts can still set the targets burst uses underneath (CPU, busy PHP workers, response time):

```bash
wpgenie site autoscale <site-id> --on --min 1 --max 4 --target 70 --target-workers 80 --target-ms 800
```

Make it lighter and see where time goes:

```bash
wpgenie site images <site-id> avif,webp        # AVIF/WebP copies of uploads, same URLs
wpgenie site insights <site-id>                # response times, cache hit rate, slow URLs, PHP errors
wpgenie site cdn <site-id> bunny 12345 cdn.example.com   # static files from a pull zone (API key on stdin)
echo "$SECRET_KEY" | wpgenie site offload <site-id> on --endpoint https://s3.eu-central-1.amazonaws.com \
  --region eu-central-1 --bucket my-media --access-key-id AKIA… \
  --public-url https://my-media.s3.eu-central-1.amazonaws.com/<site-id>/uploads --local-days 30
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

Host other people's sites: plans, customer and reseller accounts (each sees only its own sites), usage,
and billing built in (prices, order page, invoices, Stripe, Razorpay, bank transfer, taxes, reminders and
suspension: set it up with [docs/BILLING.md](docs/BILLING.md), which also covers support tickets and log
shipping), or through WHMCS ([integrations/whmcs](integrations/whmcs)) or Stripe subscriptions:

```bash
wpgenie plan create starter --name Starter --sites 3 --disk 10240 --bandwidth 100 --replicas 2 --memory 1024 \
  --features backups,staging,sftp --backup-repos local --overage notify
wpgenie account create "Acme Ltd" --plan starter --user acme     # prints the user's password
wpgenie account assign <site-id> <account-id>                    # give an existing site to an account
wpgenie account usage <account-id> --measure                     # disk and this month's bandwidth vs the plan
wpgenie account suspend <account-id>                             # sites answer 503; nothing is deleted
wpgenie token create --user whmcs --name WHMCS                   # an API token for a billing system
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
wpgenie site phpmyadmin <site-id>                       # one-time link to the database
```

Several servers (on a new VPS, `install.sh --agent` prints the address and a one-time pairing code; port
7443 open between the servers):

```bash
wpgenie node add web-2 203.0.113.7 wpg1-…              # on the panel; or Servers in the dashboard
wpgenie site create shop.example.com you@example.com   # placed on the server with the most room
wpgenie site move <site-id> web-2                      # seconds of maintenance; the old server forwards visitors
wpgenie site move --finish <site-id>                   # once DNS points at the new server
wpgenie site offload <site-id> on …                    # uploads to object storage, then:
wpgenie site spread <site-id> web-2                    # some of its replicas on web-2 too
wpgenie node drain web-2                               # move every site off a server
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
make test-integration   # + validates generated Caddy config with real Caddy, store tests on PostgreSQL too
make test-postgres      # every store-backed test with the panel database on PostgreSQL
make test-e2e           # backups, staging, SFTP, phpMyAdmin against real WordPress/MariaDB/restic
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
internal/offload/   rclone in throwaway containers: uploads offload to S3-compatible storage
internal/sftp/      the chrooted SFTP server's accounts (SHA-512 crypt, authorized_keys)
internal/files/     the dashboard's file manager (os.Root-jailed, acts as the site user)
internal/phpmyadmin/ phpMyAdmin sessions: one-time tokens, temporary DB accounts, proxy
internal/mail/      mail server + webmail containers, domains, mailboxes, DKIM
internal/updater/   WPGenie self-update (signed releases, applier with rollback)
internal/monitor/   Prometheus metrics, alerts (uptime, certificates, disk, backups), e-mail and webhooks
internal/billing/   accounts, plans, quotas, usage, suspension, invoicing (orders, taxes, promotions, Stripe,
                    Razorpay, dunning automation), outgoing webhooks
internal/support/   support tickets: departments, attachments, auto-close, notifications
internal/mailer/    e-mail to people: SMTP settings, templates, the outbox
internal/logship/   log shipping to S3-compatible storage (Vector, spool, exporters, archive)
internal/proxy/     Caddyfile rendering + live reload, Coraza WAF rules and audit log
internal/runtime/   container runtime (the seam for multi-node)
internal/store/     panel state (SQLite, or PostgreSQL)
internal/cluster/   servers of a cluster: CA, pairing, mutual TLS, tunnels, registry
internal/web/       embedded dashboard
images/php/         hardened PHP-FPM + WP-CLI image (page cache, SMTP, plugin profiler)
images/caddy/       Caddy with the Coraza WAF module
images/sftp/        OpenSSH, SFTP only, every login chrooted (built by the daemon on first use)
images/phpmyadmin/  phpMyAdmin behind WPGenie's session front (built by the daemon on first use)
deploy/             installer, compose stack, systemd unit
```

## Contributing

Issues and PRs are welcome — see the [roadmap](docs/ROADMAP.md) for where help is most useful.
Please report security issues privately as described in [SECURITY.md](SECURITY.md).

## License

[AGPL-3.0](LICENSE). You can use, modify and self-host WPGenie freely; if you offer a modified
version as a network service, you must share your changes.
