package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// Spreading a site over several servers. A site still lives on one server,
// its home: its files, database and object cache are there, its domains
// point there, and its Caddy serves static files and the page cache. Some of
// its PHP-FPM replicas run on other nodes ("guests"):
//
//   - the guest keeps a copy of the install (pushed from the home whenever
//     it changes; uploads too unless they are offloaded to object storage)
//     and the home's wp-config.php (same salts, so logins work everywhere)
//     with the database and cache hosts pointing at a link container;
//   - the link container (wpg-link-<home>, the wpgenie binary in "link"
//     mode) tunnels MariaDB and Valkey back to the home through the
//     cluster's mutual TLS: nothing is published on the network;
//   - the home's Caddy load-balances page views over all replicas, reaching
//     the guest's through local tunnel ports; anything that may write files
//     (wp-admin, sign-in, uploads, any non-GET) only goes to home replicas,
//     so the copies never diverge from the original.
//
// A guest only hosts replicas of a site the control plane granted to that
// site's home (spread grants): a node can't make another run its code.

// SpreadInput sets the other nodes a site's replicas may run on (none: all
// on its own server).
type SpreadInput struct {
	Nodes []string `json:"nodes"`
}

// spreadCounts splits total replicas over the home (index 0) and the other
// nodes, the home first: 1 replica stays home, 2 put one elsewhere, etc.
func spreadCounts(total int, nodes []string) []int {
	n := len(nodes) + 1
	out := make([]int, n)
	for i := range out {
		out[i] = total / n
		if i < total%n {
			out[i]++
		}
	}
	return out
}

// localReplicas is how many of a site's replicas run on its own server.
func localReplicas(st *store.Site) int {
	if len(st.SpreadNodes) == 0 {
		return st.Replicas
	}
	return spreadCounts(st.Replicas, st.SpreadNodes)[0]
}

// SetSpread changes the nodes a site is spread over and rebalances.
func (s *Service) SetSpread(ctx context.Context, id string, in SpreadInput) (*store.Site, error) {
	nodes := slices.Compact(slices.Sorted(slices.Values(in.Nodes)))
	for _, n := range nodes {
		if !cluster.ValidNodeID(n) && n != cluster.LocalNode {
			return nil, fmt.Errorf("%w: node %q", ErrInvalidInput, n)
		}
		if _, err := s.Peer(ctx, n); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
	}
	if s.ClusterClient == nil || (len(nodes) > 0 && s.ClusterClient() == nil) {
		return nil, fmt.Errorf("%w: this server is not part of a cluster", ErrInvalidInput)
	}
	// Every new upload would otherwise mean copying the media library to
	// every other server; with offload they only get the code.
	if len(nodes) > 0 && !s.offloaded(ctx, id) {
		return nil, fmt.Errorf("%w: turn on uploads offload first: replicas on other servers serve uploads from object storage", ErrInvalidInput)
	}
	if self, _, _ := s.selfNode(); slices.Contains(nodes, self) {
		return nil, fmt.Errorf("%w: a site's own server always runs replicas; list only other servers", ErrInvalidInput)
	}
	if slices.Contains(nodes, cluster.LocalNode) {
		// The panel holds the cluster's CA: no other server's code runs there.
		return nil, fmt.Errorf("%w: the panel's own server doesn't run other servers' replicas", ErrInvalidInput)
	}
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, fmt.Errorf("%w: an update or scan is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	s.opsMu.Lock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		s.opsMu.Unlock()
		return nil, err
	}
	if st.Status != store.StatusActive || st.ParentID != "" {
		s.opsMu.Unlock()
		return nil, fmt.Errorf("%w: only an active live site can be spread", ErrInvalidInput)
	}
	prev := st.SpreadNodes
	if err := s.Store.SetSpreadNodes(ctx, id, nodes); err != nil {
		s.opsMu.Unlock()
		return nil, err
	}
	st.SpreadNodes = nodes
	retire, err := s.reconcile(ctx, st)
	if err != nil {
		s.Store.SetSpreadNodes(context.WithoutCancel(ctx), id, prev)
		s.opsMu.Unlock()
		return nil, err
	}
	s.opsMu.Unlock()
	retire() // the other servers' replicas too (syncRemote)
	if rerr, _ := s.remoteErr.Load(id); rerr != nil && len(nodes) > 0 {
		// Some server couldn't take its replicas: put them all back here
		// rather than run the site on fewer.
		c := context.WithoutCancel(ctx)
		s.Store.SetSpreadNodes(c, id, prev)
		s.opsMu.Lock()
		if cur, err := s.Store.GetSite(c, id); err == nil {
			if again, err := s.reconcile(c, cur); err == nil {
				s.opsMu.Unlock()
				again()
			} else {
				s.opsMu.Unlock()
			}
		} else {
			s.opsMu.Unlock()
		}
		return nil, fmt.Errorf("the other servers couldn't all take replicas (the site runs on this one as before): %w", rerr.(error))
	}
	if len(nodes) == 0 {
		s.event(id, "scale", "Replicas all on this server again")
	} else {
		s.event(id, "scale", "Replicas spread over this server and "+strings.Join(nodes, ", "))
	}
	return s.Store.GetSite(ctx, id)
}

// selfNode is this server's node ID ("local" on the panel).
func (s *Service) selfNode() (string, bool, error) {
	if s.NodeID != nil {
		id, ok := s.NodeID()
		return id, ok, nil
	}
	return cluster.LocalNode, true, nil
}

// guestCall sends a request to a guest's peer API as this (home) node.
func (s *Service) guestCall(ctx context.Context, node, method, path string, body io.Reader, out any) error {
	ep, err := s.Peer(ctx, node)
	if err != nil {
		return err
	}
	cl := s.ClusterClient()
	if cl == nil {
		return errNotClustered
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+ep.Address+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.ContentLength = -1
	}
	resp, err := cl.Do(ep, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &cluster.StatusError{Code: resp.StatusCode, Msg: string(msg)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// guestSpec is what a guest needs to run replicas of a site.
type guestSpec struct {
	PHPVersion  string   `json:"php_version"`
	MemoryMB    int      `json:"memory_mb"`
	CPUs        float64  `json:"cpus"`
	MaxChildren int      `json:"max_children"`
	PHPEnv      []string `json:"php_env"`
	Domain      string   `json:"domain"`
	Count       int      `json:"count"`
	// WPConfig is the home's wp-config.php; the guest points its database
	// and cache hosts at its link container.
	WPConfig string `json:"wp_config"`
}

type guestReplicasOut struct {
	Ports []int `json:"ports"`
}

// syncRemote brings a spread site's other servers in line after a local
// reconcile, recording the outcome (SetSpread reads it; failures are site
// events).
func (s *Service) syncRemote(ctx context.Context, st *store.Site, spec runtime.SiteSpec) {
	mu, _ := s.remoteMu.LoadOrStore(st.ID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	err := s.reconcileRemote(ctx, st, spec)
	if err != nil {
		s.remoteErr.Store(st.ID, err)
		s.Log.Error("spread: replicas on other nodes", "site", st.ID, "err", err)
		s.event(st.ID, "scale", "Replicas on other servers couldn't all be updated: "+err.Error())
		return
	}
	s.remoteErr.Delete(st.ID)
}

// guestTimeout bounds each call to another server about replicas (starting
// them includes pulling or building an image); copies get longer.
const (
	guestTimeout     = 5 * time.Minute
	guestCopyTimeout = time.Hour
)

// reconcileRemote makes the replicas on other nodes match the site's
// spread (after the local replicas are right, without opsMu: it calls other
// servers). Guests start replicas first; the home's Caddy then switches to
// the new set; only then do guests stop what is no longer routed.
func (s *Service) reconcileRemote(ctx context.Context, st *store.Site, spec runtime.SiteSpec) error {
	counts := spreadCounts(st.Replicas, st.SpreadNodes)
	want := map[string]int{}
	for i, n := range st.SpreadNodes {
		want[n] = counts[i+1]
	}
	for _, u := range st.RemoteUpstreams {
		if _, ok := want[u.Node]; !ok {
			want[u.Node] = 0
		}
	}
	if len(want) == 0 {
		return nil
	}
	wpcfg, err := os.ReadFile(filepath.Join(s.Cfg.SiteDir(st.ID), "wp-config.php"))
	if err != nil {
		return err
	}
	var next []store.RemoteUpstream
	keep := map[string][]int{}
	var errs []error
	for node, n := range want {
		if n == 0 {
			continue
		}
		var out guestReplicasOut
		cctx, cancel := context.WithTimeout(ctx, guestTimeout)
		err := s.guestCall(cctx, node, http.MethodPut, "/cluster/v1/peer/sites/"+st.ID+"/replicas", jsonBody(guestSpec{
			PHPVersion: st.PHPVersion, MemoryMB: spec.MemoryMB, CPUs: spec.CPUs, MaxChildren: spec.MaxChildren,
			PHPEnv: spec.PHPEnv, Domain: st.PrimaryDomain, Count: n, WPConfig: string(wpcfg),
		}), &out)
		cancel()
		if err == nil {
			err = s.syncGuestFiles(ctx, st, node, false)
		}
		if err != nil {
			// That node keeps whatever it served before (nothing is torn down
			// on a failure); the site is still served by the others.
			errs = append(errs, fmt.Errorf("replicas on %s: %w", node, err))
			for _, u := range st.RemoteUpstreams {
				if u.Node == node {
					next = append(next, u)
					keep[node] = append(keep[node], u.RemotePort)
				}
			}
			continue
		}
		for _, p := range out.Ports {
			keep[node] = append(keep[node], p)
			i := slices.IndexFunc(st.RemoteUpstreams, func(u store.RemoteUpstream) bool { return u.Node == node && u.RemotePort == p })
			if i >= 0 {
				next = append(next, st.RemoteUpstreams[i])
				continue
			}
			next = append(next, store.RemoteUpstream{Node: node, RemotePort: p})
		}
	}
	// Local tunnel ports for the new ones.
	var need int
	for _, u := range next {
		if u.Port == 0 {
			need++
		}
	}
	// Allocating ports and recording them is one step under opsMu (no
	// network in between), like every other port allocation.
	s.opsMu.Lock()
	if need > 0 {
		ports, err := s.Store.AllocatePorts(ctx, s.Cfg.SitePortBase, need, s.heldPorts(ctx)...)
		if err != nil {
			s.opsMu.Unlock()
			return errors.Join(append(errs, err)...)
		}
		for i := range next {
			if next[i].Port == 0 {
				next[i].Port, ports = ports[0], ports[1:]
			}
		}
	}
	err = s.Store.SetRemoteUpstreams(ctx, st.ID, next)
	s.opsMu.Unlock()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if err := s.openTunnels(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.Sync(ctx); err != nil {
		return errors.Join(append(errs, err)...)
	}
	st.RemoteUpstreams = next
	// Caddy no longer routes to the rest: guests drain and stop them.
	post := context.WithoutCancel(ctx)
	for node := range want {
		q := make([]string, len(keep[node]))
		for i, p := range keep[node] {
			q[i] = strconv.Itoa(p)
		}
		path := "/cluster/v1/peer/sites/" + st.ID + "/replicas?keep=" + strings.Join(q, ",")
		if len(keep[node]) == 0 {
			path = "/cluster/v1/peer/sites/" + st.ID
		}
		cctx, cancel := context.WithTimeout(post, guestTimeout)
		if err := s.guestCall(cctx, node, http.MethodDelete, path, nil, nil); err != nil {
			s.Log.Warn("spread: retiring replicas", "site", st.ID, "node", node, "err", err)
		}
		cancel()
	}
	return errors.Join(errs...)
}

// ---- Keeping guests' copies current ----

// spreadSynced: site ID + node -> fingerprint of the install last pushed;
// pushMu: site ID + node -> *sync.Mutex.
var spreadSynced, pushMu sync.Map

// syncGuestFiles pushes the install to a guest if it changed since the last
// push (or always with force).
func (s *Service) syncGuestFiles(ctx context.Context, st *store.Site, node string, force bool) error {
	uploads := !s.offloaded(ctx, st.ID)
	fp, err := installFingerprint(s.Cfg.SiteRoot(st.ID), uploads)
	if err != nil {
		return err
	}
	key := st.ID + "|" + node
	// One copy to a node at a time (the minute's sync and a scale can meet):
	// both would use the guest's restore directory.
	mu, _ := pushMu.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if prev, ok := spreadSynced.Load(key); ok && prev == fp && !force {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, guestCopyTimeout)
	defer cancel()
	err = pipe(func(w io.Writer) error {
		return s.Runtime.Exec(ctx, st.ID, nil, w, installTar(s.Cfg.SiteRoot(st.ID), uploads)...)
	}, func(r io.Reader) error {
		q := ""
		if !uploads {
			q = "?keep_uploads=1"
		}
		return s.guestCall(ctx, node, http.MethodPut, "/cluster/v1/peer/sites/"+st.ID+"/files"+q, r, nil)
	})
	if err != nil {
		return fmt.Errorf("copying files: %w", err)
	}
	spreadSynced.Store(key, fp)
	return nil
}

// offloaded reports whether a site's uploads live in object storage (then
// guests don't need a copy of them).
func (s *Service) offloaded(ctx context.Context, id string) bool {
	on, err := s.OffloadEnabled(ctx, id)
	return err == nil && on
}

// installFingerprint summarises an install (paths, sizes, modes, times) to
// notice changes cheaply. Walked by the daemon without following symlinks
// (WalkDir never does); caches are left out, as installTar leaves them out.
func installFingerprint(docroot string, uploads bool) (string, error) {
	h := sha256.New()
	skip := map[string]bool{}
	for _, e := range copyExcludes {
		skip[e] = true
	}
	if !uploads {
		skip["wp-content/uploads"] = true
	}
	err := filepath.WalkDir(docroot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while walking
			}
			return err
		}
		rel, _ := filepath.Rel(docroot, path)
		if skip[filepath.ToSlash(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		fmt.Fprintf(h, "%s\x00%d\x00%o\x00%d\n", rel, info.Size(), info.Mode(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// RunSpread pushes changed installs to guests every minute (plugin and
// theme changes, WordPress updates, SFTP uploads: all happen at home).
func (s *Service) RunSpread(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// As a guest: what the replicas here wrote goes home, and the links
		// follow a renewed certificate.
		if guests, err := s.Store.GuestReplicas(ctx, ""); err == nil {
			seen := map[string]bool{}
			homes := map[string]bool{}
			for _, g := range guests {
				homes[g.HomeNode] = true
				if seen[g.SiteID] {
					continue
				}
				seen[g.SiteID] = true
				if err := s.pushGenerated(ctx, g); err != nil {
					s.Log.Warn("spread: sending generated files home", "site", g.SiteID, "home", g.HomeNode, "err", err)
				}
			}
			for home := range homes {
				if err := s.ensureLink(ctx, home); err != nil {
					s.Log.Warn("spread: link", "home", home, "err", err)
				}
			}
		}
		sites, err := s.Store.ListSites(ctx)
		if err != nil {
			continue
		}
		for _, st := range sites {
			if st.Status != store.StatusActive || len(st.RemoteUpstreams) == 0 {
				continue
			}
			nodes := map[string]bool{}
			for _, u := range st.RemoteUpstreams {
				nodes[u.Node] = true
			}
			for node := range nodes {
				// Not under the site's maintenance lock: an update in progress
				// may be copied half-way, and the next pass copies the rest.
				if err := s.syncGuestFiles(ctx, st, node, false); err != nil {
					s.Log.Warn("spread: updating a node's copy", "site", st.ID, "node", node, "err", err)
				}
			}
		}
	}
}

// ---- Files a guest's replicas generate ----
//
// Page views on a guest can write files: plugins that build CSS or JS on
// first view (into uploads or wp-content/cache), which the home's Caddy
// then serves, from its own disk. Each guest sends what its replicas wrote
// back to the home every minute; the home takes them only from that site's
// guests, only under uploads and cache, never PHP, symlinks, dotfiles or
// its page cache.

const (
	generatedDirs    = "wp-content/uploads wp-content/cache"
	generatedListCap = 1 << 20
)

// generatedSince: guest site ID -> unix time of the last push.
var generatedSince sync.Map

// pushGenerated sends a guest site's new files to its home.
func (s *Service) pushGenerated(ctx context.Context, g store.GuestReplica) error {
	since := time.Now().Add(-5 * time.Minute).Unix()
	if v, ok := generatedSince.Load(g.SiteID); ok {
		since = v.(int64)
	}
	started := time.Now().Add(-time.Minute).Unix() // a little overlap: nothing falls between two pushes
	root := s.Cfg.SiteRoot(g.SiteID)
	list := `set -e; cd "$1"; for d in ` + generatedDirs + `; do [ -d "$d" ] || continue
		find "$d" -path wp-content/cache/wpgenie -prune -o -type f ! -name '*.php' ! -name '.*' -exec stat -c '%Z %n' {} +
	done | awk -v t="$2" '$1 >= t { sub(/^[0-9]+ /, ""); print }'`
	var out bytes.Buffer
	if err := s.Runtime.Exec(ctx, g.SiteID, nil, &limitWriter{w: &out, n: generatedListCap}, "sh", "-c", list, "sh", root,
		strconv.FormatInt(since, 10)); err != nil {
		return err
	}
	if strings.TrimSpace(out.String()) != "" {
		files := out.String()
		err := pipe(func(w io.Writer) error {
			return s.Runtime.Exec(ctx, g.SiteID, strings.NewReader(files), w, "sh", "-c", `cd "$1" && tar -cf - -T -`, "sh", root)
		}, func(r io.Reader) error {
			cctx, cancel := context.WithTimeout(ctx, guestCopyTimeout)
			defer cancel()
			return s.guestCall(cctx, g.HomeNode, http.MethodPut, "/cluster/v1/peer/sites/"+g.SiteID+"/generated", r, nil)
		})
		if err != nil {
			return err
		}
	}
	generatedSince.Store(g.SiteID, started)
	return nil
}

// generatedScript adds files a guest sent to the install (as the site
// user): extracted aside first, then only regular files under uploads and
// cache are copied in, and only where nothing is (a guest adds what its
// replicas generated; it can't replace the home's files, nor send back
// the home's own copies stale). Busybox's cp -n skips whole directories
// that exist, hence file by file.
const generatedScript = `set -e; cd "$1"; tmp=.wpgenie-generated; rm -rf "$tmp"; mkdir "$tmp"
tar -xf - --no-same-owner -C "$tmp"
rm -rf "$tmp/wp-content/cache/wpgenie"
find "$tmp" \( -type l -o -name '*.php' -o -name '*.phtml' -o -name '*.phar' -o -name '.*' ! -path "$tmp" \) -exec rm -rf {} +
for d in ` + generatedDirs + `; do
  [ -d "$tmp/$d" ] || continue
  (cd "$tmp" && find "$d" -type f) | while IFS= read -r f; do
    [ -e "$f" ] || [ -L "$f" ] && continue
    mkdir -p "${f%/*}"
    cp "$tmp/$f" "$f"
  done
done
rm -rf "$tmp"`

// acceptGenerated takes a guest's generated files into a site living here.
func (s *Service) acceptGenerated(ctx context.Context, peer, id string, r io.Reader) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	guest := slices.Contains(st.SpreadNodes, peer)
	for _, u := range st.RemoteUpstreams {
		guest = guest || u.Node == peer
	}
	if !guest || st.Status != store.StatusActive {
		return fmt.Errorf("%w: %s runs no replicas of %s", errForbidden, peer, id)
	}
	return s.Runtime.Exec(ctx, id, r, nil, "sh", "-c", generatedScript, "sh", s.Cfg.SiteRoot(id))
}

// removeGuests takes a deleted (or moved) site's replicas off other nodes.
func (s *Service) removeGuests(ctx context.Context, st *store.Site) {
	nodes := map[string]bool{}
	for _, n := range st.SpreadNodes {
		nodes[n] = true
	}
	for _, u := range st.RemoteUpstreams {
		nodes[u.Node] = true
	}
	for node := range nodes {
		cctx, cancel := context.WithTimeout(ctx, guestTimeout)
		if err := s.guestCall(cctx, node, http.MethodDelete, "/cluster/v1/peer/sites/"+st.ID, nil, nil); err != nil {
			s.Log.Warn("spread: removing replicas from a node", "site", st.ID, "node", node, "err", err)
		}
		cancel()
	}
}

// ---- Guest side ----

const settingSpreadGrants = "cluster_spread_grants"

// SetSpreadGrant lets home run replicas of site here ("" revokes); only
// the control plane grants.
func (s *Service) SetSpreadGrant(ctx context.Context, site, home string) error {
	if !siteIDRe.MatchString(site) || (home != "" && home != cluster.LocalNode && !cluster.ValidNodeID(home)) {
		return fmt.Errorf("%w: grant", ErrInvalidInput)
	}
	m := s.spreadGrants(ctx)
	if home == "" {
		delete(m, site)
	} else {
		m[site] = home
	}
	b, _ := json.Marshal(m)
	return s.Store.SetSetting(ctx, settingSpreadGrants, string(b))
}

func (s *Service) spreadGrants(ctx context.Context) map[string]string {
	m := map[string]string{}
	if v, _ := s.Store.Setting(ctx, settingSpreadGrants); v != "" {
		json.Unmarshal([]byte(v), &m)
	}
	return m
}

// guestAllowed: home may run replicas of site here.
func (s *Service) guestAllowed(ctx context.Context, home, site string) error {
	if !siteIDRe.MatchString(site) {
		return fmt.Errorf("%w: site ID", ErrInvalidInput)
	}
	if s.spreadGrants(ctx)[site] != home {
		return fmt.Errorf("%w: the panel hasn't granted %s replicas of %s here", errForbidden, home, site)
	}
	// Never over a site that lives here: its directory is the same path.
	if _, err := s.Store.GetSite(ctx, site); err == nil {
		return fmt.Errorf("%w: %s is a site of this server", errForbidden, site)
	}
	return nil
}

var errForbidden = errors.New("forbidden")

// phpEnvRe: the WPG_* settings phpEnv produces, nothing else reaches FPM.
var phpEnvRe = regexp.MustCompile(`^WPG_(MEMORY_LIMIT|UPLOAD_MAX|POST_MAX)=[0-9]{1,5}M$|^WPG_(MAX_EXECUTION_TIME|MAX_INPUT_VARS)=[0-9]{1,6}$|^WPG_REQUEST_TIMEOUT=[0-9]{1,6}s$`)

// LinkManager runs link containers (see ensureLink).
type LinkManager interface {
	Ensure(ctx context.Context, name, home, address string) error
	Remove(ctx context.Context, name string) error
}

var (
	dbHostRe    = regexp.MustCompile(`(?m)^define\( 'DB_HOST',\s*'[^']*' \);`)
	redisHostRe = regexp.MustCompile(`(?m)^define\( 'WP_REDIS_HOST', '[^']*' \);`)
)

// LinkName is the link container through which a node's guest replicas
// reach home's database and cache.
func LinkName(home string) string { return "wpg-link-" + home }

// guestConfig points a spread site's wp-config.php at the link container.
func guestConfig(cfg, home string) ([]byte, error) {
	if !dbHostRe.MatchString(cfg) || !redisHostRe.MatchString(cfg) {
		return nil, errors.New("wp-config.php without WPGenie's DB_HOST / WP_REDIS_HOST lines")
	}
	cfg = dbHostRe.ReplaceAllLiteralString(cfg, "define( 'DB_HOST',     '"+LinkName(home)+":3306' );")
	cfg = redisHostRe.ReplaceAllLiteralString(cfg, "define( 'WP_REDIS_HOST', '"+LinkName(home)+"' );")
	return []byte(cfg), nil
}

// guestEnsure runs count replicas of home's site here with its spec, and
// reports the ports of those ready. Extra replicas keep running until
// guestTrim: the home's Caddy may still route to them.
func (s *Service) guestEnsure(ctx context.Context, home, id string, in guestSpec) ([]int, error) {
	if in.Count < 1 || in.Count > s.Cfg.MaxReplicas || !slices.Contains(s.Cfg.PHPVersions, in.PHPVersion) {
		return nil, fmt.Errorf("%w: replica spec", ErrInvalidInput)
	}
	// The spec comes from another node: checked as if a person typed it.
	if err := s.validateResources(Resources{MemoryMB: in.MemoryMB, CPUs: in.CPUs, Replicas: in.Count}); err != nil {
		return nil, err
	}
	if in.MaxChildren != runtime.FPMMaxChildren(in.MemoryMB) {
		return nil, fmt.Errorf("%w: PHP workers don't match the memory", ErrInvalidInput)
	}
	for _, e := range in.PHPEnv {
		if !phpEnvRe.MatchString(e) {
			return nil, fmt.Errorf("%w: PHP setting %q", ErrInvalidInput, e)
		}
	}
	if _, err := NormalizeDomain(in.Domain); err != nil {
		return nil, err
	}
	cfg, err := guestConfig(in.WPConfig, home)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.ensureLink(ctx, home); err != nil {
		return nil, fmt.Errorf("link to %s: %w", home, err)
	}
	if err := s.ensurePHPImage(ctx, in.PHPVersion, noProgress); err != nil {
		return nil, err
	}
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if err := layoutSite(s.Cfg.SiteDir(id), s.Cfg.SiteRoot(id), cfg); err != nil {
		return nil, err
	}
	image := s.Cfg.PHPImageFor(in.PHPVersion)
	imageID, err := s.Runtime.ImageID(ctx, image)
	if err != nil {
		return nil, err
	}
	spec := runtime.SiteSpec{ID: id, Image: image, ImageID: imageID, PHPEnv: in.PHPEnv,
		Dir: s.Cfg.SiteDir(id), Docroot: s.Cfg.SiteRoot(id), Domain: in.Domain, Network: s.Cfg.DockerNetwork,
		MemoryMB: in.MemoryMB, CPUs: in.CPUs, MaxChildren: in.MaxChildren}
	current, err := s.Runtime.Replicas(ctx, id)
	if err != nil {
		return nil, err
	}
	guests, err := s.Store.GuestReplicas(ctx, id)
	if err != nil {
		return nil, err
	}
	mine := map[int]bool{}
	for _, g := range guests {
		mine[g.Port] = true
	}
	var ready []int
	for _, r := range current {
		if r.Running && r.SpecHash == spec.Hash() && mine[r.Port] && len(ready) < in.Count {
			ready = append(ready, r.Port)
		}
	}
	ports, err := s.Store.AllocatePorts(ctx, s.Cfg.SitePortBase, in.Count-len(ready), s.heldPorts(ctx)...)
	if err != nil {
		return nil, err
	}
	var started []string
	for _, p := range ports {
		if err := s.Store.AddGuestReplica(ctx, store.GuestReplica{Port: p, SiteID: id, HomeNode: home}); err != nil {
			return nil, err
		}
		started = append(started, runtime.ContainerName(id, p))
		if err := s.Runtime.StartReplica(ctx, spec, p); err != nil {
			s.stopGuests(context.WithoutCancel(ctx), id, ports)
			return nil, err
		}
	}
	if err := s.waitReady(ctx, started, 90*time.Second); err != nil {
		s.stopGuests(context.WithoutCancel(ctx), id, ports)
		return nil, err
	}
	return slices.Sorted(slices.Values(append(ready, ports...))), nil
}

// guestFiles replaces this node's copy of the install (as the site user,
// two-phase, like restores). With keepUploads the copy's uploads stay (they
// are offloaded; the home doesn't send them).
func (s *Service) guestFiles(ctx context.Context, id string, keepUploads bool, r io.Reader) error {
	keep := keepNone
	if keepUploads {
		keep = keepCode
	}
	// The home's copy carries its managed wrappers already; this server has
	// no record of the site to rewrite them from.
	return s.replaceInstallAs(ctx, id, 0, keep, false, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	})
}

// guestTrim drains and stops this site's guest replicas not in keep.
func (s *Service) guestTrim(ctx context.Context, id string, keep []int) error {
	guests, err := s.Store.GuestReplicas(ctx, id)
	if err != nil {
		return err
	}
	var drop []int
	for _, g := range guests {
		if !slices.Contains(keep, g.Port) {
			drop = append(drop, g.Port)
		}
	}
	if len(drop) == 0 {
		return nil
	}
	current, err := s.Runtime.Replicas(ctx, id)
	if err != nil {
		return err
	}
	var old []runtime.Replica
	for _, r := range current {
		if slices.Contains(drop, r.Port) {
			old = append(old, r)
		}
	}
	s.drain(ctx, old, drainTimeout)
	s.stopGuests(ctx, id, drop)
	return nil
}

func (s *Service) stopGuests(ctx context.Context, id string, ports []int) {
	for _, p := range ports {
		if err := s.Runtime.StopReplica(ctx, runtime.ContainerName(id, p)); err != nil {
			s.Log.Warn("spread: stopping a replica", "site", id, "port", p, "err", err)
		}
		s.Store.DeleteGuestReplica(ctx, p)
	}
}

// guestRemove takes a site's replicas and copy off this node.
func (s *Service) guestRemove(ctx context.Context, home, id string) error {
	guests, err := s.Store.GuestReplicas(ctx, id)
	if err != nil {
		return err
	}
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	errs := []error{s.Runtime.RemoveSite(ctx, id)}
	for _, g := range guests {
		errs = append(errs, s.Store.DeleteGuestReplica(ctx, g.Port))
	}
	errs = append(errs, os.RemoveAll(s.Cfg.SiteDir(id)))
	// Its grant too: a new spread needs the panel again.
	errs = append(errs, s.SetSpreadGrant(ctx, id, ""))
	// The link goes when no site of that home is left here.
	all, err := s.Store.GuestReplicas(ctx, "")
	if err == nil && !slices.ContainsFunc(all, func(g store.GuestReplica) bool { return g.HomeNode == home }) {
		errs = append(errs, s.removeLink(ctx, home))
	}
	return errors.Join(errs...)
}

// ---- The link container ----

// ensureLink runs (or re-creates, when home's address changed) the link
// container for home: the wpgenie binary in "link" mode, with this node's
// cluster identity mounted read-only, on the sites' Docker network.
func (s *Service) ensureLink(ctx context.Context, home string) error {
	if s.Links == nil {
		return errors.New("links are not available on this server")
	}
	ep, err := s.Peer(ctx, home)
	if err != nil {
		return err
	}
	return s.Links.Ensure(ctx, LinkName(home), home, ep.Address)
}

func (s *Service) removeLink(ctx context.Context, home string) error {
	if s.Links == nil {
		return nil
	}
	return s.Links.Remove(ctx, LinkName(home))
}

// ---- HTTP (peer API: other nodes) ----

// PeerHandler serves what other nodes ask of this one: running replicas of
// their sites (only with the control plane's grant).
func (s *Service) PeerHandler() http.Handler {
	mux := http.NewServeMux()
	peerOf := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		p, ok := cluster.PeerFrom(r.Context())
		if !ok {
			clusterError(w, errForbidden)
			return "", false
		}
		if err := s.guestAllowed(r.Context(), p.Node, r.PathValue("id")); err != nil {
			writeClusterJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return "", false
		}
		return p.Node, true
	}
	mux.HandleFunc("PUT /cluster/v1/peer/sites/{id}/replicas", func(w http.ResponseWriter, r *http.Request) {
		home, ok := peerOf(w, r)
		if !ok {
			return
		}
		var in guestSpec
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			clusterError(w, fmt.Errorf("%w: %v", ErrInvalidInput, err))
			return
		}
		ports, err := s.guestEnsure(r.Context(), home, r.PathValue("id"), in)
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, guestReplicasOut{Ports: ports})
	})
	mux.HandleFunc("PUT /cluster/v1/peer/sites/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerOf(w, r); !ok {
			return
		}
		if err := s.guestFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("keep_uploads") == "1", r.Body); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /cluster/v1/peer/sites/{id}/replicas", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerOf(w, r); !ok {
			return
		}
		var keep []int
		for _, v := range strings.Split(r.URL.Query().Get("keep"), ",") {
			if p, err := strconv.Atoi(v); err == nil {
				keep = append(keep, p)
			}
		}
		if err := s.guestTrim(r.Context(), r.PathValue("id"), keep); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Home side: a guest's generated files (authorised by the site's spread,
	// not by a grant: this server is the site's home).
	mux.HandleFunc("PUT /cluster/v1/peer/sites/{id}/generated", func(w http.ResponseWriter, r *http.Request) {
		p, ok := cluster.PeerFrom(r.Context())
		if !ok || !siteIDRe.MatchString(r.PathValue("id")) {
			clusterError(w, errForbidden)
			return
		}
		if err := s.acceptGenerated(r.Context(), p.Node, r.PathValue("id"), http.MaxBytesReader(w, r.Body, 1<<30)); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /cluster/v1/peer/sites/{id}", func(w http.ResponseWriter, r *http.Request) {
		home, ok := peerOf(w, r)
		if !ok {
			return
		}
		if err := s.guestRemove(r.Context(), home, r.PathValue("id")); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
