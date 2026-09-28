# Architecture

WPGenie is a **control plane** (the `wpgenie` Go binary) orchestrating a **data plane** built from
proven components: Caddy, PHP-FPM containers, MariaDB and Valkey. The control plane never sits in
the path of static files and only sees dynamic requests through a sub-millisecond `forward_auth`
check.

## Components

| Component | Role | Why this choice |
|---|---|---|
| `wpgenie` daemon | API, dashboard, shield, analytics, orchestration | Single static binary: trivial install/upgrade, low memory |
| Caddy 2 | TLS termination, ACME, HTTP/3, static files, FastCGI | Automatic certificates; config hot-reload through its admin API; validates before switching |
| PHP-FPM container per site | Runs WordPress | Isolation boundary between customers; per-site resource limits |
| MariaDB 11.4 LTS | WordPress databases | One DB + one user per site, grants limited to that schema |
| Valkey 8 | Object cache | BSD-licensed Redis fork; LRU cache, no persistence |
| SQLite (panel) | Sites, traffic rollups | Zero-ops; WAL mode; a single writer is plenty for panel state |

## Request flow

1. Client connects to Caddy (`network_mode: host`, so it sees real client IPs).
2. Hardening rules run first: `wp-config.php`, dotfiles, backups, and PHP inside `uploads/` → 404;
   `xmlrpc.php` → 403.
3. Static assets (`*.css`, images, fonts, …) are served directly from disk by Caddy.
4. Dynamic requests hit `forward_auth` → `wpgenie /_shield/check`. The shield classifies the client
   (UA + FCrDNS for search engines), checks the pass cookie and rate limits, and returns
   allow (200) / challenge (403 + PoW page) / block (403) / throttle (429).
5. Allowed requests go to the site's PHP-FPM on `127.0.0.1:<port>` via FastCGI.
6. Caddy writes a JSON access log; the analytics ingester tails it and commits hourly rollups.

## Security model (defence in depth)

**Edge**
- Automatic TLS, HSTS, security headers, `Server`/`X-Powered-By` stripped.
- Shield: AI-crawler blocking, attack-tool UA blocking, spoofed-crawler detection via
  forward-confirmed reverse DNS, per-IP token-bucket rate limits with a much stricter budget for
  `POST /wp-login.php` and `xmlrpc.php`.
- Proof-of-work challenge: stateless HMAC-signed tokens bound to site, client /24 (/64 for IPv6)
  and User-Agent. No third-party CAPTCHA, no cookies until the challenge is passed.

**Site runtime** (`internal/runtime/runtime.go`)
- Runs as uid 82 (`www-data`), `--cap-drop ALL`, `no-new-privileges`, `--read-only` root FS.
- Writable locations: only the site's own docroot and a `noexec` tmpfs. The image's
  `VOLUME /var/www/html` is shadowed with a read-only tmpfs so no hidden writable volume exists.
- Memory / CPU / PID limits per site; PHP-FPM published on loopback only.
- `open_basedir` jails web PHP to the site directory; `exec`, `system`, `proc_open`, … disabled for
  web requests (WP-CLI unaffected).

**WordPress**
- `wp-config.php` lives *above* the docroot, owned by root and read-only to PHP, so a compromised
  plugin can neither serve nor rewrite it. Credentials never appear in env vars or `docker inspect`.
- `DISALLOW_FILE_EDIT`, `FORCE_SSL_ADMIN`, automatic minor core updates, random table prefix,
  random admin username (never `admin`).
- WP-CLI runs with `--skip-plugins --skip-themes`, so a malicious plugin can't hijack panel
  maintenance. Secrets are passed via stdin, never argv, and are redacted from errors.

**Control plane**
- Listens on loopback only; exposed (optionally) through Caddy with TLS.
- Constant-time bearer-token auth; strict CSP on the dashboard; DOM built with `textContent`.
- systemd sandboxing (`ProtectSystem`, `ProtectHome`, `NoNewPrivileges`, …).

## Analytics

- **Bandwidth**: sum of response body bytes (`size`) per site per hour, from Caddy's log.
- **Page views**: `GET` + `200` + `text/html` from non-bot clients.
- **Unique visitors**: HyperLogLog sketch (16 KB, ~0.8% error) per site per UTC day. The inserted
  value is `HMAC(secret, day ‖ IP ‖ UA)` — stable within a day, unlinkable across days, raw IPs are
  never stored. Daily sketches merge losslessly for multi-day ranges.
- Counters and the log offset are committed in **one transaction** → no loss or double counting on
  crash. Log rotation is detected by inode change.

## Scaling plan

The code has explicit seams for going multi-server:

1. **`runtime.Runtime` interface** — today `Docker` (local CLI). A `RemoteAgent` implementation will
   send the same calls over mTLS to a `wpgenie agent` on each node.
2. **Stateless shield tokens** — any node with the shared secret verifies any pass/challenge, so
   Caddy + shield can run on every node with no shared session store.
3. **Rate limits** are per-node (good enough: attackers are spread by DNS/anycast anyway); a shared
   Valkey-backed limiter can be added for strict global limits.
4. **Store** — SQLite now; the `store` package is the only SQL-aware code, so a Postgres backend
   is a contained change when a multi-node control plane needs it.
5. **Analytics** — each node ingests its own log and ships rollups (tiny) to the control plane.
6. **Density** — PHP-FPM `pm = ondemand` means idle sites hold no workers; Caddy serves static
   assets without touching PHP.

## Directory layout on a server

```
/etc/wpgenie/config.json          panel config + secrets (0600)
/etc/wpgenie/infra.env            MariaDB root password (0600)
/etc/wpgenie/caddy/Caddyfile      generated; last config Caddy accepted
/var/lib/wpgenie/wpgenie.db       panel state
/var/lib/wpgenie/sites/<id>/      wp-config.php (root:82 0640)
/var/lib/wpgenie/sites/<id>/public/   WordPress (82:82)
/var/lib/wpgenie/caddy/           certificates
/var/lib/wpgenie/mariadb/         databases
/var/log/wpgenie/access.log       JSON access log (rotated by Caddy)
/opt/wpgenie/                     installed sources (deploy/, images/)
```
