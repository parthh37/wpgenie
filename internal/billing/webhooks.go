package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Outgoing webhooks ("billing hooks"): events are queued in the store for
// every subscribed endpoint and delivered by Run, with retries and
// exponential backoff, so a billing system that is down for a day still
// gets them. Each delivery is signed like Stripe signs its own:
//
//	X-WPGenie-Signature: t=<unix time>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
//
// Receivers recompute the HMAC over the raw body, compare in constant time
// and reject timestamps more than a few minutes old (replays).

// Events.
const (
	EventAccountCreated        = "account.created"
	EventAccountSuspended      = "account.suspended"
	EventAccountUnsuspended    = "account.unsuspended"
	EventAccountTerminated     = "account.terminated"
	EventAccountPaymentFailed  = "account.payment_failed"
	EventPlanChanged           = "plan.changed"
	EventUsageThreshold        = "usage.threshold"
	EventSiteCreated           = "site.created"
	EventSiteDeleted           = "site.deleted"
	EventWebhookTest           = "webhook.test"
	SignatureHeader            = "X-WPGenie-Signature"
	webhookMaxAttempts         = 12
	webhookFirstRetry          = 30 * time.Second
	webhookMaxRetry            = 6 * time.Hour
	webhookBodyLimit           = 64 << 10
	webhookDeliveriesPerTick   = 20
	webhookResponseExcerptSize = 200
)

// Events lists the events endpoints can subscribe to.
var Events = []string{EventAccountCreated, EventAccountSuspended, EventAccountUnsuspended, EventAccountTerminated,
	EventAccountPaymentFailed, EventPlanChanged, EventUsageThreshold, EventSiteCreated, EventSiteDeleted}

type Webhooks struct {
	Store *store.Store
	Log   *slog.Logger
	// Client sends deliveries; nil: SafeClient(false), which refuses
	// private and loopback addresses.
	Client *http.Client
	Now    func() time.Time

	once sync.Once
	wake chan struct{} // see ch
}

// ch wakes the delivery loop early when something is queued.
func (w *Webhooks) ch() chan struct{} {
	w.once.Do(func() { w.wake = make(chan struct{}, 1) })
	return w.wake
}

func (w *Webhooks) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Webhooks) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return SafeClient(false)
}

// Sign returns the signature header value for a body sent at t.
func Sign(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", t)
	mac.Write(body)
	return "t=" + strconv.FormatInt(t, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// NewSecret makes an endpoint's signing secret.
func NewSecret() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "whsec_" + hex.EncodeToString(b)
}

// ValidateEndpoint checks an endpoint's URL (HTTPS, a host name, no
// credentials) and events.
func ValidateEndpoint(e *store.WebhookEndpoint) error {
	u, err := url.Parse(e.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(e.URL) > 500 {
		return fmt.Errorf("%w: the URL must be https://host/path, without credentials", ErrInvalid)
	}
	events := []string{}
	for _, ev := range e.Events {
		if !slices.Contains(Events, ev) {
			return fmt.Errorf("%w: unknown event %q (known: %s)", ErrInvalid, ev, strings.Join(Events, ", "))
		}
		if !slices.Contains(events, ev) {
			events = append(events, ev)
		}
	}
	slices.Sort(events)
	e.Events = events
	return nil
}

// Emit queues an event for every enabled endpoint subscribed to it. It
// never fails the operation that caused it: errors are logged.
func (w *Webhooks) Emit(ctx context.Context, event string, data map[string]any) {
	if w == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	eps, err := w.Store.WebhookEndpoints(ctx)
	if err != nil {
		w.Log.Error("webhooks: listing endpoints", "err", err)
		return
	}
	id := make([]byte, 12)
	rand.Read(id)
	now := w.now()
	body, err := json.Marshal(map[string]any{"id": "evt_" + hex.EncodeToString(id), "type": event,
		"created": now.Unix(), "data": data})
	if err != nil {
		w.Log.Error("webhooks: encoding event", "event", event, "err", err)
		return
	}
	queued := false
	for _, ep := range eps {
		if !ep.Enabled || (len(ep.Events) > 0 && !slices.Contains(ep.Events, event)) {
			continue
		}
		if err := w.Store.EnqueueDelivery(ctx, ep.ID, "evt_"+hex.EncodeToString(id), event, string(body), now); err != nil {
			w.Log.Error("webhooks: queueing a delivery", "endpoint", ep.ID, "event", event, "err", err)
			continue
		}
		queued = true
	}
	if queued {
		select {
		case w.ch() <- struct{}{}:
		default:
		}
	}
}

// Test queues a test event for one endpoint.
func (w *Webhooks) Test(ctx context.Context, endpointID int64) error {
	ep, err := w.Store.GetWebhookEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	id := make([]byte, 12)
	rand.Read(id)
	now := w.now()
	body, _ := json.Marshal(map[string]any{"id": "evt_" + hex.EncodeToString(id), "type": EventWebhookTest,
		"created": now.Unix(), "data": map[string]any{"endpoint_id": ep.ID}})
	return w.Store.EnqueueDelivery(ctx, ep.ID, "evt_"+hex.EncodeToString(id), EventWebhookTest, string(body), now)
}

// Run delivers queued events until ctx ends.
func (w *Webhooks) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		w.DeliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.ch():
		}
	}
}

// DeliverDue attempts the deliveries whose time has come.
func (w *Webhooks) DeliverDue(ctx context.Context) {
	due, err := w.Store.DueDeliveries(ctx, w.now(), webhookDeliveriesPerTick)
	if err != nil {
		w.Log.Error("webhooks: listing due deliveries", "err", err)
		return
	}
	eps := map[int64]*store.WebhookEndpoint{}
	for _, d := range due {
		ep, ok := eps[d.EndpointID]
		if !ok {
			if ep, err = w.Store.GetWebhookEndpoint(ctx, d.EndpointID); err != nil {
				continue // deleted meanwhile: its deliveries went with it
			}
			eps[d.EndpointID] = ep
		}
		w.deliver(ctx, ep, d)
	}
}

func (w *Webhooks) deliver(ctx context.Context, ep *store.WebhookEndpoint, d *store.WebhookDelivery) {
	d.Attempts++
	status, err := w.post(ctx, ep, d)
	now := w.now()
	d.LastStatus, d.LastError = status, ""
	switch {
	case err == nil:
		d.Status, d.DeliveredAt = store.DeliveryDelivered, now
	case !ep.Enabled:
		d.Status, d.LastError = store.DeliveryFailed, "endpoint disabled"
	default:
		d.LastError = err.Error()
		if d.Attempts >= webhookMaxAttempts {
			d.Status = store.DeliveryFailed
			w.Log.Warn("webhook delivery failed for good", "endpoint", ep.ID, "event", d.Event, "err", err)
		} else {
			d.NextAttemptAt = now.Add(retryDelay(d.Attempts))
		}
	}
	if err := w.Store.RecordDelivery(context.WithoutCancel(ctx), d); err != nil {
		w.Log.Error("webhooks: recording a delivery", "delivery", d.ID, "err", err)
	}
}

// retryDelay is 30 s after the first failure, doubling up to 6 hours: 12
// attempts span about a day and a half.
func retryDelay(attempts int) time.Duration {
	d := webhookFirstRetry
	for i := 1; i < attempts && d < webhookMaxRetry; i++ {
		d *= 2
	}
	return min(d, webhookMaxRetry)
}

func (w *Webhooks) post(ctx context.Context, ep *store.WebhookEndpoint, d *store.WebhookDelivery) (int, error) {
	if !ep.Enabled {
		return 0, fmt.Errorf("endpoint disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	body := []byte(d.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "WPGenie-Webhooks/1")
	req.Header.Set("X-WPGenie-Event", d.Event)
	req.Header.Set("X-WPGenie-Delivery", strconv.FormatInt(d.ID, 10))
	req.Header.Set(SignatureHeader, Sign(ep.Secret, w.now().Unix(), body))
	resp, err := w.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, webhookResponseExcerptSize))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}
	return resp.StatusCode, nil
}

// SafeClient is an HTTP client for URLs an administrator typed: it
// connects only to public addresses (checked on the address actually
// dialled, after DNS, so a name can't be rebound to 127.0.0.1 and reach
// Caddy's admin API or the database), ignores proxy settings and doesn't
// follow redirects. allowPrivate lifts the address check (tests).
func SafeClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !PublicAddr(ip) {
			return fmt.Errorf("refusing to connect to %s: not a public address", host)
		}
		return nil
	}}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// PublicAddr reports whether an address is on the public internet.
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!cgnat.Contains(ip) && !(ip.Is4() && ip.As4()[0] == 0)
}
