package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/store"
)

// Billing settings (Stripe), outgoing webhooks, Stripe's incoming webhook,
// and the site-side pieces of tenancy: who owns a site, which backup
// destinations its plan allows.

func (s *Server) billingSettings(w http.ResponseWriter, r *http.Request) error {
	cfg, err := s.Billing.StripeSettings(r.Context())
	if err != nil {
		return err
	}
	// Secrets are write-only: the panel only says whether they are set.
	return writeJSON(w, http.StatusOK, map[string]any{
		"stripe_webhook_secret_set": cfg.WebhookSecret != "",
		"stripe_secret_key_set":     cfg.SecretKey != "",
		"stripe_meter_event":        cfg.MeterEvent,
		"stripe_prices":             cfg.Prices,
		"stripe_webhook_url":        s.PanelURL + "/api/v1/billing/stripe/webhook",
		"events":                    billing.Events,
		"features":                  billing.Features,
	})
}

func (s *Server) setBillingSettings(w http.ResponseWriter, r *http.Request) error {
	var in billing.StripeSettingsInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.Billing.SetStripeSettings(r.Context(), in); err != nil {
		return err
	}
	return s.billingSettings(w, r)
}

// stripeWebhook is Stripe's endpoint: public (Stripe has no session), and
// trusted only through the Stripe-Signature over the raw body.
func (s *Server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		return
	}
	if s.Billing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": billing.ErrStripeOff.Error()})
		return
	}
	err = s.Billing.HandleStripeWebhook(r.Context(), body, r.Header.Get("Stripe-Signature"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"received": true})
	case errors.Is(err, billing.ErrStripeOff):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, billing.ErrBadSignature), errors.Is(err, billing.ErrInvalid):
		s.audit(r.Context(), store.AuditEntry{Actor: "stripe", IP: clientIP(r), Action: "stripe_webhook_refused",
			Detail: err.Error(), Status: http.StatusBadRequest})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		// Stripe retries: nothing was recorded as processed.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "processing failed; retry"})
	}
}

// ---- Outgoing webhooks ----

type endpointView struct {
	*store.WebhookEndpoint
	SecretSet bool `json:"secret_set"`
}

func (s *Server) webhookEndpoints(w http.ResponseWriter, r *http.Request) error {
	eps, err := s.Store.WebhookEndpoints(r.Context())
	if err != nil {
		return err
	}
	out := make([]endpointView, len(eps))
	for i, e := range eps {
		out[i] = endpointView{e, e.Secret != ""}
	}
	return writeJSON(w, http.StatusOK, out)
}

// createWebhookEndpoint adds an endpoint; its signing secret (generated
// unless given) is returned once.
func (s *Server) createWebhookEndpoint(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Secret string   `json:"secret"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	e := &store.WebhookEndpoint{URL: in.URL, Events: in.Events, Secret: in.Secret, Enabled: true}
	if err := billing.ValidateEndpoint(e); err != nil {
		return err
	}
	if e.Secret == "" {
		e.Secret = billing.NewSecret()
	} else if len(e.Secret) < 16 || len(e.Secret) > 200 {
		return fmt.Errorf("%w: a secret must be 16-200 characters", errBadRequest)
	}
	e, err := s.Store.CreateWebhookEndpoint(r.Context(), e)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, map[string]any{"endpoint": endpointView{e, true}, "secret": e.Secret})
}

func endpointParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, store.ErrNotFound
	}
	return id, nil
}

// updateWebhookEndpoint changes the URL, events or enabled state, and with
// rotate_secret issues a new secret (returned once).
func (s *Server) updateWebhookEndpoint(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		URL          *string  `json:"url"`
		Events       []string `json:"events"`
		Enabled      *bool    `json:"enabled"`
		RotateSecret bool     `json:"rotate_secret"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := endpointParam(r)
	if err != nil {
		return err
	}
	e, err := s.Store.GetWebhookEndpoint(r.Context(), id)
	if err != nil {
		return err
	}
	if in.URL != nil {
		e.URL = *in.URL
	}
	if in.Events != nil {
		e.Events = in.Events
	}
	if in.Enabled != nil {
		e.Enabled = *in.Enabled
	}
	if err := billing.ValidateEndpoint(e); err != nil {
		return err
	}
	if in.RotateSecret {
		e.Secret = billing.NewSecret()
	}
	if err := s.Store.UpdateWebhookEndpoint(r.Context(), e); err != nil {
		return err
	}
	out := map[string]any{"endpoint": endpointView{e, true}}
	if in.RotateSecret {
		out["secret"] = e.Secret
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteWebhookEndpoint(w http.ResponseWriter, r *http.Request) error {
	id, err := endpointParam(r)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteWebhookEndpoint(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) testWebhookEndpoint(w http.ResponseWriter, r *http.Request) error {
	id, err := endpointParam(r)
	if err != nil {
		return err
	}
	if s.Billing.Hooks == nil {
		return errors.New("webhooks are not running")
	}
	if err := s.Billing.Hooks.Test(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

// webhookDeliveries is the delivery log (?endpoint= narrows it).
func (s *Server) webhookDeliveries(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	var ep int64
	if v := r.URL.Query().Get("endpoint"); v != "" {
		if ep, err = strconv.ParseInt(v, 10, 64); err != nil {
			return errBadRequest
		}
	}
	list, err := s.Store.WebhookDeliveries(r.Context(), ep, limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) retryWebhookDelivery(w http.ResponseWriter, r *http.Request) error {
	id, err := endpointParam(r)
	if err != nil {
		return err
	}
	if err := s.Store.RetryDelivery(r.Context(), id, s.now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

// ---- Sites and accounts ----

// siteView is a site with the account owning it (0: staff-only).
type siteView struct {
	*store.Site
	AccountID int64 `json:"account_id,omitempty"`
	// Access: the level the site is shared with the tenant asking at
	// (sharing.go); "" for its owners and staff.
	Access string `json:"access,omitempty"`
}

// setSiteAccount gives a site to an account (account_id 0: back to
// staff-only). Administrators only: it bypasses the plan's site count.
func (s *Server) setSiteAccount(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		AccountID int64 `json:"account_id"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Store.GetSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if in.AccountID == 0 {
		if err := s.Billing.UnassignSite(r.Context(), st.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	} else if err := s.Billing.AssignSite(r.Context(), st.ID, in.AccountID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: no account %d", errBadRequest, in.AccountID)
		}
		return err
	}
	st, err = s.Store.GetSite(r.Context(), st.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, siteView{Site: st, AccountID: in.AccountID})
}

// backupDestinations lists where a site's backups may go: every
// destination for staff, the plan's for tenants (names only, never
// credentials).
func (s *Server) backupDestinations(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	repos, err := s.Sites.Repos(r.Context())
	if err != nil {
		return err
	}
	type dest struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	out := []dest{}
	t := tenantOf(r)
	for _, rp := range repos {
		if t == nil || slices.Contains(t.SiteLimits.BackupRepos, rp.ID) {
			out = append(out, dest{rp.ID, rp.Name, rp.Kind})
		}
	}
	return writeJSON(w, http.StatusOK, out)
}
