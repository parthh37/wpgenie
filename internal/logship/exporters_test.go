package logship

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Exporters start at the newest row when a type is first shipped, move
// their cursors once rows are in the spool, and pick up where they were
// after a restart.
func TestExporterCursors(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t)
	st := s.Store
	st.AddAudit(ctx, store.AuditEntry{Actor: "before", Action: "old"})
	enable(t, s, nil)

	// First pass: the cursor is set to the newest row; nothing old ships.
	mustf(t, s.export(ctx), "export")
	if got := readSpool(t, s.spool.Dir, TypeAudit); len(got) != 0 {
		t.Fatalf("history shipped: %q", got)
	}
	st.AddAudit(ctx, store.AuditEntry{Actor: "alice", IP: "203.0.113.4", Action: "PUT /api/v1/logs/settings", Status: 200})
	st.AddAccountEvent(ctx, 3, "plan", "Plan changed to Pro")
	mustf(t, s.export(ctx), "export")
	mustf(t, s.export(ctx), "export again") // nothing twice
	audit := readSpool(t, s.spool.Dir, TypeAudit)
	if len(audit) != 1 || !strings.Contains(audit[0], `"actor":"alice"`) {
		t.Fatalf("audit spool: %q", audit)
	}
	// account_events started at 0 rows: its first pass set the cursor, so
	// the event (added after) ships.
	if ev := readSpool(t, s.spool.Dir, TypeAccountEvents); len(ev) != 1 || !strings.Contains(ev[0], `"account_id":3`) {
		t.Fatalf("account events spool: %q", ev)
	}

	// A restart: a new service on the same store and directory.
	s2 := &Service{Store: st, Docker: s.Docker, Rclone: s.Rclone, Log: quietLog(), Cfg: s.Cfg, Server: s.Server}
	mustf(t, s2.Load(ctx), "load")
	st.AddAudit(ctx, store.AuditEntry{Actor: "bob", Action: "DELETE /api/v1/sites/x"})
	mustf(t, s2.export(ctx), "export after restart")
	audit = readSpool(t, s.spool.Dir, TypeAudit)
	if len(audit) != 2 || !strings.Contains(audit[1], `"actor":"bob"`) {
		t.Fatalf("after restart: %q", audit)
	}

	// Turned off: nothing ships and the cursor waits; back on, the gap ships.
	set, _ := s2.Settings(ctx)
	set.Types[TypeAudit] = false
	s2.SetSettings(ctx, set.Redacted())
	st.AddAudit(ctx, store.AuditEntry{Actor: "carol", Action: "x"})
	mustf(t, s2.export(ctx), "export")
	if got := readSpool(t, s.spool.Dir, TypeAudit); len(got) != 2 {
		t.Fatalf("exported while off: %q", got)
	}
	set.Types[TypeAudit] = true
	s2.SetSettings(ctx, set.Redacted())
	mustf(t, s2.export(ctx), "export")
	if got := readSpool(t, s.spool.Dir, TypeAudit); len(got) != 3 {
		t.Fatalf("gap not shipped: %q", got)
	}
}

// A job ships once it's finished; later ones wait behind it (so none is
// skipped), unless it has been running for days.
func TestExporterWaitsForUnfinished(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t)
	enable(t, s, nil)
	mustf(t, s.export(ctx), "export") // cursors at the start
	j1, _ := s.Store.CreateJob(ctx, "s1", "backup", "alice")
	j2, _ := s.Store.CreateJob(ctx, "s1", "restore", "alice")
	s.Store.FinishJob(ctx, j2, store.JobSucceeded, "", "")
	mustf(t, s.export(ctx), "export")
	if got := readSpool(t, s.spool.Dir, TypeJobs); len(got) != 0 {
		t.Fatalf("shipped past a running job: %q", got)
	}
	s.Store.FinishJob(ctx, j1, store.JobFailed, "boom", "")
	mustf(t, s.export(ctx), "export")
	got := readSpool(t, s.spool.Dir, TypeJobs)
	if len(got) != 2 {
		t.Fatalf("jobs: %q", got)
	}
	var first struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	json.Unmarshal([]byte(got[0]), &first)
	if first.ID != j1 || first.Status != store.JobFailed || first.Error != "boom" {
		t.Errorf("first job: %s", got[0])
	}

	// Stuck for days: ships as it is.
	j3, _ := s.Store.CreateJob(ctx, "s1", "clone", "bob")
	s.Now = func() time.Time { return time.Now().Add(unfinishedGrace + time.Hour) }
	mustf(t, s.export(ctx), "export")
	if got := readSpool(t, s.spool.Dir, TypeJobs); len(got) != 3 || !strings.Contains(got[2], `"status":"queued"`) {
		t.Errorf("stuck job: %q (id %d)", got, j3)
	}
}

// E-mails ship without their bodies.
func TestExporterEmailMetadataOnly(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t)
	enable(t, s, nil)
	mustf(t, s.export(ctx), "export")
	m := &store.MailMessage{AccountID: 4, Template: "invoice.paid", To: []string{"c@example.com"}, Subject: "Receipt",
		Text: "PRIVATE BODY", HTML: "<p>PRIVATE BODY</p>"}
	s.Store.EnqueueMail(ctx, m, time.Now())
	m.Status, m.SentAt, m.Attempts = store.MailSent, time.Now(), 1
	s.Store.RecordMail(ctx, m)
	mustf(t, s.export(ctx), "export")
	got := readSpool(t, s.spool.Dir, TypeEmail)
	if len(got) != 1 || strings.Contains(got[0], "PRIVATE") || !strings.Contains(got[0], `"subject":"Receipt"`) ||
		!strings.Contains(got[0], `"status":"sent"`) {
		t.Errorf("email spool: %q", got)
	}
}
