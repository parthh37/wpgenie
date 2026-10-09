// Package api exposes the panel's REST API, the embedded dashboard and the
// shield endpoints Caddy calls.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/backup"
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/files"
	"github.com/parthh37/wpgenie/internal/iprep"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/logship"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/monitor"
	"github.com/parthh37/wpgenie/internal/phpmyadmin"
	"github.com/parthh37/wpgenie/internal/sftp"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/support"
	"github.com/parthh37/wpgenie/internal/updater"
	"github.com/parthh37/wpgenie/internal/web"
	"github.com/parthh37/wpgenie/internal/wplogin"
)

type Server struct {
	Token      string
	Version    string
	Sites      *site.Service
	Store      *store.Store
	Shield     *shield.Shield
	Updater    *updater.Updater
	Mail       *mail.Service
	Jobs       *jobs.Queue
	SFTP       *sftp.Service
	PHPMyAdmin *phpmyadmin.Service
	// WPLogin signs panel users in to sites' wp-admin (optional).
	WPLogin *wplogin.Service
	// Files is the dashboard's file manager (optional).
	Files *files.Service
	// IP reputation data, for the status view (optional).
	Lists     *iprep.Lists
	Countries *iprep.Countries
	Log       *slog.Logger
	Now       func() time.Time // for tests
	// Billing is accounts, plans and billing hooks (see internal/billing);
	// without it tenants can reach nothing. PanelURL is the panel's public
	// origin (https://panel.example.com), for sign-on links and the Stripe
	// webhook's address.
	Billing  *billing.Service
	PanelURL string

	// Cluster is the control plane's registry of other servers (nil on a
	// node). Node is set on servers running `wpgenie agent`: the panel
	// signs people in; a node only takes requests the panel forwards (and
	// its local API token).
	Cluster *cluster.Controller
	Node    bool

	// Monitor serves /metrics and the alert settings (optional).
	Monitor *monitor.Service

	// Mailer sends e-mail to people: clients and staff (optional).
	Mailer *mailer.Service

	// Support is the help desk: tickets (optional; panel only).
	Support *support.Service
	// Logship ships logs to S3-compatible storage (optional).
	Logship *logship.Service

	guard loginGuard
	// storeGuard limits the public order form per client (invoicing.go).
	storeGuard loginGuard
	// routes is every route registered through route(), in order.
	routes []routeInfo
	// measured: account ID -> last on-demand disk measurement.
	measured sync.Map
	// mux is the route table, for MCP tools calling routes in-process
	// (mcp.go); oauth is the AI assistants' authorization codes (oauth.go).
	mux   *http.ServeMux
	oauth oauthState
}

// routeInfo is one authenticated route: its pattern and the staff role it
// needs (tenants: see tenantRoutes).
type routeInfo struct {
	Pattern string
	Role    string
}

// Roles a route needs (see auth.Role*).
const (
	viewer   = auth.RoleViewer
	operator = auth.RoleOperator
	admin    = auth.RoleAdmin
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.mux = mux

	// Shield endpoints: reached only through Caddy (loopback listener).
	mux.Handle("GET /_shield/check", s.Shield.CheckHandler()) // forward_auth always uses GET
	mux.Handle("POST /_shield/verify", s.Shield.VerifyHandler())

	// WPGenie tools on sites' own domains (reached only through Caddy's
	// /_wpgenie/* route, which names the site): phpMyAdmin, signing in to
	// wp-admin, and the brand's logo WordPress's admin shows.
	if s.PHPMyAdmin != nil {
		mux.Handle("GET "+phpmyadmin.Path, s.PHPMyAdmin)
		mux.Handle("POST "+phpmyadmin.Path, s.PHPMyAdmin)
	}
	if s.WPLogin != nil {
		mux.Handle("GET "+wplogin.Path, s.WPLogin)
	}
	mux.HandleFunc("GET "+site.BrandLogoPath, func(w http.ResponseWriter, r *http.Request) { s.Sites.ServeBrandLogo(w, r) })

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	// Signing in (on the panel only: a node only takes what the panel
	// forwards).
	if !s.Node {
		mux.Handle("GET /api/v1/auth/state", s.publicAuth(s.authState))
		mux.Handle("POST /api/v1/auth/setup", s.publicAuth(s.setup))
		mux.Handle("POST /api/v1/auth/login", s.publicAuth(s.login))
		mux.Handle("POST /api/v1/auth/logout", s.publicAuth(s.logout))
		mux.Handle("POST /api/v1/auth/sso", s.publicAuth(s.ssoLogin))
		// Stripe's webhook: no session; authenticated by its signature.
		mux.HandleFunc("POST /api/v1/billing/stripe/webhook", s.stripeWebhook)
	}

	s.routes = nil

	r := func(pattern, role string, h handlerFunc) {
		if s.Node && panelOnly(pattern) {
			return
		}
		s.route(mux, pattern, role, s.clustered(pattern, h))
	}
	if s.Cluster != nil {
		s.clusterRoutes(r)
	}
	s.monitoringRoutes(mux, r)
	s.mailerRoutes(r)
	// Built-in billing, support tickets and log shipping (each in its own
	// file; mux for their public endpoints).
	s.invoicingRoutes(mux, r)
	s.supportRoutes(mux, r)
	s.logshipRoutes(mux, r)
	s.sharingRoutes(r)
	s.edgeRoutes(r)
	// AI assistants: the MCP server and the OAuth that connects them.
	s.oauthRoutes(mux, r)
	s.mcpRoutes(mux)

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
	r("GET /api/v1/account/tokens", viewer, s.myTokens)
	r("POST /api/v1/account/tokens", viewer, s.createMyToken)
	r("DELETE /api/v1/account/tokens/{id}", viewer, s.deleteMyToken)

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
	r("GET /api/v1/tokens", admin, s.allTokens)
	r("DELETE /api/v1/tokens/{id}", admin, s.deleteAnyToken)
	r("POST /api/v1/users/{id}/tokens", admin, s.createUserToken)

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
	s.burstRoutes(r)
	r("GET /api/v1/sites/{id}/events", viewer, s.siteEvents)
	r("GET /api/v1/sites/{id}/metrics", viewer, s.siteMetrics)
	r("GET /api/v1/sites/{id}/updates", viewer, s.siteUpdates)
	r("POST /api/v1/sites/{id}/updates", operator, s.startUpdate)
	r("GET /api/v1/sites/{id}/updates/history", viewer, s.updateHistory)
	r("PUT /api/v1/sites/{id}/auto-update", operator, s.setAutoUpdate)
	r("POST /api/v1/sites/{id}/scan", operator, s.runScan)
	r("GET /api/v1/sites/{id}/scan", viewer, s.lastScan)
	r("POST /api/v1/sites/{id}/plugins", operator, s.analysePlugins)
	r("PUT /api/v1/sites/{id}/spread", admin, s.setSpread)
	r("GET /api/v1/sites/{id}/plugins", viewer, s.pluginReport)
	r("GET /api/v1/sites/{id}/analysis", viewer, s.siteAnalysis)
	r("POST /api/v1/sites/{id}/analysis/fix", operator, s.analysisFix)

	// WordPress itself: wp-admin without a password, administrators and
	// editors and their passwords, performance tweaks (see wordpress.go).
	r("GET /api/v1/sites/{id}/wp-admin/users", viewer, s.wpUsers)
	r("POST /api/v1/sites/{id}/wp-admin/users", operator, s.wpCreateUser)
	r("DELETE /api/v1/sites/{id}/wp-admin/users/{user}", operator, s.wpDeleteUser)
	r("POST /api/v1/sites/{id}/wp-admin/login", operator, s.wpLogin)
	r("POST /api/v1/sites/{id}/wp-admin/password", operator, s.wpAdminPassword)
	r("GET /api/v1/optimizations", viewer, s.optimizations)
	r("PUT /api/v1/sites/{id}/optimize", operator, s.setOptimize)
	r("POST /api/v1/sites/{id}/optimize/cleanup", operator, s.cleanupDB)
	s.toolsRoutes(r)
	s.hardeningRoutes(r)
	r("GET /api/v1/settings/branding", viewer, s.branding)
	r("PUT /api/v1/settings/branding", admin, s.setBranding)
	r("GET /api/v1/settings/branding/logo", viewer, s.brandLogo)

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
	r("GET /api/v1/dns-check", viewer, s.dnsCheck)
	r("GET /api/v1/sites/{id}/dns-check", viewer, s.siteDNSCheck)
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
	r("POST /api/v1/sites/{id}/phpmyadmin", operator, s.openPHPMyAdmin)
	s.fileRoutes(r)

	r("PUT /api/v1/sites/{id}/smtp", operator, s.setSiteSMTP)
	r("GET /api/v1/sites/{id}/cdn", viewer, s.cdnStatus)
	r("PUT /api/v1/sites/{id}/cdn", operator, s.setCDN)
	r("POST /api/v1/sites/{id}/cdn/purge", operator, s.purgeCDN)
	r("GET /api/v1/sites/{id}/offload", viewer, s.offloadStatus)
	r("PUT /api/v1/sites/{id}/offload", operator, s.setOffload)
	r("POST /api/v1/sites/{id}/offload/sync", operator, s.syncOffload)
	r("POST /api/v1/sites/{id}/offload/download", operator, s.downloadOffload)
	r("PUT /api/v1/sites/{id}/images", operator, s.setImages)
	r("POST /api/v1/sites/{id}/images/convert", operator, s.convertImages)
	r("GET /api/v1/sites/{id}/insights", viewer, s.insights)
	r("DELETE /api/v1/sites/{id}/insights/errors", operator, s.clearPHPErrors)

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

	// Accounts, plans and billing (the provisioning API): see accounts.go.
	r("GET /api/v1/accounts", viewer, s.listAccounts)
	r("POST /api/v1/accounts", admin, s.createAccount)
	r("GET /api/v1/accounts/{id}", viewer, s.getAccount)
	r("PUT /api/v1/accounts/{id}", admin, s.updateAccount)
	r("DELETE /api/v1/accounts/{id}", admin, s.deleteAccount)
	r("POST /api/v1/accounts/{id}/suspend", admin, s.suspendAccount)
	r("POST /api/v1/accounts/{id}/unsuspend", admin, s.unsuspendAccount)
	r("POST /api/v1/accounts/{id}/terminate", admin, s.terminateAccount)
	r("GET /api/v1/accounts/{id}/usage", viewer, s.accountUsage)
	r("POST /api/v1/accounts/{id}/usage/measure", operator, s.measureUsage)
	r("GET /api/v1/accounts/{id}/events", viewer, s.accountEvents)
	r("GET /api/v1/accounts/{id}/users", viewer, s.accountUsers)
	r("POST /api/v1/accounts/{id}/users", admin, s.createAccountUser)
	r("PUT /api/v1/accounts/{id}/users/{user}", admin, s.updateAccountUser)
	r("POST /api/v1/accounts/{id}/users/{user}/password", admin, s.accountUserPassword)
	r("DELETE /api/v1/accounts/{id}/users/{user}", admin, s.deleteAccountUser)
	r("POST /api/v1/accounts/{id}/sso", admin, s.accountSSO)
	r("GET /api/v1/usage", viewer, s.usageReport)
	r("GET /api/v1/plans", viewer, s.listPlans)
	r("POST /api/v1/plans", admin, s.createPlan)
	r("PUT /api/v1/plans/{id}", admin, s.updatePlan)
	r("DELETE /api/v1/plans/{id}", admin, s.deletePlan)
	r("PUT /api/v1/sites/{id}/account", admin, s.setSiteAccount)
	r("GET /api/v1/sites/{id}/backups/destinations", viewer, s.backupDestinations)
	r("GET /api/v1/billing/settings", admin, s.billingSettings)
	r("PUT /api/v1/billing/settings", admin, s.setBillingSettings)
	r("GET /api/v1/billing/webhooks", admin, s.webhookEndpoints)
	r("POST /api/v1/billing/webhooks", admin, s.createWebhookEndpoint)
	r("PUT /api/v1/billing/webhooks/{id}", admin, s.updateWebhookEndpoint)
	r("DELETE /api/v1/billing/webhooks/{id}", admin, s.deleteWebhookEndpoint)
	r("POST /api/v1/billing/webhooks/{id}/test", admin, s.testWebhookEndpoint)
	r("GET /api/v1/billing/deliveries", admin, s.webhookDeliveries)
	r("POST /api/v1/billing/deliveries/{id}/retry", admin, s.retryWebhookDelivery)

	static, _ := fs.Sub(web.Static, "static")
	mux.Handle("GET /", http.FileServerFS(static))
	// The panel is the React one (built into static/next). The classic one
	// stays at /classic/ until it's retired; both use the same #/ addresses,
	// so links in emails and payment returns land on the same screen.
	mux.HandleFunc("GET /{$}", panelPage(static, "next/index.html"))
	mux.HandleFunc("GET /classic/{$}", panelPage(static, "index.html"))
	mux.HandleFunc("GET /next/{$}", func(w http.ResponseWriter, r *http.Request) {
		// The browser keeps the #fragment across the redirect.
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})

	return securityHeaders(mux)
}

// panelPage serves one of the panel's HTML pages. They're revalidated on
// every load: they name the build's hashed scripts, so a stale copy after
// an upgrade would load the old panel (or none).
func panelPage(static fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, name)
	}
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
		errors.Is(err, sftp.ErrConflict), errors.Is(err, store.ErrConflict),
		errors.Is(err, errLastAdmin), errors.Is(err, billing.ErrConflict), errors.Is(err, store.ErrExists),
		errors.Is(err, store.ErrInUse), errors.Is(err, files.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, site.ErrInvalidDomain), errors.Is(err, site.ErrInvalidInput), errors.Is(err, errBadRequest),
		errors.Is(err, mail.ErrInvalid), errors.Is(err, sftp.ErrInvalid), errors.Is(err, phpmyadmin.ErrInvalid),
		errors.Is(err, backup.ErrWrongPassword), errors.Is(err, billing.ErrInvalid), errors.Is(err, billing.ErrBadSignature),
		errors.Is(err, cluster.ErrInvalid), errors.Is(err, files.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, errUnauthorized), errors.Is(err, errBadLogin):
		status = http.StatusUnauthorized
	case errors.Is(err, errForbidden), errors.Is(err, billing.ErrForbidden), errors.Is(err, billing.ErrQuota),
		errors.Is(err, files.ErrPermission):
		status = http.StatusForbidden
	case errors.Is(err, files.ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, errTooMany), errors.Is(err, wplogin.ErrTooMany):
		status = http.StatusTooManyRequests
	default:
		for _, e := range errorStatuses {
			if errors.Is(err, e.err) {
				status = e.status
				break
			}
		}
	}
	if status == http.StatusInternalServerError {
		s.Log.Error("api", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

var errBadRequest = errors.New("bad request")

// errorStatuses maps more packages' errors to statuses (see
// registerErrorStatus).
var errorStatuses []struct {
	err    error
	status int
}

// registerErrorStatus answers err (and errors wrapping it) with status;
// features in their own files call it from init().
func registerErrorStatus(err error, status int) {
	errorStatuses = append(errorStatuses, struct {
		err    error
		status int
	}{err, status})
}

// listSites lists every site for staff, and only their own (their
// customers' too, for a reseller) for tenants, with the owning account.
func (s *Server) listSites(w http.ResponseWriter, r *http.Request) error {
	sites, err := s.Store.ListSites(r.Context())
	if err != nil {
		return err
	}
	remote, err := s.clusterSites(r.Context())
	if err != nil {
		return err
	}
	// A site on its way here, or gone from here, is listed where it lives
	// (the registry), once.
	sites = slices.DeleteFunc(sites, func(st *store.Site) bool {
		return st.Status == store.StatusImporting || st.Status == store.StatusMoved
	})
	sites = append(sites, remote...)
	owners, shared, err := s.siteOwners(r)
	if err != nil {
		return err
	}
	counts, err := s.Store.SiteGrantCounts(r.Context())
	if err != nil {
		return err
	}
	out := make([]siteView, 0, len(sites))
	for _, st := range sites {
		acct, ok := owners[st.ID]
		if !ok && tenantOf(r) != nil {
			continue
		}
		v := siteView{Site: st, AccountID: acct, Access: shared[st.ID]}
		if v.Access == "" {
			v.SharedWith = counts[st.ID]
		}
		out = append(out, v)
	}
	return writeJSON(w, http.StatusOK, out)
}

// siteOwners maps sites to their accounts: the tenant's (with those shared
// with them, and their level in shared), or every owned site for staff.
func (s *Server) siteOwners(r *http.Request) (owners map[string]int64, shared map[string]string, err error) {
	if tenantOf(r) != nil {
		return s.visibleSites(r.Context(), principalFrom(r.Context()))
	}
	all, err := s.Store.AllSiteOwners(r.Context())
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]int64, len(all))
	for _, o := range all {
		out[o.SiteID] = o.AccountID
	}
	return out, nil, nil
}

func (s *Server) getSite(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Store.GetSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	v := siteView{Site: st}
	if o, err := s.Store.SiteOwnerOf(r.Context(), st.ID); err == nil {
		v.AccountID = o.AccountID
	}
	if t := tenantOf(r); t != nil {
		v.Access = t.Access
	}
	if v.Access == "" {
		if grants, err := s.Store.SiteGrants(r.Context(), st.ID); err == nil {
			v.SharedWith = len(grants)
		}
	}
	return writeJSON(w, http.StatusOK, v)
}

// createSite provisions a site. ?account= makes an account own it: a
// tenant's own account by default (or one of a reseller's customers),
// within the plan's site count and disk space; staff may give any account
// a site regardless of its plan.
func (s *Server) createSite(w http.ResponseWriter, r *http.Request) error {
	var in createSiteInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	if err := s.checkTenantDomain(r, in.Domain); err != nil {
		return err
	}
	acctID, err := s.targetAccount(r)
	if err != nil {
		return err
	}
	if acctID != 0 {
		// The check, the reservation and the ownership are one step: two
		// requests can't both take the last site of a plan.
		unlock := s.Billing.LockQuota()
		defer unlock()
		if tenantOf(r) != nil {
			if err := s.Billing.CheckNewSiteLocked(ctx, acctID); err != nil {
				return err
			}
		}
	}
	// Provisioning runs as a job; the admin credentials are its secret,
	// shown only to whoever created the site and never stored. On another
	// server when placement (or the admin) says so.
	st, id, remote, err := s.createOnNode(r, in)
	if err != nil {
		return err
	}
	if !remote {
		// Checked and reserved under the cluster's lock, like creates on
		// other servers: no two servers take one domain at once.
		if s.Cluster != nil {
			mu := s.Cluster.CreateLock()
			mu.Lock()
			st, id, err = s.Sites.StartCreate(ctx, in.CreateInput)
			mu.Unlock()
		} else {
			st, id, err = s.Sites.StartCreate(ctx, in.CreateInput)
		}
	}
	if err != nil {
		return err
	}
	if acctID != 0 {
		if err := s.assignNewSite(ctx, st.ID, acctID); err != nil {
			return err
		}
	}
	s.announceSite(id, st.ID, acctID)
	return writeJSON(w, http.StatusAccepted, map[string]any{"site": siteView{Site: st, AccountID: acctID}, "job_id": id})
}

// targetAccount is the account a new site is for (0: none, staff-only).
func (s *Server) targetAccount(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("account")
	t := tenantOf(r)
	if v == "" {
		if t != nil {
			return t.Account.ID, nil
		}
		return 0, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 0 {
		return 0, errBadRequest
	}
	if s.Billing == nil {
		return 0, fmt.Errorf("%w: accounts are not available", errBadRequest)
	}
	if t != nil {
		if _, ok := s.accountInScope(r.Context(), principalFrom(r.Context()), id); !ok {
			return 0, store.ErrNotFound
		}
		return id, nil
	}
	if id != 0 {
		if _, err := s.Store.GetAccount(r.Context(), id); err != nil {
			return 0, fmt.Errorf("%w: no account %d", errBadRequest, id)
		}
	}
	return id, nil
}

// assignNewSite records who owns a site whose creation job just started.
// Caller holds the quota lock.
func (s *Server) assignNewSite(ctx context.Context, siteID string, acctID int64) error {
	if err := s.Billing.AssignSiteLocked(ctx, siteID, acctID); err != nil {
		s.Log.Error("recording a new site's account", "site", siteID, "account", acctID, "err", err)
		return err
	}
	// A creation that failed at once has already rolled the site back (and
	// its ownership with it, or before it): don't keep a stray owner.
	if _, err := s.Store.GetSite(ctx, siteID); errors.Is(err, store.ErrNotFound) {
		s.Store.UnassignSite(ctx, siteID)
	}
	return nil
}

// announceSite waits for a creation job: on success, the owning account's
// state applies to the new site at once (an account suspended while its
// site was being created doesn't get it online until the next hourly
// reconcile), and site.created is sent.
func (s *Server) announceSite(jobID int64, siteID string, acctID int64) {
	if s.Billing == nil || s.Jobs == nil {
		return
	}
	go func() {
		ctx := context.Background()
		j, err := s.Jobs.WaitJob(ctx, jobID)
		if err != nil || j.Status != store.JobSucceeded {
			return
		}
		st, err := s.Store.GetSite(ctx, siteID)
		if err != nil {
			return
		}
		if acctID != 0 {
			if err := s.Billing.ApplyState(ctx, acctID); err != nil {
				s.Log.Warn("applying an account's state to its new site", "site", siteID, "err", err)
			}
		}
		if s.Billing.Hooks != nil {
			s.Billing.Hooks.Emit(ctx, billing.EventSiteCreated, map[string]any{"site_id": siteID,
				"account_id": acctID, "domain": st.PrimaryDomain})
		}
	}()
}

// deleteSite: operators may delete staging sites (they create them), live
// sites need an administrator; tenants may delete their own sites.
func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) error {
	if s.Cluster != nil {
		if node, remote, err := s.Cluster.SiteNode(r.Context(), r.PathValue("id")); err != nil {
			return err
		} else if remote {
			return s.deleteRemoteSite(w, r, node)
		}
	}
	st, err := s.Store.GetSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if st.ParentID == "" && tenantOf(r) == nil && auth.Level(principalFrom(r.Context()).Role) < auth.Level(admin) {
		return errForbidden
	}
	if t := tenantOf(r); st.ParentID == "" && t != nil && t.Access != "" {
		return errOwnerDeletes
	}
	var acctID int64
	if o, err := s.Store.SiteOwnerOf(r.Context(), st.ID); err == nil {
		acctID = o.AccountID
	}
	if err := s.Sites.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	if s.Billing != nil && s.Billing.Hooks != nil {
		s.Billing.Hooks.Emit(r.Context(), billing.EventSiteDeleted, map[string]any{"site_id": st.ID,
			"account_id": acctID, "domain": st.PrimaryDomain})
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
	if t := tenantOf(r); t != nil {
		st, err := s.siteRecord(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		most := in.Replicas
		if st.Autoscale {
			most = max(most, st.MaxReplicas)
		}
		if err := billing.CheckResources(t.SiteLimits, most, in.MemoryMB, in.CPUs); err != nil {
			return err
		}
	}
	if done, err := s.forwardAfterChecks(w, r, in); done || err != nil {
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
		// The shield's pages and the tools on sites' domains (phpMyAdmin, with
		// its own nonce-based CSP) aren't the panel.
		if !strings.HasPrefix(r.URL.Path, "/_shield/") && !strings.HasPrefix(r.URL.Path, "/_wpgenie/") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
			w.Header().Set("X-Frame-Options", "DENY")
		}
		h.ServeHTTP(w, r)
	})
}
