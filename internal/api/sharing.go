package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

// Sharing a site: its owners (the users of the account owning it, its
// reseller's, and staff) give users of other accounts access to it at a
// level: viewer, developer or manager (see auth.Access*). Those users
// reach the site through the tenant routes like its owners do, under the
// owner's plan, and each route needs a level: siteAccessRules, or by
// default viewer to read and developer to change. Only owners delete a
// live site or decide who it's shared with.
//
// Only existing users of other accounts can be given access: staff already
// reach every site, and users of the owning account (or its reseller) own
// it. Who a user is (their account, their other sites) is never shown to
// the owner, only the username they typed.

// accessOwner: the route is for the site's owners, never for users it is
// shared with.
const accessOwner = "owner"

var errOwnerDeletes = fmt.Errorf("%w: only the site's owner can delete a live site", errForbidden)

// siteAccessRules: tenant routes on a site whose level differs from the
// default (viewer for GET, developer for changes).
var siteAccessRules = map[string]string{
	// Reading what's inside the site: its files, its backups (a whole copy
	// of its database) and its WordPress users' addresses.
	"GET /api/v1/sites/{id}/files":                            auth.AccessDeveloper,
	"GET /api/v1/sites/{id}/files/content":                    auth.AccessDeveloper,
	"GET /api/v1/sites/{id}/files/download":                   auth.AccessDeveloper,
	"GET /api/v1/sites/{id}/backups/{repo}/{backup}/download": auth.AccessDeveloper,
	"GET /api/v1/sites/{id}/wp-admin/users":                   auth.AccessDeveloper,

	// What it costs (the owner's plan and burst minutes), where it answers
	// (domains, certificate, CDN), its mail and its protection.
	"PUT /api/v1/sites/{id}/resources":           auth.AccessManager,
	"PUT /api/v1/sites/{id}/autoscale":           auth.AccessManager,
	"PUT /api/v1/sites/{id}/burst":               auth.AccessManager,
	"POST /api/v1/sites/{id}/domains":            auth.AccessManager,
	"PUT /api/v1/sites/{id}/domains/{domain}":    auth.AccessManager,
	"DELETE /api/v1/sites/{id}/domains/{domain}": auth.AccessManager,
	"PUT /api/v1/sites/{id}/primary-domain":      auth.AccessManager,
	"PUT /api/v1/sites/{id}/certificate":         auth.AccessManager,
	"DELETE /api/v1/sites/{id}/certificate":      auth.AccessManager,
	"PUT /api/v1/sites/{id}/cdn":                 auth.AccessManager,
	"PUT /api/v1/sites/{id}/smtp":                auth.AccessManager,
	"PUT /api/v1/sites/{id}/shield":              auth.AccessManager,
	"DELETE /api/v1/sites/{id}/attack":           auth.AccessManager,

	// Deleting it: their staging copies (deleteSite keeps live sites to
	// their owners).
	"DELETE /api/v1/sites/{id}": auth.AccessDeveloper,

	// Who it's shared with.
	"GET /api/v1/sites/{id}/access":           accessOwner,
	"POST /api/v1/sites/{id}/access":          accessOwner,
	"PUT /api/v1/sites/{id}/access/{user}":    accessOwner,
	"DELETE /api/v1/sites/{id}/access/{user}": accessOwner,
}

// maxSiteGrants bounds how many users one site is shared with.
const maxSiteGrants = 25

func init() {
	registerTenantRoutes(map[string]tenantRule{
		"GET /api/v1/sites/{id}/access":           anyTenant,
		"POST /api/v1/sites/{id}/access":          anyTenant,
		"PUT /api/v1/sites/{id}/access/{user}":    anyTenant,
		"DELETE /api/v1/sites/{id}/access/{user}": anyTenant,
	})
	// Grants are the panel's, like accounts: never a site's server's.
	for _, p := range []string{"GET /api/v1/sites/{id}/access", "POST /api/v1/sites/{id}/access",
		"PUT /api/v1/sites/{id}/access/{user}", "DELETE /api/v1/sites/{id}/access/{user}",
		"GET /api/v1/sites/{id}/access/candidates"} {
		notForwarded[p] = true
	}
}

func (s *Server) sharingRoutes(r func(pattern, role string, h handlerFunc)) {
	r("GET /api/v1/sites/{id}/access", viewer, s.siteGrants)
	r("POST /api/v1/sites/{id}/access", operator, s.shareSite)
	r("PUT /api/v1/sites/{id}/access/{user}", operator, s.setSiteGrant)
	r("DELETE /api/v1/sites/{id}/access/{user}", operator, s.unshareSite)
	// Staff only (not a tenant route): who a site could be shared with.
	r("GET /api/v1/sites/{id}/access/candidates", operator, s.shareCandidates)
}

// requiredAccess is the level a tenant route on a site needs from a user
// the site is shared with.
func requiredAccess(pattern string) string {
	if a, ok := siteAccessRules[pattern]; ok {
		return a
	}
	if method, _, _ := strings.Cut(pattern, " "); method == http.MethodGet || method == http.MethodHead {
		return auth.AccessViewer
	}
	return auth.AccessDeveloper
}

// accessAllows reports whether a user with access may do what needs
// level ("" access: an owner, who may do anything).
func accessAllows(access, level string) bool {
	if access == "" {
		return true
	}
	return level != accessOwner && auth.AccessLevel(access) >= auth.AccessLevel(level)
}

// siteAccess returns the account owning a site the tenant may reach, and
// how: access is "" for a site in their scope (their account's, or for a
// reseller a customer's), else the level it was shared with them at.
func (s *Server) siteAccess(ctx context.Context, p *Principal, siteID string) (*store.Account, string, bool) {
	if owner, ok := s.siteOwnerInScope(ctx, p, siteID); ok {
		return owner, "", true
	}
	if siteID == "" || p.UserID == 0 {
		return nil, "", false
	}
	o, err := s.Store.SiteOwnerOf(ctx, siteID)
	if err != nil {
		return nil, "", false
	}
	g, err := s.Store.SiteGrant(ctx, siteID, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		// A staging copy is shared with whoever its live site is, at the
		// same level, while both are the same account's: so taking the live
		// site's access away takes its copies' too.
		st, err2 := s.siteRecord(ctx, siteID)
		if err2 != nil || st.ParentID == "" {
			return nil, "", false
		}
		if po, err2 := s.Store.SiteOwnerOf(ctx, st.ParentID); err2 != nil || po.AccountID != o.AccountID {
			return nil, "", false
		}
		g, err = s.Store.SiteGrant(ctx, st.ParentID, p.UserID)
	}
	if err != nil {
		return nil, "", false
	}
	a, err := s.Store.GetAccount(ctx, o.AccountID)
	if err != nil || a.Status == store.AccountTerminated {
		return nil, "", false
	}
	return a, g.Access, true
}

// stagingCopies are the staging copies of a site that its account owns.
func (s *Server) stagingCopies(ctx context.Context, siteID string, accountID int64) ([]string, error) {
	owned, err := s.Store.SiteOwners(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, o := range owned {
		if o.SiteID == siteID {
			continue
		}
		if st, err := s.siteRecord(ctx, o.SiteID); err == nil && st.ParentID == siteID {
			out = append(out, o.SiteID)
		}
	}
	return out, nil
}

// visibleSites maps the sites a tenant reaches to their owning account:
// those in their scope, and those shared with them (with their level in
// shared).
func (s *Server) visibleSites(ctx context.Context, p *Principal) (owners map[string]int64, shared map[string]string, err error) {
	owners, err = s.ownedSites(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	shared = map[string]string{}
	if p.UserID == 0 {
		return owners, shared, nil
	}
	grants, err := s.Store.UserSiteGrants(ctx, p.UserID)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range grants {
		if _, mine := owners[g.SiteID]; mine {
			continue
		}
		owners[g.SiteID], shared[g.SiteID] = g.AccountID, g.Access
		// Its staging copies come with it (siteAccess).
		copies, err := s.stagingCopies(ctx, g.SiteID, g.AccountID)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range copies {
			if _, seen := owners[id]; !seen {
				owners[id], shared[id] = g.AccountID, g.Access
			}
		}
	}
	return owners, shared, nil
}

// dropAccess removes what a user whose access to a shared site ended (or
// dropped to viewer) still holds there without the panel: the SFTP logins
// they added to it and its staging copies, wherever those live. (Files they
// changed, keys they added to others' logins and WordPress accounts they
// made stay: the owner checks those.)
func (s *Server) dropAccess(ctx context.Context, siteID string, userID int64) error {
	ctx = context.WithoutCancel(ctx)
	ids := []string{siteID}
	if o, err := s.Store.SiteOwnerOf(ctx, siteID); err == nil {
		copies, err := s.stagingCopies(ctx, siteID, o.AccountID)
		if err != nil {
			return err
		}
		ids = append(ids, copies...)
	}
	who := (&Principal{UserID: userID}).owner()
	var errs []error
	for _, id := range ids {
		if s.Cluster != nil {
			node, remote, err := s.Cluster.SiteNode(ctx, id)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if remote {
				path := "/cluster/v1/sites/" + url.PathEscape(id) + "/sftp?added_by=" + url.QueryEscape(who)
				if err := s.Cluster.Call(ctx, node, http.MethodDelete, path, nil, nil); err != nil {
					errs = append(errs, fmt.Errorf("site %s: %w", id, err))
				}
				continue
			}
		}
		if s.SFTP != nil {
			if _, err := s.SFTP.DeleteAddedBy(ctx, id, who); err != nil {
				errs = append(errs, fmt.Errorf("site %s: %w", id, err))
			}
		}
	}
	return errors.Join(errs...)
}

// dropSharedAccess is dropAccess for every site shared with a user, before
// the user is deleted (their grants go with them).
func (s *Server) dropSharedAccess(ctx context.Context, userID int64) {
	grants, err := s.Store.UserSiteGrants(ctx, userID)
	if err != nil {
		s.Log.Error("listing a deleted user's shared sites", "user", userID, "err", err)
		return
	}
	for _, g := range grants {
		if err := s.dropAccess(ctx, g.SiteID, userID); err != nil {
			s.Log.Error("removing a deleted user's SFTP logins", "site", g.SiteID, "user", userID, "err", err)
		}
	}
}

// errAccessLeft: access ended, but what the user held there couldn't all
// be removed.
func errAccessLeft(err error) error {
	return fmt.Errorf("their access is removed, but removing the SFTP logins they added failed (delete them under SFTP): %w", err)
}

// ---- Handlers (the site's owners and staff) ----

func (s *Server) siteGrants(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if _, err := s.siteRecord(r.Context(), id); err != nil {
		return err
	}
	list, err := s.Store.SiteGrants(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

// shareSite gives a user of another account access to the site.
func (s *Server) shareSite(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Access   string `json:"access"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if !auth.ValidAccess(in.Access) {
		return fmt.Errorf("%w: access must be viewer, developer or manager", errBadRequest)
	}
	if _, err := s.siteRecord(ctx, id); err != nil {
		return err
	}
	if _, err := s.Store.SiteOwnerOf(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: only a site that belongs to an account can be shared", errBadRequest)
		}
		return err
	}
	// Staff and unknown names answer alike: an owner learns only that a
	// customer's username exists, never that a staff one does.
	name := strings.TrimSpace(in.Username)
	u, err := s.Store.UserByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) || (err == nil && u.AccountID == 0) {
		return fmt.Errorf("%w: no user of another account is named %q", errBadRequest, name)
	}
	if err != nil {
		return err
	}
	them, err := s.principalFor(ctx, u)
	if err != nil {
		return fmt.Errorf("%w: no user of another account is named %q", errBadRequest, name)
	}
	if _, ok := s.siteOwnerInScope(ctx, them, id); ok {
		return fmt.Errorf("%w: %s already has this site through their account", errConflict, u.Username)
	}
	g := &store.SiteGrant{SiteID: id, UserID: u.ID, Access: in.Access, GrantedBy: principalFrom(ctx).Name}
	switch err := s.Store.CreateSiteGrant(ctx, g, maxSiteGrants); {
	case errors.Is(err, store.ErrExists):
		return fmt.Errorf("%w: the site is already shared with %s; change their access instead", errConflict, u.Username)
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("%w: a site can be shared with at most %d people", errConflict, maxSiteGrants)
	case err != nil:
		return err
	}
	g, err = s.Store.SiteGrant(ctx, id, u.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, g)
}

// grantUser is the {user} of a grant route: a user ID.
func grantUser(r *http.Request) (int64, error) {
	uid, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil || uid < 1 {
		return 0, store.ErrNotFound
	}
	return uid, nil
}

func (s *Server) setSiteGrant(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Access string `json:"access"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if !auth.ValidAccess(in.Access) {
		return fmt.Errorf("%w: access must be viewer, developer or manager", errBadRequest)
	}
	uid, err := grantUser(r)
	if err != nil {
		return err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := s.Store.SetSiteGrantAccess(ctx, id, uid, in.Access); err != nil {
		return err
	}
	// A viewer holds no SFTP logins.
	if !accessAllows(in.Access, auth.AccessDeveloper) {
		if err := s.dropAccess(ctx, id, uid); err != nil {
			return errAccessLeft(err)
		}
	}
	g, err := s.Store.SiteGrant(ctx, id, uid)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, g)
}

func (s *Server) unshareSite(w http.ResponseWriter, r *http.Request) error {
	uid, err := grantUser(r)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteSiteGrant(r.Context(), r.PathValue("id"), uid); err != nil {
		return err
	}
	if err := s.dropAccess(r.Context(), r.PathValue("id"), uid); err != nil {
		return errAccessLeft(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// shareCandidate is someone staff could share a site with.
type shareCandidate struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
}

// maxShareCandidates bounds the list; past it, staff type the username.
const maxShareCandidates = 500

// shareCandidates lists, for staff, the users a site can be shared with:
// users of other accounts that are still open, who don't already reach it
// through their account or a grant. Tenants never see it: who has a login
// on the panel is staff's to know.
func (s *Server) shareCandidates(w http.ResponseWriter, r *http.Request) error {
	ctx, id := r.Context(), r.PathValue("id")
	if _, err := s.siteRecord(ctx, id); err != nil {
		return err
	}
	users, err := s.Store.ListUsers(ctx)
	if err != nil {
		return err
	}
	grants, err := s.Store.SiteGrants(ctx, id)
	if err != nil {
		return err
	}
	has := map[int64]bool{}
	for _, g := range grants {
		has[g.UserID] = true
	}
	accounts := map[int64]*store.Account{}
	out := []shareCandidate{}
	for _, u := range users {
		if u.AccountID == 0 || has[u.ID] {
			continue
		}
		a, seen := accounts[u.AccountID]
		if !seen {
			if a, err = s.Store.GetAccount(ctx, u.AccountID); err != nil {
				a = nil
			}
			accounts[u.AccountID] = a
		}
		if a == nil || a.Status == store.AccountTerminated {
			continue
		}
		p, err := s.principalFor(ctx, u)
		if err != nil {
			continue
		}
		if _, mine := s.siteOwnerInScope(ctx, p, id); mine {
			continue
		}
		out = append(out, shareCandidate{UserID: u.ID, Username: u.Username, AccountID: a.ID, AccountName: a.Name})
		if len(out) == maxShareCandidates {
			break
		}
	}
	return writeJSON(w, http.StatusOK, out)
}
