package api

import (
	"net/http"

	"github.com/parthh37/wpgenie/internal/site"
)

// WordPress itself, from the panel: signing in to wp-admin without its
// password, administrators' passwords, performance tweaks, the site
// analyser, and the brand WordPress's admin shows.

// wpAdmins lists a site's WordPress administrators.
func (s *Server) wpAdmins(w http.ResponseWriter, r *http.Request) error {
	users, err := s.Sites.Administrators(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, users)
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
func (s *Server) analysisFix(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Fix string `json:"fix"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
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
