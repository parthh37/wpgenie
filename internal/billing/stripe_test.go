package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

func stripeSign(secret string, t int64, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", t, payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyStripeSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"evt_1","type":"invoice.paid"}`)
	good := stripeSign("whsec_a", now.Unix(), string(body))
	other := stripeSign("whsec_old", now.Unix(), string(body))
	cases := []struct {
		header string
		ok     bool
	}{
		{fmt.Sprintf("t=%d,v1=%s", now.Unix(), good), true},
		// During a secret roll Stripe sends several v1 signatures.
		{fmt.Sprintf("t=%d,v1=%s,v1=%s,v0=abc", now.Unix(), other, good), true},
		{fmt.Sprintf("t=%d,v1=%s", now.Unix(), other), false},
		{fmt.Sprintf("t=%d,v0=%s", now.Unix(), good), false}, // only v1 counts
		{fmt.Sprintf("t=%d,v1=%s", now.Unix()-301, stripeSign("whsec_a", now.Unix()-301, string(body))), false},
		{fmt.Sprintf("t=%d,v1=%s", now.Unix()+301, stripeSign("whsec_a", now.Unix()+301, string(body))), false},
		{fmt.Sprintf("t=%d,v1=%s", now.Unix()-299, stripeSign("whsec_a", now.Unix()-299, string(body))), true},
		{"v1=" + good, false},
		{"", false},
		{fmt.Sprintf("t=%d,v1=zz", now.Unix()), false},
	}
	for i, c := range cases {
		err := VerifyStripeSignature(body, c.header, "whsec_a", now)
		if (err == nil) != c.ok {
			t.Errorf("case %d (%s): %v", i, c.header, err)
		}
	}
	// A tampered body fails.
	if err := VerifyStripeSignature([]byte(`{"id":"evt_2"}`), fmt.Sprintf("t=%d,v1=%s", now.Unix(), good), "whsec_a", now); err == nil {
		t.Error("tampered body accepted")
	}
	if err := VerifyStripeSignature(body, fmt.Sprintf("t=%d,v1=%s", now.Unix(), good), "", now); !errors.Is(err, ErrStripeOff) {
		t.Error("no secret configured")
	}
}

// fakeStripe is Stripe's API: subscriptions and meter events.
type fakeStripe struct {
	mu     sync.Mutex
	subs   map[string]string // id -> JSON
	meters []url.Values
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk_test_1" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
		if s, ok := f.subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]; ok {
			io.WriteString(w, s)
			return
		}
		w.WriteHeader(404)
	case r.Method == "POST" && r.URL.Path == "/v1/billing/meter_events":
		r.ParseForm()
		f.meters = append(f.meters, r.PostForm)
		io.WriteString(w, `{}`)
	default:
		w.WriteHeader(404)
	}
}

func (e *env) stripe(t *testing.T, id, typ, object string) error {
	t.Helper()
	return e.stripeAt(t, id, typ, e.now.Unix(), object)
}

// stripeAt sends an event created at a given time (Stripe's "created").
func (e *env) stripeAt(t *testing.T, id, typ string, created int64, object string) error {
	t.Helper()
	payload := fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":%s}}`, id, typ, created, object)
	sig := fmt.Sprintf("t=%d,v1=%s", e.now.Unix(), stripeSign("whsec_test", e.now.Unix(), payload))
	return e.svc.HandleStripeWebhook(context.Background(), []byte(payload), sig)
}

func TestStripeLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	api := &fakeStripe{subs: map[string]string{
		"sub_1": `{"id":"sub_1","customer":"cus_1","status":"active","items":{"data":[{"price":{"id":"price_big"}}]}}`}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	e.svc.Stripe = &StripeAPI{Base: srv.URL}

	payload := `{"id":"evt_x","type":"invoice.paid","data":{"object":{}}}`
	if err := e.svc.HandleStripeWebhook(ctx, []byte(payload), "t=1,v1=00"); !errors.Is(err, ErrStripeOff) {
		t.Fatalf("unconfigured: %v", err)
	}
	secret, key, meter := "whsec_test", "sk_test_1", "wpgenie_bandwidth_mb"
	if err := e.svc.SetStripeSettings(ctx, StripeSettingsInput{WebhookSecret: &secret, SecretKey: &key, MeterEvent: &meter,
		Prices: map[string]string{"price_small": "small", "price_big": "big"}}); err != nil {
		t.Fatal(err)
	}
	bad := "sk_live_x\n"
	if err := e.svc.SetStripeSettings(ctx, StripeSettingsInput{SecretKey: &bad}); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad key accepted")
	}
	if err := e.svc.SetStripeSettings(ctx, StripeSettingsInput{Prices: map[string]string{"price_x": "nope"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("price mapped to a missing plan")
	}
	// A forged event does nothing.
	if err := e.svc.HandleStripeWebhook(ctx, []byte(payload), fmt.Sprintf("t=%d,v1=%s", e.now.Unix(),
		stripeSign("whsec_wrong", e.now.Unix(), payload))); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged: %v", err)
	}

	// Checkout: with the API key, the subscription tells the plan.
	checkout := `{"mode":"subscription","customer":"cus_1","subscription":"sub_1",
		"customer_details":{"email":"jo@example.com","name":"Jo Shop"}}`
	if err := e.stripe(t, "evt_1", "checkout.session.completed", checkout); err != nil {
		t.Fatal(err)
	}
	a, err := e.store.AccountByStripeCustomer(ctx, "cus_1")
	if err != nil || a.PlanID != "big" || a.Name != "Jo Shop" || a.Email != "jo@example.com" || a.StripeSubscriptionID != "sub_1" {
		t.Fatalf("%+v %v", a, err)
	}
	// Replayed: processed once.
	if err := e.stripe(t, "evt_1", "checkout.session.completed", checkout); err != nil {
		t.Fatal(err)
	}
	if all, _ := e.store.ListAccounts(ctx, store.AccountFilter{}); len(all) != 1 {
		t.Fatalf("%d accounts", len(all))
	}
	// A downgrade changes the plan.
	e.stripe(t, "evt_2", "customer.subscription.updated",
		`{"id":"sub_1","customer":"cus_1","status":"active","items":{"data":[{"price":{"id":"price_small"}}]}}`)
	if a, _ = e.store.GetAccount(ctx, a.ID); a.PlanID != "small" {
		t.Fatalf("plan %s", a.PlanID)
	}
	// Stripe's current state of the subscription (the API key is set, so
	// events are checked against it).
	setSub := func(status string) {
		api.mu.Lock()
		defer api.mu.Unlock()
		api.subs["sub_1"] = fmt.Sprintf(`{"id":"sub_1","customer":"cus_1","status":%q,"items":{"data":[{"price":{"id":"price_small"}}]}}`, status)
	}
	suspended := func(want bool, what string) {
		t.Helper()
		st, reason := e.status(a.ID)
		if (st == store.AccountSuspended) != want || e.sites.suspended["s1"] != want || (want && reason != ReasonBilling) {
			t.Fatalf("%s: %s %s, site suspended %v", what, st, reason, e.sites.suspended["s1"])
		}
	}
	// Failed payment: notified only; unpaid: suspended; paid: back.
	e.store.AssignSite(ctx, "s1", a.ID)
	e.stripe(t, "evt_3", "invoice.payment_failed", `{"customer":"cus_1","subscription":"sub_1","amount_due":900,"currency":"eur"}`)
	suspended(false, "first failed payment")
	setSub("unpaid")
	e.stripe(t, "evt_4", "customer.subscription.updated", `{"id":"sub_1","customer":"cus_1","status":"unpaid","items":{"data":[]}}`)
	suspended(true, "unpaid")
	// Nothing else of the customer's lifts it: an older event delivered
	// late, another subscription (a different product), another
	// subscription's invoice, a one-off invoice.
	e.stripeAt(t, "evt_4b", "customer.subscription.updated", e.now.Unix()-60,
		`{"id":"sub_1","customer":"cus_1","status":"active","items":{"data":[{"price":{"id":"price_small"}}]}}`)
	e.stripe(t, "evt_4c", "customer.subscription.created",
		`{"id":"sub_2","customer":"cus_1","status":"active","items":{"data":[{"price":{"id":"price_other"}}]}}`)
	e.stripe(t, "evt_4d", "invoice.paid", `{"customer":"cus_1","subscription":"sub_2"}`)
	e.stripe(t, "evt_4e", "invoice.paid", `{"customer":"cus_1"}`)
	suspended(true, "unrelated events")
	if a, _ := e.store.GetAccount(ctx, a.ID); a.StripeSubscriptionID != "sub_1" {
		t.Fatalf("hosting subscription replaced by %s", a.StripeSubscriptionID)
	}
	// Paid, but Stripe says the subscription is still unpaid: stays.
	e.now = e.now.Add(time.Second)
	e.stripe(t, "evt_5a", "invoice.paid", `{"customer":"cus_1","subscription":"sub_1"}`)
	suspended(true, "paid while Stripe says unpaid")
	setSub("active")
	e.stripe(t, "evt_5", "invoice.paid", `{"customer":"cus_1","subscription":"sub_1"}`)
	suspended(false, "paid")
	// An administrator's suspension isn't lifted by a payment.
	e.svc.Suspend(ctx, a.ID, ReasonAdmin)
	e.stripe(t, "evt_6", "invoice.paid", `{"customer":"cus_1","subscription":"sub_1"}`)
	if st, _ := e.status(a.ID); st != store.AccountSuspended {
		t.Fatal("payment lifted an admin suspension")
	}
	e.svc.Unsuspend(ctx, a.ID, ReasonAdmin)
	e.stripe(t, "evt_7a", "customer.subscription.deleted", `{"id":"sub_2","customer":"cus_1","status":"canceled"}`)
	suspended(false, "another subscription deleted")
	e.stripe(t, "evt_7", "customer.subscription.deleted", `{"id":"sub_1","customer":"cus_1","status":"canceled"}`)
	suspended(true, "deleted")
	// An unmapped price creates nothing; events for unknown customers are fine.
	if err := e.stripe(t, "evt_8", "customer.subscription.created",
		`{"id":"sub_9","customer":"cus_9","status":"active","items":{"data":[{"price":{"id":"price_other"}}]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AccountByStripeCustomer(ctx, "cus_9"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("account for an unmapped price")
	}
	if err := e.stripe(t, "evt_9", "invoice.paid", `{"customer":"cus_404","subscription":"sub_404"}`); err != nil {
		t.Fatal(err)
	}

	// Metered bandwidth: whole MB since the last report, once.
	e.svc.Unsuspend(ctx, a.ID, ReasonAdmin)
	traffic(t, e.store, "s1", e.now, 5*mb+100)
	if err := e.svc.EvaluateUsage(ctx); err != nil {
		t.Fatal(err)
	}
	e.svc.EvaluateUsage(ctx)
	traffic(t, e.store, "s1", e.now, 2*mb)
	e.svc.EvaluateUsage(ctx)
	if len(api.meters) != 2 || api.meters[0].Get("payload[value]") != "5" || api.meters[1].Get("payload[value]") != "2" ||
		api.meters[0].Get("payload[stripe_customer_id]") != "cus_1" || api.meters[0].Get("event_name") != meter ||
		api.meters[0].Get("identifier") == api.meters[1].Get("identifier") {
		t.Fatalf("meter events %v", api.meters)
	}
}
