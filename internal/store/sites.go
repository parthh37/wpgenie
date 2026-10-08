package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

type SiteStatus string

const (
	StatusProvisioning SiteStatus = "provisioning"
	StatusActive       SiteStatus = "active"
	StatusFailed       SiteStatus = "failed"
	// StatusSuspended: the site's account is suspended (billing, overage,
	// an administrator). Caddy answers 503 for its domains, its PHP
	// containers are stopped, and cron, backups and updates skip it; its
	// files and database are kept.
	StatusSuspended SiteStatus = "suspended"
)

type Site struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	PrimaryDomain string   `json:"primary_domain"`
	Domains       []string `json:"domains"`
	PHPVersion    string   `json:"php_version"`
	// FPMPort is a port reserved for the site at creation; the ports it is
	// actually served on are Upstreams (one per PHP-FPM replica).
	FPMPort     int        `json:"-"`
	DBName      string     `json:"db_name"`
	Status      SiteStatus `json:"status"`
	ShieldMode  string     `json:"shield_mode"`
	BlockAIBots bool       `json:"block_ai_bots"`
	MemoryMB    int        `json:"memory_mb"`
	CPUs        float64    `json:"cpus"`
	Replicas    int        `json:"replicas"`
	PageCache   bool       `json:"page_cache"`
	ObjectCache bool       `json:"object_cache"`
	// CacheMobile stores every page as separate mobile and desktop copies
	// (themes that detect phones themselves); otherwise only pages that ask
	// wp_is_mobile() get them.
	CacheMobile bool  `json:"cache_mobile"`
	Upstreams   []int `json:"upstream_ports"`
	// RemoteUpstreams are replicas on other nodes (spread sites), each
	// reached through a local tunnel port; SpreadNodes are the nodes the
	// site's replicas may also run on.
	RemoteUpstreams []RemoteUpstream `json:"remote_upstreams,omitempty"`
	SpreadNodes     []string         `json:"spread_nodes"`
	// Node is where the site lives, set by the control plane in listings
	// ("" or "local": the panel's own server).
	Node string `json:"node,omitempty"`
	// ImageFormats are the formats uploads are converted to and served in
	// ("avif", "webp"); empty: off.
	ImageFormats []string `json:"image_formats"`
	// Optimize lists the WordPress performance tweaks the site's optimize
	// mu-plugin applies (site.Optimizations keys); empty: off.
	Optimize []string `json:"optimize"`
	// Shield: request inspection, and IP networks (normalised prefixes).
	WAF        bool     `json:"waf"`
	AdminAllow []string `json:"admin_allow"`
	TrustedIPs []string `json:"trusted_ips"`
	DenyIPs    []string `json:"deny_ips"`
	// XMLRPC lets requests reach xmlrpc.php (Jetpack, the mobile apps).
	XMLRPC bool `json:"xmlrpc"`
	// Per-IP rate limits and the challenge's proof-of-work difficulty
	// (leading zero bits); 0 means the server default.
	RateRPS       float64 `json:"rate_rps"`
	RateBurst     int     `json:"rate_burst"`
	LoginPerMin   float64 `json:"login_per_min"`
	ChallengeBits int     `json:"challenge_bits"`
	// Reputation is what clients on IP blocklists get: off, challenge, block.
	Reputation string `json:"reputation"`
	// Country rules: mode off, block (the listed countries) or allow (only
	// them); the others get CountryAction (block or challenge).
	CountryMode   string   `json:"country_mode"`
	Countries     []string `json:"countries"`
	CountryAction string   `json:"country_action"`
	// BodyWAF is request-body inspection (Coraza + OWASP CRS): off, detect
	// (log matches only) or block.
	BodyWAF string `json:"body_waf"`
	// CPU autoscaling between MinReplicas and MaxReplicas, aiming to keep
	// each replica's CPU use near TargetCPU percent of its allowance.
	Autoscale   bool `json:"autoscale"`
	MinReplicas int  `json:"min_replicas"`
	MaxReplicas int  `json:"max_replicas"`
	TargetCPU   int  `json:"target_cpu"`
	// TargetWorkers (percent of PHP workers busy, queued requests included)
	// and TargetResponseMS (95th percentile of PHP response times) also
	// drive autoscaling; 0 turns either off.
	TargetWorkers    int `json:"target_workers"`
	TargetResponseMS int `json:"target_response_ms"`
	// Burst is how the site gets extra instances under load, paid for in
	// burst minutes: off, auto (only while the load needs them) or on
	// (at least one extra instance until BurstUntil, zero: until turned
	// off). BurstPaused: the site's account has no minutes left, so it
	// stays at its normal size (MinReplicas) until it does.
	BurstMode   string    `json:"burst_mode"`
	BurstUntil  time.Time `json:"burst_until,omitzero"`
	BurstPaused bool      `json:"burst_paused"`
	// AutoUpdate is the nightly WordPress update policy: off, security or all.
	AutoUpdate string `json:"auto_update"`
	// SMTP: WordPress sends its mail through the WPGenie mail server.
	SMTP bool `json:"smtp"`
	// RedirectDomains answer with a permanent redirect to PrimaryDomain
	// (www <-> apex, old names); Domains are served.
	RedirectDomains []string `json:"redirect_domains"`
	// ParentID is the live site a staging site was cloned from ("" for
	// live sites).
	ParentID string `json:"parent_id"`
	// The site lock: visitors need a username and password (HTTP basic
	// authentication, at the edge) unless their address is in LockAllow
	// (normalised prefixes). LockHash is the password's bcrypt hash: never
	// sent to clients (nor in the cluster registry's copy). Turning the lock
	// off keeps the username and password for turning it back on.
	Lock      bool     `json:"site_lock"`
	LockUser  string   `json:"site_lock_user"`
	LockHash  string   `json:"-"`
	LockAllow []string `json:"site_lock_allow"`
	// PHP holds per-site PHP settings; zero values mean the image defaults.
	PHP       PHPSettings `json:"php"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

const siteCols = `id, name, primary_domain, php_version, fpm_port, db_name, status, shield_mode, block_ai_bots,
	memory_mb, cpus, replicas, page_cache, object_cache, waf, admin_allow, trusted_ips,
	autoscale, min_replicas, max_replicas, target_cpu, auto_update, smtp,
	xmlrpc, rate_rps, rate_burst, login_per_min, challenge_bits, deny_ips, reputation,
	country_mode, countries, country_action, body_waf, parent_id, php_settings,
	cache_mobile, image_formats, target_workers, target_response_ms, optimize, burst_mode, burst_until, burst_paused,
	lock_on, lock_user, lock_hash, lock_allow, created_at, updated_at`

// PHPSettings are per-site PHP limits. Zero means the image default
// (images/php/php.ini).
type PHPSettings struct {
	MemoryLimitMB    int `json:"memory_limit_mb"`
	UploadMaxMB      int `json:"upload_max_mb"`
	MaxExecutionTime int `json:"max_execution_time"`
	MaxInputVars     int `json:"max_input_vars"`
}

func scanSite(row interface{ Scan(...any) error }) (*Site, error) {
	var s Site
	var created, updated, burstUntil int64
	var adminAllow, trusted, deny, countries, php, images, optimize, lockAllow string
	err := row.Scan(&s.ID, &s.Name, &s.PrimaryDomain, &s.PHPVersion, &s.FPMPort, &s.DBName,
		&s.Status, &s.ShieldMode, &s.BlockAIBots, &s.MemoryMB, &s.CPUs, &s.Replicas, &s.PageCache, &s.ObjectCache,
		&s.WAF, &adminAllow, &trusted, &s.Autoscale, &s.MinReplicas, &s.MaxReplicas, &s.TargetCPU, &s.AutoUpdate,
		&s.SMTP, &s.XMLRPC, &s.RateRPS, &s.RateBurst, &s.LoginPerMin, &s.ChallengeBits, &deny, &s.Reputation,
		&s.CountryMode, &countries, &s.CountryAction, &s.BodyWAF, &s.ParentID, &php,
		&s.CacheMobile, &images, &s.TargetWorkers, &s.TargetResponseMS, &optimize, &s.BurstMode, &burstUntil,
		&s.BurstPaused, &s.Lock, &s.LockUser, &s.LockHash, &lockAllow, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.AdminAllow, s.TrustedIPs, s.DenyIPs = splitList(adminAllow), splitList(trusted), splitList(deny)
	s.Countries = splitList(countries)
	s.ImageFormats = splitList(images)
	s.Optimize = splitList(optimize)
	s.LockAllow = splitList(lockAllow)
	if err := json.Unmarshal([]byte(php), &s.PHP); err != nil {
		return nil, err
	}
	if burstUntil > 0 {
		s.BurstUntil = time.Unix(burstUntil, 0).UTC()
	}
	s.CreatedAt, s.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return &s, nil
}

func splitList(v string) []string {
	if v == "" {
		return []string{}
	}
	return strings.Split(v, ",")
}

// CreateSite inserts the site, its primary domain and its first upstream
// (FPMPort) atomically. The domain PRIMARY KEY is what guarantees one domain
// can never point at two sites.
func (s *Store) CreateSite(ctx context.Context, site *Site) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if site.MinReplicas == 0 {
		site.MinReplicas, site.MaxReplicas = 1, 1
	}
	if site.TargetCPU == 0 {
		site.TargetCPU = 70
	}
	if site.AutoUpdate == "" {
		site.AutoUpdate = "security"
	}
	if site.Reputation == "" {
		site.Reputation = "challenge"
	}
	if site.CountryMode == "" {
		site.CountryMode = "off"
	}
	if site.CountryAction == "" {
		site.CountryAction = "block"
	}
	if site.BodyWAF == "" {
		site.BodyWAF = "off"
	}
	if site.BurstMode == "" {
		site.BurstMode = "off"
	}
	php, err := json.Marshal(site.PHP)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sites (`+siteCols+`) VALUES (`+strings.Repeat("?,", 49)+`?)`,
		site.ID, site.Name, site.PrimaryDomain, site.PHPVersion, site.FPMPort, site.DBName,
		site.Status, site.ShieldMode, site.BlockAIBots,
		site.MemoryMB, site.CPUs, site.Replicas, site.PageCache, site.ObjectCache,
		site.WAF, strings.Join(site.AdminAllow, ","), strings.Join(site.TrustedIPs, ","),
		site.Autoscale, site.MinReplicas, site.MaxReplicas, site.TargetCPU, site.AutoUpdate, site.SMTP,
		site.XMLRPC, site.RateRPS, site.RateBurst, site.LoginPerMin, site.ChallengeBits,
		strings.Join(site.DenyIPs, ","), site.Reputation, site.CountryMode, strings.Join(site.Countries, ","),
		site.CountryAction, site.BodyWAF, site.ParentID, string(php),
		site.CacheMobile, strings.Join(site.ImageFormats, ","), site.TargetWorkers, site.TargetResponseMS,
		strings.Join(site.Optimize, ","), site.BurstMode, unixOrZero(site.BurstUntil), site.BurstPaused,
		site.Lock, site.LockUser, site.LockHash, strings.Join(site.LockAllow, ","), now, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO site_upstreams (port, site_id) VALUES (?, ?)`, site.FPMPort, site.ID); err != nil {
		return err
	}
	domains := site.Domains
	if len(domains) == 0 {
		domains = []string{site.PrimaryDomain}
	}
	for _, d := range domains {
		if _, err := tx.ExecContext(ctx, `INSERT INTO site_domains (domain, site_id) VALUES (?, ?)`, d, site.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetSite(ctx context.Context, id string) (*Site, error) {
	site, err := scanSite(s.db.QueryRowContext(ctx, `SELECT `+siteCols+` FROM sites WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if site.Domains, site.RedirectDomains, err = s.siteDomains(ctx, id); err != nil {
		return nil, err
	}
	return site, s.siteCluster(ctx, site)
}

func (s *Store) ListSites(ctx context.Context) ([]*Site, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+siteCols+` FROM sites ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	var sites []*Site
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sites = append(sites, site)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, site := range sites {
		if site.Domains, site.RedirectDomains, err = s.siteDomains(ctx, site.ID); err != nil {
			return nil, err
		}
		if err := s.siteCluster(ctx, site); err != nil {
			return nil, err
		}
	}
	return sites, nil
}

// siteDomains returns the domains a site serves and those that redirect
// to its primary domain.
func (s *Store) siteDomains(ctx context.Context, id string) (serve, redirect []string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain, redirect FROM site_domains WHERE site_id = ? ORDER BY domain`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	serve, redirect = []string{}, []string{}
	for rows.Next() {
		var d string
		var r bool
		if err := rows.Scan(&d, &r); err != nil {
			return nil, nil, err
		}
		if r {
			redirect = append(redirect, d)
		} else {
			serve = append(serve, d)
		}
	}
	return serve, redirect, rows.Err()
}

// AddDomain attaches a domain to a site. The domain PRIMARY KEY refuses
// one already attached anywhere (ErrDomainTaken is the caller's check).
func (s *Store) AddDomain(ctx context.Context, siteID, domain string, redirect bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_domains (domain, site_id, redirect) VALUES (?, ?, ?)`,
		domain, siteID, redirect)
	return err
}

// RemoveDomain detaches a domain that isn't the site's primary one.
func (s *Store) RemoveDomain(ctx context.Context, siteID, domain string) error {
	return s.exec1(ctx, `DELETE FROM site_domains WHERE site_id = ? AND domain = ?
		AND domain <> (SELECT primary_domain FROM sites WHERE id = ?)`, siteID, domain, siteID)
}

// SetPrimaryDomain makes an attached domain the primary (served) one; the
// old primary stays attached as a redirect to it.
func (s *Store) SetPrimaryDomain(ctx context.Context, siteID, domain string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Locked: two concurrent changes must not both demote the same old
	// primary (PostgreSQL; SQLite runs one transaction at a time).
	var old string
	if err := tx.QueryRowContext(ctx, `SELECT primary_domain FROM sites WHERE id = ? FOR UPDATE`, siteID).Scan(&old); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE site_domains SET redirect = 0 WHERE site_id = ? AND domain = ?`, siteID, domain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if old != domain {
		if _, err := tx.ExecContext(ctx, `UPDATE site_domains SET redirect = 1 WHERE site_id = ? AND domain = ?`, siteID, old); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sites SET primary_domain = ?, updated_at = ? WHERE id = ?`,
		domain, time.Now().Unix(), siteID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetDomainRedirect switches an attached, non-primary domain between being
// served and redirecting to the primary domain.
func (s *Store) SetDomainRedirect(ctx context.Context, siteID, domain string, redirect bool) error {
	return s.exec1(ctx, `UPDATE site_domains SET redirect = ? WHERE site_id = ? AND domain = ?
		AND domain <> (SELECT primary_domain FROM sites WHERE id = ?)`, redirect, siteID, domain, siteID)
}

func (s *Store) DomainExists(ctx context.Context, domain string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM site_domains WHERE domain = ?`, domain).Scan(&n)
	return n > 0, err
}

// DomainIndex maps every hostname to its site ID; used by log ingestion.
func (s *Store) DomainIndex(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain, site_id FROM site_domains`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[string]string{}
	for rows.Next() {
		var d, id string
		if err := rows.Scan(&d, &id); err != nil {
			return nil, err
		}
		idx[d] = id
	}
	return idx, rows.Err()
}

func (s *Store) siteUpstreams(ctx context.Context, id string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM site_upstreams WHERE site_id = ? AND node_id = '' ORDER BY port`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AllocatePorts returns the n lowest ports >= base that no site is using or
// has reserved, skipping exclude. The caller must serialise allocation with
// recording the ports (SetUpstreams / CreateSite), or two operations could
// get the same port: internal/site does so with a process-wide mutex. Two
// control-plane processes sharing a PostgreSQL store aren't covered by it,
// but the PRIMARY KEY on site_upstreams.port and the UNIQUE fpm_port make
// the second one's insert fail rather than share a port.
func (s *Store) AllocatePorts(ctx context.Context, base, n int, exclude ...int) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM site_upstreams UNION SELECT fpm_port FROM sites
		UNION SELECT port FROM guest_replicas UNION SELECT port FROM site_forwards
		UNION SELECT http_port FROM site_forwards`)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, p := range exclude {
		used[p] = true
	}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		used[p] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []int
	for p := base; len(out) < n && p <= 65535; p++ {
		if !used[p] {
			out = append(out, p)
		}
	}
	if len(out) < n {
		return nil, errors.New("no free loopback ports left for PHP-FPM")
	}
	return out, nil
}

// SetUpstreams replaces the set of local ports a site is served on.
func (s *Store) SetUpstreams(ctx context.Context, id string, ports []int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Only the local replicas: replicas on other nodes are SetRemoteUpstreams'.
	// The site's row lock orders concurrent replacements (PostgreSQL):
	// otherwise the second DELETE misses the rows the first one inserts and
	// the site ends up with both sets.
	if err := tx.QueryRowContext(ctx, `SELECT id FROM sites WHERE id = ? FOR UPDATE`, id).Scan(new(string)); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM site_upstreams WHERE site_id = ? AND node_id = ''`, id); err != nil {
		return err
	}
	for _, p := range ports {
		if _, err := tx.ExecContext(ctx, `INSERT INTO site_upstreams (port, site_id) VALUES (?, ?)`, p, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetResources(ctx context.Context, id string, memoryMB int, cpus float64, replicas int) error {
	return s.exec1(ctx, `UPDATE sites SET memory_mb = ?, cpus = ?, replicas = ?, updated_at = ? WHERE id = ?`,
		memoryMB, cpus, replicas, time.Now().Unix(), id)
}

func (s *Store) SetCache(ctx context.Context, id string, page, object, mobile bool) error {
	return s.exec1(ctx, `UPDATE sites SET page_cache = ?, object_cache = ?, cache_mobile = ?, updated_at = ? WHERE id = ?`,
		page, object, mobile, time.Now().Unix(), id)
}

func (s *Store) SetImageFormats(ctx context.Context, id string, formats []string) error {
	return s.exec1(ctx, `UPDATE sites SET image_formats = ?, updated_at = ? WHERE id = ?`,
		strings.Join(formats, ","), time.Now().Unix(), id)
}

// SetOptimize records a site's WordPress performance tweaks.
func (s *Store) SetOptimize(ctx context.Context, id string, keys []string) error {
	return s.exec1(ctx, `UPDATE sites SET optimize = ?, updated_at = ? WHERE id = ?`,
		strings.Join(keys, ","), time.Now().Unix(), id)
}

func (s *Store) SetSiteStatus(ctx context.Context, id string, st SiteStatus) error {
	return s.exec1(ctx, `UPDATE sites SET status = ?, updated_at = ? WHERE id = ?`, st, time.Now().Unix(), id)
}

// ShieldSettings is the stored per-site shield configuration.
type ShieldSettings struct {
	Mode          string
	BlockAIBots   bool
	WAF           bool
	AdminAllow    []string
	TrustedIPs    []string
	DenyIPs       []string
	XMLRPC        bool
	RateRPS       float64
	RateBurst     int
	LoginPerMin   float64
	ChallengeBits int
	Reputation    string
	CountryMode   string
	Countries     []string
	CountryAction string
	BodyWAF       string
}

// ShieldSettings returns the shield part of a site record.
func (st *Site) ShieldSettings() ShieldSettings {
	return ShieldSettings{Mode: st.ShieldMode, BlockAIBots: st.BlockAIBots, WAF: st.WAF,
		AdminAllow: st.AdminAllow, TrustedIPs: st.TrustedIPs, DenyIPs: st.DenyIPs, XMLRPC: st.XMLRPC,
		RateRPS: st.RateRPS, RateBurst: st.RateBurst, LoginPerMin: st.LoginPerMin, ChallengeBits: st.ChallengeBits,
		Reputation: st.Reputation, CountryMode: st.CountryMode, Countries: st.Countries,
		CountryAction: st.CountryAction, BodyWAF: st.BodyWAF}
}

func (s *Store) SetShield(ctx context.Context, id string, c ShieldSettings) error {
	return s.exec1(ctx, `UPDATE sites SET shield_mode = ?, block_ai_bots = ?, waf = ?, admin_allow = ?, trusted_ips = ?,
		deny_ips = ?, xmlrpc = ?, rate_rps = ?, rate_burst = ?, login_per_min = ?, challenge_bits = ?, reputation = ?,
		country_mode = ?, countries = ?, country_action = ?, body_waf = ?, updated_at = ? WHERE id = ?`,
		c.Mode, c.BlockAIBots, c.WAF, strings.Join(c.AdminAllow, ","), strings.Join(c.TrustedIPs, ","),
		strings.Join(c.DenyIPs, ","), c.XMLRPC, c.RateRPS, c.RateBurst, c.LoginPerMin, c.ChallengeBits, c.Reputation,
		c.CountryMode, strings.Join(c.Countries, ","), c.CountryAction, c.BodyWAF, time.Now().Unix(), id)
}

// SetScaling records a site's autoscaling and burst together (one write:
// they describe one decision).
func (s *Store) SetScaling(ctx context.Context, id string, on bool, minR, maxR, targetCPU, targetWorkers, targetMS int,
	burstMode string, burstUntil time.Time) error {
	return s.exec1(ctx, `UPDATE sites SET autoscale = ?, min_replicas = ?, max_replicas = ?, target_cpu = ?,
		target_workers = ?, target_response_ms = ?, burst_mode = ?, burst_until = ?, updated_at = ? WHERE id = ?`,
		on, minR, maxR, targetCPU, targetWorkers, targetMS, burstMode, unixOrZero(burstUntil), time.Now().Unix(), id)
}

// SetBurst records a site's burst mode and when "on" ends (zero: when
// turned off).
func (s *Store) SetBurst(ctx context.Context, id, mode string, until time.Time) error {
	return s.exec1(ctx, `UPDATE sites SET burst_mode = ?, burst_until = ?, updated_at = ? WHERE id = ?`,
		mode, unixOrZero(until), time.Now().Unix(), id)
}

// SetBurstPaused records whether a site's burst is paused for want of
// minutes.
func (s *Store) SetBurstPaused(ctx context.Context, id string, paused bool) error {
	return s.exec1(ctx, `UPDATE sites SET burst_paused = ?, updated_at = ? WHERE id = ?`, paused, time.Now().Unix(), id)
}

func (s *Store) SetPHP(ctx context.Context, id, version string, settings PHPSettings) error {
	b, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return s.exec1(ctx, `UPDATE sites SET php_version = ?, php_settings = ?, updated_at = ? WHERE id = ?`,
		version, string(b), time.Now().Unix(), id)
}

// StagingOf returns the staging sites cloned from a live site.
func (s *Store) StagingOf(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sites WHERE parent_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

func (s *Store) SetSMTP(ctx context.Context, id string, on bool) error {
	return s.exec1(ctx, `UPDATE sites SET smtp = ?, updated_at = ? WHERE id = ?`, on, time.Now().Unix(), id)
}

func (s *Store) SetAutoUpdate(ctx context.Context, id, policy string) error {
	return s.exec1(ctx, `UPDATE sites SET auto_update = ?, updated_at = ? WHERE id = ?`, policy, time.Now().Unix(), id)
}

// DeleteSite removes a site's record. Its account ownership and usage go
// in the same transaction (those tables have no foreign key to sites: the
// site may live on another node), so a deleted site never leaves an owner
// behind for a later site to inherit.
func (s *Store) DeleteSite(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM sites WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := deleteSiteOwnership(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SuspendedSiteIDs returns the sites whose logins are off: suspended,
// arriving from or gone to another server, or frozen for the final copy of
// a move (FreezeSite).
func (s *Store) SuspendedSiteIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sites WHERE status IN (?, ?, ?)`,
		StatusSuspended, StatusImporting, StatusMoved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	frozen, err := s.frozenSites(ctx)
	for id := range frozen {
		out[id] = true
	}
	return out, err
}

// Site statuses of a move between servers (see site/migrate.go).
const (
	StatusImporting SiteStatus = "importing"
	StatusMoved     SiteStatus = "moved"
)

const settingFrozen = "sites_frozen"

func (s *Store) frozenSites(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	v, err := s.Setting(ctx, settingFrozen)
	if err != nil || v == "" {
		return out, err
	}
	for _, id := range strings.Split(v, ",") {
		if id != "" {
			out[id] = true
		}
	}
	return out, nil
}

// FreezeSite keeps a site's logins (SFTP) off while its files are copied
// for the last time; false lets them back.
func (s *Store) FreezeSite(ctx context.Context, id string, frozen bool) error {
	m, err := s.frozenSites(ctx)
	if err != nil {
		return err
	}
	if frozen {
		m[id] = true
	} else {
		delete(m, id)
	}
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	slices.Sort(ids)
	return s.SetSetting(ctx, settingFrozen, strings.Join(ids, ","))
}

func (s *Store) exec1(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
