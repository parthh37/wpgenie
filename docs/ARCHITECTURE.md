# Architecture

WPGenie is a **control plane** (the `wpgenie` Go binary) orchestrating a **data plane** built from
proven components: Caddy, PHP-FPM containers, MariaDB and Valkey. The control plane never sits in
the path of static files and only sees dynamic requests through a sub-millisecond `forward_auth`
check.

## Components

| Component | Role | Why this choice |
|---|---|---|
| `wpgenie` daemon | API, dashboard, shield, analytics, orchestration | Single static binary: trivial install/upgrade, low memory |
| Caddy 2 + Coraza | TLS termination, ACME, HTTP/3, static files, FastCGI, request-body WAF | Automatic certificates; config hot-reload through its admin API; validates before switching. Built locally with the Coraza module (`images/caddy`) |
| PHP-FPM container per site | Runs WordPress | Isolation boundary between customers; per-site resource limits |
| MariaDB 11.4 LTS | WordPress databases | One DB + one user per site, grants limited to that schema |
| Valkey 8 | Object cache | BSD-licensed Redis fork; LRU cache, no persistence |
| SQLite (panel) | Sites, traffic rollups | Zero-ops; WAL mode; a single writer is plenty for panel state |

## Request flow

1. Client connects to Caddy (`network_mode: host`, so it sees real client IPs), directly or through
   Cloudflare (see *CDN*).
2. Hardening rules run first: `wp-config.php`, dotfiles, backups, and PHP inside `uploads/` → 404;
   `xmlrpc.php` → 403 unless the site allows XML-RPC (Jetpack, the mobile apps).
3. Static assets (`*.css`, images, fonts, …) are served directly from disk by Caddy.
4. Dynamic requests hit `forward_auth` → `wpgenie /_shield/check`. The shield classifies the client
   (UA + FCrDNS for search engines), checks deny lists, IP reputation, country rules, the pass cookie
   and rate limits, and returns allow (200) / challenge (403 + PoW page) / block (403) / throttle (429).
   Then, for sites with body inspection on, Coraza runs the OWASP Core Rule Set over the request,
   body included (see *Request-body WAF*).
5. If the page cache is on and the request is cacheable (see below), Caddy serves the cached HTML
   from disk; PHP never runs. The cache is consulted *after* the shield, so cached pages still get
   bot blocking and rate limits.
6. Otherwise the request goes via FastCGI to one of the site's PHP-FPM replicas on
   `127.0.0.1:<port>` (`lb_policy least_conn`).
7. Caddy writes a JSON access log; the analytics ingester tails it and commits hourly rollups.

## Security model (defence in depth)

**Edge**
- Automatic TLS, HSTS, security headers, `Server`/`X-Powered-By` stripped.
- Shield policy (`shield.Decide`, one pure function, tested as a table): attack evidence is blocked
  outright (a solved challenge never excuses it); a pass skips challenges but never rate limits;
  clients that can't run JavaScript (search crawlers, monitors, webhooks) are throttled rather than
  challenged, because a challenge they can't solve would silently become a block (and challenging
  Googlebot de-indexes a site). *Under attack* challenges everyone but pass holders and verified
  crawlers.
- WAF (`shield/inspect.go`): request inspection on what `forward_auth` sees (method, URI, headers,
  never the body) for path traversal, SQL injection, XSS, PHP/shell/JNDI injection, scanner probes
  (`.env`, backups, `phpunit`, plugin `readme.txt` version sniffing) and anonymous user enumeration.
  Paths are decoded and cleaned exactly as Caddy's `php_fastcgi` resolves them (including the split
  at the first `.php`, so `/wp-login.php/x` can't dodge the login limits). A false-positive suite of
  real WordPress traffic guards against blocking customers. The shield runs *before* Caddy's own
  404 rules so probes for forbidden files still count towards a ban.
- Automatic bans (`shield/bans.go`): 5 strikes of unambiguous attack evidence within 10 minutes
  ban an address on every site for 1 hour, doubling per repeat offence up to 24 h. IPv4 bans are
  exact, IPv6 bans (and rate limits) cover the /64. Nobody can get someone else banned: rate
  limits and the admin allowlist never count, requests a browser makes on another site's behalf
  (`Sec-Fetch-Site: cross-site`, e.g. an attack URL in an `<img>`) are blocked but not counted, a
  browser's search that merely looks like SQL/HTML is blocked without a strike, and verified
  crawlers are never banned. Crawler DNS checks distinguish "not Google" (cached 6 h) from "couldn't
  tell" (1 min, treated as an unverified script). Bans live in memory.
- Every path containing `.php` goes through the shield, even with a static-looking suffix
  (`/wp-login.php/x.css` runs `wp-login.php`). URLs are decoded leniently like PHP, so one invalid
  escape can't hide a payload.
- Per site: request limits (requests/s, burst, logins/min) and challenge difficulty override the server
  defaults; a deny list blocks networks outright.
- Per site: optional wp-admin / wp-login.php IP allowlist (`admin-ajax.php` and `admin-post.php`
  stay public for front-end forms; REST/XML-RPC requests carrying credentials are covered) and
  trusted IPs that bypass the shield. The daemon's own health checks prove themselves with a
  per-process secret header, not by coming from loopback (a local tunnel would make every visitor
  look like loopback).
- Shield: AI-crawler blocking, attack-tool UA blocking, spoofed-crawler detection via
  forward-confirmed reverse DNS, per-IP token-bucket rate limits with a much stricter budget for
  `POST /wp-login.php` and `xmlrpc.php`.
- Proof-of-work challenge: stateless HMAC-signed tokens bound to site, client /24 (/64 for IPv6)
  and User-Agent. No third-party CAPTCHA, no cookies until the challenge is passed.

**IP reputation and country rules** (`internal/iprep`)
- Blocklists: Spamhaus DROP/DROPv6 (hijacked and criminal networks) and blocklist.de (addresses
  reported by fail2ban installations for attacks in the last 48 h), refreshed every 6 h and saved,
  so a restart without network keeps them. Each site chooses what listed clients get: nothing,
  a challenge (default: a person on a shared, listed address can still get in) or a block.
- Countries: DB-IP's free country database (CC BY 4.0, credited in the panel), downloaded only once
  a site uses country rules and refreshed monthly. A rule blocks or challenges the listed countries,
  or everyone *but* them. Until the database is loaded, rules are skipped rather than applied to
  everyone.
- Lookups are binary searches over sorted, merged ranges in immutable snapshots swapped atomically:
  no locks on the hot path. A feed that downloads implausibly short keeps its previous list, and every
  list drops private, loopback, CGNAT and special-purpose ranges and anything wider than /8 (/19 for
  IPv6), so a bad download can never cut off the Docker network, a tunnel or half the internet.
- Server-wide lists (panel settings): *allow* is never challenged, blocked or banned on any site;
  *deny* is blocked on every site.
- Where a client is never counts towards a ban (only what it does), and verified search engines are
  exempt from reputation and country rules: de-indexing a site is worse than a crawl from a listed
  network.

**Request-body WAF** (Coraza + OWASP CRS)
- The shield only sees what `forward_auth` passes (method, URI, headers). Attacks in POST bodies and
  JSON (most plugin exploits) need the body, so Caddy is built with the Coraza module
  (`images/caddy`); its OWASP Core Rule Set (paranoia level 1) runs in Caddy after the shield, only on
  dynamic requests.
- WordPress would trip the stock rules constantly (HTML in post content, quotes in passwords, method
  override from the block editor), so the CRS project's
  [WordPress rule exclusions](https://github.com/coreruleset/wordpress-rule-exclusions-plugin) are
  vendored unmodified in `internal/proxy/waf/`. WPGenie's own settings add: inspect bodies up to
  12.5 MB (1 MB without files) and pass the remainder rather than rejecting uploads; no response body
  buffering; `Expect` allowed (HTTP clients send it on large POSTs).
- Per site: off, *log only* (DetectionOnly: matches are recorded, nothing is blocked; the way to
  check a site before blocking) or block. New sites block; sites that existed before the feature keep
  it off until switched on.
- Coraza writes a JSON audit log with only the transaction summary and the rule messages (never
  request headers or bodies: cookies, passwords). The daemon tails it into the security log (site
  from the Host, rule ID, the variable that matched, never the matched data) and truncates it once
  read past 16 MB. Blocks are tagged with `X-WPGenie-Shield: waf` through `handle_errors`, so
  analytics count them as blocked. Body-WAF matches don't count towards bans: without the request's
  headers there's no telling a forged cross-site form submission from its victim.
- A Caddy without the module would reject the whole config, taking every site's changes with it. The
  daemon asks Caddy's `/adapt` endpoint first and renders body inspection only when the module is
  there (re-checked every minute while it isn't). Compose builds `wpgenie/caddy:2` from
  `images/caddy` (`pull_policy: build`: never pulled, so an image of that name on a registry can't
  take its place; rebuilt from cache on every `compose up`, which also covers an upgrade applied by an
  older version's updater). The installer and self-updates build it first, before anything live
  changes, and a rollback re-tags the previous image. 30 sites with
  body inspection cost Caddy about 20 MB.

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
- Panel accounts with roles, TOTP two-factor authentication and an audit log (see *Panel access*);
  the API token (root-readable, for the CLI and scripts) keeps working with full access.
- Strict CSP on the dashboard; DOM built with `textContent`.
- systemd sandboxing (`ProtectSystem`, `ProtectHome`, `NoNewPrivileges`, …).

## Panel access

The installer's API token is the server owner's key: the CLI uses it, and the dashboard asks for it
once, to create the first administrator (`POST /auth/setup` only works while there are no accounts,
and creates the first one atomically). After that people sign in with their own accounts.

- **Roles**: *viewer* (read everything), *operator* (run sites: shield, scaling, caches, updates, scans,
  plugin analysis, CDN, mailboxes, bans), *admin* (also creates and deletes sites, users, server-wide
  security lists and mail settings, self-update). Every route declares the role it needs.
- **Passwords**: PBKDF2-HMAC-SHA256, 600 000 iterations (standard library), 12+ characters. Unknown
  user names take as long as wrong passwords. 10 failures per account or 20 per address (IPv6: per
  /64) in 15 minutes lock sign-in for the rest of the window (the CLI still gets in with the token).
  Attempts are counted before the password check, so parallel bursts can't slip past the limit, and
  password checks run a few at a time so a flood queues instead of starving the server. Setup is
  throttled per address and closes for good once an account exists.
- **Two-factor**: TOTP (RFC 6238, what every authenticator app does), enrolled by scanning a QR code
  rendered on the server (the secret never goes to a third-party QR service) and confirmed with a
  code. Each code works once (the last accepted time step is stored and advanced atomically). Ten
  one-time recovery codes are shown once, stored hashed. An admin can require 2FA for everyone:
  accounts without it can only reach Account until they enrol.
- **Sessions**: a random 256-bit token in an `HttpOnly`, `SameSite=Strict` cookie (`Secure` behind
  TLS); only its SHA-256 is stored, so a copy of the database can't sign anyone in. Sessions end
  after 12 h idle or 7 days, on sign-out, and when the user is disabled, their password changes or
  their 2FA is reset. Users see and revoke their sessions; admins see everyone's.
- **CSRF**: every state-changing request made with the cookie must carry
  `X-Requested-With: wpgenie`; browsers only send custom headers cross-origin after a CORS preflight,
  which the API never grants. The sign-in endpoints require it too, so no site can sign a visitor in or
  out.
- **Audit log**: every change made through the API (method, path, status, error), including refused
  ones, and every sign-in, failure, lockout and recovery-code use, with the client address (Caddy
  passes `{client_ip}`). The newest 20 000 entries are kept. The CLI appears as `api-token`.
- An admin can't demote, disable or delete the last active admin, or delete themselves.

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

**Autoscaling (CPU).** Every 15 s the daemon takes one `docker stats` sample and computes each
site's CPU use as a fraction of its replicas' allowance. Scale-up uses the Kubernetes HPA rule
`ceil(replicas × use / target)` on a 45 s average, waits a minute between scale-ups, and at least
doubles when replicas are saturated (≥ 90 %): a CPU-limited container never *measures* above its
limit, so the ratio under-reports demand exactly when it matters. Scale-down only goes to the
highest count any sample in the last 5 minutes asked for (HPA's stabilization window), so a lull
between bursts doesn't shed capacity. A ±10 % tolerance stops flapping. Scale-ups are capped by the
host's `MemAvailable` (minus 512 MB for MariaDB, Valkey and the OS): container limits don't reserve
memory. Decisions go through the same blue/green reconcile as a manual scale, computed from the
site's state under the lock, so a decision taken before someone resized the site is dropped rather
than reverting the resize. Draining old replicas (up to 2 min) happens after the ops lock is
released, so one site's scale-down never delays another site's scale-up.

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

### CDN (Cloudflare)

A site can sit behind Cloudflare's free plan (DNS record proxied). Cloudflare then caches static
files at its edge, which Caddy marks cacheable only when they exist on disk: images, fonts and media
for 30 days, CSS/JS for 7 (never a PHP-rendered 404 for a missing file). HTML is not cached at the
edge. It changes per visitor (cookies, nonces), and the page cache already serves it without PHP.

**Real client IPs, for every site with no setup.** Behind Cloudflare every connection comes from its
edge, so the shield would rate-limit, challenge and ban Cloudflare instead of the visitor. Caddy's
`trusted_proxies` lists Cloudflare's edge networks and `client_ip_headers CF-Connecting-IP`
resolves `{client_ip}`, which Caddy then sends to the shield as `X-Forwarded-For` on every call,
replacing whatever the client sent. The header is believed only on connections from those networks,
so nobody else can choose the address that gets banned (`TestShieldSeesVisitorBehindCloudflare`
runs this in a real Caddy). `CF-Connecting-IP` is used rather than `X-Forwarded-For`, which
Cloudflare appends to and a client can seed. Analytics use the access log's `client_ip`, so
visitor counts are right too. The edge list is refreshed daily from Cloudflare's API and persisted.
A list that isn't plausibly an edge network (private or overly wide ranges) is rejected, since
trusting it would let its owners impersonate any visitor.

**Automatic purges (optional, per site).** With an API token (Zone: Read, Cache Purge: Purge;
Zone Settings: Read for the SSL check), WPGenie purges the site's own hostnames (never the whole
zone, which may serve other hosts) whenever the site's cache is purged: from the panel, after
WordPress updates, and whenever WordPress purges its page cache. The token never enters the PHP
container, where a compromised plugin could read it. PHP already touches `wpgenie.purged` on every
purge, and the daemon purges the CDN when that marker is newer than the start of its last
successful purge (so a purge during downtime is caught up, and one during an API call isn't lost).
Purges of a site are spaced a minute apart (Free: 5 purge requests/minute per account) and back
off 5 minutes after a failure. Enabling checks the token by finding each domain's zone and purging
it once; a token that can't purge is refused, not stored. A failed CDN purge never fails a
rollback. The status view checks live whether each domain resolves to Cloudflare and warns
about the SSL/TLS mode. *Flexible* makes Cloudflare fetch over HTTP, which Caddy redirects to
HTTPS: an endless redirect.

Certificates keep working behind the proxy: Caddy falls back to the HTTP-01 challenge, which
Cloudflare forwards.

### Cron

New sites set `DISABLE_WP_CRON`; the daemon runs every active site's due events each minute
(`cron_concurrency` at a time, never overlapping per site). Cron runs `wp-cron.php` with plain PHP
rather than WP-CLI because it executes plugin code: `PHP_INI_SCAN_DIR` adds `jail.ini`, the same
`open_basedir` / `disable_functions` jail as web requests. Containers from an older image have no
jail and are skipped; WordPress's own page-view cron keeps working for them.

## WordPress updates and security scans

`POST /sites/{id}/updates` (or the nightly auto-update policy) runs:

1. inventory via WP-CLI (`--skip-plugins --skip-themes`, JSON on stdout only, so PHP warnings on
   stderr can't corrupt it);
2. a health probe of `/` and `/wp-login.php` through Caddy on loopback, with full TLS verification;
3. a snapshot in `/var/lib/wpgenie/snapshots/<site>/<time>/` (root-only): the install minus
   uploads and caches, taken with `tar` *inside the site container*, a `mariadb-dump` from the
   MariaDB container (root password on stdin, never in argv/env), and the list of tables;
4. core, then plugins, then themes; caches purged;
5. a second probe, on a context of its own (a timeout or shutdown mid-update must not also
   cancel the rollback). Only a *healthy → broken* transition triggers a rollback: an unreachable site
   (no certificate yet) or one already failing can't be judged, and rolling back would discard a
   legitimate update. Rollback deletes everything the snapshot covers and extracts it again *as
   the site user inside its container* (a root-side extract could be steered through planted
   symlinks), into a temporary directory first so a broken stream leaves the site untouched, restores the database, drops tables the update created, and rewrites WPGenie's
   root-owned cache wrappers. The newest 3 snapshots per site are kept.

Nightly (in the maintenance window, `maintenance_hour`, default 03:00 server time) every site is
scanned: installed versions against WPVulnerability.net (PHP `version_compare` semantics, answers
cached), `wp core verify-checksums`, `wp plugin verify-checksums`, and PHP files under uploads. The
default auto-update policy, `security`, applies only updates that the database confirms fix a known
vulnerability; `all` applies everything; both go through the snapshot/rollback path. Premium plugins
whose updater only runs with plugins loaded are invisible to WP-CLI in this mode: update them from
wp-admin.

**Plugin analyser** (nightly after the scan, or `POST /sites/{id}/plugins`). For every plugin:

- *wordpress.org*: listed, **closed** (with date and reason: often a security issue, and a closed
  plugin never gets another fix), or not listed (premium/custom). Listed plugins with no release in
  2 years are flagged **abandoned**. Answers are cached for 12 h across sites.
- *Files*: `wp plugin verify-checksums` against the wordpress.org release: modified and added files
  (the usual shape of a backdoor or a nulled copy). Plugins not on wordpress.org can't be verified
  and are said so. PHP files in plugins and themes are also searched for markers of nulled-plugin
  distributors and the WP-VCD malware spread through them; a match in a file that verifies against
  wordpress.org is dropped (security plugins carry such strings in their signatures).
- *Cost*: `images/php/profile.php` renders the front page once with the PHP CLI inside the site's
  container, under the same jail as web requests. Hooks registered before WordPress loads time each
  plugin's main file (`plugin_loaded` fires after each one) and wrap every hook callback in place,
  keeping its key, so `remove_action` still works; time is attributed exclusively (a callback
  triggering another plugin's callbacks doesn't pay for them), and queries are counted per plugin via
  the `query` filter. The render runs twice with an opcache file cache in the container's tmpfs
  (removed afterwards): the first compiles, the second measures what FPM would spend. The page itself
  is discarded (`DONOTCACHEPAGE` keeps it out of the page cache). The report comes from code that ran
  plugins, so it is parsed as untrusted input and only ever displayed.
- Vulnerability counts come from the latest scan. Findings are logged to the site's activity log.

## Updating WPGenie itself

Trust chain: an Ed25519 public key compiled into the binary (`internal/updater/release.pub`) →
signature over `checksums.txt` → SHA-256 of the release tarball, which contains the binary,
`deploy/` and `images/`. GitHub and the network are not trusted. The daemon downloads, verifies and
extracts into `/var/lib/wpgenie/updates/<version>` (regular files under expected names only), then
starts the applier as a transient systemd unit (`systemd-run`): outside the daemon's cgroup, so
restarting the daemon doesn't kill it, and outside its sandbox, so it may write `/usr/local/bin`.
The applier is a copy of the *running* binary. It builds the new PHP image first (nothing live
changes if that fails), swaps binary, `/opt/wpgenie` and the unit file (keeping `.prev` copies),
runs `docker compose up -d`, restarts WPGenie and waits for the API to report the new version. If
anything fails it restores the previous files, re-tags the previous PHP image and restarts the old
version. On success it asks the new daemon to roll every site onto the new image (blue/green, as a
scale). Development builds (`dev`, `git describe` output) never self-update. See
[RELEASING.md](RELEASING.md) for the signing key.

## Mail

Optional, switched on from the panel with a hostname (e.g. `mail.example.com`). The daemon runs two
containers, like site replicas (spec-hash labels decide when to recreate):

- `wpgenie-mail`: docker-mailserver (Postfix, Dovecot, Rspamd for spam filtering, DKIM signing and
  DMARC/SPF checks; ClamAV off). Ports 25, 465, 587, 993. `SPOOF_PROTECTION` stops a mailbox from
  sending as any other address. Its network alias is the public hostname, so sites and Roundcube
  connect by that name and the certificate matches without leaving the Docker network.
- `wpgenie-webmail`: Roundcube on `127.0.0.1:8089`, proxied by Caddy on the mail hostname with the
  shield in front (a POST without a Roundcube session gets the strict login rate limit).
  Fail2ban bans IMAP/SMTP password guessing (10 failures in 10 minutes → 1 h, escalating), ignoring
  Docker's private ranges, whose logins the shield already limits.

TLS: Caddy serves the webmail hostname, so it obtains that certificate; the mail server mounts only
that certificate's directory read-only and reloads Postfix/Dovecot itself on renewal. The mail server
starts once the certificate exists *and* a mailbox exists (it refuses to start without one); until
then `setup` commands run in a throwaway container on the same config. Mailbox passwords go in on
stdin, are shown once and never stored by the panel; the relay password and Roundcube's session
key live in files readable only by their service, never in `docker run` arguments. DKIM keys (RSA 2048, selector `mail`) are
generated by Rspamd once the server runs, without a restart; the panel shows the MX/SPF/DKIM/DMARC
records to publish and checks them against live DNS.

WordPress mail: with SMTP on, a site gets a managed sender mailbox: `wordpress@<domain>` if that
domain's mail is hosted here, otherwise `<site-id>@<mail hostname>` (registering the site's domain
would make this server treat every address on it as local, bouncing mail to its real mailboxes
elsewhere). Its credentials
sit next to `wp-config.php` (root:82 0640) and an mu-plugin wrapper loads `smtp.php` from the image,
which points PHPMailer at the mail server (a visitor's address set as sender by a contact form
becomes Reply-To). Outbound mail can go through a relay (SES, Postmark, …) where port 25 is blocked.

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
/var/lib/wpgenie/snapshots/       pre-update snapshots (root only)
/var/lib/wpgenie/iprep/           IP blocklists and the country database
/var/lib/wpgenie/updates/         staged WPGenie releases, status.json
/var/lib/wpgenie/mail/            mailboxes (data/), mail server config, Roundcube DB
/var/log/wpgenie/access.log       JSON access log (rotated by Caddy)
/var/log/wpgenie/waf.log          Coraza audit log (read and truncated by the daemon)
/opt/wpgenie/                     installed sources (deploy/, images/)
```
