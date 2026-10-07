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
| Panel store: SQLite, or PostgreSQL | Sites, users, sessions, jobs, traffic rollups | SQLite by default: zero-ops, WAL mode, a single writer is plenty for one node. PostgreSQL (`database_url`) for a managed or replicated panel database; see *Panel database* |
| restic (container per command) | Backups | Deduplicated, compressed, encrypted; local, S3, B2, SFTP; verifiable |
| rclone (container per command) | Uploads offload to S3-compatible storage | Every S3 dialect; statistics as JSON; no daemon to keep running |
| OpenSSH (optional container) | SFTP for site files | Chroot and SFTP-only logins built in; nothing custom exposed to the internet |
| phpMyAdmin (on demand) | Database access | Official image, PHP's built-in server; runs only while someone uses it, behind WPGenie's tokens |
| `wpgenie agent` (other servers) | A node of a cluster: the same data plane for the sites placed on it | The same binary and code paths as a single server; the panel drives it over mutual TLS (see *Several servers*) |
| Vector (optional container) | Ships logs to S3-compatible storage | Open source (MPL-2.0), disk-buffered, every S3 dialect; one hardened container, configured by the daemon (see *Log shipping*) |

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
   from disk (its Brotli or gzip copy, as the browser accepts; the phone or computer copy for pages
   that differ); PHP never runs. The cache is consulted *after* the shield, so cached pages still get
   bot blocking and rate limits. JPEG/PNG uploads with converted copies are served as AVIF or WebP
   to browsers that accept them (see *Images*).
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
- *Automatic* (`auto`, new sites' mode) is Standard until the shield detects a flood
  (`shield/attack.go`), then Under attack until 15 minutes without one. Per site, in memory:
  requests in 10-second buckets over a sliding minute, against a baseline (a running average kept
  only while not under attack); a flood is at least 1,200 requests a minute and 5× the baseline, or
  30 different addresses refused for rate limits, bans or attack evidence in a minute (a distributed
  flood keeps each client near its limit; blocks for being in a country or on a blocklist don't
  count). Starts and ends are in the security log and the site's activity, and are a monitoring
  alert; the owner can end one early ("It's over").
- Protection levels (`site/protection.go`) are named sets of the strictness settings (the two WAFs,
  blocklists, rate limits, challenge difficulty) so owners pick Basic, Recommended (new sites'
  defaults) or Strict instead of tuning each; settings matching none show as custom. Lists, country
  rules, XML-RPC and AI crawlers are left alone by a level.
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

- **Roles**: staff are *viewer* (read everything), *operator* (run sites: shield, scaling, caches, updates, scans,
  plugin analysis, CDN, mailboxes, bans, backups and restores, staging and pushes, domains and
  certificates, PHP, SFTP logins, the file manager, phpMyAdmin, wp-admin sign-in and WordPress administrators' passwords, WordPress
  tweaks and the analyser's fixes; support tickets and canned replies, the order list and the e-mail
  log), *admin* (also creates and deletes sites, users, backup destinations and backups, server-wide
  security lists and mail settings, accounts, plans and billing (invoices, payments, taxes, gateways),
  help desk and e-mail settings, log shipping, self-update). Every route declares the staff role it needs. Tenants are *customer* and *reseller*
  users: they belong to an account and reach only their own sites and account (see *Accounts, plans
  and billing*); their role always follows their account's kind and has no staff level at all.
- **API tokens**: besides the installer's token, every user can create named API tokens (for scripts and
  billing systems). A token acts as its user, with the user's current role and account, never more;
  only its SHA-256 is stored, it is shown once, may expire, shows when and from where it was last used,
  and is revoked by the user or an administrator. Tokens are created from a signed-in session (or by an
  administrator), never with another token, so a leaked token can't mint successors that outlive its
  revocation. A request with a token needs no code (a script can't type one), but while the panel
  requires two-factor authentication only users who have it can make or use tokens: otherwise a password
  alone would buy a credential past the requirement. Resetting a user's password or second factor (an
  administrator's answer to a compromised account) revokes their tokens.
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

## Accounts, plans and billing

WPGenie can host other people's sites: **accounts** (organisations) of kind *customer* or *reseller*, each on
an admin-defined **plan**, with their own users. It is a security boundary, built default-deny
(`internal/billing` for the rules, `internal/api/tenancy.go` for access). Invoicing them is *Built-in
billing* (below), or an external system's job (WHMCS, Stripe subscriptions).

**Model.** An account has a name, a kind, a status (*active*, *suspended* with the reason, *terminated*, or
*pending*: ordered from the order page and awaiting its first payment), a plan, an optional reseller
(customers only: resellers are top-level, one level deep), a billing profile (see *Built-in billing*) and
external billing IDs (WHMCS service, Stripe customer and subscription). Users with `account_id` 0 are staff and keep
exactly their old behaviour. **Site ownership** is its own table (`site_accounts`, no foreign key to
`sites`): a site may live on another node; deleting a site removes its ownership in the same
transaction, and sites nobody owns are staff-only. A staging copy belongs to its live site's account.

**Access.** Tenant roles have no staff level, so every existing and future route is refused to them unless
`tenantRoutes` opens it: default deny by construction. Features in files of their own (billing, tickets)
open theirs from `init()` with `registerTenantRoutes` (a route listed twice panics), and declare resources
tenants own besides sites, accounts and jobs with `registerScope("/api/v1/<things>/{id}", owns)`: every
route under that prefix answers 404 to a tenant unless `owns` says the `{id}` is theirs (invoices, tickets).
For every tenant route, the wrapper checks centrally, before any handler runs:

- a route whose path has a site (`/sites/{id}`), an account (`/accounts/{id}`), a job (`/jobs/{id}`) or a
  registered scope's `{id}` is served only when the tenant owns it (their account's, or for a reseller also
  their customers'); otherwise 404, as if it didn't exist. Sub-resources (`{user}`, `{domain}`, `{repo}`,
  `{backup}`, a ticket's `{att}`) are looked up within that resource by the handlers. A tenant route with a
  path value of unknown ownership stops the server at startup;
- routes marked reseller-only, and "customers only" ones (suspend, terminate, change the plan of a
  customer, never the reseller's own account);
- the site's plan features (staging, backups, SFTP, the file manager, phpMyAdmin, own certificates, CDN, SMTP;
  a migration gave plans that included SFTP the file manager too);
- a suspended account (or one whose reseller is suspended, or a pending one) can sign in and read, and look
  after its own user, but change nothing, except through routes marked `whileSuspended`: paying an
  invoice, applying credit, its billing contact, buying burst minutes, and opening, answering and closing
  support tickets, which is how a suspended client gets back. A suspended customer's sites are frozen for
  their reseller too.

Tenants can't attach a domain whose mail this server hosts (the mail server treats every address on it as
local: a site there would get a sender mailbox on someone else's mail domain, DKIM-signed as theirs).
Usernames that the audit log and billing's ledgers use for non-users (`api-token`, `system`, `scheduler`,
`sso`, `stripe`, `razorpay`, `store`, `auto-pay`, `automation`, `billing`) are reserved, so nobody (the
public order page included) can register one and pass their actions off as the system's.

Lists (sites, jobs, security events, accounts, usage, plans, invoices, payments, tickets) are filtered to
the tenant's scope. Tenants
get their sites' day-to-day operations: shield settings, caches, updates, scans and plugin analysis,
backups and restores of their own sites to the plan's destinations, staging, domains, certificates, PHP
version and settings, SFTP logins, the file manager, phpMyAdmin, CDN, images, insights, SMTP, signing in to wp-admin and resetting
WordPress administrators' passwords, WordPress tweaks, the site analyser and its fixes. Resource changes (replicas,
memory, CPUs, the autoscaling maximum) are checked against the plan. Everything that touches shared
infrastructure stays staff-only: the server's settings and security lists, bans, the mail server, backup
destinations and deleting backups, restoring a backup as a new site, users outside their accounts,
plans' definitions, billing settings, audit log, self-update, branding. `TestEveryRouteIsClosedToOtherTenants`
walks the whole route table as a customer and a reseller (tokens and sessions): every staff-only route
answers 403 and every route on another account's site, account or job 404.

**Plans.** Per account: sites (staging copies included), disk (site files plus databases) and bandwidth per
UTC calendar month; per site: replicas, memory and CPUs per replica, domains; features; the backup
destinations tenants may choose; and what happens past the bandwidth: *notify* (default) or *suspend*.
0 means unlimited (the server's own limits still apply). New sites and staging copies are refused past
the site count or the disk space; resizing past the per-site limits is refused, with a message naming the
limit. Resellers assign administrator plans marked *resellable* that fit their own plan limit by limit
(features and destinations included): simpler and sounder than sub-plans, which would let a reseller
define limits nobody reviewed. A reseller's plan is also an allocation: its totals count every customer's
sites, and a customer's per-site limits and features are narrowed to the reseller's. Lowering a plan
doesn't shrink running sites; their next change is checked. For selling, a plan also has prices per billing
cycle with setup fees, a description, whether (and where) the order page lists it, the kind of account an
order for it creates, and a price per GB of bandwidth past its limit (see *Built-in billing*).

**Usage.** Bandwidth is the response bytes Caddy served (the traffic rollups). Disk is measured nightly in
the maintenance window and on demand (at most every five minutes per account): the site's directory walked
through `os.Root` without following symlinks (a link to another site or `/` counts as a few bytes), plus
the database's data and indexes from MariaDB's `information_schema`. Usage comes through the
`billing.UsageSource` interface, so a cluster adds what remote nodes report. Every hour each account's
month is recorded (`account_usage`); crossing 80% and 100% of the bandwidth or disk is notified once per
month (account log, `usage.threshold` webhook); bandwidth past 100% suspends the account when the plan
says so, and the next month (or a bigger plan) lifts that suspension by itself.

**Suspension.** Suspending an account (by an administrator, billing, overage, or a reseller for their
customer) suspends every site it owns, and a reseller's suspension takes its customers' sites down too.
A suspended site answers every domain with a static 503 page from Caddy (no PHP, no shield, no log),
its PHP replicas are stopped once no job holds the site, cron, backups, updates, scans and the autoscaler
skip it, its SFTP logins and phpMyAdmin sessions end; files, databases, backups and settings stay. Only the
party that suspended (or an administrator) lifts a suspension; a stronger reason replaces a weaker one
(admin > billing > overage > reseller). Unsuspending starts the replicas first and switches Caddy only
once they answer. Suspension goes through `billing.SiteOps` (`site.Service` here), so sites on other nodes
plug in. **Terminating** blocks the account's users (sessions end, tokens stop working), suspends its
sites and, when confirmed with the account's ID, deletes them (staging copies first; backups stay).
Everything is in the account's activity log and the audit log. An hourly reconcile catches sites created
while an account was suspended and retries failures.

**Provisioning API.** Administrators, and resellers for their customers, create accounts with a first user
(given or generated password); an idempotency key (body or `Idempotency-Key`, scoped per caller) makes a
billing system's retries return the same account. Plans are changed, accounts suspended, unsuspended and
terminated, users' passwords reset, usage reported (`GET /api/v1/usage`) and **single sign-on** links
made: a one-time token (256 bits, stored hashed, two minutes, single use through an atomic delete) in the
URL's fragment, which browsers never send to servers or in a Referer; the dashboard exchanges it (POST,
with the CSRF header) only after the user clicks *Continue*, so a link can't sign anyone in behind their
back. The link replaces the password, not the second factor: a user with two-factor authentication still
enters a code, and on a panel requiring it a user without it only reaches Account, as after a password
sign-in. Staff never sign in by link. The trade-off: whoever controls the billing system (or a reseller's
token) can sign in as its clients who have no second factor, which is what single sign-on is for.

**WHMCS** (`integrations/whmcs`): a server module mapping every WHMCS action to the provisioning API over
HTTPS (certificate verified), authenticated with a user's API token; accounts are found by WHMCS service ID.
Service IDs belong to the billing system that set them: the panel owner's WHMCS numbers top-level
accounts, a reseller's WHMCS its customers. They are unique within each namespace, and lookups and usage
reports only use the caller's, so a reseller reusing the owner's service number can't steer the owner's
WHMCS (suspensions, terminations, plan changes) onto an account of theirs; the module refuses ambiguity.

**Stripe.** `POST /api/v1/billing/stripe/webhook` is public (through the panel's Caddy site) and trusted
only through `Stripe-Signature`: HMAC-SHA256 of `timestamp.payload` with the endpoint's signing secret,
compared in constant time with every `v1` signature, timestamp within 5 minutes. Events are processed
once, one at a time, by event ID (recorded only after success, so Stripe's retries redo a failure).
Subscriptions (created, updated, or a completed checkout with the secret key set) create or update the
customer's account on the plan mapped from the price; `unpaid`, `canceled` and a deleted subscription
suspend it (reason billing), `invoice.paid` and an active subscription lift a billing suspension, a failed
payment is only notified (Stripe retries it). Only the account's own subscription counts: a customer's
other subscriptions (another product, unmapped prices), one-off invoices and other subscriptions'
invoices change nothing, and a subscription with an unmapped price never replaces the hosting one.
Stripe doesn't deliver in order, so each account remembers the creation time of the last event applied
and ignores older ones (a retried "active" can't undo a later cancellation); with the secret key set,
the subscription's current status is fetched from Stripe rather than trusted from the event, and a late
payment of a subscription that has since ended doesn't unsuspend. With a secret key and a meter event name, each account's
monthly bandwidth is reported hourly to a Billing Meter in whole MB, with an identifier per month and total
so a lost answer isn't billed twice. Secrets are write-only settings. The same endpoint also receives the
payments of built-in billing's invoices, which are handled first (see *Built-in billing*).

**Outgoing webhooks.** Administrators add HTTPS endpoints (with a secret, shown once, rotatable) for
`account.*`, `plan.changed`, `invoice.paid`, `usage.threshold`, `burst.threshold` and
`site.created`/`site.deleted`. Events are queued in the
store for each endpoint and delivered with `X-WPGenie-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret,
"t.body")>`, retried after 30 s, doubling up to 6 h, 12 attempts (about a day and a half), with a
delivery log. Deliveries connect only to public addresses, checked on the address actually dialled (a
name rebound to 127.0.0.1 can't reach Caddy's admin API), ignore proxies and don't follow redirects.

Known limits, to close before hosting mutually untrusted tenants at scale (object cache isolation, once
the first of them, is done: see *Caching*):

- **Domain ownership.** A tenant can attach any domain not yet on the server (no DNS ownership check, as
  on most shared hosting): first come, first served, and a subdomain of another customer's domain is
  served only if its DNS points here. A TXT-record check would close it.
- Backups aren't counted in the disk quota, and manual backups aren't limited beyond the per-site job
  lock and the shared heavy-job slots.

## Built-in billing

WHMCS-style invoicing inside the panel, off until an administrator turns it on (`internal/billing`:
`invoicing.go` for the settings, `invoices.go`, `tax.go`, `promotions.go`, `orders.go`, `automation.go`, the
gateways in `stripe_checkout.go` and `razorpay.go`; tables in `internal/store/invoicing.go`; routes in
`internal/api/invoicing.go`). The operator's side is [BILLING.md](BILLING.md).

**Who is billed.** Each account has a billing profile whose mode is *none*, *invoice*, *stripe_subscription*
(the Stripe flow above) or *whmcs*; without a profile the mode follows the external IDs. Only top-level
accounts are billed. A reseller's customer can't be put on *invoice* and no invoice can be made for it: its
reseller bills it, at its own prices and under its own name, with its own WHMCS through the provisioning
API or otherwise. The profile holds the cycle, the next due date and its anchor day, an optional price
override, credit, tax exemption, the billing contact (whose country and state decide taxes), a pending
cancellation, a recurring promotion, and the Stripe customer and saved card (payment method ID, brand, last
four digits, expiry: never the number). The next due date only moves forward: a payment, a plan change or a
waived period can push it later, never back over time already paid for (a plan change may only return to
where it credited the unused time from).

**Money.** One store currency (ISO code, symbol, 0–3 decimals). Every amount is an int64 of minor units
and every percentage an integer of hundredths. Every total is computed on the server, never taken from a
client: products and ratios through `math/big`, amounts bounded at 10¹² so sums can't overflow. Cycles
are 1, 3, 6, 12, 24 or 36 months. Due dates move by whole months onto the anchor day (Jan 31 → Feb 28 →
Mar 31). A due date is a whole UTC day: an invoice is overdue from the next day (`PastDue`), and no
"N days after the due date" step acts before that. **Taxes** follow WHMCS: up to two levels, each taking
the most specific rule for the client's country and state (state, then country, then everywhere). A level-2
rule may compound on level 1. Each line is rounded half up once, on the taxable amount after discounts.
Tax-inclusive prices are backed out exactly, with the rounding difference on the last line so the lines
add up to gross − net. Accounts can be exempted individually, and clients giving a tax ID can be exempted
(reverse charge) when the ID looks like one: 5–30 letters, digits, spaces or dashes, at least 4 of them digits,
so "none" exempts nobody. The profile reports an exemption by tax ID (`tax_exempt_by_tax_id`).

**Invoices.** Kinds: *order*, *renewal*, *plan_change*, *overage*, *burst_topup*, *manual*. Statuses:
*draft*, *unpaid*, *paid*, *cancelled*, *refunded*, *partially_refunded*. *Overdue* is derived, never
stored, so it can't disagree with the dates. An invoice freezes its lines, totals, tax lines and bill-to
address when it is made. Only a draft's lines change; an unpaid invoice's due date and notes still can.

- **Debts and offers.** Burst top-ups and plan changes are offers the client may leave (`Chased`): due in
  7 days, never overdue for the account, never reminded, fined or suspended for, and withdrawn after 7
  unpaid days. Everything else is a debt the dunning chases.
- **Cancelling** an invoice returns what was paid on it to credit. Cancelling a renewal *waives* its
  period: the next due date moves past it, and billing goes on with the next period. An order's promotion
  use is given back.

- **Numbers** are a prefix plus a sequence of at least six digits from a single counter row. The number is
  assigned inside the transaction that makes the invoice final: on issue, or on payment with
  `number_on: payment` (unpaid ones show *Proforma #id*). A rollback returns the number, so numbers have
  no gaps. The sequence can be moved forward, never below a number already used with the prefix.
- Renewal and overage invoices carry a unique dedupe key (account and period start, account and month),
  so a repeated run can't make a second one.
- The printable invoice (`/api/v1/invoices/{id}/print`) is a page of its own: `html/template`,
  `/invoice.css`, no inline script or style (the panel's CSP), `noindex`, `no-store`. The PDF is the
  browser's *Save as PDF*.
- CSV exports prefix cells a spreadsheet would run as formulas.

**Payments and idempotency.** Everything that moves money is serialised twice: by a service mutex
(`payMu`), and by the store's transactions, which lock the invoice row.

- **Dedupe.** A payment is recorded with a key from its gateway (`stripe:<PaymentIntent>`,
  `razorpay:<payment>`), unique in the table. A webhook, its retries, the Razorpay redirect and the same
  payment reported concurrently (PostgreSQL) are therefore one payment.
- **Settling.** In the same transaction the invoice's paid amount moves, the invoice settles when nothing
  is left, and money beyond its balance becomes credit. So does money paid on an invoice no longer unpaid
  (cancelled meanwhile). Credit is a ledger whose entries carry unique references, and each payment
  remembers how much of it is sitting in credit.
- **Effects.** What paying does comes after, step by idempotent step: activate the order, move the next
  due date, switch the plan, grant the burst minutes, lift a billing suspension, send the receipt, post the
  `invoice.paid` webhook. The invoice is marked done only when all have succeeded; the automation retries
  the others.
- **Other currencies.** A payment in a different currency from its invoice's isn't recorded; it is noted in
  the account's activity for staff to record by hand.
- **Refunds** go back through the gateway (Stripe's with an idempotency key) or to credit. The part of a
  payment that became credit is refunded first and taken back out of the credit. If the client has spent
  it since, the refund is refused (409) before any money moves at the gateway, so no one is paid twice.
  Refunds made in the gateways' own dashboards arrive by webhook and are recorded all the same (they
  already happened), as the difference from what went back through that gateway. When they take back
  credit that was spent, the account's activity says so.
- **Notes.** Staff's payment notes are hidden from tenants.
- **Fees.** Razorpay's fees are recorded; Stripe's aren't.

**Gateways.**

- **Stripe.** A Checkout Session in payment mode for the invoice's balance. The invoice ID is in the
  session's and the PaymentIntent's metadata, and the Stripe customer is created once per account (with an
  idempotency key per account). Saving the card sets `setup_future_usage=off_session`.
  - The existing webhook endpoint, with its signature check, event-ID ledger and ordering (above), handles
    invoice events first: `checkout.session.completed` (paid sessions only; a delayed method is reported by
    its PaymentIntent), `payment_intent.succeeded` and `charge.refunded`. Anything without WPGenie's
    metadata falls through to the subscription flow.
  - Automatic payment charges the saved card off-session from the due date, at most once per invoice per
    day. The day is stored before charging, and the Stripe idempotency key is invoice, day and amount. A
    decline, or a card that needs 3-D Secure, e-mails the client a link to pay.
- **Razorpay.** A Payment Link per attempt: `reference_id` `wpg_<invoice>_<attempt>`, Razorpay's own
  notifications off, callback to `/api/v1/billing/razorpay/callback`.
  - The callback is public and trusted only through `razorpay_signature`: HMAC-SHA256 of
    `link_id|reference_id|status|payment_id` with the key secret. Even then the payment is fetched from
    Razorpay's API, and only a captured one in the invoice's currency is recorded, with Razorpay's amount.
    An authorised payment isn't money yet: it may never be captured.
  - The webhook (`/api/v1/billing/razorpay/webhook`, handling `payment_link.paid` and `refund.processed`)
    is trusted only through `X-Razorpay-Signature`: HMAC-SHA256 of the raw body (at most 1 MB) with a
    separate webhook secret.
  - Signatures are compared in constant time. Refused callbacks and webhooks are written to the audit log.
    Stripe's secret and Razorpay's key and webhook secrets are write-only settings.
- **Bank transfer.** The instructions are shown with the invoice, and staff record the payment.

**Orders and pending accounts.** The public order page (`/order.html`) uses three public endpoints: the
catalog (public plans with a price), quotes, and orders.

- **Limits on orders:** per address, 5 placed and 30 attempts an hour (quotes: 300); bodies of 64 KB at
  most; a honeypot field; the usual username and password rules. The password is hashed through the sign-in
  path's bounded slots, so a flood of orders can't starve the server.
- **Placing an order.** One transaction creates the account with status *pending*, its first user (tenant
  role from the plan's account kind), the billing profile, the order and the first invoice (due that day),
  and counts the promotion's use against its limit (given back if the invoice is cancelled). A promotion
  for new clients only is refused to an e-mail address that already belongs to an account. The user is
  then signed in, as by the sign-in form.
- **A pending account** is suspended in every check (`Suspended`: anything but active), so it has no sites.
  Its users can still sign in and use the routes open while suspended. It can't be suspended or unsuspended,
  only accepted or cancelled.
- **Activation.** Paying the invoice activates the account, billed from that day, unless orders need
  approval. Orders still unpaid after 14 days are cancelled: the invoice is cancelled and the account
  terminated (it has nothing to delete).

**Plan changes and cancellations.**

- **Plan changes.** The quote credits the unused part of what was actually paid for the current period
  (the plan, pro-rata and discount lines of the paid invoice covering it, less refunds), not the list
  price. A free or waived period credits nothing. A change is refused while a chased invoice is overdue
  or a renewal is unpaid: that period would be paid twice. Issuing a renewal withdraws an unpaid plan
  change, whose price was for the period the renewal now bills.
- **Cancellation at the end of the period.** It cancels (and releases) renewals already made for later
  periods, and no new one is made. Withdrawing it lets them be made again.
- **Resellers.** A reseller with customer accounts that aren't terminated can't cancel (409). If customers
  appear before the date, the cancellation suspends the reseller instead (reason *admin*, which billing
  never lifts), until they are gone. Termination by the dunning waits for them the same way.

**Automation** (`RunInvoicing`). It runs every 15 minutes, starting 2 minutes after the daemon, one run at a
time (`TryLock`). Every step is keyed so that repeating it, or resuming after a crash, does nothing twice.
In order:

1. Retry the effects of paid invoices.
2. Make renewal invoices `days_before_due` ahead of the due date, unless a cancellation takes effect first.
3. Dunning over unpaid invoices, oldest first. Offers left unpaid for 7 days are withdrawn. For debts:
   - automatic payment;
   - a reminder before the due date;
   - overdue reminders, only the latest one due (a panel that was down for a week doesn't send a burst);
   - a late fee, once (not taxed: the invoice's taxes stay as issued; 0 days: from the first overdue day);
   - for accounts billed by invoice, suspension (reason *billing*; 0 days: never) and optional termination,
     each decided on the invoice as it is then, under `payMu`, since a payment may have landed meanwhile.
4. Lift billing suspensions once nothing is overdue. Paying does this at once too.
5. Apply the cancellations that are due.
6. Invoice last month's bandwidth overage per started GB: the higher of what was recorded during the month
   and what the rollups say now.
7. Cancel stale orders.

E-mails carry dedupe keys (`invoice.overdue:<invoice>:<day>`), so a crash between a step and its record
doesn't mail twice. Each run records its counts (renewals, reminders, suspensions…, `offers_cancelled`,
`plan_changes_replaced`, `cancellations_blocked`) and errors (`GET /api/v1/billing/automation`).

**Tenancy of billing routes.**

- **Staff.** Reading needs viewer. Listing orders and starting a client's payment need operator.
  Everything else needs admin.
- **Tenants** reach the store's display config (`/billing/config`), and their account's profile, credit and
  billing e-mails; a reseller also reads its customers'.
- **Changes** apply only to the tenant's own account (`ownAccountOnly`): the contact details, automatic
  payment, the saved card, a plan change (only to public plans of their kind), cancellation (only at the end
  of the period; staff can cancel immediately) and burst packs.
- **Invoices** are a registered scope: a tenant reaches only its own account's, never a draft, and lists are
  filtered the same way.
- **While suspended**, a tenant can still pay, apply credit, edit its billing contact, quote a plan change and
  buy burst packs, so a suspended or pending client can always get back.

## Support tickets

A help desk on the panel (`internal/support`, `internal/store/support.go`, `internal/api/support.go`).
Nodes serve none of it.

**Model.**

- **Departments** have a name, a description, a notify address, a hidden flag and a sort order. A
  *General* department is created on first use. The last visible one can't be hidden, and one with tickets
  can't be deleted.
- **A ticket** has a public mask (`WPG-` and six random digits, unique) and records:
  - its account and the user who opened it. Its **handler** isn't stored but derived when read: the
    account's *current* reseller (so a customer moved to another reseller, or away from one, takes its
    tickets along), or the operator's staff when there is none, when the reseller escalated it, or when
    staff opened it;
  - a department, and optionally a site (which must be the opener's);
  - a status: *open*, *customer_reply*, *in_progress*, *on_hold*, *answered* or *closed*;
  - a priority: *low*, *medium*, *high* or *urgent*;
  - an assignee, and the times of the last reply, the first response, escalation and closing.
- **Messages** have a side (*customer*, *handler*, *staff*, *system*), an internal flag, and a staff-only
  flag.
- **Canned replies** belong to staff.

A client's reply sets *customer_reply*, which reopens a closed ticket. A provider's public reply sets
*answered*, and so does setting that status by hand, which also restarts the auto-close clock. Clients
may only close a ticket, or reopen a closed one.

**Access.**

- **Scope.** Tickets are a registered scope: a tenant reaches its own account's, and a reseller also its
  customers'.
- **Providers** are staff, and the reseller handling the ticket. Only providers set a status other than
  closed or reopened, change the priority and department, and write internal notes; only staff assign.
- **Notes.** *Internal notes* are hidden from the customer everywhere: the conversation, list previews,
  e-mails, and attachment downloads (404). They are shared between staff and the account's reseller.
  *Staff-only notes* are hidden from the reseller too.
- **Escalation** hands a reseller's ticket to staff, adds an internal system note and e-mails staff. It is
  one-way. The reseller still sees the ticket and its internal notes.
- **Suspended and pending accounts.** Opening, replying, updating and escalating are `whileSuspended`: a
  suspended client can still ask why. A terminated account can't open tickets.
- **Staff roles:** viewers read; operators work tickets and canned replies; admins manage departments and
  settings.

**Attachments** are stored at `/var/lib/wpgenie/support/<ticket>/<random name>` (directories 0700, files
0600) and staged in `.staging/` while uploading; stale staged files are removed after 6 hours. The uploaded
name is only displayed, never used as a path.

- **Limits:** files per message (default 5, at most 10), size (default 5 MB, at most 25), and an extension
  allow-list (images, PDF, text, logs, CSV, zip).
- **Refused always:** script-capable types (HTML, SVG, JavaScript, PHP, executables), content that doesn't
  match its extension or looks like HTML or XML, and empty files.
- **Serving:** as `application/octet-stream` with `Content-Disposition: attachment`,
  `Content-Security-Policy: default-src 'none'; sandbox`, `nosniff` and `private, no-store`. Raster images
  are shown inline with their real type only when asked (`?inline=1`).

A client's file therefore never runs as a page on the panel's origin, where it would act with the viewer's
session.

**Auto-close.** Every hour, answered tickets whose last public reply is older than `auto_close_days`
(default 7; 0 turns it off) are closed with a system message and `ticket.closed`.

**E-mail** (through the outbox, below). `ticket.opened`, `ticket.reply` and `ticket.closed` go to the client
account's address. `ticket.new_staff` (a new or escalated ticket) and `ticket.customer_reply` go to the
handler: the reseller's address, or for staff the settings' notify addresses plus the department's. Each
recipient gets a message of its own, so one refused address holds up no one else and each has its line in
the e-mail log. Only public messages are ever mailed.

**Deleting an account** deletes its tickets in the same transaction, then their attachment folders. The
hourly sweep removes folders of tickets that no longer exist, in case that clean-up didn't finish.

**Known gaps:**

- No inbound mail yet: replies go through the panel.
- No per-account rate limit on tickets.
- No cap on the disk the attachments take.

## E-mail to people

`internal/mailer` is the panel's outbox for mail to people (billing, tickets). Monitoring alerts keep their
own SMTP settings, so a broken customer-mail relay can't silence alerts. Only the panel runs it; nodes
have none.

**Settings** (setting `mailer`, admin): host, port, security, credentials, from name and address, a default
Reply-To and an optional Bcc (envelope only). Security is one of:

- *starttls* (the default). It is required, not opportunistic: sending fails if the server doesn't offer it.
- *tls*: implicit TLS.
- *none*: only without a username and password, so credentials never cross the network in clear.

TLS is 1.2 or later, with the certificate verified against the host. The password is write-only
(`password_set`). Omitted on update it is kept, but only while the host, port and username stay the same:
otherwise it must be entered again, or an administrator (or a stolen session) could point the host at a
server of their own and collect the stored password with a test message. A test message is sent at once,
bypassing the queue, and reports the server's answer. The "this server's Mail" preset is offered only while
the panel's mail server runs, with its hostname (the name its certificate carries).

**Queueing.** Features call `Queue` with a template name, data, the account the message concerns and a dedupe
key. The message is rendered then and stored in `mail_outbox`. Dedupe keys are kept in a table of their
own (`mail_dedupe`) for 400 days, longer than any automation looks back, so the billing automation, replayed
webhooks and repeated runs each queue once, and trimming the log never lets an old reminder go out again.

- **Templates** are Go `text/template`, registered by each feature with sample data. Admin overrides are
  stored as settings and must render against the sample data before they're saved; an override that fails
  at send time falls back to the default. Staff write templates, so they are bounded: `range` over a
  number (the one way to loop without producing output) is refused, and a rendered subject or body stops
  at 256 KB.
- **Bodies are plain text.** The HTML part is generated from it with everything escaped (paragraphs,
  `[[Label|URL]]` buttons, the brand), so neither a template nor its data (a ticket's text) can inject markup.
- **Headers** take bare addresses only (no CR/LF, commas or angle brackets). The subject has control
  characters stripped, is cut to 250 characters and is encoded.
- **Format:** `multipart/alternative`, `Auto-Submitted: auto-generated`. There are no attachments, so invoices
  aren't attached as PDFs.

**Delivery.** One sender loop wakes on every new message and every 30 seconds. It sends in batches of 20, one
message at a time, with 15 seconds to connect and 90 seconds per message. A failure is retried after
1 minute, 5 minutes, 30 minutes, 2 hours and 6 hours, then the message is marked *failed*; a resend starts
that schedule again. A recipient the server refuses is skipped and named in the log: the message fails only
if no recipient accepts it. While sending is off or unconfigured, messages wait as *pending*, for 7 days at
most: then they are marked failed, since a payment reminder weeks late is worse than none. The newest
10 000 rows are kept. If a delivery can't be recorded, the round stops rather than send the message again
in a loop.

**Branding.** Mail carries built-in billing's company when billing is on and names one, otherwise the panel's
branding. The logo is the panel's.

**Previews and the log.**

- **Previews** are rendered on the server with the template's sample data. They are held in memory for
  10 minutes under a random token and served with their own CSP (`default-src 'none'`, inline styles,
  https/data images, `frame-ancestors 'self'`, `sandbox`) and `X-Frame-Options: SAMEORIGIN`, into an iframe
  sandboxed to popups only. The panel's own CSP forbids the inline styles mail needs, and a `srcdoc` frame
  would inherit it; this way nothing in a template or a logged message runs in the panel's origin.
- **The log** (the outbox itself) is shown the same way. Operators read it, admins resend, and tenants read
  the messages filed under their account: whether each went, never the SMTP server's answers (they can
  name internal relay hosts).

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

**Autoscaling.** Every 15 s the daemon takes one `docker stats` sample and computes each
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

CPU misses sites that *wait*: a slow payment API or a heavy query keeps every PHP worker busy while
the CPU idles, and visitors queue. Two optional targets cover them, and each metric proposes a replica
count, the highest winning (HPA's rule for several metrics; scale-down needs every sample of the window
to agree on all of them):

- **PHP workers busy** (`target_workers`, percent): requests per worker, *queued ones included*, over
  the serving replicas. Read from each replica's socket table (`/proc/<pid>/net/tcp` of its main process,
  from the host: one `docker inspect` per tick, nothing started in the containers; per container with
  `docker exec` where the host's `/proc` isn't Docker's). Established connections to :9000 are requests
  in PHP-FPM, and the listening socket's `rx_queue` is its accept backlog: requests no worker has taken.
  Because the queue counts, the metric passes 100 % under saturation and the plain HPA rule applies (no
  doubling heuristic, unlike CPU). FPM's own status page wasn't used: a worker serves it, so it stalls
  exactly when every worker is busy.
- **Response time** (`target_response_ms`): the 95th percentile of PHP response times over the scale-up
  window (≥ 20 responses), from Caddy's access log (the analytics ingester keeps the last minutes in
  memory). Above the target, one replica is added at a time, and only when the site is busy (CPU or
  workers ≥ 50 %): a slow page on idle workers is slow code or a slow database, which replicas can't fix.

**Burst.** What customers see of autoscaling. A site has a normal size (`min_replicas`) and a burst
mode: *off*, *auto* (the autoscaler above, between the normal size and a ceiling) or *on* (at least one
extra instance, more under load, optionally until a time, then back to auto). Nobody picks the ceiling:
it is the plan's replicas per site and the server's `max_replicas`, lowered until the database
connections fit (below); the host's free memory (above) and its overall CPU use are judged at each
scale-up. While more than 85 % of the server's CPUs are busy (`/proc/stat`, per tick) no site gets
extra instances: more containers on a saturated server only share out the same CPUs, and slow down
every other site. Burst is only a narrower range for the autoscaler (`burstRange`): *on* raises the
minimum by one, a pause lowers the maximum to the normal size, and the autoscaler moves the site into
the range on its next tick, through the same blue/green reconcile. `PUT /sites/{id}/autoscale` still
works and is automatic burst underneath.

*Minutes.* A site is charged one burst minute for every minute it runs above its normal size,
however many extra instances it has (flat, so customers can predict it). Minutes are counted where the
site runs (`burst_usage`, per site and UTC month); every minute the panel copies other servers'
counts (`burst_reports`, per server: a site that moved has minutes on each; a server that doesn't
answer is counted from its own record next time) and charges each site's *new* minutes to the
account owning it now, in `burst_charges`. For a month's first hour the previous month is charged
too (its last minutes, a server unreachable at midnight). That ledger is never taken
back: deleting a site or giving it to another account doesn't return its minutes, and a site given
to an account later only brings its later minutes (staff sites are charged to account 0). A plan
includes `burst_minutes` a month (0: unlimited; the `burst` feature lets tenants use burst at all);
minutes beyond them come out of the account's `burst_credit` (bought, never expire;
`POST /accounts/{id}/burst-credit`). Each month records how many minutes beyond the plan are
settled (`burst_from_credit`) and how many of those the credit paid (`burst_credit_taken`), so
settling a month again never takes credit twice, a bigger plan gives back only what was paid (plan
changes can't make credit), and the minute or two a site bursts while its pause takes effect is
forgiven, not taken from the next top-up. With nothing left, or a plan (or a reseller's) without
burst, the account's sites are paused (`burst_paused`, sent to the server the site runs on, again at
least hourly; pauses found on sites no account owns are lifted) and go back to their normal size;
buying minutes or a new month resumes them within a minute. 80 % and 100 % of the month's minutes, and
running out, are an account event and a `burst.threshold` webhook.

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

- *Compressed copies.* Each page is stored with `index.html.br` (Brotli 11, the PECL extension in the
  image) and `index.html.gz` (gzip 9), written before the page itself; Caddy's `file_server
  precompressed` sends the best one the browser accepts, with no CPU per request. Everything else is
  compressed on the fly (zstd, gzip).
- *Phones and computers.* A filter on `wp_is_mobile` notes whether the render asked it. If so, the page
  is stored as `index-mobile.html` or `index-desktop.html` (and any copy for every device is removed);
  Caddy tries `index.html` first, then the copy for the request's device, decided by `wp_is_mobile()`'s
  own rule: the `Sec-CH-UA-Mobile` client hint when sent, else the User-Agent (`proxy.MobileUA`,
  mirrored in the PHP and pinned by a test). The PHP side stores by that rule, not by the filterable
  `wp_is_mobile()`, and stores nothing when a filter makes the two disagree. Themes that sniff the
  User-Agent themselves get *separate mobile cache*: every page stored per device.
- *Purges.* Editors and above get *Purge cache* in the admin bar (page cache, this site's object
  cache keys, and the CDN through the purge marker). The hooks also fire for WP-CLI: `--skip-plugins`
  still loads mu-plugins, so content changed from the command line purges like the dashboard
  (`TestPerformanceEndToEnd`). The daemon's own database-level changes (search-replace, loads) purge
  explicitly.

**Object cache.** The Redis drop-in from the redis-cache plugin (pinned by checksum) ships in the
image and is loaded by a wrapper in `wp-content/object-cache.php`. The wrapper forces
`WP_REDIS_GRACEFUL` (Valkey down → site keeps working uncached) and `WP_REDIS_SELECTIVE_FLUSH`
(without it a flush — which WordPress core runs on updates — is a `FLUSHDB` that empties every
site's cache). Keys are prefixed with the site ID. Panel purges delete the site's keys straight from
Valkey (a server-side `SCAN` + `UNLINK` script), never via `wp cache flush`: WP-CLI would load the
site-replaceable drop-in outside the PHP jail.

*Isolation.* Every site is its own Valkey ACL user (`wpg_<site>`, password in its root-owned
`wp-config.php`), limited to keys under its prefix, with no pub/sub channels, no dangerous or administrative
command (`KEYS`, `FLUSHALL`, `CONFIG`, `CLIENT`, `DEBUG`, …), no functions and no flushing or killing others'
scripts; `INFO` stays (the drop-in reads the server version) and so does `SCAN` (its own selective flush
needs it), which lists other sites' key names but never their values. The default user is off; the daemon is
the `wpgenie` user, its password passed to `valkey-cli` in the exec's environment. Without this, PHP on one
site, which a tenant or a compromised plugin controls, could read or rewrite another site's cached options
and user capabilities and take it over. Passwords are HMACs of the site ID with a key only the daemon reads
(`/etc/wpgenie/valkey.key`), so nothing new is stored: the ACL file (`/etc/wpgenie/valkey/users.acl`, hashes
only, mounted read-only into Valkey) is rewritten and reloaded whenever a site is created or deleted, and
sites that predate it get their line in `wp-config.php` at the next start (Valkey, started before its file
existed, is restarted once to take it; the cache is disposable). A moved site gets its new server's
credentials; a spread site's replicas elsewhere use the home's, through the copied `wp-config.php`.
`TestValkeyACLIsolatesSites` checks the rules against a real Valkey, `TestCacheIsolationEndToEnd` against real
WordPress.

### Images

With image optimisation on (per site: AVIF and/or WebP), every JPEG and PNG upload gets
`<file>.avif` and `<file>.webp` next to it. Caddy serves the best one the `Accept` header allows at the
original URL (`Vary: Accept`), so HTML, the page cache and links elsewhere never change; browsers
without support get the original. A copy is served only if the original still exists, and never to
requests through the CDN edge (`{client_ip}` differs from the peer): Cloudflare ignores `Vary: Accept`
and would cache one format for everyone. Sites behind a pull zone get no negotiation for the same
reason.

- Conversion is PHP + GD in the site's container, as the site user under the cron jail
  (`image-convert.php`): EXIF orientation applied the way WordPress does (browsers rotate the original;
  GD would drop it), alpha kept, CMYK and animated images skipped, pictures over 16 megapixels skipped
  (memory). A copy that isn't smaller isn't kept. A copy carries its original's mtime, so a replaced
  original (SFTP, restore) is converted again.
- New uploads: `wp_generate_attachment_metadata` schedules a cron event that converts every size
  (AVIF takes a second or more per size, too slow for the upload request). Deleting an image deletes its
  copies (`wp_delete_file`).
- Existing files: turning it on (or *Convert now*) runs `convert-images.php` over the uploads as a job,
  niced (visitors share the container's CPU), without loading WordPress; it also removes copies whose
  original is gone or whose format was turned off. The nightly maintenance queues it again (a job, at
  most 20 minutes a night: it holds a heavy slot the night's backups share) for files that arrived
  another way.

Lazy-loading is WordPress core's (`loading="lazy"`, `fetchpriority` on the likely largest image);
nothing in WPGenie removes it.

The cache code lives read-only in the image (`/usr/local/share/wpgenie`); sites only get one-line
wrappers. A compromised plugin can't alter the cache logic, and an image upgrade updates it
everywhere. The daemon writes those wrappers as root into a directory the site controls, so all
such file operations go through `os.Root`, which refuses to follow symlinks out of the docroot.

### CDN

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

**Edge caching of pages (optional).** Cloudflare can keep HTML too. WPGenie adds one Cache Rule per zone
(Rulesets API, phase `http_request_cache_settings`, found again by its `ref`; the zone's own rules are
never touched): the site's hostnames, GET/HEAD, none of the page cache's bypass cookies (Cloudflare's
cache key ignores cookies), edge TTL *bypass by default*: Cloudflare only stores what the origin marks
cacheable. Caddy marks exactly the page-cache hits, and only device-independent ones, with
`s-maxage=3600` (`max-age=0` for browsers), so everything PHP renders and every personalised page stays
uncached, by construction rather than by a second list of exceptions. A page is at most 10 h old in
the page cache plus 1 h at the edge: still inside the nonce lifetime. Purges already cover the hostnames.
The rule follows domain changes (checked every CDN pass). The price: edge hits skip the shield and the
statistics. The token needs *Zone → Cache Rules: Edit* too.

**Pull zones.** A site can instead keep its own DNS and send only static files through a pull zone on a
hostname of its own (`cdn.example.com`): bunny.net (the API key and pull zone ID are checked: the zone
must answer on that hostname; it's purged with the site, and its origin is checked in the status) or any
other (no purges: static URLs are versioned or renamed). `cdn.php` rewrites links to the site's files
under `wp-content` and `wp-includes` in front-end HTML (attributes, `srcset`, CSS `url()`; never `.php`,
another host or JSON-escaped links) inside the page cache's buffer, so cached pages carry them; changing
the hostname purges the page cache. Fonts get `Access-Control-Allow-Origin: *`, which browsers require
from another origin.

### Cron

New sites set `DISABLE_WP_CRON`; the daemon runs every active site's due events each minute
(`cron_concurrency` at a time, never overlapping per site). Cron runs `wp-cron.php` with plain PHP
rather than WP-CLI because it executes plugin code: `PHP_INI_SCAN_DIR` adds `jail.ini`, the same
`open_basedir` / `disable_functions` jail as web requests. Containers from an older image have no
jail and are skipped; WordPress's own page-view cron keeps working for them.

## Performance insights

Per site, over a chosen period: PHP response times (median, 95th and 99th percentile, a histogram),
the page cache hit rate, the slowest URLs and PHP errors.

- **Response times** come from Caddy's access log, which the analytics ingester already reads: requests
  PHP answered (not cache hits, static files, shield blocks or Caddy's own answers), in the same
  transaction as the traffic counters. Durations are what the visitor waited, a queue for a worker
  included. Hourly rows hold a histogram (fixed buckets, 50 ms to 10 s), so percentiles over any range
  are merged exactly rather than averaged. URLs taking a second or more are grouped by method and path
  (no query string) with count, average and slowest: the 500 most recent per site (anyone can make up
  slow URLs), for 7 days.
- **PHP errors** go to the site's own log, `logs/php-error.log` next to `wp-config.php` (FPM's
  `error_log`, and the cron jail's; the directory is the site user's, Caddy can't read it). The daemon
  reads it every 30 s through `os.Root`, with one non-blocking open checked against what `Lstat` saw
  (the site can swap the file for a symlink or a named pipe at any moment; neither steers or stalls the
  daemon), reading and truncating only through that handle. It groups entries by
  level, message and file:line, and attributes each to a plugin, theme, mu-plugin or core by its path;
  for an error inside WordPress, by the first plugin or theme in its stack trace. The read offset is
  saved with the counts in one transaction, and the file is truncated once read past 8 MB. The newest
  300 kinds per site are kept, for 30 days since last seen. A slowlog with stack traces isn't possible:
  FPM needs ptrace for it, which the containers don't have.

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

## WordPress from the panel

**Signing in to wp-admin without a password** (`internal/wplogin`, `site/wpadmin.go`). *WP Admin* in the panel
(or `POST /sites/{id}/wp-admin/login`, operators and tenants) signs the browser in as one of the site's
administrators (the oldest unless another is chosen):

1. WP-CLI (plugins and themes skipped, so a compromised plugin can't observe or steer it) makes a real WordPress
   session: a `WP_Session_Tokens` token, recorded with the requester's address and browser, and the
   `secure_auth` and `logged_in` cookies for it, on the paths `wp_set_auth_cookie()` uses. It is an ordinary
   session: listed in the user's sessions, two days like any sign-in without *remember me*, ended by logging out
   everywhere or a password change. Only administrators can be signed in as.
2. The daemon keeps the cookies in memory behind a random one-time token and returns
   `https://<site>/_wpgenie/login?wpgenie_token=…`, valid two minutes. The host is WordPress's own (its
   `siteurl`, when that is one of the site's domains): the cookies are only valid there.
3. Caddy's `/_wpgenie/*` route (before the shield and the WAF, naming the site in a header it sets) brings the
   browser to the daemon, which checks the token was made for this site and this host, forgets it, sets the
   cookies (`Secure`, `HttpOnly`, session cookies; the site's `COOKIE_DOMAIN` only if it covers the host) and
   redirects to wp-admin. `no-store` and `no-referrer`: the URL isn't cached or leaked.

Why a link rather than a password or a WordPress plugin: cookies for the site's domain can only be set by a
response from that domain, and a plugin checking tokens would put the secret where site code can read it. Here
nothing about the sign-in is stored anywhere; cookie names and values WordPress printed are checked before
they go into `Set-Cookie` headers (the cookie constants are overridable in `wp-config.php`'s editable part).
Deleting or suspending a site forgets its unused links; the admin allowlist still applies to wp-admin.

**Administrators and editors.** `GET /sites/{id}/wp-admin/users` lists both roles (one `wp eval`: `get_users`
with `role__in`, plus the ID of the site's *first user*, the lowest ID whatever its role: the account
`wp core install` made, or an imported site's original owner). `POST` there adds one (`wp user create
--role=… --prompt=user_pass`; login and e-mail are positional, so neither may start with `-`); `DELETE
…/users/{user}` deletes one with `wp user delete --reassign=<first user>`, so their posts and pages survive. The
first user can never be deleted, nor the last administrator. `POST /sites/{id}/wp-admin/password` sets a new
password for an administrator or editor (or a random 24-character one, shown once) with `wp user update
--prompt=user_pass` (the password on stdin, never argv) and ends all their sessions. WordPress sends no e-mail
about any of it.

**Branding** (`site/branding.go`, `images/php/branding.php`). One server-wide brand (`PUT /settings/branding`,
admins): a name, a link and a logo (PNG, JPEG, GIF, WebP or SVG, 256 KB at most, its bytes checked against its
type). Every site gets a root-owned `wpgenie-brand.php` mu-plugin wrapper (name and link base64-encoded, so no
value can end the PHP string) loading `branding.php` from the image: the brand's logo and link on the login
page, the brand in place of WordPress's logo menu in the admin bar, *Hosted by* in the admin footer, the brand in
page titles, and no WordPress news widget or welcome panel. The logo is never written into sites' directories
(site code could replace it): the daemon serves it at `/_wpgenie/brand/logo?v=<hash>` on each site's domain,
cached for good per version, SVG under a `sandbox` CSP. The panel sends the whole brand to every node (and to a
node when it's added), and each rewrites its sites' wrappers; sites still being provisioned get theirs when
they go live (the WordPress image only copies core into an empty docroot).

**WordPress tweaks** (`site/optimize.go`, `images/php/optimize.php`). What performance plugins do on top of
caching, as a list per site (`PUT /sites/{id}/optimize`): no emoji scripts, no oEmbed discovery, no generator,
RSD, WLW or shortlink tags, Heartbeat every 60 s in wp-admin and not on visitors' pages, no self-pingbacks, no
Dashicons for visitors, optionally no jQuery Migrate on visitors' pages (old themes may need it), and a nightly
database cleanup (expired transients; auto-drafts older than a week; spam older than a month; revisions older
than a month beyond the newest five per post; bounded batches, through WordPress's own delete functions so
metadata and counts stay consistent). A root-owned `wpgenie-optimize.php` wrapper names the ones on; nothing
to install, update or switch off from wp-admin. New sites get all but jQuery Migrate; sites that existed
before keep none until they're turned on (by hand, or by the analyser's fix). Changing them purges the page
cache.

**Site analyser** (`site/analysis.go`, `GET /sites/{id}/analysis`). A live look inside WordPress through one
WP-CLI call (version, debug display, search-engine visibility, open registration and its role, administrators,
autoloaded options, expired transients, revisions, spam, inactive plugins and themes, database size) combined
with the last scan (installed versions against known vulnerabilities, file integrity), the plugin analysis
(closed, abandoned, nulled) and the site's settings (shield, WAF, caches, images, tweaks, automatic updates).
Each finding is critical, a warning or informational, in security, performance or upkeep; the score starts at
100 and loses 20, 8 or 2 per finding (grades A to F). Findings WPGenie can fix carry a fix
(`POST /sites/{id}/analysis/fix`): each one is an existing operation (caches, WebP, tweaks, database cleanup,
a scan, a security or full update with its snapshot and rollback, automatic security updates, the shield, the
WAF) or a single WP-CLI option change (new registrations as subscribers, search engines allowed), so tenants
reach nothing through it they couldn't already. A staging copy isn't faulted for hiding from search engines.

## Jobs

Long operations (creating a site, backups, restores, staging clones and pushes, PHP and primary
domain changes) are jobs (`internal/jobs`): the API answers `202` with a job ID, and the dashboard's
jobs tray and `wpgenie jobs` follow its progress. A job waits (*queued*) until it holds the site's
maintenance lock, the one updates and scans take, so nothing overlaps on one site. Heavy jobs (whole-site
copies) also share `job_concurrency` slots (default 2): ten scheduled backups starting at once take turns.
A job keeps running when the request that started it goes away, and one left queued or running by a
stopped daemon is marked failed at the next start. A new site's admin password is the job's *secret*:
held in memory for 30 minutes, shown only to whoever created the site, never written anywhere.

## Backups and environments

**Backups** (`internal/backup`, `site/backups.go`) are [restic](https://restic.net/) snapshots, so they
are deduplicated (content-defined chunks, compressed), encrypted and verifiable. One repository per
destination serves every site that uses it: WordPress core, common plugins and themes are stored once for
all of them. Destinations: a directory on this server (`local`, created on first use at
`/var/lib/wpgenie/backups/local`), S3-compatible storage, Backblaze B2, or an SFTP server.

- restic runs in a throwaway container per command (`restic/restic`, pinned): it sees only the site's
  docroot (read-only), the database dump and, for a local repository, the repository; it keeps only
  `DAC_READ_SEARCH` and has no network for local repositories. It stores symlinks as symlinks and never
  follows them; a symlink it met would resolve inside its own container anyway. The repository password,
  cloud keys and SSH key go in on stdin (a `KEY=VALUE` block the entrypoint exports), never in argv or
  the environment `docker inspect` shows. restic's cache persists under `backups/cache` (root only).
- An SFTP destination gets its own Ed25519 key (its public half shown to add to the server's
  `authorized_keys`) and the server's host key is pinned when it's added (`ssh-keyscan`, strongest key
  type): a changed key fails the backup instead of sending data to someone in the middle.
- A backup is one snapshot: `/backup/files` (the docroot minus caches) and `/backup/db` with the
  `mariadb-dump` (single transaction: no locks) and `meta.json` (domain, table prefix, the tables, PHP
  version). Tags: `wpgenie`, `site=<id>`, `domain=<primary>` and the kind: *scheduled* (the site's keep
  rules apply: last/daily/weekly/monthly, restic's `forget`), *manual* (kept until deleted) or *safety*
  (taken before a restore or a push; kept 7 days). Data of forgotten snapshots goes with a weekly `prune`
  (and `check`) per repository in the maintenance window.
- **Schedule**: daily and longer schedules run in the maintenance window (before the night's automatic
  updates); shorter ones by interval; a failed attempt is retried after an hour. New live sites get daily
  local backups (7 daily, 4 weekly, 6 monthly); sites that existed before keep none until one is set.
- **Restore** backs the site up first (*safety*), then restores files and/or the database. Files come out
  of restic as a tar stream and are written by the site user inside its container (never as root, see
  *WordPress updates*), in two phases: extracted into a temporary directory, and swapped in only after the
  process producing the stream exited cleanly and the archive holds a WordPress install; a source that dies
  half-way can't leave a half-replaced site even if tar took the cut stream for a complete one. The database
  is loaded from the dump and tables the backup didn't have are dropped. WPGenie's own wrappers are
  rewritten and caches purged. A backup can also become a **new site** on another domain, deleted sites'
  included (their backups stay in the repository): same machinery, links rewritten to the new domain.
- **Download** streams the snapshot from restic as `.tar.gz`; a failure half-way aborts the connection
  rather than end a truncated archive that looks complete.

**Staging** (`site/staging.go`) is a full copy of a live site as a site of its own (container, database,
settings), linked by `parent_id`: files copied between the two containers (site user on both ends),
database copied, links rewritten, `blog_public` 0. It is `WP_ENVIRONMENT_TYPE=staging` (Jetpack,
WooCommerce Subscriptions and others stop acting as the live site), Caddy adds `X-Robots-Tag: noindex`,
and it gets no cron, no mail through the mail server, no automatic updates and no scheduled backups:
a copy of a shop must not charge renewals or e-mail customers. A **push** backs the live site up first,
then copies code (everything but uploads), or all files, and/or the database or chosen tables. The
database leaves staging already rewritten: WP-CLI's `search-replace --export` writes the SQL with the
replacement applied (staging itself is untouched), and it is loaded into the live database table by
table, so live never holds staging links, not even for a moment. That SQL comes out of the staging
site's container, whose WordPress a compromised plugin could have altered, so it is loaded as a temporary
MariaDB account with rights on the live site's database only (dropped afterwards), never as root; every
load also runs in the client's `--sandbox` (no `\!` shell or `source` lines). Staging and push jobs lock
both sites at once (all or nothing), before taking a heavy slot, so two jobs can't deadlock. The live site's own search-engine
setting is kept. A whole-database push drops live tables staging doesn't have.

**Link rewriting** (clone, push, primary domain change, restore as a new site) is one WP-CLI regex
search-replace over every table: `//old` and JSON-escaped `\/\/old` (page builders), never followed by
another hostname character, so `a.test` → `b.test` leaves `a.test.au` alone. WP-CLI handles serialised
PHP data; GUIDs are left as WordPress requires.

**Domains** (`site/domains.go`): a site serves its primary domain and any aliases; *redirect* domains
answer `301` to the primary one with the path (their own Caddy block: no PHP). Making another domain
primary rewrites the links (www ↔ bare domain is this) and turns the old primary into a redirect; if the
proxy can't be switched, the links are put back. A site may bring **its own certificate** (e.g. a
Cloudflare origin certificate): the chain and key must match, be valid now and cover every served domain;
RSA keys need 2048+ bits. Files go next to the Caddyfile (`certs/<site>/`, key `root:<caddy group>
0640`); Caddy then serves it and skips ACME for those names (HTTP still redirects to HTTPS). Nothing
renews it: an event is logged daily from two weeks before it expires. If Caddy refuses the config the
previous certificate is put back.

**PHP versions** (8.2, 8.3, 8.4) are images of the same Dockerfile (`wpgenie/php:<version>`); the
default's is built by the installer and updates, others by the daemon the first time a site switches (a
job). The switch is a blue/green reconcile, health-checked like updates: a site working before and
broken after (a plugin using something the new PHP removed) is switched back. After a self-update,
images of other versions in use are rebuilt before sites are rolled. **PHP settings** (memory_limit,
upload size, max_execution_time, max_input_vars) reach FPM as `WPG_*` variables read by `pool.conf`
(defaults in the image); FPM's `request_terminate_timeout` is kept 30 s above max_execution_time. They
only enter the replica spec when set, so upgrading doesn't roll sites that don't use them.

**SFTP** (`internal/sftp`, `images/sftp`): one OpenSSH server for every site, published on `sftp_port`
(2222), running while any login exists. Each login is chrooted (`ChrootDirectory`) to its site's
directory (`root:82 0751`, as sshd requires), starts in `public/`, gets `internal-sftp` only (no shell,
forwarding or tunnels) and writes as uid 82, the site user, with umask 022. It can read `wp-config.php`
like the site's PHP can, and nothing of other sites. Logins are `<site>` or `<site>-<suffix>`, so they
never clash with system accounts; passwords (generated, shown once) are stored only as SHA-512 crypt
hashes (what the container's sshd reads without PAM), public keys are parsed and re-serialised without
options (`command=`, `from=` …). The daemon renders `passwd`/`group`/`shadow` and `authorized_keys` and the
container installs them atomically; deleting a login or changing its password ends its open sessions.
Host keys persist. OpenSSH's own `PerSourcePenalties` slows down addresses that keep failing. Docker
publishes the port past `ufw` (as it does for every published port).

**File manager** (`internal/files`, `static/files.js`): the dashboard browses a site's `public/`, uploads
(drag and drop, up to 1 GB a file, streamed to disk), edits text files, downloads files and folders (zip,
streamed), renames and moves, copies, changes permissions, extracts zip archives and deletes. The daemon
does it as root, into a directory the site can write to and plant symlinks in, so:

- Every operation goes through an `os.Root` opened on `public/`: a path that resolves outside it fails,
  `..`, absolute paths and the site's links included, so `wp-config.php` (above `public/`), other sites
  and the host are out of reach. Paths are cleaned as absolute ones first (`/../x` is `/x`).
- It does what the site user could, and a little less: entries are added, renamed and removed only in
  directories uid 82 owns and may write; files are changed and re-moded only if uid 82 owns them (and
  written only with the owner's write bit), and whatever it creates is chowned to 82 through its open
  handle. WPGenie's root-owned drop-ins (`object-cache.php`, the page-cache and SMTP must-use plugins) are
  read-only here, as they are to PHP. No setuid/setgid/sticky bits; folders keep `7xx` for the site.
- Files are replaced by writing a dotfile next to them (Caddy never serves dotfiles) and renaming it over
  the old one: PHP and Caddy see the old or the new version, never half, and a hard link planted in the
  docroot can't turn a save into a write to what it links to. Chmod goes through a handle checked
  (`os.SameFile`) to be the entry that was looked at.
- The editor gets UTF-8 text only (≤ 5 MB): a browser's text box would silently replace other bytes and
  the save would corrupt the file. Saves carry the version read (inode, mtime, size); a file changed
  meanwhile (WordPress, SFTP, another tab) is a conflict the editor asks about. CRLF files keep CRLF.
- Archives are checked whole before anything is written: relative, clean, forward-slash paths only (no
  zip slip, drive letters or backslashes), links and special entries skipped, at most 2 GB (declared, and
  counted while writing, so an archive understating its sizes stops too) and 50 000 entries. Existing
  files are only replaced when asked. Copies have the same limits and don't copy links.
- Downloads come from the panel's origin, and a site's files are anyone's who could upload to it: an
  HTML or SVG file shown as a page would run its script as the signed-in user (staff looking at a
  tenant's site). Files are attachments of type `application/octet-stream` with `nosniff` and
  `Content-Security-Policy: default-src 'none'; sandbox`; only raster images are shown inline, as images.
- Changes are refused while the site's logins are off (suspended, or frozen for the last copy of a move,
  like SFTP); a site mid-move can't be browsed. Paths travel in the query string, so the audit log records
  which files every change touched. For a site on another server the requests (uploads and downloads
  streamed) are forwarded like every site route. Viewers browse and download; changes need an operator,
  tenants the plan's `files` feature.

**phpMyAdmin** (`internal/phpmyadmin`, `images/phpmyadmin`) opens on the **site's own domain**
(`https://<site>/_wpgenie/phpmyadmin/`), not the panel's: phpMyAdmin renders whatever the database holds, a
compromised plugin controls that, and on the panel's origin an XSS in phpMyAdmin could act with the
operator's panel session; on the site's origin it reaches nothing the site's code couldn't already.
Caddy routes `/_wpgenie/*` to the daemon before the shield and the WAF (it's SQL, the WAF would block it),
naming the site in a header it sets itself. Opening it creates a temporary MariaDB account with rights on
that one database (5 connections) and a one-time token valid 2 minutes; the token is exchanged for an
`HttpOnly`, `Secure` cookie scoped to the path, and the URL loses the token. The daemon proxies to the
phpMyAdmin container on loopback with the account in headers and a per-start secret the container's router
checks on every request, so neither sites (same Docker network) nor anyone else can use phpMyAdmin
directly. The router serves only phpMyAdmin's entry points and static files, never its libraries.
phpMyAdmin signs in with the session's account (`auth_type config`, `only_db`): there's no login form,
so no other server or account can be reached. It lands on the site's tables, and features that would
write to the WordPress database by themselves (configuration storage, `ZeroConf`) or call out (version
checks, error reports) are off. Imports take up to 64 MB (compressed dumps are bigger). Sessions end after 15
minutes idle or an hour: the account is dropped and its connections killed; the container stops when no
session is left, and leftover accounts are dropped at startup.

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

## Monitoring and alerts

`internal/monitor` watches the server from the outside, the way visitors see it, and tells people
when something breaks.

**Alerts.** Every minute the evaluator runs these checks on *live* sites (active, not staging):

- **Site down**: `site.HTTPProber` (home and login page through Caddy on loopback, full TLS
  verification, the shield's per-process health token) — critical after `down_after` (3) failed
  probes in a row, resolved by the first success. A site that has never been judged healthy and is
  unreachable (no certificate yet: DNS doesn't point here) isn't paged, the update manager's "can't
  be judged" rule; once it has worked, unreachable counts (Caddy down pages every site). A site with a
  running job (restore, push, clone) or WordPress update isn't judged while it runs. Probes aren't
  traffic: the ingester drops requests carrying the health token from the figures (by the token
  Caddy logs, never by User-Agent, which anyone could send to vanish from bandwidth counts).
- **Certificates**: a TLS handshake with `127.0.0.1:443` and SNI = each served and redirecting domain,
  i.e. exactly what browsers get, ACME or uploaded. Warning under 14 days, critical under 3, critical
  when expired, not covering the name or not chaining to a public root. Uploaded certificates that
  don't chain publicly (Cloudflare origin certificates) are judged on names and dates only. Once an
  hour per domain, every ten minutes while the last result was bad. A failed handshake isn't judged
  (no certificate yet; Caddy down is the uptime check's).
- **Disk**: the filesystems of the data directory and the log directory (each once), used as `df`
  counts it: warning at 85%, critical at 95%.
- **Backups**: warning when a scheduled backup hasn't succeeded for two intervals.

State lives in the store: one `alerts` row per watched target once judged (`firing` or `resolved`;
the row's existence is what "was healthy once" means, so it survives restarts), and `alert_history`
(firing, severity changes, resolved; newest 1000). Targets that disappear (a deleted site or domain)
are resolved and forgotten. Thresholds are settings (`GET/PUT /api/v1/monitoring/settings`, admin).

**Notifications** go out on firing, severity change and resolve, and again every `renotify_hours`
(4) while firing: one message per channel per evaluation, listing every change (when Caddy stops, one
message, not fifty). Channels:

- **E-mail** over SMTP with STARTTLS (required, not opportunistic) or implicit TLS; never in clear.
  Header fields are single-line and length-bounded; addresses are validated when saved.
- **Webhooks**: JSON POST with `text` (Slack, Mattermost), `content` (Discord) and the structured
  alerts, signed: `X-WPGenie-Timestamp` and `X-WPGenie-Signature: sha256=hex(HMAC-SHA256(secret,
  timestamp + "." + body))` (receivers should refuse old timestamps). https only, no redirects
  followed, no proxy, and the dialer refuses loopback, link-local (cloud metadata) and multicast
  addresses whatever the name resolves to, so a webhook can't be pointed at the daemon's own APIs.

Deliveries run in the background (at most 16 at once) with retries after 15 s, 1 min and 4 min;
failures are logged and never delay the next evaluation. `POST /api/v1/monitoring/test` sends one test
message per channel and reports each result. Secrets are write-only, like the CDN token: the SMTP
password, webhook URLs (a chat webhook's URL *is* its credential; the API shows scheme and host) and
signing secrets are never returned. Omitted on update they are kept, but only where they would go to
the same place (the SMTP password for the same host, port and user; a webhook's secret for the same
URL), or anyone with API access could send them to a server of their choosing. Missing signing
secrets are generated and shown once.

**Metrics.** `GET /metrics` serves the Prometheus text format (0.0.4) on the daemon's loopback
listener, and through Caddy only when the panel domain is published. It needs its own bearer token
(`POST /api/v1/monitoring/metrics-token`, admin, shown once, only its SHA-256 stored): never the API
token, which can change everything, nor a session. Without a token, `/metrics` answers 401 to
everyone. Series: build info and uptime; per site (label `site`) requests, bytes, page views, bot hits,
shield blocks, 5xx and page cache hits/misses (counters fed in-process by the access-log ingester's
committed batches, reset at restart as Prometheus expects), the PHP response time histogram
(`store.LatencyBuckets`), replicas desired/running, CPU, PHP workers busy and queued, known
vulnerabilities, last successful backup, probe up/duration; certificate expiry and validity per
domain; shield decisions by verdict and active bans; jobs by status; filesystem size/free/available;
host memory; alerts firing by severity. Labels are bounded by design (sites, domains, paths of watched
filesystems, verdicts, statuses), never URLs, IPs or user agents.

**Several servers.** The evaluator only sees its data sources, func fields on `monitor.Service`:
`Sites`, `Probe` and `CertCheck` (given the `*store.Site`, so they route to the node serving it),
`Hosts` (one `Host` per node, with its memory and filesystems; disk alerts are keyed
`disk:<node>:<path>`) and `Nodes`. On a panel with other servers `monitor.Cluster` supplies them (see
*Several servers*); nodes don't evaluate or notify, the panel does it for every server.

## Log shipping

Off until it's turned on in the Logs tab (`internal/logship`, `internal/api/logship.go`). Logs then go to
S3-compatible storage, and the server keeps only a little of them locally.

**What ships.** Each type can be switched on or off. Every type but *containers* is on by default.

| Type | Source | How |
|---|---|---|
| `access` | Caddy's JSON access log; each line gets a `site` from a host → site table the daemon writes | tailed by Vector |
| `php_errors`, `waf` | sites' PHP error logs, Coraza's audit log | spooled by the daemon |
| `security` | the shield's blocks, bans and challenges (otherwise only in memory) | spooled |
| `daemon` | WPGenie's own log (a tee `slog.Handler`, Info and up) | spooled |
| `audit`, `jobs`, `account_events`, `email` | rows of those tables (for e-mail, the outbox's metadata, never bodies) | exported |
| `mail` | docker-mailserver's log files (the panel only) | tailed |
| `containers` | Docker's json-file logs of `wpg-*` and `wpgenie-*` containers | read by the daemon, spooled |

Some types are spooled instead of tailed because the daemon reads those files and then truncates them
(PHP's error logs, the WAF log): a tail would race the truncation. So the daemon's readers tee what they
read. Containers' output is read by the daemon too: Vector could tail it only with Docker's containers
directory mounted, and that directory also holds every container's configuration (database passwords,
keys). The daemon keeps its offsets in memory. It reads a container from the end the first time it sees it
after starting, and a container that appears later from its start, so output written while the daemon was
down isn't shipped. Exported types keep a cursor per type in `ingest_state`, which moves only once the rows
are in the spool and fsynced. Nothing is backfilled on first enable: tails start at the end, exporters at
the newest row, and the spool takes records only while shipping is on.

**Vector.** Logs are shipped by Vector (`timberio/vector`, pinned by digest in `vector_image` in
`config.json`) in the container `wpgenie-vector`.

- **Hardening.** It runs as root with no capabilities at all (`--cap-drop ALL`), `no-new-privileges`, a
  read-only root, a 16 MB `noexec` `/tmp`, 384 MB of memory, 256 PIDs and the default bridge network, never
  the sites' network. Without `DAC_READ_SEARCH` it can't read files it doesn't own. So the daemon makes the
  logs it tails readable to their group and adds that group to the container: Caddy writes the access log
  `0640`, and the mail server's logs and their directory are made group-readable. The WAF log next to the
  access log stays `0600`.
- **Mounts.** Everything is read-only except its data directory (checkpoints and a disk buffer of about
  256 MB) and the spool, whose files it deletes once shipped.
- **Configuration.** The daemon generates `vector.yaml` (0600) with a writer that quotes every value. The
  access keys are in an AWS credentials file (0600, `auth.credentials_file`) in a directory mounted
  read-only. They are never in argv or the container's environment, which `docker inspect` shows.
- **Lifecycle.** The container is recreated only when the hash of its configuration, credentials and
  arguments changes. A change to the enrichment tables alone is a `SIGHUP`. Both are checked every minute and
  on save. Starting it (which may pull the image) is bounded at 10 minutes. A container that keeps crashing
  is left to Docker's restart policy and its growing backoff, and the status shows its exit code and
  restarts. `logship/tmp/` (archives being read) is cleared at start.
- **Status.** Vector's Prometheus exporter listens on the container's own loopback, and the daemon reads it
  through `docker exec`. It supplies events and bytes sent, errors, the last upload and the buffer, which is
  shown apart from the spool.

Objects are written as `<prefix><server>/<type>/YYYY/MM/DD/HH-<uuid>.log.gz` (or `.zst`): newline-delimited
JSON, UTC hours. A batch closes at `batch_max_mb` (10, uncompressed; at most 16, since up to a dozen batches
sit in the container's 384 MB at once) or `batch_max_seconds` (300).

**Spool.** Files are named `logship/spool/<type>/<YYYYMMDDHH>-<n>.jsonl`.

- A file is written as `.part` and renamed once complete (the hour changes, it reaches 8 MiB, or it is a
  minute old), so Vector never reads half a file.
- Each file starts with a random marker line, because Vector recognises files by their first bytes; the
  marker is dropped before shipping. Records are capped at 256 KiB.
- The request path never waits on the spool: records go through a queue of 8192, and overflow is dropped and
  counted. Exporters write synchronously and fsync before their cursors move. If a file can't be finished,
  its synced lines are kept, so nothing a cursor has passed is lost.
- The spool is capped at `spool_cap_mb` (1 GB), checked every 10 seconds. Past the cap the oldest complete
  files are deleted and counted as dropped.
- While the storage is unreachable, Vector's disk buffer fills first. Vector then stops reading, and the spool
  grows up to its cap. The buffer (about 256 MB) comes on top of the spool's cap.

**Keeping the local disk small** while logs ship:

- Caddy keeps `local_access_logs` rotated access logs of 100 MiB (default 2; 10 without shipping).
- Every detached `docker run` the daemon makes gets json-file options of `container_log_mb` (default 10) × 2
  files, when that is Docker's driver. Containers keep the options they were created with until they are
  recreated. The options aren't in their spec hashes, so changing them never restarts anything.
- The compose stack's containers are always capped at 10 MB × 3.
- In the bucket, `archive_retention_days` (default 90; 0 keeps everything) is enforced by the panel. At most
  once a day it deletes older objects with rclone (a throwaway container, keys on stdin), touching only keys
  of the archive's own shape under the current prefix. After a first run that lists the whole prefix, it
  lists only the day prefixes that have expired since, for every server it knows.

**Settings and access** (setting `logship`, admin). The secret key is write-only, and is kept on update only
while the endpoint, bucket and access key stay the same: an administrator can't redirect a stored secret to
a server of their choosing. Endpoints are validated as for uploads offload. Link-local addresses and cloud
metadata services are refused too, since the shipper and rclone would send them the keys and report what they
answer.

- *Test connection* writes and deletes a small object through rclone. Rclone uses the same path-style
  addressing as Vector, as the archive browser and retention do, so the test tells the truth about shipping.
- The archive browser lists one server, type and day. It shows gzip objects decompressed (at most 20 MB),
  and downloads stream any object whole, as stored. zstd objects can only be downloaded. Keys are checked
  against the prefix and the archive's shape.
- Status is open to staff viewers; everything else is admin-only.

**Several servers.** Every server runs its own Vector and exporters, and names its objects by its node ID
(the panel's are `panel`; node IDs `panel` and `unpaired` are reserved). Saving on the panel pushes the
settings, secret included, to every node over the cluster's mutual TLS. Every minute the panel sends them
again to any server that doesn't have the current version, so a server that was down gets them. Nodes have
no mail logs and don't trim the archive. Exporters read each server's own database, whose cursors are its
own; two panel processes on one PostgreSQL would export the panel's tables twice (several panels on one
database isn't supported yet).

The servers share the bucket and its key, so a compromised server can read, and if the key may delete,
remove, every server's logs. Where that matters, turn on versioning or object lock. Or let a lifecycle rule
expire old logs (retention *Forever* in WPGenie) and give the key no delete permission; *Test connection*
then reports that deleting its test object failed.

## Panel database

The panel's own state (sites, users, sessions, audit log, jobs, backups settings, traffic rollups)
lives in `/var/lib/wpgenie/wpgenie.db` (SQLite, WAL mode, one connection, `0600`) unless
`database_url` in `/etc/wpgenie/config.json` names a PostgreSQL database:

```json
"database_url": "postgres://wpgenie:PASSWORD@db.example.com:5432/wpgenie?sslmode=verify-full"
```

Stay on SQLite for a single server: nothing to run or back up separately, and it is fast. Use
PostgreSQL (13 or later, built with ICU, as every mainstream package and managed service is) to keep the
panel's state in a managed or replicated database (point-in-time recovery, a standby panel ready to take
over; nodes of a cluster always keep their own SQLite). The database holds password hashes, TOTP secrets and
backup credentials, so a server that isn't on the same machine (loopback address or Unix socket) is
only reached over TLS: without `sslmode` the connection verifies the certificate and host name
(`verify-full`; system CAs, or `sslrootcert=/path`), `require` and `verify-ca` are respected, and
`disable`, `allow` and `prefer` (which may fall back to plaintext) are refused, by `wpgenie` at
startup and by config validation. Give the role its own database and ownership of it (it creates
tables and a collation when it migrates); nothing else is needed. Schema migrations run at startup
in one transaction under an advisory lock, so two panel processes starting together don't race.

**Moving to PostgreSQL:** create an empty database, stop the daemon, then

```bash
systemctl stop wpgenie
wpgenie store migrate-to-postgres -        # reads the URL from stdin (keeps the password out of ps and history)
```

It checks that the daemon isn't answering, reads the SQLite database read-only in one snapshot,
migrates PostgreSQL to the same schema version, copies every table in one transaction (ids included;
identity sequences continue after the highest id either database handed out), refuses a target that
already has data, verifies the row counts and prints the `database_url` line to add. Start the daemon
and it runs any newer migrations. The SQLite file is left as it was: removing `database_url` goes
back to it (without what was written on PostgreSQL meanwhile).

How the two stay equivalent: queries and migrations are written once, in SQL both accept, with `?`
placeholders (`internal/store/dialect.go`, `translate.go`). On PostgreSQL the store rebinds
placeholders, qualifies upsert column references, maps `IFNULL`, scalar `MAX`/`MIN`, `LIKE`
(case-insensitive, as in SQLite) and `INSERT OR IGNORE`, and translates migrations mechanically
(`INTEGER` → `BIGINT`, `REAL` → `DOUBLE PRECISION`, `BLOB` → `BYTEA`, `TEXT` → `TEXT COLLATE "C"` so
comparisons and sort order match SQLite's, `COLLATE NOCASE` → a case-insensitive ICU collation,
reserved column names quoted). Constructs only one database runs (`INSERT OR REPLACE`, `rowid`,
`strftime`, `LastInsertId`, …) are rejected on both, so the default SQLite test run catches them;
`make test-postgres` runs every store-backed test on PostgreSQL, and the store tests compare the
schema both databases end up with.

## Uploads offload

Per site (off by default), `wp-content/uploads` is copied to S3-compatible object storage (AWS S3, Cloudflare
R2, Backblaze B2, MinIO, …), and Caddy fetches any upload missing on disk from the storage's **public URL**
(the bucket's own address for the prefix, or a CDN in front of it). Local copies can then be removed after
`local_days`, and replicas on other servers (Phase 5) can serve every upload. Settings: endpoint, region,
bucket, key prefix (default `<site-id>/uploads/`), access key ID, secret key (write-only), public URL, an
optional `public-read` ACL for services that need one per object, and `local_days` (0 keeps every local copy).

- **Enabling is checked end to end**: a test object is written with the key, fetched over HTTPS through the
  public URL (a direct `200` with the same bytes; redirects don't count, Caddy wouldn't follow them), and
  deleted. Settings that fail any step are refused with the step's error and not stored. Endpoint and public
  URL must be `https://`; the public URL must use a public hostname (no IP address or internal name: visitors'
  requests are proxied there) and a path of unreserved characters (it goes into the Caddyfile). No two sites
  may use nested prefixes in the same bucket: a site's delete queue can only reach its own objects.
- **rclone** (`rclone/rclone`, pinned by `rclone_image`) runs in a throwaway container per command, like restic:
  no capabilities, read-only root filesystem, bounded memory and PIDs, the default bridge network (never the
  sites' network). The keys go in on stdin, a `KEY=VALUE` block the entrypoint exports (`RCLONE_S3_*`); the
  remote is `:s3:` configured by flags and that environment only (no config file), so nothing secret is in argv
  or `docker inspect`. The key never enters the PHP container and no API returns it.
- **rclone never reads a site's files.** A site controls every path under its docroot and could make
  `wp-content/uploads` a symlink at any moment: bind-mounted, Docker would resolve it on the host (any
  directory published to a public bucket); inside rclone's container it would reach rclone's own
  `/proc/self/environ`, where the keys are. So the daemon walks the uploads through `os.Root` (after an
  `Lstat`/`SameFile` check that it opened the real directory; never following a symlink, never blocking on a
  named pipe, reading only regular files) and copies what needs uploading into a root-only scratch directory
  (`/var/lib/wpgenie/offload/<site>/`), in batches of at most 256 MB / 2 000 files, with their mtimes. rclone
  uploads that directory with `--no-check-dest` (no listing, no request per file); the scratch copy is removed
  after each batch.
- **Copies.** Every minute (and within ~15 s of an upload or delete: PHP touches `logs/offload.poke`), the files
  whose mtime is newer than the start of the last successful copy (minus a 2-minute margin for writes in
  progress), minus those the previous copies already sent unchanged. Nightly, in the maintenance window (or
  after 48 h without one), a **full copy**: the bucket's prefix is listed once (`lsf` with sizes, upload times
  and ETag MD5s) and every file it lacks, or holds with another size or uploaded before the file last changed,
  is uploaded (rclone's `--update --use-server-modtime` rule). Files that arrive with old mtimes (SFTP preserving
  times, restores, pushes) are caught by the full copy. Remote objects are never deleted because a local copy is
  missing: local copies may have been removed on purpose. The first copy after enabling is a full one.
  Content-Type comes from the extension; every object gets `Cache-Control: public, max-age=2592000`.
- **Never offloaded** (they stay on disk only): anything with `.php` (or `.phtml`, `.phar`, …) in its name,
  dotfiles and dot directories, what Caddy refuses to serve (`*.sql`, `*.sql.gz`, `*.bak`, `*.log`, `*.ini`, …),
  the AVIF/WebP copies of images (negotiated from disk only), names with control characters, and directories
  where plugins keep private files behind `.htaccess` rules a public bucket doesn't have: WooCommerce's
  downloadable products and logs, Easy Digital Downloads, Gravity Forms / WPForms / Contact Form 7 uploads,
  backup plugins' archives (UpdraftPlus, BackWPup, WPvivid, All-in-One WP Migration, …), security plugin logs.
- **Deletes.** The mu-plugin wrapper (`wpgenie-offload.php`, loading `offload.php` read-only from the image)
  appends, one per line and relative to the uploads directory, every file of an attachment WordPress deletes
  (`delete_attachment`: the file, the original of a scaled image, every size and edit backups, which covers
  local copies already removed, for which WordPress never calls `wp_delete_file`) and every other
  `wp_delete_file`, to `logs/offload-deletes.queue` in the site directory (PHP-writable, not Caddy-readable),
  under `flock(LOCK_EX)`. The daemon reads it like the PHP error log (one non-blocking `O_NOFOLLOW` open checked
  against `Lstat`, at most 16 MB a pass), holding the same lock from reading to truncating so no line is lost,
  and treats every line as untrusted: relative, already clean (`path.Clean` must not change it), no `..`,
  control characters or backslashes, and wanted by the filters above. Paths go into `offload_deletes` (so a
  failed delete is retried, up to 500 000 waiting per site) and are deleted with `rclone delete
  --files-from-raw -` (the list follows the secret block on stdin). Deletes run before uploads, and a path that
  exists on disk again (a new upload took the name) isn't deleted: it's uploaded instead.
- **Serving.** For a site with offload (or a staging site whose live site has it), Caddy tries the file on disk
  first; a `GET`/`HEAD` under `/wp-content/uploads/` whose path has no `.php` and no dot segment, for a file that
  doesn't exist on disk, is reverse-proxied to the public URL: the path mapped onto its prefix, the query string
  dropped (it could address the storage's API, e.g. `?acl`), the storage's `Host` and TLS server name, no
  cookies, `Authorization`, `Referer` or `X-Forwarded-*` sent; `Set-Cookie` and `X-Amz-*` headers dropped,
  `Cache-Control: public, max-age=2592000` set, and any `4xx` from the storage (XML naming the bucket) turned
  into a plain 404. It runs after the shield, the hardening rules and the AVIF/WebP negotiation, before PHP.
  Negotiation keeps working for files on disk; an image whose local copy was removed is served in its original
  format (its converted copies are removed with it by the nightly conversion, which drops copies without an
  original). `TestOffloadFallbackInRealCaddy` runs this in a real Caddy against MinIO and a stand-in storage
  that echoes what it receives.
- **Removing local copies** (`local_days` > 0) only after a successful full copy, and only for files whose
  mtime is older than `local_days` *and* older than that copy's start, which the listing shows in the bucket
  with the same size, an MD5 equal to the local file's (ETags of multipart uploads aren't MD5s: such files,
  over 200 MB, and buckets with KMS encryption keep their local copies) and an upload time after the file last
  changed; and only if the public URL answers a `HEAD` for one of them with the same length. Each file is
  hashed through a non-blocking `O_NOFOLLOW` open and removed only if it is still the same file (inode, size,
  mtime) afterwards. When in doubt, the file stays. Turning offload off, or moving it to another bucket or
  prefix, is refused while uploads only exist in the bucket, until *Copy back* (a job: the missing files are
  downloaded into the scratch directory and written into uploads through `os.Root`, owned by the site, never
  over an existing file or through a symlink) or an explicit `force`. What removed local copies cost: WordPress
  can't edit or regenerate those images, and backups no longer contain them (the bucket does).
- **Status**: last incremental and full copy, what they uploaded (rclone's JSON statistics), totals, deletes
  waiting, uploads only in the bucket, the last error. Failures are retried with exponential backoff (1 minute
  doubling to an hour) and logged as site events once per distinct error, and again when copies work again.
  Up to three incremental and two full copies run at once across sites.
- **Staging** sites never offload by default (a clone gets no settings, and the live site's wrapper is removed
  from the copy, so it can't queue deletes for the live bucket). A staging site without offload of its own
  serves uploads it lacks from its live site's public URL, read-only (no credentials involved), so uploads the
  live site no longer keeps locally still show.
- **Deleting a site doesn't delete its objects**: the bucket is the operator's. Neither does turning offload
  off.
- `(*site.Service).OffloadEnabled` says whether a site's uploads are offloaded: spreading a site's replicas
  across servers will require it.

## Several servers

A panel can run sites on other servers. Every server is a complete data plane for the sites placed on it
(its own Caddy, PHP containers, MariaDB and Valkey) run by the same daemon: `wpgenie agent` is `serve` as a
*node*, with its own SQLite store, shield, analytics, cron, autoscaler, backups, SFTP and phpMyAdmin for its sites.
The panel (the control plane, itself node `local`) keeps the registry of which site lives where, signs people
in and checks what they may do, and forwards everything about a site to the server it lives on. A node
holds nothing of other nodes' sites, except the install (not the database) of sites spread onto it (below):
a compromised node exposes its own sites, not the cluster, with one exception: shared backup destinations
(below) are on every node with their credentials, so a compromised node can read or delete every server's
backups there. Give servers that shouldn't trust each other destinations of their own. Nothing about a
single-server install changes until the first node is added.

**Mutual TLS** (`internal/cluster`). The panel creates a private CA (ECDSA P-256, `/etc/wpgenie/cluster`,
root only) when the first node is added; its key never leaves the panel. A node's identity is a DNS name in
its certificate (`<id>.nodes.wpgenie`; the panel's also carries `control.wpgenie`), and every connection to
a node verifies the chain against that node's name, and the key against the one it was paired with (the
panel keeps its hash; nodes get each other's with the peer list), so reaching the wrong server fails the
handshake even with a certificate the CA signed. Certificates last a year and are reissued over the channel 60 days before they expire. All traffic is HTTP/2
over TLS 1.3 on port 7443.

**Pairing.** A new server prints a pairing code (`wpgenie agent pair-code`, also at the end of `install.sh
--agent`): the SHA-256 of its public key and a one-time 128-bit secret. The panel dials the address given
with it, refuses the connection unless the server holds that key (so neither the wrong server nor anyone in
between gets anything), issues the node's certificate for that key and hands it over with the secret. The
node accepts it only with the secret, only once (the secret is deleted), and only while unpaired (a paired
node answers nothing without a cluster certificate), with a few wrong guesses per start at most. Neither side
has to trust the network. The panel opens every control connection, so a node never needs to reach the panel
for that; servers do reach each other on 7443 for tunnels while a site moves between them or is spread over
them, the panel's own server included when it is one end (a move onto it, or a site of its own spread).

**What a node accepts.** Panel API requests arrive only over a connection with the panel's certificate,
carrying the acting user (name, role, job owner, client address for the node's audit log); the node applies
the same role checks as the panel. The node's own loopback API ignores those headers (they mean nothing
without the certificate) and takes its API token for local CLI use; it serves no sign-in, accounts, billing,
tickets or mail. Other nodes get only tunnels to targets tied to a relationship the panel set up (below) and
the replica API for sites the panel granted them.

**The registry.** `cluster_sites` holds each remote site's node and the node's own record of it, refreshed
after every change made through the panel and on every health pass (every 30 s: facts, certificates, the
site list). Listings, placement, access checks and domain uniqueness (a domain can't be taken on two servers)
read it, so the panel shows every site when a node is down. Site routes (`/api/v1/sites/{id}/…`) for a
remote site are reverse-proxied to its node as the signed-in user, downloads and job output streaming
through; the session cookie or API token never leave the panel. Server-wide lists (security allow/deny,
bans) are applied on every server; bans and security events are merged into one view. Shared backup
destinations (S3, B2, SFTP) are copied to every node with their password and keys, since a site's backups go
to the same repository whichever server it is on; each server keeps its own `local` repository, and a
destination can't be deleted while sites on any server use it. A site's WordPress mail goes through the
mail server next to the panel: the panel creates the sender mailbox and only sends the node its credentials.

**Accounts across servers.** Tenancy lives on the panel only: a site's account is keyed by its ID
(`site_accounts`, no foreign key to `sites`), so ownership, plan features and quotas are checked on the panel
before anything is forwarded, for sites on every server alike. Handlers with plan checks (resources, autoscale,
domains, backup destinations, staging, pushes) run those checks against the site's record (the registry's copy
for a remote site) and only then forward; a tenant's request reaches the node as an operator's (nodes know
nothing of accounts; the panel already decided), and a tenant deleting its own live site as an admin's.
Billing's per-site operations (suspend, bring back, delete, measure disk, count bandwidth) go to the server
each site lives on (`site.ClusterOps`), so a plan's limits and a suspension cover all of an account's sites.

**Job IDs** tell the panel where a job ran without any change to the dashboard: node number *n* numbers its
jobs from *n*×10¹² (SQLite's `sqlite_sequence`, PostgreSQL's `setval`), so `GET /jobs/{id}` is forwarded by
range and the job list merges every server's. Node numbers are never reused.

**Placement.** A new site goes to the server the admin picks, or else to the one with the smallest share of
its memory already promised to sites (container limits × replicas: limits don't reserve memory), among the
reachable, active servers with at least 5 GB and 10 % of their disk free, then the fewest sites.
`place_on_control: false` keeps sites off the panel's own server. A *draining* server takes no new sites.

**Moving a site** (`POST /sites/{id}/migrate`, a job on the panel; *Drain* moves every site of a server one
after the other). The target creates the site with the same ID and settings but a fresh database account
(status *importing*, not served). A first copy runs while the site is live: files as the site user on both
ends, extracted in two phases as for restores, then the database. The source then goes into WordPress's
maintenance mode (`.maintenance` with a time a day ahead: it holds however long the copy takes, yet can't hold
a site down for good) and a second pass copies the database again and the files whose change time (ctime, which `touch` can't set
back) is after the first pass began, then deletes on the target what the source no longer has (a manifest of
the source's paths): the only downtime, usually seconds to a few minutes. The site's SFTP logins are frozen
from the maintenance page on, so nothing changes behind the last copy. The target goes live with them,
CDN settings, own certificate, backup policy (the local repository becomes the target's own), uploads offload
(with its sync history: the bucket already holds what it says) and mail credentials; the registry switches.
Anything failing before then deletes the half-imported copy and takes the source out of maintenance. If the
panel stops following a move (it died, or the network did), each end cleans up after an hour: an import that
stopped arriving is removed, and a maintenance page a move put up is taken down.

DNS still sends visitors to the old server for a while, so the old server stops its replicas and forwards the
site's domains to the new one through the cluster tunnel, with the visitor's address in a PROXY protocol
header, into a Caddy listener on the new server that is loopback-only and trusts PROXY headers only from
loopback (`127.0.0.1:8444`, only rendered while a peer may forward). There the full site block runs (shield,
WAF, page cache) with the real client address, and PHP sees HTTPS as the visitor did. Let's Encrypt's HTTP
challenges are forwarded too, so the new server has its certificates before DNS moves. The forward and the
old copy last 7 days, or until *Finish move* once DNS points at the new server's public IP; the new server
accepts forwarded visitors only from that old server (the panel tells it), since a peer sending PROXY headers
could claim any address. Staging sites don't move (delete the copy, move the live site, clone again); nor do
spread sites.

**Spreading a site** (`PUT /sites/{id}/spread`). A site still lives on one server, its home: files,
database, object cache, Caddy (static files and the page cache) and cron are there. Some of its PHP replicas
run on other nodes:

- The home keeps a copy of the install on each such node, pushed as the site user whenever its fingerprint
  (paths, sizes, modes, times; caches excluded) changes, checked every minute. Uploads offload is required,
  so the copies never carry the media library (every upload would otherwise be a copy of it to every server).
- The guest's `wp-config.php` is the home's (same salts, so a login works on every replica) with the database
  and cache hosts pointing at `wpg-link-<home>`: the wpgenie binary in *link* mode in a container on the
  guest's Docker network, with no capabilities and a read-only filesystem, tunnelling MariaDB and Valkey to
  the home over the cluster's mutual TLS. The home serves those tunnels only to nodes running its sites'
  replicas; nothing is published on any network, Valkey not even on the host's loopback.
- The home's Caddy load-balances page views over every replica, reaching the guests' through local tunnel
  ports (the guest serves `fpm:<port>` only to the home of that replica); anything that may write files
  (wp-admin, sign-in, any non-GET request) goes only to home replicas, so the copies never diverge.
- A guest runs replicas only of a site the panel granted to that site's home, never over a site of its own
  (same path), and checks the spec it is sent (PHP version, memory, CPU, settings) as if typed in the panel: a
  node can't make another run its code.
- The link container mounts the binary and this node's own certificate, key and CA, file by file (not the
  cluster directory); the panel's server is never a guest (its directory holds the CA's key).
- Files a page view generates on a guest (CSS and JS plugins build under `uploads` or `wp-content/cache`) are
  sent back to the home every minute, since the home's Caddy serves them from its disk. The home takes them
  from that site's guests only, as the site user, under those two directories, no PHP, symlinks, dotfiles or
  page cache, and only where it has no file yet: a guest can't replace the home's files.
- Replicas are shared out home first (one replica stays home; two put one elsewhere); autoscaling reads the
  home replicas. Every replica counts against the site's database connection limit.

What spreading doesn't change: every site still reaches only its own cache keys (its Valkey user, see
*Caching*); the link gives a guest's other sites a route to the home's Valkey and MariaDB, but no user
there. Rate limits and bans are per server (each shield sees its own traffic).

**Monitoring.** The panel watches every server: remote sites' uptime and certificates are checked through the
tunnel to their own Caddy with that node's health token (fetched over mTLS, kept in memory only), disks from
each node's health checks (last known figures while it is unreachable, so its alerts aren't resolved as if it
were gone), and a node that stops answering for 3 minutes is an alert of its own. `/metrics?node=<id>` serves a
node's metrics through the panel: one scrape token, no route from Prometheus to the nodes. The shield's health
token is hashed in Caddy's access log (the ingester recognises probes by the hash), so the log never holds it.

**Updates.** A node updates itself like the panel (signed releases, rollback), when asked from *Servers*; it is
a systemd drop-in (`wpgenie.service.d/agent.conf`) that makes the unit run `wpgenie agent`, so updates, which
replace the unit file, keep it a node.

## Directory layout on a server

```
/etc/wpgenie/config.json          panel config + secrets (0600)
/etc/wpgenie/infra.env            MariaDB root password (0600)
/etc/wpgenie/caddy/Caddyfile      generated; last config Caddy accepted
/var/lib/wpgenie/wpgenie.db       panel state (SQLite; kept as it was after moving to PostgreSQL)
/var/lib/wpgenie/sites/<id>/      root:82 0751; wp-config.php (root:82 0640)
/var/lib/wpgenie/sites/<id>/public/   WordPress (82:82)
/var/lib/wpgenie/sites/<id>/logs/     PHP's error log (82:82 0750; read by the daemon)
/var/lib/wpgenie/sites.nosymfollow/   read-only, symlink-free view of sites/ (Caddy's only view)
/var/lib/wpgenie/caddy/           certificates (owned by wpgenie-caddy)
/var/lib/wpgenie/mariadb/         databases
/var/lib/wpgenie/snapshots/       pre-update snapshots (root only)
/var/lib/wpgenie/backups/local/   the local restic repository (root only)
/var/lib/wpgenie/backups/cache/   restic's cache; staging/ holds database dumps while backing up
/var/lib/wpgenie/offload/<id>/    uploads on their way to object storage (root only, emptied after each batch)
/var/lib/wpgenie/sftp/            SFTP accounts (config/) and host keys
/etc/wpgenie/caddy/certs/<site>/  sites' own TLS certificates
/etc/wpgenie/cluster/             cluster CA and identity (panel: ca.key, control.*; node: node.key,
                                  node.pem, ca.pem, node.json; pairing.json until paired), root only
/var/lib/wpgenie/iprep/           IP blocklists and the country database
/var/lib/wpgenie/updates/         staged WPGenie releases, status.json
/var/lib/wpgenie/mail/            mailboxes (data/), mail server config, Roundcube DB
/var/lib/wpgenie/support/<ticket>/  ticket attachments under random names (0600); .staging/ while uploading
/var/lib/wpgenie/logship/         log shipping: vector.yaml and secrets/credentials (0600), tables/
                                  (host → site), data/ (Vector's checkpoints and disk buffer), spool/<type>/
                                  (what the daemon ships), tmp/ (archives being read; cleared at start)
/var/log/wpgenie/access.log       JSON access log (rotated by Caddy: 10 old files kept, `local_access_logs`
                                  while shipped)
/var/log/wpgenie/waf.log          Coraza audit log (read and truncated by the daemon)
/opt/wpgenie/                     installed sources (deploy/, images/)
```
