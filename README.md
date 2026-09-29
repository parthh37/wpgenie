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
| 🧩 | Self-hosted proof-of-work challenge (CAPTCHA without Google/Cloudflare, no tracking) | ✅ |
| ✔️ | Search-engine verification (forward-confirmed reverse DNS) — fake "Googlebots" are caught | ✅ |
| 📊 | Visitor counts (HyperLogLog, no raw IPs stored), page views, **bandwidth per site** | ✅ |
| 🔐 | WordPress hardening: `wp-config.php` outside docroot & read-only, file editor disabled, PHP jailed with `open_basedir`, uploads can't execute PHP | ✅ |
| 🖥️ | Dashboard + REST API + CLI | ✅ (basic) |
| 🧱 | Web Application Firewall (Coraza + OWASP CRS) | 🚧 Phase 1 |
| 🔎 | Plugin analyser & automatic vulnerability checks | 🚧 Phase 1 |
| 🔄 | Update manager with pre-update snapshot + health check + auto-rollback | 🚧 Phase 1 |
| 💾 | Backups (files + DB) to local/S3 with one-click restore | 🚧 Phase 2 |
| 🧪 | Staging environments, SFTP, PHP version switching | 🚧 Phase 2 |
| ⚡ | Full-page cache served by Caddy, Redis object cache, system cron | ✅ |
| 📈 | Scaling: per-site memory/CPU, replicas with zero-downtime rollouts, per-site DB connection limits | ✅ |
| 🖼️ | Image optimisation, CDN integration | 🚧 Phase 3 |
| ✉️ | Mailboxes (Stalwart), DKIM/SPF/DMARC, transactional mail | 🚧 Phase 4 |
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

After upgrading WPGenie (which rebuilds the PHP image), `wpgenie site scale <site-id>` with no
flags rolls a site onto the new image.

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
make php-image          # build the hardened PHP runtime image
make build              # ./bin/wpgenie
```

Layout:

```
cmd/wpgenie/        daemon + CLI entrypoint
internal/shield/    bot classification, rate limiting, PoW challenge, policy
internal/analytics/ Caddy log tailing → visitors / bandwidth rollups
internal/site/      site lifecycle orchestration (with rollback)
internal/proxy/     Caddyfile rendering + live reload
internal/runtime/   container runtime (the seam for multi-node)
internal/store/     panel state (SQLite)
internal/web/       embedded dashboard
images/php/         hardened PHP-FPM + WP-CLI image
deploy/             installer, compose stack, systemd unit
```

## Contributing

Issues and PRs are welcome — see the [roadmap](docs/ROADMAP.md) for where help is most useful.
Please report security issues privately as described in [SECURITY.md](SECURITY.md).

## License

[AGPL-3.0](LICENSE). You can use, modify and self-host WPGenie freely; if you offer a modified
version as a network service, you must share your changes.
