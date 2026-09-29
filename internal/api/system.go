package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/parthh37/wpgenie/internal/updater"
)

func (s *Server) systemInfo(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, s.Updater.Info())
}

// systemVersion is what the self-update applier polls to see the new
// version come up.
func (s *Server) systemVersion(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, map[string]string{"version": s.Version})
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, _ = s.Updater.Check(ctx) // the outcome, error included, is in Info
	return writeJSON(w, http.StatusOK, s.Updater.Info())
}

// selfUpdate downloads and verifies the latest release, then hands over to
// the applier, which restarts WPGenie: poll GET /system for progress.
func (s *Server) selfUpdate(w http.ResponseWriter, r *http.Request) error {
	to, err := s.Updater.Update(r.Context())
	switch {
	case errors.Is(err, updater.ErrUpToDate), errors.Is(err, updater.ErrBusy):
		return writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, updater.ErrNoSigningKey), errors.Is(err, updater.ErrNotService), errors.Is(err, updater.ErrBadSignature):
		return writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": err.Error()})
	case err != nil:
		return err
	}
	s.Log.Info("self-update started", "from", s.Version, "to", to)
	return writeJSON(w, http.StatusAccepted, map[string]string{"version": to})
}

// rollSites moves every site onto the current PHP image in the background.
func (s *Server) rollSites(w http.ResponseWriter, r *http.Request) error {
	go s.Sites.RollSites(context.WithoutCancel(r.Context()))
	w.WriteHeader(http.StatusAccepted)
	return nil
}
