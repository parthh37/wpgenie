package billing

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/parthh37/wpgenie/internal/store"
)

// Taxes, WHMCS-style: up to two levels (CGST + SGST, GST + PST). For each
// level the most specific rule matching the client wins: their state,
// then their country, then "everywhere" (country ""). A level-2 rule may
// compound: it applies to the subtotal plus the level-1 tax. Each tax line
// is rounded half up on the invoice's taxable amount (after discounts);
// with tax-inclusive prices the tax is backed out of them, and the lines
// add up exactly to what was backed out.

var (
	countryRe = regexp.MustCompile(`^[A-Z]{2}$`)
	stateRe   = regexp.MustCompile(`^[\p{L}\p{N} .'-]{1,50}$`)
)

// normalizeRule validates a tax rule and puts its codes in canonical form.
func normalizeRule(t *store.TaxRule) error {
	t.Name = strings.TrimSpace(t.Name)
	t.Country, t.State = strings.ToUpper(strings.TrimSpace(t.Country)), strings.ToUpper(strings.TrimSpace(t.State))
	if t.Level == 0 {
		t.Level = 1
	}
	switch {
	case t.Name == "" || textField("name", t.Name, 50, false) != nil:
		return fmt.Errorf("%w: a tax's name is 1-50 characters", ErrInvalid)
	case t.Country != "" && !countryRe.MatchString(t.Country):
		return fmt.Errorf("%w: the country is a two-letter ISO code (IN, US…), or empty for everywhere", ErrInvalid)
	case t.State != "" && (t.Country == "" || !stateRe.MatchString(t.State)):
		return fmt.Errorf("%w: a state needs a country, and is up to 50 letters or digits", ErrInvalid)
	case t.Rate < 0 || t.Rate > 100_00:
		return fmt.Errorf("%w: the rate is 0 to 10000 (hundredths of a percent: 1800 is 18%%)", ErrInvalid)
	case t.Level != 1 && t.Level != 2:
		return fmt.Errorf("%w: the level is 1 or 2", ErrInvalid)
	case t.Compound && t.Level != 2:
		return fmt.Errorf("%w: only a level-2 tax compounds (on the level-1 tax)", ErrInvalid)
	}
	return nil
}

func (s *Service) TaxRules(ctx context.Context) ([]*store.TaxRule, error) {
	return s.Store.TaxRules(ctx)
}

func (s *Service) CreateTaxRule(ctx context.Context, t *store.TaxRule) (*store.TaxRule, error) {
	if err := normalizeRule(t); err != nil {
		return nil, err
	}
	return s.Store.CreateTaxRule(ctx, t)
}

func (s *Service) UpdateTaxRule(ctx context.Context, t *store.TaxRule) (*store.TaxRule, error) {
	if err := normalizeRule(t); err != nil {
		return nil, err
	}
	if err := s.Store.UpdateTaxRule(ctx, t); err != nil {
		return nil, err
	}
	return s.Store.GetTaxRule(ctx, t.ID)
}

func (s *Service) DeleteTaxRule(ctx context.Context, id int64) error {
	return s.Store.DeleteTaxRule(ctx, id)
}

// TaxContext is what taxes an invoice: the rules that apply to its client
// (level 1 then level 2, at most one each) and whether prices include tax.
type TaxContext struct {
	Rules     []*store.TaxRule
	Inclusive bool
}

// SelectRules picks, per level, the most specific rule for a country and
// state.
func SelectRules(rules []*store.TaxRule, country, state string) []*store.TaxRule {
	country, state = strings.ToUpper(strings.TrimSpace(country)), strings.ToUpper(strings.TrimSpace(state))
	var out []*store.TaxRule
	for level := 1; level <= 2; level++ {
		var best *store.TaxRule
		bestScore := -1
		for _, r := range rules {
			if r.Level != level {
				continue
			}
			score := -1
			switch {
			case r.Country == "":
				score = 0
			case r.Country == country && r.State == "":
				score = 1
			case r.Country == country && state != "" && r.State == state:
				score = 2
			}
			if score > bestScore || (score == bestScore && score >= 0 && r.ID < best.ID) {
				best, bestScore = r, score
			}
		}
		if best != nil && bestScore >= 0 {
			out = append(out, best)
		}
	}
	return out
}

// taxContext is the tax that applies to a client: none when taxes are off,
// the account is exempt, or it gave a tax ID and that exempts.
func (s *Service) taxContext(ctx context.Context, cfg *InvoicingSettings, c store.BillingContact, exempt bool) (TaxContext, error) {
	tc := TaxContext{Inclusive: cfg.Tax.Enabled && cfg.Tax.Inclusive}
	if !cfg.Tax.Enabled || exempt || ExemptByTaxID(cfg, c) {
		return tc, nil
	}
	rules, err := s.Store.TaxRules(ctx)
	if err != nil {
		return tc, err
	}
	tc.Rules = SelectRules(rules, c.Country, c.State)
	return tc, nil
}

var taxIDRe = regexp.MustCompile(`^[A-Za-z0-9 -]{5,30}$`)

// PlausibleTaxID: 5-30 letters, digits, dashes or spaces, at least 4 of
// them digits (a VAT or GST number, not "none" or "n/a").
func PlausibleTaxID(id string) bool {
	id = strings.TrimSpace(id)
	if !taxIDRe.MatchString(id) {
		return false
	}
	digits := 0
	for _, r := range id {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 4
}

// ExemptByTaxID reports whether a client pays no tax for giving a tax ID
// (the setting is on, and the ID looks like one).
func ExemptByTaxID(cfg *InvoicingSettings, c store.BillingContact) bool {
	return cfg.Tax.Enabled && cfg.Tax.ExemptWithTaxID && PlausibleTaxID(c.TaxID)
}

// ComputeTotals totals invoice items: the subtotal (everything but
// discounts), the discount (positive), the taxes on the taxable amount
// after discounts, and the total.
func ComputeTotals(items []store.InvoiceItem, tc TaxContext) store.InvoiceTotals {
	var t store.InvoiceTotals
	var base int64
	for _, it := range items {
		if it.Kind == ItemDiscount {
			t.Discount -= it.Amount
		} else {
			t.Subtotal += it.Amount
		}
		if it.Taxable {
			base += it.Amount
		}
	}
	t.TaxLines = taxLines(base, tc)
	for _, l := range t.TaxLines {
		t.Tax += l.Amount
	}
	t.Total = t.Subtotal - t.Discount
	if !tc.Inclusive {
		t.Total += t.Tax
	}
	return t
}

// taxLines computes each tax on base. Exclusive: base is the net amount.
// Inclusive: base is gross; the net is base / (1+r1+r2) (or
// (1+r1)(1+r2) compounded), and the rounding difference goes to the last
// line so the lines sum to exactly gross - net.
func taxLines(base int64, tc TaxContext) []store.TaxLine {
	lines := []store.TaxLine{}
	if len(tc.Rules) == 0 {
		return lines
	}
	net := base
	if tc.Inclusive {
		num := new(big.Int).Mul(big.NewInt(base), big.NewInt(10000))
		den := big.NewInt(10000)
		var simple int64
		compounded := false
		for _, r := range tc.Rules {
			if r.Compound {
				compounded = true
			}
			simple += r.Rate
		}
		if compounded && len(tc.Rules) == 2 {
			num.Mul(num, big.NewInt(10000))
			den = new(big.Int).Mul(big.NewInt(10000+tc.Rules[0].Rate), big.NewInt(10000+tc.Rules[1].Rate))
		} else {
			den = big.NewInt(10000 + simple)
		}
		net = roundDiv(num, den)
	}
	var level1 int64
	for i, r := range tc.Rules {
		on := net
		if r.Compound && i > 0 {
			on += level1
		}
		amt := MulDiv(on, r.Rate, 10000)
		if i == 0 {
			level1 = amt
		}
		lines = append(lines, store.TaxLine{Name: r.Name, Rate: r.Rate, Amount: amt})
	}
	if tc.Inclusive {
		var sum int64
		for _, l := range lines {
			sum += l.Amount
		}
		lines[len(lines)-1].Amount += base - net - sum
	}
	return lines
}
