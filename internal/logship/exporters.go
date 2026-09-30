package logship

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// The exporters ship rows of the daemon's own tables: the audit log, jobs,
// accounts' activity and the e-mail outbox (metadata only: no bodies).
// Each keeps a cursor (the last row ID shipped) in the store, moved only
// once the rows are on disk in the spool, so a restart neither loses nor
// repeats any. A type turned on for the first time starts from the newest
// row: the archive begins when shipping does.

const (
	exportBatch = 500
	// exportPasses bounds the batches one pass takes per type (a backlog
	// after a long pause drains over a few passes).
	exportPasses = 20
	// unfinishedGrace: a job or e-mail still unfinished after this long
	// ships as it is rather than holding back everything after it.
	unfinishedGrace = 48 * time.Hour
)

// exportRow is a row to ship; ready is false for a row that may still
// change (a running job, an e-mail being retried).
type exportRow struct {
	id    int64
	at    time.Time
	ready bool
	v     any
}

type exporter struct {
	typ, table string
	fetch      func(ctx context.Context, st *store.Store, after int64) ([]exportRow, error)
}

var exporters = []exporter{
	{TypeAudit, "audit_log", func(ctx context.Context, st *store.Store, after int64) ([]exportRow, error) {
		rows, err := st.AuditAfter(ctx, after, exportBatch)
		out := make([]exportRow, len(rows))
		for i, e := range rows {
			out[i] = exportRow{e.ID, e.Time, true, e}
		}
		return out, err
	}},
	{TypeJobs, "jobs", func(ctx context.Context, st *store.Store, after int64) ([]exportRow, error) {
		rows, err := st.JobsAfter(ctx, after, exportBatch)
		out := make([]exportRow, len(rows))
		for i, j := range rows {
			done := j.Status == store.JobSucceeded || j.Status == store.JobFailed
			out[i] = exportRow{j.ID, j.CreatedAt, done, j}
		}
		return out, err
	}},
	{TypeAccountEvents, "account_events", func(ctx context.Context, st *store.Store, after int64) ([]exportRow, error) {
		rows, err := st.AccountEventsAfter(ctx, after, exportBatch)
		out := make([]exportRow, len(rows))
		for i, e := range rows {
			out[i] = exportRow{e.ID, e.Time, true, e}
		}
		return out, err
	}},
	{TypeEmail, "mail_outbox", func(ctx context.Context, st *store.Store, after int64) ([]exportRow, error) {
		rows, err := st.MailAfter(ctx, after, exportBatch)
		out := make([]exportRow, len(rows))
		for i, m := range rows {
			out[i] = exportRow{m.ID, m.CreatedAt, m.Status != store.MailPending, emailEntry{
				ID: m.ID, Time: m.CreatedAt, AccountID: m.AccountID, Template: m.Template, To: m.To, Subject: m.Subject,
				Status: m.Status, Attempts: m.Attempts, LastError: m.LastError, SentAt: m.SentAt}}
		}
		return out, err
	}},
}

// emailEntry is an outbox message as shipped: who, what about, and how
// delivery went.
type emailEntry struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	AccountID int64     `json:"account_id,omitempty"`
	Template  string    `json:"template,omitempty"`
	To        []string  `json:"to"`
	Subject   string    `json:"subject"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	SentAt    time.Time `json:"sent_at,omitzero"`
}

// export runs every exporter whose type ships.
func (s *Service) export(ctx context.Context) error {
	var errs []error
	for _, ex := range exporters {
		if !s.spool.Accepts(ex.typ) {
			continue
		}
		if err := s.exportOne(ctx, ex); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) exportOne(ctx context.Context, ex exporter) error {
	cursor, ok, err := s.Store.LogCursor(ctx, ex.typ)
	if err != nil {
		return err
	}
	if !ok {
		top, err := s.Store.LogTableMax(ctx, ex.table)
		if err != nil {
			return err
		}
		return s.Store.SetLogCursor(ctx, ex.typ, top)
	}
	now := s.now()
	for range exportPasses {
		rows, err := ex.fetch(ctx, s.Store, cursor)
		if err != nil {
			return err
		}
		var lines [][]byte
		last := cursor
		for _, r := range rows {
			if !r.ready && now.Sub(r.at) < unfinishedGrace {
				break // it (and everything after it) waits for the next pass
			}
			b, err := json.Marshal(r.v)
			if err != nil {
				return err
			}
			lines = append(lines, b)
			last = r.id
		}
		if len(lines) > 0 {
			if err := s.spool.WriteSync(ex.typ, lines); err != nil {
				return err
			}
			if err := s.Store.SetLogCursor(ctx, ex.typ, last); err != nil {
				return err
			}
			cursor = last
		}
		if len(rows) < exportBatch || len(lines) < len(rows) {
			return nil
		}
	}
	return nil
}
