package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Cluster supplies the data sources of a control plane with other servers:
// every site, wherever it lives, probed and certificate-checked through the
// Caddy that serves it (another node's through the cluster tunnel, with
// that node's shield health token), and every server's resources.
type Cluster struct {
	Ctrl  *cluster.Controller
	Store *store.Store
	// This server's own sources.
	LocalProbe site.Prober
	LocalCerts *TLSChecker
	LocalHosts func(ctx context.Context) ([]Host, error)

	mu     sync.Mutex
	tokens map[string]nodeToken // node -> its health token, memory only
}

type nodeToken struct {
	token string
	at    time.Time
}

// Wire sets s's data sources to c's.
func (c *Cluster) Wire(s *Service) {
	s.Sites, s.Probe, s.CertCheck, s.Hosts, s.Nodes = c.Sites, c.Probe, c.CertCheck, c.Hosts, c.NodeStates
	s.NodeMetrics = c.Metrics
}

// Metrics fetches a node's metrics (the text exposition) over the cluster
// channel.
func (c *Cluster) Metrics(ctx context.Context, node string) ([]byte, error) {
	cl := c.Ctrl.Client()
	if cl == nil {
		return nil, cluster.ErrNoCluster
	}
	ep, err := c.Ctrl.Endpoint(ctx, node)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+ep.Address+"/cluster/v1/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(ep, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &cluster.StatusError{Code: resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// Sites lists this server's sites and the registry's.
func (c *Cluster) Sites(ctx context.Context) ([]*store.Site, error) {
	sites, err := c.Store.ListSites(ctx)
	if err != nil {
		return nil, err
	}
	reg, err := c.Store.ClusterSites(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, cs := range reg {
		sites = append(sites, cs.Site)
	}
	return sites, nil
}

func remote(st *store.Site) bool { return st != nil && st.Node != "" && st.Node != cluster.LocalNode }

// dialer reaches a node's Caddy (127.0.0.1:443 there) through the tunnel.
func (c *Cluster) dialer(ctx context.Context, node string) (func(context.Context) (net.Conn, error), error) {
	cl := c.Ctrl.Client()
	if cl == nil {
		return nil, cluster.ErrNoCluster
	}
	ep, err := c.Ctrl.Endpoint(ctx, node)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (net.Conn, error) { return cl.DialTunnel(ctx, ep, cluster.TargetHTTPS, nil) }, nil
}

// token is a node's health token, refetched every few minutes (a restarted
// node has a new one).
func (c *Cluster) token(ctx context.Context, node string) (string, error) {
	c.mu.Lock()
	t, ok := c.tokens[node]
	c.mu.Unlock()
	if ok && time.Since(t.at) < 5*time.Minute {
		return t.token, nil
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := c.Ctrl.Call(ctx, node, http.MethodGet, "/cluster/v1/health-token", nil, &out); err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[string]nodeToken{}
	}
	c.tokens[node] = nodeToken{out.Token, time.Now()}
	c.mu.Unlock()
	return out.Token, nil
}

// Probe checks a site through the Caddy that serves it.
func (c *Cluster) Probe(ctx context.Context, st *store.Site) site.Health {
	if !remote(st) {
		return c.LocalProbe.Probe(ctx, st.PrimaryDomain)
	}
	dial, err := c.dialer(ctx, st.Node)
	if err != nil {
		return site.Health{Detail: "server " + st.Node + ": " + err.Error()}
	}
	tok, err := c.token(ctx, st.Node)
	if err != nil {
		// Can't tell the site from its server: the node alert covers it.
		return site.Health{Detail: "server " + st.Node + " unreachable: " + err.Error()}
	}
	return (&site.HTTPProber{Token: tok, Dial: dial}).Probe(ctx, st.PrimaryDomain)
}

// CertCheck asks the Caddy serving the domain for its certificate.
func (c *Cluster) CertCheck(ctx context.Context, t CertTarget) CertResult {
	if !remote(t.Site) {
		return c.LocalCerts.Check(ctx, t)
	}
	dial, err := c.dialer(ctx, t.Site.Node)
	if err != nil {
		return CertResult{}
	}
	return (&TLSChecker{Roots: c.LocalCerts.Roots, Dial: dial}).Check(ctx, t)
}

// Hosts is this server's resources and each node's as it last reported
// them. A node that stopped answering keeps its last figures (its own
// alert says it's unreachable) rather than having its disk alerts resolved
// as if it were gone.
func (c *Cluster) Hosts(ctx context.Context) ([]Host, error) {
	hosts, err := c.LocalHosts(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		var info cluster.NodeInfo
		if err := json.Unmarshal(n.Info, &info); err != nil || info.DiskBytes == 0 {
			continue // never reported (just added, or an older version)
		}
		hosts = append(hosts, Host{Node: n.ID,
			MemTotal: uint64(info.MemTotalMB) << 20, MemAvailable: uint64(info.MemAvailableMB) << 20,
			Disks: []Disk{{Path: "/var/lib/wpgenie", Size: info.DiskBytes, Free: info.DiskFreeBytes, Avail: info.DiskAvailBytes}}})
	}
	return hosts, nil
}

// NodeStates lists the nodes as the control plane last saw them.
func (c *Cluster) NodeStates(ctx context.Context) ([]NodeState, error) {
	if !c.Ctrl.Enabled() {
		return nil, nil
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeState, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, NodeState{ID: n.ID, Name: n.Name, LastSeen: n.LastSeen, Error: n.LastError})
	}
	return out, nil
}
