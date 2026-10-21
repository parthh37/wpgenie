package api

import (
	"net/http"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/site"
)

// Divi: the host's Elegant Themes license (see internal/site/divi.go).
// Administrators set it; the key is write-only: no response ever carries
// it, only whether one is saved and its last four characters. Everyone
// signed in (tenants too: they create sites) learns whether new sites get
// Divi. Installing Divi on a site is a change to that site: operators,
// and tenants on their own sites (developer level when it's shared).

func init() {
	registerTenantRoutes(map[string]tenantRule{
		"GET /api/v1/settings/divi":    anyTenant, // configured and new_sites only
		"POST /api/v1/sites/{id}/divi": anyTenant,
	})
	registerErrorStatus(site.ErrDiviRefused, http.StatusBadRequest)
}

func (s *Server) diviRoutes(r func(pattern, role string, h handlerFunc)) {
	r("GET /api/v1/settings/divi", viewer, s.diviSettings)
	r("PUT /api/v1/settings/divi", admin, s.setDivi)
	r("POST /api/v1/settings/divi/check", admin, s.checkDivi)
	r("POST /api/v1/sites/{id}/divi", operator, s.installDivi)
}

// isAdmin: the request is an administrator's (or the API token's).
func isAdmin(r *http.Request) bool {
	p := principalFrom(r.Context())
	return p != nil && !auth.IsTenant(p.Role) && auth.Level(p.Role) >= auth.Level(auth.RoleAdmin)
}

func (s *Server) diviSettings(w http.ResponseWriter, r *http.Request) error {
	l, err := s.Sites.Divi(r.Context())
	if err != nil {
		return err
	}
	v := l.View()
	if !isAdmin(r) {
		v = v.Public()
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, v)
}

// setDivi changes the license here and on every other server: they
// install Divi on their own sites, with their own copy of the license.
func (s *Server) setDivi(w http.ResponseWriter, r *http.Request) error {
	var in site.DiviInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	l, err := s.Sites.SetDivi(r.Context(), in)
	if err != nil {
		return err
	}
	if s.Cluster != nil {
		// The whole license, key included, whatever this request changed.
		s.broadcast(r, http.MethodPut, "/api/v1/settings/divi", l.Input())
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, l.View())
}

// checkDivi asks Elegant Themes whether the saved account gets Divi.
func (s *Server) checkDivi(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.CheckDivi(r.Context()); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"message": "Elegant Themes accepted the license: Divi can be downloaded"})
}

// installDivi installs and activates Divi on a site, as a job.
func (s *Server) installDivi(w http.ResponseWriter, r *http.Request) error {
	id, err := s.Sites.StartDiviInstall(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}
