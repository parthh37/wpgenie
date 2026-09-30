// Package billing is WPGenie's multi-tenant model: accounts (customers and
// resellers) on admin-defined plans, which sites they own, quotas, usage
// metering, suspension, and the hooks billing systems use (the provisioning
// API's rules, Stripe webhooks, outgoing webhooks).
//
// It never talks to Docker or Caddy itself: sites are suspended, brought
// back and deleted through SiteOps, and usage comes from UsageSource, so
// sites on other nodes plug in behind the same interfaces. Access control
// (who may call what) is the API's; this package enforces the rules every
// caller shares: plan limits, suspension authority, reseller allocation.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

var (
	ErrInvalid   = errors.New("invalid input")
	ErrQuota     = errors.New("plan limit reached")
	ErrForbidden = errors.New("not allowed")
	ErrConflict  = errors.New("conflict")
)

// Features a plan can include. Anything not listed in a tenant's plan (and
// their reseller's) is refused to them.
const (
	FeatureStaging      = "staging"      // staging copies and pushes
	FeatureBackups      = "backups"      // backups, restores, downloads, backup policy
	FeatureSFTP         = "sftp"         // SFTP logins
	FeatureFiles        = "files"        // the dashboard's file manager
	FeaturePHPMyAdmin   = "phpmyadmin"   // phpMyAdmin database access
	FeatureCertificates = "certificates" // their own TLS certificates
	FeatureCDN          = "cdn"          // Cloudflare / pull-zone CDNs
	FeatureSMTP         = "smtp"         // WordPress mail through the mail server
	FeatureBurst        = "burst"        // extra instances under load, within the plan's burst minutes
)

// Features lists every plan feature.
var Features = []string{FeatureStaging, FeatureBackups, FeatureSFTP, FeatureFiles, FeaturePHPMyAdmin, FeatureCertificates, FeatureCDN,
	FeatureSMTP, FeatureBurst}

// Suspension reasons: who suspended an account. Only the same party, or an
// administrator, lifts a suspension; a stronger reason replaces a weaker
// one (an administrator's suspension outranks a reseller's).
const (
	ReasonReseller = "reseller"
	ReasonOverage  = "overage"
	ReasonBilling  = "billing"
	ReasonAdmin    = "admin"
)

func reasonRank(r string) int {
	switch r {
	case ReasonReseller:
		return 1
	case ReasonOverage:
		return 2
	case ReasonBilling:
		return 3
	case ReasonAdmin:
		return 4
	}
	return 0
}

// MaxCustomersPerReseller bounds the accounts one reseller can create.
const MaxCustomersPerReseller = 1000

// Overage actions for bandwidth beyond the plan.
const (
	OverageNotify  = "notify"
	OverageSuspend = "suspend"
)

// SiteOps acts on sites wherever they run. site.Service implements it for
// this server; a cluster control plane routes to the site's node.
type SiteOps interface {
	Suspend(ctx context.Context, siteID string) error
	Unsuspend(ctx context.Context, siteID string) error
	Delete(ctx context.Context, siteID string) error
}

// UsageSource reports sites' usage. StoreUsage reads this server's
// traffic rollups and disk measurements; remote nodes' reports plug in
// here.
type UsageSource interface {
	// Bandwidth returns bytes served per site in [from, to).
	Bandwidth(ctx context.Context, siteIDs []string, from, to time.Time) (map[string]int64, error)
	// Disk returns the last disk measurement per site.
	Disk(ctx context.Context, siteIDs []string) (map[string]store.SiteUsage, error)
}

// DiskMeter measures a site's disk use now (site.Service on this server).
type DiskMeter interface {
	MeasureDisk(ctx context.Context, siteID string) (store.SiteUsage, error)
}

// StoreUsage is the UsageSource of this server.
type StoreUsage struct{ Store *store.Store }

func (u StoreUsage) Bandwidth(ctx context.Context, ids []string, from, to time.Time) (map[string]int64, error) {
	return u.Store.Bandwidth(ctx, ids, from, to)
}

func (u StoreUsage) Disk(ctx context.Context, ids []string) (map[string]store.SiteUsage, error) {
	return u.Store.SiteUsages(ctx, ids)
}

type Service struct {
	Store *store.Store
	Sites SiteOps
	// Usage defaults to StoreUsage; Meter measures disk (nil: never).
	Usage UsageSource
	Meter DiskMeter
	// Hooks sends outgoing webhooks (nil: none); Stripe calls Stripe's API
	// for subscriptions and metered usage.
	Hooks  *Webhooks
	Stripe *StripeAPI
	// Burst counts sites' burst minutes and pauses their burst wherever
	// they run (nil: burst isn't metered and never paused).
	Burst BurstOps
	// InWindow reports whether a time is in the nightly maintenance window
	// (disk measurement runs then); nil: 03:00-05:00 local time.
	InWindow func(time.Time) bool
	Log      *slog.Logger
	Now      func() time.Time

	// mu serialises account status changes and quota checks with what
	// they guard (a site creation, a plan change): two concurrent creates
	// can't both take an account's last site.
	mu        sync.Mutex
	stripeMu  sync.Mutex // one Stripe event at a time (idempotency)
	measureMu sync.Mutex
	lastDisk  time.Time
	burst     burstState
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) usage() UsageSource {
	if s.Usage != nil {
		return s.Usage
	}
	return StoreUsage{s.Store}
}

func (s *Service) emit(ctx context.Context, event string, data map[string]any) {
	if s.Hooks != nil {
		s.Hooks.Emit(ctx, event, data)
	}
}

func (s *Service) event(ctx context.Context, accountID int64, kind, msg string) {
	if err := s.Store.AddAccountEvent(context.WithoutCancel(ctx), accountID, kind, msg); err != nil {
		s.Log.Warn("recording account event", "account", accountID, "err", err)
	}
}

// ---- Plans ----

var planIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// NormalizePlan validates a plan and puts its lists in canonical order.
func NormalizePlan(p *store.Plan) error {
	if !planIDRe.MatchString(p.ID) {
		return fmt.Errorf("%w: plan id must be 1-32 lowercase letters, digits or -", ErrInvalid)
	}
	if err := validName(p.Name); err != nil {
		return err
	}
	if p.MaxSites < 0 || p.DiskMB < 0 || p.BandwidthGB < 0 || p.MaxReplicas < 0 || p.MaxMemoryMB < 0 ||
		p.MaxCPUs < 0 || p.MaxDomains < 0 || p.BurstMinutes < 0 {
		return fmt.Errorf("%w: limits can't be negative (0 means unlimited)", ErrInvalid)
	}
	if p.MaxMemoryMB > 0 && p.MaxMemoryMB < 256 {
		return fmt.Errorf("%w: max_memory_mb must be at least 256 (a replica's minimum)", ErrInvalid)
	}
	if p.MaxCPUs > 0 && p.MaxCPUs < 0.25 {
		return fmt.Errorf("%w: max_cpus must be at least 0.25", ErrInvalid)
	}
	if p.Overage == "" {
		p.Overage = OverageNotify
	}
	if p.Overage != OverageNotify && p.Overage != OverageSuspend {
		return fmt.Errorf("%w: overage must be notify or suspend", ErrInvalid)
	}
	feats := []string{}
	for _, f := range p.Features {
		if !slices.Contains(Features, f) {
			return fmt.Errorf("%w: unknown feature %q (known: %s)", ErrInvalid, f, strings.Join(Features, ", "))
		}
		if !slices.Contains(feats, f) {
			feats = append(feats, f)
		}
	}
	slices.Sort(feats)
	p.Features = feats
	repos := []string{}
	for _, r := range p.BackupRepos {
		if r == "" || strings.ContainsAny(r, ", \t\n") {
			return fmt.Errorf("%w: invalid backup destination %q", ErrInvalid, r)
		}
		if !slices.Contains(repos, r) {
			repos = append(repos, r)
		}
	}
	slices.Sort(repos)
	p.BackupRepos = repos
	return nil
}

// fitsLimit: a child limit fits a parent's when the parent is unlimited
// (0) or the child is limited and no larger.
func fitsLimit[T int | int64 | float64](child, parent T) bool {
	return parent == 0 || (child != 0 && child <= parent)
}

// Fits checks that a plan a reseller assigns stays within the reseller's
// own plan, limit by limit: a reseller can never hand out more than they
// have.
func Fits(child, parent *store.Plan) error {
	var over []string
	check := func(ok bool, name string) {
		if !ok {
			over = append(over, name)
		}
	}
	check(fitsLimit(child.MaxSites, parent.MaxSites), "max_sites")
	check(fitsLimit(child.DiskMB, parent.DiskMB), "disk_mb")
	check(fitsLimit(child.BandwidthGB, parent.BandwidthGB), "bandwidth_gb")
	check(fitsLimit(child.MaxReplicas, parent.MaxReplicas), "max_replicas")
	check(fitsLimit(child.MaxMemoryMB, parent.MaxMemoryMB), "max_memory_mb")
	check(fitsLimit(child.MaxCPUs, parent.MaxCPUs), "max_cpus")
	check(fitsLimit(child.MaxDomains, parent.MaxDomains), "max_domains")
	check(fitsLimit(child.BurstMinutes, parent.BurstMinutes), "burst_minutes")
	for _, f := range child.Features {
		check(slices.Contains(parent.Features, f), "feature "+f)
	}
	for _, r := range child.BackupRepos {
		check(slices.Contains(parent.BackupRepos, r), "backup destination "+r)
	}
	if len(over) > 0 {
		return fmt.Errorf("%w: plan %s exceeds the reseller's plan (%s)", ErrForbidden, child.ID, strings.Join(over, ", "))
	}
	return nil
}

// ResellerPlans are the plans a reseller may assign: resellable ones that
// fit their own plan.
func (s *Service) ResellerPlans(ctx context.Context, reseller *store.Account) ([]*store.Plan, error) {
	own, err := s.Store.GetPlan(ctx, reseller.PlanID)
	if err != nil {
		return nil, err
	}
	all, err := s.Store.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	out := []*store.Plan{}
	for _, p := range all {
		if p.Resellable && Fits(p, own) == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// Limits are what applies to one account's sites: its plan's, narrowed by
// its reseller's plan (a customer never gets more than their reseller has,
// even if an administrator later lowers the reseller's plan).
type Limits struct {
	MaxSites    int     `json:"max_sites"`
	DiskMB      int64   `json:"disk_mb"`
	BandwidthGB int64   `json:"bandwidth_gb"`
	MaxReplicas int     `json:"max_replicas"`
	MaxMemoryMB int     `json:"max_memory_mb"`
	MaxCPUs     float64 `json:"max_cpus"`
	MaxDomains  int     `json:"max_domains"`
	// BurstMinutes are the account's own monthly minutes (a reseller's
	// plan doesn't narrow them: each account has its own).
	BurstMinutes int64    `json:"burst_minutes"`
	Features     []string `json:"features"`
	BackupRepos  []string `json:"backup_repos"`
}

func limitsOf(p *store.Plan) Limits {
	return Limits{MaxSites: p.MaxSites, DiskMB: p.DiskMB, BandwidthGB: p.BandwidthGB, MaxReplicas: p.MaxReplicas,
		MaxMemoryMB: p.MaxMemoryMB, MaxCPUs: p.MaxCPUs, MaxDomains: p.MaxDomains, BurstMinutes: p.BurstMinutes,
		Features: slices.Clone(p.Features), BackupRepos: slices.Clone(p.BackupRepos)}
}

// minLimit is the tighter of two limits where 0 means unlimited.
func minLimit[T int | int64 | float64](a, b T) T {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

func (l Limits) narrow(o Limits) Limits {
	l.MaxReplicas = minLimit(l.MaxReplicas, o.MaxReplicas)
	l.MaxMemoryMB = minLimit(l.MaxMemoryMB, o.MaxMemoryMB)
	l.MaxCPUs = minLimit(l.MaxCPUs, o.MaxCPUs)
	l.MaxDomains = minLimit(l.MaxDomains, o.MaxDomains)
	l.Features = slices.DeleteFunc(l.Features, func(f string) bool { return !slices.Contains(o.Features, f) })
	l.BackupRepos = slices.DeleteFunc(l.BackupRepos, func(r string) bool { return !slices.Contains(o.BackupRepos, r) })
	return l
}

// Has reports whether the limits include a feature.
func (l Limits) Has(feature string) bool { return slices.Contains(l.Features, feature) }

// LimitsFor returns the limits of an account's sites.
func (s *Service) LimitsFor(ctx context.Context, a *store.Account) (Limits, error) {
	p, err := s.Store.GetPlan(ctx, a.PlanID)
	if err != nil {
		return Limits{}, err
	}
	l := limitsOf(p)
	if a.ParentID != 0 {
		parent, err := s.Store.GetAccount(ctx, a.ParentID)
		if err != nil {
			return Limits{}, err
		}
		pp, err := s.Store.GetPlan(ctx, parent.PlanID)
		if err != nil {
			return Limits{}, err
		}
		l = l.narrow(limitsOf(pp))
	}
	return l, nil
}

// ---- Accounts ----

func validName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > 100 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: name must be 1-100 characters on one line", ErrInvalid)
	}
	return nil
}

var (
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	externalRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{0,128}$`)
)

// AccountInput is what creating or editing an account sets.
type AccountInput struct {
	Name                 string `json:"name"`
	Kind                 string `json:"kind"`
	PlanID               string `json:"plan_id"`
	ParentID             int64  `json:"parent_id"`
	Email                string `json:"email"`
	WHMCSServiceID       string `json:"whmcs_service_id"`
	StripeCustomerID     string `json:"stripe_customer_id"`
	StripeSubscriptionID string `json:"stripe_subscription_id"`
}

// validate checks an account's fields and its place in the hierarchy:
// resellers are top-level, customers belong to nobody or to a reseller.
// byReseller: the plan must be one the parent may hand out.
func (s *Service) validate(ctx context.Context, a *store.Account, byReseller bool) error {
	if err := validName(a.Name); err != nil {
		return err
	}
	if a.Kind != store.AccountCustomer && a.Kind != store.AccountReseller {
		return fmt.Errorf("%w: kind must be customer or reseller", ErrInvalid)
	}
	if a.Email != "" && (!emailRe.MatchString(a.Email) || len(a.Email) > 200) {
		return fmt.Errorf("%w: invalid email", ErrInvalid)
	}
	for _, v := range []string{a.WHMCSServiceID, a.StripeCustomerID, a.StripeSubscriptionID} {
		if !externalRe.MatchString(v) {
			return fmt.Errorf("%w: billing IDs are up to 128 letters, digits and . _ : -", ErrInvalid)
		}
	}
	plan, err := s.Store.GetPlan(ctx, a.PlanID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: no plan %q", ErrInvalid, a.PlanID)
	}
	if err != nil {
		return err
	}
	if a.ParentID == 0 {
		return nil
	}
	if a.Kind == store.AccountReseller {
		return fmt.Errorf("%w: a reseller account can't belong to another account", ErrInvalid)
	}
	if a.ParentID == a.ID {
		return fmt.Errorf("%w: an account can't be its own parent", ErrInvalid)
	}
	parent, err := s.Store.GetAccount(ctx, a.ParentID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: no account %d", ErrInvalid, a.ParentID)
	}
	if err != nil {
		return err
	}
	if parent.Kind != store.AccountReseller {
		return fmt.Errorf("%w: parent account %d is not a reseller", ErrInvalid, a.ParentID)
	}
	if byReseller {
		if !plan.Resellable {
			return fmt.Errorf("%w: plan %s is not offered to resellers", ErrForbidden, plan.ID)
		}
		pp, err := s.Store.GetPlan(ctx, parent.PlanID)
		if err != nil {
			return err
		}
		return Fits(plan, pp)
	}
	return nil
}

// CreateAccount creates an account and, if first is given, its first user
// (their role follows the account's kind). With an idempotency key, a key
// already used returns that account and existed = true. byReseller means a
// reseller is creating one of their customers.
func (s *Service) CreateAccount(ctx context.Context, in AccountInput, idemKey string, first *store.NewUser, byReseller bool) (*store.Account, *store.User, bool, error) {
	a := &store.Account{Name: strings.TrimSpace(in.Name), Kind: in.Kind, PlanID: in.PlanID, ParentID: in.ParentID,
		Email: strings.TrimSpace(in.Email), WHMCSServiceID: in.WHMCSServiceID, StripeCustomerID: in.StripeCustomerID,
		StripeSubscriptionID: in.StripeSubscriptionID}
	if a.Kind == "" {
		a.Kind = store.AccountCustomer
	}
	if len(idemKey) > 200 {
		return nil, nil, false, fmt.Errorf("%w: idempotency key too long", ErrInvalid)
	}
	if err := s.validate(ctx, a, byReseller); err != nil {
		return nil, nil, false, err
	}
	if byReseller {
		// Accounts cost nothing until they have sites, but a reseller's
		// automation gone wrong shouldn't fill the panel with them.
		kids, err := s.Store.ListAccounts(ctx, store.AccountFilter{ParentID: a.ParentID})
		if err != nil {
			return nil, nil, false, err
		}
		if len(kids) >= MaxCustomersPerReseller {
			return nil, nil, false, fmt.Errorf("%w: %d customer accounts at most", ErrQuota, MaxCustomersPerReseller)
		}
	}
	acct, user, existed, err := s.Store.CreateAccount(ctx, a, idemKey, first)
	if errors.Is(err, store.ErrDuplicateWHMCS) {
		return nil, nil, false, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if errors.Is(err, store.ErrExists) {
		return nil, nil, false, fmt.Errorf("%w: user %s already exists", ErrConflict, first.Username)
	}
	if err != nil || existed {
		return acct, user, existed, err
	}
	s.event(ctx, acct.ID, "account", fmt.Sprintf("Account created (%s, plan %s)", acct.Kind, acct.PlanID))
	s.emit(ctx, EventAccountCreated, map[string]any{"account": acct})
	return acct, user, false, nil
}

// UpdateAccount stores new values for an account; a plan change is
// validated like a creation and announced (plan.changed).
func (s *Service) UpdateAccount(ctx context.Context, id int64, in AccountInput, byReseller bool) (*store.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	old := *a
	a.Name, a.Kind, a.PlanID, a.ParentID, a.Email = strings.TrimSpace(in.Name), in.Kind, in.PlanID, in.ParentID, strings.TrimSpace(in.Email)
	a.WHMCSServiceID, a.StripeCustomerID, a.StripeSubscriptionID = in.WHMCSServiceID, in.StripeCustomerID, in.StripeSubscriptionID
	// A reseller re-checks the plan only when changing it: an administrator
	// may have given the customer something the reseller can't.
	if err := s.validate(ctx, a, byReseller && a.PlanID != old.PlanID); err != nil {
		return nil, err
	}
	if old.Kind == store.AccountReseller && a.Kind != store.AccountReseller {
		kids, err := s.Store.ListAccounts(ctx, store.AccountFilter{ParentID: id})
		if err != nil {
			return nil, err
		}
		if len(kids) > 0 {
			return nil, fmt.Errorf("%w: the reseller still has %d customer account(s)", ErrConflict, len(kids))
		}
	}
	if err := s.Store.UpdateAccount(ctx, a); errors.Is(err, store.ErrDuplicateWHMCS) {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	} else if err != nil {
		return nil, err
	}
	if a.Kind != old.Kind {
		if err := s.Store.SetAccountUsersRole(ctx, id, auth.TenantRole(a.Kind)); err != nil {
			return nil, err
		}
		s.event(ctx, id, "account", "Now a "+a.Kind+" account")
	}
	if a.PlanID != old.PlanID {
		s.event(ctx, id, "plan", fmt.Sprintf("Plan changed from %s to %s", old.PlanID, a.PlanID))
		s.emit(ctx, EventPlanChanged, map[string]any{"account_id": id, "from": old.PlanID, "to": a.PlanID})
	}
	if a.ParentID != old.ParentID {
		// A new reseller may be suspended (or the old one was).
		s.applyStateLocked(ctx, id)
	}
	return s.Store.GetAccount(ctx, id)
}

// Suspend suspends an account and every site it owns (a reseller's
// customers' sites too). Suspending an already suspended account keeps the
// stronger reason.
func (s *Service) Suspend(ctx context.Context, id int64, reason string) (*store.Account, error) {
	if reasonRank(reason) == 0 {
		return nil, fmt.Errorf("%w: reason must be admin, billing, overage or reseller", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	switch a.Status {
	case store.AccountTerminated:
		return nil, fmt.Errorf("%w: the account is terminated", ErrConflict)
	case store.AccountSuspended:
		if reasonRank(reason) <= reasonRank(a.SuspendReason) {
			return a, s.applyStateLocked(ctx, id)
		}
	}
	if err := s.Store.SetAccountStatus(ctx, id, store.AccountSuspended, reason, s.now()); err != nil {
		return nil, err
	}
	s.event(ctx, id, "suspend", "Suspended ("+reason+")")
	s.emit(ctx, EventAccountSuspended, map[string]any{"account_id": id, "reason": reason})
	s.Log.Info("account suspended", "account", id, "reason", reason)
	err = s.applyStateLocked(ctx, id)
	a, gerr := s.Store.GetAccount(ctx, id)
	return a, errors.Join(err, gerr)
}

// Unsuspend lifts a suspension made for reason by (or anyone, by an
// administrator: ReasonAdmin), and brings the account's sites back.
// Terminated accounts come back only by an administrator.
func (s *Service) Unsuspend(ctx context.Context, id int64, by string) (*store.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case a.Status == store.AccountActive:
		return a, s.applyStateLocked(ctx, id)
	case a.Status == store.AccountTerminated && by != ReasonAdmin:
		return nil, fmt.Errorf("%w: only an administrator can reactivate a terminated account", ErrForbidden)
	case by != ReasonAdmin && by != a.SuspendReason:
		return nil, fmt.Errorf("%w: suspended by %s; only they or an administrator can lift it", ErrForbidden, a.SuspendReason)
	}
	if err := s.Store.SetAccountStatus(ctx, id, store.AccountActive, "", s.now()); err != nil {
		return nil, err
	}
	s.event(ctx, id, "unsuspend", "Unsuspended ("+by+")")
	s.emit(ctx, EventAccountUnsuspended, map[string]any{"account_id": id, "by": by})
	s.Log.Info("account unsuspended", "account", id, "by", by)
	err = s.applyStateLocked(ctx, id)
	a, gerr := s.Store.GetAccount(ctx, id)
	return a, errors.Join(err, gerr)
}

// TerminateResult reports what terminating an account did.
type TerminateResult struct {
	Account *store.Account    `json:"account"`
	Deleted []string          `json:"deleted_sites"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// Terminate ends an account: its users can no longer sign in, its sites
// are suspended and, with deleteSites, deleted (staging copies first).
// Running it again retries deletions that failed.
func (s *Service) Terminate(ctx context.Context, id int64, deleteSites bool) (*TerminateResult, error) {
	s.mu.Lock()
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if a.Kind == store.AccountReseller {
		kids, err := s.Store.ListAccounts(ctx, store.AccountFilter{ParentID: id})
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		for _, k := range kids {
			if k.Status != store.AccountTerminated {
				s.mu.Unlock()
				return nil, fmt.Errorf("%w: terminate the reseller's customer accounts first (%d left)", ErrConflict, len(kids))
			}
		}
	}
	if a.Status != store.AccountTerminated {
		if err := s.Store.SetAccountStatus(ctx, id, store.AccountTerminated, "terminated", s.now()); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.event(ctx, id, "terminate", "Terminated")
		s.emit(ctx, EventAccountTerminated, map[string]any{"account_id": id, "delete_sites": deleteSites})
		// Signed-in users go now, not at their next request.
		if users, err := s.Store.AccountUsers(ctx, id); err == nil {
			for _, u := range users {
				s.Store.DeleteUserSessions(ctx, u.ID, "")
			}
		}
	}
	applyErr := s.applyStateLocked(ctx, id)
	s.mu.Unlock()
	res := &TerminateResult{Deleted: []string{}, Errors: map[string]string{}}
	if deleteSites {
		owned, err := s.Store.SiteOwners(ctx, id)
		if err != nil {
			return nil, err
		}
		// A live site can't go while its staging copy exists: two passes
		// delete staging copies in the first and live sites in the second.
		pending := owned
		for pass := 0; pass < 2 && len(pending) > 0; pass++ {
			var again []store.SiteOwner
			for _, o := range pending {
				if err := s.Sites.Delete(ctx, o.SiteID); err != nil && !errors.Is(err, store.ErrNotFound) {
					res.Errors[o.SiteID] = err.Error()
					again = append(again, o)
					continue
				}
				delete(res.Errors, o.SiteID)
				// A site that lived elsewhere leaves its ownership here.
				s.Store.UnassignSite(ctx, o.SiteID)
				res.Deleted = append(res.Deleted, o.SiteID)
				s.emit(ctx, EventSiteDeleted, map[string]any{"site_id": o.SiteID, "account_id": id})
			}
			pending = again
		}
		if len(res.Deleted) > 0 {
			s.event(ctx, id, "terminate", fmt.Sprintf("Deleted %d site(s): %s", len(res.Deleted), strings.Join(res.Deleted, ", ")))
		}
	}
	res.Account, err = s.Store.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		msgs := make([]string, 0, len(res.Errors))
		for site, e := range res.Errors {
			msgs = append(msgs, site+": "+e)
		}
		slices.Sort(msgs)
		return res, fmt.Errorf("terminated, but deleting sites failed (run it again to retry): %s", strings.Join(msgs, "; "))
	}
	return res, applyErr
}

// DeleteAccount removes a terminated account that owns nothing any more.
func (s *Service) DeleteAccount(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	if a.Status != store.AccountTerminated {
		return fmt.Errorf("%w: terminate the account first", ErrConflict)
	}
	if err := s.Store.DeleteAccount(ctx, id); errors.Is(err, store.ErrInUse) {
		return fmt.Errorf("%w: the account still owns sites or customer accounts", ErrConflict)
	} else if err != nil {
		return err
	}
	s.Log.Info("account deleted", "account", id)
	return nil
}

// Suspended reports whether an account is effectively suspended: its own
// status, or its reseller's.
func (s *Service) Suspended(ctx context.Context, a *store.Account) (bool, error) {
	if a.Status != store.AccountActive {
		return true, nil
	}
	if a.ParentID == 0 {
		return false, nil
	}
	p, err := s.Store.GetAccount(ctx, a.ParentID)
	if err != nil {
		return true, err // fail closed
	}
	return p.Status != store.AccountActive, nil
}

// ApplyState makes an account's sites (and its customers' sites, for a
// reseller) match whether it is suspended.
func (s *Service) ApplyState(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyStateLocked(ctx, id)
}

func (s *Service) applyStateLocked(ctx context.Context, id int64) error {
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	accounts := []*store.Account{a}
	if a.Kind == store.AccountReseller {
		kids, err := s.Store.ListAccounts(ctx, store.AccountFilter{ParentID: id})
		if err != nil {
			return err
		}
		accounts = append(accounts, kids...)
	}
	var errs []error
	for _, acct := range accounts {
		suspended, err := s.Suspended(ctx, acct)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		owned, err := s.Store.SiteOwners(ctx, acct.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, o := range owned {
			errs = append(errs, s.applySite(ctx, o, suspended))
		}
	}
	return errors.Join(errs...)
}

// applySite suspends or brings back one site. The flag in site_accounts is
// what billing did: a site suspended by billing comes back only through
// billing.
func (s *Service) applySite(ctx context.Context, o store.SiteOwner, suspended bool) error {
	switch {
	case suspended && !o.Suspended:
		if err := s.Sites.Suspend(ctx, o.SiteID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			// A site still being created is caught by the next reconcile.
			s.Log.Warn("suspending a site", "site", o.SiteID, "account", o.AccountID, "err", err)
			return fmt.Errorf("suspending site %s: %w", o.SiteID, err)
		}
		return s.Store.SetSiteSuspended(ctx, o.SiteID, true)
	case !suspended && o.Suspended:
		if err := s.Sites.Unsuspend(ctx, o.SiteID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.Log.Warn("unsuspending a site", "site", o.SiteID, "account", o.AccountID, "err", err)
			return fmt.Errorf("unsuspending site %s: %w", o.SiteID, err)
		}
		return s.Store.SetSiteSuspended(ctx, o.SiteID, false)
	}
	return nil
}

// ReconcileAll applies every account's state to its sites: catches sites
// created while an account was suspended, and retries failures.
func (s *Service) ReconcileAll(ctx context.Context) error {
	accounts, err := s.Store.ListAccounts(ctx, store.AccountFilter{})
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accounts {
		if a.ParentID != 0 {
			continue // reconciled with its reseller
		}
		errs = append(errs, s.ApplyState(ctx, a.ID))
	}
	return errors.Join(errs...)
}

// ---- Sites and quotas ----

// LockQuota serialises a quota check with the change it allows; call the
// returned function when the change is recorded (the site is assigned).
func (s *Service) LockQuota() func() {
	s.mu.Lock()
	return s.mu.Unlock
}

// scopeIDs are the accounts whose sites count towards an account's totals:
// itself and, for a reseller, its customers.
func (s *Service) scopeIDs(ctx context.Context, a *store.Account) ([]int64, error) {
	ids := []int64{a.ID}
	if a.Kind == store.AccountReseller {
		kids, err := s.Store.ListAccounts(ctx, store.AccountFilter{ParentID: a.ID})
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			ids = append(ids, k.ID)
		}
	}
	return ids, nil
}

// CheckNewSiteLocked verifies an account may have one more site: it is
// active, under its plan's site count and disk space, and so is its
// reseller (whose totals include every customer). Caller holds LockQuota.
func (s *Service) CheckNewSiteLocked(ctx context.Context, accountID int64) error {
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if suspended, err := s.Suspended(ctx, a); err != nil {
		return err
	} else if suspended {
		return fmt.Errorf("%w: account %s is %s", ErrForbidden, a.Name, a.Status)
	}
	chain := []*store.Account{a}
	if a.ParentID != 0 {
		p, err := s.Store.GetAccount(ctx, a.ParentID)
		if err != nil {
			return err
		}
		chain = append(chain, p)
	}
	for _, acct := range chain {
		plan, err := s.Store.GetPlan(ctx, acct.PlanID)
		if err != nil {
			return err
		}
		ids, err := s.scopeIDs(ctx, acct)
		if err != nil {
			return err
		}
		owned, err := s.Store.SiteOwners(ctx, ids...)
		if err != nil {
			return err
		}
		whose := "the plan"
		if acct.ID != a.ID {
			whose = "the reseller's plan"
		}
		if plan.MaxSites > 0 && len(owned) >= plan.MaxSites {
			return fmt.Errorf("%w: %s allows %d site(s), staging copies included", ErrQuota, whose, plan.MaxSites)
		}
		if plan.DiskMB > 0 {
			disk, err := s.diskBytes(ctx, owned)
			if err != nil {
				return err
			}
			if disk >= plan.DiskMB<<20 {
				return fmt.Errorf("%w: disk space is used up (%d of %d MB in %s)", ErrQuota, disk>>20, plan.DiskMB, whose)
			}
		}
	}
	return nil
}

func (s *Service) diskBytes(ctx context.Context, owned []store.SiteOwner) (int64, error) {
	ids := make([]string, len(owned))
	for i, o := range owned {
		ids[i] = o.SiteID
	}
	disk, err := s.usage().Disk(ctx, ids)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, u := range disk {
		n += u.FilesBytes + u.DBBytes
	}
	return n, nil
}

// CheckResources verifies a site's size against its account's limits.
// replicas is the most the site may run (its autoscaling maximum when
// autoscaling).
func CheckResources(l Limits, replicas, memoryMB int, cpus float64) error {
	switch {
	case l.MaxReplicas > 0 && replicas > l.MaxReplicas:
		return fmt.Errorf("%w: your plan allows up to %d replica(s) per site", ErrQuota, l.MaxReplicas)
	case l.MaxMemoryMB > 0 && memoryMB > l.MaxMemoryMB:
		return fmt.Errorf("%w: your plan allows up to %d MB of memory per replica", ErrQuota, l.MaxMemoryMB)
	case l.MaxCPUs > 0 && cpus > l.MaxCPUs:
		return fmt.Errorf("%w: your plan allows up to %g CPU(s) per replica", ErrQuota, l.MaxCPUs)
	}
	return nil
}

// AssignSite makes an account own a site and applies the account's state
// to it: a site given to a suspended account is suspended, one billing
// suspended and moved to an active account comes back.
func (s *Service) AssignSite(ctx context.Context, siteID string, accountID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AssignSiteLocked(ctx, siteID, accountID)
}

// AssignSiteLocked is AssignSite for a caller holding LockQuota.
func (s *Service) AssignSiteLocked(ctx context.Context, siteID string, accountID int64) error {
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := s.Store.AssignSite(ctx, siteID, accountID); err != nil {
		return err
	}
	s.event(ctx, accountID, "site", "Site "+siteID+" assigned")
	suspended, err := s.Suspended(ctx, a)
	if err != nil {
		return err
	}
	o, err := s.Store.SiteOwnerOf(ctx, siteID)
	if err != nil {
		return err
	}
	if err := s.applySite(ctx, *o, suspended); err != nil {
		// A site still being created can't be suspended yet: the hourly
		// reconcile does it once it is.
		s.Log.Warn("applying the account's state to an assigned site", "site", siteID, "err", err)
	}
	return nil
}

// UnassignSite makes a site staff-only again (its suspension by billing,
// if any, is lifted).
func (s *Service) UnassignSite(ctx context.Context, siteID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.Store.SiteOwnerOf(ctx, siteID)
	if err != nil {
		return err
	}
	if err := s.applySite(ctx, *o, false); err != nil {
		return err
	}
	if err := s.Store.UnassignSite(ctx, siteID); err != nil {
		return err
	}
	s.event(ctx, o.AccountID, "site", "Site "+siteID+" unassigned")
	return nil
}
