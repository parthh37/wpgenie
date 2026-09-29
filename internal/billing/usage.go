package billing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Usage metering. Bandwidth is what Caddy served (response bytes, the
// traffic rollups) per UTC calendar month; disk is each site's files plus
// its database, measured nightly and on demand. Every hour the month's
// totals are recorded per account, compared with the plan, and crossing
// 80% or 100% is notified once a month (account log, usage.threshold
// webhook). Bandwidth past 100% suspends the account when the plan says
// so; the next month (or a bigger plan) lifts that suspension by itself.

const (
	mb = int64(1) << 20
	gb = int64(1) << 30
)

// MonthStart is the start of t's UTC calendar month.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// SiteUsage is one site's share of an account's usage.
type SiteUsage struct {
	SiteID         string    `json:"site_id"`
	AccountID      int64     `json:"account_id"`
	FilesBytes     int64     `json:"files_bytes"`
	DBBytes        int64     `json:"db_bytes"`
	BandwidthBytes int64     `json:"bandwidth_bytes"`
	MeasuredAt     time.Time `json:"measured_at,omitzero"`
}

// Usage is an account's usage this month against its plan. For a reseller
// the totals include every customer's sites: that is what the reseller's
// plan allocates.
type Usage struct {
	AccountID           int64       `json:"account_id"`
	Name                string      `json:"name"`
	Status              string      `json:"status"`
	PlanID              string      `json:"plan_id"`
	WHMCSServiceID      string      `json:"whmcs_service_id,omitempty"`
	StripeCustomerID    string      `json:"stripe_customer_id,omitempty"`
	MonthStart          time.Time   `json:"month_start"`
	Sites               int         `json:"sites"`
	MaxSites            int         `json:"max_sites"`
	DiskBytes           int64       `json:"disk_bytes"`
	DiskLimitBytes      int64       `json:"disk_limit_bytes"`
	BandwidthBytes      int64       `json:"bandwidth_bytes"`
	BandwidthLimitBytes int64       `json:"bandwidth_limit_bytes"`
	IncludesCustomers   bool        `json:"includes_customers"`
	PerSite             []SiteUsage `json:"per_site"`
}

// percent is used/limit in whole percent; 0 for no limit.
func percent(used, limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(used * 100 / limit)
}

// threshold is the highest notification level reached: 0, 80 or 100.
func threshold(pct int) int {
	switch {
	case pct >= 100:
		return 100
	case pct >= 80:
		return 80
	}
	return 0
}

// UsageOf computes an account's usage now.
func (s *Service) UsageOf(ctx context.Context, a *store.Account) (*Usage, error) {
	plan, err := s.Store.GetPlan(ctx, a.PlanID)
	if err != nil {
		return nil, err
	}
	ids, err := s.scopeIDs(ctx, a)
	if err != nil {
		return nil, err
	}
	owned, err := s.Store.SiteOwners(ctx, ids...)
	if err != nil {
		return nil, err
	}
	siteIDs := make([]string, len(owned))
	for i, o := range owned {
		siteIDs[i] = o.SiteID
	}
	now := s.now()
	month := MonthStart(now)
	bw, err := s.usage().Bandwidth(ctx, siteIDs, month, now.Add(time.Hour))
	if err != nil {
		return nil, err
	}
	disk, err := s.usage().Disk(ctx, siteIDs)
	if err != nil {
		return nil, err
	}
	u := &Usage{AccountID: a.ID, Name: a.Name, Status: a.Status, PlanID: a.PlanID, WHMCSServiceID: a.WHMCSServiceID,
		StripeCustomerID: a.StripeCustomerID, MonthStart: month, Sites: len(owned), MaxSites: plan.MaxSites,
		DiskLimitBytes: plan.DiskMB * mb, BandwidthLimitBytes: plan.BandwidthGB * gb,
		IncludesCustomers: len(ids) > 1, PerSite: []SiteUsage{}}
	for _, o := range owned {
		d := disk[o.SiteID]
		su := SiteUsage{SiteID: o.SiteID, AccountID: o.AccountID, FilesBytes: d.FilesBytes, DBBytes: d.DBBytes,
			BandwidthBytes: bw[o.SiteID], MeasuredAt: d.MeasuredAt}
		u.DiskBytes += su.FilesBytes + su.DBBytes
		u.BandwidthBytes += su.BandwidthBytes
		u.PerSite = append(u.PerSite, su)
	}
	return u, nil
}

// MeasureAccount measures the disk use of an account's sites (a reseller's
// customers' too) now. Sites this server can't measure (elsewhere) keep
// their last report.
func (s *Service) MeasureAccount(ctx context.Context, a *store.Account) error {
	ids, err := s.scopeIDs(ctx, a)
	if err != nil {
		return err
	}
	owned, err := s.Store.SiteOwners(ctx, ids...)
	if err != nil {
		return err
	}
	return s.measure(ctx, owned)
}

func (s *Service) measure(ctx context.Context, owned []store.SiteOwner) error {
	if s.Meter == nil {
		return errors.New("disk measurement is not available")
	}
	// One walk at a time: measuring is disk-heavy on a server that serves.
	s.measureMu.Lock()
	defer s.measureMu.Unlock()
	var errs []error
	for _, o := range owned {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u, err := s.Meter.MeasureDisk(ctx, o.SiteID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("site %s: %w", o.SiteID, err))
			continue
		}
		if err := s.Store.SetSiteUsage(ctx, u); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunUsage meters usage until ctx ends: the month's totals and thresholds
// every hour, disk every night in the maintenance window.
func (s *Service) RunUsage(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	var lastEval time.Time
	// Let the first proxy sync and the analytics ingester catch up.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
	}
	for {
		now := s.now()
		if s.inWindow(now) && now.Sub(s.lastDisk) > 20*time.Hour {
			s.lastDisk = now
			if owned, err := s.Store.AllSiteOwners(ctx); err != nil {
				s.Log.Error("usage: listing sites", "err", err)
			} else if err := s.measure(ctx, owned); err != nil {
				s.Log.Warn("usage: measuring disk", "err", err)
			}
		}
		if now.Sub(lastEval) >= time.Hour {
			lastEval = now
			if err := s.EvaluateUsage(ctx); err != nil {
				s.Log.Warn("usage: evaluating", "err", err)
			}
			if err := s.ReconcileAll(ctx); err != nil {
				s.Log.Warn("accounts: applying suspensions", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) inWindow(now time.Time) bool {
	if s.InWindow != nil {
		return s.InWindow(now)
	}
	h := now.Hour()
	return h >= 3 && h < 5
}

// EvaluateUsage records every account's usage this month, notifies the
// thresholds crossed, applies the plans' overage action, and reports
// bandwidth to Stripe when metering is configured.
func (s *Service) EvaluateUsage(ctx context.Context) error {
	accounts, err := s.Store.ListAccounts(ctx, store.AccountFilter{})
	if err != nil {
		return err
	}
	cfg, err := s.StripeSettings(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accounts {
		if a.Status == store.AccountTerminated {
			continue
		}
		errs = append(errs, s.evaluate(ctx, a, cfg))
	}
	return errors.Join(errs...)
}

func (s *Service) evaluate(ctx context.Context, a *store.Account, cfg *StripeSettings) error {
	u, err := s.UsageOf(ctx, a)
	if err != nil {
		return err
	}
	plan, err := s.Store.GetPlan(ctx, a.PlanID)
	if err != nil {
		return err
	}
	now := s.now()
	row, err := s.Store.RecordAccountUsage(ctx, a.ID, u.MonthStart, u.BandwidthBytes, u.DiskBytes, now)
	if err != nil {
		return err
	}
	bwPct, diskPct := percent(u.BandwidthBytes, u.BandwidthLimitBytes), percent(u.DiskBytes, u.DiskLimitBytes)
	bwLevel, diskLevel := max(row.BWNotified, threshold(bwPct)), max(row.DiskNotified, threshold(diskPct))
	if bwLevel > row.BWNotified {
		s.notify(ctx, a, "bandwidth", bwLevel, u.BandwidthBytes, u.BandwidthLimitBytes, u.MonthStart)
	}
	if diskLevel > row.DiskNotified {
		s.notify(ctx, a, "disk", diskLevel, u.DiskBytes, u.DiskLimitBytes, u.MonthStart)
	}
	if bwLevel != row.BWNotified || diskLevel != row.DiskNotified {
		if err := s.Store.SetUsageNotified(ctx, a.ID, u.MonthStart, bwLevel, diskLevel); err != nil {
			return err
		}
	}
	var errs []error
	// Suspension is decided on the usage, not on the notification: an
	// account unsuspended for another reason mid-month is caught again.
	switch {
	case bwPct >= 100 && plan.Overage == OverageSuspend && a.Status == store.AccountActive:
		s.event(ctx, a.ID, "usage", "Suspended: the month's bandwidth is used up")
		_, err := s.Suspend(ctx, a.ID, ReasonOverage)
		errs = append(errs, err)
	case a.Status == store.AccountSuspended && a.SuspendReason == ReasonOverage &&
		(bwPct < 100 || plan.Overage != OverageSuspend):
		// A new month, a bigger plan, or one that only notifies.
		_, err := s.Unsuspend(ctx, a.ID, ReasonOverage)
		errs = append(errs, err)
	}
	errs = append(errs, s.reportMeter(ctx, a, cfg, row, u.BandwidthBytes))
	return errors.Join(errs...)
}

func (s *Service) notify(ctx context.Context, a *store.Account, metric string, level int, used, limit int64, month time.Time) {
	msg := fmt.Sprintf("%s: %d%% of the plan's allowance used (%d of %d MB)", metric, level, used/mb, limit/mb)
	if metric == "bandwidth" {
		msg = fmt.Sprintf("bandwidth: %d%% of this month's allowance used (%.1f of %d GB)", level,
			float64(used)/float64(gb), limit/gb)
	}
	s.event(ctx, a.ID, "usage", msg)
	s.Log.Info("usage threshold", "account", a.ID, "metric", metric, "percent", level)
	s.emit(ctx, EventUsageThreshold, map[string]any{"account_id": a.ID, "metric": metric, "percent": level,
		"used_bytes": used, "limit_bytes": limit, "month": month.Format("2006-01")})
}

// reportMeter sends the bandwidth served since the last report to Stripe's
// meter, in whole MB. The identifier names the month and running total, so
// a report Stripe received but whose answer was lost is not counted twice.
func (s *Service) reportMeter(ctx context.Context, a *store.Account, cfg *StripeSettings, row *store.AccountUsage, bw int64) error {
	if cfg.SecretKey == "" || cfg.MeterEvent == "" || a.StripeCustomerID == "" || s.Stripe == nil {
		return nil
	}
	total := bw / mb
	delta := total - row.ReportedMB
	if delta <= 0 {
		return nil
	}
	id := fmt.Sprintf("wpgenie-%d-%s-%d", a.ID, row.MonthStart.Format("200601"), total)
	if err := s.Stripe.MeterEvent(ctx, cfg.SecretKey, cfg.MeterEvent, a.StripeCustomerID, delta, id, s.now()); err != nil {
		return fmt.Errorf("reporting usage of account %d to Stripe: %w", a.ID, err)
	}
	return s.Store.SetUsageReported(ctx, a.ID, row.MonthStart, total)
}

// UsageReport is the usage of several accounts (the provisioning API's
// usage endpoint, WHMCS's UsageUpdate).
func (s *Service) UsageReport(ctx context.Context, accounts []*store.Account) ([]*Usage, error) {
	out := make([]*Usage, 0, len(accounts))
	for _, a := range accounts {
		u, err := s.UsageOf(ctx, a)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	slices.SortFunc(out, func(x, y *Usage) int { return int(x.AccountID - y.AccountID) })
	return out, nil
}
