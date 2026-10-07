package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/store"
)

// Accounts, plans and the provisioning API (see internal/billing). Staff
// see and manage every account; a reseller their own and their customers';
// a customer only their own. Scope is checked by the route wrapper; the
// handlers only decide what each may change.

// accountView is an account as the API shows it.
type accountView struct {
	*store.Account
	Plan       *store.Plan    `json:"plan"`
	Limits     billing.Limits `json:"limits"`
	Suspended  bool           `json:"effectively_suspended"`
	Sites      int            `json:"sites"`
	ParentName string         `json:"parent_name,omitempty"`
}

func (s *Server) viewAccount(r *http.Request, a *store.Account) (*accountView, error) {
	ctx := r.Context()
	v := &accountView{Account: a}
	var err error
	if v.Plan, err = s.Store.GetPlan(ctx, a.PlanID); err != nil {
		return nil, err
	}
	if v.Limits, err = s.Billing.LimitsFor(ctx, a); err != nil {
		return nil, err
	}
	if v.Suspended, err = s.Billing.Suspended(ctx, a); err != nil {
		return nil, err
	}
	owned, err := s.Store.SiteOwners(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	v.Sites = len(owned)
	if a.ParentID != 0 {
		if p, err := s.Store.GetAccount(ctx, a.ParentID); err == nil {
			v.ParentName = p.Name
		}
	}
	return v, nil
}

func accountParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%w", store.ErrNotFound)
	}
	return id, nil
}

func (s *Server) accountFromPath(r *http.Request) (*store.Account, error) {
	id, err := accountParam(r)
	if err != nil {
		return nil, err
	}
	return s.Store.GetAccount(r.Context(), id)
}

// listAccounts: staff filter by billing IDs or reseller (?whmcs_service_id=,
// ?stripe_customer_id=, ?parent=); a reseller sees their own account and
// their customers'.
func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := store.AccountFilter{WHMCSServiceID: q.Get("whmcs_service_id"), StripeCustomerID: q.Get("stripe_customer_id")}
	if v := q.Get("parent"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return errBadRequest
		}
		f.ParentID = n
	}
	if t := tenantOf(r); t != nil {
		f.Scope = t.Account.ID
	}
	whmcsNamespace(r, &f, q.Get("parent") != "")
	list, err := s.Store.ListAccounts(r.Context(), f)
	if err != nil {
		return err
	}
	out := make([]*accountView, 0, len(list))
	for _, a := range list {
		v, err := s.viewAccount(r, a)
		if err != nil {
			return err
		}
		out = append(out, v)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) error {
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	v, err := s.viewAccount(r, a)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// createAccount is the provisioning API's "create account": the account
// and, optionally, its first user (with the given password, or a generated
// one returned once). An idempotency key (body or Idempotency-Key header)
// makes retries safe: the same key returns the account made the first
// time (200, existed) instead of a second one. Keys are per caller: a
// reseller can't look up another's accounts by guessing keys.
func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		billing.AccountInput
		IdempotencyKey string `json:"idempotency_key"`
		User           *struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"user"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	key := in.IdempotencyKey
	if key == "" {
		key = r.Header.Get("Idempotency-Key")
	}
	byReseller := false
	if key != "" {
		key = "staff:" + key
	}
	if t := tenantOf(r); t != nil {
		// A reseller creates customer accounts of their own, and never
		// touches Stripe's IDs (the panel owner's billing).
		if in.Kind != "" && in.Kind != store.AccountCustomer {
			return fmt.Errorf("%w: resellers create customer accounts", errForbidden)
		}
		if in.ParentID != 0 && in.ParentID != t.Account.ID {
			return fmt.Errorf("%w: customer accounts belong to you", errForbidden)
		}
		if in.StripeCustomerID != "" || in.StripeSubscriptionID != "" {
			return fmt.Errorf("%w: Stripe IDs are set by the panel's owner", errForbidden)
		}
		in.Kind, in.ParentID, byReseller = store.AccountCustomer, t.Account.ID, true
		if key != "" {
			key = fmt.Sprintf("account:%d:%s", t.Account.ID, strings.TrimPrefix(key, "staff:"))
		}
	}
	var first *store.NewUser
	password, generated := "", false
	if in.User != nil {
		if err := validUsername(in.User.Username); err != nil {
			return err
		}
		password = in.User.Password
		if password == "" {
			password, generated = auth.GeneratePassword(), true
		}
		if err := auth.ValidatePassword(password); err != nil {
			return fmt.Errorf("%w: %v", errBadRequest, err)
		}
		kind := in.Kind
		if kind == "" {
			kind = store.AccountCustomer
		}
		hash, err := auth.HashPassword(password)
		if err != nil {
			return err
		}
		first = &store.NewUser{Username: in.User.Username, PasswordHash: hash, Role: auth.TenantRole(kind)}
	}
	a, u, existed, err := s.Billing.CreateAccount(r.Context(), in.AccountInput, key, first, byReseller)
	if err != nil {
		return err
	}
	v, err := s.viewAccount(r, a)
	if err != nil {
		return err
	}
	if existed {
		return writeJSON(w, http.StatusOK, map[string]any{"account": v, "existed": true})
	}
	out := map[string]any{"account": v}
	if u != nil {
		out["user"] = u
		if generated {
			out["password"] = password
		}
	}
	return writeJSON(w, http.StatusCreated, out)
}

// updateAccount changes an account. Resellers may rename their customers,
// change their plan (one they may hand out), email and WHMCS service ID;
// the rest (kind, reseller, Stripe IDs) is the panel owner's.
func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name                 *string `json:"name"`
		PlanID               *string `json:"plan_id"`
		Email                *string `json:"email"`
		WHMCSServiceID       *string `json:"whmcs_service_id"`
		Kind                 *string `json:"kind"`
		ParentID             *int64  `json:"parent_id"`
		StripeCustomerID     *string `json:"stripe_customer_id"`
		StripeSubscriptionID *string `json:"stripe_subscription_id"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	t := tenantOf(r)
	if t != nil && (in.Kind != nil || in.ParentID != nil || in.StripeCustomerID != nil || in.StripeSubscriptionID != nil) {
		return fmt.Errorf("%w: only the panel's administrators change an account's kind, reseller or Stripe IDs", errForbidden)
	}
	upd := billing.AccountInput{Name: a.Name, Kind: a.Kind, PlanID: a.PlanID, ParentID: a.ParentID, Email: a.Email,
		WHMCSServiceID: a.WHMCSServiceID, StripeCustomerID: a.StripeCustomerID, StripeSubscriptionID: a.StripeSubscriptionID}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&upd.Name, in.Name)
	set(&upd.PlanID, in.PlanID)
	set(&upd.Email, in.Email)
	set(&upd.WHMCSServiceID, in.WHMCSServiceID)
	set(&upd.Kind, in.Kind)
	set(&upd.StripeCustomerID, in.StripeCustomerID)
	set(&upd.StripeSubscriptionID, in.StripeSubscriptionID)
	if in.ParentID != nil {
		upd.ParentID = *in.ParentID
	}
	a, err = s.Billing.UpdateAccount(r.Context(), a.ID, upd, t != nil)
	if err != nil {
		return err
	}
	v, err := s.viewAccount(r, a)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// suspendAccount: staff give a reason (admin, billing, overage; default
// admin); a reseller's suspension of their customer is always "reseller",
// which only they or an administrator lift.
func (s *Server) suspendAccount(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &in); err != nil {
			return err
		}
	}
	reason := in.Reason
	if t := tenantOf(r); t != nil {
		reason = billing.ReasonReseller
	} else if reason == "" {
		reason = billing.ReasonAdmin
	} else if reason == billing.ReasonReseller {
		return fmt.Errorf("%w: reason must be admin, billing or overage", errBadRequest)
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	a, err := s.Billing.Suspend(r.Context(), id, reason)
	if err != nil {
		return err
	}
	v, err := s.viewAccount(r, a)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// unsuspendAccount: staff lift any suspension; a reseller only their own.
func (s *Server) unsuspendAccount(w http.ResponseWriter, r *http.Request) error {
	by := billing.ReasonAdmin
	if tenantOf(r) != nil {
		by = billing.ReasonReseller
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	a, err := s.Billing.Unsuspend(r.Context(), id, by)
	if err != nil {
		return err
	}
	v, err := s.viewAccount(r, a)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// terminateAccount ends an account; with delete_sites its sites are
// deleted too. confirm must repeat the account's ID: deleting sites is
// irreversible (their backups stay in their destinations).
func (s *Server) terminateAccount(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Confirm     string `json:"confirm"`
		DeleteSites bool   `json:"delete_sites"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	if in.Confirm != strconv.FormatInt(id, 10) {
		return fmt.Errorf("%w: set confirm to the account's ID (%d) to terminate it", errBadRequest, id)
	}
	res, err := s.Billing.Terminate(r.Context(), id, in.DeleteSites)
	if err != nil && res == nil {
		return err
	}
	if err != nil {
		s.Log.Error("terminating an account", "account", id, "err", err)
		return writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "result": res})
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) error {
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	removeTicketFiles := s.ticketFilesOf(r.Context(), id)
	if err := s.Billing.DeleteAccount(r.Context(), id); err != nil {
		return err
	}
	removeTicketFiles()
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) accountUsage(w http.ResponseWriter, r *http.Request) error {
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	u, err := s.Billing.UsageOf(r.Context(), a)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, u)
}

// measureUsage measures the account's disk use now: at most every five
// minutes per account (it walks every file of every site).
func (s *Server) measureUsage(w http.ResponseWriter, r *http.Request) error {
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	now := s.now()
	if last, ok := s.measured.Load(a.ID); ok && now.Sub(last.(time.Time)) < 5*time.Minute {
		w.Header().Set("Retry-After", "300")
		return fmt.Errorf("%w: measured less than five minutes ago", errTooMany)
	}
	s.measured.Store(a.ID, now)
	if err := s.Billing.MeasureAccount(r.Context(), a); err != nil {
		return err
	}
	return s.accountUsage(w, r)
}

func (s *Server) accountEvents(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 50, 500)
	if err != nil {
		return err
	}
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	ev, err := s.Store.AccountEvents(r.Context(), id, limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ev)
}

// WHMCS service IDs are the caller's own billing system's: the panel
// owner's WHMCS numbers top-level accounts, a reseller's WHMCS their
// customers. The same number in the other's namespace is someone else's
// service, so lookups and reports only use the caller's namespace (else a
// reseller could steer the owner's WHMCS actions onto an account of theirs
// by reusing a service number).

// whmcsNamespace narrows a lookup by WHMCS service ID to the caller's
// namespace (staff: top-level accounts, unless they ask for a reseller's
// customers with ?parent=).
func whmcsNamespace(r *http.Request, f *store.AccountFilter, explicitParent bool) {
	if f.WHMCSServiceID == "" {
		return
	}
	if t := tenantOf(r); t != nil {
		f.ParentID = t.Account.ID
	} else if !explicitParent {
		f.TopLevel = true
	}
}

// usageReport is every visible account's usage this month (WHMCS's
// UsageUpdate): all for staff, a reseller's own and customers', a
// customer's own. ?whmcs_service_id= narrows it. whmcs_service_id is only
// reported for accounts in the caller's WHMCS namespace.
func (s *Server) usageReport(w http.ResponseWriter, r *http.Request) error {
	f := store.AccountFilter{WHMCSServiceID: r.URL.Query().Get("whmcs_service_id")}
	var list []*store.Account
	var err error
	t := tenantOf(r)
	switch {
	case t != nil && t.Account.Kind == store.AccountReseller:
		f.Scope = t.Account.ID
		whmcsNamespace(r, &f, false)
		list, err = s.Store.ListAccounts(r.Context(), f)
	case t != nil:
		if f.WHMCSServiceID == "" {
			list = []*store.Account{t.Account}
		}
	default:
		whmcsNamespace(r, &f, false)
		list, err = s.Store.ListAccounts(r.Context(), f)
	}
	if err != nil {
		return err
	}
	out, err := s.Billing.UsageReport(r.Context(), list)
	if err != nil {
		return err
	}
	parentOf := map[int64]int64{}
	for _, a := range list {
		parentOf[a.ID] = a.ParentID
	}
	for _, u := range out {
		ns := int64(0)
		if t != nil {
			ns = t.Account.ID
		}
		if parentOf[u.AccountID] != ns {
			u.WHMCSServiceID = ""
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// ---- Users of an account ----

// accountUser finds {user} (ID or name) among an account's users.
func (s *Server) accountUser(r *http.Request) (*store.User, error) {
	id, err := accountParam(r)
	if err != nil {
		return nil, err
	}
	u, err := s.userParamNamed(r, "user")
	if err != nil {
		return nil, err
	}
	if u.AccountID != id {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (s *Server) userParamNamed(r *http.Request, name string) (*store.User, error) {
	v := r.PathValue(name)
	if id, err := strconv.ParseInt(v, 10, 64); err == nil {
		return s.Store.GetUser(r.Context(), id)
	}
	return s.Store.UserByName(r.Context(), v)
}

func (s *Server) accountUsers(w http.ResponseWriter, r *http.Request) error {
	id, err := accountParam(r)
	if err != nil {
		return err
	}
	users, err := s.Store.AccountUsers(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, users)
}

func (s *Server) createAccountUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"` // empty: generated and returned once
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	if a.Status == store.AccountTerminated {
		return fmt.Errorf("%w: the account is terminated", errConflict)
	}
	if err := validUsername(in.Username); err != nil {
		return err
	}
	generated := in.Password == ""
	if generated {
		in.Password = auth.GeneratePassword()
	}
	if err := auth.ValidatePassword(in.Password); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return err
	}
	u, err := s.Store.CreateAccountUser(r.Context(), a.ID, in.Username, hash, auth.TenantRole(a.Kind))
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("%w: user %s already exists", errConflict, in.Username)
	}
	if err != nil {
		return err
	}
	out := map[string]any{"user": u}
	if generated {
		out["password"] = in.Password
	}
	return writeJSON(w, http.StatusCreated, out)
}

// updateAccountUser disables or enables a user of an account.
func (s *Server) updateAccountUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Disabled bool `json:"disabled"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.accountUser(r)
	if err != nil {
		return err
	}
	if in.Disabled && principalFrom(r.Context()).UserID == u.ID {
		return fmt.Errorf("%w: you can't disable yourself", errBadRequest)
	}
	if err := s.Store.SetUserRole(r.Context(), u.ID, u.Role, in.Disabled); err != nil {
		return err
	}
	if in.Disabled {
		s.Store.DeleteUserSessions(r.Context(), u.ID, "")
	}
	u, err = s.Store.GetUser(r.Context(), u.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, u)
}

// accountUserPassword sets a user's password (WHMCS's ChangePassword), or a
// generated one returned once, signs them out everywhere and revokes their
// API tokens.
func (s *Server) accountUserPassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &in); err != nil {
			return err
		}
	}
	u, err := s.accountUser(r)
	if err != nil {
		return err
	}
	pw := in.Password
	if pw == "" {
		pw = auth.GeneratePassword()
	}
	if err := auth.ValidatePassword(pw); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := s.Store.SetUserPassword(r.Context(), u.ID, hash); err != nil {
		return err
	}
	if err := s.Store.DeleteUserSessions(r.Context(), u.ID, principalFrom(r.Context()).SessionID); err != nil {
		return err
	}
	if err := s.Store.DeleteUserAPITokens(r.Context(), u.ID); err != nil {
		return err
	}
	if in.Password != "" {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) deleteAccountUser(w http.ResponseWriter, r *http.Request) error {
	u, err := s.accountUser(r)
	if err != nil {
		return err
	}
	if principalFrom(r.Context()).UserID == u.ID {
		return fmt.Errorf("%w: you can't delete your own account", errBadRequest)
	}
	// Their grants cascade; the SFTP logins they added to shared sites wouldn't.
	s.dropSharedAccess(r.Context(), u.ID)
	if err := s.Store.DeleteUser(r.Context(), u.ID); err != nil { // sessions and tokens cascade
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- Single sign-on ----

// ssoTTL is how long a sign-on link works: long enough to be clicked from
// a billing portal (and a second factor typed), short enough that one
// leaked from a log or a browser history is already dead.
const ssoTTL = 2 * time.Minute

// accountSSO makes a one-time sign-on link for a user of the account. The
// token travels in the URL's fragment, which browsers never send to a
// server or in a Referer; the dashboard exchanges it (POST /auth/sso) for
// a session like a sign-in. Staff can't be signed in this way.
func (s *Server) accountSSO(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"` // empty: the account's only (or first) enabled user
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &in); err != nil {
			return err
		}
	}
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	if a.Status == store.AccountTerminated {
		return fmt.Errorf("%w: the account is terminated", errConflict)
	}
	users, err := s.Store.AccountUsers(r.Context(), a.ID)
	if err != nil {
		return err
	}
	var u *store.User
	for _, x := range users {
		if !x.Disabled && (in.Username == "" || strings.EqualFold(x.Username, in.Username)) {
			u = x
			break
		}
	}
	if u == nil {
		return fmt.Errorf("%w: no enabled user of this account by that name", store.ErrNotFound)
	}
	tok := auth.RandomToken(32)
	expires := s.now().Add(ssoTTL)
	if err := s.Store.CreateSSOToken(r.Context(), auth.HashToken(tok), u.ID, expires); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"url": s.PanelURL + "/#sso=" + tok, "username": u.Username,
		"expires_at": expires})
}

// ssoLogin exchanges a sign-on token for a session. The token replaces the
// password, not the second factor: a user with two-factor authentication
// still enters a code (need_code, the token stays valid until it expires).
// A user without it, on a panel that requires it, gets a session that can
// only reach Account until they enrol, like after a password sign-in.
func (s *Server) ssoLogin(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	ctx, ip, now := r.Context(), clientIP(r), s.now()
	if len(in.Token) > 100 || len(in.Code) > 64 {
		return errBadLogin
	}
	ipk := ipKey(ip)
	ipk.key = "sso-" + ipk.key
	if d, first := s.guard.attempt(now, ipk); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		if first {
			s.audit(ctx, store.AuditEntry{Actor: "sso", IP: ip, Action: "sso_throttled", Status: http.StatusTooManyRequests})
		}
		return errTooMany
	}
	fail := func(actor, reason string) error {
		s.audit(ctx, store.AuditEntry{Actor: actor, IP: ip, Action: "sso_failed", Detail: reason, Status: http.StatusUnauthorized})
		return errBadLogin
	}
	hash := auth.HashToken(in.Token)
	uid, err := s.Store.SSOTokenUser(ctx, hash, now)
	if err != nil {
		return fail("sso", "invalid or expired link")
	}
	u, err := s.Store.GetUser(ctx, uid)
	if err != nil || u.Disabled {
		return fail("sso", "user disabled")
	}
	p, err := s.principalFor(ctx, u)
	if err != nil || !auth.IsTenant(p.Role) {
		return fail(u.Username, "account closed")
	}
	if u.TOTPEnabled {
		code := strings.TrimSpace(in.Code)
		if code == "" {
			s.guard.undo(ipk)
			return writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "enter the code from your authenticator app",
				"need_code": true, "username": u.Username})
		}
		// Codes count against the user like at sign-in.
		if d, _ := s.guard.attempt(now, userKey(u.Username)); d > 0 {
			return errTooMany
		}
		if step, err := auth.CheckTOTP(u.TOTPSecret, code, now, u.TOTPLastStep); err == nil {
			if ok, err := s.Store.UseTOTPStep(ctx, u.ID, step); err != nil {
				return err
			} else if !ok {
				return fail(u.Username, "two-factor code already used")
			}
		} else if ok, err := s.Store.UseRecoveryCode(ctx, u.ID, auth.HashRecoveryCode(code)); err != nil {
			return err
		} else if !ok {
			return fail(u.Username, "wrong two-factor code")
		}
		s.guard.forgive(userKey(u.Username))
	}
	// Single use, atomically: of two tabs racing with one link, one wins.
	if _, err := s.Store.ConsumeSSOToken(ctx, hash, now); err != nil {
		return fail(u.Username, "link already used")
	}
	s.guard.forgive(ipk)
	s.Store.PruneSessions(ctx, now, sessionIdle)
	s.Store.TouchLogin(ctx, u.ID, now)
	s.audit(ctx, store.AuditEntry{Actor: u.Username, IP: ip, Action: "sso_login", Target: u.Username, Status: http.StatusOK})
	return s.startSession(w, r, u)
}

// ---- Plans ----

// listPlans: staff see every plan; a reseller their own and those they may
// assign; a customer their own.
func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t := tenantOf(r)
	if t == nil {
		list, err := s.Store.ListPlans(ctx)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, list)
	}
	own, err := s.Store.GetPlan(ctx, t.Account.PlanID)
	if err != nil {
		return err
	}
	out := []*store.Plan{own}
	if t.Account.Kind == store.AccountReseller {
		more, err := s.Billing.ResellerPlans(ctx, t.Account)
		if err != nil {
			return err
		}
		for _, p := range more {
			if p.ID != own.ID {
				out = append(out, p)
			}
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) error {
	var p store.Plan
	if err := decode(w, r, &p); err != nil {
		return err
	}
	if err := billing.NormalizePlan(&p); err != nil {
		return err
	}
	if err := s.Store.CreatePlan(r.Context(), &p); err != nil {
		if errors.Is(err, store.ErrExists) {
			return fmt.Errorf("%w: plan %s already exists", errConflict, p.ID)
		}
		return err
	}
	out, err := s.Store.GetPlan(r.Context(), p.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updatePlan(w http.ResponseWriter, r *http.Request) error {
	var p store.Plan
	if err := decode(w, r, &p); err != nil {
		return err
	}
	p.ID = r.PathValue("id")
	if err := billing.NormalizePlan(&p); err != nil {
		return err
	}
	if err := s.Store.UpdatePlan(r.Context(), &p); err != nil {
		return err
	}
	out, err := s.Store.GetPlan(r.Context(), p.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) deletePlan(w http.ResponseWriter, r *http.Request) error {
	if err := s.Store.DeletePlan(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrInUse) {
			return fmt.Errorf("%w: accounts are on this plan", errConflict)
		}
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
