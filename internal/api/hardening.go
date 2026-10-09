package api

import (
	"net/http"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/site"
)

// WordPress hardening (site/hardening.go): the catalogue, a site's
// choice, and signing everyone out of WordPress. Both changes are about
// the site's protection, so someone it is shared with needs manager
// access, as for the shield.

func init() {
	registerTenantRoutes(map[string]tenantRule{
		"GET /api/v1/hardening":                      anyTenant,
		"PUT /api/v1/sites/{id}/hardening":           anyTenant,
		"POST /api/v1/sites/{id}/hardening/sign-out": anyTenant,
	})
	siteAccessRules["PUT /api/v1/sites/{id}/hardening"] = auth.AccessManager
	siteAccessRules["POST /api/v1/sites/{id}/hardening/sign-out"] = auth.AccessManager
	fixRoutes[site.FixHardening] = "PUT /api/v1/sites/{id}/hardening"
}

func (s *Server) hardeningRoutes(r func(pattern, role string, h handlerFunc)) {
	r("GET /api/v1/hardening", viewer, s.hardeningOptions)
	r("PUT /api/v1/sites/{id}/hardening", operator, s.setHardening)
	r("POST /api/v1/sites/{id}/hardening/sign-out", operator, s.signOutEveryone)
}

// hardeningOptions lists the hardening a site can have.
func (s *Server) hardeningOptions(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, site.HardeningOptions)
}

func (s *Server) setHardening(w http.ResponseWriter, r *http.Request) error {
	var in site.HardeningInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetHardening(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// signOutEveryone ends every WordPress session on the site (new salts).
func (s *Server) signOutEveryone(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.SignOutEveryone(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
}
