package site

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
)

func TestSetBurstModes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, bad := range []BurstInput{{Mode: "turbo"}, {Mode: BurstAuto, Hours: 2}, {Mode: BurstOn, Hours: -1},
		{Mode: BurstOn, Hours: MaxBurstHours + 1}, {Mode: BurstAuto, Base: -1}} {
		if _, err := h.svc.SetBurst(ctx, "s1", bad, 0); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("SetBurst(%+v) = %v", bad, err)
		}
	}
	// A plan that allows one instance leaves nothing to burst into.
	if _, err := h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstAuto}, 1); !errors.Is(err, ErrInvalidInput) ||
		!strings.Contains(err.Error(), "can't have more") {
		t.Fatalf("no room: %v", err)
	}

	// Automatic: the normal size stays, the most comes from the plan.
	st, err := h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstAuto}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Autoscale || st.BurstMode != BurstAuto || st.MinReplicas != 1 || st.MaxReplicas != 4 || st.Replicas != 1 ||
		st.TargetCPU != 70 || st.TargetWorkers != burstTargetWorkers {
		t.Fatalf("auto: %+v", st)
	}
	if Bursting(st) {
		t.Fatal("at its normal size, a site isn't bursting")
	}

	// On: an extra instance at once, for the hours asked.
	before := time.Now()
	if st, err = h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstOn, Hours: 3}, 4); err != nil {
		t.Fatal(err)
	}
	if st.Replicas != 2 || st.BurstMode != BurstOn || st.BurstUntil.Before(before.Add(3*time.Hour-time.Second)) || !Bursting(st) {
		t.Fatalf("on: %+v", st)
	}
	if b, _ := h.svc.Burst(ctx, st, time.Now()); !b.Bursting || b.Base != 1 || b.Max != 4 || b.Replicas != 2 || b.Mode != BurstOn {
		t.Fatalf("status: %+v", b)
	}

	// Off: back to the normal size, no autoscaling.
	if st, err = h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstOff}, 4); err != nil {
		t.Fatal(err)
	}
	if st.Autoscale || st.BurstMode != BurstOff || st.Replicas != 1 || !st.BurstUntil.IsZero() {
		t.Fatalf("off: %+v", st)
	}
	// A bigger normal size while off.
	if st, err = h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstOff, Base: 2}, 4); err != nil || st.Replicas != 2 {
		t.Fatalf("base 2: %+v %v", st, err)
	}
	// Off with a normal size outside the automatic range it had.
	if _, err = h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstAuto, Base: 2}, 4); err != nil {
		t.Fatal(err)
	}
	if st, err = h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstOff, Base: 1}, 4); err != nil || st.Replicas != 1 || st.Autoscale {
		t.Fatalf("off at 1 from auto 2-4: %+v %v", st, err)
	}
	events, _ := h.svc.Store.Events(ctx, "s1", 10)
	if len(events) == 0 || !strings.HasPrefix(events[0].Message, "Burst off") {
		t.Fatalf("events: %+v", events)
	}
}

func TestBurstPauseAndMeter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstOn}, 3); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	h.svc.burstTick(ctx, now)
	h.svc.burstTick(ctx, now.Add(time.Minute))
	if m, _ := h.svc.Store.BurstMinutes(ctx, MonthStart(now)); m["s1"] != 2 {
		t.Fatalf("minutes %v", m)
	}

	// Paused: the autoscaler takes it back to its normal size, and the
	// minutes stop.
	if err := h.svc.SetBurstPaused(ctx, "s1", true); err != nil {
		t.Fatal(err)
	}
	scalers := map[string]*scaler{}
	h.rt.cpu = map[string]float64{runtime.ContainerName("s1", 19000): 10, runtime.ContainerName("s1", 19001): 10}
	h.svc.autoscaleTick(ctx, now, scalers)
	waitIdle(t, scalers["s1"])
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	if st.Replicas != 1 || Bursting(st) {
		t.Fatalf("paused: %d replicas", st.Replicas)
	}
	events, _ := h.svc.Store.Events(ctx, "s1", 5)
	if !strings.Contains(events[0].Message, "burst minutes used up") {
		t.Fatalf("events: %+v", events)
	}
	h.svc.burstTick(ctx, now.Add(2*time.Minute))
	if m, _ := h.svc.Store.BurstMinutes(ctx, MonthStart(now)); m["s1"] != 2 {
		t.Fatalf("minutes counted while paused: %v", m)
	}
	// Pausing twice is a no-op; resuming brings the extra instance back.
	h.svc.SetBurstPaused(ctx, "s1", true)
	h.svc.SetBurstPaused(ctx, "s1", false)
	h.rt.cpu = map[string]float64{runtime.ContainerName("s1", 19000): 10}
	h.svc.autoscaleTick(ctx, now.Add(time.Minute), scalers)
	waitIdle(t, scalers["s1"])
	if st, _ := h.svc.Store.GetSite(ctx, "s1"); st.Replicas != 2 {
		t.Fatalf("resumed: %d replicas", st.Replicas)
	}
}

func TestTimedBurstEnds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, err := h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstOn, Hours: 1}, 3)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.burstTick(ctx, st.BurstUntil.Add(-time.Minute))
	if st, _ := h.svc.Store.GetSite(ctx, "s1"); st.BurstMode != BurstOn {
		t.Fatalf("ended early: %+v", st)
	}
	h.svc.burstTick(ctx, st.BurstUntil)
	st, _ = h.svc.Store.GetSite(ctx, "s1")
	if st.BurstMode != BurstAuto || !st.BurstUntil.IsZero() || !st.Autoscale {
		t.Fatalf("after the hour: %+v", st)
	}
}

// A busy server holds scale-outs, but never the normal size.
func TestBusyServerHoldsBurst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	load := 0.95
	h.svc.HostLoad = func() float64 { return load }
	if _, err := h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstAuto}, 3); err != nil {
		t.Fatal(err)
	}
	h.rt.cpu = map[string]float64{runtime.ContainerName("s1", 19000): 100}
	scalers := map[string]*scaler{}
	t0 := time.Now()
	h.svc.autoscaleTick(ctx, t0, scalers)
	h.svc.autoscaleTick(ctx, t0.Add(autoscaleInterval), scalers)
	waitIdle(t, scalers["s1"])
	if st, _ := h.svc.Store.GetSite(ctx, "s1"); st.Replicas != 1 {
		t.Fatalf("scaled out on a busy server: %d", st.Replicas)
	}
	events, _ := h.svc.Store.Events(ctx, "s1", 5)
	if !strings.Contains(events[0].Message, "the server is busy (95%") {
		t.Fatalf("events: %+v", events)
	}
	load = 0.4
	h.svc.autoscaleTick(ctx, t0.Add(2*autoscaleInterval), scalers)
	waitIdle(t, scalers["s1"])
	if st, _ := h.svc.Store.GetSite(ctx, "s1"); st.Replicas != 2 {
		t.Fatalf("not scaled out once the server had room: %d", st.Replicas)
	}
}

// Autoscaling through the older API is automatic burst underneath.
func TestAutoscaleIsBurst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, err := h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{Enabled: true, MinReplicas: 1, MaxReplicas: 3, TargetCPU: 60})
	if err != nil || st.BurstMode != BurstAuto {
		t.Fatalf("%+v %v", st, err)
	}
	h.svc.SetBurst(ctx, "s1", BurstInput{Mode: BurstOn}, 3)
	if st, _ = h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{Enabled: true, MinReplicas: 1, MaxReplicas: 3, TargetCPU: 60}); st.BurstMode != BurstOn {
		t.Fatalf("retuning autoscaling switched burst off: %+v", st)
	}
	if st, _ = h.svc.SetAutoscale(ctx, "s1", AutoscaleSettings{MinReplicas: 1, MaxReplicas: 3, TargetCPU: 60}); st.BurstMode != BurstOff {
		t.Fatalf("autoscaling off: %+v", st)
	}
}

func TestBurstCeilingFitsTheDatabase(t *testing.T) {
	h := newHarness(t)
	h.svc.Cfg.MaxReplicas = 8
	// One site may use half the connections: exactly 3 instances' workers.
	h.svc.Cfg.DBMaxConnections = 2 * dbConnLimit(3, runtime.FPMMaxChildren(512))
	n := h.svc.burstCeiling(512, 1, 0, 0)
	if n != 3 {
		t.Fatalf("ceiling %d, want 3", n)
	}
	if got := h.svc.burstCeiling(512, 1, 2, 0); got != min(2, n) {
		t.Fatalf("plan cap: %d", got)
	}
	if got := h.svc.burstCeiling(512, 1, 0, 1); got != 1 {
		t.Fatalf("caller cap: %d", got)
	}
}

func TestParseProcStat(t *testing.T) {
	busy, total, ok := parseProcStat([]byte("cpu  100 5 50 800 45 0 0 0 0 0\ncpu0 1 2 3 4\nintr 1\n"))
	if !ok || total != 1000 || busy != 155 {
		t.Fatalf("%d/%d %v", busy, total, ok)
	}
	if _, _, ok := parseProcStat([]byte("intr 1\n")); ok {
		t.Fatal("no cpu line")
	}
	var h hostCPU
	if h.sample(); h.total > 0 && h.sample() > 1 {
		t.Fatal("more than all CPUs in use")
	}
}
