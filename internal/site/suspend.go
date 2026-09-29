package site

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/store"
)

// Suspension takes a site offline when its account is suspended (billing,
// bandwidth overage, an administrator) without losing anything: Caddy
// answers every domain with a static 503 page, the PHP replicas are
// stopped to free their memory and CPU, and cron, backups, updates, scans
// and the autoscaler skip it (they all act on active sites only). Files,
// the database, backups and settings stay; Unsuspend starts the replicas
// again and puts the site back in the proxy.

// suspendedProxySite is a suspended site's proxy entry: every domain it
// answers for (redirects included) gets the 503 page.
func suspendedProxySite(st *store.Site, customCert bool) proxy.Site {
	domains := append([]string{st.PrimaryDomain}, slices.DeleteFunc(slices.Clone(st.Domains),
		func(d string) bool { return d == st.PrimaryDomain })...)
	return proxy.Site{ID: st.ID, Name: st.Name, Domains: append(domains, st.RedirectDomains...),
		CustomCert: customCert, Suspended: true}
}

// Suspend suspends an active site (a no-op for one already suspended).
// Traffic is cut first; the replicas stop once no update, backup or other
// job holds the site (those check the site is active before they start,
// and one already running finishes first).
func (s *Service) Suspend(ctx context.Context, id string) error {
	s.opsMu.Lock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		s.opsMu.Unlock()
		return err
	}
	switch st.Status {
	case store.StatusSuspended, store.StatusFailed: // a failed site serves nothing already
		s.opsMu.Unlock()
		return nil
	case store.StatusActive:
	default: // still being created: the account's hourly reconcile comes back
		s.opsMu.Unlock()
		return fmt.Errorf("%w: site is %s", ErrConflict, st.Status)
	}
	if err := s.Store.SetSiteStatus(ctx, id, store.StatusSuspended); err != nil {
		s.opsMu.Unlock()
		return err
	}
	if err := s.Sync(ctx); err != nil {
		// Caddy still serves it: say so rather than stop replicas it routes to.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		err = errors.Join(err, s.Store.SetSiteStatus(c, id, store.StatusActive), s.Sync(c))
		s.opsMu.Unlock()
		return err
	}
	s.opsMu.Unlock()
	s.event(id, "account", "Suspended: visitors get a 503 page and PHP is stopped; files and database are kept")
	if s.SiteSuspended != nil {
		s.SiteSuspended(ctx, id, true)
	}
	post := context.WithoutCancel(ctx)
	lock := s.maintLock(id)
	if lock.TryLock() {
		defer lock.Unlock()
		s.stopSuspended(post, id)
		return nil
	}
	s.Log.Info("suspended site's replicas stop after the running job", "site", id)
	go func() {
		lock.Lock()
		defer lock.Unlock()
		s.stopSuspended(post, id)
	}()
	return nil
}

// stopSuspended removes a suspended site's replicas, unless it was
// unsuspended in the meantime. Caller holds the site's maintenance lock.
func (s *Service) stopSuspended(ctx context.Context, id string) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil || st.Status != store.StatusSuspended {
		return
	}
	s.stopReplicasLocked(ctx, id)
}

// stopReplicasLocked removes every replica of a site and releases its
// ports. Caller holds opsMu.
func (s *Service) stopReplicasLocked(ctx context.Context, id string) {
	reps, err := s.Runtime.Replicas(ctx, id)
	if err != nil {
		s.Log.Error("listing a suspended site's replicas", "site", id, "err", err)
		return
	}
	for _, r := range reps {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		if err := s.Runtime.StopReplica(c, r.Name); err != nil {
			s.Log.Error("stopping a suspended site's replica", "site", id, "container", r.Name, "err", err)
		}
		cancel()
	}
	// Its ports go back to the pool; Unsuspend allocates fresh ones.
	if err := s.Store.SetUpstreams(ctx, id, nil); err != nil {
		s.Log.Error("releasing a suspended site's ports", "site", id, "err", err)
	}
}

// Unsuspend brings a suspended site back (a no-op for an active one): its
// replicas start at its stored size, and only once they answer does the
// proxy route to them instead of the 503 page.
func (s *Service) Unsuspend(ctx context.Context, id string) error {
	retire, err := s.unsuspendLocked(ctx, id)
	if err != nil {
		return err
	}
	if retire == nil {
		return nil // was not suspended
	}
	retire()
	s.event(id, "account", "Unsuspended: the site is online again")
	if s.SiteSuspended != nil {
		s.SiteSuspended(ctx, id, false)
	}
	return nil
}

func (s *Service) unsuspendLocked(ctx context.Context, id string) (func(), error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	switch st.Status {
	case store.StatusActive, store.StatusFailed:
		return nil, nil
	case store.StatusSuspended:
	default:
		return nil, fmt.Errorf("%w: site is %s", ErrConflict, st.Status)
	}
	// Start the replicas while the site is still suspended: the proxy keeps
	// serving the 503 page (a Sync in between must never see an active site
	// without upstreams), then switch it over.
	retire, err := s.reconcile(ctx, st)
	if err != nil {
		// Whatever the proxy last loaded shows this site's 503 page, never
		// its new replicas: anything the reconcile left running can go.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		s.stopReplicasLocked(c, id)
		return nil, err
	}
	if err := s.Store.SetSiteStatus(ctx, id, store.StatusActive); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if rerr := s.Store.SetSiteStatus(c, id, store.StatusSuspended); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		// The 503 page stays: the replicas just started go again.
		if rerr := s.Sync(c); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		s.stopReplicasLocked(c, id)
		return nil, err
	}
	return retire, nil
}
