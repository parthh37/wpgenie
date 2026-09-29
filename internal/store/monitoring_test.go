package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestAlertsAndBoundedHistory(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1_800_000_000, 0).UTC()
	a := Alert{Key: "disk:/data", Kind: "disk", Target: "/data", Severity: "warning", State: AlertFiring,
		Message: "90% full", Since: now, UpdatedAt: now}
	if err := st.PutAlert(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if err := st.PutAlert(ctx, Alert{Key: "certificate:a.test", Kind: "certificate", Target: "a.test",
		State: AlertResolved, Since: now, UpdatedAt: now}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.AlertsNotified(ctx, []string{a.Key}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Alerts(ctx)
	if err != nil || len(rows) != 2 || rows[0].Key != a.Key || !rows[0].NotifiedAt.Equal(now.Add(time.Minute)) ||
		!rows[1].NotifiedAt.IsZero() {
		t.Fatalf("alerts %+v %v", rows, err)
	}
	for i := range keepAlertHistory + 5 {
		a.Message = fmt.Sprint(i)
		if err := st.PutAlert(ctx, a, true); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := st.AlertHistory(ctx, 5000)
	if err != nil || len(hist) != keepAlertHistory || hist[0].Message != fmt.Sprint(keepAlertHistory+4) {
		t.Fatalf("history: %d lines, newest %+v, %v", len(hist), hist[0], err)
	}
	if err := st.DeleteAlert(ctx, a.Key); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.Alerts(ctx); len(rows) != 1 {
		t.Errorf("after delete: %+v", rows)
	}

	id, _ := st.CreateJob(ctx, "", "backup", "x")
	st.CreateJob(ctx, "", "backup", "x")
	st.FinishJob(ctx, id, JobSucceeded, "", "")
	if n, err := st.JobCounts(ctx); err != nil || n[JobQueued] != 1 || n[JobSucceeded] != 1 {
		t.Errorf("job counts %v %v", n, err)
	}
}
