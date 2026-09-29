// Package api exposes the panel's REST API, the embedded dashboard and the
// shield endpoints Caddy calls.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/updater"
	"github.com/parthh37/wpgenie/internal/web"
)

type Server struct {
	Token   string
	Version string
	Sites   *site.Service
	Store   *store.Store
	Shield  *shield.Shield
	Updater *updater.Updater
	Mail    *mail.Service
	Log     *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Shield endpoints: reached only through Caddy (loopback listener).
	mux.Handle("GET /_shield/check", s.Shield.CheckHandler()) // forward_auth always uses GET
	mux.Handle("POST /_shield/verify", s.Shield.VerifyHandler())

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mux.Handle("GET /api/v1/sites", s.auth(s.listSites))
	mux.Handle("POST /api/v1/sites", s.auth(s.createSite))
	mux.Handle("GET /api/v1/sites/{id}", s.auth(s.getSite))
	mux.Handle("DELETE /api/v1/sites/{id}", s.auth(s.deleteSite))
	mux.Handle("PUT /api/v1/sites/{id}/shield", s.auth(s.setShield))
	mux.Handle("GET /api/v1/sites/{id}/stats", s.auth(s.siteStats))
	mux.Handle("PUT /api/v1/sites/{id}/resources", s.auth(s.setResources))
	mux.Handle("PUT /api/v1/sites/{id}/cache", s.auth(s.setCache))
	mux.Handle("POST /api/v1/sites/{id}/cache/purge", s.auth(s.purgeCache))
	mux.Handle("PUT /api/v1/sites/{id}/autoscale", s.auth(s.setAutoscale))
	mux.Handle("GET /api/v1/sites/{id}/events", s.auth(s.siteEvents))
	mux.Handle("GET /api/v1/sites/{id}/metrics", s.auth(s.siteMetrics))
	mux.Handle("GET /api/v1/sites/{id}/updates", s.auth(s.siteUpdates))
	mux.Handle("POST /api/v1/sites/{id}/updates", s.auth(s.startUpdate))
	mux.Handle("GET /api/v1/sites/{id}/updates/history", s.auth(s.updateHistory))
	mux.Handle("PUT /api/v1/sites/{id}/auto-update", s.auth(s.setAutoUpdate))
	mux.Handle("POST /api/v1/sites/{id}/scan", s.auth(s.runScan))
	mux.Handle("GET /api/v1/sites/{id}/scan", s.auth(s.lastScan))

	mux.Handle("PUT /api/v1/sites/{id}/smtp", s.auth(s.setSiteSMTP))
	mux.Handle("GET /api/v1/sites/{id}/cdn", s.auth(s.cdnStatus))
	mux.Handle("PUT /api/v1/sites/{id}/cdn", s.auth(s.setCDN))
	mux.Handle("POST /api/v1/sites/{id}/cdn/purge", s.auth(s.purgeCDN))

	mux.Handle("GET /api/v1/mail", s.auth(s.mailStatus))
	mux.Handle("PUT /api/v1/mail", s.auth(s.setMail))
	mux.Handle("PUT /api/v1/mail/relay", s.auth(s.setRelay))
	mux.Handle("DELETE /api/v1/mail/relay", s.auth(s.deleteRelay))
	mux.Handle("GET /api/v1/mail/domains", s.auth(s.mailDomains))
	mux.Handle("POST /api/v1/mail/domains", s.auth(s.addMailDomain))
	mux.Handle("GET /api/v1/mail/domains/{domain}", s.auth(s.mailDomain))
	mux.Handle("DELETE /api/v1/mail/domains/{domain}", s.auth(s.deleteMailDomain))
	mux.Handle("GET /api/v1/mail/mailboxes", s.auth(s.mailboxes))
	mux.Handle("POST /api/v1/mail/mailboxes", s.auth(s.createMailbox))
	mux.Handle("PUT /api/v1/mail/mailboxes/{address}/password", s.auth(s.setMailboxPassword))
	mux.Handle("PUT /api/v1/mail/mailboxes/{address}/quota", s.auth(s.setMailboxQuota))
	mux.Handle("DELETE /api/v1/mail/mailboxes/{address}", s.auth(s.deleteMailbox))
	mux.Handle("GET /api/v1/mail/aliases", s.auth(s.mailAliases))
	mux.Handle("POST /api/v1/mail/aliases", s.auth(s.addMailAlias))
	mux.Handle("DELETE /api/v1/mail/aliases", s.auth(s.deleteMailAlias))

	mux.Handle("GET /api/v1/system", s.auth(s.systemInfo))
	mux.Handle("GET /api/v1/system/version", s.auth(s.systemVersion))
	mux.Handle("POST /api/v1/system/update/check", s.auth(s.checkUpdate))
	mux.Handle("POST /api/v1/system/update", s.auth(s.selfUpdate))
	mux.Handle("POST /api/v1/system/roll-sites", s.auth(s.rollSites))

	mux.Handle("GET /api/v1/security/bans", s.auth(s.listBans))
	mux.Handle("POST /api/v1/security/bans", s.auth(s.addBan))
	mux.Handle("DELETE /api/v1/security/bans", s.auth(s.removeBan))
	mux.Handle("GET /api/v1/security/events", s.auth(s.securityEvents))

	static, _ := fs.Sub(web.Static, "static")
	mux.Handle("GET /", http.FileServerFS(static))

	return securityHeaders(mux)
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// auth wraps a handler with bearer-token auth and uniform error handling.
func (s *Server) auth(h handlerFunc) http.Handler {
	want := []byte("Bearer " + s.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if err := h(w, r); err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, store.ErrNotFound):
				status = http.StatusNotFound
			case errors.Is(err, site.ErrDomainTaken), errors.Is(err, site.ErrConflict),
				errors.Is(err, mail.ErrConflict), errors.Is(err, mail.ErrDisabled):
				status = http.StatusConflict
			case errors.Is(err, site.ErrInvalidDomain), errors.Is(err, site.ErrInvalidInput), errors.Is(err, errBadRequest),
				errors.Is(err, mail.ErrInvalid):
				status = http.StatusBadRequest
			}
			if status == http.StatusInternalServerError {
				s.Log.Error("api", "method", r.Method, "path", r.URL.Path, "err", err)
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
		}
	})
}

var errBadRequest = errors.New("bad request")

func (s *Server) listSites(w http.ResponseWriter, r *http.Request) error {
	sites, err := s.Store.ListSites(r.Context())
	if err != nil {
		return err
	}
	if sites == nil {
		sites = []*store.Site{}
	}
	return writeJSON(w, http.StatusOK, sites)
}

func (s *Server) getSite(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Store.GetSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) createSite(w http.ResponseWriter, r *http.Request) error {
	var in site.CreateInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, creds, err := s.Sites.Create(r.Context(), in)
	if err != nil {
		return err
	}
	// Credentials are returned exactly once and never stored by the panel.
	return writeJSON(w, http.StatusCreated, map[string]any{"site": st, "credentials": creds})
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) setShield(w http.ResponseWriter, r *http.Request) error {
	var in site.ShieldInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetShield(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// setResources scales a site (memory/CPU per replica, replica count). It
// runs a rolling replacement synchronously: expect a few seconds per replica.
func (s *Server) setResources(w http.ResponseWriter, r *http.Request) error {
	var in site.Resources
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.Scale(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) setCache(w http.ResponseWriter, r *http.Request) error {
	var in site.CacheSettings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetCache(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) purgeCache(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.Purge(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// cdnStatus checks DNS and the Cloudflare zone live: a second or two.
func (s *Server) cdnStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Sites.CDNStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) setCDN(w http.ResponseWriter, r *http.Request) error {
	var in site.CDNInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetCDN(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) purgeCDN(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.PurgeCDN(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) siteStats(w http.ResponseWriter, r *http.Request) error {
	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil || n < 1 || n > 24*90 {
			return errBadRequest
		}
		hours = n
	}
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	stats, err := s.Store.SiteStats(r.Context(), r.PathValue("id"), time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, stats)
}

func decode(w http.ResponseWriter, r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return errors.Join(errBadRequest, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/_shield/") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
			w.Header().Set("X-Frame-Options", "DENY")
		}
		h.ServeHTTP(w, r)
	})
}
