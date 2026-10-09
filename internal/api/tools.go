package api

import (
	"net/http"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/site"
)

// A site's Tools: maintenance mode, debug mode and its log, search &
// replace, scheduled tasks (WP-Cron), themes and a few WordPress settings
// (see internal/site: maintenance.go, debug.go, tools.go, themes.go).
// Updating a theme is the update manager's (POST /sites/{id}/updates).
//
// Every route is on a site, so tenants reach them for their own sites and
// those shared with them: reading needs viewer access, changing developer
// (the default), except the PHP error log, which can hold what visitors
// sent and paths: developer, like the site's files.

func init() {
	routes := []string{
		"GET /api/v1/sites/{id}/tools/maintenance",
		"PUT /api/v1/sites/{id}/tools/maintenance",
		"GET /api/v1/sites/{id}/tools/debug",
		"PUT /api/v1/sites/{id}/tools/debug",
		"GET /api/v1/sites/{id}/tools/debug/log",
		"DELETE /api/v1/sites/{id}/tools/debug/log",
		"POST /api/v1/sites/{id}/tools/search-replace",
		"GET /api/v1/sites/{id}/tools/cron",
		"POST /api/v1/sites/{id}/tools/cron/run",
		"GET /api/v1/sites/{id}/tools/themes",
		"POST /api/v1/sites/{id}/tools/themes",
		"POST /api/v1/sites/{id}/tools/themes/{theme}/activate",
		"DELETE /api/v1/sites/{id}/tools/themes/{theme}",
		"GET /api/v1/sites/{id}/tools/settings",
		"PUT /api/v1/sites/{id}/tools/settings",
	}
	rules := map[string]tenantRule{}
	for _, p := range routes {
		rules[p] = anyTenant
	}
	registerTenantRoutes(rules)
	siteAccessRules["GET /api/v1/sites/{id}/tools/debug/log"] = auth.AccessDeveloper
}

func (s *Server) toolsRoutes(r func(pattern, role string, h handlerFunc)) {
	r("GET /api/v1/sites/{id}/tools/maintenance", viewer, s.maintenanceMode)
	r("PUT /api/v1/sites/{id}/tools/maintenance", operator, s.setMaintenanceMode)
	r("GET /api/v1/sites/{id}/tools/debug", viewer, s.debugMode)
	r("PUT /api/v1/sites/{id}/tools/debug", operator, s.setDebugMode)
	r("GET /api/v1/sites/{id}/tools/debug/log", operator, s.debugLog)
	r("DELETE /api/v1/sites/{id}/tools/debug/log", operator, s.clearDebugLog)
	r("POST /api/v1/sites/{id}/tools/search-replace", operator, s.searchReplace)
	r("GET /api/v1/sites/{id}/tools/cron", viewer, s.cronEvents)
	r("POST /api/v1/sites/{id}/tools/cron/run", operator, s.runCronEvent)
	r("GET /api/v1/sites/{id}/tools/themes", viewer, s.themes)
	r("POST /api/v1/sites/{id}/tools/themes", operator, s.installTheme)
	r("POST /api/v1/sites/{id}/tools/themes/{theme}/activate", operator, s.activateTheme)
	r("DELETE /api/v1/sites/{id}/tools/themes/{theme}", operator, s.deleteTheme)
	r("GET /api/v1/sites/{id}/tools/settings", viewer, s.wpSettings)
	r("PUT /api/v1/sites/{id}/tools/settings", operator, s.setWPSettings)
}

func (s *Server) maintenanceMode(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Sites.MaintenanceMode(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, m)
}

func (s *Server) setMaintenanceMode(w http.ResponseWriter, r *http.Request) error {
	var in site.MaintenanceInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	m, err := s.Sites.SetMaintenanceMode(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, m)
}

func (s *Server) debugMode(w http.ResponseWriter, r *http.Request) error {
	d, err := s.Sites.DebugMode(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *Server) setDebugMode(w http.ResponseWriter, r *http.Request) error {
	var in site.DebugInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	d, err := s.Sites.SetDebugMode(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

// debugLog is the end of the site's PHP error log (where debug mode
// writes): the last 200 lines at most.
func (s *Server) debugLog(w http.ResponseWriter, r *http.Request) error {
	l, err := s.Sites.DebugLogTail(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) clearDebugLog(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.ClearDebugLog(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// searchReplace starts a search & replace (or its dry run) as a job; the
// counts are the job's result.
func (s *Server) searchReplace(w http.ResponseWriter, r *http.Request) error {
	var in site.SearchReplaceInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := s.Sites.StartSearchReplace(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}

func (s *Server) cronEvents(w http.ResponseWriter, r *http.Request) error {
	c, err := s.Sites.CronEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c)
}

// runCronEvent makes one scheduled task due and runs WordPress's cron in
// the background: 202, it may take a while.
func (s *Server) runCronEvent(w http.ResponseWriter, r *http.Request) error {
	var in site.CronRunInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.Sites.RunCronEvent(r.Context(), r.PathValue("id"), in); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (s *Server) themes(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Sites.Themes(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

// installTheme installs a theme from wordpress.org, as a job.
func (s *Server) installTheme(w http.ResponseWriter, r *http.Request) error {
	var in site.ThemeInstallInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := s.Sites.StartThemeInstall(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}

func (s *Server) activateTheme(w http.ResponseWriter, r *http.Request) error {
	t, err := s.Sites.ActivateTheme(r.Context(), r.PathValue("id"), r.PathValue("theme"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTheme(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.DeleteTheme(r.Context(), r.PathValue("id"), r.PathValue("theme")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) wpSettings(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Sites.WordPressSettings(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) setWPSettings(w http.ResponseWriter, r *http.Request) error {
	var in site.WPSettingsInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetWordPressSettings(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}
