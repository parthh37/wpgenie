package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/store"
)

// A node's side of the cluster: what the control plane asks of a server
// running `wpgenie agent` besides the panel API it forwards (listing its
// sites, configuration only the panel has, migrations), which tunnels a
// peer may open here, and the facts it reports on each health check.

// ClusterHandler serves the control plane's internal requests (the agent
// only routes requests from the control plane's certificate here).
func (s *Service) ClusterHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cluster/v1/sites", func(w http.ResponseWriter, r *http.Request) {
		sites, err := s.Store.ListSites(r.Context())
		if err != nil {
			clusterError(w, err)
			return
		}
		// A site arriving or gone to another server is listed there, not here.
		out := []*store.Site{}
		for _, st := range sites {
			if st.Status != StatusImporting && st.Status != StatusMoved {
				out = append(out, st)
			}
		}
		writeClusterJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /cluster/v1/sites/{id}", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.Store.GetSite(r.Context(), r.PathValue("id"))
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, st)
	})
	// The shield's health token, for the panel's uptime and certificate
	// checks of sites here (kept in its memory only: it lets probes past
	// this server's shield).
	mux.HandleFunc("GET /cluster/v1/health-token", func(w http.ResponseWriter, r *http.Request) {
		writeClusterJSON(w, http.StatusOK, map[string]string{"token": s.HealthToken})
	})
	mux.HandleFunc("PUT /cluster/v1/peers", func(w http.ResponseWriter, r *http.Request) {
		var in cluster.Peers
		if err := decodeCluster(w, r, &in); err != nil {
			return
		}
		if err := s.SetPeers(r.Context(), in); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /cluster/v1/sites/{id}/smtp", func(w http.ResponseWriter, r *http.Request) {
		var in SMTPCreds
		if err := decodeCluster(w, r, &in); err != nil {
			return
		}
		st, err := s.ApplySMTP(r.Context(), r.PathValue("id"), in)
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("DELETE /cluster/v1/sites/{id}/sftp", func(w http.ResponseWriter, r *http.Request) {
		if s.DropSFTPAddedBy == nil {
			writeClusterJSON(w, http.StatusOK, map[string]int{"deleted": 0})
			return
		}
		n, err := s.DropSFTPAddedBy(r.Context(), r.PathValue("id"), r.URL.Query().Get("added_by"))
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, map[string]int{"deleted": n})
	})
	mux.HandleFunc("PUT /cluster/v1/repos/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in RepoRecord
		if err := decodeCluster(w, r, &in); err != nil {
			return
		}
		if in.ID != r.PathValue("id") {
			clusterError(w, fmt.Errorf("%w: repository ID mismatch", ErrInvalidInput))
			return
		}
		if err := s.PutRepoRecord(r.Context(), in); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /cluster/v1/repos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == LocalRepoID {
			clusterError(w, fmt.Errorf("%w: every server keeps its own local repository", ErrInvalidInput))
			return
		}
		if err := s.Store.DeleteRepo(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /cluster/v1/repos/{id}/adopt", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			MoveLocal bool `json:"move_local"`
		}
		if err := decodeCluster(w, r, &in); err != nil {
			return
		}
		res, err := s.AdoptRepo(r.Context(), r.PathValue("id"), in.MoveLocal)
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /cluster/v1/repos/{id}/sites", func(w http.ResponseWriter, r *http.Request) {
		policies, err := s.Store.BackupPolicies(r.Context())
		if err != nil {
			clusterError(w, err)
			return
		}
		sites := []string{}
		for _, p := range policies {
			if p.RepoID == r.PathValue("id") {
				sites = append(sites, p.SiteID)
			}
		}
		writeClusterJSON(w, http.StatusOK, sites)
	})
	mux.HandleFunc("PUT /cluster/v1/spread-grants/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Home string `json:"home"`
		}
		if decodeCluster(w, r, &in) != nil {
			return
		}
		if err := s.SetSpreadGrant(r.Context(), r.PathValue("id"), in.Home); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.migrationRoutes(mux)
	s.billingRoutes(mux)
	return mux
}

// SMTPCreds turn a site's WordPress mail on (through a mailbox the panel
// made on its mail server) or off, on a server without the mail server.
type SMTPCreds struct {
	On       bool   `json:"on"`
	Host     string `json:"host"`
	Address  string `json:"address"`
	Password string `json:"password"`
}

// ApplySMTP installs or removes the credentials the panel sends.
func (s *Service) ApplySMTP(ctx context.Context, id string, in SMTPCreds) (*store.Site, error) {
	if _, err := s.Store.GetSite(ctx, id); err != nil {
		return nil, err
	}
	if in.On && (in.Host == "" || in.Address == "" || in.Password == "") {
		return nil, fmt.Errorf("%w: host, address and password are required", ErrInvalidInput)
	}
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, fmt.Errorf("%w: an update or scan is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	if in.On {
		if err := s.installSMTP(ctx, id, in.Host, in.Address, in.Password); err != nil {
			return nil, err
		}
		s.event(id, "mail", "WordPress mail now goes through the WPGenie mail server")
	} else {
		if err := s.removeSMTP(ctx, id); err != nil {
			return nil, err
		}
		s.event(id, "mail", "WordPress mail through the WPGenie mail server turned off")
	}
	return s.Store.GetSite(ctx, id)
}

// RepoRecord is a backup destination with its secrets, as the panel sends
// it to every node: a shared repository (S3, B2, SFTP) must be opened with
// the same password and keys from every server that backs up into it.
type RepoRecord struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Location string            `json:"location"`
	Password string            `json:"password"`
	Secrets  store.RepoSecrets `json:"secrets"`
}

// RepoRecords are the shared destinations to copy to nodes (every server
// has its own "local" one).
func (s *Service) RepoRecords(ctx context.Context) ([]RepoRecord, error) {
	repos, err := s.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	var out []RepoRecord
	for _, r := range repos {
		if r.ID == LocalRepoID {
			continue
		}
		out = append(out, RepoRecord{ID: r.ID, Name: r.Name, Kind: r.Kind, Location: r.Location,
			Password: r.Password, Secrets: r.Secrets})
	}
	return out, nil
}

// PutRepoRecord stores a destination the panel sent (as it is: it was
// initialised and checked where it was added).
func (s *Service) PutRepoRecord(ctx context.Context, in RepoRecord) error {
	if in.ID == LocalRepoID || in.ID == "" || in.Kind == "" || in.Password == "" {
		return fmt.Errorf("%w: incomplete repository", ErrInvalidInput)
	}
	_, err := s.Store.GetRepo(ctx, in.ID)
	added := errors.Is(err, store.ErrNotFound)
	if err := s.Store.PutRepo(ctx, &store.BackupRepo{ID: in.ID, Name: in.Name, Kind: in.Kind, Location: in.Location,
		Password: in.Password, Secrets: in.Secrets}); err != nil {
		return err
	}
	if added && s.Backups != nil {
		// Like on the panel: this server's sites without a destination
		// take the new one when it is the one they'd default to.
		s.adoptIfPreferred(ctx, in.ID)
	}
	return nil
}

// ---- Peers ----

const settingPeers = "cluster_peers"

// SetPeers stores the directory of servers the panel sends.
func (s *Service) SetPeers(ctx context.Context, p cluster.Peers) error {
	for _, n := range p.Nodes {
		if n.ID != cluster.LocalNode && !cluster.ValidNodeID(n.ID) {
			return fmt.Errorf("%w: node ID %q", ErrInvalidInput, n.ID)
		}
		if _, _, err := net.SplitHostPort(n.Address); err != nil {
			return fmt.Errorf("%w: address %q", ErrInvalidInput, n.Address)
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, settingPeers, string(b))
}

// Peer is how to reach another server of the cluster.
func (s *Service) Peer(ctx context.Context, id string) (cluster.Endpoint, error) {
	v, err := s.Store.Setting(ctx, settingPeers)
	if err != nil {
		return cluster.Endpoint{}, err
	}
	var p cluster.Peers
	if v != "" {
		if err := json.Unmarshal([]byte(v), &p); err != nil {
			return cluster.Endpoint{}, err
		}
	}
	for _, n := range p.Nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return cluster.Endpoint{}, fmt.Errorf("%w: server %q isn't in this node's directory yet", store.ErrNotFound, id)
}

// ---- Tunnels ----

const settingIngress = "cluster_ingress"

// ingressEntry: a site that moved here, whose old server passes its
// visitors on until DNS follows.
type ingressEntry struct {
	From  string `json:"from"`
	Until int64  `json:"until"`
}

// AllowIngress lets a site's old server pass that site's visitors (with
// their addresses) to this one for a while; d <= 0 ends it. Only that site
// gets the loopback copy that believes PROXY headers, and only from that
// server: a peer can claim any client address, so the trust stays as
// narrow as the move.
func (s *Service) AllowIngress(ctx context.Context, siteID, node string, d time.Duration) error {
	if node != cluster.LocalNode && !cluster.ValidNodeID(node) {
		return fmt.Errorf("%w: node", ErrInvalidInput)
	}
	m := s.ingress(ctx)
	if d <= 0 {
		delete(m, siteID)
	} else {
		m[siteID] = ingressEntry{From: node, Until: time.Now().Add(d).Unix()}
	}
	b, _ := json.Marshal(m)
	return s.Store.SetSetting(ctx, settingIngress, string(b))
}

// ingress lists the sites forwarded here (unexpired).
func (s *Service) ingress(ctx context.Context) map[string]ingressEntry {
	m := map[string]ingressEntry{}
	if v, _ := s.Store.Setting(ctx, settingIngress); v != "" {
		json.Unmarshal([]byte(v), &m)
	}
	now := time.Now().Unix()
	for k, e := range m {
		if e.Until < now {
			delete(m, k)
		}
	}
	return m
}

// ingressFrom reports whether node passes visitors of some site here.
func (s *Service) ingressFrom(ctx context.Context, node string) bool {
	for _, e := range s.ingress(ctx) {
		if e.From == node {
			return true
		}
	}
	return false
}

// TunnelTarget decides what a cluster peer may reach on this server
// through a tunnel. Nothing is reachable by default; each target is tied
// to a relationship the control plane set up:
//
//   - fpm:<port>: a replica this node runs for the peer's site (the
//     peer's Caddy load-balances to it);
//   - mariadb, valkey: the peer runs replicas of a site that lives here
//     (they use this server's database and object cache);
//   - ingress, http: the peer is a site's old server, passing visitors on
//     until DNS moves (see AllowIngress);
//   - https: the control plane's health checks of sites here.
func (s *Service) TunnelTarget(p cluster.Peer, target string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kind, arg, _ := strings.Cut(target, ":")
	switch kind {
	case cluster.TargetFPM:
		port, err := strconv.Atoi(arg)
		if err != nil {
			return "", false
		}
		guests, err := s.Store.GuestReplicas(ctx, "")
		if err != nil {
			return "", false
		}
		for _, g := range guests {
			if g.Port == port && g.HomeNode == p.Node {
				return "127.0.0.1:" + strconv.Itoa(port), true
			}
		}
	case cluster.TargetMariaDB, cluster.TargetValkey:
		if !s.hostsSpreadFor(ctx, p.Node) {
			return "", false
		}
		if kind == cluster.TargetMariaDB {
			return s.Cfg.MariaDBLoopback(), true
		}
		addr, err := s.valkeyAddr(ctx)
		if err != nil {
			s.Log.Warn("cluster: finding Valkey for a tunnel", "err", err)
			return "", false
		}
		return addr, true
	case cluster.TargetIngress, cluster.TargetHTTP:
		if !s.ingressFrom(ctx, p.Node) {
			return "", false
		}
		if kind == cluster.TargetHTTP {
			return "127.0.0.1:80", true
		}
		return s.Cfg.IngressListen(), true
	case cluster.TargetHTTPS:
		if p.Control {
			return "127.0.0.1:443", true
		}
	}
	return "", false
}

// valkeyAddr is where this host reaches Valkey: the configured address, or
// the container's on the Docker network (never published on the host:
// Valkey has no password).
func (s *Service) valkeyAddr(ctx context.Context) (string, error) {
	if s.Cfg.ValkeyAddr != "" {
		return s.Cfg.ValkeyAddr, nil
	}
	if s.ContainerIP == nil {
		return "", errors.New("no Valkey address")
	}
	ip, err := s.ContainerIP(ctx, s.Cfg.RedisHost)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ip, "6379"), nil
}

// hostsSpreadFor reports whether node runs replicas of a site living here.
func (s *Service) hostsSpreadFor(ctx context.Context, node string) bool {
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		return false
	}
	for _, st := range sites {
		for _, u := range st.RemoteUpstreams {
			if u.Node == node {
				return true
			}
		}
		for _, n := range st.SpreadNodes {
			if n == node {
				return true
			}
		}
	}
	return false
}

// ---- Facts ----

var processStart = time.Now()

// NodeInfo reports this server for the control plane's health checks and
// placement decisions.
func (s *Service) NodeInfo(ctx context.Context, version string) cluster.NodeInfo {
	info := cluster.NodeInfo{Version: version, CPUs: goruntime.NumCPU(), Uptime: int64(time.Since(processStart).Seconds())}
	info.Hostname, _ = os.Hostname()
	info.MemTotalMB, info.MemAvailableMB = hostMemoryMB(), hostAvailableMB()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.Cfg.DataDir, &fs); err == nil {
		bs := uint64(fs.Bsize)
		info.DiskBytes, info.DiskFreeBytes, info.DiskAvailBytes = fs.Blocks*bs, fs.Bfree*bs, fs.Bavail*bs
		info.DiskTotalGB = float64(info.DiskBytes) / (1 << 30)
		info.DiskFreeGB = float64(info.DiskAvailBytes) / (1 << 30)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			info.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if sites, err := s.Store.ListSites(ctx); err == nil {
		info.Sites = len(sites)
		for _, st := range sites {
			info.CommittedMB += st.MemoryMB * max(st.Replicas, 1)
		}
	}
	return info
}

// ---- helpers ----

func writeClusterJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decodeCluster(w http.ResponseWriter, r *http.Request, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeClusterJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
		return err
	}
	return nil
}

// clusterError answers with the status the panel API would use.
func clusterError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrInvalidDomain):
		status = http.StatusBadRequest
	case errors.Is(err, ErrConflict), errors.Is(err, ErrDomainTaken), errors.Is(err, store.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, errForbidden):
		status = http.StatusForbidden
	}
	writeClusterJSON(w, status, map[string]string{"error": err.Error()})
}
