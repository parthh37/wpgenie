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
5. If the page cache is on and the request is cacheable (see below), Caddy serves the cached HTML
   from disk; PHP never runs. The cache is consulted *after* the shield, so cached pages still get
   bot blocking and rate limits.
6. Otherwise the request goes via FastCGI to one of the site's PHP-FPM replicas on
   `127.0.0.1:<port>` (`lb_policy least_conn`).
7. Caddy writes a JSON access log; the analytics ingester tails it and commits hourly rollups.

## Security model (defence in depth)

**Edge**
- Automatic TLS, HSTS, security headers, `Server`/`X-Powered-By` stripped.
- Shield: AI-crawler blocking, attack-tool UA blocking, spoofed-crawler detection via
  forward-confirmed reverse DNS, per-IP token-bucket rate limits with a much stricter budget for
  `POST /wp-login.php` and `xmlrpc.php`.
- Proof-of-work challenge: stateless HMAC-signed tokens bound to site, client /24 (/64 for IPv6)
  and User-Agent. No third-party CAPTCHA, no cookies until the challenge is passed.

**Static files (Caddy)**

Caddy's `file_server` follows symlinks, and a site can create symlinks in its own docroot (plugin
archive extraction, WP-CLI, anything not jailed by `open_basedir`). Path-based deny rules only see
the request path (`/wp-content/uploads/x.txt`), never the target. So without the controls below, a
symlink planted by site A would make Caddy serve site B's `wp-config.php` or its own TLS private
keys. Two independent layers:

- **No symlinks, enforced by the kernel.** Caddy never mounts `/var/lib/wpgenie/sites`. It mounts
  `/var/lib/wpgenie/sites.nosymfollow`, a `bind,ro,nosymfollow` view of it
  (`deploy/var-lib-wpgenie-sites.nosymfollow.mount`, Linux 5.10+), where opening any symlink fails
  with `ELOOP`, wherever it points. PHP containers mount the real directory, so WordPress is
  unaffected; the only cost is that symlinks inside a docroot aren't served as static files.
  `install.sh` checks the flag inside the running Caddy container and stops Caddy if it's missing
  (Docker's `local` volume driver, for example, silently drops it).
- **Unprivileged Caddy.** Caddy runs as the `wpgenie-caddy` system user with only
  `NET_BIND_SERVICE`, outside www-data's group (82). Site directories are `0751` (traverse, no
  listing), so it reaches `public/` but can't read `wp-config.php` (`root:82 0640`) even if it
  opens it. This layer alone doesn't protect Caddy's own certificates or other sites' public
  files. That's the first layer's job.

`TestStaticFilesDoNotFollowSymlinks` (Docker) exercises both layers with the rendered Caddyfile
and the unit's mount options. It also checks that the unprotected setup does leak, so the test
proves something. Serving only each docroot to Caddy was rejected: compose mounts are static, so
every new site would need a Caddy restart or per-site host mounts.

**Site runtime** (`internal/runtime/runtime.go`)
- Runs as uid 82 (`www-data`), `--cap-drop ALL`, `no-new-privileges`, `--read-only` root FS.
- Writable locations: only the site's own docroot and a `noexec` tmpfs. The image's
  `VOLUME /var/www/html` is shadowed with a read-only tmpfs so no hidden writable volume exists.
- Memory / CPU / PID limits per site; PHP-FPM published on loopback only.
- `open_basedir` jails web PHP to the site directory; `exec`, `system`, `proc_open`, … disabled for
  web requests (WP-CLI unaffected).

**WordPress**
- `wp-config.php` lives *above* the docroot, owned by root and read-only to PHP, so a compromised
  plugin can neither serve nor rewrite it (not even through a symlink; see *Static files*). Credentials never appear in env vars or `docker inspect`.
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

## Scaling a site

A site is scaled along three independent axes, all from the dashboard, the API
(`PUT /api/v1/sites/{id}/resources`, `PUT …/cache`, `POST …/cache/purge`) or the CLI
(`wpgenie site scale|cache|purge`).

**Up — resources per replica.** Each PHP-FPM container gets a memory and CPU limit.
`pm.max_children` is derived from the memory limit (`runtime.FPMMaxChildren`) and passed in as
`WPG_MAX_CHILDREN`, so a traffic spike queues requests instead of getting the container OOM-killed.

**Out — replicas.** A site runs 1–`max_replicas` identical containers, each on its own loopback
port (`site_upstreams` table). All replicas bind-mount the same docroot, so uploads and plugin
installs are immediately visible to every replica: no shared storage is needed on a single node.
Caddy load-balances with `least_conn` and retries connection failures for up to 5s. Passive health
checks (`max_fails 3` within 10s) are only enabled with 2+ replicas: on a single replica, marking the
only upstream down would turn one slow request into 10s of 503s.

Every change is a **blue/green reconcile** (`site.Service.reconcile`):

1. keep replicas whose `wpgenie.spec` label matches the desired spec (image ID, memory, CPU,
   workers);
2. start the missing ones on fresh ports and wait until PHP-FPM listens (checked inside the
   container via `/proc/net/tcp`: docker-proxy accepts connections before PHP does);
3. switch Caddy to exactly the new set (atomic reload);
4. wait until the old replicas have no active connections, then remove them.

Any failure before step 3 removes what was started and leaves the site untouched. If the proxy
update fails, the old upstreams are re-applied first, and the new replicas are only removed once that
succeeds (Caddy may already be routing to them); if the proxy state can't be confirmed, both sets
keep running. After the switch the work continues even if the API caller disconnects, and ports
still published by any WPGenie container are never re-allocated. Signalling
PHP-FPM (`SIGQUIT`) is not relied on to finish in-flight requests: in testing it dropped them,
which is why step 4 drains by connection count. Because the image ID is part of the spec, rebuilding
the PHP image and running `wpgenie site scale <id>` rolls a site onto it with no downtime.

**Database fairness.** Every busy worker holds a MariaDB connection and all sites share one server.
Each site's DB user gets `MAX_USER_CONNECTIONS = replicas × workers + 5`, and a site may not be
scaled beyond half of `db_max_connections`, so one busy or attacked site can't take the others
down with "Error establishing a database connection".

### Caching

**Page cache** (`images/php/page-cache.php`, loaded by an mu-plugin wrapper). WordPress renders a
page once and stores it at `wp-content/cache/wpgenie/<path>/index.html`; Caddy serves that file to
later visitors. A page is stored only for anonymous `GET` requests without a query string, whose
cookies are all on a known-harmless allowlist (analytics, WordPress's test cookie: any other cookie
may personalise the HTML, and a stored page is served to everyone), with no active PHP session, a
`200 text/html` response without `Set-Cookie` or `no-cache`, a path ending in `/`, and when nothing
set `DONOTCACHEPAGE` (WooCommerce cart/checkout do). Caddy applies the same request checks with a
stricter cookie rule, so a cached page is only served to requests that could have produced it.
Any content change (post published/edited, comment approved, menu, widgets, theme, plugins,
stock) purges the whole cache: atomic rename, then delete. Pages expire after 10h, below
WordPress's 12h nonce tick, so cached forms never carry expired nonces.

**Object cache.** The Redis drop-in from the redis-cache plugin (pinned by checksum) ships in the
image and is loaded by a wrapper in `wp-content/object-cache.php`. The wrapper forces
`WP_REDIS_GRACEFUL` (Valkey down → site keeps working uncached) and `WP_REDIS_SELECTIVE_FLUSH`
(without it a flush — which WordPress core runs on updates — is a `FLUSHDB` that empties every
site's cache). Keys are prefixed with the site ID. Panel purges delete the site's keys straight from
Valkey (a server-side `SCAN` + `UNLINK` script), never via `wp cache flush`: WP-CLI would load the
site-replaceable drop-in outside the PHP jail.

The cache code lives read-only in the image (`/usr/local/share/wpgenie`); sites only get one-line
wrappers. A compromised plugin can't alter the cache logic, and an image upgrade updates it
everywhere. The daemon writes those wrappers as root into a directory the site controls, so all
such file operations go through `os.Root`, which refuses to follow symlinks out of the docroot.

### Cron

New sites set `DISABLE_WP_CRON`; the daemon runs every active site's due events each minute
(`cron_concurrency` at a time, never overlapping per site). Cron runs `wp-cron.php` with plain PHP
rather than WP-CLI because it executes plugin code: `PHP_INI_SCAN_DIR` adds `jail.ini`, the same
`open_basedir` / `disable_functions` jail as web requests. Containers from an older image have no
jail and are skipped; WordPress's own page-view cron keeps working for them.

## Multi-server plan

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
6. **Upstreams are addresses** (`host:port`) in the proxy layer, so replicas on other nodes slot
   into the same `php_fastcgi` load balancer. Across nodes the docroot is no longer shared:
   uploads need offloading to object storage (or a shared filesystem) first.
7. **Density** — PHP-FPM `pm = ondemand` means idle sites hold no workers; Caddy serves static
   assets and cached pages without touching PHP.

## Directory layout on a server

```
/etc/wpgenie/config.json          panel config + secrets (0600)
/etc/wpgenie/infra.env            MariaDB root password (0600)
/etc/wpgenie/caddy/Caddyfile      generated; last config Caddy accepted
/var/lib/wpgenie/wpgenie.db       panel state
/var/lib/wpgenie/sites/<id>/      root:82 0751; wp-config.php (root:82 0640)
/var/lib/wpgenie/sites/<id>/public/   WordPress (82:82)
/var/lib/wpgenie/sites.nosymfollow/   read-only, symlink-free view of sites/ (Caddy's only view)
/var/lib/wpgenie/caddy/           certificates (owned by wpgenie-caddy)
/var/lib/wpgenie/mariadb/         databases
/var/log/wpgenie/access.log       JSON access log (rotated by Caddy)
/opt/wpgenie/                     installed sources (deploy/, images/)
```
