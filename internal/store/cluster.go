package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Node is a server running `wpgenie agent`, as the control plane sees it.
type Node struct {
	ID      string `json:"id"`
	Num     int64  `json:"num"`
	Name    string `json:"name"`
	Address string `json:"address"` // its cluster listener, host:port
	// PublicIP is where sites on it expect their DNS to point.
	PublicIP string `json:"public_ip"`
	// Status: active, or draining (no new sites; its sites are being moved).
	Status       string          `json:"status"`
	Info         json.RawMessage `json:"info"`
	CertNotAfter time.Time       `json:"cert_not_after"`
	LastSeen     time.Time       `json:"last_seen,omitzero"`
	LastError    string          `json:"last_error,omitempty"`
	// KeyHash pins the node's public key (SHA-256, hex), fixed at pairing.
	KeyHash   string    `json:"key_hash"`
	CreatedAt time.Time `json:"created_at"`
}

const (
	NodeActive   = "active"
	NodeDraining = "draining"
)

const nodeCols = `id, num, name, address, public_ip, status, info, cert_not_after, last_seen, last_error, key_hash, created_at`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var info string
	var notAfter, seen, created int64
	err := row.Scan(&n.ID, &n.Num, &n.Name, &n.Address, &n.PublicIP, &n.Status, &info, &notAfter, &seen, &n.LastError,
		&n.KeyHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.Info = json.RawMessage(info)
	n.CertNotAfter, n.CreatedAt = time.Unix(notAfter, 0).UTC(), time.Unix(created, 0).UTC()
	if seen > 0 {
		n.LastSeen = time.Unix(seen, 0).UTC()
	}
	return &n, nil
}

// NextNodeNum is the number the next node gets (never reused: job IDs
// are numbered from it, see JobIDFloor).
func (s *Store) NextNodeNum(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(num), 0) FROM nodes`).Scan(&n); err != nil {
		return 0, err
	}
	v, err := s.Setting(ctx, "cluster_max_node_num")
	if err != nil {
		return 0, err
	}
	var prev int64
	if v != "" {
		json.Unmarshal([]byte(v), &prev)
	}
	return max(n, prev) + 1, nil
}

func (s *Store) CreateNode(ctx context.Context, n *Node) error {
	now := time.Now()
	n.CreatedAt = now.UTC().Truncate(time.Second)
	if n.Status == "" {
		n.Status = NodeActive
	}
	if len(n.Info) == 0 {
		n.Info = json.RawMessage("{}")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodes (`+nodeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Num, n.Name, n.Address, n.PublicIP, n.Status, string(n.Info), n.CertNotAfter.Unix(), now.Unix(), "",
		n.KeyHash, now.Unix()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return ErrConflict
		}
		return err
	}
	// Remember the highest number ever given out, so a removed node's
	// number (and its job IDs) never comes back.
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('cluster_max_node_num', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(n.Num, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrConflict: a unique value is taken.
var ErrConflict = errors.New("conflict")

func (s *Store) GetNode(ctx context.Context, id string) (*Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
}

func (s *Store) NodeByNum(ctx context.Context, num int64) (*Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE num = ?`, num))
}

func (s *Store) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY num`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeSeen records a successful health check.
func (s *Store) NodeSeen(ctx context.Context, id string, info json.RawMessage, certNotAfter time.Time) error {
	return s.exec1(ctx, `UPDATE nodes SET info = ?, cert_not_after = ?, last_seen = ?, last_error = '' WHERE id = ?`,
		string(info), certNotAfter.Unix(), time.Now().Unix(), id)
}

// NodeFailed records a failed health check.
func (s *Store) NodeFailed(ctx context.Context, id, msg string) error {
	return s.exec1(ctx, `UPDATE nodes SET last_error = ? WHERE id = ?`, msg, id)
}

func (s *Store) UpdateNode(ctx context.Context, id, name, address, publicIP, status string) error {
	return s.exec1(ctx, `UPDATE nodes SET name = ?, address = ?, public_ip = ?, status = ? WHERE id = ?`,
		name, address, publicIP, status, id)
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	return s.exec1(ctx, `DELETE FROM nodes WHERE id = ?`, id)
}

// ---- The control plane's registry of sites on other nodes ----

// ClusterSite is a site that lives on another node. Site is the node's own
// view of it, refreshed after every change and on each health pass.
type ClusterSite struct {
	SiteID        string
	NodeID        string
	PrimaryDomain string
	Site          *Site
	UpdatedAt     time.Time
}

// PutClusterSite records (or refreshes) where a site lives and its record.
func (s *Store) PutClusterSite(ctx context.Context, nodeID string, site *Site) error {
	data, err := json.Marshal(site)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO cluster_sites (site_id, node_id, primary_domain, data, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (site_id) DO UPDATE SET node_id = excluded.node_id,
		primary_domain = excluded.primary_domain, data = excluded.data, updated_at = excluded.updated_at`,
		site.ID, nodeID, site.PrimaryDomain, string(data), time.Now().Unix())
	return err
}

func scanClusterSite(row interface{ Scan(...any) error }) (*ClusterSite, error) {
	var c ClusterSite
	var data string
	var updated int64
	if err := row.Scan(&c.SiteID, &c.NodeID, &c.PrimaryDomain, &data, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Site = &Site{}
	if err := json.Unmarshal([]byte(data), c.Site); err != nil {
		return nil, err
	}
	c.Site.Node = c.NodeID
	c.Site.normalizeLists()
	c.UpdatedAt = time.Unix(updated, 0).UTC()
	return &c, nil
}

// normalizeLists makes absent lists empty, as sites read from this store
// always have them (clients iterate them without checking).
func (st *Site) normalizeLists() {
	for _, l := range []*[]string{&st.Domains, &st.RedirectDomains, &st.AdminAllow, &st.TrustedIPs, &st.DenyIPs,
		&st.Countries, &st.ImageFormats, &st.Optimize, &st.Harden, &st.SpreadNodes, &st.LockAllow} {
		if *l == nil {
			*l = []string{}
		}
	}
	if st.Upstreams == nil {
		st.Upstreams = []int{}
	}
}

// ClusterSite is where a site lives when it isn't on this server;
// ErrNotFound for local (or unknown) sites.
func (s *Store) ClusterSite(ctx context.Context, siteID string) (*ClusterSite, error) {
	return scanClusterSite(s.db.QueryRowContext(ctx,
		`SELECT site_id, node_id, primary_domain, data, updated_at FROM cluster_sites WHERE site_id = ?`, siteID))
}

// ClusterSites lists the registry, optionally for one node ("" = all).
func (s *Store) ClusterSites(ctx context.Context, nodeID string) ([]*ClusterSite, error) {
	q := `SELECT site_id, node_id, primary_domain, data, updated_at FROM cluster_sites`
	var args []any
	if nodeID != "" {
		q += ` WHERE node_id = ?`
		args = append(args, nodeID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY site_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ClusterSite
	for rows.Next() {
		c, err := scanClusterSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteClusterSite(ctx context.Context, siteID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cluster_sites WHERE site_id = ?`, siteID)
	return err
}

// ClusterDomainTaken reports whether a domain is served (or redirected)
// by a site on another node; except is a site ID to leave out (a site
// arriving here from another server has its registry entry still).
func (s *Store) ClusterDomainTaken(ctx context.Context, domain, except string) (bool, error) {
	list, err := s.ClusterSites(ctx, "")
	if err != nil {
		return false, err
	}
	for _, c := range list {
		if c.SiteID == except {
			continue
		}
		if c.PrimaryDomain == domain {
			return true, nil
		}
		for _, d := range append(append([]string{}, c.Site.Domains...), c.Site.RedirectDomains...) {
			if d == domain {
				return true, nil
			}
		}
	}
	return false, nil
}

// ---- Spread sites (on the home node) ----

// RemoteUpstream is a replica on another node, reached through Port, a
// local tunnel listener.
type RemoteUpstream struct {
	Port       int    `json:"port"`
	Node       string `json:"node"`
	RemotePort int    `json:"remote_port"`
}

func (s *Store) siteCluster(ctx context.Context, site *Site) error {
	var err error
	if site.Upstreams, err = s.siteUpstreams(ctx, site.ID); err != nil {
		return err
	}
	var spread string
	if err := s.db.QueryRowContext(ctx, `SELECT spread_nodes FROM sites WHERE id = ?`, site.ID).Scan(&spread); err != nil {
		return err
	}
	site.SpreadNodes = splitList(spread)
	site.RemoteUpstreams, err = s.remoteUpstreams(ctx, site.ID)
	return err
}

func (s *Store) remoteUpstreams(ctx context.Context, siteID string) ([]RemoteUpstream, error) {
	q := `SELECT port, node_id, remote_port, site_id FROM site_upstreams WHERE node_id <> ''`
	var args []any
	if siteID != "" {
		q += ` AND site_id = ?`
		args = append(args, siteID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY port`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteUpstream
	for rows.Next() {
		var u RemoteUpstream
		var sid string
		if err := rows.Scan(&u.Port, &u.Node, &u.RemotePort, &sid); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// AllRemoteUpstreams lists every tunnel port of every spread site (site
// ID -> upstreams), to (re)open the tunnels at startup.
func (s *Store) AllRemoteUpstreams(ctx context.Context) (map[string][]RemoteUpstream, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port, node_id, remote_port, site_id FROM site_upstreams WHERE node_id <> '' ORDER BY port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]RemoteUpstream{}
	for rows.Next() {
		var u RemoteUpstream
		var sid string
		if err := rows.Scan(&u.Port, &u.Node, &u.RemotePort, &sid); err != nil {
			return nil, err
		}
		out[sid] = append(out[sid], u)
	}
	return out, rows.Err()
}

// SetRemoteUpstreams replaces a site's replicas on other nodes.
func (s *Store) SetRemoteUpstreams(ctx context.Context, siteID string, ups []RemoteUpstream) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM site_upstreams WHERE site_id = ? AND node_id <> ''`, siteID); err != nil {
		return err
	}
	for _, u := range ups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO site_upstreams (port, site_id, node_id, remote_port) VALUES (?, ?, ?, ?)`,
			u.Port, siteID, u.Node, u.RemotePort); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetSpreadNodes(ctx context.Context, siteID string, nodes []string) error {
	return s.exec1(ctx, `UPDATE sites SET spread_nodes = ?, updated_at = ? WHERE id = ?`,
		strings.Join(nodes, ","), time.Now().Unix(), siteID)
}

// ---- Guest replicas (on the node running them) ----

type GuestReplica struct {
	Port     int    `json:"port"`
	SiteID   string `json:"site_id"`
	HomeNode string `json:"home_node"`
}

func (s *Store) AddGuestReplica(ctx context.Context, g GuestReplica) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO guest_replicas (port, site_id, home_node, created_at) VALUES (?, ?, ?, ?)`,
		g.Port, g.SiteID, g.HomeNode, time.Now().Unix())
	return err
}

func (s *Store) DeleteGuestReplica(ctx context.Context, port int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM guest_replicas WHERE port = ?`, port)
	return err
}

// GuestReplicas lists replicas run for other nodes ("" = every site).
func (s *Store) GuestReplicas(ctx context.Context, siteID string) ([]GuestReplica, error) {
	q := `SELECT port, site_id, home_node FROM guest_replicas`
	var args []any
	if siteID != "" {
		q += ` WHERE site_id = ?`
		args = append(args, siteID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY port`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GuestReplica
	for rows.Next() {
		var g GuestReplica
		if err := rows.Scan(&g.Port, &g.SiteID, &g.HomeNode); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---- Forwards of sites that moved to another node ----

type SiteForward struct {
	SiteID  string   `json:"site_id"`
	NodeID  string   `json:"node_id"`
	Address string   `json:"address"`
	Domains []string `json:"domains"`
	// Port and HTTPPort are local tunnel listeners to the new server's
	// ingress (visitors) and port 80 (certificate challenges).
	Port      int       `json:"port"`
	HTTPPort  int       `json:"http_port"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Store) PutSiteForward(ctx context.Context, f SiteForward) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_forwards (site_id, node_id, address, domains, port, http_port, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (site_id) DO UPDATE SET node_id = excluded.node_id,
		address = excluded.address, domains = excluded.domains, port = excluded.port, http_port = excluded.http_port,
		expires_at = excluded.expires_at`,
		f.SiteID, f.NodeID, f.Address, strings.Join(f.Domains, ","), f.Port, f.HTTPPort, f.ExpiresAt.Unix(), time.Now().Unix())
	return err
}

func (s *Store) SiteForwards(ctx context.Context) ([]SiteForward, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, node_id, address, domains, port, http_port, expires_at FROM site_forwards ORDER BY site_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SiteForward
	for rows.Next() {
		var f SiteForward
		var domains string
		var exp int64
		if err := rows.Scan(&f.SiteID, &f.NodeID, &f.Address, &domains, &f.Port, &f.HTTPPort, &exp); err != nil {
			return nil, err
		}
		f.Domains, f.ExpiresAt = splitList(domains), time.Unix(exp, 0).UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSiteForward(ctx context.Context, siteID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM site_forwards WHERE site_id = ?`, siteID)
	return err
}

// ---- Job numbering ----

// JobIDFloor makes this server's job IDs start above floor, so a node's
// jobs never share an ID with the control plane's or another node's: the
// panel finds a job's server from its ID alone (see cluster.JobStride).
// Sequences are the one thing the two databases name differently, so this
// speaks each one's own SQL, bypassing the portable-query checks.
func (s *Store) JobIDFloor(ctx context.Context, floor int64) error {
	var cur int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM jobs`).Scan(&cur); err != nil {
		return err
	}
	if cur >= floor {
		return nil
	}
	if s.db.postgres {
		// The next ID handed out is floor+1.
		_, err := s.db.sql.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('jobs', 'id'), $1)`, floor)
		return err
	}
	// AUTOINCREMENT continues after the highest value it has handed out,
	// recorded in sqlite_sequence (a row appears with the first insert).
	tx, err := s.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = 'jobs' AND seq < ?`, floor, floor)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_sequence WHERE name = 'jobs'`).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES ('jobs', ?)`, floor); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
