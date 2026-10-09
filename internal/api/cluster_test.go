package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/updater"
)

type nopProxy struct{ sites []proxy.Site }

func (p *nopProxy) Apply(_ context.Context, sites []proxy.Site) error { p.sites = sites; return nil }

type server struct {
	st    *store.Store
	svc   *site.Service
	api   *Server
	h     http.Handler
	agent *cluster.Agent
	proxy *nopProxy
}

func newClusterServer(t *testing.T, node bool) *server {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ClusterDir = t.TempDir()
	px := &nopProxy{}
	svc := &site.Service{Cfg: cfg, Store: st, Log: log, Proxy: px}
	svc.Jobs = &jobs.Queue{Store: st, Log: log}
	sh := shield.New(shield.Options{Secret: []byte("k"), Sites: svc.ShieldLookup})
	a := &cluster.Agent{Dir: cfg.ClusterDir, Log: log, Tunnel: svc.TunnelTarget, Peers: svc.PeerHandler(),
		Info: func(ctx context.Context) any { return svc.NodeInfo(ctx, "test") }}
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	svc.ClusterClient = a.Client
	srv := &server{st: st, svc: svc, agent: a, proxy: px}
	srv.api = &Server{Token: "tok", Version: "test", Sites: svc, Store: st, Shield: sh, Jobs: svc.Jobs, Log: log,
		Updater: &updater.Updater{Current: "test", Repo: "o/r", StateDir: t.TempDir(), APIBase: "http://127.0.0.1:1"},
		Node:    node}
	if node {
		a.Control = svc.ClusterHandler()
		a.OnPaired = func(_ string, num int64) { st.JobIDFloor(context.Background(), num*cluster.JobStride) }
	} else {
		ctrl := &cluster.Controller{Store: st, Dir: cfg.ClusterDir, Log: log, Agent: a, PlaceOnControl: true,
			LocalInfo: func(ctx context.Context) cluster.NodeInfo { return svc.NodeInfo(ctx, "test") }}
		svc.Cluster, svc.DomainTaken = ctrl, st.ClusterDomainTaken
		srv.api.Cluster = ctrl
		ctrl.Configure = srv.api.ConfigureNode
	}
	srv.h = srv.api.Handler()
	if node {
		a.API = srv.h
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		a.Listen = ln.Addr().String()
		go a.ServeListener(ctx, ln)
	}
	return srv
}

func (s *server) do(t *testing.T, method, path, body string, out any) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if out != nil && rec.Code/100 == 2 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	if rec.Code/100 != 2 {
		t.Logf("%s %s: %d %s", method, path, rec.Code, rec.Body)
	}
	return rec.Code
}

func TestClusterForwarding(t *testing.T) {
	ctx := context.Background()
	panel := newClusterServer(t, false)
	node := newClusterServer(t, true)

	// Pairing through the API.
	code, err := node.agent.PairingCode()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(cluster.AddNodeInput{Name: "Web 2", Address: node.agent.Listen, PairingCode: code})
	var n store.Node
	if st := panel.do(t, "POST", "/api/v1/nodes", string(body), &n); st != 201 || n.ID != "web-2" || n.Num != 1 {
		t.Fatalf("add node: %d %+v", st, n)
	}
	if st := panel.do(t, "POST", "/api/v1/nodes", string(body), nil); st/100 == 2 {
		t.Fatal("a pairing code worked twice")
	}

	// A site created on the node shows up on the panel after a health pass.
	remote := &store.Site{ID: "sremote1", Name: "Remote", PrimaryDomain: "remote.test", PHPVersion: "8.3",
		FPMPort: 19000, DBName: "wp_sremote1", Status: store.StatusActive, ShieldMode: "standard",
		MemoryMB: 512, CPUs: 1, Replicas: 1}
	if err := node.st.CreateSite(ctx, remote); err != nil {
		t.Fatal(err)
	}
	panel.api.Cluster.Refresh(ctx)
	var sites []store.Site
	panel.do(t, "GET", "/api/v1/sites", "", &sites)
	if len(sites) != 1 || sites[0].ID != "sremote1" || sites[0].Node != "web-2" {
		t.Fatalf("panel listing: %+v", sites)
	}

	// Reads and changes about it are the node's.
	var got store.Site
	if st := panel.do(t, "GET", "/api/v1/sites/sremote1", "", &got); st != 200 || got.PrimaryDomain != "remote.test" {
		t.Fatalf("forwarded read: %d %+v", st, got)
	}
	if st := panel.do(t, "PUT", "/api/v1/sites/sremote1/shield", `{"mode":"under_attack","block_ai_bots":true}`, nil); st != 200 {
		t.Fatalf("forwarded change: %d", st)
	}
	if st, _ := node.st.GetSite(ctx, "sremote1"); st.ShieldMode != "under_attack" {
		t.Fatalf("the change didn't reach the node: %s", st.ShieldMode)
	}
	if cs, _ := panel.st.ClusterSite(ctx, "sremote1"); cs.Site.ShieldMode != "under_attack" {
		t.Fatal("the registry wasn't refreshed after the change")
	}
	if len(node.proxy.sites) != 1 {
		t.Fatalf("node's Caddy not synced: %+v", node.proxy.sites)
	}

	// Nor added to a site on another server when it is used elsewhere (the
	// node only knows its own sites): the panel checks every server first.
	local := &store.Site{ID: "slocal01", Name: "Local", PrimaryDomain: "local.test", PHPVersion: "8.3",
		FPMPort: 19500, DBName: "wp_slocal01", Status: store.StatusActive, ShieldMode: "standard",
		MemoryMB: 512, CPUs: 1, Replicas: 1}
	if err := panel.st.CreateSite(ctx, local); err != nil {
		t.Fatal(err)
	}
	if st := panel.do(t, "POST", "/api/v1/sites/sremote1/domains", `{"domain":"local.test"}`, nil); st != 409 {
		t.Fatalf("a domain of another server's site added: %d", st)
	}

	// A node can't take over the panel's sites or domains through its list:
	// the panel would forward that site's operations (and secrets) to it.
	for _, fake := range []*store.Site{
		{ID: "slocal01", Name: "hijack", PrimaryDomain: "elsewhere.test", PHPVersion: "8.3", FPMPort: 19700,
			DBName: "wp_slocal01", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1},
		{ID: "sclaim01", Name: "claim", PrimaryDomain: "local.test", PHPVersion: "8.3", FPMPort: 19701,
			DBName: "wp_sclaim01", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1},
	} {
		if err := node.st.CreateSite(ctx, fake); err != nil {
			t.Fatal(err)
		}
	}
	panel.api.Cluster.Refresh(ctx)
	if _, err := panel.st.ClusterSite(ctx, "slocal01"); err == nil {
		t.Fatal("a node's site took over a panel site's ID in the registry")
	}
	if _, err := panel.st.ClusterSite(ctx, "sclaim01"); err == nil {
		t.Fatal("a node's site claiming the panel's domain entered the registry")
	}
	if node, remote, _ := panel.api.Cluster.SiteNode(ctx, "slocal01"); remote {
		t.Fatalf("slocal01 now routed to %s", node)
	}

	// A domain used on another server can't be taken here.
	if st := panel.do(t, "POST", "/api/v1/sites", `{"domain":"remote.test","admin_email":"a@b.co","node":"local"}`, nil); st != 409 {
		t.Fatalf("duplicate domain across servers: %d", st)
	}

	// Jobs: the node's IDs tell the panel where they ran.
	jid, err := node.st.CreateJob(ctx, "sremote1", "backup", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if cluster.JobNodeNum(jid) != 1 {
		t.Fatalf("node job ID %d not numbered for node 1", jid)
	}
	var job struct {
		Job store.Job `json:"job"`
	}
	if st := panel.do(t, "GET", "/api/v1/jobs/"+strconv.FormatInt(jid, 10), "", &job); st != 200 || job.Job.ID != jid {
		t.Fatalf("remote job: %d %+v", st, job)
	}
	var list []store.Job
	panel.do(t, "GET", "/api/v1/jobs", "", &list)
	if len(list) != 1 || list[0].ID != jid {
		t.Fatalf("merged jobs: %+v", list)
	}

	// Server-wide security lists and backup destinations reach the node.
	if st := panel.do(t, "PUT", "/api/v1/security/settings", `{"allow":["203.0.113.0/24"],"deny":[]}`, nil); st != 200 {
		t.Fatalf("security settings: %d", st)
	}
	if g, _ := node.svc.GlobalLists(ctx); len(g.Allow) != 1 {
		t.Fatalf("security lists not on the node: %+v", g)
	}
	if err := panel.st.CreateRepo(ctx, &store.BackupRepo{ID: "rs3", Name: "S3", Kind: "s3", Location: "s3:https://s3.test/b",
		Password: "pw", Secrets: store.RepoSecrets{AccessKeyID: "AK", SecretAccessKey: "SK"}}); err != nil {
		t.Fatal(err)
	}
	// The Divi license reaches the node whole (it installs Divi on its own
	// sites); the panel's answer never carries the key.
	var dv site.DiviView
	if st := panel.do(t, "PUT", "/api/v1/settings/divi", `{"username":"acme","api_key":"abcdef0123456789"}`, &dv); st != 200 ||
		!dv.Configured || !dv.NewSites || dv.KeyHint != "…6789" {
		t.Fatalf("divi license: %d %+v", st, dv)
	}
	if l, _ := node.svc.Divi(ctx); l.Username != "acme" || l.APIKey != "abcdef0123456789" || !l.NewSites {
		t.Fatalf("divi license on the node: %+v", l)
	}
	// Only the switch changes on the panel: the node still gets the key.
	if st := panel.do(t, "PUT", "/api/v1/settings/divi", `{"new_sites":false}`, nil); st != 200 {
		t.Fatalf("divi switch: %d", st)
	}
	if l, _ := node.svc.Divi(ctx); l.APIKey != "abcdef0123456789" || l.NewSites {
		t.Fatalf("divi switch on the node: %+v", l)
	}
	// A node that lost it (or paired later) gets it when configured.
	if _, err := node.svc.SetDivi(ctx, site.DiviInput{APIKey: new(string)}); err != nil {
		t.Fatal(err)
	}
	nd, _ := panel.st.GetNode(ctx, "web-2")
	if err := panel.api.ConfigureNode(ctx, nd); err != nil {
		t.Fatal(err)
	}
	if l, _ := node.svc.Divi(ctx); l.APIKey != "abcdef0123456789" || l.Username != "acme" {
		t.Fatalf("divi license after configuring the node: %+v", l)
	}
	if r, err := node.st.GetRepo(ctx, "rs3"); err != nil || r.Password != "pw" || r.Secrets.SecretAccessKey != "SK" {
		t.Fatalf("repository on the node: %+v %v", r, err)
	}

	// The node's own API ignores forged identity headers: only a request
	// over the panel's certificate carries an identity.
	req := httptest.NewRequest("GET", "/api/v1/sites", nil)
	req.Header.Set(cluster.HeaderActor, "mallory")
	req.Header.Set(cluster.HeaderRole, "admin")
	rec := httptest.NewRecorder()
	node.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged identity on the node's loopback API: %d", rec.Code)
	}
	// The node serves no sign-in or accounts.
	for _, p := range []string{"/api/v1/auth/state", "/api/v1/users", "/api/v1/mail"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		node.h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK && !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("node serves %s", p)
		}
	}

	// Nodes listing: the panel's server first.
	var nodes []nodeView
	panel.do(t, "GET", "/api/v1/nodes", "", &nodes)
	if len(nodes) != 2 || !nodes[0].Local || nodes[1].ID != "web-2" || !nodes[1].Up || nodes[1].Sites != 1 {
		t.Fatalf("nodes: %+v", nodes)
	}

	// Spreading a remote site: the panel grants, forwards (the body intact)
	// and takes the grant back when the node refuses.
	if st := panel.do(t, "PUT", "/api/v1/sites/sremote1/spread", `{"nodes":["local"]}`, nil); st != 400 {
		t.Fatalf("refused spread: %d", st)
	}
	if v, _ := panel.st.Setting(ctx, "cluster_spread_grants"); v != "" && v != "{}" {
		t.Fatalf("grant left behind after a refused spread: %s", v)
	}

	// Removing a node with sites needs force.
	if st := panel.do(t, "DELETE", "/api/v1/nodes/web-2", "", nil); st != 409 {
		t.Fatalf("remove with sites: %d", st)
	}
	_ = io.Discard
	_ = time.Second
}

// The route wrapper must let handlers reach the connection: streaming
// (flush) and long downloads (write deadline) depend on it.
func TestRecorderUnwraps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: &statusCapture{ResponseWriter: w}}
		rc := http.NewResponseController(rec)
		if err := rc.SetWriteDeadline(time.Now().Add(time.Hour)); err != nil {
			t.Errorf("write deadline through the recorders: %v", err)
		}
		if err := rc.Flush(); err != nil {
			t.Errorf("flush through the recorders: %v", err)
		}
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
