package support

import (
	"context"
	"fmt"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Run closes answered tickets nobody replied to (see AutoClose) every
// hour until ctx ends, and clears uploads interrupted requests left in
// the staging folder.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := s.AutoClose(ctx); err != nil {
			s.Log.Warn("support: closing tickets without a reply", "err", err)
		} else if n > 0 {
			s.Log.Info("support: closed tickets without a reply", "tickets", n)
		}
		s.CleanStaging(6 * time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AutoClose closes the tickets answered more than the settings' days ago
// that the customer hasn't replied to since, noting it in each and
// e-mailing the customer. It is idempotent: a ticket is closed (and
// e-mailed about) once, whether runs overlap, repeat or crash midway.
func (s *Service) AutoClose(ctx context.Context) (int, error) {
	s.closing.Lock()
	defer s.closing.Unlock()
	st, err := s.Settings(ctx)
	if err != nil || st.AutoCloseDays == 0 {
		return 0, err
	}
	now := s.now().UTC().Truncate(time.Second)
	cutoff := now.Add(-time.Duration(st.AutoCloseDays) * 24 * time.Hour)
	closed := 0
	for {
		batch, err := s.Store.ListTickets(ctx, store.TicketFilter{Statuses: []string{store.TicketAnswered},
			LastReplyBefore: cutoff, Limit: 100})
		if err != nil {
			return closed, err
		}
		n := 0
		for _, t := range batch {
			note := &store.TicketMessage{Author: "WPGenie", Side: store.SideSystem, CreatedAt: now,
				Body: fmt.Sprintf("Closed automatically: no reply for %d days. Reply to open it again.", st.AutoCloseDays)}
			ok, err := s.Store.AutoCloseTicket(ctx, t.ID, cutoff, note)
			if err != nil {
				return closed, err
			}
			if !ok {
				continue // someone replied in the meantime
			}
			n++
			t.Status, t.ClosedAt = store.TicketClosed, now
			s.notifyClosed(ctx, t, true, st.AutoCloseDays)
		}
		closed += n
		// Every ticket of a full batch changed under us: stop rather than
		// read the same ones forever.
		if len(batch) < 100 || n == 0 {
			return closed, nil
		}
	}
}
