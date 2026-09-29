// Package api exposes the panel's REST API, the embedded dashboard and the
// shield endpoints Caddy calls.
package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/adminer"
	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/backup"
	"github.com/parthh37/wpgenie/internal/iprep"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/sftp"
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
	Jobs    *jobs.Queue
	SFTP    *sftp.Service
	Adminer *adminer.Service
	// IP reputation data, for the status view (optional).
	Lists     *iprep.Lists
	Countries *iprep.Countries
	Log       *slog.Logger
	Now       func() time.Time // for tests

	guard loginGuard
}

// Roles a route needs (see auth.Role*).
const (
	viewer   = auth.RoleViewer
	operator = auth.RoleOperator
	admin    = auth.RoleAdmin
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Shield endpoints: reached only through Caddy (loopback listener).
	mux.Handle("GET /_shield/check", s.Shield.CheckHandler()) // forward_auth always uses GET
	mux.Handle("POST /_shield/verify", s.Shield.VerifyHandler())

	// WPGenie tools on sites' own domains (reached only through Caddy's
	// /_wpgenie/* route, which names the site): Adminer.
	if s.Adminer != nil {
		mux.Handle("GET "+adminer.Path, s.Adminer)
		mux.Handle("POST "+adminer.Path, s.Adminer)
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	// Signing in.
	mux.Handle("GET /api/v1/auth/state", s.publicAuth(s.authState))
	mux.Handle("POST /api/v1/auth/setup", s.publicAuth(s.setup))
	mux.Handle("POST /api/v1/auth/login", s.publicAuth(s.login))
	mux.Handle("POST /api/v1/auth/logout", s.publicAuth(s.logout))

	r := func(pattern, role string, h handlerFunc) { s.route(mux, pattern, role, h) }

	// Your own account: any role, and reachable before enrolling in 2FA
	// when the panel requires it.
	r("GET /api/v1/account", viewer, s.account)
	r("PUT /api/v1/account/password", viewer, s.changePassword)
	r("POST /api/v1/account/totp", viewer, s.startTOTP)
	r("GET /api/v1/account/totp/qr.svg", viewer, s.totpQR)
	r("PUT /api/v1/account/totp", viewer, s.confirmTOTP)
	r("DELETE /api/v1/account/totp", viewer, s.disableTOTP)
	r("POST /api/v1/account/recovery-codes", viewer, s.newRecoveryCodes)
	r("GET /api/v1/account/sessions", viewer, s.mySessions)
	r("DELETE /api/v1/account/sessions/{id}", viewer, s.deleteMySession)

	r("GET /api/v1/users", admin, s.listUsers)
	r("POST /api/v1/users", admin, s.createUser)
	r("PUT /api/v1/users/{id}", admin, s.updateUser)
	r("POST /api/v1/users/{id}/password", admin, s.resetPassword)
	r("DELETE /api/v1/users/{id}/totp", admin, s.resetTOTP)
	r("DELETE /api/v1/users/{id}", admin, s.deleteUser)
	r("GET /api/v1/sessions", admin, s.allSessions)
	r("DELETE /api/v1/sessions/{id}", admin, s.deleteAnySession)
	r("GET /api/v1/audit", admin, s.auditLog)
	r("GET /api/v1/settings/auth", admin, s.authSettings)
	r("PUT /api/v1/settings/auth", admin, s.setAuthSettings)

	r("GET /api/v1/sites", viewer, s.listSites)
	r("POST /api/v1/sites", admin, s.createSite)
	r("GET /api/v1/sites/{id}", viewer, s.getSite)
	// Operators may delete staging sites (they create them); live sites
	// need an admin (checked in deleteSite).
	r("DELETE /api/v1/sites/{id}", operator, s.deleteSite)
	r("PUT /api/v1/sites/{id}/shield", operator, s.setShield)
	r("GET /api/v1/sites/{id}/stats", viewer, s.siteStats)
	r("PUT /api/v1/sites/{id}/resources", operator, s.setResources)
	r("PUT /api/v1/sites/{id}/cache", operator, s.setCache)
	r("POST /api/v1/sites/{id}/cache/purge", operator, s.purgeCache)
	r("PUT /api/v1/sites/{id}/autoscale", operator, s.setAutoscale)
	r("GET /api/v1/sites/{id}/events", viewer, s.siteEvents)
	r("GET /api/v1/sites/{id}/metrics", viewer, s.siteMetrics)
	r("GET /api/v1/sites/{id}/updates", viewer, s.siteUpdates)
	r("POST /api/v1/sites/{id}/updates", operator, s.startUpdate)
	r("GET /api/v1/sites/{id}/updates/history", viewer, s.updateHistory)
	r("PUT /api/v1/sites/{id}/auto-update", operator, s.setAutoUpdate)
	r("POST /api/v1/sites/{id}/scan", operator, s.runScan)
	r("GET /api/v1/sites/{id}/scan", viewer, s.lastScan)
	r("POST /api/v1/sites/{id}/plugins", operator, s.analysePlugins)
	r("GET /api/v1/sites/{id}/plugins", viewer, s.pluginReport)

	r("GET /api/v1/jobs", viewer, s.listJobs)
	r("GET /api/v1/jobs/{id}", viewer, s.getJob)
	r("DELETE /api/v1/jobs/{id}/secret", viewer, s.dropJobSecret)

	r("GET /api/v1/backups/repos", viewer, s.listRepos)
	r("POST /api/v1/backups/repos", admin, s.addRepo)
	r("DELETE /api/v1/backups/repos/{id}", admin, s.deleteRepo)
	r("POST /api/v1/backups/repos/{id}/check", operator, s.checkRepo)
	r("POST /api/v1/backups/repos/{id}/password", admin, s.repoPassword)
	r("GET /api/v1/backups/repos/{id}/backups", viewer, s.repoBackups)
	r("DELETE /api/v1/backups/repos/{id}/backups/{backup}", admin, s.deleteRepoBackup)
	r("POST /api/v1/backups/restore-new", admin, s.restoreAsNew)
	r("GET /api/v1/sites/{id}/backups", viewer, s.siteBackups)
	r("PUT /api/v1/sites/{id}/backups/policy", operator, s.setBackupPolicy)
	r("POST /api/v1/sites/{id}/backups", operator, s.startBackup)
	r("POST /api/v1/sites/{id}/backups/restore", operator, s.startRestore)
	r("GET /api/v1/sites/{id}/backups/{repo}/{backup}/download", operator, s.downloadBackup)
	r("DELETE /api/v1/sites/{id}/backups/{repo}/{backup}", admin, s.deleteBackup)

	r("POST /api/v1/sites/{id}/staging", operator, s.createStaging)
	r("POST /api/v1/sites/{id}/push", operator, s.pushStaging)
	r("GET /api/v1/sites/{id}/tables", viewer, s.siteTables)
	r("POST /api/v1/sites/{id}/domains", operator, s.addDomain)
	r("PUT /api/v1/sites/{id}/domains/{domain}", operator, s.setDomain)
	r("DELETE /api/v1/sites/{id}/domains/{domain}", operator, s.removeDomain)
	r("PUT /api/v1/sites/{id}/primary-domain", operator, s.setPrimaryDomain)
	r("GET /api/v1/sites/{id}/certificate", viewer, s.getCert)
	r("PUT /api/v1/sites/{id}/certificate", operator, s.setCert)
	r("DELETE /api/v1/sites/{id}/certificate", operator, s.deleteCert)
	r("GET /api/v1/php", viewer, s.phpVersions)
	r("PUT /api/v1/sites/{id}/php", operator, s.setPHP)
	r("GET /api/v1/sites/{id}/sftp", viewer, s.listSFTP)
	r("POST /api/v1/sites/{id}/sftp", operator, s.addSFTP)
	r("PUT /api/v1/sites/{id}/sftp/{user}/keys", operator, s.setSFTPKeys)
	r("PUT /api/v1/sites/{id}/sftp/{user}/password", operator, s.setSFTPPassword)
	r("DELETE /api/v1/sites/{id}/sftp/{user}", operator, s.deleteSFTP)
	r("POST /api/v1/sites/{id}/adminer", operator, s.openAdminer)

	r("PUT /api/v1/sites/{id}/smtp", operator, s.setSiteSMTP)
	r("GET /api/v1/sites/{id}/cdn", viewer, s.cdnStatus)
	r("PUT /api/v1/sites/{id}/cdn", operator, s.setCDN)
	r("POST /api/v1/sites/{id}/cdn/purge", operator, s.purgeCDN)

	r("GET /api/v1/mail", viewer, s.mailStatus)
	r("PUT /api/v1/mail", admin, s.setMail)
	r("PUT /api/v1/mail/relay", admin, s.setRelay)
	r("DELETE /api/v1/mail/relay", admin, s.deleteRelay)
	r("GET /api/v1/mail/domains", viewer, s.mailDomains)
	r("POST /api/v1/mail/domains", operator, s.addMailDomain)
	r("GET /api/v1/mail/domains/{domain}", viewer, s.mailDomain)
	r("DELETE /api/v1/mail/domains/{domain}", operator, s.deleteMailDomain)
	r("GET /api/v1/mail/mailboxes", viewer, s.mailboxes)
	r("POST /api/v1/mail/mailboxes", operator, s.createMailbox)
	r("PUT /api/v1/mail/mailboxes/{address}/password", operator, s.setMailboxPassword)
	r("PUT /api/v1/mail/mailboxes/{address}/quota", operator, s.setMailboxQuota)
	r("DELETE /api/v1/mail/mailboxes/{address}", operator, s.deleteMailbox)
	r("GET /api/v1/mail/aliases", viewer, s.mailAliases)
	r("POST /api/v1/mail/aliases", operator, s.addMailAlias)
	r("DELETE /api/v1/mail/aliases", operator, s.deleteMailAlias)

	r("GET /api/v1/system", viewer, s.systemInfo)
	r("GET /api/v1/system/version", viewer, s.systemVersion)
	r("POST /api/v1/system/update/check", operator, s.checkUpdate)
	r("POST /api/v1/system/update", admin, s.selfUpdate)
	r("POST /api/v1/system/roll-sites", admin, s.rollSites)

	r("GET /api/v1/security/bans", viewer, s.listBans)
	r("POST /api/v1/security/bans", operator, s.addBan)
	r("DELETE /api/v1/security/bans", operator, s.removeBan)
	r("GET /api/v1/security/events", viewer, s.securityEvents)
	r("GET /api/v1/security/settings", viewer, s.securitySettings)
	r("PUT /api/v1/security/settings", admin, s.setSecuritySettings)
	r("GET /api/v1/security/reputation", viewer, s.reputationStatus)
	r("POST /api/v1/security/reputation/refresh", admin, s.refreshReputation)

	static, _ := fs.Sub(web.Static, "static")
	mux.Handle("GET /", http.FileServerFS(static))

	return securityHeaders(mux)
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// writeError maps an error to a status and a JSON body.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, site.ErrDomainTaken), errors.Is(err, site.ErrConflict),
		errors.Is(err, mail.ErrConflict), errors.Is(err, mail.ErrDisabled), errors.Is(err, errConflict),
		errors.Is(err, sftp.ErrConflict),
		errors.Is(err, errLastAdmin):
		status = http.StatusConflict
	case errors.Is(err, site.ErrInvalidDomain), errors.Is(err, site.ErrInvalidInput), errors.Is(err, errBadRequest),
		errors.Is(err, mail.ErrInvalid), errors.Is(err, sftp.ErrInvalid), errors.Is(err, adminer.ErrInvalid),
		errors.Is(err, backup.ErrWrongPassword):
		status = http.StatusBadRequest
	case errors.Is(err, errUnauthorized), errors.Is(err, errBadLogin):
		status = http.StatusUnauthorized
	case errors.Is(err, errForbidden):
		status = http.StatusForbidden
	case errors.Is(err, errTooMany):
		status = http.StatusTooManyRequests
	}
	if status == http.StatusInternalServerError {
		s.Log.Error("api", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
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
	// Provisioning runs as a job; the admin credentials are its secret,
	// shown only to whoever created the site and never stored.
	st, id, err := s.Sites.StartCreate(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, map[string]any{"site": st, "job_id": id})
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Store.GetSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if st.ParentID == "" && auth.Level(principalFrom(r.Context()).Role) < auth.Level(admin) {
		return errForbidden
	}
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
		// The shield's pages and the tools on sites' domains (Adminer, with
		// its own nonce-based CSP) aren't the panel.
		if !strings.HasPrefix(r.URL.Path, "/_shield/") && !strings.HasPrefix(r.URL.Path, "/_wpgenie/") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
			w.Header().Set("X-Frame-Options", "DENY")
		}
		h.ServeHTTP(w, r)
	})
}
