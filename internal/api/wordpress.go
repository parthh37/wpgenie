package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/parthh37/wpgenie/internal/site"
)

// WordPress itself, from the panel: signing in to wp-admin without its
// password, administrators' passwords, performance tweaks, the site
// analyser, and the brand WordPress's admin shows.

// wpUsers lists a site's WordPress administrators and editors.
func (s *Server) wpUsers(w http.ResponseWriter, r *http.Request) error {
	users, err := s.Sites.Users(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, users)
}

// wpCreateUser adds an administrator or editor: their password is in the
// response, once.
func (s *Server) wpCreateUser(w http.ResponseWriter, r *http.Request) error {
	var in site.NewUserInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	res, err := s.Sites.CreateUser(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusCreated, res)
}

// wpDeleteUser deletes an administrator or editor (never the site's first
// user, nor its last administrator).
func (s *Server) wpDeleteUser(w http.ResponseWriter, r *http.Request) error {
	uid, err := strconv.Atoi(r.PathValue("user"))
	if err != nil || uid <= 0 {
		return errBadRequest
	}
	u, err := s.Sites.DeleteUser(r.Context(), r.PathValue("id"), uid)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, u)
}

// wpLogin returns a one-time link that signs the browser in to wp-admin
// as an administrator (user_id 0 or absent: the oldest).
func (s *Server) wpLogin(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		UserID int `json:"user_id"`
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &in); err != nil {
			return err
		}
	}
	if s.WPLogin == nil {
		return errBadRequest
	}
	// A request forwarded from the panel keeps the browser's User-Agent.
	l, err := s.WPLogin.Open(r.Context(), r.PathValue("id"), in.UserID, principalFrom(r.Context()).Name, clientIP(r), r.UserAgent())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

// wpAdminPassword resets an administrator's password: the new one is in
// the response, once.
func (s *Server) wpAdminPassword(w http.ResponseWriter, r *http.Request) error {
	var in site.PasswordInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	res, err := s.Sites.ResetAdminPassword(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, res)
}

// optimizations lists the performance tweaks a site can have.
func (s *Server) optimizations(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, site.Optimizations)
}

func (s *Server) setOptimize(w http.ResponseWriter, r *http.Request) error {
	var in site.OptimizeInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetOptimize(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// cleanupDB cleans a site's database now (the nightly tweak's work).
func (s *Server) cleanupDB(w http.ResponseWriter, r *http.Request) error {
	res, err := s.Sites.CleanupDatabase(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

// siteAnalysis runs the site analyser: a few seconds.
func (s *Server) siteAnalysis(w http.ResponseWriter, r *http.Request) error {
	a, err := s.Sites.Analyse(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, a)
}

// analysisFix applies one of the analyser's fixes.
// fixRoutes: the route each analyser fix does the work of. Someone a site
// is shared with needs that route's level for the fix (the WordPress
// settings ones, which have no route, need the fix route's own).
var fixRoutes = map[string]string{
	site.FixPageCache:      "PUT /api/v1/sites/{id}/cache",
	site.FixObjectCache:    "PUT /api/v1/sites/{id}/cache",
	site.FixImages:         "PUT /api/v1/sites/{id}/images",
	site.FixOptimize:       "PUT /api/v1/sites/{id}/optimize",
	site.FixDBCleanup:      "POST /api/v1/sites/{id}/optimize/cleanup",
	site.FixScan:           "POST /api/v1/sites/{id}/scan",
	site.FixUpdateSecurity: "POST /api/v1/sites/{id}/updates",
	site.FixUpdateAll:      "POST /api/v1/sites/{id}/updates",
	site.FixAutoUpdate:     "PUT /api/v1/sites/{id}/auto-update",
	site.FixShield:         "PUT /api/v1/sites/{id}/shield",
	site.FixWAF:            "PUT /api/v1/sites/{id}/shield",
}

func (s *Server) analysisFix(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Fix string `json:"fix"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if t := tenantOf(r); t != nil && t.Access != "" {
		if route, ok := fixRoutes[in.Fix]; ok {
			if need := requiredAccess(route); !accessAllows(t.Access, need) {
				return fmt.Errorf("%w: this fix needs %s access to the site; you have %s", errForbidden, need, t.Access)
			}
		}
	}
	res, err := s.Sites.ApplyFix(r.Context(), r.PathValue("id"), in.Fix)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *Server) branding(w http.ResponseWriter, r *http.Request) error {
	b, err := s.Sites.Branding(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, b.View())
}

// setBranding changes the brand here and on every other server (each
// rewrites its sites' wrappers and serves the logo on their domains).
func (s *Server) setBranding(w http.ResponseWriter, r *http.Request) error {
	var in site.BrandingInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	b, err := s.Sites.SetBranding(r.Context(), in)
	if err != nil {
		return err
	}
	if s.Cluster != nil {
		// The whole brand, logo included, whatever this request changed.
		s.broadcast(r, http.MethodPut, "/api/v1/settings/branding", b.Input())
	}
	return writeJSON(w, http.StatusOK, b.View())
}

// brandLogo serves the logo to the panel (its preview).
func (s *Server) brandLogo(w http.ResponseWriter, r *http.Request) error {
	s.Sites.ServeBrandLogo(w, r)
	return nil
}
