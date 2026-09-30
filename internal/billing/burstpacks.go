package billing

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"github.com/parthh37/wpgenie/internal/store"
)

// Burst minute packs: minutes clients buy on top of their plan's (see
// burst.go). Buying one makes a burst_topup invoice, paid like any other
// (or from the account's credit); once paid, its minutes are added to the
// account's burst credit exactly once (store.GrantBurstMinutes marks them
// granted in the same transaction), whatever replays a gateway sends.

const (
	KindBurstTopup = "burst_topup"
	ItemBurst      = "burst"
	// MethodCredit pays from the account's credit (burst packs).
	MethodCredit = "credit"
)

var packIDRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

func validPacks(packs []BurstPack) error {
	if len(packs) > 20 {
		return fmt.Errorf("%w: at most 20 burst packs", ErrInvalid)
	}
	var seen []string
	for _, p := range packs {
		switch {
		case !packIDRe.MatchString(p.ID):
			return fmt.Errorf("%w: a burst pack's id is 1-32 lowercase letters, digits or -", ErrInvalid)
		case slices.Contains(seen, p.ID):
			return fmt.Errorf("%w: burst pack %s is listed twice", ErrInvalid, p.ID)
		case p.Minutes <= 0 || p.Minutes > maxBurstCredit:
			return fmt.Errorf("%w: burst pack %s: minutes are 1 to %d", ErrInvalid, p.ID, maxBurstCredit)
		case p.Price < 0 || p.Price > maxAmount:
			return fmt.Errorf("%w: burst pack %s: invalid price", ErrInvalid, p.ID)
		}
		seen = append(seen, p.ID)
	}
	return nil
}

// BurstPurchase is a burst pack bought: its invoice and how to pay it.
type BurstPurchase struct {
	Invoice *InvoiceDetail `json:"invoice"`
	Next    *PayNext       `json:"next"`
}

// BuyBurstPack invoices a burst pack to an account and starts paying it
// with method (a payment method, or "credit"). A reseller's customers
// aren't billed here (ErrInvalid).
func (s *Service) BuyBurstPack(ctx context.Context, accountID int64, packID, method string) (*BurstPurchase, error) {
	cfg, err := s.Invoicing(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("%w: built-in billing is off", ErrConflict)
	}
	i := slices.IndexFunc(cfg.BurstPacks, func(p BurstPack) bool { return p.ID == packID })
	if i < 0 {
		return nil, fmt.Errorf("%w: no burst pack %q", ErrInvalid, packID)
	}
	pack := cfg.BurstPacks[i]
	if method != MethodCredit && !slices.Contains(cfg.methodIDs(), method) {
		return nil, fmt.Errorf("%w: %q isn't a payment method available now", ErrInvalid, method)
	}
	a, err := s.billableAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if a.Status == store.AccountTerminated {
		return nil, fmt.Errorf("%w: the account is terminated", ErrConflict)
	}
	if a.BurstCredit+pack.Minutes > maxBurstCredit {
		return nil, fmt.Errorf("%w: an account holds at most %d burst minutes", ErrInvalid, maxBurstCredit)
	}
	s.payMu.Lock()
	p, err := s.profile(ctx, accountID)
	var inv *store.Invoice
	if err == nil {
		inv, err = s.buildInvoice(ctx, cfg, newInvoice{Account: a, Profile: p, Kind: KindBurstTopup, Due: days(Day(s.now()), 7),
			Items: []store.InvoiceItem{{Kind: ItemBurst, Description: fmt.Sprintf("%d burst minutes", pack.Minutes), Quantity: 1,
				UnitPrice: pack.Price, Amount: pack.Price, Taxable: true}}})
	}
	if err == nil && method == MethodCredit && p.Credit < inv.Total {
		err = fmt.Errorf("%w: not enough credit (%s available)", ErrInvalid, FormatMoney(p.Credit, cfg.Currency))
	}
	if err == nil {
		inv.Units = pack.Minutes
		inv, err = s.createInvoiceLocked(ctx, cfg, inv, false)
	}
	if err == nil && method == MethodCredit && inv.Status == store.InvoiceUnpaid {
		var paid bool
		if _, paid, err = s.Store.ApplyCredit(ctx, inv.ID, 0, "system", s.now(), payNumbering(cfg)); err == nil && paid {
			if inv, err = s.Store.GetInvoice(ctx, inv.ID); err == nil {
				err = s.paidLocked(ctx, cfg, inv, nil)
			}
		}
	}
	s.payMu.Unlock()
	if err != nil {
		return nil, err
	}
	s.event(ctx, accountID, "burst", fmt.Sprintf("Burst pack %s (%d minutes) ordered", pack.ID, pack.Minutes))
	if inv, err = s.Store.GetInvoice(ctx, inv.ID); err != nil {
		return nil, err
	}
	out := &BurstPurchase{Invoice: s.invoiceDetail(cfg, inv)}
	if inv.Status != store.InvoiceUnpaid {
		out.Next = &PayNext{Paid: true}
		return out, nil
	}
	if out.Next, err = s.startPayment(ctx, cfg, inv, method, false); err != nil {
		return nil, err
	}
	return out, nil
}

// burstPaid grants a paid top-up's minutes (once) and meters burst again,
// so sites paused for want of minutes come back.
func (s *Service) burstPaid(ctx context.Context, inv *store.Invoice) error {
	n, err := s.Store.GrantBurstMinutes(ctx, inv.ID)
	if err != nil || n == 0 {
		return err
	}
	s.event(ctx, inv.AccountID, "burst", fmt.Sprintf("%d burst minutes added (invoice %s)", n, displayNumber(inv)))
	s.KickBurst()
	return nil
}
