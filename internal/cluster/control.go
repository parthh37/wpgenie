package cluster

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// JobStride separates servers' job IDs: node number n numbers its jobs from
// n*JobStride, so the panel finds a job's server from its ID alone and the
// dashboard needs no idea of servers. 1e12 jobs per server; IDs stay far
// below 2^53, so JavaScript numbers hold them exactly.
const JobStride = 1_000_000_000_000

// JobNodeNum is the number of the server that ran a job (0: the control
// plane itself).
func JobNodeNum(jobID int64) int64 { return jobID / JobStride }

// NodeInfo is what a server reports about itself on each health check.
type NodeInfo struct {
	Version        string  `json:"version"`
	Hostname       string  `json:"hostname"`
	CPUs           int     `json:"cpus"`
	MemTotalMB     int     `json:"mem_total_mb"`
	MemAvailableMB int     `json:"mem_available_mb"`
	DiskTotalGB    float64 `json:"disk_total_gb"`
	DiskFreeGB     float64 `json:"disk_free_gb"`
	// The data directory's filesystem in bytes, as df counts it (Free
	// includes blocks reserved for root, Avail doesn't).
	DiskBytes      uint64  `json:"disk_bytes"`
	DiskFreeBytes  uint64  `json:"disk_free_bytes"`
	DiskAvailBytes uint64  `json:"disk_avail_bytes"`
	Load1          float64 `json:"load1"`
	Sites          int     `json:"sites"`
	// CommittedMB is the memory the sites on it may use (limit x replicas).
	CommittedMB int       `json:"committed_mb"`
	Uptime      int64     `json:"uptime_s"`
	CertExpires time.Time `json:"cert_expires,omitzero"`
}

// Controller is the control plane's view of the cluster: the CA, the nodes
// and the registry of which site lives where.
type Controller struct {
	Store *store.Store
	Dir   string // /etc/wpgenie/cluster
	Log   *slog.Logger
	// Agent is the control plane's own cluster listener (node "local").
	Agent *Agent
	// StartAgent starts Agent's listener; called once the cluster exists
	// (the first node is added, or at startup when it already did).
	StartAgent func()
	// LocalInfo reports the control plane's own server (placement).
	LocalInfo func(ctx context.Context) NodeInfo
	// Configure pushes what a node needs from the control plane (peers,
	// backup destinations, server-wide security lists); called after
	// pairing and whenever a node comes back after being unreachable.
	Configure func(ctx context.Context, n *store.Node) error
	// PlaceOnControl lets new sites be placed on the control plane's own
	// server too (the default; off keeps the panel's server for the panel).
	PlaceOnControl bool

	mu       sync.Mutex
	ca       *CA
	client   *Client
	started  bool
	addMu    sync.Mutex // one pairing at a time (node numbers)
	down     sync.Map   // node ID -> true while unreachable
	createMu sync.Mutex
}

// ErrNoCluster: nothing to do until a node is added.
var ErrNoCluster = errors.New("no other servers have been added")

// Load picks up an existing cluster at startup.
func (c *Controller) Load() error {
	ca, err := LoadCA(c.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return c.adopt(ca)
}

func (c *Controller) adopt(ca *CA) error {
	cert, err := ControlIdentity(c.Dir, ca)
	if err != nil {
		return err
	}
	client, err := NewClient(ca.PEM(), cert)
	if err != nil {
		return err
	}
	if c.Agent != nil {
		if err := c.Agent.SetControlIdentity(ca.PEM(), cert); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.ca, c.client = ca, client
	start := !c.started && c.StartAgent != nil
	c.started = true
	c.mu.Unlock()
	if start {
		c.StartAgent()
	}
	return nil
}

// Enabled reports whether the cluster exists (a node was ever added).
func (c *Controller) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client != nil
}

// Client dials nodes as the control plane (nil without a cluster).
func (c *Controller) Client() *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

// Endpoint resolves a node.
func (c *Controller) Endpoint(ctx context.Context, id string) (Endpoint, error) {
	n, err := c.Store.GetNode(ctx, id)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{ID: n.ID, Address: n.Address, Pin: n.KeyHash}, nil
}

// Call sends a JSON request to a node's cluster listener.
func (c *Controller) Call(ctx context.Context, nodeID, method, path string, body, out any) error {
	cl := c.Client()
	if cl == nil {
		return ErrNoCluster
	}
	ep, err := c.Endpoint(ctx, nodeID)
	if err != nil {
		return err
	}
	return cl.Call(ctx, ep, method, path, body, out)
}

// AddNodeInput enrols a server that printed a pairing code.
type AddNodeInput struct {
	ID          string `json:"id"` // optional; derived from the name
	Name        string `json:"name"`
	Address     string `json:"address"` // host:port of `wpgenie agent`
	PublicIP    string `json:"public_ip"`
	PairingCode string `json:"pairing_code"`
}

// ErrInvalid is a bad request.
var ErrInvalid = errors.New("invalid")

// AddNode pairs a new server and records it.
func (c *Controller) AddNode(ctx context.Context, in AddNodeInput) (*store.Node, error) {
	c.addMu.Lock()
	defer c.addMu.Unlock()
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 || strings.ContainsFunc(in.Name, func(r rune) bool { return r < ' ' }) {
		return nil, fmt.Errorf("%w: name must be 1-100 characters on one line", ErrInvalid)
	}
	if in.ID == "" {
		in.ID = slug(in.Name)
	}
	if !ValidNodeID(in.ID) {
		return nil, fmt.Errorf("%w: node ID must be lowercase letters, digits and dashes (not %q)", ErrInvalid, LocalNode)
	}
	if ReservedNodeID(in.ID) {
		return nil, fmt.Errorf("%w: node ID %q is reserved (choose another name or ID)", ErrInvalid, in.ID)
	}
	addr, err := normalizeAddress(in.Address)
	if err != nil {
		return nil, err
	}
	if in.PublicIP != "" {
		if net.ParseIP(in.PublicIP) == nil {
			return nil, fmt.Errorf("%w: public_ip must be an IP address", ErrInvalid)
		}
	}
	code, err := ParsePairingCode(in.PairingCode)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := c.Store.GetNode(ctx, in.ID); err == nil {
		return nil, fmt.Errorf("%w: a node with ID %q exists (choose another name or ID)", store.ErrConflict, in.ID)
	}
	c.mu.Lock()
	ca := c.ca
	c.mu.Unlock()
	if ca == nil {
		if ca, err = LoadOrCreateCA(c.Dir); err != nil {
			return nil, err
		}
		if err := c.adopt(ca); err != nil {
			return nil, err
		}
	}
	num, err := c.Store.NextNodeNum(ctx)
	if err != nil {
		return nil, err
	}
	info, notAfter, err := Pair(ctx, ca, addr, code, in.ID, num)
	if err != nil {
		return nil, fmt.Errorf("pairing failed: %w", err)
	}
	if in.PublicIP == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && net.ParseIP(host) != nil {
			in.PublicIP = host
		}
	}
	n := &store.Node{ID: in.ID, Num: num, Name: in.Name, Address: addr, PublicIP: in.PublicIP,
		Info: info, CertNotAfter: notAfter, KeyHash: hex.EncodeToString(code.KeyHash[:])}
	if err := c.Store.CreateNode(ctx, n); err != nil {
		return nil, err
	}
	c.Log.Info("cluster: node added", "node", n.ID, "address", addr)
	if c.Configure != nil {
		if err := c.Configure(ctx, n); err != nil {
			c.Log.Warn("cluster: configuring the new node", "node", n.ID, "err", err)
		}
	}
	return c.Store.GetNode(ctx, n.ID)
}

// UpdateNodeInput changes a node's settings.
type UpdateNodeInput struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	PublicIP string `json:"public_ip"`
	Status   string `json:"status"`
}

func (c *Controller) UpdateNode(ctx context.Context, id string, in UpdateNodeInput) (*store.Node, error) {
	n, err := c.Store.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		n.Name = strings.TrimSpace(in.Name)
	}
	if in.Address != "" {
		if n.Address, err = normalizeAddress(in.Address); err != nil {
			return nil, err
		}
		if cl := c.Client(); cl != nil {
			cl.Forget(id)
		}
	}
	if in.PublicIP != "" {
		if net.ParseIP(in.PublicIP) == nil {
			return nil, fmt.Errorf("%w: public_ip must be an IP address", ErrInvalid)
		}
		n.PublicIP = in.PublicIP
	}
	switch in.Status {
	case "":
	case store.NodeActive, store.NodeDraining:
		n.Status = in.Status
	default:
		return nil, fmt.Errorf("%w: status must be active or draining", ErrInvalid)
	}
	if err := c.Store.UpdateNode(ctx, id, n.Name, n.Address, n.PublicIP, n.Status); err != nil {
		return nil, err
	}
	c.PushPeers(ctx)
	return c.Store.GetNode(ctx, id)
}

// RemoveNode forgets a node. Sites still registered on it make that an
// error unless force (they keep running there, out of the panel's reach).
func (c *Controller) RemoveNode(ctx context.Context, id string, force bool) error {
	sites, err := c.Store.ClusterSites(ctx, id)
	if err != nil {
		return err
	}
	if len(sites) > 0 && !force {
		return fmt.Errorf("%w: %d sites live on this node; move them first (or remove it anyway)", store.ErrConflict, len(sites))
	}
	for _, s := range sites {
		if err := c.Store.DeleteClusterSite(ctx, s.SiteID); err != nil {
			return err
		}
	}
	if err := c.Store.DeleteNode(ctx, id); err != nil {
		return err
	}
	if cl := c.Client(); cl != nil {
		cl.Forget(id)
	}
	c.PushPeers(ctx)
	return nil
}

// SiteNode is where a site lives: ok is false for sites on this server.
func (c *Controller) SiteNode(ctx context.Context, siteID string) (nodeID string, ok bool, err error) {
	if !c.Enabled() {
		return "", false, nil
	}
	cs, err := c.Store.ClusterSite(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return cs.NodeID, true, nil
}

// Forward proxies a panel API request to the node a site lives on.
func (c *Controller) Forward(w http.ResponseWriter, r *http.Request, nodeID string, id Identity) error {
	cl := c.Client()
	if cl == nil {
		return ErrNoCluster
	}
	ep, err := c.Endpoint(r.Context(), nodeID)
	if err != nil {
		return err
	}
	cl.ReverseProxy(ep, id).ServeHTTP(w, r)
	return nil
}

// RefreshSite reloads a remote site's record into the registry (after a
// change made through the panel); a site the node no longer has leaves it.
func (c *Controller) RefreshSite(ctx context.Context, nodeID, siteID string) error {
	var st store.Site
	err := c.Call(ctx, nodeID, http.MethodGet, "/cluster/v1/sites/"+siteID, nil, &st)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		if cs, err := c.Store.ClusterSite(ctx, siteID); err == nil && cs.NodeID == nodeID {
			return c.Store.DeleteClusterSite(ctx, siteID)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if st.ID != siteID {
		return fmt.Errorf("node %s answered for site %s with %q", nodeID, siteID, st.ID)
	}
	if why := c.refuse(ctx, nodeID, &st); why != "" {
		return fmt.Errorf("node %s: %s", nodeID, why)
	}
	return c.Store.PutClusterSite(ctx, nodeID, &st)
}

// refuse says why a node's record of a site can't enter the registry ("":
// it can). A node lists whatever it likes; it may refresh the sites it
// holds and add new ones, never take over a site (or a domain) the panel or
// another server has: the panel would then send it that site's operations
// (and their secrets). Moves switch the registry themselves (Register).
func (c *Controller) refuse(ctx context.Context, nodeID string, st *store.Site) string {
	if !siteIDRe.MatchString(st.ID) {
		return fmt.Sprintf("invalid site ID %q", st.ID)
	}
	if cs, err := c.Store.ClusterSite(ctx, st.ID); err == nil {
		if cs.NodeID == nodeID {
			return ""
		}
		return fmt.Sprintf("site %s lives on %s", st.ID, cs.NodeID)
	}
	if _, err := c.Store.GetSite(ctx, st.ID); err == nil {
		return fmt.Sprintf("site %s lives on the panel's own server", st.ID)
	}
	for _, d := range append(append([]string{st.PrimaryDomain}, st.Domains...), st.RedirectDomains...) {
		if taken, err := c.Store.DomainExists(ctx, d); err != nil || taken {
			return fmt.Sprintf("site %s claims %s, which is used on the panel's own server", st.ID, d)
		}
		if taken, err := c.Store.ClusterDomainTaken(ctx, d, st.ID); err != nil || taken {
			return fmt.Sprintf("site %s claims %s, which is used on another server", st.ID, d)
		}
	}
	return ""
}

var siteIDRe = regexp.MustCompile(`^s[a-z0-9]{7}$`)

// Register records a site created (or imported) on a node.
func (c *Controller) Register(ctx context.Context, nodeID string, st *store.Site) error {
	return c.Store.PutClusterSite(ctx, nodeID, st)
}

// CreateLock serialises site creation across the cluster so a domain is
// checked and claimed by one request at a time.
func (c *Controller) CreateLock() *sync.Mutex { return &c.createMu }

// Candidate is a server a new site could be placed on.
type Candidate struct {
	Node string
	Info NodeInfo
}

// Place picks the server for a new site: among the reachable, active
// servers with disk to spare, the one with the smallest share of its
// memory already promised to sites (container limits don't reserve
// memory, so this is what keeps a server from being overcommitted), then
// the fewest sites.
func (c *Controller) Place(ctx context.Context) (string, error) {
	var cands []Candidate
	if c.PlaceOnControl && c.LocalInfo != nil {
		cands = append(cands, Candidate{Node: LocalNode, Info: c.LocalInfo(ctx)})
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Status != store.NodeActive || time.Since(n.LastSeen) > 2*time.Minute || n.LastError != "" {
			continue
		}
		var info NodeInfo
		if json.Unmarshal(n.Info, &info) != nil {
			continue
		}
		cands = append(cands, Candidate{Node: n.ID, Info: info})
	}
	best := pick(cands)
	if best == "" {
		return "", errors.New("no server can take a new site: every server is draining, unreachable or short of disk")
	}
	return best, nil
}

func pick(cands []Candidate) string {
	cands = slices.DeleteFunc(slices.Clone(cands), func(c Candidate) bool {
		// At least 5 GB and 10% of the disk free.
		return c.Info.DiskTotalGB > 0 && (c.Info.DiskFreeGB < 5 || c.Info.DiskFreeGB < c.Info.DiskTotalGB/10)
	})
	if len(cands) == 0 {
		return ""
	}
	share := func(c Candidate) float64 {
		capacity := float64(c.Info.MemTotalMB - 1024) // MariaDB, Valkey, Caddy, the OS
		if capacity <= 0 {
			return 1e9
		}
		return float64(c.Info.CommittedMB) / capacity
	}
	slices.SortStableFunc(cands, func(a, b Candidate) int {
		if sa, sb := share(a), share(b); sa != sb {
			if sa < sb {
				return -1
			}
			return 1
		}
		return a.Info.Sites - b.Info.Sites
	})
	return cands[0].Node
}

// Peers is the directory every server gets: how to reach the others.
type Peers struct {
	Nodes []Endpoint `json:"nodes"`
}

// PushPeers sends the directory to every node (best effort: a node that
// was unreachable gets it when it comes back, through Configure).
func (c *Controller) PushPeers(ctx context.Context) {
	if c.Client() == nil {
		return
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		c.Log.Warn("cluster: listing nodes", "err", err)
		return
	}
	for _, n := range nodes {
		if err := c.pushPeers(ctx, n.ID); err != nil {
			c.Log.Warn("cluster: sending the directory", "node", n.ID, "err", err)
		}
	}
}

func (c *Controller) pushPeers(ctx context.Context, nodeID string) error {
	d, err := c.Directory(ctx)
	if err != nil {
		return err
	}
	return c.Call(ctx, nodeID, http.MethodPut, "/cluster/v1/peers", d, nil)
}

// Directory lists every server's cluster address, the control plane's
// included (as "local", when its listener address is known).
func (c *Controller) Directory(ctx context.Context) (Peers, error) {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return Peers{}, err
	}
	var d Peers
	if addr, _ := c.Store.Setting(ctx, "cluster_control_address"); addr != "" {
		d.Nodes = append(d.Nodes, Endpoint{ID: LocalNode, Address: addr})
	}
	for _, n := range nodes {
		d.Nodes = append(d.Nodes, Endpoint{ID: n.ID, Address: n.Address, Pin: n.KeyHash})
	}
	return d, nil
}

// Run checks every node every 30 seconds: reachable, its facts, its sites,
// its certificate (renewed well before it expires).
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if c.Enabled() {
			c.checkAll(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// renewControl reissues the panel's own certificate well before it expires
// (a panel that runs for a year without restarting would otherwise lose
// every node).
func (c *Controller) renewControl() {
	c.mu.Lock()
	ca, client := c.ca, c.client
	c.mu.Unlock()
	if ca == nil || c.Agent == nil || time.Until(c.Agent.CertNotAfter()) > RenewBefore {
		return
	}
	cert, err := ControlIdentity(c.Dir, ca)
	if err != nil {
		c.Log.Error("cluster: renewing the panel's certificate", "err", err)
		return
	}
	client.SetCertificate(cert)
	if err := c.Agent.SetControlIdentity(ca.PEM(), cert); err != nil {
		c.Log.Error("cluster: renewing the panel's certificate", "err", err)
		return
	}
	c.Log.Info("cluster: the panel's certificate was renewed")
}

// Refresh checks every node now (instead of waiting for the next pass).
func (c *Controller) Refresh(ctx context.Context) { c.checkAll(ctx) }

func (c *Controller) checkAll(ctx context.Context) {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		c.Log.Warn("cluster: listing nodes", "err", err)
		return
	}
	c.renewControl()
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if err := c.check(cctx, n); err != nil {
				if _, was := c.down.Swap(n.ID, true); !was {
					c.Log.Warn("cluster: node unreachable", "node", n.ID, "err", err)
				}
				c.Store.NodeFailed(ctx, n.ID, err.Error())
			}
		}()
	}
	wg.Wait()
}

func (c *Controller) check(ctx context.Context, n *store.Node) error {
	cl := c.Client()
	ep := Endpoint{ID: n.ID, Address: n.Address}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+n.Address+"/cluster/v1/info", nil)
	if err != nil {
		return err
	}
	resp, err := cl.Do(ep, req)
	if err != nil {
		return err
	}
	var info json.RawMessage
	derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || derr != nil {
		return fmt.Errorf("health check: status %d %v", resp.StatusCode, derr)
	}
	notAfter := PeerCertExpiry(resp.TLS)
	if time.Until(notAfter) < RenewBefore {
		c.mu.Lock()
		ca := c.ca
		c.mu.Unlock()
		if na, err := cl.Renew(ctx, ca, ep); err != nil {
			c.Log.Warn("cluster: renewing the node's certificate", "node", n.ID, "err", err)
		} else {
			notAfter = na
			c.Log.Info("cluster: node certificate renewed", "node", n.ID, "until", na)
		}
	}
	if err := c.Store.NodeSeen(ctx, n.ID, info, notAfter); err != nil {
		return err
	}
	if _, was := c.down.LoadAndDelete(n.ID); was || n.LastSeen.IsZero() {
		c.Log.Info("cluster: node reachable", "node", n.ID)
		if c.Configure != nil {
			if err := c.Configure(ctx, n); err != nil {
				c.Log.Warn("cluster: configuring the node", "node", n.ID, "err", err)
			}
		}
	}
	return c.syncSites(ctx, n.ID)
}

// syncSites refreshes the registry from the node's own list: records
// change on the node without the panel (autoscaling, jobs finishing).
func (c *Controller) syncSites(ctx context.Context, nodeID string) error {
	var sites []*store.Site
	if err := c.Call(ctx, nodeID, http.MethodGet, "/cluster/v1/sites", nil, &sites); err != nil {
		return fmt.Errorf("listing its sites: %w", err)
	}
	have := map[string]bool{}
	for _, st := range sites {
		if why := c.refuse(ctx, nodeID, st); why != "" {
			c.Log.Warn("cluster: ignoring a site a node listed", "node", nodeID, "why", why)
			continue
		}
		have[st.ID] = true
		if err := c.Store.PutClusterSite(ctx, nodeID, st); err != nil {
			return err
		}
	}
	reg, err := c.Store.ClusterSites(ctx, nodeID)
	if err != nil {
		return err
	}
	for _, cs := range reg {
		// A site the node doesn't have was deleted there (a create that
		// failed and rolled back, too). The grace covers a listing taken
		// just before a site was created and registered.
		if !have[cs.SiteID] && time.Since(cs.UpdatedAt) > time.Minute {
			if err := c.Store.DeleteClusterSite(ctx, cs.SiteID); err != nil {
				return err
			}
		}
	}
	return nil
}

// slug turns a name into a node ID.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if b.Len() >= 32 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// normalizeAddress checks host:port and adds the default port (7443).
func normalizeAddress(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("%w: address is required (the new server's host or IP)", ErrInvalid)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "7443"
		if strings.Count(addr, ":") > 1 { // bare IPv6
			host = strings.Trim(addr, "[]")
		}
	}
	if host == "" || strings.ContainsAny(host, "/ @") {
		return "", fmt.Errorf("%w: address must be host or host:port", ErrInvalid)
	}
	return net.JoinHostPort(host, port), nil
}
