package logship

import (
	"context"
	"fmt"
	"time"
)

// Status is log shipping on this server, for the panel.
type Status struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	// Health: ok | warning | error | off, and what it means in plain
	// words.
	Health        string `json:"health"`
	HealthMessage string `json:"health_message"`

	Destination struct {
		Provider string `json:"provider"`
		Endpoint string `json:"endpoint"`
		Bucket   string `json:"bucket"`
		Prefix   string `json:"prefix"`
	} `json:"destination"`
	Shipper struct {
		State   string `json:"state"` // running | restarting | exited | … | "" (none)
		Image   string `json:"image"`
		Version string `json:"version,omitempty"`
		Error   string `json:"error,omitempty"`
	} `json:"shipper"`

	LastUpload   time.Time `json:"last_upload,omitzero"`
	SentEvents   int64     `json:"sent_events"` // since the shipper started
	SentBytes    int64     `json:"sent_bytes"`
	UploadErrors int64     `json:"upload_errors"`
	BufferBytes  int64     `json:"buffer_bytes"`

	Spool struct {
		Bytes int64 `json:"bytes"`
		Files int64 `json:"files"`
		Cap   int64 `json:"cap"`
	} `json:"spool"`
	// DroppedToday counts events dropped today, every type.
	DroppedToday int64 `json:"dropped_today"`

	Retention struct {
		Days    int       `json:"days"`
		LastRun time.Time `json:"last_run,omitzero"`
		Deleted int       `json:"deleted"`
		Error   string    `json:"error,omitempty"`
	} `json:"retention"`
	ExportError string `json:"export_error,omitempty"`

	Types []TypeStatus `json:"types"`
}

// TypeStatus is one kind of log: whether it ships, today's volume and
// the last two weeks'.
type TypeStatus struct {
	Type
	Enabled   bool        `json:"enabled"`
	Available bool        `json:"available"`
	Today     DayVolume   `json:"today"`
	History   []DayVolume `json:"history"` // 14 days, oldest first
}

// DayVolume is a day's amount of one kind of log.
type DayVolume struct {
	Day     string `json:"day"` // YYYY-MM-DD
	Events  int64  `json:"events"`
	Bytes   int64  `json:"bytes"`
	Dropped int64  `json:"dropped"`
}

// historyDays is how many days of volumes the status has.
const historyDays = 14

// Status reports on shipping here. Its metrics are at most a minute old
// (refreshed now when older).
func (s *Service) Status(ctx context.Context) (*Status, error) {
	set := s.current()
	out := &Status{Enabled: set.Enabled, Server: s.server()}
	out.Destination.Provider, out.Destination.Endpoint = set.Destination.Provider, set.Destination.Endpoint
	out.Destination.Bucket, out.Destination.Prefix = set.Destination.Bucket, set.Destination.Prefix
	out.Shipper.Image = s.Cfg.Image
	state, _, err := s.inspect(ctx)
	if err != nil {
		out.Shipper.Error = err.Error()
	}
	out.Shipper.State = state
	if set.Enabled && state == "running" {
		s.mu.Lock()
		stale := s.now().Sub(s.st.metricsAt) > time.Minute
		s.mu.Unlock()
		if stale {
			s.pollMetrics(ctx)
		}
	}

	now := s.now().UTC()
	vols, err := s.Store.LogVolumes(ctx, now.AddDate(0, 0, -(historyDays-1)))
	if err != nil {
		return nil, err
	}
	byType := map[string]map[string]DayVolume{}
	for _, v := range vols {
		if byType[v.Kind] == nil {
			byType[v.Kind] = map[string]DayVolume{}
		}
		d := v.Day.Format(time.DateOnly)
		byType[v.Kind][d] = DayVolume{Day: d, Events: v.Events, Bytes: v.Bytes, Dropped: v.Dropped}
	}
	// What the spool counted since the last minute's flush joins today.
	pending := s.spool.peek()
	today := now.Format(time.DateOnly)
	for _, t := range Types {
		ts := TypeStatus{Type: t, Enabled: set.Types[t.Name], Available: s.Available(t.Name), History: []DayVolume{}}
		for i := historyDays - 1; i >= 0; i-- {
			d := now.AddDate(0, 0, -i).Format(time.DateOnly)
			v := byType[t.Name][d]
			v.Day = d
			if d == today {
				c := pending[t.Name]
				v.Events, v.Bytes, v.Dropped = v.Events+c.Events, v.Bytes+c.Bytes, v.Dropped+c.Dropped
				ts.Today = v
				out.DroppedToday += v.Dropped
			}
			ts.History = append(ts.History, v)
		}
		out.Types = append(out.Types, ts)
	}
	out.Spool.Bytes, out.Spool.Files = s.spool.Usage()
	out.Spool.Cap = int64(set.SpoolCapMB) << 20

	p := s.loadPersisted(ctx)
	out.Retention.Days, out.Retention.LastRun, out.Retention.Deleted, out.Retention.Error =
		set.ArchiveRetentionDays, p.RetentionAt, p.Deleted, p.RetErr

	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	out.LastUpload = st.lastUpload
	out.ExportError = st.exportErr
	if st.applyErr != "" {
		out.Shipper.Error = st.applyErr
	}
	if m := st.metrics; m != nil && state == "running" {
		out.Shipper.Version = m.Version
		out.SentEvents, out.SentBytes, out.UploadErrors, out.BufferBytes =
			int64(m.Sent), int64(m.SentBytes), int64(m.Errors), int64(m.BufferBytes)
	}
	out.Health, out.HealthMessage = s.health(set, out, st)
	return out, nil
}

// health sums the status up in plain words.
func (s *Service) health(set Settings, st *Status, rs runState) (string, string) {
	switch {
	case !set.Enabled:
		return "off", "Log shipping is off: logs stay on this server only."
	case set.Destination.check() != nil:
		return "error", "Log shipping is on, but where logs go isn't complete: finish the destination."
	case rs.applyErr != "":
		return "error", "The log shipper couldn't start: " + rs.applyErr
	case st.Shipper.State == "":
		return "warning", "The log shipper is starting (its image is downloaded the first time)."
	case st.Shipper.State != "running":
		return "error", fmt.Sprintf("The log shipper isn't running (it's %s).", st.Shipper.State)
	case !rs.lastErrorAt.IsZero() && rs.lastErrorAt.After(rs.lastUpload):
		return "error", "Uploads are failing: the storage refuses them or can't be reached. Use \"Test connection\" to see why."
	case st.DroppedToday > 0:
		return "warning", fmt.Sprintf("%d log lines were dropped today: the space for logs waiting to be uploaded was full.", st.DroppedToday)
	case st.Spool.Cap > 0 && st.Spool.Bytes > st.Spool.Cap*8/10:
		return "warning", "Logs are piling up on this server: the storage may be unreachable."
	case rs.metricsErr != "":
		return "warning", "The log shipper runs, but its counters can't be read: " + rs.metricsErr
	case rs.exportErr != "":
		return "warning", "Some of the panel's own logs couldn't be exported: " + rs.exportErr
	case st.LastUpload.IsZero():
		return "ok", fmt.Sprintf("Shipping. The first upload happens within %s.", humanDuration(set.BatchMaxSeconds))
	}
	return "ok", "Logs are shipping."
}

func humanDuration(secs int) string {
	if secs < 120 {
		return fmt.Sprintf("%d seconds", secs)
	}
	return fmt.Sprintf("%d minutes", secs/60)
}

// peek is every type's counts since the last Take, left in place.
func (s *Spool) peek() map[string]Counts {
	out := map[string]Counts{}
	for name, c := range s.counts {
		out[name] = Counts{c.events.Load(), c.bytes.Load(), c.dropped.Load()}
	}
	return out
}
