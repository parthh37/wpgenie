package api

import (
	"net/http"

	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// A site's edge rules (see site/edge.go): its lock (a username and password
// for every visitor) and its redirects. Changing either decides who reaches
// the site and where its addresses lead: manager access for users it is
// shared with (siteAccessRules); reading the redirects is anyone's who sees
// the site. The lock's password is never sent back, only whether it is on
// and its username (in the site's record).

func init() {
	registerTenantRoutes(map[string]tenantRule{
		"PUT /api/v1/sites/{id}/lock":           anyTenant,
		"GET /api/v1/sites/{id}/redirects":      anyTenant,
		"PUT /api/v1/sites/{id}/redirects":      anyTenant,
		"GET /api/v1/sites/{id}/redirects/test": anyTenant,
	})
}

func (s *Server) edgeRoutes(r func(pattern, role string, h handlerFunc)) {
	r("PUT /api/v1/sites/{id}/lock", operator, s.setSiteLock)
	r("GET /api/v1/sites/{id}/redirects", viewer, s.siteRedirects)
	r("PUT /api/v1/sites/{id}/redirects", operator, s.setSiteRedirects)
	r("GET /api/v1/sites/{id}/redirects/test", viewer, s.testSiteRedirect)
}

func (s *Server) setSiteLock(w http.ResponseWriter, r *http.Request) error {
	var in site.LockInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetLock(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// redirectList is a site's redirects as the API sends and takes them.
type redirectList struct {
	Rules []store.Redirect `json:"rules"`
	// Max is how many a site may have (responses only).
	Max int `json:"max,omitempty"`
}

func (s *Server) siteRedirects(w http.ResponseWriter, r *http.Request) error {
	rules, err := s.Sites.Redirects(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, redirectList{Rules: rules, Max: site.MaxRedirects})
}

// setSiteRedirects replaces the whole list: the panel edits it and sends
// it back, so the order people made them in stays.
func (s *Server) setSiteRedirects(w http.ResponseWriter, r *http.Request) error {
	var in redirectList
	if err := decode(w, r, &in); err != nil {
		return err
	}
	rules, err := s.Sites.SetRedirects(r.Context(), r.PathValue("id"), in.Rules)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, redirectList{Rules: rules, Max: site.MaxRedirects})
}

// testSiteRedirect says which redirect answers ?path= (a path or an
// address), as Caddy decides it.
func (s *Server) testSiteRedirect(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Sites.TestRedirect(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, m)
}
