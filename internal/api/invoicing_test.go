package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

// invoicingEnv is the tenancy panel with built-in billing on: A, B and R
// billed by invoice (manual payments), C (R's customer) not.
type invoicingEnv struct {
	*tenancyEnv
	inv map[string]int64 // "A", "B", "R", "A draft" -> invoice
}

func newInvoicingEnv(t *testing.T) *invoicingEnv {
	t.Helper()
	e := &invoicingEnv{tenancyEnv: newTenancyEnv(t), inv: map[string]int64{}}
	ctx := context.Background()
	b := e.api.Billing
	b.PanelURL = "https://panel.test"
	b.Mailer = &mailer.Service{Store: e.st, Log: slog.New(slog.DiscardHandler), PanelURL: "https://panel.test"}
	for _, id := range []string{"basic", "full"} {
		p, _ := e.st.GetPlan(ctx, id)
		p.Public, p.Prices = true, map[string]store.PlanPrice{"monthly": {Price: 1000}, "annually": {Price: 10000}}
		if err := e.st.UpdatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	cfg := billing.DefaultInvoicing()
	cfg.Enabled = true
	cfg.Methods.Manual.Instructions = "Bank: XX"
	cfg.BurstPacks = []billing.BurstPack{{ID: "b500", Minutes: 500, Price: 500}}
	if _, err := b.SetInvoicing(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mode, cycle := billing.ModeInvoice, "monthly"
	for _, name := range []string{"A", "B", "R"} {
		if _, err := b.UpdateProfile(ctx, e.acct[name].ID, billing.ProfileInput{Mode: &mode, Cycle: &cycle}); err != nil {
			t.Fatal(err)
		}
		inv, err := b.CreateInvoice(ctx, billing.ManualInvoiceInput{AccountID: e.acct[name].ID,
			Items: []billing.ItemInput{{Description: "Work for " + name, Quantity: 1, UnitPrice: 2000}}})
		if err != nil {
			t.Fatal(err)
		}
		e.inv[name] = inv.ID
	}
	d, err := b.CreateInvoice(ctx, billing.ManualInvoiceInput{AccountID: e.acct["A"].ID, Draft: true,
		Items: []billing.ItemInput{{Description: "Draft", Quantity: 1, UnitPrice: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	e.inv["A draft"] = d.ID
	return e
}

func (e *invoicingEnv) id(name string) string { return strconv.FormatInt(e.inv[name], 10) }

func (e *invoicingEnv) acctPath(name, rest string) string {
	return fmt.Sprintf("/api/v1/accounts/%d%s", e.acct[name].ID, rest)
}

// Every invoice route: someone else's invoice (and one's own draft) is
// 404 to a tenant; staff-only routes are 403 even on their own invoice.
func TestInvoicingRoutesAreScoped(t *testing.T) {
	e := newInvoicingEnv(t)
	n := 0
	for _, rt := range e.api.routes {
		if !strings.Contains(rt.Pattern, "/api/v1/invoices/{id}") {
			continue
		}
		n++
		method, path, _ := strings.Cut(rt.Pattern, " ")
		_, open := tenantRoutes[rt.Pattern]
		for _, c := range []struct{ who, inv string }{{"alice", "B"}, {"alice", "A draft"}, {"alice", "R"}, {"rita", "C"},
			{"rita", "A"}, {"session:carl", "R"}} {
			want := http.StatusNotFound
			if !open {
				want = http.StatusForbidden
			}
			p := strings.Replace(path, "{id}", e.id(c.inv), 1)
			if got := e.as(c.who, method, p, "{}", nil); got != want {
				t.Errorf("%s %s %s (%s's): %d, want %d", c.who, method, p, c.inv, got, want)
			}
		}
		if !open {
			if got := e.as("alice", method, strings.Replace(path, "{id}", e.id("A"), 1), "{}", nil); got != http.StatusForbidden {
				t.Errorf("alice: staff route %s on her invoice: %d", rt.Pattern, got)
			}
		}
	}
	if n != 10 {
		t.Fatalf("only %d invoice routes", n)
	}
	// Their own: yes.
	var inv billing.InvoiceDetail
	if c := e.as("alice", "GET", "/api/v1/invoices/"+e.id("A"), "", &inv); c != 200 || inv.AccountID != e.acct["A"].ID ||
		len(inv.Items) != 1 || inv.PayMethods[0] != "manual" {
		t.Fatalf("own invoice: %d %+v", c, inv)
	}
	var next billing.PayNext
	if c := e.as("alice", "POST", "/api/v1/invoices/"+e.id("A")+"/pay", `{"method":"manual"}`, &next); c != 200 ||
		next.Instructions != "Bank: XX" {
		t.Fatalf("pay: %d %+v", c, next)
	}
	// Lists are theirs only, whatever they ask for.
	var list []billing.InvoiceView
	e.as("alice", "GET", "/api/v1/invoices?account="+strconv.FormatInt(e.acct["B"].ID, 10), "", &list)
	if len(list) != 1 || list[0].ID != e.inv["A"] {
		t.Fatalf("alice's invoices %+v", list)
	}
	e.as("rita", "GET", "/api/v1/invoices", "", &list)
	if len(list) != 1 || list[0].ID != e.inv["R"] {
		t.Fatalf("the reseller sees its own invoices only: %+v", list)
	}
	e.as("tok", "GET", "/api/v1/invoices", "", &list)
	if len(list) != 4 {
		t.Fatalf("staff see %d", len(list))
	}
	e.as("tok", "POST", "/api/v1/invoices/"+e.id("B")+"/payments", `{"amount":2000,"method":"bank","reference":"T1"}`, nil)
	var tx []store.Payment
	e.as("alice", "GET", "/api/v1/transactions", "", &tx)
	if len(tx) != 0 {
		t.Fatalf("alice sees B's payment: %+v", tx)
	}
	e.as("bob", "GET", "/api/v1/transactions", "", &tx)
	if len(tx) != 1 {
		t.Fatalf("bob's payments %+v", tx)
	}
	for _, p := range []string{"/api/v1/invoices.csv", "/api/v1/transactions.csv", "/api/v1/billing/overview",
		"/api/v1/billing/invoicing", "/api/v1/orders", "/api/v1/billing/promotions", "/api/v1/billing/automation"} {
		if c := e.as("rita", "GET", p, "", nil); c != 403 {
			t.Errorf("rita GET %s: %d", p, c)
		}
	}
	var cfg billing.ClientConfig
	if c := e.as("carl", "GET", "/api/v1/billing/config", "", &cfg); c != 200 || !cfg.Enabled || len(cfg.BurstPacks) != 1 {
		t.Fatalf("config %d %+v", c, cfg)
	}
}

func TestBillingProfileRoutesAreScoped(t *testing.T) {
	e := newInvoicingEnv(t)
	var prof billing.ProfileView
	// Reading: their own; a reseller also its customers'.
	for _, c := range []struct {
		who, acct string
		want      int
	}{{"alice", "A", 200}, {"alice", "B", 404}, {"rita", "C", 200}, {"carl", "C", 200}, {"carl", "R", 404}} {
		if got := e.as(c.who, "GET", e.acctPath(c.acct, "/billing"), "", &prof); got != c.want {
			t.Errorf("%s reads %s's billing: %d", c.who, c.acct, got)
		}
		if got := e.as(c.who, "GET", e.acctPath(c.acct, "/credit"), "", nil); got != c.want {
			t.Errorf("%s reads %s's credit: %d", c.who, c.acct, got)
		}
	}
	// Changing: their own account only; never a customer's, even for its
	// reseller (WPGenie doesn't bill them).
	for _, c := range []struct{ method, rest, body string }{
		{"PUT", "/billing/contact", `{"email":"x@y.test"}`},
		{"PUT", "/billing/auto-pay", `{"auto_pay":false}`},
		{"DELETE", "/billing/card", ""},
		{"POST", "/plan-change/quote", `{"plan_id":"full"}`},
		{"POST", "/plan-change", `{"plan_id":"full"}`},
		{"POST", "/cancel", `{"when":"end_of_period"}`},
		{"DELETE", "/cancel", ""},
		{"POST", "/burst/buy", `{"pack":"b500","method":"manual"}`},
	} {
		if got := e.as("rita", c.method, e.acctPath("C", c.rest), c.body, nil); got != 404 {
			t.Errorf("rita %s %s on her customer: %d", c.method, c.rest, got)
		}
		if got := e.as("alice", c.method, e.acctPath("B", c.rest), c.body, nil); got != 404 {
			t.Errorf("alice %s %s on B: %d", c.method, c.rest, got)
		}
	}
	for _, c := range []struct{ method, rest, body string }{
		{"PUT", "/billing", `{"mode":"none"}`},
		{"POST", "/credit", `{"amount":100}`},
	} {
		if got := e.as("alice", c.method, e.acctPath("A", c.rest), c.body, nil); got != 403 {
			t.Errorf("alice %s %s: %d", c.method, c.rest, got)
		}
	}
	var out map[string]string
	if c := e.as("alice", "PUT", e.acctPath("A", "/billing/contact"), `{"email":"nope"}`, &out); c != 400 || out["field"] != "contact.email" {
		t.Fatalf("field error: %d %v", c, out)
	}
	if c := e.as("alice", "PUT", e.acctPath("A", "/billing/contact"), `{"email":"jo@a.test","country":"fr"}`, &prof); c != 200 ||
		prof.Contact.Country != "FR" {
		t.Fatalf("contact: %d %+v", c, prof)
	}
	if c := e.as("alice", "POST", e.acctPath("A", "/cancel"), `{"when":"immediately"}`, nil); c != 403 {
		t.Fatalf("a client cancelled immediately: %d", c)
	}
	if c := e.as("alice", "POST", e.acctPath("A", "/cancel"), `{"when":"end_of_period","reason":"moving"}`, &prof); c != 200 ||
		prof.CancelAt == nil || prof.CancelReason != "moving" {
		t.Fatalf("cancel: %d %+v", c, prof)
	}
	if c := e.as("alice", "DELETE", e.acctPath("A", "/cancel"), "", &prof); c != 200 || prof.CancelAt != nil {
		t.Fatalf("withdraw: %d %+v", c, prof)
	}
	var q billing.PlanChangeQuote
	if c := e.as("alice", "POST", e.acctPath("A", "/plan-change/quote"), `{"plan_id":"full"}`, &q); c != 200 || q.Charge == 0 {
		t.Fatalf("quote: %d %+v", c, q)
	}
	// Burst packs: their own account; a reseller's customer is refused.
	var buy billing.BurstPurchase
	if c := e.as("alice", "POST", e.acctPath("A", "/burst/buy"), `{"pack":"b500","method":"manual"}`, &buy); c != 201 ||
		buy.Invoice == nil || buy.Invoice.Kind != "burst_topup" || buy.Next.Instructions == "" {
		t.Fatalf("burst: %d %+v", c, buy)
	}
	if c := e.as("carl", "POST", e.acctPath("C", "/burst/buy"), `{"pack":"b500","method":"manual"}`, nil); c != 400 {
		t.Fatalf("a reseller's customer bought burst minutes: %d", c)
	}
	// Staff do all of it.
	if c := e.as("tok", "POST", e.acctPath("B", "/credit"), `{"amount":500,"description":"Goodwill"}`, nil); c != 201 {
		t.Fatalf("staff credit: %d", c)
	}
	if c := e.as("tok", "PUT", e.acctPath("C", "/billing"), `{"mode":"invoice"}`, nil); c != 400 {
		t.Fatalf("a reseller's customer billed by invoice: %d", c)
	}
}

// A pending (or suspended) account pays, fixes its billing details and
// reads, but changes nothing else.
func TestPendingAccountsCanPay(t *testing.T) {
	e := newInvoicingEnv(t)
	e.st.SetAccountStatus(context.Background(), e.acct["A"].ID, store.AccountPending, "", e.now)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/v1/invoices/" + e.id("A") + "/pay", `{"method":"manual"}`, 200},
		{"POST", "/api/v1/invoices/" + e.id("A") + "/apply-credit", `{}`, 200},
		{"PUT", e.acctPath("A", "/billing/contact"), `{"email":"a@a.test"}`, 200},
		{"POST", e.acctPath("A", "/plan-change/quote"), `{"plan_id":"full"}`, 409}, // allowed, but not while pending
		{"POST", e.acctPath("A", "/plan-change"), `{"plan_id":"full"}`, 403},
		{"POST", e.acctPath("A", "/burst/buy"), `{"pack":"b500","method":"manual"}`, 201},
		{"POST", "/api/v1/sites", `{"domain":"new.test","admin_email":"a@b.co"}`, 403},
		{"GET", "/api/v1/invoices", "", 200},
	} {
		if got := e.as("alice", c.method, c.path, c.body, nil); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

func (e *invoicingEnv) public(method, path, body string, csrf bool, hdr map[string]string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if csrf {
		req.Header.Set(csrfHeader, csrfValue)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestOrderForm(t *testing.T) {
	e := newInvoicingEnv(t)
	resp, body := e.public("GET", "/api/v1/store/catalog", "", false, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"id":"basic"`) {
		t.Fatalf("catalog: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.public("POST", "/api/v1/store/quote", `{"plan_id":"basic","cycle":"monthly"}`, false, nil); resp.StatusCode != 403 {
		t.Fatalf("quote without the CSRF header: %d", resp.StatusCode)
	}
	if resp, body := e.public("POST", "/api/v1/store/quote", `{"plan_id":"basic","cycle":"monthly"}`, true, nil); resp.StatusCode != 200 ||
		!strings.Contains(body, `"total":1000`) {
		t.Fatalf("quote: %d %s", resp.StatusCode, body)
	}
	order := func(user, extra string) (*http.Response, string) {
		return e.public("POST", "/api/v1/store/orders", fmt.Sprintf(`{"plan_id":"basic","cycle":"monthly","method":"manual",
			"contact":{"first_name":"Jo","email":"jo@shop.test"},"user":{"username":%q,"password":"a long password"}%s}`, user, extra),
			true, nil)
	}
	for _, c := range []struct{ user, extra, field string }{
		{"x", "", "user.username"},
		{"alice", "", "user.username"}, // taken
		{"newbie", `,"promo":"NOPE"`, "promo"},
	} {
		resp, body := order(c.user, c.extra)
		if resp.StatusCode != 400 || !strings.Contains(body, `"field":"`+c.field+`"`) {
			t.Errorf("%s %s: %d %s", c.user, c.extra, resp.StatusCode, body)
		}
	}
	if resp, _ := order("spammer", `,"website":"http://spam.test"`); resp.StatusCode != 400 {
		t.Fatalf("honeypot: %d", resp.StatusCode)
	}
	resp, body = order("newbie", "")
	if resp.StatusCode != 201 || !strings.Contains(body, `"instructions":"Bank: XX"`) {
		t.Fatalf("order: %d %s", resp.StatusCode, body)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatal("not signed in")
	}
	u, _ := e.st.UserByName(context.Background(), "newbie")
	e.sessionFor["newbie"] = cookie.Value
	var me struct {
		Account accountView `json:"account"`
	}
	if c := e.as("session:newbie", "GET", "/api/v1/account", "", &me); c != 200 || me.Account.Status != "pending" ||
		me.Account.ID != u.AccountID {
		t.Fatalf("the new client: %d %+v", c, me)
	}
	var list []billing.InvoiceView
	if c := e.as("session:newbie", "GET", "/api/v1/invoices", "", &list); c != 200 || len(list) != 1 || list[0].Kind != "order" {
		t.Fatalf("their invoice: %d %+v", c, list)
	}
	// Five orders an hour from one address; refused ones don't count.
	for i := 2; i <= 5; i++ {
		if resp, body := order(fmt.Sprintf("buyer%d", i), ""); resp.StatusCode != 201 {
			t.Fatalf("order %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if resp, _ := order("buyer6", ""); resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("sixth order: %d", resp.StatusCode)
	}
	e.now = e.now.Add(time.Hour + time.Minute)
	if resp, _ := order("buyer7", ""); resp.StatusCode != 201 {
		t.Fatalf("an hour later: %d", resp.StatusCode)
	}
	// Staff see the orders, with the address.
	var orders []billing.OrderView
	if c := e.as("tok", "GET", "/api/v1/orders?status=pending", "", &orders); c != 200 || len(orders) != 6 ||
		orders[0].IP == "" || orders[0].InvoiceStatus != "unpaid" {
		t.Fatalf("orders: %d %+v", c, orders)
	}
	if c := e.as("tok", "POST", fmt.Sprintf("/api/v1/orders/%d/accept", orders[0].ID), "", nil); c != 204 {
		t.Fatalf("accept: %d", c)
	}
	if c := e.as("tok", "POST", fmt.Sprintf("/api/v1/orders/%d/cancel", orders[1].ID), "", nil); c != 204 {
		t.Fatalf("cancel: %d", c)
	}
}

func TestInvoicePrintAndExports(t *testing.T) {
	e := newInvoicingEnv(t)
	ctx := context.Background()
	e.st.UpdateAccount(ctx, &store.Account{ID: e.acct["A"].ID, Name: "<b>Evil</b>", Kind: "customer", PlanID: "basic"})
	inv, err := e.api.Billing.CreateInvoice(ctx, billing.ManualInvoiceInput{AccountID: e.acct["A"].ID,
		Items: []billing.ItemInput{{Description: "=HYPERLINK(\"x\")", Quantity: 2, UnitPrice: 1234}}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/invoices/%d/print", e.srv.URL, inv.ID), nil)
	req.Header.Set("Authorization", "Bearer "+e.tokens["alice"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("print: %d %v", resp.StatusCode, resp.Header)
	}
	for _, want := range []string{`href="/invoice.css"`, `class="invoice status-unpaid"`, `class="items"`, `class="totals"`,
		`class="bill-to"`, `class="print-btn"`, "&lt;b&gt;Evil&lt;/b&gt;", "$24.68", inv.Number, "Bank: XX"} {
		if !strings.Contains(page, want) {
			t.Errorf("print page lacks %q", want)
		}
	}
	if strings.Contains(page, "<b>Evil</b>") || strings.Contains(page, "style=") || strings.Contains(page, "<script>") {
		t.Fatal("print page: unescaped or inline content")
	}
	e.as("tok", "POST", fmt.Sprintf("/api/v1/invoices/%d/payments", inv.ID), `{"amount":2468,"method":"cash"}`, nil)
	req, _ = http.NewRequest("GET", fmt.Sprintf("%s/api/v1/invoices/%d/print", e.srv.URL, inv.ID), nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, _ = http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `class="stamp paid"`) {
		t.Fatal("no PAID stamp")
	}
	req, _ = http.NewRequest("GET", e.srv.URL+"/api/v1/invoices.csv", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, _ = http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(b)
	if !strings.HasPrefix(out, "number,account_id,client") || !strings.Contains(out, "<b>Evil</b>") ||
		!strings.Contains(out, "24.68") || !strings.Contains(resp.Header.Get("Content-Disposition"), "invoices.csv") {
		t.Fatalf("csv: %s", out)
	}
	// A client's name can't make a spreadsheet run a formula.
	for in, want := range map[string]string{"=SUM(A1)": "'=SUM(A1)", "+1": "'+1", "@x": "'@x", "Acme": "Acme", "": ""} {
		if got := csvText(in); got != want {
			t.Errorf("csvText(%q) = %q", in, got)
		}
	}
}

func TestRazorpayEndpoints(t *testing.T) {
	e := newInvoicingEnv(t)
	body := `{"event":"payment_link.paid","payload":{}}`
	sign := func(secret, msg string) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(msg))
		return hex.EncodeToString(m.Sum(nil))
	}
	if resp, _ := e.public("POST", "/api/v1/billing/razorpay/webhook", body, false, nil); resp.StatusCode != 404 {
		t.Fatalf("unconfigured webhook: %d", resp.StatusCode)
	}
	cfg, _ := e.api.Billing.Invoicing(context.Background())
	ks, ws := "rzsecret", "rzwebhook"
	cfg.Methods.Razorpay = billing.RazorpayMethod{Enabled: true, Name: "UPI", KeyID: "rzp_test_abcdef12", KeySecret: &ks, WebhookSecret: &ws}
	if _, err := e.api.Billing.SetInvoicing(context.Background(), *cfg); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.public("POST", "/api/v1/billing/razorpay/webhook", body, false,
		map[string]string{"X-Razorpay-Signature": sign("wrong", body)}); resp.StatusCode != 400 {
		t.Fatalf("forged webhook: %d", resp.StatusCode)
	}
	if resp, _ := e.public("POST", "/api/v1/billing/razorpay/webhook", body, false,
		map[string]string{"X-Razorpay-Signature": sign("rzwebhook", body)}); resp.StatusCode != 200 {
		t.Fatalf("signed webhook: %d", resp.StatusCode)
	}
	ref := "wpg_" + e.id("A") + "_1"
	q := url.Values{"razorpay_payment_id": {""}, "razorpay_payment_link_id": {"plink_X"},
		"razorpay_payment_link_reference_id": {ref}, "razorpay_payment_link_status": {"cancelled"}}
	q.Set("razorpay_signature", sign("wrong", "x"))
	if resp, _ := e.public("GET", "/api/v1/billing/razorpay/callback?"+q.Encode(), "", false, nil); resp.StatusCode != 400 {
		t.Fatalf("forged callback: %d", resp.StatusCode)
	}
	q.Set("razorpay_signature", sign("rzsecret", "plink_X|"+ref+"|cancelled|"))
	resp, _ := e.public("GET", "/api/v1/billing/razorpay/callback?"+q.Encode(), "", false, nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "https://panel.test/#/billing/invoices/"+e.id("A") {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}
