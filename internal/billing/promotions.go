package billing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Promotions: promo codes giving a percentage or a fixed amount off a
// plan's price (not its setup fee), for some plans and cycles or all,
// between two dates, a limited number of times. A recurring promotion
// keeps applying to the account's renewals (for as long as it exists and
// is enabled); otherwise only the first invoice gets it.

// Promotion is a promotion as the API shows it.
type Promotion struct {
	ID             int64      `json:"id"`
	Code           string     `json:"code"`
	Description    string     `json:"description"`
	Type           string     `json:"type"`
	Value          int64      `json:"value"`
	Plans          []string   `json:"plans"`
	Cycles         []string   `json:"cycles"`
	Recurring      bool       `json:"recurring"`
	MaxUses        int        `json:"max_uses"`
	Uses           int        `json:"uses"`
	StartsAt       *time.Time `json:"starts_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	NewClientsOnly bool       `json:"new_clients_only"`
	Enabled        bool       `json:"enabled"`
}

// tptr is a time for JSON: null when zero.
func tptr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func promoView(p *store.Promotion) Promotion {
	return Promotion{ID: p.ID, Code: p.Code, Description: p.Description, Type: p.Type, Value: p.Value, Plans: p.Plans,
		Cycles: p.Cycles, Recurring: p.Recurring, MaxUses: p.MaxUses, Uses: p.Uses, StartsAt: tptr(p.StartsAt),
		ExpiresAt: tptr(p.ExpiresAt), NewClientsOnly: p.NewClientsOnly, Enabled: p.Enabled}
}

var promoCodeRe = regexp.MustCompile(`^[A-Z0-9_-]{2,32}$`)

func (s *Service) normalizePromo(ctx context.Context, in Promotion) (*store.Promotion, error) {
	p := &store.Promotion{ID: in.ID, Code: strings.ToUpper(strings.TrimSpace(in.Code)), Description: strings.TrimSpace(in.Description),
		Type: in.Type, Value: in.Value, Recurring: in.Recurring, MaxUses: in.MaxUses, NewClientsOnly: in.NewClientsOnly,
		Enabled: in.Enabled, Plans: []string{}, Cycles: []string{}}
	if in.StartsAt != nil {
		p.StartsAt = in.StartsAt.UTC()
	}
	if in.ExpiresAt != nil {
		p.ExpiresAt = in.ExpiresAt.UTC()
	}
	switch {
	case !promoCodeRe.MatchString(p.Code):
		return nil, fmt.Errorf("%w: a code is 2-32 letters, digits, - or _", ErrInvalid)
	case textField("description", p.Description, 200, false) != nil:
		return nil, fmt.Errorf("%w: the description is at most 200 characters on one line", ErrInvalid)
	case p.Type != "percent" && p.Type != "fixed":
		return nil, fmt.Errorf("%w: the type is percent or fixed", ErrInvalid)
	case p.Value <= 0 || (p.Type == "percent" && p.Value > 10000) || p.Value > maxAmount:
		return nil, fmt.Errorf("%w: the value is a positive amount, or 1 to 10000 for a percentage (1000 is 10%%)", ErrInvalid)
	case p.MaxUses < 0:
		return nil, fmt.Errorf("%w: max_uses can't be negative (0: unlimited)", ErrInvalid)
	case !p.StartsAt.IsZero() && !p.ExpiresAt.IsZero() && !p.ExpiresAt.After(p.StartsAt):
		return nil, fmt.Errorf("%w: a promotion expires after it starts", ErrInvalid)
	}
	for _, id := range in.Plans {
		if _, err := s.Store.GetPlan(ctx, id); err != nil {
			return nil, fmt.Errorf("%w: no plan %q", ErrInvalid, id)
		}
		if !slices.Contains(p.Plans, id) {
			p.Plans = append(p.Plans, id)
		}
	}
	for _, c := range in.Cycles {
		if CycleMonths(c) == 0 {
			return nil, fmt.Errorf("%w: unknown cycle %q", ErrInvalid, c)
		}
		if !slices.Contains(p.Cycles, c) {
			p.Cycles = append(p.Cycles, c)
		}
	}
	slices.Sort(p.Plans)
	return p, nil
}

func (s *Service) Promotions(ctx context.Context) ([]Promotion, error) {
	list, err := s.Store.Promotions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Promotion, len(list))
	for i, p := range list {
		out[i] = promoView(p)
	}
	return out, nil
}

func (s *Service) CreatePromotion(ctx context.Context, in Promotion) (*Promotion, error) {
	p, err := s.normalizePromo(ctx, in)
	if err != nil {
		return nil, err
	}
	p, err = s.Store.CreatePromotion(ctx, p)
	if errors.Is(err, store.ErrExists) {
		return nil, fmt.Errorf("%w: code %s is taken", ErrConflict, in.Code)
	}
	if err != nil {
		return nil, err
	}
	v := promoView(p)
	return &v, nil
}

func (s *Service) UpdatePromotion(ctx context.Context, in Promotion) (*Promotion, error) {
	p, err := s.normalizePromo(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := s.Store.UpdatePromotion(ctx, p); errors.Is(err, store.ErrExists) {
		return nil, fmt.Errorf("%w: code %s is taken", ErrConflict, in.Code)
	} else if err != nil {
		return nil, err
	}
	p, err = s.Store.GetPromotion(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	v := promoView(p)
	return &v, nil
}

func (s *Service) DeletePromotion(ctx context.Context, id int64) error {
	return s.Store.DeletePromotion(ctx, id)
}

// applies reports whether a promotion covers a plan and cycle.
func promoApplies(p *store.Promotion, plan, cycle string) bool {
	return (len(p.Plans) == 0 || slices.Contains(p.Plans, plan)) && (len(p.Cycles) == 0 || slices.Contains(p.Cycles, cycle))
}

// checkPromo finds a code a new order may use now: an error saying why
// not otherwise (for the order form to show).
func (s *Service) checkPromo(ctx context.Context, code, plan, cycle string) (*store.Promotion, error) {
	p, err := s.Store.PromotionByCode(ctx, strings.TrimSpace(code))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: that code doesn't exist", ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	switch {
	case !p.Enabled:
		return nil, fmt.Errorf("%w: that code isn't active", ErrInvalid)
	case !p.StartsAt.IsZero() && now.Before(p.StartsAt):
		return nil, fmt.Errorf("%w: that code isn't valid yet", ErrInvalid)
	case !p.ExpiresAt.IsZero() && !now.Before(p.ExpiresAt):
		return nil, fmt.Errorf("%w: that code has expired", ErrInvalid)
	case p.MaxUses > 0 && p.Uses >= p.MaxUses:
		return nil, fmt.Errorf("%w: that code has been used up", ErrInvalid)
	case !promoApplies(p, plan, cycle):
		return nil, fmt.Errorf("%w: that code doesn't apply to this plan or billing cycle", ErrInvalid)
	}
	return p, nil
}

// promoDiscount is a promotion's discount on a price (never more than it).
func promoDiscount(p *store.Promotion, price int64) int64 {
	if p == nil || price <= 0 {
		return 0
	}
	if p.Type == "percent" {
		return min(MulDiv(price, p.Value, 10000), price)
	}
	return min(p.Value, price)
}

// discountItem is the invoice line of a promotion's discount (none when 0).
func discountItem(p *store.Promotion, amount int64, c Currency) []store.InvoiceItem {
	if amount <= 0 {
		return nil
	}
	what := FormatMoney(p.Value, c) + " off"
	if p.Type == "percent" {
		what = strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%d.%02d", p.Value/100, p.Value%100), "0"), ".") + "% off"
	}
	desc := "Promotion " + p.Code + " — " + what
	return []store.InvoiceItem{{Kind: ItemDiscount, Description: desc, Quantity: 1, UnitPrice: -amount, Amount: -amount, Taxable: true}}
}
