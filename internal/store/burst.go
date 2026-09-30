package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Burst minutes: how many minutes each site ran above its normal size in
// a UTC month (recorded where the site runs; the panel keeps a copy of
// its other servers' sites), and what accounts have left. The rules live
// in internal/site (metering) and internal/billing (allowances).

// AddBurstMinutes adds minutes to a site's month.
func (s *Store) AddBurstMinutes(ctx context.Context, siteID string, month time.Time, n int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO burst_usage (site_id, month_start, minutes) VALUES (?, ?, ?)
		ON CONFLICT (site_id, month_start) DO UPDATE SET minutes = burst_usage.minutes + excluded.minutes`,
		siteID, month.Unix(), n)
	return err
}

// SetBurstReport records a site's month as another server counted it.
// Each server counts only the minutes the site ran there, so a site that
// moved has a report from each; a lower report than the last is ignored
// (a server's count only grows).
func (s *Store) SetBurstReport(ctx context.Context, siteID, nodeID string, month time.Time, n int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO burst_reports (site_id, node_id, month_start, minutes) VALUES (?, ?, ?, ?)
		ON CONFLICT (site_id, node_id, month_start) DO UPDATE SET minutes = excluded.minutes
		WHERE excluded.minutes > burst_reports.minutes`, siteID, nodeID, month.Unix(), n)
	return err
}

// BurstMinutes returns a month's minutes by site, wherever they were
// counted (here and other servers' reports): the given sites', or with
// none given, every site's.
func (s *Store) BurstMinutes(ctx context.Context, month time.Time, siteIDs ...string) (map[string]int64, error) {
	filter, args := burstFilter(month, siteIDs)
	return s.burstSums(ctx, `SELECT site_id, SUM(minutes) FROM (SELECT site_id, minutes FROM burst_usage WHERE `+filter+
		` UNION ALL SELECT site_id, minutes FROM burst_reports WHERE `+filter+`) AS m GROUP BY site_id`, append(args, args...)...)
}

// LocalBurstMinutes returns the minutes counted on this server only (what
// it reports to the panel).
func (s *Store) LocalBurstMinutes(ctx context.Context, month time.Time) (map[string]int64, error) {
	filter, args := burstFilter(month, nil)
	return s.burstSums(ctx, `SELECT site_id, minutes FROM burst_usage WHERE `+filter, args...)
}

func burstFilter(month time.Time, siteIDs []string) (string, []any) {
	q, args := `month_start = ?`, []any{month.Unix()}
	if len(siteIDs) > 0 {
		q += ` AND site_id IN (` + placeholders(len(siteIDs)) + `)`
		for _, id := range siteIDs {
			args = append(args, id)
		}
	}
	return q, args
}

func (s *Store) burstSums(ctx context.Context, q string, args ...any) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// BurstCharged returns, by site, the minutes of a month already charged
// to accounts (whichever account owned the site at the time).
func (s *Store) BurstCharged(ctx context.Context, month time.Time) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, SUM(minutes) FROM burst_charges WHERE month_start = ?
		GROUP BY site_id`, month.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// AddBurstCharge charges a site's new minutes to the account that owns
// it now. Charges are never taken back: a deleted site's minutes stay
// used, and a site given to another account brings only its later
// minutes along.
func (s *Store) AddBurstCharge(ctx context.Context, accountID int64, siteID string, month time.Time, n int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO burst_charges (account_id, site_id, month_start, minutes) VALUES (?, ?, ?, ?)
		ON CONFLICT (account_id, site_id, month_start) DO UPDATE SET minutes = burst_charges.minutes + excluded.minutes`,
		accountID, siteID, month.Unix(), n)
	return err
}

// AccountBurstCharges returns the minutes charged to accounts in a month,
// by account, and by site within each account.
func (s *Store) AccountBurstCharges(ctx context.Context, month time.Time) (map[int64]map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, site_id, minutes FROM burst_charges WHERE month_start = ?`, month.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]int64{}
	for rows.Next() {
		var a int64
		var id string
		var n int64
		if err := rows.Scan(&a, &id, &n); err != nil {
			return nil, err
		}
		if out[a] == nil {
			out[a] = map[string]int64{}
		}
		out[a][id] = n
	}
	return out, rows.Err()
}

// AccountBurst is an account's burst minutes in one month: used in total,
// how many beyond the plan's are settled (paid from the credit, or
// forgiven when it ran dry: burst pauses at zero, and minutes run while
// the pause took effect aren't billed), how many of those the credit
// actually paid, and the last notification sent (billing's levels).
type AccountBurst struct {
	Used        int64
	FromCredit  int64
	CreditTaken int64
	Notified    int
}

// AccountBurstFor returns an account's burst minutes of a month (zero
// when none were recorded).
func (s *Store) AccountBurstFor(ctx context.Context, accountID int64, month time.Time) (AccountBurst, error) {
	var b AccountBurst
	err := s.db.QueryRowContext(ctx, `SELECT burst_minutes, burst_from_credit, burst_credit_taken, burst_notified
		FROM account_usage WHERE account_id = ? AND month_start = ?`, accountID, month.Unix()).
		Scan(&b.Used, &b.FromCredit, &b.CreditTaken, &b.Notified)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountBurst{}, nil
	}
	return b, err
}

// ChargeBurst records an account's burst minutes of a month and settles
// its credit, in one transaction: fromCredit is how many of the month's
// minutes go beyond the plan's (see SettleBurst). It returns the credit
// left.
func (s *Store) ChargeBurst(ctx context.Context, accountID int64, month time.Time, used, fromCredit int64, notified int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var credit int64
	if err := tx.QueryRowContext(ctx, `SELECT burst_credit FROM accounts WHERE id = ?`, accountID).Scan(&credit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var prev AccountBurst
	err = tx.QueryRowContext(ctx, `SELECT burst_from_credit, burst_credit_taken FROM account_usage
		WHERE account_id = ? AND month_start = ?`, accountID, month.Unix()).Scan(&prev.FromCredit, &prev.CreditTaken)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	settled, taken, d := SettleBurst(fromCredit, prev.FromCredit, prev.CreditTaken, credit)
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_usage (account_id, month_start, burst_minutes, burst_from_credit,
		burst_credit_taken, burst_notified, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (account_id, month_start)
		DO UPDATE SET burst_minutes = excluded.burst_minutes, burst_from_credit = excluded.burst_from_credit,
		burst_credit_taken = excluded.burst_credit_taken, burst_notified = excluded.burst_notified`,
		accountID, month.Unix(), used, settled, taken, notified, time.Now().Unix()); err != nil {
		return 0, err
	}
	if d != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET burst_credit = burst_credit + ? WHERE id = ?`, d, accountID); err != nil {
			return 0, err
		}
	}
	return credit + d, tx.Commit()
}

// SettleBurst settles a month's minutes beyond the plan: want of them, of
// which settled were settled before and taken paid from the credit. New
// ones are paid as far as the credit goes and the rest forgiven (a later
// top-up is for later bursts); fewer (a bigger plan) give back what was
// paid, never more. It returns the new settled and taken, and the change
// to the credit.
func SettleBurst(want, settled, taken, credit int64) (newSettled, newTaken, change int64) {
	switch d := want - settled; {
	case d > 0:
		pay := min(d, max(credit, 0))
		return want, taken + pay, -pay
	case d < 0:
		back := min(-d, taken)
		return want, taken - back, back
	}
	return settled, taken, 0
}

// AddBurstCredit adds minutes to an account's credit (a negative amount
// takes them away, never below zero) and returns the new balance.
func (s *Store) AddBurstCredit(ctx context.Context, accountID, minutes int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE accounts SET burst_credit = CASE WHEN burst_credit + ? < 0 THEN 0
		ELSE burst_credit + ? END, updated_at = ? WHERE id = ?`, minutes, minutes, time.Now().Unix(), accountID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	var credit int64
	if err := tx.QueryRowContext(ctx, `SELECT burst_credit FROM accounts WHERE id = ?`, accountID).Scan(&credit); err != nil {
		return 0, err
	}
	return credit, tx.Commit()
}
