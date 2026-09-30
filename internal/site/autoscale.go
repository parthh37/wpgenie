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

// Autoscaling. Every autoscaleInterval the daemon samples each serving
// replica's CPU use as a fraction of its allowance (0.7 = 70% of the CPUs it
// was given) and, optionally, how busy its PHP workers are (requests being
// served or queued, per worker) and the site's recent PHP response times.
// Each metric proposes a replica count and the highest wins (Kubernetes
// HPA's rule for several metrics); replicas are added or removed through
// the same blue/green reconcile as a manual scale, so autoscaling is
// exactly as zero-downtime.
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
	// Workers: percent of PHP workers busy, queued requests included (so
	// it can pass 100).
	MinTargetWorkers = 20
	MaxTargetWorkers = 100
	MinTargetMS      = 50
	MaxTargetMS      = 30000
	// Response times only count with enough of them to judge by...
	minLatencySamples = 20
	// ...and when the site is busy: a slow page on idle workers is slow
	// code or a slow database, which more replicas can't fix.
	latencyPressure = 0.5
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
	// TargetWorkers (percent of PHP workers busy, queued requests included)
	// catches sites that wait rather than compute (external APIs, slow
	// queries): their CPU stays low while requests queue. 0: off.
	TargetWorkers int `json:"target_workers"`
	// TargetResponseMS adds a replica while the 95th percentile of PHP
	// response times stays above it under load. 0: off.
	TargetResponseMS int `json:"target_response_ms"`
}

type autoscalePolicy struct {
	min, max int
	target   float64 // CPU, fraction, e.g. 0.7
	workers  float64 // fraction; 0: off
	ms       float64 // 0: off
}

// hpa is the HPA rule for one metric: the replica count that would bring
// its average back to the target, assuming load spreads evenly.
func hpa(current int, util, target float64) int {
	if ratio := util / target; math.Abs(ratio-1) > scaleTolerance {
		return int(math.Ceil(float64(current)*ratio - 1e-9))
	}
	return current
}

// desiredReplicas is the HPA rule: the replica count that would bring the
// average CPU use back to the target, assuming load spreads evenly.
func desiredReplicas(current int, util float64, p autoscalePolicy) int {
	want := hpa(current, util, p.target)
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
	// workers is the PHP workers' load (requests per worker, queued ones
	// included), -1 when unknown; p95 the response times' 95th percentile
	// over the scale-up window, from n responses.
	workers float64
	p95     float64
	n       int
}

// desired is the replica count one sample asks for, over every metric the
// policy uses, and which metric asked for the most.
func (p autoscalePolicy) desired(current int, s cpuSample) (int, string) {
	want, why := desiredReplicas(current, s.util, p), "cpu"
	if p.workers > 0 && s.workers >= 0 {
		if w := min(max(hpa(current, s.workers, p.workers), p.min), p.max); w > want {
			want, why = w, "workers"
		}
	}
	busy := max(s.util, s.workers)
	if p.ms > 0 && s.n >= minLatencySamples && s.p95 > p.ms*(1+scaleTolerance) && busy >= latencyPressure {
		if w := min(current+1, p.max); w > want {
			want, why = w, "latency"
		}
	}
	return want, why
}

func (p autoscalePolicy) reason(why string, s cpuSample) string {
	switch why {
	case "workers":
		return fmt.Sprintf("PHP workers %.0f%% busy, queued requests included (target %.0f%%)", s.workers*100, p.workers*100)
	case "latency":
		return fmt.Sprintf("95%% of PHP responses took up to %.0f ms (target %.0f ms) with the site %.0f%% busy", s.p95, p.ms, max(s.util, s.workers)*100)
	}
	return fmt.Sprintf("CPU at %.0f%% of each replica's allowance (target %.0f%%)", s.util*100, p.target*100)
}

// scaler is one site's autoscaling history. Only the autoscaler loop
// touches it, except busy, which a running scale action clears.
type scaler struct {
	policy    autoscalePolicy
	samples   []cpuSample // newest last, within scaleDownWindow
	since     time.Time   // first sample under the current policy
	lastScale time.Time
	memWarned time.Time
	cpuWarned time.Time
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

const outsideRange = "outside the autoscaling range"

// next returns the replica count the site should run now, and why.
func (sc *scaler) next(now time.Time, current int) (int, string) {
	p := sc.policy
	if current < p.min || current > p.max {
		return min(max(current, p.min), p.max), outsideRange
	}
	// Up: the recent average, only counting samples taken since the last
	// scale (earlier ones measured a different replica count). Response
	// times are already a percentile over that window: the newest sample's.
	var avg cpuSample
	var n, nw int
	for _, s := range sc.samples {
		if now.Sub(s.at) <= scaleUpWindow && s.at.After(sc.lastScale) {
			avg.util += s.util
			n++
			if s.workers >= 0 {
				avg.workers += s.workers
				nw++
			}
			avg.p95, avg.n = s.p95, s.n
		}
	}
	if n >= 2 && now.Sub(sc.lastScale) >= scaleUpCooldown {
		avg.util /= float64(n)
		if nw > 0 {
			avg.workers /= float64(nw)
		} else {
			avg.workers = -1
		}
		if want, why := p.desired(current, avg); want > current {
			return want, p.reason(why, avg)
		}
	}
	// Down: every sample in the window must agree the site is over-provisioned.
	if now.Sub(sc.since) < scaleDownWindow || now.Sub(sc.lastScale) < scaleDownWindow {
		return current, ""
	}
	stable, peak, peakWorkers := p.min, 0.0, -1.0
	for _, s := range sc.samples {
		want, _ := p.desired(s.replicas, s)
		stable = max(stable, want)
		peak, peakWorkers = max(peak, s.util), max(peakWorkers, s.workers)
	}
	if stable < current {
		why := fmt.Sprintf("CPU peaked at %.0f%% over the last %d minutes (target %.0f%%)",
			peak*100, int(scaleDownWindow.Minutes()), p.target*100)
		if p.workers > 0 && peakWorkers >= 0 {
			why += fmt.Sprintf(", PHP workers at %.0f%% (target %.0f%%)", peakWorkers*100, p.workers*100)
		}
		return stable, why
	}
	return current, ""
}

// CPUReading is a site's latest measured load: CPU use and, when known,
// its PHP workers and response times.
type CPUReading struct {
	At       time.Time `json:"at"`
	Percent  float64   `json:"percent"` // average use in % of each replica's allowance
	Replicas int       `json:"replicas"`
	// Workers is the percent of PHP workers busy, queued requests included
	// (over 100: requests wait); Queued the requests waiting right now.
	Workers *float64 `json:"workers_percent,omitempty"`
	Queued  int      `json:"queued"`
	// P95MS is the 95th percentile of PHP response times over the last
	// minute, from Responses responses.
	P95MS     *float64 `json:"p95_ms,omitempty"`
	Responses int      `json:"responses"`
}

// LatencySource reports a site's recent PHP response times
// (analytics.Recent).
type LatencySource interface {
	Percentile(site string, since time.Time, p float64) (float64, int)
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
	load, err := s.Runtime.FPMLoad(ctx)
	if err != nil {
		s.Log.Warn("autoscaler: sampling PHP workers", "err", err) // CPU alone still works
	}
	hostBusy := s.hostLoad()
	for _, st := range active {
		util, n := siteUtilization(st, usage)
		if n == 0 {
			continue
		}
		reading := CPUReading{At: now, Percent: math.Round(util * 100), Replicas: n}
		workers, queued, ok := siteWorkers(st, load)
		if !ok {
			workers = -1
		} else {
			w := math.Round(workers * 100)
			reading.Workers, reading.Queued = &w, queued
		}
		sc := scalers[st.ID]
		since := now.Add(-scaleUpWindow)
		if sc != nil && sc.lastScale.After(since) {
			since = sc.lastScale // earlier responses came from another replica count
		}
		var p95 float64
		var responses int
		if s.Latency != nil {
			p95, responses = s.Latency.Percentile(st.ID, since, 0.95)
			if last, m := s.Latency.Percentile(st.ID, now.Add(-time.Minute), 0.95); m > 0 {
				reading.P95MS, reading.Responses = &last, m
			}
		}
		s.cpu.Store(st.ID, reading)
		if !st.Autoscale {
			continue
		}
		lo, hi, rangeWhy := burstRange(st)
		p := autoscalePolicy{lo, hi, float64(st.TargetCPU) / 100,
			float64(st.TargetWorkers) / 100, float64(st.TargetResponseMS)}
		if sc == nil || sc.policy != p {
			sc = &scaler{policy: p} // new settings: judge them on fresh data
			scalers[st.ID] = sc
		}
		if sc.busy.Load() {
			continue // mid-scale: the replica set is changing under the sample
		}
		sc.observe(cpuSample{at: now, util: util, replicas: n, workers: workers, p95: p95, n: responses})
		want, why := sc.next(now, st.Replicas)
		if why == outsideRange && rangeWhy != "" {
			why = rangeWhy
		}
		if floor := max(st.Replicas, st.MinReplicas); want > floor && hostBusy >= hostBusyCPU {
			// The server is busy as a whole: more instances would only share
			// out the same CPUs. The normal size is always kept.
			if now.Sub(sc.cpuWarned) > 30*time.Minute {
				sc.cpuWarned = now
				s.event(st.ID, "autoscale", fmt.Sprintf("Wanted %d instances but the server is busy (%.0f%% of its CPU in use); "+
					"extra instances wait until it has room", want, hostBusy*100))
			}
			want = floor
		}
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

// siteWorkers is the load on the PHP workers of the replicas serving the
// site: requests (served or queued) per worker, and how many are queued.
func siteWorkers(st *store.Site, load map[string]runtime.FPMLoad) (float64, int, bool) {
	workers := runtime.FPMMaxChildren(st.MemoryMB)
	var reqs, queued, n int
	for _, port := range st.Upstreams {
		if l, ok := load[runtime.ContainerName(st.ID, port)]; ok {
			reqs += l.Requests
			queued += l.Queued
			n++
		}
	}
	if n == 0 || workers <= 0 {
		return 0, 0, false
	}
	return float64(reqs) / float64(n*workers), queued, true
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

// hostLoad is the share of the server's CPUs in use since the last tick
// (-1: unknown).
func (s *Service) hostLoad() float64 {
	if s.HostLoad != nil {
		return s.HostLoad()
	}
	return s.hostCPU.sample()
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
	if a.TargetWorkers != 0 && (a.TargetWorkers < MinTargetWorkers || a.TargetWorkers > MaxTargetWorkers) {
		return nil, fmt.Errorf("%w: target_workers must be 0 (off) or between %d and %d (%%)", ErrInvalidInput, MinTargetWorkers, MaxTargetWorkers)
	}
	if a.TargetResponseMS != 0 && (a.TargetResponseMS < MinTargetMS || a.TargetResponseMS > MaxTargetMS) {
		return nil, fmt.Errorf("%w: target_response_ms must be 0 (off) or between %d and %d", ErrInvalidInput, MinTargetMS, MaxTargetMS)
	}
	var prev *store.Site
	st, err := s.scaleWithUndo(ctx, id, func(st *store.Site) (Resources, error) {
		if a.Enabled {
			// The autoscaler may go up to the maximum at the current size.
			if err := s.validateResources(Resources{st.MemoryMB, st.CPUs, a.MaxReplicas}); err != nil {
				return Resources{}, fmt.Errorf("at %d replicas: %w", a.MaxReplicas, err)
			}
		}
		cp := *st
		prev = &cp
		// Autoscaling is burst underneath: on keeps a burst that is on,
		// else it is automatic.
		mode, until := BurstOff, time.Time{}
		if a.Enabled {
			mode = BurstAuto
			if st.BurstMode == BurstOn {
				mode, until = BurstOn, st.BurstUntil
			}
		}
		if err := s.Store.SetScaling(ctx, id, a.Enabled, a.MinReplicas, a.MaxReplicas, a.TargetCPU, a.TargetWorkers,
			a.TargetResponseMS, mode, until); err != nil {
			return Resources{}, err
		}
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
		return s.Store.SetScaling(c, id, prev.Autoscale, prev.MinReplicas, prev.MaxReplicas, prev.TargetCPU,
			prev.TargetWorkers, prev.TargetResponseMS, prev.BurstMode, prev.BurstUntil)
	})
	if err != nil {
		return nil, err
	}
	if a.Enabled {
		targets := fmt.Sprintf("%d%% CPU", a.TargetCPU)
		if a.TargetWorkers > 0 {
			targets += fmt.Sprintf(", %d%% PHP workers busy", a.TargetWorkers)
		}
		if a.TargetResponseMS > 0 {
			targets += fmt.Sprintf(", %d ms (95th percentile)", a.TargetResponseMS)
		}
		s.event(id, "autoscale", fmt.Sprintf("Autoscaling on: %d–%d replicas, targets %s", a.MinReplicas, a.MaxReplicas, targets))
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
