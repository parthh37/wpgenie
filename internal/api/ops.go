package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// limitParam parses ?limit=, defaulting to def and capped at max.
func limitParam(r *http.Request, def, max int) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, errBadRequest
	}
	return min(n, max), nil
}

func (s *Server) setAutoscale(w http.ResponseWriter, r *http.Request) error {
	var in site.AutoscaleSettings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if t := tenantOf(r); t != nil && in.Enabled {
		st, err := s.siteRecord(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		if err := billing.CheckResources(t.SiteLimits, in.MaxReplicas, st.MemoryMB, st.CPUs); err != nil {
			return err
		}
	}
	if done, err := s.forwardAfterChecks(w, r, in); done || err != nil {
		return err
	}
	st, err := s.Sites.SetAutoscale(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) siteEvents(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 50, 500)
	if err != nil {
		return err
	}
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	ev, err := s.Store.Events(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ev)
}

// siteMetrics is live data that isn't stored: the latest CPU sample.
func (s *Server) siteMetrics(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	out := map[string]any{"cpu": nil}
	if c, ok := s.Sites.CPU(r.PathValue("id")); ok {
		out["cpu"] = c
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) listBans(w http.ResponseWriter, r *http.Request) error {
	bans := s.clusterBans(r, s.Shield.Bans())
	if bans == nil {
		bans = []shield.Ban{}
	}
	return writeJSON(w, http.StatusOK, bans)
}

func (s *Server) addBan(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Addr   string `json:"addr"`
		Hours  int    `json:"hours"` // 0 = the maximum (30 days)
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Hours < 0 || len(in.Reason) > 200 {
		return errBadRequest
	}
	key, err := s.Shield.BanAddr(in.Addr, time.Duration(in.Hours)*time.Hour, in.Reason)
	if err != nil {
		return errors.Join(errBadRequest, err)
	}
	s.Log.Info("manual ban", "addr", key, "hours", in.Hours)
	s.broadcast(r, http.MethodPost, "/api/v1/security/bans", in)
	return writeJSON(w, http.StatusCreated, map[string]string{"addr": key})
}

// removeBan takes ?addr= because an IPv6 entry ("2001:db8::/64") contains
// a slash and can't be a path segment.
func (s *Server) removeBan(w http.ResponseWriter, r *http.Request) error {
	ok, err := s.Shield.Unban(r.URL.Query().Get("addr"))
	if err != nil {
		return errors.Join(errBadRequest, err)
	}
	if s.broadcast(r, http.MethodDelete, "/api/v1/security/bans?"+r.URL.RawQuery, nil) > 0 {
		ok = true
	}
	if !ok {
		return writeJSON(w, http.StatusNotFound, map[string]string{"error": "no active ban for that address"})
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// securityEvents is the shield's recent log: every site's for staff, only
// their sites' for tenants.
func (s *Server) securityEvents(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 500)
	if err != nil {
		return err
	}
	siteID := r.URL.Query().Get("site")
	if tenantOf(r) == nil {
		return writeJSON(w, http.StatusOK, s.clusterEvents(r, s.Shield.Events(siteID, limit), limit))
	}
	owned, err := s.ownedSites(r.Context(), principalFrom(r.Context()))
	if err != nil {
		return err
	}
	if siteID != "" {
		if _, ok := owned[siteID]; !ok {
			return store.ErrNotFound
		}
	}
	// Every server's events (sites on other servers included), then only
	// the tenant's sites.
	all := s.clusterEvents(r, s.Shield.Events(siteID, max(limit, 500)), max(limit, 500))
	out := []shield.Event{}
	for _, e := range all {
		if _, ok := owned[e.Site]; ok && len(out) < limit {
			out = append(out, e)
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// siteUpdates lists installed components and available updates, live from
// WP-CLI (a few seconds).
func (s *Server) siteUpdates(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	inv, err := s.Sites.Inventory(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inv)
}

// startUpdate snapshots and updates in the background: 202 with the run ID,
// whose outcome appears in the update history.
func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) error {
	var in site.UpdateRequest
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := s.Sites.StartUpdate(r.Context(), r.PathValue("id"), in, "manual")
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, map[string]int64{"run_id": id})
}

func (s *Server) updateHistory(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 20, 100)
	if err != nil {
		return err
	}
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	runs, err := s.Store.Updates(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		return err
	}
	out := make([]map[string]any, len(runs))
	for i, run := range runs {
		var d site.UpdateDetails
		_ = json.Unmarshal([]byte(run.Details), &d)
		out[i] = map[string]any{"run": run, "details": d}
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) setAutoUpdate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Policy string `json:"policy"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetAutoUpdate(r.Context(), r.PathValue("id"), in.Policy)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// runScan scans synchronously (up to a minute: WP-CLI checksums plus the
// vulnerability database).
func (s *Server) runScan(w http.ResponseWriter, r *http.Request) error {
	rep, err := s.Sites.Scan(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, rep)
}

func (s *Server) lastScan(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	rep, err := s.Sites.LastScan(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		return writeJSON(w, http.StatusOK, nil) // never scanned yet
	}
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, rep)
}
