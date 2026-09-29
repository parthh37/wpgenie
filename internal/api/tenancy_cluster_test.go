package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/store"
)

// A tenant's site on another server: ownership and the plan are checked on
// the panel, before anything reaches the node; what the plan allows is
// forwarded (as an operator: nodes know nothing of accounts).
func TestTenantsOnRemoteSites(t *testing.T) {
	ctx := context.Background()
	e := newTenancyEnv(t)
	dir := t.TempDir()
	agent := &cluster.Agent{Dir: dir, Log: slog.New(slog.DiscardHandler)}
	ctrl := &cluster.Controller{Store: e.st, Dir: dir, Log: slog.New(slog.DiscardHandler), Agent: agent}
	e.api.Cluster = ctrl
	e.api.Sites.Cluster, e.api.Sites.DomainTaken = ctrl, e.st.ClusterDomainTaken
	e.srv.Config.Handler = e.api.Handler()

	node := newClusterServer(t, true)
	code, err := node.agent.PairingCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.AddNode(ctx, cluster.AddNodeInput{Name: "web-2", Address: node.agent.Listen, PairingCode: code}); err != nil {
		t.Fatal(err)
	}
	remote := &store.Site{ID: "sremot01", Name: "Remote", PrimaryDomain: "remote.test", PHPVersion: "8.3",
		FPMPort: 19000, DBName: "wp_sremot01", Status: store.StatusActive, ShieldMode: "standard",
		MemoryMB: 512, CPUs: 1, Replicas: 1}
	if err := node.st.CreateSite(ctx, remote); err != nil {
		t.Fatal(err)
	}
	ctrl.Refresh(ctx)
	if err := e.st.AssignSite(ctx, "sremot01", e.acct["A"].ID); err != nil {
		t.Fatal(err)
	}

	// Alice sees it with her other sites; Bob doesn't see it at all.
	var list []siteView
	e.as("alice", "GET", "/api/v1/sites", "", &list)
	if !strings.Contains(strings.Join(ids(list), ","), "sremot01") {
		t.Fatalf("alice's sites: %v", ids(list))
	}
	e.as("bob", "GET", "/api/v1/sites", "", &list)
	if strings.Contains(strings.Join(ids(list), ","), "sremot01") {
		t.Fatal("bob sees alice's remote site")
	}
	if c := e.as("bob", "GET", "/api/v1/sites/sremot01", "", nil); c != 404 {
		t.Fatalf("bob reading alice's remote site: %d", c)
	}
	if c := e.as("bob", "PUT", "/api/v1/sites/sremot01/shield", `{"mode":"off","block_ai_bots":false}`, nil); c != 404 {
		t.Fatalf("bob changing alice's remote site: %d", c)
	}

	// Her plan (Basic: 2 replicas, 1 GB, 1 CPU, no staging) holds on the
	// remote site: refused here, never forwarded.
	var out map[string]string
	if c := e.as("alice", "PUT", "/api/v1/sites/sremot01/resources", `{"memory_mb":2048,"cpus":1,"replicas":1}`, &out); c != 403 ||
		!strings.Contains(out["error"], "plan") {
		t.Fatalf("resources beyond the plan on a remote site: %d %v", c, out)
	}
	if c := e.as("alice", "PUT", "/api/v1/sites/sremot01/autoscale", `{"enabled":true,"min_replicas":1,"max_replicas":5,"target_cpu":70}`, &out); c != 403 {
		t.Fatalf("autoscale beyond the plan on a remote site: %d %v", c, out)
	}
	if c := e.as("alice", "PUT", "/api/v1/sites/sremot01/backups/policy", `{"repo_id":"offsite","interval_hours":24}`, &out); c != 403 {
		t.Fatalf("a destination outside the plan on a remote site: %d %v", c, out)
	}
	if c := e.as("alice", "POST", "/api/v1/sites/sremot01/staging", `{}`, &out); c != 403 {
		t.Fatalf("staging without the feature on a remote site: %d", c)
	}
	// Tenants don't choose servers.
	if c := e.as("alice", "POST", "/api/v1/sites", `{"domain":"x.test","admin_email":"a@b.co","node":"web-2"}`, &out); c != 403 {
		t.Fatalf("a tenant choosing a server: %d %v", c, out)
	}

	// Within the plan: forwarded, done on the node.
	var got store.Site
	if c := e.as("alice", "PUT", "/api/v1/sites/sremot01/shield", `{"mode":"under_attack","block_ai_bots":true}`, &got); c != 200 {
		t.Fatalf("alice's own change: %d", c)
	}
	if st, _ := node.st.GetSite(ctx, "sremot01"); st.ShieldMode != "under_attack" {
		t.Fatalf("the change didn't reach the node: %s", st.ShieldMode)
	}
	// The node recorded who did it, as the operator the panel vouched for.
	entries, err := node.st.Audit(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(entries)
	if !strings.Contains(string(b), "alice") {
		t.Fatalf("node's audit log doesn't name alice: %s", b)
	}
}
