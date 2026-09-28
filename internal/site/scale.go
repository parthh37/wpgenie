package site

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"slices"
	"strconv"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

const (
	defaultMemoryMB = 512
	defaultCPUs     = 1.0
	minMemoryMB     = 256
	minCPUs         = 0.25
	// Connections a site needs beyond its FPM workers: WP-Cron and WP-CLI.
	dbSpareConns = 5
)

// Resources is how much compute a site gets. MemoryMB and CPUs apply to
// each replica: two replicas of 1 GB / 1 CPU can use up to 2 GB / 2 CPUs.
type Resources struct {
	MemoryMB int     `json:"memory_mb"`
	CPUs     float64 `json:"cpus"`
	Replicas int     `json:"replicas"`
}

func dbConnLimit(replicas, maxChildren int) int { return replicas*maxChildren + dbSpareConns }

func (s *Service) validateResources(r Resources) error {
	if r.MemoryMB < minMemoryMB {
		return fmt.Errorf("%w: memory_mb must be at least %d", ErrInvalidInput, minMemoryMB)
	}
	if host := hostMemoryMB(); host > 0 && r.MemoryMB > host {
		return fmt.Errorf("%w: memory_mb %d exceeds this server's %d MB", ErrInvalidInput, r.MemoryMB, host)
	}
	if n := goruntime.NumCPU(); r.CPUs < minCPUs || r.CPUs > float64(n) {
		return fmt.Errorf("%w: cpus must be between %g and %d (this server's cores)", ErrInvalidInput, minCPUs, n)
	}
	if r.Replicas < 1 || r.Replicas > s.Cfg.MaxReplicas {
		return fmt.Errorf("%w: replicas must be between 1 and %d", ErrInvalidInput, s.Cfg.MaxReplicas)
	}
	// Every busy worker holds a MariaDB connection, and all sites share one
	// server: no single site may claim more than half of it.
	conns := dbConnLimit(r.Replicas, runtime.FPMMaxChildren(r.MemoryMB))
	if budget := s.Cfg.DBMaxConnections / 2; conns > budget {
		return fmt.Errorf("%w: %d replicas × %d PHP workers need %d database connections; one site may use %d "+
			"(use fewer replicas or less memory, or raise db_max_connections with MariaDB's max_connections)",
			ErrInvalidInput, r.Replicas, runtime.FPMMaxChildren(r.MemoryMB), conns, budget)
	}
	return nil
}

// hostMemoryMB is the server's total RAM, or 0 when unknown (non-Linux dev).
func hostMemoryMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var kb int
		if _, err := fmt.Sscanf(sc.Text(), "MemTotal: %d kB", &kb); err == nil {
			return kb / 1024
		}
	}
	return 0
}

func (s *Service) specFor(ctx context.Context, st *store.Site) (runtime.SiteSpec, error) {
	imageID, err := s.Runtime.ImageID(ctx, s.Cfg.PHPImage)
	if err != nil {
		return runtime.SiteSpec{}, err
	}
	return runtime.SiteSpec{
		ID: st.ID, Image: s.Cfg.PHPImage, ImageID: imageID,
		Dir: s.Cfg.SiteDir(st.ID), Docroot: s.Cfg.SiteRoot(st.ID), Domain: st.PrimaryDomain,
		Network: s.Cfg.DockerNetwork, MemoryMB: st.MemoryMB, CPUs: st.CPUs,
		MaxChildren: runtime.FPMMaxChildren(st.MemoryMB),
	}, nil
}

// Scale changes a site's resources and replica count without downtime.
// Calling it with the current values is also how a site picks up a rebuilt
// PHP image: replicas from the old image are rolled.
func (s *Service) Scale(ctx context.Context, id string, r Resources) (*store.Site, error) {
	if err := s.validateResources(r); err != nil {
		return nil, err
	}
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	old := Resources{st.MemoryMB, st.CPUs, st.Replicas}
	if err := s.Store.SetResources(ctx, id, r.MemoryMB, r.CPUs, r.Replicas); err != nil {
		return nil, err
	}
	st.MemoryMB, st.CPUs, st.Replicas = r.MemoryMB, r.CPUs, r.Replicas
	if err := s.reconcile(ctx, st); err != nil {
		// The old replicas are still the ones serving: record that.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rerr := s.Store.SetResources(c, id, old.MemoryMB, old.CPUs, old.Replicas); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return nil, err
	}
	return s.Store.GetSite(ctx, id)
}

// reconcile makes the site's running replicas match its stored resources
// with a blue/green swap. Caller holds opsMu.
//
//  1. Keep replicas that already match the desired spec.
//  2. Start the missing ones on fresh ports and wait until PHP-FPM listens.
//  3. Point Caddy at exactly the new set (atomic config reload).
//  4. Drain and remove everything else.
//
// Until step 3 succeeds nothing visitors reach has changed, so any failure
// before it only has to clean up the containers it started. Failures after
// it are logged, not returned: the site is already served by the new spec
// and leftover containers are retired by the next reconcile.
func (s *Service) reconcile(ctx context.Context, st *store.Site) error {
	spec, err := s.specFor(ctx, st)
	if err != nil {
		return err
	}
	current, err := s.Runtime.Replicas(ctx, st.ID)
	if err != nil {
		return err
	}
	hash := spec.Hash()
	var keepPorts []int
	var retire []runtime.Replica
	for _, r := range current {
		if r.Running && r.SpecHash == hash && slices.Contains(st.Upstreams, r.Port) && len(keepPorts) < st.Replicas {
			keepPorts = append(keepPorts, r.Port)
		} else {
			retire = append(retire, r)
		}
	}

	// Ports still held by containers the store no longer lists (e.g. a drain
	// cut short by a daemon restart) must not be handed out again.
	all, err := s.Runtime.Replicas(ctx, "")
	if err != nil {
		return err
	}
	var held []int
	for _, r := range all {
		held = append(held, r.Port)
	}
	newPorts, err := s.Store.AllocatePorts(ctx, s.Cfg.SitePortBase, st.Replicas-len(keepPorts), held...)
	if err != nil {
		return err
	}
	var started []string
	cleanup := func(cause error) error {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range started {
			if err := s.Runtime.StopReplica(c, name); err != nil {
				s.Log.Error("removing replica after failed scale", "container", name, "err", err)
			}
		}
		return cause
	}
	for _, p := range newPorts {
		// Tracked before starting: a failed `docker run` can still leave a
		// created container behind.
		started = append(started, runtime.ContainerName(st.ID, p))
		if err := s.Runtime.StartReplica(ctx, spec, p); err != nil {
			return cleanup(err)
		}
	}
	if err := s.waitReady(ctx, started, 60*time.Second); err != nil {
		return cleanup(err)
	}

	ports := slices.Sorted(slices.Values(append(keepPorts, newPorts...)))
	oldPorts := st.Upstreams
	if err := s.Store.SetUpstreams(ctx, st.ID, ports); err != nil {
		return cleanup(err)
	}
	if err := s.Sync(ctx); err != nil {
		// Apply can fail after Caddy already switched (the POST succeeded but
		// persisting the Caddyfile didn't, or the caller went away): put the
		// old upstreams back in the proxy before stopping anything it might
		// be routing to.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		rerr := s.Store.SetUpstreams(c, st.ID, oldPorts)
		if rerr == nil {
			rerr = s.Sync(c)
		}
		if rerr != nil {
			s.Log.Error("proxy state unknown after a failed sync; leaving old and new replicas running "+
				"(the next scale of this site retires the extras)", "site", st.ID, "err", rerr)
			return errors.Join(err, rerr)
		}
		return cleanup(err)
	}
	st.Upstreams = ports
	s.Log.Info("site scaled", "site", st.ID, "replicas", st.Replicas, "memory_mb", st.MemoryMB,
		"cpus", st.CPUs, "workers_per_replica", spec.MaxChildren, "ports", ports)

	// Traffic has switched: finish the job even if the caller disconnects,
	// or the old replicas would keep running (and holding ports) unrouted.
	post := context.WithoutCancel(ctx)
	if err := s.DB.SetConnectionLimit(post, "u_"+st.ID, dbConnLimit(st.Replicas, spec.MaxChildren)); err != nil {
		s.Log.Error("setting database connection limit", "site", st.ID, "err", err)
	}
	// Signalling PHP-FPM does not reliably let running requests finish, so
	// wait until the old replicas are idle before stopping them.
	s.drain(post, retire, drainTimeout)
	for _, r := range retire {
		c, cancel := context.WithTimeout(post, time.Minute)
		if err := s.Runtime.StopReplica(c, r.Name); err != nil {
			s.Log.Error("stopping old replica", "site", st.ID, "container", r.Name, "err", err)
		}
		cancel()
	}
	return nil
}

// drainTimeout bounds how long a retiring replica may keep serving; it
// matches request_terminate_timeout in pool.conf, after which PHP-FPM would
// kill the request anyway.
var drainTimeout = 120 * time.Second

// drain waits until every running replica in rs has had no active
// connections on two consecutive checks, or until timeout.
func (s *Service) drain(ctx context.Context, rs []runtime.Replica, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, r := range rs {
		if !r.Running {
			continue
		}
		idle := 0
		for idle < 2 {
			n, err := s.Runtime.ActiveConnections(ctx, r.Name)
			if err != nil {
				break // gone or unreachable: nothing left to wait for
			}
			if n == 0 {
				idle++
			} else {
				idle = 0
			}
			if time.Now().After(deadline) {
				s.Log.Warn("replica still busy after drain timeout, stopping it anyway",
					"container", r.Name, "active_connections", n)
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

func (s *Service) waitReady(ctx context.Context, names []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, name := range names {
		for {
			ok, err := s.Runtime.Ready(ctx, name)
			if ok {
				break
			}
			if time.Now().After(deadline) {
				if err == nil {
					err = errors.New("PHP-FPM is not listening")
				}
				return fmt.Errorf("replica %s did not become ready in %s: %w", name, timeout, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return nil
}

// upstreamAddrs is the proxy's view of a site's replicas.
func upstreamAddrs(ports []int) []string {
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = "127.0.0.1:" + strconv.Itoa(p)
	}
	return out
}
