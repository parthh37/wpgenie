package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/parthh37/wpgenie/internal/monitor"
)

// Monitoring: Prometheus metrics and alerts. /metrics has its own bearer
// token (never the API token, which can change everything): a scraper
// only ever needs to read.
func (s *Server) monitoringRoutes(mux *http.ServeMux, r func(string, string, handlerFunc)) {
	if s.Monitor == nil {
		return
	}
	mux.Handle("GET /metrics", s.Monitor.MetricsHandler())
	r("GET /api/v1/monitoring/alerts", viewer, s.monitoringAlerts)
	r("GET /api/v1/monitoring/settings", admin, s.monitoringSettings)
	r("PUT /api/v1/monitoring/settings", admin, s.setMonitoringSettings)
	r("POST /api/v1/monitoring/test", admin, s.testNotifications)
	r("POST /api/v1/monitoring/metrics-token", admin, s.rotateMetricsToken)
}

func (s *Server) monitoringAlerts(w http.ResponseWriter, r *http.Request) error {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return errBadRequest
		}
		limit = n
	}
	o, err := s.Monitor.Alerts(r.Context(), limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, o)
}

// monitoringView is the settings without secrets, plus whether a scrape
// token exists.
type monitoringView struct {
	monitor.Settings
	MetricsToken monitor.TokenInfo `json:"metrics_token"`
	// GeneratedSecrets are webhook signing secrets created by this request
	// (by webhook ID): shown once.
	GeneratedSecrets map[string]string `json:"generated_secrets,omitempty"`
}

func (s *Server) monitoringSettings(w http.ResponseWriter, r *http.Request) error {
	set, err := s.Monitor.Settings(r.Context())
	if err != nil {
		return err
	}
	tok, err := s.Monitor.MetricsTokenInfo(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, monitoringView{Settings: set.Redacted(), MetricsToken: tok})
}

func (s *Server) setMonitoringSettings(w http.ResponseWriter, r *http.Request) error {
	var in monitor.Settings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	set, generated, err := s.Monitor.SetSettings(r.Context(), in)
	if errors.Is(err, monitor.ErrInvalid) {
		return errors.Join(errBadRequest, err)
	}
	if err != nil {
		return err
	}
	tok, err := s.Monitor.MetricsTokenInfo(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, monitoringView{Settings: set.Redacted(), MetricsToken: tok, GeneratedSecrets: generated})
}

// testNotifications runs synchronously: up to ~45 s per channel, in
// parallel.
func (s *Server) testNotifications(w http.ResponseWriter, r *http.Request) error {
	res, err := s.Monitor.TestChannels(r.Context())
	if err != nil {
		return err
	}
	if res == nil {
		res = []monitor.ChannelResult{}
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *Server) rotateMetricsToken(w http.ResponseWriter, r *http.Request) error {
	tok, err := s.Monitor.RotateMetricsToken(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}
