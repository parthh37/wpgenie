# Billing suite, support tickets and log shipping — build spec

Working spec for four parallel workstreams. Read all of it; build only yours.
When the work is merged this file is replaced by user docs.

## Product decisions (from the operator; don't revisit)

- WHMCS-style billing **built into WPGenie**: products with prices, orders from a public order form, invoices,
  payments, credit, promotions, taxes, dunning (reminders → suspension → optional termination), a client area.
- Gateways: **Stripe** (card; Checkout + saved card auto-charge), **Razorpay** (Payment Links), **bank
  transfer / manual** (instructions shown, staff record the payment). No PayPal.
- **The operator bills customers and resellers. Resellers' own customers are never billed by WPGenie**
  (resellers bill them elsewhere, e.g. their WHMCS through `integrations/whmcs`). Accounts with a parent are
  always billing mode `none`.
- **One store currency** + tax rules by country/state (two levels, like WHMCS: e.g. CGST+SGST, GST+PST),
  optional client tax ID, per-account tax exemption.
- **Support tickets** included (departments, replies, internal notes, attachments, canned replies,
  auto-close, e-mail notifications). A reseller's customers' tickets are handled by that reseller, who can
  escalate them to the operator.
- **Logs shipped to S3-compatible storage** with an open-source shipper (Vector), configured from the admin
  panel, so logs don't fill the server's disk.
- Users are not technical: defaults must work, expert knobs go behind "Advanced", the UI must be rich and
  friendly (cards, charts, clear empty states, inline help), and still accessible (labels, focus, contrast).

## What already exists (read before building)

- `internal/billing`: accounts (customer/reseller, `parent_id`), plans (limits + features + `resellable`),
  suspension with reasons (`reseller < overage < billing < admin`), `Terminate`, usage metering, Stripe
  subscriptions + metered bandwidth (`stripe.go`), outgoing signed webhooks (`webhooks.go`).
- `internal/api/tenancy.go`: tenants reach only `tenantRoutes` (default deny); ownership of `{id}` checked
  centrally. **New in this branch** (use these, don't edit the map in place):
  - `registerTenantRoutes(map[string]tenantRule{...})` from your file's `init()`;
  - `registerScope("/api/v1/<things>/{id}", func(s *Server, ctx, p *Principal, id string) bool)` — routes under
    that prefix are 404 to tenants unless your func says they own `{id}`;
  - `tenantRule{whileSuspended: true}` — a suspended account may still call it (pay, open a ticket);
  - `registerErrorStatus(yourpkg.ErrInvalid, http.StatusBadRequest)` etc. from `init()`, instead of editing
    `writeError`.
- `internal/mailer` (**new, shared, done**): `mailer.Register(Template{...})` from your package's `init()`
  (name, group, description, subject/body as text/template, `Vars`, `Sample` data used by the editor's
  preview and validation); `(*mailer.Service).Queue(ctx, mailer.Message{To, Template, Data, AccountID,
  DedupeKey, ReplyTo})` queues a rendered message (use DedupeKey for anything automation sends, e.g.
  `"invoice.reminder:42:2026-10-05"`). Body conventions: blank line = paragraph, a line `[[Label|https://…]]`
  = button. Every template also gets `.Brand.Name`, `.Brand.URL`, `.PanelURL`. Messages are delivered by
  `mailer.Run`; the outbox is the e-mail log (`GET /api/v1/email/log`, per account
  `GET /api/v1/accounts/{id}/emails`). Staff edit templates at `/api/v1/email/templates`.
- Store: SQLite by default, PostgreSQL optional; queries are written once (see `internal/store/dialect.go`:
  `?` placeholders, `INSERT ... RETURNING id`, no `LastInsertId`, no `INSERT OR REPLACE`). Every store function
  gets a case in the suite run on both backends (`suite_test.go`, `forEachBackend`). Times are unix seconds in
  INTEGER columns. Money is INTEGER minor units.
- **Your migration slot already exists**: `invoicingSchema` (`internal/store/invoicing.go`), `supportSchema`
  (`internal/store/support.go`), `logshipSchema` (`internal/store/logship.go`) — replace the `SELECT 1;`
  placeholder with your DDL (several statements in one string are fine). Don't add entries to `migrations`.
- **Your route hook already exists**: `invoicingRoutes`, `supportRoutes`, `logshipRoutes` in
  `internal/api/<name>.go`, called from `Handler()` with the mux (for public endpoints) and `r(pattern, role,
  handler)` (authenticated; roles `viewer < operator < admin` for staff).
- Dashboard: vanilla JS, no frameworks, no CDNs (CSP `default-src 'self'`: no inline scripts or style
  attributes set from HTML strings; set `el.style.x` from JS is fine). Helpers in `app.js`/`ux.js`/`panels.js`
  (`h()`, `api()`, `table()`, `ask()`, toasts, dialogs, `loaders[tab]`, hash routes). Icons: Lucide symbols in
  the sprite in `index.html`. Light/dark themes via CSS variables in `style.css`. Look at `accounts.js` and
  `files.js` for recent, idiomatic examples. Preview without Docker: see "Previewing the UI" below.

## Ownership of files (to merge cleanly)

Each workstream creates its own files. Shared files may only get these small additions (the lead merges):
`internal/api/server.go` (one `Server` field), `cmd/wpgenie/main.go` (a wiring block), `internal/config/config.go`
(a field + default), `internal/web/static/index.html` (script/link tags, nav buttons, sprite icons, section
containers), `internal/web/static/app.js` (only if unavoidable, a line or two). Put CSS in your own file
(`billing.css`, `support.css`, `logs.css`) linked from `index.html`, using the variables of `style.css`.

## Conventions

- Match the code around you: comment density and style (explain *why*), naming, error wrapping
  (`fmt.Errorf("%w: …", ErrInvalid)`), tests next to code, table-driven where it fits.
- Security: every tenant route is scoped; secrets are write-only in APIs (`*_set` booleans); webhook
  signatures verified in constant time; public endpoints rate-limited and size-limited; no user input in
  headers/filenames/SQL without validation.
- Done means: `go build ./... && go vet ./... && go test ./internal/...` pass, `node --check` passes on your JS,
  `gofmt -l` clean. Commit your work on your branch (clear message, ending with the line
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`). Don't run Docker, don't push.

---

## Workstream B — Billing backend (`internal/billing` + store + API)

New files in `internal/billing` (e.g. `invoicing.go`, `invoices.go`, `payments.go`, `gateways.go`,
`stripe_checkout.go`, `razorpay.go`, `orders.go`, `tax.go`, `promotions.go`, `automation.go`, `emails.go`),
`internal/store/invoicing.go` (schema + queries), `internal/api/invoicing.go` (+ tests). Extend existing types
where noted (Plan, Account) — additive columns via your migration.

### Money, currency, cycles
- Amounts: int64 minor units everywhere in APIs (`"total": 11800` = 118.00). Currency settings: ISO code,
  symbol, decimals (0–3). Percentages as hundredths of a percent (`1800` = 18%).
- Cycles: `monthly`(1) `quarterly`(3) `semiannually`(6) `annually`(12) `biennially`(24) `triennially`(36)
  months. Due dates advance by whole months from an **anchor day** (31 → last day of shorter months, back to
  31 when possible).

### Plans (products)
Additive fields on `store.Plan` / plans API (`GET/POST/PUT /api/v1/plans`):
```json
{ "description": "For growing stores", "public": true, "sort": 10, "account_kind": "customer",
  "prices": { "monthly": {"price": 1500, "setup_fee": 0}, "annually": {"price": 15000, "setup_fee": 0} },
  "overage_gb_price": 0 }
```
`public`: listed on the order form. `account_kind`: what an order creates (`customer` | `reseller`). A plan
without prices is free / not orderable. `overage_gb_price` > 0: bandwidth beyond the plan is invoiced per
(started) GB at month end for internally billed accounts.

### Billing profile (per account)
`GET /api/v1/accounts/{id}/billing` (staff; tenant: own account and a reseller's customers read-only):
```json
{ "mode": "invoice",            // none | invoice | stripe_subscription (legacy Stripe flow) | whmcs
  "cycle": "monthly", "price_override": null, "next_due_at": "2026-10-30T00:00:00Z", "anchor_day": 30,
  "auto_pay": false, "card": {"brand": "visa", "last4": "4242", "exp_month": 12, "exp_year": 2028} ,
  "credit": 2500, "tax_exempt": false, "cancel_at": null, "cancel_reason": "",
  "contact": {"first_name":"","last_name":"","company":"","email":"a@b.c","phone":"","address1":"","address2":"",
              "city":"","state":"","postcode":"","country":"IN","tax_id":""},
  "balance_due": 11800, "overdue": true, "recurring_amount": 1500,
  "upcoming": {"period_start":"…","period_end":"…","amount":1500} }
```
- `PUT /api/v1/accounts/{id}/billing` (admin): mode, cycle, price_override, next_due_at, tax_exempt, auto_pay.
  Setting mode `invoice` on an account with a parent → 400.
- `PUT /api/v1/accounts/{id}/billing/contact` (tenant own account; whileSuspended; staff too): contact only.
- `PUT /api/v1/accounts/{id}/billing/auto-pay` `{ "auto_pay": true }` (tenant; needs a saved card).
- `DELETE /api/v1/accounts/{id}/billing/card` (tenant/admin): forget the saved card (detach at Stripe).
- `GET /api/v1/accounts/{id}/credit` → `[{"id","amount","balance","description","invoice_id","at","by"}]`;
  `POST /api/v1/accounts/{id}/credit` (admin) `{ "amount": 1000, "description": "Goodwill" }` (negative removes).
- Plan changes (tenant, own account; staff any):
  `POST /api/v1/accounts/{id}/plan-change/quote` `{ "plan_id": "pro", "cycle": "annually" }` →
  `{ "credit": 700, "charge": 12000, "subtotal": 11300, "tax_lines": [...], "total": 13334, "effective": "now",
     "new_next_due_at": "…", "items": [...] }` (proration: unused fraction of what was paid for the current period
  becomes a credit line; the new plan is charged pro rata to the next due date, or for a whole new cycle when
  the cycle changes). `POST /api/v1/accounts/{id}/plan-change` same body → `{ "invoice": {...} }` or
  `{ "applied": true }` when nothing is due (negative totals go to credit). The plan switches when that invoice
  is paid.
- Cancellation: `POST /api/v1/accounts/{id}/cancel` `{ "when": "end_of_period" | "immediately", "reason": "" }`
  (tenant: end_of_period only; admin both) and `DELETE /api/v1/accounts/{id}/cancel` (withdraw). At `cancel_at`
  the account is terminated (sites deleted only if the setting says so); unpaid renewal invoices are cancelled.

### Invoices
Invoice JSON (list items omit `items`, `payments`, `billing_address`):
```json
{ "id": 12, "number": "INV-000123", "account_id": 3, "account_name": "Acme Ltd", "kind": "renewal",
  "status": "unpaid", "overdue": true, "currency": "USD",
  "issued_at": "…", "due_at": "…", "paid_at": null, "period_start": "…", "period_end": "…",
  "subtotal": 10000, "discount": 1000, "tax_lines": [{"name":"GST","rate":1800,"amount":1620}], "tax": 1620,
  "total": 10620, "credit_applied": 0, "amount_paid": 0, "amount_refunded": 0, "balance": 10620,
  "items": [{"id":1,"kind":"plan","description":"Pro plan — Oct 30 to Nov 29, 2026","quantity":1,
             "unit_price":10000,"amount":10000,"taxable":true}],
  "billing_address": {"name":"…","company":"…","lines":["…"],"country":"IN","tax_id":"…"},
  "notes": "", "payments": [{"id":5,"gateway":"stripe","reference":"pi_…","amount":10620,"fee":0,
             "at":"…","refunded":0,"note":""}],
  "pay_methods": ["stripe","razorpay","manual"] }
```
- `kind`: `order | renewal | plan_change | overage | manual`. Item kinds: `plan setup discount proration credit
  late_fee overage custom`.
- `status`: `draft | unpaid | paid | cancelled | refunded` (+ `partially_refunded` as `refunded` < total).
  `overdue` is derived (unpaid and past due). Totals are computed server-side only; tax rounds half-up per
  tax line on the invoice's taxable subtotal (after discount); tax-inclusive mode backs tax out of prices.
- Numbering: `prefix` + zero-padded sequence; assigned when issued (`number_on: issue`) or when paid
  (`number_on: payment`, unpaid ones show `"Proforma #12"`). Gapless, assigned in a transaction.
- Routes (tenants: invoices of their own account only; resellers see only their own, not customers'):
  - `GET /api/v1/invoices?status=&account=&q=&from=&to=&overdue=1&before=&limit=` (staff viewer; tenant own)
  - `GET /api/v1/invoices/{id}`; `GET /api/v1/invoices/{id}/print` → printable HTML page (own CSS file, no
    inline styles; "Print / Save as PDF" button; company block, logo, addresses, items, taxes, payments, PAID
    stamp)
  - `POST /api/v1/invoices` (admin) `{ "account_id", "items":[{"description","quantity","unit_price","taxable"}],
    "due_at", "notes", "draft": true, "send_email": true }`; `PUT /api/v1/invoices/{id}` (admin; drafts fully,
    unpaid: notes/due_at only); `POST /api/v1/invoices/{id}/issue`; `POST /api/v1/invoices/{id}/cancel`
  - `POST /api/v1/invoices/{id}/payments` (admin) `{ "amount", "method": "bank|cash|cheque|other", "reference",
    "at", "note" }` (over-payment goes to credit)
  - `POST /api/v1/invoices/{id}/refund` (admin) `{ "payment_id", "amount", "to": "gateway" | "credit" }`
  - `POST /api/v1/invoices/{id}/remind` (admin; sends the reminder now)
  - `POST /api/v1/invoices/{id}/apply-credit` `{ "amount": null }` (tenant own, whileSuspended; staff)
  - `POST /api/v1/invoices/{id}/pay` `{ "method": "stripe|razorpay|manual", "save_card": true }` (tenant own,
    whileSuspended; staff) → one of `{ "redirect_url": "https://checkout.stripe.com/…" }`,
    `{ "instructions": "Bank: …", "reference": "INV-000123" }`, `{ "paid": true }`
  - `GET /api/v1/transactions?account=&method=&from=&to=&before=&limit=` (staff; tenant own)
  - `GET /api/v1/invoices.csv`, `GET /api/v1/transactions.csv` (admin; `from`, `to`)
- Paying an invoice: records a payment (idempotent per gateway reference), marks paid when balance ≤ 0, then
  its effects: an `order` invoice activates the pending account (or waits for approval); `renewal` advances
  `paid_until`/`next_due_at`; `plan_change` switches the plan; lifting a `billing` suspension when nothing is
  overdue any more; e-mail receipt; outgoing webhook `invoice.paid`.

### Gateways
Settings: `GET/PUT /api/v1/billing/invoicing` (admin) — see "Settings" below; Stripe keys stay in the existing
`/api/v1/billing/settings` (secret key, webhook secret).
- **Stripe**: Checkout Session (`mode=payment`, `client_reference_id`, `metadata[wpgenie_invoice]`, customer
  created/reused, `payment_intent_data[setup_future_usage]=off_session` when saving the card), success/cancel URLs
  back to `/#/billing/invoices/{id}?paid=1`. The **webhook** (existing endpoint, extend `processStripe`) records
  `checkout.session.completed` (payment_status paid) and `payment_intent.succeeded` for off-session charges;
  `charge.refunded` records refunds made in Stripe's dashboard. Saved card: store payment method ID + brand/last4/
  expiry on the profile. Auto-charge: PaymentIntent `off_session=true, confirm=true`; `requires_action` or a
  decline → `invoice.payment_failed` e-mail with a pay link. Refunds via `POST /v1/refunds`. Stub Stripe's API in
  tests with `httptest` (see `stripe_test.go`).
- **Razorpay**: Payment Links API (`POST https://api.razorpay.com/v1/payment_links`, basic auth key_id:key_secret,
  `reference_id` = `wpg_<invoice>_<attempt>`, `callback_url` = `<panel>/api/v1/billing/razorpay/callback`,
  `callback_method=get`, notify off). Callback (public): verify
  `HMAC_SHA256(payment_link_id|reference_id|status|payment_id, key_secret)`, record, redirect to the invoice.
  Webhook (public) `POST /api/v1/billing/razorpay/webhook`: verify `X-Razorpay-Signature` =
  `HMAC_SHA256(raw body, webhook_secret)`; handle `payment_link.paid` (and `refund.processed`). Refunds:
  `POST /v1/payments/{id}/refund`. Constant-time comparisons. API base overridable for tests.
- **Manual / bank transfer**: `instructions` text; `pay` returns them; staff record the payment.
- **Credit**: `apply-credit`; with `auto_apply_credit`, new invoices take available credit automatically.

### Orders (public order form backend)
- `GET /api/v1/store/catalog` (public): `{ "enabled", "company": {"name","logo_url"}, "currency": {...},
  "tax_inclusive", "terms_url", "methods": [{"id","name","description"}], "plans": [{"id","name","description",
  "account_kind","features":[...],"limits":{...},"prices":{...}}], "countries_need_state": ["US","CA","IN","AU"] }`
- `POST /api/v1/store/quote` (public) `{ "plan_id","cycle","promo","country","state","tax_id" }` →
  `{ "items","subtotal","discount","tax_lines","total","recurring_total","promo": {"valid","message"} }`
- `POST /api/v1/store/orders` (public; rate-limited per IP, e.g. 5/hour; body ≤ 64 KB; honeypot field
  `website` must be empty) `{ "plan_id","cycle","promo","method","accept_terms":true,
  "contact": {...contact fields, email required}, "user": {"username","password"} }` → creates, in one
  transaction, a **pending** account (new status `pending`: treated as suspended — no sites — but its users can
  sign in, pay and open tickets), its first user (tenant role from `account_kind`), billing profile, the order and
  its invoice; signs the user in (session cookie, same as login); e-mails `order.received`; returns
  `{ "account_id","invoice_id","next": {redirect_url|instructions|paid} }`. Validation errors are field-specific
  (`{"error": "...", "field": "user.username"}`).
- On payment: account → `active` (or stays pending with `orders_need_approval`), `account.welcome` e-mail,
  webhook `account.created`.
- Staff: `GET /api/v1/orders?status=pending|active|cancelled` (operator) with account, plan, cycle, total,
  invoice status, IP, created; `POST /api/v1/orders/{id}/accept` (admin; activates even if unpaid),
  `POST /api/v1/orders/{id}/cancel` (admin; cancels the invoice and terminates the pending account).

### Promotions and taxes
- `GET/POST /api/v1/billing/promotions`, `PUT/DELETE /api/v1/billing/promotions/{id}` (admin):
  `{ "id","code","description","type":"percent|fixed","value":1000,"plans":[],"cycles":[],"recurring":false,
     "max_uses":0,"uses":0,"starts_at":null,"expires_at":null,"new_clients_only":true,"enabled":true }`
  (`recurring`: applies to renewals too, else the first invoice only.)
- `GET/POST /api/v1/billing/tax-rules`, `PUT/DELETE /api/v1/billing/tax-rules/{id}` (admin):
  `{ "id","name":"GST","country":"IN","state":"","rate":1800,"level":1,"compound":false }` — per level, the most
  specific rule matching the client's country/state wins (state > country > `""` = everywhere); level 2 may be
  compound (applied on subtotal + level 1).

### Settings
`GET/PUT /api/v1/billing/invoicing` (admin; secrets write-only with `*_set`):
```json
{ "enabled": true,
  "currency": {"code":"USD","symbol":"$","decimals":2},
  "company": {"name":"","address":"multi-line","email":"","phone":"","tax_id":"","website":""},
  "invoice": {"prefix":"INV-","next_number":1,"number_on":"issue","days_before_due":7,"footer":"","terms_url":""},
  "tax": {"enabled":false,"inclusive":false,"exempt_with_tax_id":false},
  "automation": {"reminder_days_before":3,"overdue_reminder_days":[1,3,7],"suspend_after_days":5,
                 "terminate_after_days":0,"terminate_deletes_sites":false,"late_fee":{"type":"none","amount":0},
                 "late_fee_after_days":3,"auto_apply_credit":true,"orders_need_approval":false,
                 "autocharge":true},
  "methods": {"stripe":{"enabled":false,"name":"Card"},
              "razorpay":{"enabled":false,"name":"UPI, cards & netbanking","key_id":"","key_secret_set":false,
                          "webhook_secret_set":false,"webhook_url":"…"},
              "manual":{"enabled":true,"name":"Bank transfer","instructions":""}},
  "stripe_ready": false }
```
`GET /api/v1/billing/config` (any signed-in user): currency, company name, enabled methods with names, tax
inclusive — what the client area needs to format and offer.

### Automation (`RunInvoicing(ctx)`, every 15 minutes; idempotent; one run at a time)
1. Renewal invoices `days_before_due` ahead of `next_due_at` for `mode=invoice` accounts with a price (once per
   period: unique `(account_id, period_start, kind)`), credit applied if enabled, `invoice.created` e-mail.
2. Auto-charge saved cards on the due date (once per invoice per day).
3. Reminders: `reminder_days_before` before due; overdue reminders on each of `overdue_reminder_days` (dedupe keys).
4. Late fee once, `late_fee_after_days` after due (fixed amount or percent of the balance) as an item.
5. Suspend (`billing` reason) `suspend_after_days` after due (e-mail `account.suspended`); unsuspend when
   nothing is overdue (e-mail `account.unsuspended`).
6. Terminate `terminate_after_days` after due (0 = never), deleting sites only if `terminate_deletes_sites`.
7. Cancellations whose `cancel_at` has passed; bandwidth overage invoices for the previous month.
8. Pending orders unpaid for 14 days are cancelled.
`GET /api/v1/billing/automation` (admin) → last runs with counts/errors; `POST /api/v1/billing/automation/run`.

### Overview for staff
`GET /api/v1/billing/overview` (staff viewer) →
```json
{ "currency": {...}, "mrr": 0, "income_this_month": 0, "income_last_month": 0, "outstanding": 0,
  "overdue_total": 0, "overdue_count": 0, "unpaid_count": 0, "billed_accounts": 0, "pending_orders": 0,
  "credit_total": 0, "income_by_month": [{"month":"2025-11","amount":0}, "... 12 months"],
  "recent_payments": [], "overdue_invoices": [], "upcoming_renewals": [] }
```
(MRR: each active invoice-billed account's recurring price / cycle months.)

### E-mail templates to register (group "Billing")
`invoice.created`, `invoice.reminder`, `invoice.overdue`, `invoice.paid` (receipt), `invoice.payment_failed`,
`order.received`, `account.welcome`, `account.suspended`, `account.unsuspended`, `account.cancelled`,
`credit.added`. Recipient: the account's contact e-mail (fallback: account e-mail). Links point to
`<panel>/#/billing/invoices/{id}`. Also: set the mailer's `Brand` from `company` (name, website, address as
footer) when billing is enabled — expose `(*billing.Service).MailBrand(ctx) mailer.Brand`; the lead wires it.

### Tests
Proration math, due-date anchoring (Jan 31 → Feb 28/29 → Mar 31), tax (levels, compound, inclusive, exempt,
most-specific), promotions (limits, expiry, recurring), numbering (gapless, `number_on`), idempotent payments
and webhooks (replays), automation idempotency across runs and times (fake clock), order flow end to end with a
fake Stripe/Razorpay (`httptest`), tenant scoping of every new route (add to the tenancy tests pattern).

---

## Workstream F — Billing UI (admin, client area, order page, e-mail settings)

Build against the contract above (Workstream B builds the API in parallel; the lead reconciles). Files:
`internal/web/static/billing.js`, `billing.css`, `clientarea.js`, `order.html`, `order.js`, `order.css`,
`invoice.css` (print view styles, used by B's print HTML at `/invoice.css`), `emails.js`. Replace the current
admin "Billing" tab content in `accounts.js` only by moving its Stripe/webhooks settings into the new Billing
settings screens (keep every existing capability).

- **Staff "Billing" section** (sub-navigation within the tab):
  - *Overview*: KPI cards (MRR, income this month vs last with % change, outstanding, overdue with count,
    pending orders), a 12-month income bar chart (SVG, accessible: title/desc + a data table alternative), recent
    payments, overdue invoices with "Send reminder", upcoming renewals.
  - *Invoices*: status filter chips with counts, search, date range, table (number, client, issued, due, total,
    balance, status pill; overdue highlighted), "New invoice" (client picker, line-item editor with live totals,
    draft/issue, e-mail toggle), CSV export.
  - *Invoice detail*: document-like view (company, client, items, taxes, totals, PAID/OVERDUE stamp), actions:
    Print/PDF (opens `/api/v1/invoices/{id}/print`), Record payment, Refund, Send reminder, Apply credit, Cancel,
    Edit/Issue (drafts); payments list; e-mails sent about it.
  - *Transactions*, *Orders* (pending approvals with Accept / Cancel), *Promotions*, *Tax rules* (friendly
    presets: "India GST 18% (CGST 9% + SGST 9%)", "EU VAT", "Canada GST/HST") — each CRUD with dialogs.
  - *Settings*: Company, Invoices & numbering, Taxes, Automation (a visual timeline of the dunning steps:
    invoice → reminder → due → overdue reminders → suspend → terminate, driven by the numbers), Payment methods
    (Stripe: keys + webhook URL + status; Razorpay: keys + webhook URL; Bank transfer: instructions), Webhooks
    (existing outgoing webhooks UI moved here).
  - *E-mail*: SMTP settings with provider hints and "Send test"; template list grouped (Billing, Support…) with
    editor (subject/body, placeholder chips that insert `{{.X}}`, live preview in an iframe pointing at the
    `html_url` returned by `POST /api/v1/email/templates/{name}/preview`, reset to default); e-mail log with
    status filter, view (iframe to `/api/v1/email/log/{id}/html`), resend.
  - Account detail (existing Accounts screen): a "Billing" panel — mode/cycle/next due/price, contact, credit
    (+ add), invoices of the account, "New invoice", plan change, cancel.
  - Plans screen: pricing per cycle editor (price + setup fee), description, public toggle, account kind, sort,
    overage price.
- **Client area** (tenants; `clientarea.js`): a "Billing" tab with *Overview* (balance due banner with "Pay
  now", credit, current plan card with next due date and price, "Change plan" dialog with plan comparison and
  live quote, auto-pay toggle + saved card, cancel service), *Invoices* (list + detail + Pay dialog: choose
  method → redirect or show bank instructions; apply credit), *Transactions*, *Billing details* (contact form,
  country select, tax ID), *E-mails* (history, view). Pending (unpaid order) and suspended-for-billing accounts
  land on Billing with a clear banner. Returning from Stripe with `?paid=1` shows a "Payment received, thank
  you" state (poll the invoice until paid).
- **Order page** (`/order.html`, public; link it from the sign-in screen when the store is enabled): 1) plans
  as cards with a cycle switcher (monthly/annually + "save N%"), features and limits; 2) details (contact +
  username/password with strength hint; field errors inline); 3) review: promo code, live quote with taxes,
  payment method, terms checkbox → submit → redirect/instructions. Responsive, keyboard accessible, themed.
- Add nav entries: staff "Billing" (exists), tenants "Billing"; sprite icons as needed (`i-receipt`,
  `i-wallet`, `i-percent`, `i-mail-cog`...).

---

## Workstream T — Support tickets (`internal/support` + store + API + UI)

Files: `internal/support/*.go`, `internal/store/support.go`, `internal/api/support.go` (+ tests),
`internal/web/static/support.js`, `support.css`.

- **Model**: departments (`name, description, notify_email, hidden, sort`); tickets (`mask` like `WPG-482913`
  unique random, `account_id`, `user_id` (opener), `handler_account_id` (0 = operator's staff; a reseller's
  account for its customers' tickets), `department_id`, `site_id` (optional, must be in the opener's scope),
  `subject`, `status` = `open | answered | customer_reply | in_progress | on_hold | closed`, `priority` =
  `low | medium | high | urgent`, `assigned_user_id`, timestamps incl. `last_reply_at`, `first_response_at`,
  `closed_at`); messages (`author`, `staff` bool, `internal` note bool — never shown to customers); attachments
  (≤ 5 files × 5 MB per message by default; stored under `DataDir/support/<ticket>/<id>` with random names;
  served `Content-Disposition: attachment`, `Content-Security-Policy: default-src 'none'; sandbox`,
  `X-Content-Type-Options: nosniff`; images may preview inline); canned replies (`title, body`).
- **Rules**: a customer's reply sets `customer_reply`; a staff/handler reply sets `answered`; auto-close answered
  tickets after `auto_close_days` (setting, default 7) with an e-mail; customers can close and reopen (a reply
  reopens). Tickets from a reseller's customer are handled by the reseller (`handler_account_id`); the
  reseller can **escalate** to the operator. Staff see and act on all tickets.
- **Settings** (admin): `enabled`, `notify_emails` (staff recipients), `auto_close_days`, attachment limits and
  allowed extensions, `reply_to`.
- **API** (tenant access via `registerScope("/api/v1/tickets/{id}", …)`: own account's tickets; for resellers
  also their customers'; create/reply/close `whileSuspended`):
  `GET /api/v1/tickets?status=&department=&priority=&assigned=&account=&q=&before=&limit=`,
  `POST /api/v1/tickets` (JSON, or multipart with files; staff may open on behalf of an account),
  `GET /api/v1/tickets/{id}`, `POST /api/v1/tickets/{id}/replies` (JSON or multipart; `internal` for staff and
  handlers), `PUT /api/v1/tickets/{id}` (staff/handler: status, priority, department, assignee; customer: close/
  reopen only), `POST /api/v1/tickets/{id}/escalate` (reseller handler), `GET /api/v1/tickets/{id}/attachments/{att}`,
  `GET /api/v1/support/departments` (tenants: visible ones), department CRUD (admin), canned replies CRUD
  (operator), `GET/PUT /api/v1/support/settings` (admin), `GET /api/v1/support/overview` (staff: counts by
  status/department, awaiting reply, average first response time last 30 days).
- **E-mail templates** (group "Support"): `ticket.opened` (to the customer), `ticket.new_staff` (to staff or the
  handling reseller), `ticket.reply` (to the customer), `ticket.customer_reply` (to staff/handler),
  `ticket.closed`. Reply-To: settings `reply_to`. (Inbound e-mail piping is a follow-up.)
- **UI**: "Support" nav item for staff (Operate group) and tenants, with a badge (tickets awaiting you); list
  with status chips and filters, priority/status pills, relative times; ticket view as a conversation (customer
  vs staff bubbles, internal notes highlighted, attachments with icons, image thumbnails), reply box with canned
  replies picker, attachments (drag & drop), "Reply & close"; side panel with status, priority, department,
  assignee, account, site links; "New ticket" form for tenants (department cards, related site, priority);
  admin screens for departments, canned replies, settings.
- **Tests**: store suite cases, scoping (customer A can't see B's; reseller sees customers'; internal notes
  hidden), status transitions, auto-close, attachment limits and headers, e-mails queued.

---

## Workstream L — Log shipping to S3 (`internal/logship` + UI)

Files: `internal/logship/*.go`, `internal/store/logship.go` (only if you need tables; cursors can use the
existing `ingest_state`), `internal/api/logship.go` (+ tests), `internal/web/static/logs.js`, `logs.css`;
small hooks elsewhere as listed.

- **Shipper**: [Vector](https://vector.dev) (MPL-2.0) in a container `wpgenie-vector` the daemon manages (like
  other managed containers — see how `internal/phpmyadmin` / `internal/mail` start theirs through `runtime`).
  Image in config (`VectorImage`, pin a real, current `timberio/vector:<version>-alpine` tag — check Docker Hub).
  Hardened: `--cap-drop ALL` + only `DAC_READ_SEARCH` (read logs owned by others), read-only root FS, mounts
  read-only except its data dir (checkpoints, disk buffer) and the spool (it deletes shipped spool files), memory
  limit, internal metrics on `127.0.0.1` only (prometheus_exporter sink) for status. Config generated by the daemon
  (`DataDir/logship/vector.yaml`, 0600), credentials via an env file (0600), never in argv. Restart only when the
  generated config changes.
- **What ships** (each toggleable; object keys `<prefix><server>/<type>/YYYY/MM/DD/HH-<id>.log.gz`, NDJSON, gzip
  (or zstd), batches by size/time, disk buffer so outages don't lose logs):
  - `access` — Caddy's access log, tailed by Vector directly (Caddy rotates it), enriched with the site ID
    (daemon writes a CSV enrichment table host → site);
  - `php_errors`, `waf` — the daemon truncates these files after reading them, so **tee them from the daemon's
    readers** into the spool (hooks: `site.Service` PHP log reader, `proxy.WAFLog`);
  - `security` — shield blocks/bans/challenges (only in memory today): hook `shield.Record`;
  - `daemon` — the daemon's own slog output: a tee `slog.Handler` wrapping the JSON handler in `serve`;
  - `audit`, `jobs`, `account_events`, `email` (outbox metadata, no bodies) — DB exporters with cursors;
  - `mail` — docker-mailserver's log files (tailed);
  - `containers` — Docker json-file logs of WPGenie's containers (read-only mount of their log files; name
    mapping from the daemon), off by default.
- **Spool**: `DataDir/logship/spool/<type>/<hour>-<n>.jsonl`, rotated by hour/size; Vector deletes files after
  shipping (`remove_after_secs`); a size cap (setting, default 1 GB) drops the oldest when the destination is down
  (counted, shown). Spool writes are non-blocking for callers (buffered channel; drop + count when full).
- **Keep local disk small**: when shipping is on — Caddy `roll_keep` from settings (default 2; plumb into
  `proxy.Config` / `Caddyfile.tmpl`), Docker log rotation (`max-size`, `max-file`) on containers the daemon starts
  and in `deploy/docker-compose.yml`, spool deleted after upload.
- **Archive retention** in the bucket: nightly delete of objects older than N days (0 = keep) with the existing
  rclone runner (`internal/offload`).
- **Settings** (admin; `GET/PUT /api/v1/logs/settings`, secrets write-only): enabled; destination (provider preset
  `aws | r2 | b2 | wasabi | spaces | minio | custom`, endpoint, region, bucket, prefix, access key, secret,
  path-style); types; compression; batch max MB / max seconds; spool cap; archive retention days; local
  retention (access log files kept, container log size).
- **API**: `POST /api/v1/logs/test` (writes and deletes a small object via rclone; returns the error text),
  `GET /api/v1/logs/status` (container state and version, per type: events and bytes sent, errors, last upload,
  spool size, dropped), `GET /api/v1/logs/archives?type=&date=YYYY-MM-DD` (list objects), `GET
  /api/v1/logs/archives/object?key=` (decompressed, text/plain, capped at 20 MB; key validated against the
  prefix), all staff-only.
- **Cluster**: every server ships its own logs with the panel's settings (keys include the server name).
  Find how the panel hands settings to nodes (`cluster`, `ConfigureNode`) and do the same; if that's too large,
  ship the panel's logs and leave nodes as a documented follow-up.
- **UI** ("Logs" under Infrastructure): status header (shipping on/off, destination, last upload, health), cards
  per log type with icon, description, volume today and a toggle; destination form with provider presets that fill
  endpoint/region hints; "Test connection"; retention controls with plain-language help ("Keep 2 old access log
  files on this server"); archive browser (type, date, list, view/download).
- **Tests**: config generation (golden), spool rotation/cap/drop, exporters' cursors, settings validation and
  redaction, archive key validation, API scoping.

---

## Previewing the UI (no Docker)

A throwaway `cmd/<name>/main.go` (delete before committing): build `api.Server` like `newTenancyEnv` in
`internal/api/tenancy_test.go` (store on a temp file, `site.Service`, `shield.New`, updater with
`APIBase: "http://127.0.0.1:1"`), create a site and an admin user, add `GET /dev-login` that creates a session
(`st.CreateSession`) and sets the `wpgenie_session` cookie, serve on 127.0.0.1. Static files are embedded:
rebuild after JS/CSS edits.
