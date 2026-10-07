package api

import (
	"errors"
	"net/http"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Burst (a site's extra instances under load, paid for in minutes), the
// security levels, and automatic Under attack periods: the settings the
// panel shows people who shouldn't need to know what a replica is.

func (s *Server) burstRoutes(r func(pattern, role string, h handlerFunc)) {
	r("GET /api/v1/sites/{id}/burst", viewer, s.siteBurst)
	r("PUT /api/v1/sites/{id}/burst", operator, s.setBurst)
	r("GET /api/v1/accounts/{id}/burst", viewer, s.accountBurst)
	r("POST /api/v1/accounts/{id}/burst-credit", admin, s.addBurstCredit)
	r("GET /api/v1/security/levels", viewer, s.protectionLevels)
	r("GET /api/v1/sites/{id}/attack", viewer, s.siteAttack)
	r("DELETE /api/v1/sites/{id}/attack", operator, s.endAttack)
}

// siteBurstView is a site's burst and, when an account owns the site,
// what that account has left.
type siteBurstView struct {
	site.BurstStatus
	Account *billing.BurstBalance `json:"account"`
}

// siteBurst is served by the panel for every site: minutes are kept here
// (other servers report theirs), and so are accounts.
func (s *Server) siteBurst(w http.ResponseWriter, r *http.Request) error {
	st, err := s.siteRecord(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	b, err := s.Sites.Burst(r.Context(), st, s.now())
	if err != nil {
		return err
	}
	out := siteBurstView{BurstStatus: b}
	if s.Billing != nil {
		o, err := s.Store.SiteOwnerOf(r.Context(), st.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			a, err := s.Store.GetAccount(r.Context(), o.AccountID)
			if err != nil {
				return err
			}
			if out.Account, err = s.Billing.BurstBalanceOf(r.Context(), a); err != nil {
				return err
			}
			// Someone the site is shared with sees this site's minutes,
			// not which other sites the account has.
			if t := tenantOf(r); t != nil && t.Access != "" {
				out.Account.PerSite = map[string]int64{st.ID: out.Account.PerSite[st.ID]}
			}
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// setBurst checks a tenant's plan here, then changes the site where it
// runs: the plan's instance limit travels in the request.
func (s *Server) setBurst(w http.ResponseWriter, r *http.Request) error {
	var in site.BurstInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if t := tenantOf(r); t != nil {
		st, err := s.siteRecord(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		if in.Base > 0 {
			if err := billing.CheckResources(t.SiteLimits, in.Base, st.MemoryMB, st.CPUs); err != nil {
				return err
			}
		}
		if m := t.SiteLimits.MaxReplicas; m > 0 && (in.MaxReplicas == 0 || in.MaxReplicas > m) {
			in.MaxReplicas = m
		}
	}
	if done, err := s.forwardAfterChecks(w, r, in); done || err != nil {
		return err
	}
	st, err := s.Sites.SetBurst(r.Context(), r.PathValue("id"), in, 0)
	if err != nil {
		return err
	}
	if s.Billing != nil && in.Mode != site.BurstOff {
		s.Billing.KickBurst() // an account already out of minutes pauses it at once
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) accountBurst(w http.ResponseWriter, r *http.Request) error {
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	b, err := s.Billing.BurstBalanceOf(r.Context(), a)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, b)
}

// addBurstCredit adds bought burst minutes to an account (negative:
// removes them); billing systems call it when a customer buys a pack.
func (s *Server) addBurstCredit(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Minutes int64 `json:"minutes"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	if _, err := s.Billing.AddBurstCredit(r.Context(), a.ID, in.Minutes); err != nil {
		return err
	}
	return s.accountBurst(w, r)
}

func (s *Server) protectionLevels(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, site.ProtectionLevels)
}

// siteAttack is the site's automatic Under attack period, if one is on
// (the shield of the server the site runs on: forwarded there).
func (s *Server) siteAttack(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	out := map[string]any{"active": false}
	if st, ok := s.Shield.Attack(r.PathValue("id")); ok {
		out = map[string]any{"active": true, "attack": st}
	}
	return writeJSON(w, http.StatusOK, out)
}

// endAttack ends an automatic Under attack period now (the owner knows
// it's over). The shield starts another if the flood goes on.
func (s *Server) endAttack(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	if !s.Shield.EndAttack(r.PathValue("id")) {
		return writeJSON(w, http.StatusNotFound, map[string]string{"error": "the site isn't under attack"})
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
