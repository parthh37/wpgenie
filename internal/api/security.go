package api

import (
	"errors"
	"net/http"

	"github.com/parthh37/wpgenie/internal/iprep"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

func (s *Server) securitySettings(w http.ResponseWriter, r *http.Request) error {
	g, err := s.Sites.GlobalLists(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, g)
}

func (s *Server) setSecuritySettings(w http.ResponseWriter, r *http.Request) error {
	var in site.GlobalLists
	if err := decode(w, r, &in); err != nil {
		return err
	}
	g, err := s.Sites.SetGlobalLists(r.Context(), in)
	if err != nil {
		return err
	}
	s.Shield.SetGlobal(g.Shield())
	return writeJSON(w, http.StatusOK, g)
}

// reputationStatus reports the blocklists, the country database and
// whether Caddy can inspect request bodies.
func (s *Server) reputationStatus(w http.ResponseWriter, _ *http.Request) error {
	out := map[string]any{"lists": []iprep.FeedStatus{}, "countries": nil, "waf_available": false}
	if s.Lists != nil {
		out["lists"] = s.Lists.Status()
	}
	if s.Countries != nil {
		out["countries"] = s.Countries.Status()
	}
	if p, ok := s.Sites.Proxy.(interface{ WAFAvailable() bool }); ok {
		out["waf_available"] = p.WAFAvailable()
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) refreshReputation(w http.ResponseWriter, r *http.Request) error {
	if s.Lists == nil {
		return errors.New("IP reputation is not configured")
	}
	err := s.Lists.Refresh(r.Context())
	if s.Countries != nil && s.Sites.CountryRulesInUse() {
		err = errors.Join(err, s.Countries.Update(r.Context(), s.now()))
	}
	if err != nil {
		s.Log.Warn("refreshing IP reputation", "err", err)
	}
	return s.reputationStatus(w, r) // per-feed errors are in the status
}

func (s *Server) pluginReport(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	rep, err := s.Sites.LastPluginReport(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		return writeJSON(w, http.StatusOK, nil)
	}
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, rep)
}

// analysePlugins runs synchronously: typically 10-30 seconds.
func (s *Server) analysePlugins(w http.ResponseWriter, r *http.Request) error {
	rep, err := s.Sites.AnalysePlugins(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, rep)
}
