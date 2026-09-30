package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Multi-server: the control plane keeps the registry of which site lives
// on which server and forwards everything about a site to the server it
// lives on, as the signed-in user (role and job ownership), after checking
// access here. Lists (sites, jobs, bans, security events) merge every
// server's; server-wide settings (security lists, bans, backup
// destinations) are applied everywhere.

// clusterRoutes registers the nodes API (control plane only).
func (s *Server) clusterRoutes(r func(pattern, role string, h handlerFunc)) {
	r("GET /api/v1/nodes", viewer, s.listNodes)
	r("POST /api/v1/nodes", admin, s.addNode)
	r("PUT /api/v1/nodes/{node}", admin, s.updateNode)
	r("DELETE /api/v1/nodes/{node}", admin, s.removeNode)
	r("POST /api/v1/nodes/{node}/update", admin, s.updateNodeSoftware)
	r("POST /api/v1/nodes/{node}/drain", admin, s.drainNode)
	r("POST /api/v1/sites/{id}/migrate", admin, s.migrateSite)
	r("GET /api/v1/sites/{id}/move", viewer, s.siteMove)
	r("POST /api/v1/sites/{id}/move/finish", admin, s.finishMove)
}

// notForwarded are site routes the control plane handles itself even for
// sites on other servers.
var notForwarded = map[string]bool{
	"POST /api/v1/sites/{id}/migrate":     true,
	"GET /api/v1/sites/{id}/move":         true,
	"POST /api/v1/sites/{id}/move/finish": true,
	"PUT /api/v1/sites/{id}/spread":       true,
	"POST /api/v1/sites/{id}/domains":     true,
	"GET /api/v1/sites/{id}/dns-check":    true,
	// Plan checks for tenants run here first (then forwardAfterChecks).
	"PUT /api/v1/sites/{id}/autoscale":      true,
	"PUT /api/v1/sites/{id}/resources":      true,
	"PUT /api/v1/sites/{id}/backups/policy": true,
	"POST /api/v1/sites/{id}/staging":       true,
	"POST /api/v1/sites/{id}/push":          true,
	"PUT /api/v1/sites/{id}/smtp":           true,
	"DELETE /api/v1/sites/{id}":             true,
}

// clustered sends requests about a site on another server there.
func (s *Server) clustered(pattern string, h handlerFunc) handlerFunc {
	if s.Cluster == nil || !strings.Contains(pattern, "/api/v1/sites/{id}") || notForwarded[pattern] {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) error {
		node, remote, err := s.Cluster.SiteNode(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		if !remote {
			return h(w, r)
		}
		return s.forwardSite(w, r, node, r.PathValue("id"))
	}
}

// siteRecord is a site wherever it lives: the registry's copy for one on
// another server (first: a site that moved away still has a record here),
// else this server's.
func (s *Server) siteRecord(ctx context.Context, id string) (*store.Site, error) {
	if s.Cluster != nil {
		if cs, err := s.Store.ClusterSite(ctx, id); err == nil {
			return cs.Site, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	return s.Store.GetSite(ctx, id)
}

// forwardAfterChecks sends a request about a site on another server there,
// once the handler has checked it here (plan limits need the panel's
// accounts); in is the body it decoded. False: the site is local.
func (s *Server) forwardAfterChecks(w http.ResponseWriter, r *http.Request, in any) (bool, error) {
	if s.Cluster == nil {
		return false, nil
	}
	node, remote, err := s.Cluster.SiteNode(r.Context(), r.PathValue("id"))
	if err != nil || !remote {
		return false, err
	}
	rebody(r, in)
	return true, s.forwardSite(w, r, node, r.PathValue("id"))
}

// rebody replaces a request's body (already read by decode) with v, so
// the request can still be forwarded.
func rebody(r *http.Request, v any) {
	b, _ := json.Marshal(v)
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", strconv.Itoa(len(b)))
}

// identity is who a forwarded request acts as.
// A tenant's request reaches a node as an operator's: the panel checked
// ownership and the plan (nodes know nothing of accounts), and the node
// applies what an operator may do on a site.
func (s *Server) identity(r *http.Request) cluster.Identity {
	p := principalFrom(r.Context())
	role := p.Role
	if !auth.ValidRole(role) || tenantOf(r) != nil {
		role = auth.RoleOperator
	}
	return cluster.Identity{Name: p.Name, Role: role, Owner: p.owner(), IP: clientIP(r)}
}

func (s *Server) forwardSite(w http.ResponseWriter, r *http.Request, node, siteID string) error {
	// The node bounds its own work; a backup download through here may take
	// hours, which the panel's write timeout would cut.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(12 * time.Hour))
	rec := &statusCapture{ResponseWriter: w}
	if err := s.Cluster.Forward(rec, r, node, s.identity(r)); err != nil {
		return err
	}
	// Keep the registry's copy (listings, access checks) current; file
	// manager changes don't touch the site's record.
	fileOp := strings.HasPrefix(r.URL.Path, "/api/v1/sites/"+siteID+"/files")
	if r.Method != http.MethodGet && r.Method != http.MethodHead && rec.status/100 == 2 && !fileOp {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
		defer cancel()
		if err := s.Cluster.RefreshSite(ctx, node, siteID); err != nil {
			s.Log.Warn("cluster: refreshing a site after a change", "site", siteID, "node", node, "err", err)
		}
	}
	return nil
}

type statusCapture struct {
	http.ResponseWriter
	status int
}

func (c *statusCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *statusCapture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.ResponseWriter.Write(b)
}

func (c *statusCapture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *statusCapture) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// nodeAPI calls a node's panel API as the given identity.
func (s *Server) nodeAPI(ctx context.Context, node string, id cluster.Identity, method, path string, body, out any) error {
	cl := s.Cluster.Client()
	if cl == nil {
		return cluster.ErrNoCluster
	}
	ep, err := s.Cluster.Endpoint(ctx, node)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+ep.Address+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cluster.HeaderActor, id.Name)
	req.Header.Set(cluster.HeaderRole, id.Role)
	req.Header.Set(cluster.HeaderOwner, id.Owner)
	req.Header.Set(cluster.HeaderIP, id.IP)
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
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// eachNode runs fn for every node in parallel (with a deadline so one
// unreachable server doesn't stall a listing) and reports failures.
func (s *Server) eachNode(ctx context.Context, timeout time.Duration, fn func(ctx context.Context, n *store.Node) error) map[string]error {
	if s.Cluster == nil || !s.Cluster.Enabled() {
		return nil
	}
	nodes, err := s.Store.ListNodes(ctx)
	if err != nil {
		return map[string]error{"": err}
	}
	var mu sync.Mutex
	errs := map[string]error{}
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := fn(c, n); err != nil {
				mu.Lock()
				errs[n.ID] = err
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errs
}

// broadcast applies a server-wide change on every node as the panel and
// reports on how many it took effect (a 404 is "nothing to change there").
func (s *Server) broadcast(r *http.Request, method, path string, body any) (applied int) {
	id := s.identity(r)
	id.Role = auth.RoleAdmin // the panel already authorised the change
	var mu sync.Mutex
	errs := s.eachNode(context.WithoutCancel(r.Context()), 20*time.Second, func(ctx context.Context, n *store.Node) error {
		err := s.nodeAPI(ctx, n.ID, id, method, path, body, nil)
		var se *cluster.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil
		}
		if err == nil {
			mu.Lock()
			applied++
			mu.Unlock()
		}
		return err
	})
	for node, err := range errs {
		s.Log.Warn("cluster: applying a change on a node", "node", node, "path", path, "err", err)
	}
	return applied
}

// panelOnly are routes a node doesn't serve: people sign in, manage
// accounts and the mail server on the panel.
func panelOnly(pattern string) bool {
	path := pattern[strings.IndexByte(pattern, ' ')+1:]
	for _, p := range []string{"/api/v1/account", "/api/v1/users", "/api/v1/sessions", "/api/v1/settings/auth",
		"/api/v1/mail", "/api/v1/nodes"} {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return pattern == "PUT /api/v1/sites/{id}/smtp"
}

// setRemoteSMTP turns a remote site's WordPress mail on or off: the mail
// server runs next to the panel, so the sender mailbox is made here and
// only its credentials go to the site's server.
func (s *Server) setRemoteSMTP(w http.ResponseWriter, r *http.Request, node string, on bool) error {
	cs, err := s.Store.ClusterSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if s.Mail == nil || (on && s.Mail.SMTPHost() == "") {
		return fmt.Errorf("%w: enable mail (a mail hostname) first", site.ErrInvalidInput)
	}
	in := site.SMTPCreds{On: on}
	if on {
		addr, pw, err := s.Mail.EnsureSender(r.Context(), cs.SiteID, cs.Site.PrimaryDomain)
		if err != nil {
			return err
		}
		in.Host, in.Address, in.Password = s.Mail.SMTPHost(), addr, pw
	}
	var st store.Site
	if err := s.Cluster.Call(r.Context(), node, http.MethodPut, "/cluster/v1/sites/"+cs.SiteID+"/smtp", in, &st); err != nil {
		if on && !cs.Site.SMTP {
			c, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
			defer cancel()
			s.Mail.RemoveSender(c, cs.SiteID, cs.Site.PrimaryDomain)
		}
		return clusterErr(err)
	}
	if !on {
		if err := s.Mail.RemoveSender(r.Context(), cs.SiteID, cs.Site.PrimaryDomain); err != nil {
			return err
		}
	}
	if err := s.Cluster.Register(r.Context(), node, &st); err != nil {
		return err
	}
	st.Node = node
	return writeJSON(w, http.StatusOK, &st)
}

// ConfigureNode sends a node what it needs from the panel: the directory
// of servers, the shared backup destinations and the server-wide security
// lists. Called after pairing and when a node comes back.
func (s *Server) ConfigureNode(ctx context.Context, n *store.Node) error {
	s.Cluster.PushPeers(ctx)
	var errs []error
	repos, err := s.Sites.RepoRecords(ctx)
	if err != nil {
		return err
	}
	for _, rr := range repos {
		if err := s.Cluster.Call(ctx, n.ID, http.MethodPut, "/cluster/v1/repos/"+rr.ID, rr, nil); err != nil {
			errs = append(errs, fmt.Errorf("backup destination %s: %w", rr.Name, err))
		}
	}
	g, err := s.Sites.GlobalLists(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	panel := cluster.Identity{Name: "panel", Role: auth.RoleAdmin, Owner: "api-token"}
	if err := s.nodeAPI(ctx, n.ID, panel, http.MethodPut, "/api/v1/security/settings", g, nil); err != nil {
		errs = append(errs, fmt.Errorf("security lists: %w", err))
	}
	if b, err := s.Sites.Branding(ctx); err != nil {
		errs = append(errs, fmt.Errorf("branding: %w", err))
	} else if err := s.nodeAPI(ctx, n.ID, panel, http.MethodPut, "/api/v1/settings/branding", b.Input(), nil); err != nil {
		errs = append(errs, fmt.Errorf("branding: %w", err))
	}
	return errors.Join(errs...)
}

// pushRepos sends the shared backup destinations to every node.
func (s *Server) pushRepos(ctx context.Context) {
	repos, err := s.Sites.RepoRecords(ctx)
	if err != nil {
		s.Log.Warn("cluster: listing backup destinations", "err", err)
		return
	}
	errs := s.eachNode(context.WithoutCancel(ctx), 20*time.Second, func(ctx context.Context, n *store.Node) error {
		for _, rr := range repos {
			if err := s.Cluster.Call(ctx, n.ID, http.MethodPut, "/cluster/v1/repos/"+rr.ID, rr, nil); err != nil {
				return err
			}
		}
		return nil
	})
	for node, err := range errs {
		s.Log.Warn("cluster: sending backup destinations", "node", node, "err", err)
	}
}

// repoUnusedElsewhere refuses to delete a destination sites on other
// servers still back up to (every server must agree before any forgets it).
func (s *Server) repoUnusedElsewhere(ctx context.Context, repoID string) error {
	var mu sync.Mutex
	var using []string
	errs := s.eachNode(ctx, 10*time.Second, func(ctx context.Context, n *store.Node) error {
		var sites []string
		if err := s.Cluster.Call(ctx, n.ID, http.MethodGet, "/cluster/v1/repos/"+repoID+"/sites", nil, &sites); err != nil {
			return err
		}
		mu.Lock()
		for _, id := range sites {
			using = append(using, id+" (on "+n.ID+")")
		}
		mu.Unlock()
		return nil
	})
	for node, err := range errs {
		return errors.Join(errConflict, fmt.Errorf("can't check server %s: %w", node, err))
	}
	if len(using) > 0 {
		return errors.Join(errConflict, fmt.Errorf("sites back up to it: %s", strings.Join(using, ", ")))
	}
	return nil
}

// ---- Nodes ----

type nodeView struct {
	*store.Node
	Local bool `json:"local"`
	Sites int  `json:"sites"`
	Up    bool `json:"up"`
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) error {
	out := []nodeView{}
	if s.Cluster != nil && s.Cluster.LocalInfo != nil {
		info, _ := json.Marshal(s.Cluster.LocalInfo(r.Context()))
		local, _ := s.Store.ListSites(r.Context())
		out = append(out, nodeView{Node: &store.Node{ID: cluster.LocalNode, Name: "This server (panel)", Status: store.NodeActive,
			Info: info, LastSeen: s.now().UTC()}, Local: true, Sites: len(local), Up: true})
	}
	nodes, err := s.Store.ListNodes(r.Context())
	if err != nil {
		return err
	}
	reg, err := s.Store.ClusterSites(r.Context(), "")
	if err != nil {
		return err
	}
	count := map[string]int{}
	for _, c := range reg {
		count[c.NodeID]++
	}
	for _, n := range nodes {
		up := n.LastError == "" && time.Since(n.LastSeen) < 2*time.Minute
		out = append(out, nodeView{Node: n, Sites: count[n.ID], Up: up})
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) addNode(w http.ResponseWriter, r *http.Request) error {
	if s.Cluster == nil {
		return errors.Join(errBadRequest, errors.New("this server is a node; add servers on the panel"))
	}
	var in cluster.AddNodeInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	n, err := s.Cluster.AddNode(r.Context(), in)
	if err != nil {
		return clusterErr(err)
	}
	return writeJSON(w, http.StatusCreated, n)
}

func (s *Server) updateNode(w http.ResponseWriter, r *http.Request) error {
	var in cluster.UpdateNodeInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	n, err := s.Cluster.UpdateNode(r.Context(), r.PathValue("node"), in)
	if err != nil {
		return clusterErr(err)
	}
	return writeJSON(w, http.StatusOK, n)
}

func (s *Server) removeNode(w http.ResponseWriter, r *http.Request) error {
	if err := s.Cluster.RemoveNode(r.Context(), r.PathValue("node"), r.URL.Query().Get("force") == "1"); err != nil {
		return clusterErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// updateNodeSoftware asks a node to update WPGenie to the release the
// panel runs (it downloads and verifies the signed release itself).
func (s *Server) updateNodeSoftware(w http.ResponseWriter, r *http.Request) error {
	id := s.identity(r)
	var out json.RawMessage
	if err := s.nodeAPI(r.Context(), r.PathValue("node"), id, http.MethodPost, "/api/v1/system/update", nil, &out); err != nil {
		return clusterErr(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, err := w.Write(out)
	return err
}

// migrateSite moves a site to another server (a job).
func (s *Server) migrateSite(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Node string `json:"node"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := s.Sites.StartMigration(r.Context(), r.PathValue("id"), in.Node)
	if err != nil {
		return clusterErr(err)
	}
	return writeJSON(w, http.StatusAccepted, map[string]int64{"job_id": id})
}

// setSpread runs a site's replicas on other servers too. The panel is what
// authorises it: each of those servers gets a grant for the site's home
// first (a node can't make another run its code), then the home rebalances.
func (s *Server) setSpread(w http.ResponseWriter, r *http.Request) error {
	var in site.SpreadInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id := r.PathValue("id")
	if s.Cluster == nil {
		// A node: the panel checked and granted; rebalance here.
		st, err := s.Sites.SetSpread(r.Context(), id, in)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, st)
	}
	home, remote, err := s.Cluster.SiteNode(r.Context(), id)
	if err != nil {
		return err
	}
	if !remote {
		home = cluster.LocalNode
		if _, err := s.Store.GetSite(r.Context(), id); err != nil {
			return err
		}
	}
	for _, n := range in.Nodes {
		if n == home {
			return errors.Join(errBadRequest, errors.New("list only other servers: the site's own always runs replicas"))
		}
		if n == cluster.LocalNode {
			continue
		}
		node, err := s.Store.GetNode(r.Context(), n)
		if err != nil {
			return errors.Join(errBadRequest, fmt.Errorf("server %q: %w", n, err))
		}
		if node.Status != store.NodeActive {
			return errors.Join(errConflict, fmt.Errorf("server %s is %s", n, node.Status))
		}
	}
	grant := func(ctx context.Context, node, to string) error {
		if node == cluster.LocalNode {
			return s.Sites.SetSpreadGrant(ctx, id, to)
		}
		return s.Cluster.Call(ctx, node, http.MethodPut, "/cluster/v1/spread-grants/"+id, map[string]string{"home": to}, nil)
	}
	var prev []string
	if remote {
		if cs, err := s.Store.ClusterSite(r.Context(), id); err == nil {
			prev = cs.Site.SpreadNodes
		}
	} else if cur, err := s.Store.GetSite(r.Context(), id); err == nil {
		prev = cur.SpreadNodes
	}
	for _, n := range in.Nodes {
		if err := grant(r.Context(), n, home); err != nil {
			return clusterErr(fmt.Errorf("authorising %s: %w", n, err))
		}
	}
	// A refused change takes back the grants it added.
	undo := func() {
		for _, n := range in.Nodes {
			if !slices.Contains(prev, n) {
				grant(context.WithoutCancel(r.Context()), n, "")
			}
		}
	}
	var st *store.Site
	if remote {
		rebody(r, in) // decoded above
		rec := &statusCapture{ResponseWriter: w}
		if err := s.forwardSite(rec, r, home, id); err != nil {
			undo()
			return err
		}
		if rec.status/100 != 2 {
			undo()
			return nil
		}
	} else if st, err = s.Sites.SetSpread(r.Context(), id, in); err != nil {
		undo()
		return err
	}
	// The previous spread's other servers lose their grant now that the
	// home has taken its replicas off them.
	for _, n := range prev {
		if !slices.Contains(in.Nodes, n) {
			if err := grant(context.WithoutCancel(r.Context()), n, ""); err != nil {
				s.Log.Warn("cluster: revoking a spread grant", "site", id, "node", n, "err", err)
			}
		}
	}
	if st != nil {
		return writeJSON(w, http.StatusOK, st)
	}
	return nil
}

func (s *Server) siteMove(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Sites.LastMove(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"move": m})
}

// finishMove deletes a moved site's old copy now (DNS already moved).
func (s *Server) finishMove(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.FinishMove(r.Context(), r.PathValue("id")); err != nil {
		return clusterErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// drainNode moves every site off a node (a job).
func (s *Server) drainNode(w http.ResponseWriter, r *http.Request) error {
	id, err := s.Sites.StartDrain(r.Context(), r.PathValue("node"))
	if err != nil {
		return clusterErr(err)
	}
	return writeJSON(w, http.StatusAccepted, map[string]int64{"job_id": id})
}

// clusterErr maps cluster errors onto API statuses.
func clusterErr(err error) error {
	var se *cluster.StatusError
	switch {
	case errors.As(err, &se) && se.Code == http.StatusNotFound:
		return errors.Join(store.ErrNotFound, err)
	case errors.As(err, &se) && se.Code == http.StatusConflict:
		return errors.Join(errConflict, err)
	case errors.As(err, &se) && se.Code/100 == 4:
		return errors.Join(errBadRequest, err)
	}
	return err
}

// ---- Merged listings ----

// clusterSites is every site on other servers, from the registry.
func (s *Server) clusterSites(ctx context.Context) ([]*store.Site, error) {
	if s.Cluster == nil {
		return nil, nil
	}
	reg, err := s.Store.ClusterSites(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]*store.Site, 0, len(reg))
	for _, c := range reg {
		out = append(out, c.Site)
	}
	return out, nil
}

// createSiteInput is a new site, optionally on a chosen server.
type createSiteInput struct {
	site.CreateInput
	Node string `json:"node"`
}

// createOnNode creates a site on another server (the chosen one, or the
// one placement picks); remote is false when it belongs on this server.
// Only staff choose servers: a tenant's sites go where placement puts them.
func (s *Server) createOnNode(r *http.Request, in createSiteInput) (st *store.Site, jobID int64, remote bool, err error) {
	if in.Node != "" && tenantOf(r) != nil {
		return nil, 0, false, errors.Join(errForbidden, errors.New("the server a site is created on is chosen by the hosting provider"))
	}
	if s.Cluster == nil || !s.Cluster.Enabled() {
		if in.Node != "" && in.Node != cluster.LocalNode {
			return nil, 0, false, errors.Join(errBadRequest, errors.New("no other servers have been added"))
		}
		return nil, 0, false, nil
	}
	node := in.Node
	if node == "" {
		if node, err = s.Cluster.Place(r.Context()); err != nil {
			return nil, 0, false, errors.Join(errConflict, err)
		}
	}
	if node == cluster.LocalNode {
		return nil, 0, false, nil
	}
	n, err := s.Store.GetNode(r.Context(), node)
	if err != nil {
		return nil, 0, false, err
	}
	if n.Status != store.NodeActive {
		return nil, 0, false, errors.Join(errConflict, fmt.Errorf("node %s is %s", n.ID, n.Status))
	}
	domain, err := site.NormalizeDomain(in.Domain)
	if err != nil {
		return nil, 0, false, err
	}
	mu := s.Cluster.CreateLock()
	mu.Lock()
	defer mu.Unlock()
	if err := s.Sites.DomainFree(r.Context(), domain); err != nil {
		return nil, 0, false, err
	}
	var out struct {
		Site  *store.Site `json:"site"`
		JobID int64       `json:"job_id"`
	}
	if err := s.nodeAPI(r.Context(), node, s.identity(r), http.MethodPost, "/api/v1/sites", in.CreateInput, &out); err != nil {
		return nil, 0, false, clusterErr(err)
	}
	if out.Site == nil {
		return nil, 0, false, errors.New("the node didn't return the new site")
	}
	// The node's own record of it (the answer is the job's starting point).
	if err := s.Cluster.RefreshSite(r.Context(), node, out.Site.ID); err != nil {
		return nil, 0, false, err
	}
	if cs, err := s.Store.ClusterSite(r.Context(), out.Site.ID); err == nil {
		out.Site = cs.Site
	}
	return out.Site, out.JobID, true, nil
}

// domainTaken checks every server: this one's sites, the panel and mail
// hostnames (the site service checks those too), and the registry.
func (s *Server) domainTaken(ctx context.Context, domain string) (bool, error) {
	if taken, err := s.Store.DomainExists(ctx, domain); err != nil || taken {
		return taken, err
	}
	return s.Store.ClusterDomainTaken(ctx, domain, "")
}

// deleteRemoteSite deletes a site on another server, then forgets it.
func (s *Server) deleteRemoteSite(w http.ResponseWriter, r *http.Request, node string) error {
	cs, err := s.Store.ClusterSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	// As for local sites: a live site needs an admin, or its tenant (whose
	// right to delete the route wrapper checked).
	tenant := tenantOf(r) != nil
	if cs.Site.ParentID == "" && !tenant && auth.Level(principalFrom(r.Context()).Role) < auth.Level(admin) {
		return errForbidden
	}
	var acctID int64
	if o, err := s.Store.SiteOwnerOf(r.Context(), cs.SiteID); err == nil {
		acctID = o.AccountID
	}
	id := s.identity(r)
	if tenant {
		id.Role = auth.RoleAdmin // decided here; the node checks roles, not plans
	}
	rec := &statusCapture{ResponseWriter: w}
	if err := s.Cluster.Forward(rec, r, node, id); err != nil {
		return err
	}
	if rec.status/100 == 2 || rec.status == http.StatusNotFound {
		ctx := context.WithoutCancel(r.Context())
		if err := s.Store.DeleteClusterSite(ctx, cs.SiteID); err != nil {
			return err
		}
		if err := s.Store.UnassignSite(ctx, cs.SiteID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.Log.Warn("forgetting a deleted site's account", "site", cs.SiteID, "err", err)
		}
		// The servers it was spread to lose their grant for it.
		for _, n := range cs.Site.SpreadNodes {
			if err := s.Cluster.Call(ctx, n, http.MethodPut, "/cluster/v1/spread-grants/"+cs.SiteID, map[string]string{"home": ""}, nil); err != nil {
				s.Log.Warn("revoking a deleted site's spread grant", "site", cs.SiteID, "node", n, "err", err)
			}
		}
		if cs.Site.SMTP && s.Mail != nil {
			if err := s.Mail.RemoveSender(ctx, cs.SiteID, cs.Site.PrimaryDomain); err != nil {
				s.Log.Warn("removing a deleted site's mail sender", "site", cs.SiteID, "err", err)
			}
		}
		if s.Billing != nil && s.Billing.Hooks != nil {
			s.Billing.Hooks.Emit(ctx, billing.EventSiteDeleted, map[string]any{"site_id": cs.SiteID,
				"account_id": acctID, "domain": cs.PrimaryDomain})
		}
	}
	return nil
}

// createStagingOnNode clones a site on another server there (a staging
// copy lives next to its live site). False: the live site is here.
func (s *Server) createStagingOnNode(r *http.Request, in site.StagingInput) (*store.Site, int64, bool, error) {
	if s.Cluster == nil {
		return nil, 0, false, nil
	}
	node, remote, err := s.Cluster.SiteNode(r.Context(), r.PathValue("id"))
	if err != nil || !remote {
		return nil, 0, false, err
	}
	if in.Domain != "" {
		mu := s.Cluster.CreateLock()
		mu.Lock()
		defer mu.Unlock()
		if err := s.Sites.DomainFree(r.Context(), in.Domain); err != nil {
			return nil, 0, false, err
		}
	}
	var out struct {
		Site  *store.Site `json:"site"`
		JobID int64       `json:"job_id"`
	}
	if err := s.nodeAPI(r.Context(), node, s.identity(r), http.MethodPost,
		"/api/v1/sites/"+r.PathValue("id")+"/staging", in, &out); err != nil {
		return nil, 0, false, clusterErr(err)
	}
	if out.Site == nil {
		return nil, 0, false, errors.New("the node didn't return the staging site")
	}
	if err := s.Cluster.RefreshSite(r.Context(), node, out.Site.ID); err != nil {
		return nil, 0, false, err
	}
	if cs, err := s.Store.ClusterSite(r.Context(), out.Site.ID); err == nil {
		out.Site = cs.Site
	}
	return out.Site, out.JobID, true, nil
}

// remoteJob forwards job requests for jobs that ran on another server
// (their IDs say which, see cluster.JobStride).
func (s *Server) remoteJob(w http.ResponseWriter, r *http.Request) (bool, error) {
	if s.Cluster == nil {
		return false, nil
	}
	id, err := jobID(r)
	if err != nil {
		return false, err
	}
	num := cluster.JobNodeNum(id)
	if num == 0 {
		return false, nil
	}
	n, err := s.Store.NodeByNum(r.Context(), num)
	if err != nil {
		return true, err
	}
	return true, s.Cluster.Forward(w, r, n.ID, s.identity(r))
}

// clusterJobs adds other servers' jobs to a listing.
func (s *Server) clusterJobs(r *http.Request, local []store.Job, limit int) []store.Job {
	if s.Cluster == nil || !s.Cluster.Enabled() {
		return local
	}
	var mu sync.Mutex
	all := slices.Clone(local)
	id := s.identity(r)
	s.eachNode(r.Context(), 5*time.Second, func(ctx context.Context, n *store.Node) error {
		var list []store.Job
		if err := s.nodeAPI(ctx, n.ID, id, http.MethodGet, "/api/v1/jobs?"+r.URL.RawQuery, nil, &list); err != nil {
			return err
		}
		mu.Lock()
		all = append(all, list...)
		mu.Unlock()
		return nil
	})
	slices.SortStableFunc(all, func(a, b store.Job) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// clusterBans adds bans in force on other servers (a ban is per server:
// the shield that saw the attack keeps it in memory).
func (s *Server) clusterBans(r *http.Request, local []shield.Ban) []shield.Ban {
	if s.Cluster == nil || !s.Cluster.Enabled() {
		return local
	}
	var mu sync.Mutex
	byAddr := map[string]shield.Ban{}
	for _, b := range local {
		byAddr[b.Addr] = b
	}
	id := s.identity(r)
	s.eachNode(r.Context(), 5*time.Second, func(ctx context.Context, n *store.Node) error {
		var list []shield.Ban
		if err := s.nodeAPI(ctx, n.ID, id, http.MethodGet, "/api/v1/security/bans", nil, &list); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, b := range list {
			if cur, ok := byAddr[b.Addr]; !ok || b.Until.After(cur.Until) {
				byAddr[b.Addr] = b
			}
		}
		return nil
	})
	out := make([]shield.Ban, 0, len(byAddr))
	for _, b := range byAddr {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b shield.Ban) int { return b.Until.Compare(a.Until) })
	return out
}

// clusterEvents adds other servers' security events.
func (s *Server) clusterEvents(r *http.Request, local []shield.Event, limit int) []shield.Event {
	if s.Cluster == nil || !s.Cluster.Enabled() {
		return local
	}
	var mu sync.Mutex
	all := slices.Clone(local)
	id := s.identity(r)
	s.eachNode(r.Context(), 5*time.Second, func(ctx context.Context, n *store.Node) error {
		var list []shield.Event
		q := url.Values{"limit": {strconv.Itoa(limit)}}
		if site := r.URL.Query().Get("site"); site != "" {
			q.Set("site", site)
		}
		if err := s.nodeAPI(ctx, n.ID, id, http.MethodGet, "/api/v1/security/events?"+q.Encode(), nil, &list); err != nil {
			return err
		}
		mu.Lock()
		all = append(all, list...)
		mu.Unlock()
		return nil
	})
	slices.SortStableFunc(all, func(a, b shield.Event) int { return b.Time.Compare(a.Time) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}
