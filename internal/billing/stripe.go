package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Stripe: subscriptions become accounts. Stripe calls the panel's public
// webhook endpoint; every call is authenticated by its Stripe-Signature
// (HMAC-SHA256 with the endpoint's signing secret, 5-minute tolerance) and
// processed once per event ID. Prices map to plans (admin-configured).
// With a secret key and a meter event name, each account's monthly
// bandwidth is also reported to Stripe as metered usage (Billing Meters),
// so it can be billed.

// Settings keys (the panel's settings table; secrets never leave it).
const (
	SettingStripeWebhookSecret = "billing_stripe_webhook_secret"
	SettingStripeSecretKey     = "billing_stripe_secret_key"
	SettingStripeMeterEvent    = "billing_stripe_meter_event"
	SettingStripePrices        = "billing_stripe_prices"
)

// StripeTolerance is how old a signed webhook may be: older ones are
// refused as replays.
const StripeTolerance = 5 * time.Minute

var (
	ErrStripeOff    = errors.New("Stripe webhooks are not configured")
	ErrBadSignature = errors.New("invalid Stripe signature")
)

// StripeSettings is the Stripe configuration.
type StripeSettings struct {
	WebhookSecret string
	SecretKey     string
	MeterEvent    string
	// Prices maps Stripe price IDs to plan IDs.
	Prices map[string]string
}

func (s *Service) StripeSettings(ctx context.Context) (*StripeSettings, error) {
	var out StripeSettings
	var err error
	get := func(k string) string {
		if err != nil {
			return ""
		}
		var v string
		v, err = s.Store.Setting(ctx, k)
		return v
	}
	out.WebhookSecret, out.SecretKey, out.MeterEvent = get(SettingStripeWebhookSecret), get(SettingStripeSecretKey), get(SettingStripeMeterEvent)
	prices := get(SettingStripePrices)
	if err != nil {
		return nil, err
	}
	out.Prices = map[string]string{}
	if prices != "" {
		if err := json.Unmarshal([]byte(prices), &out.Prices); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// StripeSettingsInput changes the Stripe configuration; nil fields are
// kept, "" clears a secret.
type StripeSettingsInput struct {
	WebhookSecret *string           `json:"webhook_secret"`
	SecretKey     *string           `json:"secret_key"`
	MeterEvent    *string           `json:"meter_event"`
	Prices        map[string]string `json:"prices"`
}

var (
	priceRe      = regexp.MustCompile(`^price_[A-Za-z0-9]{1,100}$`)
	meterEventRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
	stripeIDRe   = regexp.MustCompile(`^[a-z]{2,8}_[A-Za-z0-9]{1,200}$`)
	printableRe  = regexp.MustCompile(`^[\x21-\x7e]{1,300}$`)
)

func (s *Service) SetStripeSettings(ctx context.Context, in StripeSettingsInput) error {
	if in.WebhookSecret != nil && *in.WebhookSecret != "" && (!strings.HasPrefix(*in.WebhookSecret, "whsec_") || !printableRe.MatchString(*in.WebhookSecret)) {
		return fmt.Errorf("%w: the webhook signing secret starts with whsec_", ErrInvalid)
	}
	if in.SecretKey != nil && *in.SecretKey != "" && (!(strings.HasPrefix(*in.SecretKey, "sk_") || strings.HasPrefix(*in.SecretKey, "rk_")) ||
		!printableRe.MatchString(*in.SecretKey)) {
		return fmt.Errorf("%w: the secret key starts with sk_ (or rk_ for a restricted key)", ErrInvalid)
	}
	if in.MeterEvent != nil && *in.MeterEvent != "" && !meterEventRe.MatchString(*in.MeterEvent) {
		return fmt.Errorf("%w: invalid meter event name", ErrInvalid)
	}
	for price, plan := range in.Prices {
		if !priceRe.MatchString(price) {
			return fmt.Errorf("%w: %q is not a Stripe price ID (price_…)", ErrInvalid, price)
		}
		if _, err := s.Store.GetPlan(ctx, plan); err != nil {
			return fmt.Errorf("%w: price %s: no plan %q", ErrInvalid, price, plan)
		}
	}
	set := func(k string, v *string) error {
		if v == nil {
			return nil
		}
		return s.Store.SetSetting(ctx, k, *v)
	}
	if err := errors.Join(set(SettingStripeWebhookSecret, in.WebhookSecret), set(SettingStripeSecretKey, in.SecretKey),
		set(SettingStripeMeterEvent, in.MeterEvent)); err != nil {
		return err
	}
	if in.Prices != nil {
		b, err := json.Marshal(in.Prices)
		if err != nil {
			return err
		}
		return s.Store.SetSetting(ctx, SettingStripePrices, string(b))
	}
	return nil
}

// VerifyStripeSignature checks a Stripe-Signature header ("t=<unix>,v1=<hex>
// [,v1=…]") against the raw payload: HMAC-SHA256 of "<t>.<payload>" with
// the signing secret, compared in constant time with every v1 signature
// (several during a secret roll), and a timestamp within StripeTolerance.
func VerifyStripeSignature(payload []byte, header, secret string, now time.Time) error {
	if secret == "" {
		return ErrStripeOff
	}
	var ts string
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			if b, err := hex.DecodeString(v); err == nil && len(b) == sha256.Size {
				sigs = append(sigs, b)
			}
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(t, 0)); d > StripeTolerance || d < -StripeTolerance {
		return fmt.Errorf("%w: timestamp outside the tolerance", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(payload)
	want := mac.Sum(nil)
	ok := false
	for _, sig := range sigs {
		// Every signature is compared: no early exit to time.
		if hmac.Equal(sig, want) {
			ok = true
		}
	}
	if !ok {
		return ErrBadSignature
	}
	return nil
}

// stripeRef is an ID field that Stripe sends as a string, or as the object
// when expanded.
type stripeRef string

func (r *stripeRef) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*r = stripeRef(s)
		return nil
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*r = stripeRef(obj.ID)
	return nil
}

type stripeEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type stripeSubscription struct {
	ID       stripeRef `json:"id"`
	Customer stripeRef `json:"customer"`
	Status   string    `json:"status"`
	Items    struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

type stripeCheckout struct {
	Mode            string    `json:"mode"`
	Customer        stripeRef `json:"customer"`
	Subscription    stripeRef `json:"subscription"`
	CustomerDetails struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"customer_details"`
	Metadata map[string]string `json:"metadata"`
}

type stripeInvoice struct {
	Customer      stripeRef `json:"customer"`
	Subscription  stripeRef `json:"subscription"`
	CustomerEmail string    `json:"customer_email"`
	AmountDue     int64     `json:"amount_due"`
	Currency      string    `json:"currency"`
}

// HandleStripeWebhook authenticates and processes one webhook call. An
// error means Stripe should retry (the event isn't recorded as processed);
// ErrStripeOff and ErrBadSignature are the caller's fault.
func (s *Service) HandleStripeWebhook(ctx context.Context, payload []byte, sigHeader string) error {
	cfg, err := s.StripeSettings(ctx)
	if err != nil {
		return err
	}
	if cfg.WebhookSecret == "" {
		return ErrStripeOff
	}
	if err := VerifyStripeSignature(payload, sigHeader, cfg.WebhookSecret, s.now()); err != nil {
		return err
	}
	var ev stripeEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.ID == "" || ev.Type == "" {
		return fmt.Errorf("%w: not a Stripe event", ErrInvalid)
	}
	// One at a time, so a retry racing the original is seen as a duplicate.
	s.stripeMu.Lock()
	defer s.stripeMu.Unlock()
	if seen, err := s.Store.StripeEventSeen(ctx, ev.ID); err != nil {
		return err
	} else if seen {
		return nil
	}
	if err := s.processStripe(ctx, cfg, &ev); err != nil {
		s.Log.Error("processing a Stripe event", "event", ev.ID, "type", ev.Type, "err", err)
		return err
	}
	return s.Store.RecordStripeEvent(ctx, ev.ID, ev.Type, s.now())
}

func (s *Service) processStripe(ctx context.Context, cfg *StripeSettings, ev *stripeEvent) error {
	obj := ev.Data.Object
	switch ev.Type {
	case "checkout.session.completed":
		var c stripeCheckout
		if err := json.Unmarshal(obj, &c); err != nil {
			return err
		}
		if c.Customer == "" {
			return nil // a one-off payment without a customer: nothing to host
		}
		if c.Subscription != "" && cfg.SecretKey != "" && s.Stripe != nil {
			sub, err := s.Stripe.Subscription(ctx, cfg.SecretKey, string(c.Subscription))
			if err != nil {
				return err
			}
			return s.syncSubscription(ctx, cfg, ev, sub, c.CustomerDetails.Email, c.CustomerDetails.Name)
		}
		// Without the API, the plan comes with customer.subscription.created;
		// the checkout only fills in who the customer is.
		sub := &stripeSubscription{ID: c.Subscription, Customer: c.Customer}
		if plan := c.Metadata["wpgenie_plan"]; plan != "" {
			if _, err := s.Store.GetPlan(ctx, plan); err == nil {
				return s.upsertStripeAccount(ctx, sub, plan, c.CustomerDetails.Email, c.CustomerDetails.Name)
			}
		}
		if a, err := s.Store.AccountByStripeCustomer(ctx, string(c.Customer)); err == nil {
			return s.fillStripeContact(ctx, a, c.CustomerDetails.Email, c.CustomerDetails.Name)
		}
		return nil
	case "customer.subscription.created", "customer.subscription.updated":
		var sub stripeSubscription
		if err := json.Unmarshal(obj, &sub); err != nil {
			return err
		}
		return s.syncSubscription(ctx, cfg, ev, &sub, "", "")
	case "customer.subscription.deleted":
		var sub stripeSubscription
		if err := json.Unmarshal(obj, &sub); err != nil {
			return err
		}
		a, ok, err := s.stripeAccount(ctx, ev, string(sub.Customer), string(sub.ID))
		if !ok || err != nil {
			return err
		}
		s.event(ctx, a.ID, "billing", "Stripe subscription "+string(sub.ID)+" ended")
		_, err = s.Suspend(ctx, a.ID, ReasonBilling)
		return s.stripeApplied(ctx, a, ev, ignoreTerminated(err))
	case "invoice.paid":
		var inv stripeInvoice
		if err := json.Unmarshal(obj, &inv); err != nil {
			return err
		}
		if inv.Subscription == "" {
			return nil // a one-off invoice says nothing about the hosting subscription
		}
		a, ok, err := s.stripeAccount(ctx, ev, string(inv.Customer), string(inv.Subscription))
		if !ok || err != nil {
			return err
		}
		if a.Status != store.AccountSuspended || a.SuspendReason != ReasonBilling {
			return s.stripeApplied(ctx, a, ev, nil)
		}
		// A late payment of a subscription that has since ended doesn't
		// bring the account back: ask Stripe when we can.
		if cfg.SecretKey != "" && s.Stripe != nil {
			cur, err := s.Stripe.Subscription(ctx, cfg.SecretKey, string(inv.Subscription))
			if err != nil {
				return err
			}
			if cur.Status != "active" && cur.Status != "trialing" {
				return s.stripeApplied(ctx, a, ev, nil)
			}
		}
		s.event(ctx, a.ID, "billing", "Invoice paid")
		_, err = s.Unsuspend(ctx, a.ID, ReasonBilling)
		return s.stripeApplied(ctx, a, ev, err)
	case "invoice.payment_failed":
		var inv stripeInvoice
		if err := json.Unmarshal(obj, &inv); err != nil {
			return err
		}
		if inv.Subscription == "" {
			return nil
		}
		a, ok, err := s.stripeAccount(ctx, ev, string(inv.Customer), string(inv.Subscription))
		if !ok || err != nil {
			return err
		}
		// Stripe retries the payment; the account is suspended only once
		// the subscription gives up (unpaid or canceled).
		s.event(ctx, a.ID, "billing", "A payment failed")
		s.emit(ctx, EventAccountPaymentFailed, map[string]any{"account_id": a.ID, "stripe_customer_id": a.StripeCustomerID,
			"amount_due": inv.AmountDue, "currency": inv.Currency})
		return nil
	}
	return nil // not an event WPGenie acts on
}

// stripeAccount finds the account an event about a customer's subscription
// is for. ok is false when WPGenie must ignore it: no account, another of
// the customer's subscriptions (a customer can have several; only the
// hosting one counts), or an event older than the last one applied
// (Stripe doesn't deliver in order: a retried "active" must not undo a
// later cancellation).
func (s *Service) stripeAccount(ctx context.Context, ev *stripeEvent, customer, subscription string) (*store.Account, bool, error) {
	a, err := s.Store.AccountByStripeCustomer(ctx, customer)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if a.StripeSubscriptionID != "" && subscription != a.StripeSubscriptionID {
		return nil, false, nil
	}
	last, err := s.Store.StripeEventAt(ctx, a.ID)
	if err != nil {
		return nil, false, err
	}
	if ev.Created < last {
		s.Log.Info("ignoring an out-of-order Stripe event", "event", ev.ID, "account", a.ID)
		return nil, false, nil
	}
	return a, true, nil
}

// stripeApplied records the time of the event last applied to an account.
func (s *Service) stripeApplied(ctx context.Context, a *store.Account, ev *stripeEvent, err error) error {
	if err != nil {
		return err
	}
	return s.Store.SetStripeEventAt(ctx, a.ID, ev.Created)
}

func ignoreTerminated(err error) error {
	if errors.Is(err, ErrConflict) {
		return nil // a terminated account stays terminated
	}
	return err
}

// syncSubscription creates or updates the subscription's account: its plan
// from the price, and its status from the subscription's. A subscription
// whose price maps to no plan never creates an account, and never replaces
// the account's hosting subscription.
func (s *Service) syncSubscription(ctx context.Context, cfg *StripeSettings, ev *stripeEvent, sub *stripeSubscription, email, name string) error {
	plan := ""
	for _, it := range sub.Items.Data {
		if p, ok := cfg.Prices[it.Price.ID]; ok {
			plan = p
			break
		}
	}
	a, err := s.Store.AccountByStripeCustomer(ctx, string(sub.Customer))
	switch {
	case errors.Is(err, store.ErrNotFound) && plan == "":
		s.Log.Warn("Stripe subscription with no price mapped to a plan: no account created", "customer", sub.Customer)
		return nil
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	case a.StripeSubscriptionID != "" && string(sub.ID) != a.StripeSubscriptionID && plan == "":
		return nil // another product of theirs
	default:
		if last, err := s.Store.StripeEventAt(ctx, a.ID); err != nil {
			return err
		} else if ev.Created < last {
			s.Log.Info("ignoring an out-of-order Stripe event", "event", ev.ID, "account", a.ID)
			return nil
		}
	}
	// With the API key, the subscription's current state beats the
	// event's snapshot.
	if cfg.SecretKey != "" && s.Stripe != nil && sub.ID != "" && ev.Type != "checkout.session.completed" {
		cur, err := s.Stripe.Subscription(ctx, cfg.SecretKey, string(sub.ID))
		if err != nil {
			return err
		}
		sub.Status = cur.Status
	}
	if err := s.upsertStripeAccount(ctx, sub, plan, email, name); err != nil {
		return err
	}
	a, err = s.Store.AccountByStripeCustomer(ctx, string(sub.Customer))
	if errors.Is(err, store.ErrNotFound) {
		return nil // no plan mapped: nothing was created
	}
	if err != nil {
		return err
	}
	switch sub.Status {
	case "unpaid", "canceled", "incomplete_expired":
		s.event(ctx, a.ID, "billing", "Stripe subscription is "+sub.Status)
		_, err = s.Suspend(ctx, a.ID, ReasonBilling)
		err = ignoreTerminated(err)
	case "active", "trialing":
		if a.Status == store.AccountSuspended && a.SuspendReason == ReasonBilling {
			_, err = s.Unsuspend(ctx, a.ID, ReasonBilling)
		}
	case "past_due":
		s.event(ctx, a.ID, "billing", "Stripe subscription is past due")
	}
	return s.stripeApplied(ctx, a, ev, err)
}

// upsertStripeAccount makes sure the customer has an account on plan (when
// one is mapped), recording the subscription.
func (s *Service) upsertStripeAccount(ctx context.Context, sub *stripeSubscription, plan, email, name string) error {
	customer := string(sub.Customer)
	if !stripeIDRe.MatchString(customer) {
		return fmt.Errorf("%w: invalid Stripe customer %q", ErrInvalid, customer)
	}
	if sub.ID != "" && !stripeIDRe.MatchString(string(sub.ID)) {
		return fmt.Errorf("%w: invalid Stripe subscription %q", ErrInvalid, sub.ID)
	}
	a, err := s.Store.AccountByStripeCustomer(ctx, customer)
	if errors.Is(err, store.ErrNotFound) {
		if plan == "" {
			s.Log.Warn("Stripe subscription with no price mapped to a plan: no account created", "customer", customer)
			return nil
		}
		acctName := strings.TrimSpace(name)
		if acctName == "" {
			acctName = email
		}
		if acctName == "" {
			acctName = customer
		}
		if !emailRe.MatchString(email) {
			email = ""
		}
		_, _, _, err := s.CreateAccount(ctx, AccountInput{Name: truncateRunes(acctName, 100), Kind: store.AccountCustomer,
			PlanID: plan, Email: email, StripeCustomerID: customer, StripeSubscriptionID: string(sub.ID)},
			"stripe:"+customer, nil, false)
		return err
	}
	if err != nil {
		return err
	}
	in := inputOf(a)
	if sub.ID != "" {
		in.StripeSubscriptionID = string(sub.ID)
	}
	if plan != "" {
		in.PlanID = plan
	}
	if in == inputOf(a) {
		return s.fillStripeContact(ctx, a, email, name)
	}
	if _, err := s.UpdateAccount(ctx, a.ID, in, false); err != nil {
		return err
	}
	return s.fillStripeContact(ctx, a, email, name)
}

// fillStripeContact adds the checkout's email to an account without one.
func (s *Service) fillStripeContact(ctx context.Context, a *store.Account, email, name string) error {
	in := inputOf(a)
	if in.Email == "" && emailRe.MatchString(email) {
		in.Email = email
	}
	if (in.Name == a.StripeCustomerID || in.Name == "") && strings.TrimSpace(name) != "" {
		in.Name = truncateRunes(strings.TrimSpace(name), 100)
	}
	if in == inputOf(a) {
		return nil
	}
	_, err := s.UpdateAccount(ctx, a.ID, in, false)
	return err
}

func inputOf(a *store.Account) AccountInput {
	return AccountInput{Name: a.Name, Kind: a.Kind, PlanID: a.PlanID, ParentID: a.ParentID, Email: a.Email,
		WHMCSServiceID: a.WHMCSServiceID, StripeCustomerID: a.StripeCustomerID, StripeSubscriptionID: a.StripeSubscriptionID}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// StripeAPI calls Stripe's REST API (form-encoded requests, the secret key
// as a bearer token).
type StripeAPI struct {
	Base   string // default https://api.stripe.com
	Client *http.Client
}

func (a *StripeAPI) call(ctx context.Context, key, method, path string, form url.Values, out any) error {
	base := a.Base
	if base == "" {
		base = "https://api.stripe.com"
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(b, &e)
		return fmt.Errorf("stripe: %s: %s", resp.Status, e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Subscription fetches a subscription (its prices and status).
func (a *StripeAPI) Subscription(ctx context.Context, key, id string) (*stripeSubscription, error) {
	if !stripeIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid subscription ID", ErrInvalid)
	}
	var sub stripeSubscription
	if err := a.call(ctx, key, http.MethodGet, "/v1/subscriptions/"+id, nil, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// MeterEvent reports usage to a Billing Meter. The identifier makes a
// retried report count once.
func (a *StripeAPI) MeterEvent(ctx context.Context, key, event, customer string, value int64, identifier string, at time.Time) error {
	form := url.Values{"event_name": {event}, "payload[stripe_customer_id]": {customer},
		"payload[value]": {strconv.FormatInt(value, 10)}, "identifier": {identifier},
		"timestamp": {strconv.FormatInt(at.Unix(), 10)}}
	return a.call(ctx, key, http.MethodPost, "/v1/billing/meter_events", form, nil)
}
