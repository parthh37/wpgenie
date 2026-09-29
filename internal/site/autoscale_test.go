package site

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
)

func TestDesiredReplicas(t *testing.T) {
	p := autoscalePolicy{min: 1, max: 8, target: 0.7}
	for _, c := range []struct {
		current int
		util    float64
		want    int
	}{
		{1, 0.70, 1},  // on target
		{2, 0.75, 2},  // within tolerance: no flapping on small drifts
		{2, 0.85, 3},  // ceil(2 × 0.85/0.7) = ceil(2.43)
		{1, 0.95, 2},  // saturated: the ratio (1.36) under-reports, double
		{3, 1.00, 6},  // saturated: doubling beats ceil(3 × 1.43) = 5
		{5, 1.00, 8},  // capped at max
		{4, 0.20, 2},  // ceil(4 × 0.29) = ceil(1.14)
		{4, 0.00, 1},  // idle: down to min
		{2, 0.629, 2}, // 10% under target: tolerance holds
	} {
		if got := desiredReplicas(c.current, c.util, p); got != c.want {
			t.Errorf("desiredReplicas(%d, %.2f) = %d, want %d", c.current, c.util, got, c.want)
		}
	}
	if got := desiredReplicas(4, 0, autoscalePolicy{min: 2, max: 8, target: 0.7}); got != 2 {
		t.Errorf("min not honoured: %d", got)
	}
}

// feed runs one sample per interval from start and returns the decision
// after the last sample.
func feed(sc *scaler, start time.Time, current int, utils ...float64) (time.Time, int) {
	now := start
	want := current
	for i, u := range utils {
		now = start.Add(time.Duration(i) * autoscaleInterval)
		sc.observe(cpuSample{at: now, util: u, replicas: current})
		want, _ = sc.next(now, current)
	}
	return now, want
}

func TestScalerScalesUpOnSustainedLoadOnly(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	sc := &scaler{policy: autoscalePolicy{1, 4, 0.7}}
	if _, want := feed(sc, t0, 1, 0.99); want != 1 {
		t.Fatal("one hot sample (a cron run) must not scale")
	}
	now, want := feed(sc, t0.Add(autoscaleInterval), 1, 0.99)
	if want != 2 {
		t.Fatalf("two saturated samples: want %d, expected 2", want)
	}

	// Scaled: the next decision waits for the cooldown and fresh samples.
	sc.lastScale = now
	_, want = feed(sc, now.Add(autoscaleInterval), 2, 0.99, 0.99, 0.99)
	if want != 2 {
		t.Fatalf("scaled again %v after the last scale: cooldown ignored", 3*autoscaleInterval)
	}
	_, want = feed(sc, now.Add(scaleUpCooldown), 2, 0.99)
	if want != 4 {
		t.Fatalf("after the cooldown, still saturated: want %d, expected 4", want)
	}
}

func TestScalerScalesDownOnlyAfterStableWindow(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	sc := &scaler{policy: autoscalePolicy{1, 4, 0.7}}
	quiet := make([]float64, int(scaleDownWindow/autoscaleInterval))
	for i := range quiet {
		quiet[i] = 0.1
	}
	if _, want := feed(sc, t0, 4, quiet...); want != 4 {
		t.Fatal("scaled down before a full window of samples existed (e.g. right after a daemon restart)")
	}
	now, want := feed(sc, t0.Add(time.Duration(len(quiet))*autoscaleInterval), 4, 0.1, 0.1)
	if want != 1 {
		t.Fatalf("quiet for the whole window: want %d, expected 1", want)
	}

	// A burst anywhere in the window holds the capacity it needed.
	sc = &scaler{policy: autoscalePolicy{1, 4, 0.7}, since: now.Add(-time.Hour)}
	burst := slices.Clone(quiet)
	burst[len(burst)/2] = 0.95 // 4 replicas saturated once, 2.5 minutes ago
	if _, want := feed(sc, now, 4, burst...); want != 4 {
		t.Fatalf("a burst %v ago must hold 4 replicas, got %d", scaleDownWindow/2, want)
	}
}

func TestScalerClampsIntoRange(t *testing.T) {
	sc := &scaler{policy: autoscalePolicy{2, 4, 0.7}}
	if want, why := sc.next(time.Now(), 1); want != 2 || why == "" {
		t.Fatalf("below min: %d %q", want, why)
	}
	if want, _ := sc.next(time.Now(), 6); want != 4 {
		t.Fatalf("above max: %d", want)
	}
}

func TestSiteUtilizationIgnoresRetiringReplicas(t *testing.T) {
	h := newHarness(t)
	st, _ := h.svc.Store.GetSite(context.Background(), "s1")
	st.CPUs, st.Upstreams = 2, []int{19000, 19001}
	usage := map[string]float64{
		runtime.ContainerName("s1", 19000): 150, // 75% of 2 CPUs
		runtime.ContainerName("s1", 19001): 50,  // 25%
		runtime.ContainerName("s1", 19005): 200, // draining, not serving
		runtime.ContainerName("s2", 19002): 200, // another site
	}
	util, n := siteUtilization(st, usage)
	if n != 2 || util != 0.5 {
		t.Fatalf("siteUtilization = %v over %d replicas, want 0.5 over 2", util, n)
	}
}

func TestAutoscalerTickScalesOutAndLogs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{Enabled: true, MinReplicas: 1, MaxReplicas: 3, TargetCPU: 60}); err != nil {
		t.Fatal(err)
	}
	h.rt.cpu = map[string]float64{
		runtime.ContainerName("s1", 19000): 100, // saturated at 1 CPU
		runtime.ContainerName("s1", 19001): 100,
	}
	scalers := map[string]*scaler{}
	t0 := time.Now()
	h.svc.autoscaleTick(ctx, t0, scalers)
	h.svc.autoscaleTick(ctx, t0.Add(autoscaleInterval), scalers)
	waitIdle(t, scalers["s1"])

	if got := h.upstreams(t); !slices.Equal(got, []int{19000, 19001}) {
		t.Fatalf("upstreams after scale-out = %v", got)
	}
	if r, ok := h.svc.CPU("s1"); !ok || r.Percent != 100 {
		t.Errorf("CPU reading = %+v, %v", r, ok)
	}
	events, _ := h.svc.Store.Events(ctx, "s1", 10)
	if len(events) < 2 || !strings.HasPrefix(events[0].Message, "Scaled out 1 → 2 replicas: CPU at 100%") {
		t.Fatalf("events = %+v", events)
	}

	// Manual scaling now has to respect the range.
	if _, err := h.svc.Scale(ctx, "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 5}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("manual scale outside the autoscaling range: %v", err)
	}
}

func TestAutoscalerNeverRevertsAConcurrentResize(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{Enabled: true, MinReplicas: 1, MaxReplicas: 3, TargetCPU: 60}); err != nil {
		t.Fatal(err)
	}
	// Decided 1 → 2 at 512 MB, but the owner went to 1 GB meanwhile.
	if _, err := h.svc.Scale(ctx, "s1", Resources{MemoryMB: 1024, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	h.svc.autoscaleTo(ctx, "s1", 1, 2, "test")
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	if st.MemoryMB != 1024 || st.Replicas != 2 {
		t.Fatalf("autoscaler reverted a manual resize: %d MB × %d", st.MemoryMB, st.Replicas)
	}
	// A decision taken for a replica count that no longer holds is dropped.
	h.svc.autoscaleTo(ctx, "s1", 1, 3, "stale")
	if st, _ := h.svc.Store.GetSite(ctx, "s1"); st.Replicas != 2 {
		t.Fatalf("stale decision applied: %d replicas", st.Replicas)
	}
}

func TestSetAutoscaleValidatesAndClamps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, bad := range []AutoscaleSettings{
		{Enabled: true, MinReplicas: 0, MaxReplicas: 2, TargetCPU: 70},
		{Enabled: true, MinReplicas: 3, MaxReplicas: 2, TargetCPU: 70},
		{Enabled: true, MinReplicas: 1, MaxReplicas: 99, TargetCPU: 70},
		{Enabled: true, MinReplicas: 1, MaxReplicas: 2, TargetCPU: 5},
	} {
		if _, err := h.svc.SetAutoscale(ctx, "s1", bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("SetAutoscale(%+v) = %v, want invalid input", bad, err)
		}
	}
	// A range above the current count scales in immediately.
	st, err := h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{Enabled: true, MinReplicas: 2, MaxReplicas: 4, TargetCPU: 70})
	if err != nil {
		t.Fatal(err)
	}
	if st.Replicas != 2 || !st.Autoscale || len(st.Upstreams) != 2 {
		t.Fatalf("site after enabling with min 2: %+v", st)
	}

	// If scaling into range fails, the old settings are kept.
	h.rt.failStart = h.rt.starts + 1
	if _, err := h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{Enabled: true, MinReplicas: 3, MaxReplicas: 4, TargetCPU: 70}); err == nil {
		t.Fatal("expected the failed start to surface")
	}
	if st, _ := h.svc.Store.GetSite(ctx, "s1"); st.MinReplicas != 2 || st.Replicas != 2 {
		t.Fatalf("settings after a failed clamp: min %d, %d replicas", st.MinReplicas, st.Replicas)
	}
}

// Draining a replica can take up to two minutes; it must not block other
// sites (or the autoscaler) from scaling meanwhile.
func TestDrainDoesNotHoldTheOpsLock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Scale(ctx, "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	h.rt.busy[runtime.ContainerName("s1", 19001)] = 3
	locked := false
	h.rt.onBusy = func() {
		if h.svc.opsMu.TryLock() {
			h.svc.opsMu.Unlock()
		} else {
			locked = true
		}
	}
	if _, err := h.svc.Scale(ctx, "s1", Resources{MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*h.log, "busy "+runtime.ContainerName("s1", 19001)) {
		t.Fatalf("the busy replica was never drained: %v", *h.log)
	}
	if locked {
		t.Fatal("opsMu was held while draining")
	}
}

func waitIdle(t *testing.T, sc *scaler) {
	t.Helper()
	if sc == nil {
		t.Fatal("no scaler: the site was never considered")
	}
	deadline := time.Now().Add(5 * time.Second)
	for sc.busy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("scale action did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
