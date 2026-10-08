package site

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// RunCron runs every active site's due WP-Cron events once a minute until
// ctx ends. It replaces WordPress's default of spawning cron from page views
// (new sites set DISABLE_WP_CRON), which piles work onto visitor requests on
// busy sites and never runs on quiet ones.
func (s *Service) RunCron(ctx context.Context) {
	sem := make(chan struct{}, s.Cfg.CronConcurrency)
	var inFlight, noJail sync.Map
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sites, err := s.Store.ListSites(ctx)
		if err != nil {
			s.Log.Error("cron: listing sites", "err", err)
			continue
		}
		for _, st := range sites {
			// Staging sites get no cron: a copy of a shop must not charge
			// renewals or e-mail customers.
			if st.Status != store.StatusActive || st.ParentID != "" {
				continue
			}
			// A slow run is never stacked with the next minute's run.
			if _, busy := inFlight.LoadOrStore(st.ID, true); busy {
				continue
			}
			go func(st *store.Site) {
				defer inFlight.Delete(st.ID)
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				c, cancel := context.WithTimeout(ctx, 6*time.Minute) // > the in-container timeout
				defer cancel()
				out, err := s.Runtime.RunCron(c, runtime.SiteSpec{
					ID: st.ID, Dir: s.Cfg.SiteDir(st.ID), Docroot: s.Cfg.SiteRoot(st.ID), Domain: st.PrimaryDomain,
				})
				if ctx.Err() == nil {
					s.recordCronRun(st.ID, err, out)
				}
				switch {
				case errors.Is(err, runtime.ErrNoJail):
					if _, seen := noJail.LoadOrStore(st.ID, true); !seen {
						s.Log.Info("cron: site runs an old PHP image, leaving cron to WordPress until it is scaled/rolled", "site", st.ID)
					}
				case err != nil && ctx.Err() == nil:
					s.Log.Warn("cron run failed", "site", st.ID, "err", err, "out", truncate(string(out), 500))
				}
			}(st)
		}
	}
}

// cronRun is the outcome of a site's last WP-Cron run (Tools shows it).
type cronRun struct {
	At     time.Time
	Err    string
	NoJail bool // the site runs an image without the cron jail
}

func (s *Service) recordCronRun(id string, err error, out []byte) {
	r := cronRun{At: time.Now().UTC(), NoJail: errors.Is(err, runtime.ErrNoJail)}
	if err != nil && !r.NoJail {
		r.Err = truncate(strings.TrimSpace(err.Error()+" "+string(out)), 300)
	}
	s.cronRuns.Store(id, r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
