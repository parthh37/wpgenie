package store

import (
	"context"
	"testing"
	"time"
)

func TestStoreLogship(t *testing.T) { forEachBackend(t, testLogship) }

func testLogship(t *testing.T, s *Store) {
	ctx := context.Background()
	day := time.Date(2026, 9, 30, 13, 5, 0, 0, time.UTC)

	// Volumes add up per day and kind.
	for _, v := range []LogVolume{
		{Day: day, Kind: "access", Events: 10, Bytes: 1000},
		{Day: day.Add(time.Hour), Kind: "access", Events: 5, Bytes: 500, Dropped: 2},
		{Day: day, Kind: "audit", Events: 1, Bytes: 80},
		{Day: day.AddDate(0, 0, -20), Kind: "access", Events: 99, Bytes: 9900},
	} {
		if err := s.AddLogVolume(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	vols, err := s.LogVolumes(ctx, day.AddDate(0, 0, -13))
	if err != nil {
		t.Fatal(err)
	}
	midnight := day.Truncate(24 * time.Hour)
	if len(vols) != 2 || vols[0] != (LogVolume{Day: midnight, Kind: "access", Events: 15, Bytes: 1500, Dropped: 2}) ||
		vols[1].Kind != "audit" || vols[1].Events != 1 {
		t.Errorf("LogVolumes = %+v", vols)
	}
	if err := s.PruneLogVolumes(ctx, day.AddDate(0, 0, -14)); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.LogVolumes(ctx, time.Time{}); len(all) != 2 {
		t.Errorf("after pruning: %+v", all)
	}

	// Cursors: absent until set, then kept (next to the access log's).
	if _, ok, err := s.LogCursor(ctx, "audit"); ok || err != nil {
		t.Errorf("fresh cursor: %v %v", ok, err)
	}
	for _, v := range []int64{7, 42} {
		if err := s.SetLogCursor(ctx, "audit", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := s.LogCursor(ctx, "audit"); v != 42 || !ok || err != nil {
		t.Errorf("cursor = %d %v %v", v, ok, err)
	}

	// The exporters' tables, read after a cursor, oldest first.
	if _, err := s.LogTableMax(ctx, "users; DROP TABLE x"); err == nil {
		t.Error("LogTableMax accepted any table")
	}
	for i := range 3 {
		if err := s.AddAudit(ctx, AuditEntry{Actor: "alice", IP: "203.0.113.1", Action: "PUT /api/v1/x", Status: 200 + i}); err != nil {
			t.Fatal(err)
		}
	}
	top, err := s.LogTableMax(ctx, "audit_log")
	if err != nil || top < 3 {
		t.Fatalf("LogTableMax = %d, %v", top, err)
	}
	audit, err := s.AuditAfter(ctx, top-2, 10)
	if err != nil || len(audit) != 2 || audit[0].ID != top-1 || audit[1].Status != 202 || audit[0].Actor != "alice" {
		t.Errorf("AuditAfter = %+v, %v", audit, err)
	}
	if more, _ := s.AuditAfter(ctx, top, 10); len(more) != 0 {
		t.Errorf("AuditAfter(top) = %+v", more)
	}

	j1, _ := s.CreateJob(ctx, "sa", "backup", "alice")
	j2, _ := s.CreateJob(ctx, "sb", "restore", "bob")
	jobs, err := s.JobsAfter(ctx, j1-1, 10)
	if err != nil || len(jobs) != 2 || jobs[0].ID != j1 || jobs[1].ID != j2 || jobs[1].Kind != "restore" {
		t.Errorf("JobsAfter = %+v, %v", jobs, err)
	}

	if err := s.AddAccountEvent(ctx, 5, "plan", "Plan changed"); err != nil {
		t.Fatal(err)
	}
	evs, err := s.AccountEventsAfter(ctx, 0, 10)
	if err != nil || len(evs) != 1 || evs[0].AccountID != 5 || evs[0].Message != "Plan changed" || evs[0].Time.IsZero() {
		t.Errorf("AccountEventsAfter = %+v, %v", evs, err)
	}

	if _, err := s.EnqueueMail(ctx, &MailMessage{To: []string{"a@example.com"}, Subject: "Hi", Text: "body", Template: "t"},
		day); err != nil {
		t.Fatal(err)
	}
	mails, err := s.MailAfter(ctx, 0, 10)
	if err != nil || len(mails) != 1 || mails[0].Subject != "Hi" || mails[0].Status != MailPending {
		t.Errorf("MailAfter = %+v, %v", mails, err)
	}
}
