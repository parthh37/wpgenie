package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

type SiteStatus string

const (
	StatusProvisioning SiteStatus = "provisioning"
	StatusActive       SiteStatus = "active"
	StatusFailed       SiteStatus = "failed"
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
	Upstreams   []int      `json:"upstream_ports"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

const siteCols = `id, name, primary_domain, php_version, fpm_port, db_name, status, shield_mode, block_ai_bots,
	memory_mb, cpus, replicas, page_cache, object_cache, created_at, updated_at`

func scanSite(row interface{ Scan(...any) error }) (*Site, error) {
	var s Site
	var created, updated int64
	err := row.Scan(&s.ID, &s.Name, &s.PrimaryDomain, &s.PHPVersion, &s.FPMPort, &s.DBName,
		&s.Status, &s.ShieldMode, &s.BlockAIBots, &s.MemoryMB, &s.CPUs, &s.Replicas, &s.PageCache, &s.ObjectCache,
		&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, s.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return &s, nil
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
	_, err = tx.ExecContext(ctx, `INSERT INTO sites (`+siteCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		site.ID, site.Name, site.PrimaryDomain, site.PHPVersion, site.FPMPort, site.DBName,
		site.Status, site.ShieldMode, site.BlockAIBots,
		site.MemoryMB, site.CPUs, site.Replicas, site.PageCache, site.ObjectCache, now, now)
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
	if site.Domains, err = s.siteDomains(ctx, id); err != nil {
		return nil, err
	}
	site.Upstreams, err = s.siteUpstreams(ctx, id)
	return site, err
}

func (s *Store) ListSites(ctx context.Context) ([]*Site, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+siteCols+` FROM sites ORDER BY created_at`)
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
		if site.Domains, err = s.siteDomains(ctx, site.ID); err != nil {
			return nil, err
		}
		if site.Upstreams, err = s.siteUpstreams(ctx, site.ID); err != nil {
			return nil, err
		}
	}
	return sites, nil
}

func (s *Store) siteDomains(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain FROM site_domains WHERE site_id = ? ORDER BY domain`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
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
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM site_upstreams WHERE site_id = ? ORDER BY port`, id)
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
// get the same port.
func (s *Store) AllocatePorts(ctx context.Context, base, n int, exclude ...int) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM site_upstreams UNION SELECT fpm_port FROM sites`)
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

// SetUpstreams replaces the set of ports a site is served on.
func (s *Store) SetUpstreams(ctx context.Context, id string, ports []int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM site_upstreams WHERE site_id = ?`, id); err != nil {
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

func (s *Store) SetCache(ctx context.Context, id string, page, object bool) error {
	return s.exec1(ctx, `UPDATE sites SET page_cache = ?, object_cache = ?, updated_at = ? WHERE id = ?`,
		page, object, time.Now().Unix(), id)
}

func (s *Store) SetSiteStatus(ctx context.Context, id string, st SiteStatus) error {
	return s.exec1(ctx, `UPDATE sites SET status = ?, updated_at = ? WHERE id = ?`, st, time.Now().Unix(), id)
}

func (s *Store) SetShield(ctx context.Context, id, mode string, blockAI bool) error {
	return s.exec1(ctx, `UPDATE sites SET shield_mode = ?, block_ai_bots = ?, updated_at = ? WHERE id = ?`,
		mode, blockAI, time.Now().Unix(), id)
}

func (s *Store) DeleteSite(ctx context.Context, id string) error {
	return s.exec1(ctx, `DELETE FROM sites WHERE id = ?`, id)
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
