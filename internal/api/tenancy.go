package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Tenants (users of customer and reseller accounts) reach only the routes
// listed in tenantRoutes: every other route is staff-only, whatever it is
// and whenever it was added. Tenant roles have no staff level, so the
// staff role check refuses them everywhere else too.
//
// Ownership is checked here, centrally, never in handlers: a route whose
// path has a site ({id} under /sites/), an account ({id} under
// /accounts/) or a job ({id} under /jobs/) is only served when the tenant
// owns it: their own account's, or for a reseller also their customers'.
// Anything else answers 404, as if it didn't exist. Other path values
// ({user}, {domain}, {repo}, {backup}) are parts of that site or account,
// and the handlers look them up within it. A tenant route with a path
// value of unknown ownership makes the server refuse to start.

// tenantRule opens a route to tenants.
type tenantRule struct {
	// reseller: users of reseller accounts only.
	reseller bool
	// child: the {id} account must be one of the reseller's customers,
	// never their own account (suspend, terminate, change the plan).
	child bool
	// feature: the site's plan must include it.
	feature string
}

var (
	anyTenant    = tenantRule{}
	resellerOnly = tenantRule{reseller: true}
	resellerKids = tenantRule{reseller: true, child: true}
)

func feature(f string) tenantRule { return tenantRule{feature: f} }

// tenantRoutes: everything tenants may call. Server settings, the mail
// server, backup destinations, bans and server-wide lists, users outside
// their accounts, plans' definitions, billing settings and self-update stay
// staff-only by not being here. Resource changes are checked against the
// plan by their handlers.
var tenantRoutes = map[string]tenantRule{
	// Their own user: password, two-factor, sessions, API tokens.
	"GET /api/v1/account":                              anyTenant,
	"PUT /api/v1/account/password":                     anyTenant,
	"POST /api/v1/account/totp":                        anyTenant,
	"GET /api/v1/account/totp/qr.svg":                  anyTenant,
	"PUT /api/v1/account/totp":                         anyTenant,
	"DELETE /api/v1/account/totp":                      anyTenant,
	"POST /api/v1/account/recovery-codes":              anyTenant,
	"GET /api/v1/account/sessions":                     anyTenant,
	"DELETE /api/v1/account/sessions/{id}":             anyTenant,
	"GET /api/v1/account/tokens":                       anyTenant,
	"POST /api/v1/account/tokens":                      anyTenant,
	"DELETE /api/v1/account/tokens/{id}":               anyTenant,
	"GET /api/v1/usage":                                anyTenant,
	"GET /api/v1/plans":                                anyTenant,
	"GET /api/v1/php":                                  anyTenant,
	"GET /api/v1/dns-check":                            anyTenant, // public DNS; no server names
	"GET /api/v1/security/events":                      anyTenant, // filtered to their sites
	"GET /api/v1/jobs":                                 anyTenant, // filtered to their sites and jobs
	"GET /api/v1/jobs/{id}":                            anyTenant,
	"DELETE /api/v1/jobs/{id}/secret":                  anyTenant,
	"GET /api/v1/accounts":                             resellerOnly,
	"POST /api/v1/accounts":                            resellerOnly, // a customer of theirs
	"GET /api/v1/accounts/{id}":                        anyTenant,
	"PUT /api/v1/accounts/{id}":                        resellerKids,
	"POST /api/v1/accounts/{id}/suspend":               resellerKids,
	"POST /api/v1/accounts/{id}/unsuspend":             resellerKids,
	"POST /api/v1/accounts/{id}/terminate":             resellerKids,
	"GET /api/v1/accounts/{id}/usage":                  anyTenant,
	"POST /api/v1/accounts/{id}/usage/measure":         anyTenant,
	"GET /api/v1/accounts/{id}/events":                 anyTenant,
	"GET /api/v1/accounts/{id}/users":                  resellerOnly,
	"POST /api/v1/accounts/{id}/users":                 resellerOnly,
	"PUT /api/v1/accounts/{id}/users/{user}":           resellerOnly,
	"POST /api/v1/accounts/{id}/users/{user}/password": resellerOnly,
	"DELETE /api/v1/accounts/{id}/users/{user}":        resellerOnly,
	"POST /api/v1/accounts/{id}/sso":                   resellerKids,

	// Their sites.
	"GET /api/v1/sites":                                       anyTenant, // filtered
	"POST /api/v1/sites":                                      anyTenant, // within the plan
	"GET /api/v1/sites/{id}":                                  anyTenant,
	"DELETE /api/v1/sites/{id}":                               anyTenant,
	"PUT /api/v1/sites/{id}/shield":                           anyTenant,
	"GET /api/v1/sites/{id}/stats":                            anyTenant,
	"PUT /api/v1/sites/{id}/resources":                        anyTenant, // within the plan
	"PUT /api/v1/sites/{id}/cache":                            anyTenant,
	"POST /api/v1/sites/{id}/cache/purge":                     anyTenant,
	"PUT /api/v1/sites/{id}/autoscale":                        anyTenant, // within the plan
	"GET /api/v1/sites/{id}/events":                           anyTenant,
	"GET /api/v1/sites/{id}/metrics":                          anyTenant,
	"GET /api/v1/sites/{id}/updates":                          anyTenant,
	"POST /api/v1/sites/{id}/updates":                         anyTenant,
	"GET /api/v1/sites/{id}/updates/history":                  anyTenant,
	"PUT /api/v1/sites/{id}/auto-update":                      anyTenant,
	"POST /api/v1/sites/{id}/scan":                            anyTenant,
	"GET /api/v1/sites/{id}/scan":                             anyTenant,
	"POST /api/v1/sites/{id}/plugins":                         anyTenant,
	"GET /api/v1/sites/{id}/plugins":                          anyTenant,
	"GET /api/v1/sites/{id}/analysis":                         anyTenant,
	"POST /api/v1/sites/{id}/analysis/fix":                    anyTenant, // each fix is a route tenants have
	"GET /api/v1/sites/{id}/wp-admin/users":                   anyTenant,
	"POST /api/v1/sites/{id}/wp-admin/login":                  anyTenant,
	"POST /api/v1/sites/{id}/wp-admin/password":               anyTenant,
	"GET /api/v1/optimizations":                               anyTenant,
	"PUT /api/v1/sites/{id}/optimize":                         anyTenant,
	"POST /api/v1/sites/{id}/optimize/cleanup":                anyTenant,
	"GET /api/v1/sites/{id}/tables":                           anyTenant,
	"GET /api/v1/sites/{id}/dns-check":                        anyTenant,
	"POST /api/v1/sites/{id}/domains":                         anyTenant, // within the plan
	"PUT /api/v1/sites/{id}/domains/{domain}":                 anyTenant,
	"DELETE /api/v1/sites/{id}/domains/{domain}":              anyTenant,
	"PUT /api/v1/sites/{id}/primary-domain":                   anyTenant,
	"PUT /api/v1/sites/{id}/php":                              anyTenant,
	"PUT /api/v1/sites/{id}/images":                           anyTenant,
	"POST /api/v1/sites/{id}/images/convert":                  anyTenant,
	"GET /api/v1/sites/{id}/insights":                         anyTenant,
	"DELETE /api/v1/sites/{id}/insights/errors":               anyTenant,
	"GET /api/v1/sites/{id}/certificate":                      anyTenant,
	"PUT /api/v1/sites/{id}/certificate":                      feature(billing.FeatureCertificates),
	"DELETE /api/v1/sites/{id}/certificate":                   feature(billing.FeatureCertificates),
	"GET /api/v1/sites/{id}/backups":                          feature(billing.FeatureBackups),
	"GET /api/v1/sites/{id}/backups/destinations":             feature(billing.FeatureBackups),
	"PUT /api/v1/sites/{id}/backups/policy":                   feature(billing.FeatureBackups), // plan's destinations only
	"POST /api/v1/sites/{id}/backups":                         feature(billing.FeatureBackups),
	"POST /api/v1/sites/{id}/backups/restore":                 feature(billing.FeatureBackups),
	"GET /api/v1/sites/{id}/backups/{repo}/{backup}/download": feature(billing.FeatureBackups),
	"POST /api/v1/sites/{id}/staging":                         feature(billing.FeatureStaging), // within the plan
	"POST /api/v1/sites/{id}/push":                            feature(billing.FeatureStaging),
	"GET /api/v1/sites/{id}/sftp":                             feature(billing.FeatureSFTP),
	"POST /api/v1/sites/{id}/sftp":                            feature(billing.FeatureSFTP),
	"PUT /api/v1/sites/{id}/sftp/{user}/keys":                 feature(billing.FeatureSFTP),
	"PUT /api/v1/sites/{id}/sftp/{user}/password":             feature(billing.FeatureSFTP),
	"DELETE /api/v1/sites/{id}/sftp/{user}":                   feature(billing.FeatureSFTP),
	"POST /api/v1/sites/{id}/adminer":                         feature(billing.FeatureAdminer),
	"GET /api/v1/sites/{id}/files":                            feature(billing.FeatureFiles),
	"GET /api/v1/sites/{id}/files/content":                    feature(billing.FeatureFiles),
	"GET /api/v1/sites/{id}/files/download":                   feature(billing.FeatureFiles),
	"PUT /api/v1/sites/{id}/files/content":                    feature(billing.FeatureFiles),
	"POST /api/v1/sites/{id}/files/folder":                    feature(billing.FeatureFiles),
	"POST /api/v1/sites/{id}/files/move":                      feature(billing.FeatureFiles),
	"POST /api/v1/sites/{id}/files/copy":                      feature(billing.FeatureFiles),
	"PUT /api/v1/sites/{id}/files/mode":                       feature(billing.FeatureFiles),
	"POST /api/v1/sites/{id}/files/extract":                   feature(billing.FeatureFiles),
	"DELETE /api/v1/sites/{id}/files":                         feature(billing.FeatureFiles),
	"PUT /api/v1/sites/{id}/smtp":                             feature(billing.FeatureSMTP),
	"GET /api/v1/sites/{id}/cdn":                              feature(billing.FeatureCDN),
	"PUT /api/v1/sites/{id}/cdn":                              feature(billing.FeatureCDN),
	"POST /api/v1/sites/{id}/cdn/purge":                       feature(billing.FeatureCDN),
}

type scope int

const (
	scopeNone    scope = iota
	scopeSelf          // the caller's own user (handlers filter by user)
	scopeSite          // {id} is a site
	scopeAccount       // {id} is an account
	scopeJob           // {id} is a job
)

// pathOf is a route pattern's path ("GET /a/b" -> "/a/b").
func pathOf(pattern string) string {
	_, path, _ := strings.Cut(pattern, " ")
	return path
}

// routeScope says what a route's {id} is, from its path.
func routeScope(pattern string) scope {
	path := pathOf(pattern)
	switch {
	case strings.HasPrefix(path, "/api/v1/sites/{id}"):
		return scopeSite
	case strings.HasPrefix(path, "/api/v1/accounts/{id}"):
		return scopeAccount
	case strings.HasPrefix(path, "/api/v1/jobs/{id}"):
		return scopeJob
	case path == "/api/v1/account" || strings.HasPrefix(path, "/api/v1/account/"):
		return scopeSelf
	}
	return scopeNone
}

// checkTenantRoute refuses, at startup, a tenant route whose path values
// the ownership check doesn't cover.
func checkTenantRoute(pattern string) {
	rule, ok := tenantRoutes[pattern]
	if !ok {
		return
	}
	sc := routeScope(pattern)
	if sc == scopeNone && strings.Contains(pattern, "{") {
		panic("api: tenant route " + pattern + " has path values of unknown ownership")
	}
	if rule.child && sc != scopeAccount {
		panic("api: tenant route " + pattern + " is for customer accounts but has no account")
	}
	if rule.feature != "" && sc != scopeSite {
		panic("api: tenant route " + pattern + " needs a plan feature but has no site")
	}
}

// tenant is what the route wrapper resolved for a tenant request.
type tenant struct {
	Account *store.Account
	Limits  billing.Limits
	// Suspended: the account (or its reseller) is suspended.
	Suspended bool
	// For routes on a site: the account owning it (the tenant's own or,
	// for a reseller, a customer's) and that account's limits.
	SiteAccount *store.Account
	SiteLimits  billing.Limits
}

type tenantKey struct{}

// tenantOf returns the tenant making a request, or nil for staff and the
// API token.
func tenantOf(r *http.Request) *tenant {
	t, _ := r.Context().Value(tenantKey{}).(*tenant)
	return t
}

var (
	errTenantForbidden = errors.New("forbidden: not available to customer accounts")
	errSuspended       = errors.New("forbidden: your account is suspended; contact your provider")
)

// authorizeTenant decides a tenant's request. It returns the request's
// context with the tenant attached, or the status and error to refuse it
// with.
func (s *Server) authorizeTenant(r *http.Request, pattern string, p *Principal) (context.Context, int, error) {
	rule, ok := tenantRoutes[pattern]
	if !ok || (rule.reseller && p.Role != auth.RoleReseller) {
		return nil, http.StatusForbidden, errTenantForbidden
	}
	ctx := r.Context()
	if s.Billing == nil {
		return nil, http.StatusForbidden, errTenantForbidden
	}
	acct, err := s.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return nil, http.StatusForbidden, errTenantForbidden
	}
	t := &tenant{Account: acct}
	if t.Suspended, err = s.Billing.Suspended(ctx, acct); err != nil {
		return nil, http.StatusForbidden, errTenantForbidden
	}
	sc := routeScope(pattern)
	mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
	// A suspended account can still sign in, read, and look after its own
	// user (password, two-factor), but change nothing.
	if t.Suspended && mutating && sc != scopeSelf {
		return nil, http.StatusForbidden, errSuspended
	}
	if t.Limits, err = s.Billing.LimitsFor(ctx, acct); err != nil {
		return nil, http.StatusForbidden, errTenantForbidden
	}
	notFound := fmt.Errorf("%w", store.ErrNotFound)
	switch sc {
	case scopeSite:
		owner, ok := s.siteOwnerInScope(ctx, p, r.PathValue("id"))
		if !ok {
			return nil, http.StatusNotFound, notFound
		}
		t.SiteAccount = owner
		if t.SiteLimits, err = s.Billing.LimitsFor(ctx, owner); err != nil {
			return nil, http.StatusForbidden, errTenantForbidden
		}
		// A reseller's customer may be suspended while the reseller isn't:
		// its sites stay frozen for the reseller too.
		if owner.ID != acct.ID && mutating {
			if suspended, err := s.Billing.Suspended(ctx, owner); err != nil || suspended {
				return nil, http.StatusForbidden, errors.New("forbidden: the account owning this site is suspended")
			}
		}
	case scopeAccount:
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return nil, http.StatusNotFound, notFound
		}
		a, ok := s.accountInScope(ctx, p, id)
		if !ok || (rule.child && a.ID == p.AccountID) {
			return nil, http.StatusNotFound, notFound
		}
	case scopeJob:
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || !s.jobVisible(ctx, p, id) {
			return nil, http.StatusNotFound, notFound
		}
	}
	if rule.feature != "" && !t.SiteLimits.Has(rule.feature) {
		return nil, http.StatusForbidden, fmt.Errorf("forbidden: your plan doesn't include %s", rule.feature)
	}
	return context.WithValue(ctx, tenantKey{}, t), 0, nil
}

// checkTenantDomain refuses a tenant a domain whose mail this server
// hosts: the mail server treats every address on it as local, so a site
// on it would get a sender mailbox (SMTP) on someone else's mail domain,
// DKIM-signed as theirs. (Domains not yet on the server are first come,
// first served, as on most shared hosting: see the architecture notes.)
func (s *Server) checkTenantDomain(r *http.Request, raw string) error {
	if tenantOf(r) == nil || raw == "" {
		return nil
	}
	d, err := site.NormalizeDomain(raw)
	if err != nil {
		return nil // the site service reports it
	}
	hosted, err := s.Store.MailDomainExists(r.Context(), d)
	if err != nil {
		return err
	}
	if hosted || (s.Mail != nil && s.mailHost() == d) {
		return fmt.Errorf("%w: mail for %s is hosted on this server; ask your provider to add the site", errForbidden, d)
	}
	return nil
}

// mailHost is the mail server's hostname ("" when mail is off).
func (s *Server) mailHost() string {
	if s.Mail == nil {
		return ""
	}
	host, _ := s.Mail.Webmail()
	return host
}

// accountInScope returns an account the tenant may see: their own, or for
// a reseller one of their customers.
func (s *Server) accountInScope(ctx context.Context, p *Principal, id int64) (*store.Account, bool) {
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		return nil, false
	}
	if a.ID == p.AccountID || (p.Role == auth.RoleReseller && a.ParentID == p.AccountID) {
		return a, true
	}
	return nil, false
}

// siteOwnerInScope returns the account owning a site, if it is in the
// tenant's scope. Sites no account owns are staff-only.
func (s *Server) siteOwnerInScope(ctx context.Context, p *Principal, siteID string) (*store.Account, bool) {
	if siteID == "" {
		return nil, false
	}
	o, err := s.Store.SiteOwnerOf(ctx, siteID)
	if err != nil {
		return nil, false
	}
	return s.accountInScope(ctx, p, o.AccountID)
}

// scopeAccountIDs are the accounts a tenant sees: their own and, for a
// reseller, their customers'.
func (s *Server) scopeAccountIDs(ctx context.Context, p *Principal) ([]int64, error) {
	ids := []int64{p.AccountID}
	if p.Role == auth.RoleReseller {
		kids, err := s.Store.ListAccounts(ctx, store.AccountFilter{ParentID: p.AccountID})
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			ids = append(ids, k.ID)
		}
	}
	return ids, nil
}

// ownedSites maps the sites in a tenant's scope to their owning account.
func (s *Server) ownedSites(ctx context.Context, p *Principal) (map[string]int64, error) {
	ids, err := s.scopeAccountIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	owners, err := s.Store.SiteOwners(ctx, ids...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(owners))
	for _, o := range owners {
		out[o.SiteID] = o.AccountID
	}
	return out, nil
}

// jobVisible: a tenant sees jobs of their sites, and jobs they started
// (a failed create's site is gone, its job isn't).
func (s *Server) jobVisible(ctx context.Context, p *Principal, id int64) bool {
	j, err := s.Store.GetJob(ctx, id)
	if err != nil {
		return false
	}
	if _, ok := s.siteOwnerInScope(ctx, p, j.SiteID); ok {
		return true
	}
	owner, err := s.Store.JobOwner(ctx, id)
	return err == nil && owner != "" && owner == p.owner()
}

// sortedKeys returns a map's keys, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
