package site

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// CPU autoscaling. Every autoscaleInterval the daemon samples each serving
// replica's CPU use as a fraction of its allowance (0.7 = 70% of the CPUs it
// was given) and adds or removes replicas through the same blue/green
// reconcile as a manual scale, so autoscaling is exactly as zero-downtime.
const (
	autoscaleInterval = 15 * time.Second
	// Scale-up acts on the average of this window, so one busy sample (a
	// cron run, a cache warm-up) doesn't add replicas...
	scaleUpWindow = 45 * time.Second
	// ...and waits this long after any scale for the new replicas to take
	// their share before judging again.
	scaleUpCooldown = time.Minute
	// Scale-down goes only to the highest count any sample in this window
	// asked for (Kubernetes HPA's stabilization window): a lull between
	// bursts must not shed the capacity the next burst needs.
	scaleDownWindow = 5 * time.Minute
	// Within ±10% of the target nothing changes, so the count doesn't flap.
	scaleTolerance = 0.1
	// A replica at its CPU limit is throttled, so its measured use caps
	// near 100% however much work is queued behind it.
	saturatedCPU = 0.9

	MinTargetCPU = 20
	MaxTargetCPU = 95
	// hostMemReserveMB stays free for MariaDB, Valkey, Caddy and the OS
	// when the autoscaler adds replicas.
	hostMemReserveMB = 512
)

// AutoscaleSettings is the autoscaling configuration of a site.
type AutoscaleSettings struct {
	Enabled     bool `json:"enabled"`
	MinReplicas int  `json:"min_replicas"`
	MaxReplicas int  `json:"max_replicas"`
	// TargetCPU is the CPU use, in percent of each replica's allowance, that
	// the autoscaler steers towards: lower keeps more headroom for spikes,
	// higher packs more traffic into fewer replicas.
	TargetCPU int `json:"target_cpu"`
}

type autoscalePolicy struct {
	min, max int
	target   float64 // fraction, e.g. 0.7
}

// desiredReplicas is the HPA rule: the replica count that would bring the
// average CPU use back to the target, assuming load spreads evenly.
func desiredReplicas(current int, util float64, p autoscalePolicy) int {
	want := current
	if ratio := util / p.target; math.Abs(ratio-1) > scaleTolerance {
		want = int(math.Ceil(float64(current)*ratio - 1e-9))
	}
	if util >= saturatedCPU && util > p.target*(1+scaleTolerance) {
		// Saturated replicas under-report demand (throttled at their limit),
		// so the ratio above is a lower bound: at least double. Only above
		// the target band: with a 95% target, 92% is where the site should
		// sit, and doubling there would flap forever.
		want = max(want, current*2)
	}
	return min(max(want, p.min), p.max)
}

type cpuSample struct {
	at       time.Time
	util     float64
	replicas int
}

// scaler is one site's autoscaling history. Only the autoscaler loop
// touches it, except busy, which a running scale action clears.
type scaler struct {
	policy    autoscalePolicy
	samples   []cpuSample // newest last, within scaleDownWindow
	since     time.Time   // first sample under the current policy
	lastScale time.Time
	memWarned time.Time
	busy      atomic.Bool
}

func (sc *scaler) observe(s cpuSample) {
	if sc.since.IsZero() {
		sc.since = s.at
	}
	sc.samples = append(sc.samples, s)
	cut := 0
	for cut < len(sc.samples) && s.at.Sub(sc.samples[cut].at) > scaleDownWindow {
		cut++
	}
	sc.samples = sc.samples[cut:]
}

// next returns the replica count the site should run now, and why.
func (sc *scaler) next(now time.Time, current int) (int, string) {
	p := sc.policy
	if current < p.min || current > p.max {
		return min(max(current, p.min), p.max), "outside the autoscaling range"
	}
	// Up: the recent average, only counting samples taken since the last
	// scale (earlier ones measured a different replica count).
	var sum float64
	var n int
	for _, s := range sc.samples {
		if now.Sub(s.at) <= scaleUpWindow && s.at.After(sc.lastScale) {
			sum += s.util
			n++
		}
	}
	if n >= 2 && now.Sub(sc.lastScale) >= scaleUpCooldown {
		avg := sum / float64(n)
		if want := desiredReplicas(current, avg, p); want > current {
			return want, fmt.Sprintf("CPU at %.0f%% of each replica's allowance (target %.0f%%)", avg*100, p.target*100)
		}
	}
	// Down: every sample in the window must agree the site is over-provisioned.
	if now.Sub(sc.since) < scaleDownWindow || now.Sub(sc.lastScale) < scaleDownWindow {
		return current, ""
	}
	stable, peak := p.min, 0.0
	for _, s := range sc.samples {
		stable = max(stable, desiredReplicas(s.replicas, s.util, p))
		peak = max(peak, s.util)
	}
	if stable < current {
		return stable, fmt.Sprintf("CPU peaked at %.0f%% over the last %d minutes (target %.0f%%)",
			peak*100, int(scaleDownWindow.Minutes()), p.target*100)
	}
	return current, ""
}

// CPUReading is a site's latest measured CPU use.
type CPUReading struct {
	At       time.Time `json:"at"`
	Percent  float64   `json:"percent"` // average use in % of each replica's allowance
	Replicas int       `json:"replicas"`
}

// CPU returns the latest CPU reading of a site, if any.
func (s *Service) CPU(id string) (CPUReading, bool) {
	v, ok := s.cpu.Load(id)
	if !ok {
		return CPUReading{}, false
	}
	return v.(CPUReading), true
}

// RunAutoscaler samples CPU use and scales autoscaled sites until ctx ends.
// Readings are kept for every active site so the panel can show them.
func (s *Service) RunAutoscaler(ctx context.Context) {
	t := time.NewTicker(autoscaleInterval)
	defer t.Stop()
	scalers := map[string]*scaler{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.autoscaleTick(ctx, now, scalers)
		}
	}
}

func (s *Service) autoscaleTick(ctx context.Context, now time.Time, scalers map[string]*scaler) {
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		s.Log.Error("autoscaler: listing sites", "err", err)
		return
	}
	active := map[string]*store.Site{}
	for _, st := range sites {
		if st.Status == store.StatusActive {
			active[st.ID] = st
		}
	}
	for id := range scalers {
		if st, ok := active[id]; !ok || !st.Autoscale {
			delete(scalers, id)
		}
	}
	s.cpu.Range(func(k, _ any) bool {
		if _, ok := active[k.(string)]; !ok {
			s.cpu.Delete(k)
		}
		return true
	})
	if len(active) == 0 {
		return
	}
	usage, err := s.Runtime.CPUUsage(ctx)
	if err != nil {
		s.Log.Warn("autoscaler: sampling CPU", "err", err)
		return
	}
	for _, st := range active {
		util, n := siteUtilization(st, usage)
		if n == 0 {
			continue
		}
		s.cpu.Store(st.ID, CPUReading{At: now, Percent: math.Round(util * 100), Replicas: n})
		if !st.Autoscale {
			continue
		}
		p := autoscalePolicy{st.MinReplicas, st.MaxReplicas, float64(st.TargetCPU) / 100}
		sc := scalers[st.ID]
		if sc == nil || sc.policy != p {
			sc = &scaler{policy: p} // new settings: judge them on fresh data
			scalers[st.ID] = sc
		}
		if sc.busy.Load() {
			continue // mid-scale: the replica set is changing under the sample
		}
		sc.observe(cpuSample{at: now, util: util, replicas: n})
		want, why := sc.next(now, st.Replicas)
		if want > st.Replicas {
			if capped := s.capByHostMemory(st, want); capped < want {
				if now.Sub(sc.memWarned) > 30*time.Minute {
					sc.memWarned = now
					s.event(st.ID, "autoscale", fmt.Sprintf("Wanted %d replicas but the server only has memory for %d; "+
						"add RAM or lower the site's memory per replica", want, capped))
				}
				want = capped
			}
		}
		if want == st.Replicas {
			continue
		}
		sc.lastScale = now
		sc.busy.Store(true)
		go func(id string, from, to int) {
			defer sc.busy.Store(false)
			s.autoscaleTo(ctx, id, from, to, why)
		}(st.ID, st.Replicas, want)
	}
}

// siteUtilization averages CPU use over the replicas currently serving the
// site (retiring ones are excluded: they are draining, not serving).
func siteUtilization(st *store.Site, usage map[string]float64) (float64, int) {
	if st.CPUs <= 0 {
		return 0, 0
	}
	var sum float64
	var n int
	for _, port := range st.Upstreams {
		if v, ok := usage[runtime.ContainerName(st.ID, port)]; ok {
			sum += v / 100 / st.CPUs
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

// capByHostMemory limits a scale-up to what fits in the server's available
// memory: container limits don't reserve memory, so without this a busy
// site could scale the whole server into swap or the OOM killer.
func (s *Service) capByHostMemory(st *store.Site, want int) int {
	avail := hostAvailableMB()
	if avail <= 0 || st.MemoryMB <= 0 {
		return want // unknown (non-Linux development): don't guess
	}
	fit := (avail - hostMemReserveMB) / st.MemoryMB
	return max(st.Replicas, min(want, st.Replicas+fit))
}

var errAutoscaleOff = errors.New("autoscaling was turned off")

func (s *Service) autoscaleTo(ctx context.Context, id string, from, to int, why string) {
	_, err := s.scaleWith(ctx, id, func(st *store.Site) (Resources, error) {
		if !st.Autoscale {
			return Resources{}, errAutoscaleOff
		}
		if st.Replicas != from {
			return Resources{}, fmt.Errorf("%w: replicas changed from %d to %d meanwhile", ErrConflict, from, st.Replicas)
		}
		return Resources{MemoryMB: st.MemoryMB, CPUs: st.CPUs, Replicas: to}, nil
	})
	switch {
	case errors.Is(err, errAutoscaleOff), errors.Is(err, ErrConflict), errors.Is(err, store.ErrNotFound):
		return // the decision is stale; the next tick decides again
	case err != nil:
		s.Log.Error("autoscale failed", "site", id, "from", from, "to", to, "err", err)
		// A persistent failure retries every minute: log it to the site's
		// activity at most every 30 minutes, or it would push out everything else.
		now := time.Now()
		if last, ok := s.scaleFailNoted.Load(id); !ok || now.Sub(last.(time.Time)) > 30*time.Minute {
			s.scaleFailNoted.Store(id, now)
			s.event(id, "autoscale", fmt.Sprintf("Scaling %d → %d replicas failed: %v", from, to, err))
		}
		return
	}
	s.scaleFailNoted.Delete(id)
	verb := "out"
	if to < from {
		verb = "in"
	}
	s.Log.Info("autoscaled", "site", id, "from", from, "to", to, "why", why)
	s.event(id, "autoscale", fmt.Sprintf("Scaled %s %d → %d replicas: %s", verb, from, to, why))
}

// SetAutoscale configures autoscaling. Turning it on with the current
// replica count outside [min, max] scales the site into range at once.
func (s *Service) SetAutoscale(ctx context.Context, id string, a AutoscaleSettings) (*store.Site, error) {
	if a.MinReplicas < 1 || a.MaxReplicas < a.MinReplicas || a.MaxReplicas > s.Cfg.MaxReplicas {
		return nil, fmt.Errorf("%w: need 1 <= min_replicas <= max_replicas <= %d", ErrInvalidInput, s.Cfg.MaxReplicas)
	}
	if a.TargetCPU < MinTargetCPU || a.TargetCPU > MaxTargetCPU {
		return nil, fmt.Errorf("%w: target_cpu must be between %d and %d (%%)", ErrInvalidInput, MinTargetCPU, MaxTargetCPU)
	}
	var prev *store.Site
	st, err := s.scaleWithUndo(ctx, id, func(st *store.Site) (Resources, error) {
		if a.Enabled {
			// The autoscaler may go up to the maximum at the current size.
			if err := s.validateResources(Resources{st.MemoryMB, st.CPUs, a.MaxReplicas}); err != nil {
				return Resources{}, fmt.Errorf("at %d replicas: %w", a.MaxReplicas, err)
			}
		}
		if err := s.Store.SetAutoscale(ctx, id, a.Enabled, a.MinReplicas, a.MaxReplicas, a.TargetCPU); err != nil {
			return Resources{}, err
		}
		cp := *st
		prev = &cp
		st.Autoscale, st.MinReplicas, st.MaxReplicas = a.Enabled, a.MinReplicas, a.MaxReplicas
		r := Resources{st.MemoryMB, st.CPUs, st.Replicas}
		if a.Enabled {
			r.Replicas = min(max(r.Replicas, a.MinReplicas), a.MaxReplicas)
		}
		return r, nil
	}, func(c context.Context) error {
		// Scaling into the new range failed: keep the settings that match
		// what is actually running. Runs under opsMu, before the autoscaler
		// can act on the settings being reverted.
		if prev == nil {
			return nil
		}
		return s.Store.SetAutoscale(c, id, prev.Autoscale, prev.MinReplicas, prev.MaxReplicas, prev.TargetCPU)
	})
	if err != nil {
		return nil, err
	}
	if a.Enabled {
		s.event(id, "autoscale", fmt.Sprintf("Autoscaling on: %d–%d replicas, target %d%% CPU", a.MinReplicas, a.MaxReplicas, a.TargetCPU))
	} else {
		s.event(id, "autoscale", fmt.Sprintf("Autoscaling off: staying at %d replicas", st.Replicas))
	}
	return st, nil
}

// event records a line in a site's activity log; failures only get logged.
func (s *Service) event(siteID, kind, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.AddEvent(ctx, siteID, kind, msg); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Warn("recording site event", "site", siteID, "err", err)
	}
}
