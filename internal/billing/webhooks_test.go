package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// receiver verifies deliveries the way the docs tell integrators to.
type receiver struct {
	mu     sync.Mutex
	secret string
	fail   int // answer 500 this many times first
	got    []map[string]any
	bad    int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var ts, sig string
	for _, p := range strings.Split(r.Header.Get(SignatureHeader), ",") {
		k, v, _ := strings.Cut(p, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	mac := hmac.New(sha256.New, []byte(rc.secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	got, _ := hex.DecodeString(sig)
	if !hmac.Equal(got, mac.Sum(nil)) {
		rc.bad++
		w.WriteHeader(401)
		return
	}
	if rc.fail > 0 {
		rc.fail--
		http.Error(w, "down for maintenance", 503)
		return
	}
	var ev map[string]any
	json.Unmarshal(body, &ev)
	rc.got = append(rc.got, ev)
}

func TestWebhookDeliveryRetriesAndSignature(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rc := &receiver{fail: 2}
	srv := httptest.NewTLSServer(rc)
	defer srv.Close()
	client := srv.Client()
	// Deliveries to the test server (loopback) need the private-address
	// check lifted, with the test server's certificate trusted.
	safe := SafeClient(true)
	safe.Transport.(*http.Transport).TLSClientConfig = client.Transport.(*http.Transport).TLSClientConfig
	hooks := &Webhooks{Store: e.store, Log: e.svc.Log, Client: safe, Now: func() time.Time { return e.now }}
	e.svc.Hooks = hooks

	ep := &store.WebhookEndpoint{URL: srv.URL + "/hook", Secret: NewSecret(), Enabled: true,
		Events: []string{EventAccountSuspended, EventAccountCreated}}
	if err := ValidateEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	rc.secret = ep.Secret
	if _, err := e.store.CreateWebhookEndpoint(ctx, ep); err != nil {
		t.Fatal(err)
	}
	a := e.account(AccountInput{Name: "A", PlanID: "small"})
	e.svc.Suspend(ctx, a.ID, ReasonAdmin)
	e.svc.Unsuspend(ctx, a.ID, ReasonAdmin) // not subscribed

	hooks.DeliverDue(ctx)
	ds, _ := e.store.WebhookDeliveries(ctx, 0, 10)
	if len(ds) != 2 {
		t.Fatalf("%d deliveries queued", len(ds))
	}
	for _, d := range ds {
		if d.Status != store.DeliveryPending || d.Attempts != 1 || d.LastStatus != 503 ||
			!strings.Contains(d.LastError, "maintenance") || !d.NextAttemptAt.Equal(e.now.Add(30*time.Second)) {
			t.Fatalf("after a failure: %+v", d)
		}
	}
	// Not due yet: nothing is sent.
	hooks.DeliverDue(ctx)
	if rc.fail != 0 {
		t.Fatalf("receiver saw %d attempts", 2-rc.fail)
	}
	e.now = e.now.Add(31 * time.Second)
	hooks.DeliverDue(ctx)
	if len(rc.got) != 2 || rc.bad != 0 {
		t.Fatalf("got %d, bad signatures %d", len(rc.got), rc.bad)
	}
	types := []string{rc.got[0]["type"].(string), rc.got[1]["type"].(string)}
	if types[0] != EventAccountCreated || types[1] != EventAccountSuspended {
		t.Fatalf("order %v", types)
	}
	if data := rc.got[1]["data"].(map[string]any); data["reason"] != "admin" || data["account_id"] != float64(a.ID) {
		t.Fatalf("payload %v", rc.got[1])
	}
	ds, _ = e.store.WebhookDeliveries(ctx, 0, 10)
	if ds[0].Status != store.DeliveryDelivered || ds[0].Attempts != 2 {
		t.Fatalf("%+v", ds[0])
	}

	// A test delivery, and giving up after the last attempt.
	hooks.Test(ctx, 1)
	rc.fail = 100
	for i := 0; i < webhookMaxAttempts; i++ {
		e.now = e.now.Add(webhookMaxRetry)
		hooks.DeliverDue(ctx)
	}
	ds, _ = e.store.WebhookDeliveries(ctx, 0, 1)
	if ds[0].Event != EventWebhookTest || ds[0].Status != store.DeliveryFailed || ds[0].Attempts != webhookMaxAttempts {
		t.Fatalf("%+v", ds[0])
	}
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if d := retryDelay(i + 1); d != w {
			t.Errorf("attempt %d: %s, want %s", i+1, d, w)
		}
	}
	if retryDelay(30) != webhookMaxRetry {
		t.Error("cap")
	}
}

func TestEndpointValidationAndSafeClient(t *testing.T) {
	for _, u := range []string{"http://hooks.example/x", "https://user:pw@hooks.example/", "ftp://x", "https://", "not a url"} {
		if err := ValidateEndpoint(&store.WebhookEndpoint{URL: u}); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if err := ValidateEndpoint(&store.WebhookEndpoint{URL: "https://hooks.example/x", Events: []string{"root.granted"}}); err == nil {
		t.Error("unknown event accepted")
	}
	// Loopback (Caddy's admin API, the panel itself) is never dialled.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the safe client reached a loopback server")
	}))
	defer srv.Close()
	if _, err := SafeClient(false).Get(srv.URL); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("loopback: %v", err)
	}
	for addr, public := range map[string]bool{"8.8.8.8": true, "2606:4700::1": true, "10.1.2.3": false,
		"192.168.1.1": false, "169.254.169.254": false, "100.64.0.1": false, "::1": false, "0.1.2.3": false,
		"::ffff:127.0.0.1": false, "fd00::1": false} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != public {
			t.Errorf("%s public=%v", addr, got)
		}
	}
	if s := Sign("k", 5, []byte("b")); !strings.HasPrefix(s, "t=5,v1=") || len(s) != len("t=5,v1=")+64 {
		t.Errorf("signature %s", s)
	}
}
