package site

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/parthh37/wpgenie/internal/cluster"
)

// Local tunnel listeners on this server: loopback ports Caddy connects to
// that come out on another server of the cluster. Two kinds:
//
//   - a moved site's forward (ingress and certificate challenges to its
//     new server; store.SiteForward);
//   - a spread site's replicas on other nodes (FastCGI to fpm:<port>;
//     store.RemoteUpstream).
//
// openTunnels makes the open listeners match the store, so it is safe to
// call after any change and at startup.

type tunnelKey struct {
	port   int
	node   string
	addr   string
	target string
}

type tunnels struct {
	mu   sync.Mutex
	open map[tunnelKey]*cluster.Forward
}

var errNotClustered = errors.New("this server is not part of a cluster")

func (s *Service) openTunnels(ctx context.Context) error {
	want := map[tunnelKey]bool{}
	fwds, err := s.Store.SiteForwards(ctx)
	if err != nil {
		return err
	}
	for _, f := range fwds {
		want[tunnelKey{f.Port, f.NodeID, f.Address, cluster.TargetIngress}] = true
		want[tunnelKey{f.HTTPPort, f.NodeID, f.Address, cluster.TargetHTTP}] = true
	}
	ups, err := s.Store.AllRemoteUpstreams(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, list := range ups {
		for _, u := range list {
			ep, err := s.Peer(ctx, u.Node)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			want[tunnelKey{u.Port, u.Node, ep.Address, cluster.TargetFPM + ":" + strconv.Itoa(u.RemotePort)}] = true
		}
	}

	s.tun.mu.Lock()
	defer s.tun.mu.Unlock()
	if s.tun.open == nil {
		s.tun.open = map[tunnelKey]*cluster.Forward{}
	}
	for k, f := range s.tun.open {
		if !want[k] {
			f.Close()
			delete(s.tun.open, k)
		}
	}
	if len(want) == 0 {
		return errors.Join(errs...)
	}
	var client *cluster.Client
	if s.ClusterClient != nil {
		client = s.ClusterClient()
	}
	if client == nil {
		return errors.Join(append(errs, errNotClustered)...)
	}
	for k := range want {
		if _, ok := s.tun.open[k]; ok {
			continue
		}
		f, err := client.Listen(net.JoinHostPort("127.0.0.1", strconv.Itoa(k.port)),
			cluster.Endpoint{ID: k.node, Address: k.addr}, k.target, s.Log)
		if err != nil {
			errs = append(errs, fmt.Errorf("tunnel on port %d to %s: %w", k.port, k.node, err))
			continue
		}
		s.tun.open[k] = f
	}
	return errors.Join(errs...)
}

// closeUnusedTunnels closes listeners the store no longer asks for.
func (s *Service) closeUnusedTunnels(ctx context.Context) {
	if err := s.openTunnels(ctx); err != nil && !errors.Is(err, errNotClustered) {
		s.Log.Warn("cluster: tunnels", "err", err)
	}
}

// heldPorts are the ports WPGenie containers still publish (a replica
// being drained, a guest replica): never handed out again while held.
func (s *Service) heldPorts(ctx context.Context) []int {
	all, err := s.Runtime.Replicas(ctx, "")
	if err != nil {
		return nil
	}
	held := make([]int, 0, len(all))
	for _, r := range all {
		held = append(held, r.Port)
	}
	return held
}
