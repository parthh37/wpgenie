package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestClusterStore(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		num, err := s.NextNodeNum(ctx)
		if err != nil || num != 1 {
			t.Fatalf("first node number: %d %v", num, err)
		}
		n := &Node{ID: "web-2", Num: num, Name: "Web 2", Address: "10.0.0.2:7443", PublicIP: "203.0.113.2",
			Info: json.RawMessage(`{"cpus":4}`), CertNotAfter: time.Now().Add(time.Hour)}
		if err := s.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateNode(ctx, &Node{ID: "web-3", Num: num, Name: "dup", Address: "x:1"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate node number: %v", err)
		}
		// Numbers are never reused, even after the node is removed.
		if err := s.DeleteNode(ctx, "web-2"); err != nil {
			t.Fatal(err)
		}
		if next, _ := s.NextNodeNum(ctx); next != 2 {
			t.Fatalf("number after removal: %d", next)
		}
		n.Num = 2
		if err := s.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		if err := s.NodeSeen(ctx, "web-2", json.RawMessage(`{"cpus":8}`), time.Now().Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, err := s.NodeByNum(ctx, 2)
		if err != nil || got.ID != "web-2" || string(got.Info) != `{"cpus":8}` || got.LastSeen.IsZero() {
			t.Fatalf("node: %+v %v", got, err)
		}

		// Registry: a site on another node, its domains taken cluster-wide.
		remote := &Site{ID: "sremote1", PrimaryDomain: "r.test", Domains: []string{"r.test", "alias.r.test"},
			RedirectDomains: []string{"www.r.test"}, Replicas: 2}
		if err := s.PutClusterSite(ctx, "web-2", remote); err != nil {
			t.Fatal(err)
		}
		remote.Replicas = 3
		if err := s.PutClusterSite(ctx, "web-2", remote); err != nil { // upsert
			t.Fatal(err)
		}
		cs, err := s.ClusterSite(ctx, "sremote1")
		if err != nil || cs.NodeID != "web-2" || cs.Site.Replicas != 3 || cs.Site.Node != "web-2" {
			t.Fatalf("registry: %+v %v", cs, err)
		}
		for d, want := range map[string]bool{"r.test": true, "alias.r.test": true, "www.r.test": true, "other.test": false} {
			if taken, err := s.ClusterDomainTaken(ctx, d, ""); err != nil || taken != want {
				t.Errorf("ClusterDomainTaken(%s) = %v %v", d, taken, err)
			}
		}

		// A local site with local and remote upstreams kept apart.
		local := &Site{ID: "slocal01", Name: "l", PrimaryDomain: "l.test", PHPVersion: "8.3", FPMPort: 19000,
			DBName: "wp_slocal01", Status: StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 3}
		if err := s.CreateSite(ctx, local); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSpreadNodes(ctx, "slocal01", []string{"web-2"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetRemoteUpstreams(ctx, "slocal01", []RemoteUpstream{{Port: 19100, Node: "web-2", RemotePort: 19000}}); err != nil {
			t.Fatal(err)
		}
		// Rescaling the local replicas leaves the remote ones alone.
		if err := s.SetUpstreams(ctx, "slocal01", []int{19000, 19001}); err != nil {
			t.Fatal(err)
		}
		st, err := s.GetSite(ctx, "slocal01")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(st.Upstreams, []int{19000, 19001}) || len(st.RemoteUpstreams) != 1 ||
			st.RemoteUpstreams[0].Port != 19100 || !slices.Equal(st.SpreadNodes, []string{"web-2"}) {
			t.Fatalf("upstreams: local %v remote %+v spread %v", st.Upstreams, st.RemoteUpstreams, st.SpreadNodes)
		}
		// Every port in use is skipped when allocating: local, tunnels, guest
		// replicas and forwards.
		if err := s.AddGuestReplica(ctx, GuestReplica{Port: 19002, SiteID: "sguest01", HomeNode: "web-3"}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutSiteForward(ctx, SiteForward{SiteID: "sgone001", NodeID: "web-2", Address: "10.0.0.2:7443",
			Domains: []string{"g.test"}, Port: 19003, HTTPPort: 19004, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		ports, err := s.AllocatePorts(ctx, 19000, 2)
		if err != nil || !slices.Equal(ports, []int{19005, 19006}) {
			t.Fatalf("allocation around used ports: %v %v", ports, err)
		}
		all, err := s.AllRemoteUpstreams(ctx)
		if err != nil || len(all["slocal01"]) != 1 {
			t.Fatalf("all remote upstreams: %+v %v", all, err)
		}
		fw, err := s.SiteForwards(ctx)
		if err != nil || len(fw) != 1 || fw[0].HTTPPort != 19004 || fw[0].Domains[0] != "g.test" {
			t.Fatalf("forwards: %+v %v", fw, err)
		}

		// Job numbering: a node's jobs start above its floor.
		const floor = 2_000_000_000_000
		if err := s.JobIDFloor(ctx, floor); err != nil {
			t.Fatal(err)
		}
		id, err := s.CreateJob(ctx, "", "backup", "x")
		if err != nil || id <= floor || id > floor+10 {
			t.Fatalf("job after floor: %d %v", id, err)
		}
		if err := s.JobIDFloor(ctx, floor); err != nil { // idempotent, never backwards
			t.Fatal(err)
		}
		if id2, _ := s.CreateJob(ctx, "", "backup", "x"); id2 != id+1 {
			t.Fatalf("job numbering moved: %d after %d", id2, id)
		}
	})
}
