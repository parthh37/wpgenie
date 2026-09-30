package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Burst minutes. A site is charged one minute for every minute it runs
// above its normal size (site.Bursting), counted where it runs. Every
// minute the panel charges each site's new minutes to the account that
// owns it now (burst_charges: never taken back, so deleting or giving
// away a site doesn't return its minutes), takes what goes beyond the
// plan's monthly minutes out of the account's credit, and pauses the
// burst of an account's sites while it has nothing left, or while its
// plan doesn't include burst. Staff sites aren't limited; their minutes
// are charged to account 0 so a site given to an account later only
// brings its later minutes along. For the new month's first hour the
// previous month is charged too: its last minutes, and those of a server
// that couldn't be reached at the turn of the month.

// BurstOps reaches sites' burst wherever they run (site.ClusterOps).
type BurstOps interface {
	// BurstMinutes returns every site's burst minutes of a month.
	BurstMinutes(ctx context.Context, month time.Time) (map[string]int64, error)
	// SetBurstPaused pauses or resumes a site's burst.
	SetBurstPaused(ctx context.Context, siteID string, paused bool) error
	// BurstPaused lists the sites whose burst is paused now.
	BurstPaused(ctx context.Context) (map[string]bool, error)
}

const (
	// burstResync: how often a site's pause is sent again although this
	// panel believes the site has it (a node that restored a backup, a
	// site moved meanwhile).
	burstResync = time.Hour
	// maxBurstCredit bounds one account's credit (and one top-up).
	maxBurstCredit = 100_000_000
	// Notification levels of burst_notified: 80 and 100 percent of the
	// month's minutes, and burstExhausted: nothing left (credit included).
	burstExhausted = 200
)

type burstState struct {
	mu     sync.Mutex
	paused map[string]burstSent // site ID -> what was last sent, and when
	kick   chan struct{}
	once   sync.Once
}

type burstSent struct {
	paused bool
	at     time.Time
}

func (s *Service) kickChan() chan struct{} {
	s.burst.once.Do(func() { s.burst.kick = make(chan struct{}, 1) })
	return s.burst.kick
}

// BurstBalance is an account's burst minutes this month.
type BurstBalance struct {
	MonthStart time.Time `json:"month_start"`
	// Allowed: the plan includes burst (tenants can switch it on).
	Allowed bool `json:"allowed"`
	// Included are the month's minutes (Unlimited: no limit), Used those
	// used so far (credit included), Credit the bought minutes left, and
	// Remaining what is left in total.
	Included  int64 `json:"included"`
	Unlimited bool  `json:"unlimited"`
	Used      int64 `json:"used"`
	Credit    int64 `json:"credit"`
	Remaining int64 `json:"remaining"`
	// PerSite are the minutes each of the account's sites was charged.
	PerSite map[string]int64 `json:"per_site"`
}

// BurstBalanceOf returns an account's burst minutes this month.
func (s *Service) BurstBalanceOf(ctx context.Context, a *store.Account) (*BurstBalance, error) {
	plan, err := s.Store.GetPlan(ctx, a.PlanID)
	if err != nil {
		return nil, err
	}
	limits, err := s.LimitsFor(ctx, a)
	if err != nil {
		return nil, err
	}
	month := MonthStart(s.now())
	charges, err := s.Store.AccountBurstCharges(ctx, month)
	if err != nil {
		return nil, err
	}
	fresh, err := s.Store.GetAccount(ctx, a.ID) // the credit, as charged just now
	if err != nil {
		return nil, err
	}
	b := &BurstBalance{MonthStart: month, Allowed: limits.Has(FeatureBurst), Included: plan.BurstMinutes,
		Unlimited: plan.BurstMinutes == 0, Credit: fresh.BurstCredit, PerSite: map[string]int64{}}
	for site, n := range charges[a.ID] {
		b.Used += n
		b.PerSite[site] = n
	}
	b.Remaining = burstRemaining(plan.BurstMinutes, b.Used, b.Credit)
	return b, nil
}

// burstRemaining is what an account has left: the month's minutes not
// used yet, and its credit (from which the minutes beyond the month's
// were already taken).
func burstRemaining(included, used, credit int64) int64 {
	return max(0, included-used) + credit
}

// AddBurstCredit adds bought minutes to an account (negative: takes them
// away, never below zero) and returns it. Paused sites resume within
// seconds.
func (s *Service) AddBurstCredit(ctx context.Context, id, minutes int64) (*store.Account, error) {
	if minutes == 0 || minutes > maxBurstCredit || minutes < -maxBurstCredit {
		return nil, fmt.Errorf("%w: minutes must be a non-zero number up to %d", ErrInvalid, maxBurstCredit)
	}
	a, err := s.Store.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.BurstCredit+minutes > maxBurstCredit {
		return nil, fmt.Errorf("%w: an account holds at most %d burst minutes", ErrInvalid, maxBurstCredit)
	}
	credit, err := s.Store.AddBurstCredit(ctx, id, minutes)
	if err != nil {
		return nil, err
	}
	verb := "added"
	if minutes < 0 {
		verb, minutes = "removed", -minutes
	}
	s.event(ctx, id, "burst", fmt.Sprintf("%d burst minutes %s (%d in credit)", minutes, verb, credit))
	s.KickBurst()
	return s.Store.GetAccount(ctx, id)
}

// KickBurst meters burst now rather than at the next minute (credit
// added, a plan changed).
func (s *Service) KickBurst() {
	select {
	case s.kickChan() <- struct{}{}:
	default:
	}
}

// RunBurst meters burst minutes every minute until ctx ends.
func (s *Service) RunBurst(ctx context.Context) {
	if s.Burst == nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	kick := s.kickChan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
		if err := s.MeterBurst(ctx); err != nil {
			s.Log.Warn("burst: metering", "err", err)
		}
	}
}

// MeterBurst charges sites' new burst minutes to their accounts and
// pauses or resumes their burst by what the accounts have left.
func (s *Service) MeterBurst(ctx context.Context) error {
	if s.Burst == nil {
		return nil
	}
	now := s.now()
	month := MonthStart(now)
	owners, err := s.Store.AllSiteOwners(ctx)
	if err != nil {
		return err
	}
	var errs []error
	if now.Sub(month) < time.Hour {
		errs = append(errs, s.chargeMonth(ctx, month.AddDate(0, -1, 0), owners, nil))
	}
	pause := map[string]bool{}
	for _, o := range owners {
		pause[o.SiteID] = false
	}
	if err := s.chargeMonth(ctx, month, owners, pause); err != nil {
		return errors.Join(append(errs, err)...)
	}
	// Staff sites are never paused: those paused under an account they
	// left (as stored where they run, or as sent from here).
	paused, err := s.Burst.BurstPaused(ctx)
	errs = append(errs, err)
	s.burst.mu.Lock()
	for site, last := range s.burst.paused {
		if last.paused {
			paused[site] = true
		}
	}
	s.burst.mu.Unlock()
	for site := range paused {
		if _, owned := pause[site]; !owned {
			pause[site] = false
		}
	}
	for site, p := range pause {
		errs = append(errs, s.applyBurstPause(ctx, site, p, now))
	}
	return errors.Join(errs...)
}

// chargeMonth charges a month's new minutes and settles every account's
// month. With pause, it also records which sites must pause (their
// account has nothing left); without, it notifies nothing (a month that
// is over).
func (s *Service) chargeMonth(ctx context.Context, month time.Time, owners []store.SiteOwner, pause map[string]bool) error {
	minutes, err := s.Burst.BurstMinutes(ctx, month)
	if err != nil {
		return err
	}
	charged, err := s.Store.BurstCharged(ctx, month)
	if err != nil {
		return err
	}
	ownerOf := make(map[string]int64, len(owners))
	for _, o := range owners {
		ownerOf[o.SiteID] = o.AccountID
	}
	var errs []error
	for site, n := range minutes {
		if d := n - charged[site]; d > 0 {
			errs = append(errs, s.Store.AddBurstCharge(ctx, ownerOf[site], site, month, d))
		}
	}
	byAccount, err := s.Store.AccountBurstCharges(ctx, month)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	accounts, err := s.Store.ListAccounts(ctx, store.AccountFilter{})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, a := range accounts {
		if a.Status == store.AccountTerminated {
			continue
		}
		out, err := s.chargeBurst(ctx, a, month, byAccount[a.ID], pause == nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("account %d: %w", a.ID, err))
			continue
		}
		if pause == nil {
			continue
		}
		for _, o := range owners {
			if o.AccountID == a.ID {
				pause[o.SiteID] = out
			}
		}
	}
	if pause != nil {
		for site := range minutes {
			if _, owned := pause[site]; !owned {
				pause[site] = false // a staff site
			}
		}
	}
	return errors.Join(errs...)
}

// chargeBurst records an account's month, takes minutes beyond the plan's
// out of its credit, notifies thresholds (unless quiet), and reports
// whether its sites' burst must pause: nothing left, or a plan (or a
// reseller's) without burst.
func (s *Service) chargeBurst(ctx context.Context, a *store.Account, month time.Time, sites map[string]int64, quiet bool) (bool, error) {
	plan, err := s.Store.GetPlan(ctx, a.PlanID)
	if err != nil {
		return false, err
	}
	limits, err := s.LimitsFor(ctx, a)
	if err != nil {
		return false, err
	}
	pause := !limits.Has(FeatureBurst)
	var used int64
	for _, n := range sites {
		used += n
	}
	prev, err := s.Store.AccountBurstFor(ctx, a.ID, month)
	if err != nil {
		return false, err
	}
	included := plan.BurstMinutes
	var fromCredit int64
	if included > 0 {
		fromCredit = max(0, used-included)
	}
	// The credit once the month is settled (as the store will).
	_, _, change := store.SettleBurst(fromCredit, prev.FromCredit, prev.CreditTaken, a.BurstCredit)
	credit := a.BurstCredit + change
	remaining := burstRemaining(included, used, credit)
	exhausted := included > 0 && remaining <= 0
	level := prev.Notified
	switch {
	case exhausted:
		level = burstExhausted
	case level == burstExhausted:
		level = 100 // credit was added: running out again is notified again
	case included > 0 && used >= included:
		level = 100
	case included > 0 && used*100 >= included*80:
		level = max(level, 80)
	}
	if used == prev.Used && fromCredit == prev.FromCredit && level == prev.Notified {
		return exhausted || pause, nil // nothing new: most accounts, most minutes
	}
	if _, err := s.Store.ChargeBurst(ctx, a.ID, month, used, fromCredit, level); err != nil {
		return false, err
	}
	if level > prev.Notified && !quiet {
		s.notifyBurst(ctx, a, level, used, included, credit, month)
	}
	return exhausted || pause, nil
}

func (s *Service) notifyBurst(ctx context.Context, a *store.Account, level int, used, included, credit int64, month time.Time) {
	var msg string
	switch level {
	case burstExhausted:
		msg = fmt.Sprintf("Burst minutes used up (%d this month): sites stay at their normal size until minutes are added", used)
	case 100:
		msg = fmt.Sprintf("This month's %d burst minutes are used up; bought minutes are used now (%d left)", included, credit)
	default:
		msg = fmt.Sprintf("%d%% of this month's burst minutes used (%d of %d)", level, used, included)
	}
	s.event(ctx, a.ID, "burst", msg)
	s.Log.Info("burst threshold", "account", a.ID, "level", level, "used", used)
	percent := level
	if level == burstExhausted {
		percent = 100
	}
	s.emit(ctx, EventBurstThreshold, map[string]any{"account_id": a.ID, "percent": percent, "exhausted": level == burstExhausted,
		"used_minutes": used, "included_minutes": included, "credit_minutes": credit, "month": month.Format("2006-01")})
}

// applyBurstPause sends a site's pause where it runs when it changed, or
// when it was last sent long ago.
func (s *Service) applyBurstPause(ctx context.Context, site string, paused bool, now time.Time) error {
	s.burst.mu.Lock()
	if s.burst.paused == nil {
		s.burst.paused = map[string]burstSent{}
	}
	last, ok := s.burst.paused[site]
	s.burst.mu.Unlock()
	if ok && last.paused == paused && now.Sub(last.at) < burstResync {
		return nil
	}
	if err := s.Burst.SetBurstPaused(ctx, site, paused); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // deleted meanwhile
		}
		return fmt.Errorf("site %s: %w", site, err)
	}
	s.burst.mu.Lock()
	s.burst.paused[site] = burstSent{paused, now}
	s.burst.mu.Unlock()
	return nil
}
