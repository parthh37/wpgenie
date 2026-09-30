package site

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Burst is autoscaling as customers see it: a site has a normal size
// (MinReplicas instances) and, while bursting, gets extra instances that
// are paid for in burst minutes: one per minute it runs above its normal
// size, however many extra instances it has. How many it gets is not a
// setting: the load decides (the autoscaler), up to what its plan, the
// database and the server's free memory allow, and none while the server
// as a whole is busy.
//
//   - off: always the normal size;
//   - auto: extra instances only while the load needs them;
//   - on: at least one extra instance, more under load, until BurstUntil
//     (then back to auto) or until turned off.
//
// When an account's minutes run out billing pauses its sites' burst
// (BurstPaused): they go back to their normal size, and burst resumes by
// itself when minutes are added or the month turns.
const (
	BurstOff  = "off"
	BurstAuto = "auto"
	BurstOn   = "on"

	// MaxBurstHours bounds a timed "on".
	MaxBurstHours = 24 * 7
	// Targets used while bursting: the ones experts tuned, else these.
	burstTargetCPU     = 70
	burstTargetWorkers = 80
	// hostBusyCPU: at this share of the server's CPUs in use, sites get no
	// more instances. More containers on a saturated server only share out
	// the same CPUs, and slow down every other site on it.
	hostBusyCPU = 0.85
	// burstMeterInterval is a burst minute.
	burstMeterInterval = time.Minute
)

// BurstInput changes how a site bursts.
type BurstInput struct {
	Mode string `json:"mode"`
	// Hours limits "on" (then it goes back to auto); 0: until turned off.
	Hours int `json:"hours"`
	// Base is the normal size in instances (0: keep it).
	Base int `json:"base"`
	// MaxReplicas caps the instances (0: as many as the server allows).
	MaxReplicas int `json:"max_replicas"`
}

// BurstStatus is a site's burst as the panel shows it.
type BurstStatus struct {
	Mode   string    `json:"mode"`
	Until  time.Time `json:"until,omitzero"`
	Paused bool      `json:"paused"`
	// Base is the normal size, Replicas what runs now and Max the most the
	// site may get (its plan, the database; the server's free memory and
	// load are judged as it scales).
	Base     int  `json:"base"`
	Replicas int  `json:"replicas"`
	Max      int  `json:"max"`
	Bursting bool `json:"bursting"`
	// Minutes are the site's burst minutes this month.
	Minutes int64 `json:"minutes"`
}

// BaseReplicas is a site's normal size.
func BaseReplicas(st *store.Site) int {
	if st.Autoscale {
		return st.MinReplicas
	}
	return st.Replicas
}

// Bursting reports whether a site runs above its normal size now: the
// minutes it is charged for.
func Bursting(st *store.Site) bool {
	return st.Autoscale && st.BurstMode != BurstOff && st.Replicas > st.MinReplicas
}

// burstRange is the replica range the autoscaler keeps a site in, and why
// when that differs from the stored one.
func burstRange(st *store.Site) (lo, hi int, why string) {
	lo, hi = st.MinReplicas, st.MaxReplicas
	switch {
	case st.BurstMode == BurstOff:
	case st.BurstPaused:
		return lo, lo, "burst minutes used up"
	case st.BurstMode == BurstOn:
		return min(lo+1, hi), hi, "burst is on"
	}
	return lo, hi, ""
}

// burstCeiling is the most instances a site of this size may run: the
// server's replica limit, the plan's (planMax, 0: none) and the caller's
// (want, 0: none), lowered until the database connections fit.
func (s *Service) burstCeiling(memoryMB int, cpus float64, planMax, want int) int {
	n := s.Cfg.MaxReplicas
	for _, c := range []int{planMax, want} {
		if c > 0 {
			n = min(n, c)
		}
	}
	for ; n > 1; n-- {
		if s.validateResources(Resources{memoryMB, cpus, n}) == nil {
			break
		}
	}
	return max(n, 1)
}

// SetBurst changes how a site bursts; planMax is the most instances its
// plan allows (0: no limit). Switching on scales it at once.
func (s *Service) SetBurst(ctx context.Context, id string, in BurstInput, planMax int) (*store.Site, error) {
	if !slices.Contains([]string{BurstOff, BurstAuto, BurstOn}, in.Mode) {
		return nil, fmt.Errorf("%w: mode must be off, auto or on", ErrInvalidInput)
	}
	if in.Hours < 0 || in.Hours > MaxBurstHours || (in.Hours > 0 && in.Mode != BurstOn) {
		return nil, fmt.Errorf("%w: hours must be 0-%d, and only with mode on", ErrInvalidInput, MaxBurstHours)
	}
	if in.Base < 0 || in.MaxReplicas < 0 {
		return nil, fmt.Errorf("%w: base and max_replicas can't be negative", ErrInvalidInput)
	}
	var until time.Time
	if in.Hours > 0 {
		until = time.Now().Add(time.Duration(in.Hours) * time.Hour).Truncate(time.Second)
	}
	var prev *store.Site
	st, err := s.scaleWithUndo(ctx, id, func(st *store.Site) (Resources, error) {
		cp := *st
		prev = &cp
		base := BaseReplicas(st)
		if in.Base > 0 {
			base = in.Base
		}
		r := Resources{st.MemoryMB, st.CPUs, base}
		if in.Mode == BurstOff {
			if err := s.Store.SetScaling(ctx, id, false, base, base, st.TargetCPU, st.TargetWorkers, st.TargetResponseMS,
				BurstOff, time.Time{}); err != nil {
				return Resources{}, err
			}
			// What the scale is checked against: no autoscaling range any more.
			st.Autoscale, st.MinReplicas, st.MaxReplicas, st.BurstMode = false, base, base, BurstOff
			return r, nil
		}
		ceiling := s.burstCeiling(st.MemoryMB, st.CPUs, planMax, in.MaxReplicas)
		if base >= ceiling {
			return Resources{}, fmt.Errorf("%w: at %d instance(s) of %s this site can't have more (the most is %d); "+
				"use a smaller normal size to burst", ErrInvalidInput, base, sizeMB(st.MemoryMB), ceiling)
		}
		cpu, workers := st.TargetCPU, st.TargetWorkers
		if cpu == 0 {
			cpu = burstTargetCPU
		}
		if !st.Autoscale && workers == 0 {
			// Coming from a fixed size: also scale sites that wait on slow
			// APIs or queries (busy workers, idle CPU).
			workers = burstTargetWorkers
		}
		if err := s.Store.SetScaling(ctx, id, true, base, ceiling, cpu, workers, st.TargetResponseMS, in.Mode, until); err != nil {
			return Resources{}, err
		}
		st.Autoscale, st.MinReplicas, st.MaxReplicas, st.BurstMode = true, base, ceiling, in.Mode
		lo, hi, _ := burstRange(st)
		r.Replicas = min(max(st.Replicas, lo), hi)
		return r, nil
	}, func(c context.Context) error {
		if prev == nil {
			return nil
		}
		return s.Store.SetScaling(c, id, prev.Autoscale, prev.MinReplicas, prev.MaxReplicas, prev.TargetCPU,
			prev.TargetWorkers, prev.TargetResponseMS, prev.BurstMode, prev.BurstUntil)
	})
	if err != nil {
		return nil, err
	}
	switch {
	case in.Mode == BurstOff:
		s.event(id, "burst", fmt.Sprintf("Burst off: %d instance(s)", st.Replicas))
	case in.Mode == BurstAuto:
		s.event(id, "burst", fmt.Sprintf("Burst automatic: %d instance(s) normally, up to %d under load", st.MinReplicas, st.MaxReplicas))
	case until.IsZero():
		s.event(id, "burst", fmt.Sprintf("Burst on until turned off: up to %d instances", st.MaxReplicas))
	default:
		s.event(id, "burst", fmt.Sprintf("Burst on for %d hour(s), then automatic: up to %d instances", in.Hours, st.MaxReplicas))
	}
	return st, nil
}

// SetBurstPaused pauses a site's burst (its account has no minutes left)
// or resumes it. The autoscaler moves the site into the new range.
func (s *Service) SetBurstPaused(ctx context.Context, id string, paused bool) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if st.BurstPaused == paused {
		return nil
	}
	if err := s.Store.SetBurstPaused(ctx, id, paused); err != nil {
		return err
	}
	if paused {
		s.event(id, "burst", "Burst paused: no burst minutes left. The site stays at its normal size until minutes are added.")
	} else {
		s.event(id, "burst", "Burst resumed: burst minutes are available again")
	}
	return nil
}

// Burst returns a site's burst status.
func (s *Service) Burst(ctx context.Context, st *store.Site, now time.Time) (BurstStatus, error) {
	b := BurstStatus{Mode: st.BurstMode, Until: st.BurstUntil, Paused: st.BurstPaused, Base: BaseReplicas(st),
		Replicas: st.Replicas, Max: BaseReplicas(st), Bursting: Bursting(st)}
	if st.Autoscale {
		b.Max = st.MaxReplicas
	}
	if b.Mode == "" {
		b.Mode = BurstOff
	}
	m, err := s.Store.BurstMinutes(ctx, MonthStart(now), st.ID)
	b.Minutes = m[st.ID]
	return b, err
}

// MonthStart is the start of t's UTC calendar month (burst minutes are
// counted per month, like billing's usage).
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// RunBurst counts burst minutes and ends timed bursts until ctx ends.
func (s *Service) RunBurst(ctx context.Context) {
	t := time.NewTicker(burstMeterInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.burstTick(ctx, now)
		}
	}
}

func (s *Service) burstTick(ctx context.Context, now time.Time) {
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		s.Log.Error("burst: listing sites", "err", err)
		return
	}
	month := MonthStart(now)
	for _, st := range sites {
		if st.Status != store.StatusActive {
			continue
		}
		if Bursting(st) {
			if err := s.Store.AddBurstMinutes(ctx, st.ID, month, 1); err != nil {
				s.Log.Error("burst: counting a minute", "site", st.ID, "err", err)
			}
		}
		if st.BurstMode == BurstOn && !st.BurstUntil.IsZero() && !now.Before(st.BurstUntil) {
			// Back to automatic: the autoscaler sheds the extra instances as
			// the load allows (its usual scale-down window).
			if err := s.Store.SetBurst(ctx, st.ID, BurstAuto, time.Time{}); err != nil {
				s.Log.Error("burst: ending a timed burst", "site", st.ID, "err", err)
				continue
			}
			s.event(st.ID, "burst", "Timed burst ended: back to automatic")
		}
	}
}

// hostCPU measures the share of the server's CPUs in use between two
// readings of /proc/stat.
type hostCPU struct {
	mu          sync.Mutex
	busy, total uint64
}

// sample returns the share in use since the last call, or -1 when unknown
// (the first call, or no /proc: development).
func (h *hostCPU) sample() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return -1
	}
	busy, total, ok := parseProcStat(b)
	if !ok {
		return -1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pb, pt := h.busy, h.total
	h.busy, h.total = busy, total
	if pt == 0 || total <= pt || busy < pb {
		return -1
	}
	return float64(busy-pb) / float64(total-pt)
}

// parseProcStat reads the aggregate "cpu" line: busy is everything but
// idle and iowait.
func parseProcStat(b []byte) (busy, total uint64, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("cpu ")) {
			continue
		}
		var v [10]uint64
		n, _ := fmt.Sscan(string(line[4:]), &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7], &v[8], &v[9])
		if n < 4 {
			return 0, 0, false
		}
		// user nice system idle iowait irq softirq steal guest guest_nice;
		// guest time is already counted in user.
		for i := 0; i < min(n, 8); i++ {
			total += v[i]
		}
		idle := v[3]
		if n > 4 {
			idle += v[4]
		}
		return total - idle, total, true
	}
	return 0, 0, false
}

func sizeMB(mb int) string {
	if mb >= 1024 && mb%1024 == 0 {
		return fmt.Sprintf("%d GB", mb/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}
