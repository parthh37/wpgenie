package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestAddMonthsAnchoring(t *testing.T) {
	for _, c := range []struct {
		from   time.Time
		n, day int
		want   time.Time
	}{
		{day(2026, 1, 31), 1, 31, day(2026, 2, 28)},
		{day(2026, 2, 28), 1, 31, day(2026, 3, 31)}, // back to the 31st
		{day(2028, 1, 31), 1, 31, day(2028, 2, 29)}, // leap year
		{day(2026, 1, 30), 1, 30, day(2026, 2, 28)},
		{day(2026, 3, 31), -1, 31, day(2026, 2, 28)},
		{day(2026, 1, 31), 12, 31, day(2027, 1, 31)},
		{day(2026, 11, 15), 3, 15, day(2027, 2, 15)},
		{day(2026, 11, 30), 3, 0, day(2027, 2, 28)}, // anchor 0: the date's own day
		{time.Date(2026, 5, 31, 17, 30, 0, 0, time.UTC), 1, 31, day(2026, 6, 30)},
	} {
		if got := AddMonths(c.from, c.n, c.day); !got.Equal(c.want) {
			t.Errorf("AddMonths(%s, %d, %d) = %s, want %s", c.from.Format(time.DateOnly), c.n, c.day,
				got.Format(time.DateOnly), c.want.Format(time.DateOnly))
		}
	}
	// A year of monthly due dates anchored on the 31st.
	due, want := day(2026, 1, 31), []int{28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	for i, d := range want {
		if due = AddMonths(due, 1, 31); due.Day() != d {
			t.Fatalf("month %d: %s", i+2, due.Format(time.DateOnly))
		}
	}
}

func TestMoneyRounding(t *testing.T) {
	for _, c := range []struct{ a, b, c, want int64 }{
		{5, 1, 2, 3}, {-5, 1, 2, -3}, {4, 1, 2, 2}, {1, 1, 3, 0}, {2, 1, 3, 1}, {1015, 1800, 10000, 183},
		{1005, 1000, 10000, 101}, {1_000_000_000_000, 100_000_000, 100_000_000, 1_000_000_000_000},
	} {
		if got := MulDiv(c.a, c.b, c.c); got != c.want {
			t.Errorf("MulDiv(%d, %d, %d) = %d, want %d", c.a, c.b, c.c, got, c.want)
		}
	}
	usd := Currency{Code: "USD", Symbol: "$", Decimals: 2}
	for _, c := range []struct {
		v    int64
		c    Currency
		want string
	}{
		{11800, usd, "$118.00"}, {123456789, usd, "$1,234,567.89"}, {-5, usd, "-$0.05"}, {0, usd, "$0.00"},
		{1500, Currency{Code: "JPY", Symbol: "¥", Decimals: 0}, "¥1,500"},
		{12345, Currency{Code: "KWD", Decimals: 3}, "KWD 12.345"},
	} {
		if got := FormatMoney(c.v, c.c); got != c.want {
			t.Errorf("FormatMoney(%d) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestTaxes(t *testing.T) {
	rules := []*store.TaxRule{
		{ID: 1, Name: "CGST", Country: "IN", Rate: 900, Level: 1},
		{ID: 2, Name: "SGST", Country: "IN", Rate: 900, Level: 2},
		{ID: 3, Name: "IGST MH", Country: "IN", State: "MH", Rate: 1800, Level: 1},
		{ID: 4, Name: "GST", Country: "CA", Rate: 500, Level: 1},
		{ID: 5, Name: "PST", Country: "CA", Rate: 700, Level: 2, Compound: true},
		{ID: 6, Name: "VAT", Rate: 2000, Level: 1},
		{ID: 7, Name: "VAT again", Rate: 1000, Level: 1},
	}
	names := func(rs []*store.TaxRule) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return strings.Join(out, "+")
	}
	for _, c := range []struct{ country, state, want string }{
		{"IN", "KA", "CGST+SGST"},
		{"in", "mh", "IGST MH+SGST"}, // state beats country, per level
		{"FR", "", "VAT"},            // everywhere; the lowest ID of equals
		{"CA", "BC", "GST+PST"},
		{"", "", "VAT"},
	} {
		if got := names(SelectRules(rules, c.country, c.state)); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.country, c.state, got, c.want)
		}
	}
	plan := store.InvoiceItem{Kind: ItemPlan, Amount: 10000, Taxable: true}
	disc := store.InvoiceItem{Kind: ItemDiscount, Amount: -1000, Taxable: true}
	setup := store.InvoiceItem{Kind: ItemSetup, Amount: 500}
	gst := []*store.TaxRule{{Name: "GST", Rate: 1800, Level: 1}}
	// The spec's example: tax on the subtotal after the discount.
	tot := ComputeTotals([]store.InvoiceItem{plan, disc}, TaxContext{Rules: gst})
	if tot.Subtotal != 10000 || tot.Discount != 1000 || tot.Tax != 1620 || tot.Total != 10620 || tot.TaxLines[0].Amount != 1620 {
		t.Fatalf("exclusive %+v", tot)
	}
	// Non-taxable lines are totalled, not taxed; two levels.
	tot = ComputeTotals([]store.InvoiceItem{plan, disc, setup}, TaxContext{Rules: SelectRules(rules, "IN", "KA")})
	if tot.Subtotal != 10500 || tot.Tax != 1620 || tot.TaxLines[0].Amount != 810 || tot.TaxLines[1].Amount != 810 ||
		tot.Total != 11120 {
		t.Fatalf("two levels %+v", tot)
	}
	// Compound: PST on the subtotal plus GST.
	tot = ComputeTotals([]store.InvoiceItem{plan}, TaxContext{Rules: SelectRules(rules, "CA", "ON")})
	if tot.TaxLines[0].Amount != 500 || tot.TaxLines[1].Amount != 735 || tot.Total != 11235 {
		t.Fatalf("compound %+v", tot)
	}
	// Half up, per line.
	tot = ComputeTotals([]store.InvoiceItem{{Kind: ItemCustom, Amount: 1015, Taxable: true}}, TaxContext{Rules: gst})
	if tot.Tax != 183 {
		t.Fatalf("rounding %+v", tot)
	}
	// Inclusive: the tax is backed out; the lines add up to exactly what
	// was backed out and the total is the price.
	for _, c := range []struct {
		gross int64
		rules []*store.TaxRule
		net   int64
	}{
		{11800, gst, 10000},
		{999, gst, 847},
		{1000, SelectRules(rules, "IN", "KA"), 847},
		{11235, SelectRules(rules, "CA", "ON"), 10000},
		{1, SelectRules(rules, "IN", "KA"), 1},
	} {
		tot := ComputeTotals([]store.InvoiceItem{{Kind: ItemPlan, Amount: c.gross, Taxable: true}}, TaxContext{Rules: c.rules,
			Inclusive: true})
		if tot.Total != c.gross || tot.Tax != c.gross-c.net {
			t.Errorf("inclusive %d: %+v, want net %d", c.gross, tot, c.net)
		}
		var sum int64
		for _, l := range tot.TaxLines {
			sum += l.Amount
		}
		if sum != tot.Tax {
			t.Errorf("inclusive %d: lines %+v don't add up to %d", c.gross, tot.TaxLines, tot.Tax)
		}
	}
	// A credit (negative) is taxed negatively: a downgrade gives the tax back.
	tot = ComputeTotals([]store.InvoiceItem{{Kind: ItemCredit, Amount: -1000, Taxable: true}}, TaxContext{Rules: gst})
	if tot.Tax != -180 || tot.Total != -1180 {
		t.Fatalf("negative %+v", tot)
	}

	// Who is exempt.
	e := newInvEnv(t)
	ctx := context.Background()
	cfg, _ := e.svc.Invoicing(ctx)
	e.svc.CreateTaxRule(ctx, &store.TaxRule{Name: "GST", Country: "in", Rate: 1800})
	in := store.BillingContact{Country: "IN", State: "KA"}
	if tc, _ := e.svc.taxContext(ctx, cfg, in, false); len(tc.Rules) != 0 {
		t.Fatal("taxes while disabled")
	}
	cfg.Tax.Enabled, cfg.Tax.ExemptWithTaxID = true, true
	if tc, _ := e.svc.taxContext(ctx, cfg, in, false); len(tc.Rules) != 1 || tc.Rules[0].Country != "IN" {
		t.Fatalf("rules %+v", tc)
	}
	if tc, _ := e.svc.taxContext(ctx, cfg, in, true); len(tc.Rules) != 0 {
		t.Fatal("an exempt account taxed")
	}
	// Only something that looks like a tax ID exempts.
	for _, id := range []string{"none", "n/a", "GST123", "x", "27AAPFU0939F1ZV 27AAPFU0939F1ZV 27AAPFU"} {
		in.TaxID = id
		if tc, _ := e.svc.taxContext(ctx, cfg, in, false); len(tc.Rules) != 1 || ExemptByTaxID(cfg, in) {
			t.Errorf("tax ID %q exempts", id)
		}
	}
	for _, id := range []string{"27AAPFU0939F1ZV", "DE 123 456 789", "GB-1234"} {
		in.TaxID = id
		if tc, _ := e.svc.taxContext(ctx, cfg, in, false); len(tc.Rules) != 0 || !ExemptByTaxID(cfg, in) {
			t.Errorf("tax ID %q doesn't exempt", id)
		}
	}
	for _, bad := range []store.TaxRule{{Name: "", Rate: 100}, {Name: "X", Country: "IND"}, {Name: "X", State: "MH"},
		{Name: "X", Rate: 10001}, {Name: "X", Level: 3}, {Name: "X", Compound: true}} {
		if _, err := e.svc.CreateTaxRule(ctx, &bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// ---- A billing environment: priced plans, settings, gateways ----

// fakeGateways stands in for Stripe's and Razorpay's APIs.
type fakeGateways struct {
	mu        sync.Mutex
	calls     []string
	checkouts []url.Values
	charges   []url.Values
	refunds   []url.Values
	links     []map[string]any
	decline   bool
	payments  map[string]string // Razorpay payment ID -> JSON
}

func (f *fakeGateways) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGateways) stripe(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk_test_1" {
		w.WriteHeader(401)
		return
	}
	r.ParseForm()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/customers":
		io.WriteString(w, `{"id":"cus_T1"}`)
	case r.Method == "POST" && r.URL.Path == "/v1/checkout/sessions":
		f.checkouts = append(f.checkouts, r.PostForm)
		io.WriteString(w, `{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/payment_intents/"):
		io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/v1/payment_intents/")+`","payment_method":"pm_card1"}`)
	case r.Method == "GET" && r.URL.Path == "/v1/payment_methods/pm_card1":
		io.WriteString(w, `{"id":"pm_card1","card":{"brand":"visa","last4":"4242","exp_month":12,"exp_year":2030}}`)
	case r.Method == "POST" && r.URL.Path == "/v1/payment_methods/pm_card1/detach":
		io.WriteString(w, `{}`)
	case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
		f.charges = append(f.charges, r.PostForm)
		if f.decline {
			w.WriteHeader(402)
			io.WriteString(w, `{"error":{"message":"Your card was declined.","code":"card_declined"}}`)
			return
		}
		io.WriteString(w, `{"id":"pi_auto`+r.PostForm.Get("metadata[wpgenie_invoice]")+`","status":"succeeded","amount_received":`+
			r.PostForm.Get("amount")+`}`)
	case r.Method == "POST" && r.URL.Path == "/v1/refunds":
		f.refunds = append(f.refunds, r.PostForm)
		io.WriteString(w, `{"id":"re_test`+r.PostForm.Get("amount")+`"}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGateways) razorpay(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, secret, ok := r.BasicAuth(); !ok || id != "rzp_test_abcdef12" || secret != "rzsecret" {
		w.WriteHeader(401)
		return
	}
	f.calls = append(f.calls, "rzp "+r.Method+" "+r.URL.Path)
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/payment_links":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.links = append(f.links, body)
		io.WriteString(w, `{"id":"plink_T1","short_url":"https://rzp.io/i/T1","reference_id":"`+body["reference_id"].(string)+`"}`)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/payments/"):
		if p, ok := f.payments[strings.TrimPrefix(r.URL.Path, "/v1/payments/")]; ok {
			io.WriteString(w, p)
			return
		}
		w.WriteHeader(404)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/refund"):
		io.WriteString(w, `{"id":"rfnd_TEST0001"}`)
	default:
		w.WriteHeader(404)
	}
}

type invEnv struct {
	*env
	gw *fakeGateways
}

// newInvEnv is newEnv with built-in billing on: priced public plans, USD,
// the three payment methods, and fake gateways.
func newInvEnv(t *testing.T) *invEnv {
	t.Helper()
	e := &invEnv{env: newEnv(t), gw: &fakeGateways{payments: map[string]string{}}}
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	e.svc.Mailer = &mailer.Service{Store: e.store, Log: log, PanelURL: "https://panel.test", Now: func() time.Time { return e.now }}
	e.svc.PanelURL = "https://panel.test"
	stripeSrv := httptest.NewServer(http.HandlerFunc(e.gw.stripe))
	t.Cleanup(stripeSrv.Close)
	rzpSrv := httptest.NewServer(http.HandlerFunc(e.gw.razorpay))
	t.Cleanup(rzpSrv.Close)
	e.svc.Stripe, e.svc.Razorpay = &StripeAPI{Base: stripeSrv.URL}, &RazorpayAPI{Base: rzpSrv.URL}
	for _, p := range []*store.Plan{
		{ID: "basic", Name: "Basic", MaxSites: 3, BandwidthGB: 10, OverageGBPrice: 100, Public: true, Sort: 1,
			Prices: map[string]store.PlanPrice{"monthly": {Price: 1000}, "annually": {Price: 10000, SetupFee: 500}}},
		{ID: "pro", Name: "Pro", MaxSites: 10, Public: true, Sort: 2,
			Prices: map[string]store.PlanPrice{"monthly": {Price: 3000}, "annually": {Price: 30000}}},
		{ID: "private", Name: "Private", Prices: map[string]store.PlanPrice{"monthly": {Price: 500}}},
		{ID: "agency", Name: "Agency", AccountKind: store.AccountReseller, Public: true,
			Prices: map[string]store.PlanPrice{"monthly": {Price: 9000}}},
	} {
		if err := NormalizePlan(p); err != nil {
			t.Fatal(err)
		}
		if err := e.store.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	secret, key := "whsec_test", "sk_test_1"
	if err := e.svc.SetStripeSettings(ctx, StripeSettingsInput{WebhookSecret: &secret, SecretKey: &key}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultInvoicing()
	cfg.Enabled = true
	cfg.Company = Company{Name: "Host Co", Address: "1 Main St\nSpringfield", Website: "https://host.test", TaxID: "TX-1"}
	cfg.Methods.Stripe.Enabled = true
	rs, ws := "rzsecret", "rzwebhook"
	cfg.Methods.Razorpay = RazorpayMethod{Enabled: true, Name: "UPI", KeyID: "rzp_test_abcdef12", KeySecret: &rs, WebhookSecret: &ws}
	cfg.Methods.Manual.Instructions = "IBAN XX00 1234"
	if _, err := e.svc.SetInvoicing(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

// setting changes the invoicing settings.
func (e *invEnv) setting(f func(*InvoicingSettings)) {
	e.t.Helper()
	cfg, err := e.svc.Invoicing(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	f(cfg)
	if _, err := e.svc.SetInvoicing(context.Background(), *cfg); err != nil {
		e.t.Fatal(err)
	}
}

// billed makes an account billed by invoice from next.
func (e *invEnv) billed(name, plan, cycle string, next time.Time) *store.Account {
	e.t.Helper()
	ctx := context.Background()
	a := e.account(AccountInput{Name: name, PlanID: plan, Email: strings.ToLower(name) + "@client.test"})
	mode, nd := ModeInvoice, next.Format(time.DateOnly)
	if _, err := e.svc.UpdateProfile(ctx, a.ID, ProfileInput{Mode: &mode, Cycle: &cycle, NextDueAt: &nd}); err != nil {
		e.t.Fatal(err)
	}
	return a
}

// mails counts the queued e-mails of a template (to an account: 0 any).
func (e *invEnv) mails(template string, account int64) int {
	e.t.Helper()
	list, err := e.store.MailLog(context.Background(), store.MailFilter{AccountID: account, Limit: 500})
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, m := range list {
		if m.Template == template {
			n++
		}
	}
	return n
}

func (e *invEnv) invoices(account int64) []*store.Invoice {
	e.t.Helper()
	list, err := e.store.ListInvoices(context.Background(), store.InvoiceFilter{AccountID: account})
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func (e *invEnv) manualInvoice(account int64, amount int64, draft bool) *InvoiceDetail {
	e.t.Helper()
	inv, err := e.svc.CreateInvoice(context.Background(), ManualInvoiceInput{AccountID: account, Draft: draft, SendEmail: true,
		Items: []ItemInput{{Description: "Consulting", Quantity: 1, UnitPrice: amount, Taxable: true}}})
	if err != nil {
		e.t.Fatal(err)
	}
	return inv
}

func TestInvoicingSettings(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	cfg, err := e.svc.Invoicing(ctx)
	if err != nil || !cfg.StripeReady || !cfg.Methods.Razorpay.KeySecretSet || !cfg.Methods.Razorpay.WebhookSecretSet ||
		cfg.Methods.Razorpay.WebhookURL != "https://panel.test/api/v1/billing/razorpay/webhook" || cfg.Invoice.NextNumber != 1 {
		t.Fatalf("settings %+v %v", cfg, err)
	}
	red := cfg.Redacted()
	if b, _ := json.Marshal(red); strings.Contains(string(b), "rzsecret") || strings.Contains(string(b), "rzwebhook") {
		t.Fatalf("secrets leak: %s", b)
	}
	// Saving what the API showed keeps the secrets.
	if _, err := e.svc.SetInvoicing(ctx, red); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = e.svc.Invoicing(ctx); str(cfg.Methods.Razorpay.KeySecret) != "rzsecret" {
		t.Fatal("secret lost")
	}
	if got := len(cfg.PayMethods()); got != 3 {
		t.Fatalf("%d methods", got)
	}
	for name, f := range map[string]func(*InvoicingSettings){
		"currency":   func(c *InvoicingSettings) { c.Currency.Code = "DOLLARS" },
		"decimals":   func(c *InvoicingSettings) { c.Currency.Decimals = 4 },
		"number_on":  func(c *InvoicingSettings) { c.Invoice.NumberOn = "never" },
		"prefix":     func(c *InvoicingSettings) { c.Invoice.Prefix = "<script>" },
		"late fee":   func(c *InvoicingSettings) { c.Automation.LateFee = LateFee{Type: "percent", Amount: 20000} },
		"terminate":  func(c *InvoicingSettings) { c.Automation.TerminateAfterDays = 2 }, // before suspension (5)
		"overdue":    func(c *InvoicingSettings) { c.Automation.OverdueReminderDays = []int{0, 3} },
		"razorpay":   func(c *InvoicingSettings) { c.Methods.Razorpay.KeyID = "key" },
		"terms":      func(c *InvoicingSettings) { c.Invoice.TermsURL = "javascript:alert(1)" },
		"email":      func(c *InvoicingSettings) { c.Company.Email = "nope" },
		"company":    func(c *InvoicingSettings) { c.Company.Name = "A\x00B" },
		"rzp secret": func(c *InvoicingSettings) { s := "has space"; c.Methods.Razorpay.KeySecret = &s },
	} {
		c := cfg.Redacted()
		f(&c)
		if _, err := e.svc.SetInvoicing(ctx, c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Overdue reminder days are kept sorted and unique.
	e.setting(func(c *InvoicingSettings) { c.Automation.OverdueReminderDays = []int{7, 1, 3, 3} })
	if cfg, _ = e.svc.Invoicing(ctx); len(cfg.Automation.OverdueReminderDays) != 3 || cfg.Automation.OverdueReminderDays[0] != 1 {
		t.Fatalf("days %v", cfg.Automation.OverdueReminderDays)
	}
	// The e-mail brand is the company's.
	b := e.svc.MailBrand(ctx)
	if b.Name != "Host Co" || b.URL != "https://host.test" || b.Footer != "1 Main St · Springfield · Tax ID TX-1" {
		t.Fatalf("brand %+v", b)
	}
	e.setting(func(c *InvoicingSettings) { c.Enabled = false })
	if b := e.svc.MailBrand(ctx); b.Name != "" {
		t.Fatal("brand while billing is off")
	}
	cc, _ := e.svc.ClientConfig(ctx)
	if cc.Enabled || cc.CompanyName != "Host Co" || cc.Currency.Code != "USD" {
		t.Fatalf("client config %+v", cc)
	}
}

func TestPlanPricesValidation(t *testing.T) {
	for _, p := range []*store.Plan{
		{ID: "a", Name: "A", Prices: map[string]store.PlanPrice{"weekly": {Price: 1}}},
		{ID: "a", Name: "A", Prices: map[string]store.PlanPrice{"monthly": {Price: -1}}},
		{ID: "a", Name: "A", AccountKind: "admin"},
		{ID: "a", Name: "A", OverageGBPrice: -5},
	} {
		if err := NormalizePlan(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	p := &store.Plan{ID: "a", Name: "A"}
	if err := NormalizePlan(p); err != nil || p.AccountKind != "customer" || p.Prices == nil {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestPromotions(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	past, future := e.now.Add(-time.Hour), e.now.Add(time.Hour)
	mk := func(p Promotion) *Promotion {
		t.Helper()
		out, err := e.svc.CreatePromotion(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	mk(Promotion{Code: "ten", Type: "percent", Value: 1000, Enabled: true})
	mk(Promotion{Code: "OFF", Type: "percent", Value: 1000})
	mk(Promotion{Code: "LATER", Type: "percent", Value: 1000, Enabled: true, StartsAt: &future})
	mk(Promotion{Code: "GONE", Type: "percent", Value: 1000, Enabled: true, ExpiresAt: &past})
	mk(Promotion{Code: "ONCE", Type: "fixed", Value: 5000, Enabled: true, MaxUses: 1})
	mk(Promotion{Code: "PROONLY", Type: "fixed", Value: 100, Enabled: true, Plans: []string{"pro"}})
	mk(Promotion{Code: "YEARLY", Type: "fixed", Value: 100, Enabled: true, Cycles: []string{"annually"}})
	if _, err := e.svc.CreatePromotion(ctx, Promotion{Code: "TEN", Type: "fixed", Value: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate code: %v", err)
	}
	for _, bad := range []Promotion{{Code: "x", Type: "fixed", Value: 1}, {Code: "AB", Type: "percent", Value: 10001},
		{Code: "AB", Type: "gift", Value: 1}, {Code: "AB", Type: "fixed", Value: 1, Plans: []string{"nope"}},
		{Code: "AB", Type: "fixed", Value: 1, StartsAt: &future, ExpiresAt: &past}} {
		if _, err := e.svc.CreatePromotion(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	for _, c := range []struct {
		code, plan, cycle string
		ok                bool
	}{
		{"TEN", "basic", "monthly", true}, {"ten", "pro", "annually", true}, {"NOPE", "basic", "monthly", false},
		{"OFF", "basic", "monthly", false}, {"LATER", "basic", "monthly", false}, {"GONE", "basic", "monthly", false},
		{"ONCE", "basic", "monthly", true}, {"PROONLY", "basic", "monthly", false}, {"PROONLY", "pro", "monthly", true},
		{"YEARLY", "basic", "monthly", false}, {"YEARLY", "basic", "annually", true},
	} {
		if _, err := e.svc.checkPromo(ctx, c.code, c.plan, c.cycle); (err == nil) != c.ok {
			t.Errorf("%s on %s %s: %v", c.code, c.plan, c.cycle, err)
		}
	}
	once, _ := e.store.PromotionByCode(ctx, "ONCE")
	if d := promoDiscount(once, 1000); d != 1000 {
		t.Fatalf("a fixed discount beyond the price: %d", d)
	}
	e.store.UsePromotion(ctx, once.ID)
	if _, err := e.svc.checkPromo(ctx, "ONCE", "basic", "monthly"); err == nil {
		t.Fatal("used-up code accepted")
	}
	ten, _ := e.store.PromotionByCode(ctx, "TEN")
	if d := promoDiscount(ten, 1005); d != 101 {
		t.Fatalf("percent discount %d", d)
	}
	if items := discountItem(ten, 100, Currency{Symbol: "$", Decimals: 2}); len(items) != 1 ||
		items[0].Description != "Promotion TEN — 10% off" || items[0].Amount != -100 {
		t.Fatalf("discount line %+v", items)
	}
	// Quotes report a bad code without failing.
	q, err := e.svc.Quote(ctx, QuoteInput{PlanID: "basic", Cycle: "annually", Promo: "GONE"})
	if err != nil || q.Promo == nil || q.Promo.Valid || q.Promo.Message != "that code has expired" || q.Total != 10500 {
		t.Fatalf("quote %+v %v", q, err)
	}
	q, _ = e.svc.Quote(ctx, QuoteInput{PlanID: "basic", Cycle: "annually", Promo: "ten"})
	if !q.Promo.Valid || q.Discount != 1000 || q.Total != 9500 || q.RecurringTotal != 10000 {
		t.Fatalf("quote with a first-invoice code %+v", q)
	}
	ten.Recurring = true
	e.store.UpdatePromotion(ctx, ten)
	if q, _ = e.svc.Quote(ctx, QuoteInput{PlanID: "basic", Cycle: "annually", Promo: "ten"}); q.RecurringTotal != 9000 {
		t.Fatalf("recurring quote %+v", q)
	}
	if _, err := e.svc.Quote(ctx, QuoteInput{PlanID: "private", Cycle: "monthly"}); !isField(err, "plan_id") {
		t.Fatalf("a private plan quoted: %v", err)
	}
	if _, err := e.svc.Quote(ctx, QuoteInput{PlanID: "pro", Cycle: "biennially"}); !isField(err, "cycle") {
		t.Fatalf("a cycle not sold: %v", err)
	}
}

func isField(err error, field string) bool {
	fe, ok := errors.AsType[*FieldError](err)
	return ok && fe.Field == field
}

func TestInvoiceNumbering(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	a := e.billed("Acme", "basic", "monthly", e.now.AddDate(0, 1, 0))
	i1 := e.manualInvoice(a.ID, 1000, false)
	d := e.manualInvoice(a.ID, 500, true)
	i2 := e.manualInvoice(a.ID, 700, false)
	if i1.Number != "INV-000001" || d.Number != "Draft #"+itoa(d.ID) || i2.Number != "INV-000002" || d.IssuedAt != nil {
		t.Fatalf("numbers %s %s %s", i1.Number, d.Number, i2.Number)
	}
	issued, err := e.svc.IssueInvoice(ctx, d.ID, true)
	if err != nil || issued.Number != "INV-000003" || issued.IssuedAt == nil || issued.Status != "unpaid" {
		t.Fatalf("issued %+v %v", issued, err)
	}
	if _, err := e.svc.IssueInvoice(ctx, d.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("issued twice: %v", err)
	}
	// A cancelled invoice keeps its number: no gaps.
	if c, err := e.svc.CancelInvoice(ctx, i2.ID, "admin"); err != nil || c.Number != "INV-000002" || c.Status != "cancelled" {
		t.Fatalf("cancel %+v %v", c, err)
	}
	// Numbered on payment: unpaid ones are proformas.
	e.setting(func(c *InvoicingSettings) { c.Invoice.NumberOn = "payment"; c.Invoice.Prefix = "2026/" })
	p1 := e.manualInvoice(a.ID, 1000, false)
	p2 := e.manualInvoice(a.ID, 1000, false)
	if p1.Number != "Proforma #"+itoa(p1.ID) {
		t.Fatalf("proforma %s", p1.Number)
	}
	pay := func(id int64) *InvoiceDetail {
		t.Helper()
		if _, err := e.svc.RecordPayment(ctx, id, PaymentInput{Gateway: "bank", Amount: 1000, By: "admin"}); err != nil {
			t.Fatal(err)
		}
		inv, _ := e.svc.Invoice(ctx, id)
		return inv
	}
	if got := pay(p2.ID); got.Number != "2026/000004" || got.Status != "paid" {
		t.Fatalf("numbered on payment %s", got.Number)
	}
	if got := pay(p1.ID); got.Number != "2026/000005" {
		t.Fatalf("second %s", got.Number)
	}
	// The next number moves forward only past numbers used.
	e.setting(func(c *InvoicingSettings) { c.Invoice.NextNumber = 1000 })
	cfg, _ := e.svc.Invoicing(ctx)
	cfg.Invoice.NextNumber = 5
	if _, err := e.svc.SetInvoicing(ctx, cfg.Redacted()); !errors.Is(err, ErrConflict) {
		t.Fatalf("a used number again: %v", err)
	}
	if got := pay(e.manualInvoice(a.ID, 1000, false).ID); got.Number != "2026/001000" {
		t.Fatalf("after moving %s", got.Number)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
