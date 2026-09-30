# Billing, support and logs: the operator's guide

WPGenie can run your hosting business end to end: plans with prices, a public order page, invoices,
card, UPI and bank payments, reminders and suspension for unpaid invoices, a client area, support
tickets, and your logs kept in your own storage. This guide sets it up step by step. Everything is in
the dashboard; you need an **admin** panel user.

How it fits together, in one paragraph: you (the operator) sell **plans** to **customers** and
**resellers**. Each account billed *by invoice* gets an invoice a week before each renewal, by e-mail.
It pays by card (Stripe), Razorpay or bank transfer. If it doesn't pay, it is reminded, then its sites
are suspended (and, if you choose, the account is closed). Paying brings the sites back at once.
**Resellers' own customers are never billed by WPGenie**: the reseller bills them itself (for example
with its own WHMCS, see [integrations/whmcs](../integrations/whmcs)).

**Before you start**

- The panel must have a public address with HTTPS (`PANEL_DOMAIN` at install, e.g.
  `panel.example.com`). The order page, the payment pages' way back to you, the links in e-mails and
  the Stripe and Razorpay webhooks all use it. A panel reachable only through an SSH tunnel can't take
  payments.
- All dates are UTC days. An invoice due on the 21st can be paid all that day, and is overdue from the
  22nd.
- Amounts in the dashboard are ordinary numbers (`15.00`). The API uses whole minor units (`1500`).

## 1. Send e-mail

Invoices, reminders, receipts and ticket replies are e-mailed. Set up sending first.

1. Open **Billing → E-mail → Sending**.
2. Choose your provider (Gmail / Google Workspace, Microsoft 365, Amazon SES, SendGrid, Mailgun,
   Postmark, Brevo, Zoho, or *Other*). This fills in the server, port and security.
3. Enter the username and password (for most providers an *app password* or *SMTP key*, not your
   normal password), the **From address** (an address your provider lets you send from, e.g.
   `billing@example.com`) and a **From name** (`Example Hosting`). *Reply-To* and a *BCC* copy for
   yourself are optional.
4. Save, then **Send test** to your own address. The answer shows the server's error if it fails.
5. Turn sending on.

Security is **STARTTLS** (usually port 587) or **TLS** (usually 465). Both check the server's
certificate. *None* (port 25) is only allowed without a username and password, so a password never
crosses the network in clear.

To use the mail server WPGenie runs itself (the **Mail** tab), create a mailbox such as
`billing@example.com` and use your mail hostname (e.g. `mail.example.com`), port 587, STARTTLS, and
that mailbox's address and password.

**Templates.** **Billing → E-mail → Templates** lists every message, grouped (Billing, Support), with
a preview. Edit the subject and body in plain text. A blank line starts a new paragraph, and a line
`[[Pay now|{{.Invoice.URL}}]]` becomes a button. Click a placeholder to insert it. **Reset** goes back
to the original. A template that fails to render isn't saved.

**Sent e-mail** is the log of every message: when it was sent, to whom, and whether it arrived.
Messages that fail are retried after 1 minute, 5 minutes, 30 minutes, 2 hours and 6 hours; after that
they are marked *failed*. Open one to see it as the client did, or **Resend** it. Messages written
while sending is off wait in the log and go out once it's on.

## 2. Company and currency

**Billing → Settings → Company**:

- **Built-in billing**: the master switch. Off, no invoices or reminders are made and the order page is
  closed. Existing invoices stay visible to clients.
- **Company**: name, address, e-mail, phone, website and tax ID (VAT, GST or company number). They are
  printed on every invoice. Once billing is on, e-mails are branded with them too.
- **Currency**: the ISO code (`USD`, `EUR`, `INR`…), its symbol and decimals. There is **one currency**
  for the whole store. Choose it before your first invoice: invoices keep the currency they were made
  in.

**Billing → Settings → Invoices**:

- **Number prefix** (`INV-`) and **next number**. Numbers have no gaps: `INV-000001`, `INV-000002`…
  To continue an old series, raise the next number. It can't go back to numbers already used.
- **Give invoices their number**: *when they're issued* (the usual way), or *when they're paid*.
  Until paid, the second kind show as *Proforma #12*. Some countries require this so that numbers only
  go to real sales.
- **Create renewal invoices** N days before the due date (default **7**), so clients have time to pay.
- **Terms of service URL**: if set, people ordering must accept them.
- **Invoice footer**: printed at the bottom of every invoice (bank details, a thank-you, legal text).

## 3. Taxes

**Billing → Settings → Taxes**:

- **Charge tax**: on or off.
- **Your prices**: *don't include tax* (tax is added on top: a 100.00 plan with 18% tax costs 118.00)
  or *include tax* (the price is what clients pay: 84.75 plus 15.25 tax).
- **No tax for clients with a tax ID**: for business clients who account for the tax themselves
  (EU reverse charge). It applies to every client who enters a tax ID, wherever they are. The number
  isn't validated.

Single accounts can also be marked **tax exempt** (Accounts → the account → Billing).

**Tax rules** (**Billing → Tax rules**) say what each client pays, based on the country and state
in their billing details. A rule has a name (printed on invoices), a country (two-letter code, or
empty for everywhere), an optional state, a rate and a **level**:

- For each of the two levels, the most specific matching rule applies: one for the client's state
  wins over one for their country, which wins over one for everywhere. A client can pay at most one
  level-1 tax and one level-2 tax.
- A level-2 rule is a second tax on top (SGST after CGST, PST after GST). Marked **compound**, it is
  charged on the price plus the level-1 tax. Otherwise both are charged on the price.
- State names are compared as written, ignoring case. `Maharashtra` doesn't match `MH`. Write the
  state the way your clients' addresses do.

The presets (**Start from a preset**) add the usual rules for you to check: India GST, EU VAT, Canada
GST/HST (+ PST/QST), UK VAT, Australia GST.

**Example: India GST.** Your business is in Maharashtra. Clients in Maharashtra pay CGST 9% + SGST 9%.
Clients elsewhere in India pay IGST 18%. Clients abroad pay nothing (export of services). The preset
asks for your state and adds:

| Name | Country | State | Rate | Level |
|---|---|---|---|---|
| CGST | IN | Maharashtra | 9% | 1 |
| SGST | IN | Maharashtra | 9% | 2 |
| IGST | IN | *(any)* | 18% | 1 |

A client in Pune gets CGST (the state rule beats the country rule on level 1) and SGST. A client in
Bengaluru gets IGST only (no level-2 rule matches Karnataka). A 1,000.00 plan for the Pune client:
1,000.00 + CGST 90.00 + SGST 90.00 = 1,180.00. Clients in India (and in the US, Canada and Australia)
must give a state when they order.

**Example: EU VAT (One-Stop Shop).** The preset adds one level-1 `VAT` rule per EU country at its
standard rate (as of 2025; check them). A consumer in Germany pays 19% and one in France pays 20%.
Clients outside the EU pay nothing. For business clients with a VAT number, turn on **No tax for
clients with a tax ID** (reverse charge). That switch also exempts business clients in your own
country, who should normally pay your VAT. If you have such clients, leave it off and mark only
foreign business accounts **tax exempt**.

Rounding: each tax line is rounded half up once per invoice, on the taxable amount after discounts.
Late fees aren't taxed.

## 4. Plans and prices

Plans are in the **Plans** tab: limits (sites, disk, bandwidth, per-site resources, burst minutes),
features and backup destinations, as before. For selling, each plan also has:

- **Prices per billing cycle**: monthly, every 3 months, every 6 months, yearly, every 2 years,
  every 3 years. Give a price for each cycle you sell, plus an optional **setup fee** charged once with
  the first invoice. A plan without any price isn't sold.
- **Description**, shown on the order page.
- **Show it on the order page** (public) and its **position** there (lower first). Plans that aren't
  public can still be given to accounts by staff.
- **An order creates**: a *customer* account, or a *reseller* account. Reseller plans are what you
  sell to resellers (see *Resellers*).
- **Extra bandwidth**: a price per GB past the plan's monthly bandwidth (0: none). At the start of each
  month, accounts billed by invoice get an invoice for last month's excess, per started GB.
- **Resellers may assign it** (*resellable*): unchanged. Resellers give these plans to their own
  customers.

**Billing an account you already have.** Open **Accounts → the account → Billing** and set **Billed
by: Invoices**, the cycle and the next due date (empty: from today). A **price override** replaces
the plan's price for this account. Accounts created from the order page are billed by invoice from
the start. The other modes are *Not billed here* (free, or billed elsewhere), *WHMCS* and *Stripe
subscription* (the older setup where subscriptions are made in Stripe).

## 5. Payment methods

**Billing → Settings → Payment methods.** Clients see only the methods that are switched on *and*
configured. Rename them if you like ("Card", "UPI, cards & netbanking", "Bank transfer").

### Stripe (cards)

Clients pay on Stripe's own checkout page and can tick **Save my card**. A saved card lets them turn
on **automatic payment**: the card is charged on each due date.

1. In Stripe, open **Developers → API keys**. Copy the **secret key** (`sk_live_…`; `sk_test_…` to try
   it first in test mode). A restricted key works too, if it may write Customers, Checkout Sessions,
   PaymentIntents, PaymentMethods and Refunds.
2. In WPGenie, under **Stripe keys**, paste the secret key.
3. In Stripe, open **Developers → Webhooks → Add endpoint**. The endpoint URL is shown in WPGenie next
   to the keys:

   ```
   https://panel.example.com/api/v1/billing/stripe/webhook
   ```

   Select these events:

   - `checkout.session.completed`: a checkout was paid;
   - `payment_intent.succeeded`: an automatic charge, or a payment method that confirms later;
   - `charge.refunded`: refunds you make in Stripe's dashboard are recorded on the invoice.

   If you also use
   subscriptions created in Stripe (the older setup), add `customer.subscription.created`,
   `customer.subscription.updated`, `customer.subscription.deleted`, `invoice.paid` and
   `invoice.payment_failed`.
4. Copy the endpoint's **signing secret** (`whsec_…`) into WPGenie's **Webhook signing secret**, and
   save.
5. Turn on **Offer card payments**. The card shows *ready* once both secrets are set.

In test mode, pay with Stripe's test card `4242 4242 4242 4242`, any future date and any CVC. Then
check that the invoice turns *paid* by itself. Switch to live keys (and a live webhook, which has its
own signing secret) when it works. Your store currency must be one Stripe accepts.

### Razorpay (UPI, cards, netbanking, wallets)

Each payment is a Razorpay payment link. The client pays on Razorpay's page and comes back to the
invoice.

1. In the Razorpay dashboard, under **Account & Settings → API Keys**, generate a key. Copy the **Key
   ID** (`rzp_test_…` or `rzp_live_…`) and the **Key Secret** into WPGenie.
2. Under **Account & Settings → Webhooks**, add a webhook:
   - URL: the **Webhook URL** WPGenie shows, i.e.
     `https://panel.example.com/api/v1/billing/razorpay/webhook`;
   - Secret: a long random string of your choice;
   - Active events: `payment_link.paid` and `refund.processed`.
3. Paste the same secret into WPGenie's **Webhook secret**, turn on **Offer Razorpay** and save.

The return address (`/api/v1/billing/razorpay/callback`) is set on each link by WPGenie. You don't
configure it. The webhook catches payments whose client closed the page before coming back. Your
store currency must be one your Razorpay account accepts (INR, unless international payments are
enabled).

### Bank transfer (and other offline payments)

Turn on **Offer bank transfer** and write the **instructions**: bank, account name and number,
IBAN/SWIFT or IFSC, and "quote the invoice number". Clients who choose it see them with the invoice
number as the reference.

When the money arrives, open the invoice and click **Record payment**. Give the amount, the method
(bank transfer, cash, cheque, other), the reference and the date. A payment smaller than the balance
leaves the invoice unpaid with what's left. Anything paid beyond the balance becomes the account's
credit.

### Credit

Each account has a **credit** balance. It is filled by payments beyond an invoice's balance, by
refunds to credit, by the unused part of a plan when downgrading, and by staff (**Add credit** on the
account, which e-mails the client). With *Pay new invoices from account credit* (**Reminders &
suspension**, on by default), new invoices take what they can from it. Clients can also apply it to an
invoice themselves.

## 6. Reminders and suspension

**Billing → Settings → Reminders & suspension** shows the timeline, driven by your numbers. Every 15
minutes WPGenie checks every invoice. Each step happens once, however often it runs. Defaults,
counted from the due date (day 0):

| When | What happens | E-mail |
|---|---|---|
| day −7 | The renewal invoice is created (*Create renewal invoices*, in Invoices settings). Credit pays what it can | `invoice.created` |
| day −3 | Friendly reminder, if still unpaid (0: none) | `invoice.reminder` |
| day 0 | Due. For clients who turned on automatic payment, the saved card is charged, and tried again each day while unpaid (*Charge saved cards on the due date*) | `invoice.payment_failed` if declined |
| day 1, 3, 7 | Overdue reminders (a comma-separated list of days) | `invoice.overdue` |
| day 3 | Late fee, if you set one: a fixed amount or a percentage of the balance (default: none) | |
| day 5 | The account is **suspended**: its sites show a "suspended" page, nothing is deleted | `account.suspended` |
| never | **Close the account** after N days (0: never). Its sites are deleted only if you also turn on *Delete the sites of closed accounts* | `account.cancelled` |

Paying (or crediting, or cancelling) the overdue invoices lifts a billing suspension at once, and the
client gets `account.unsuspended`. If WPGenie was down for a while, only the latest reminder due is
sent, not a burst of old ones. **Run now** runs the checks immediately, and the list below it shows
what each run did and any errors.

Accounts not billed by invoice still get reminders (and late fees) for invoices you send them by hand,
but are never suspended or closed by this timeline.

## 7. The order page

Your public order page is:

```
https://panel.example.com/order.html
```

It shows your public plans as cards with a cycle switcher. It asks for contact details and a username
and password, then shows a quote with taxes, a promo code field and the payment methods. Link to it
from your website's pricing page. You can link straight to a plan and cycle:

```
https://panel.example.com/order.html?plan=pro&cycle=annually
```

(`plan` is the plan's ID.) The panel's sign-in page also shows an "Order hosting" link while the store
is open.

Placing an order creates, at once, a **pending** account (no sites until paid), its first user
(signed in straight away), and its first invoice, due that day. The client then pays. Paying
activates the account, billed from that day, and e-mails `account.welcome`. A free or fully discounted
order is active at once. Orders left unpaid for 14 days are cancelled by themselves. A pending client
can sign in, pay, update their billing details and open support tickets, but can't create sites.

**Approving orders yourself.** Turn on *Approve new orders by hand* (**Reminders & suspension**) and
paid orders wait for you under **Billing → Orders**. **Accept** activates an order even if it isn't paid
yet (its invoice stays to be paid). **Cancel** cancels its invoice and closes the pending account. A
payment already made is left for you to refund.

**Promo codes** (**Billing → Promotions**): a percentage or a fixed amount off the plan's price (not
the setup fee), for some plans and cycles or all, between two dates, a limited number of times.
*Recurring* codes also discount every renewal. Otherwise only the first invoice is discounted. Codes
are entered on the order page.

The order page limits each address to 5 orders an hour and refuses what looks automated.

## 8. Resellers

Resellers are businesses that resell your hosting to their own customers under their own name.

1. Create a plan for resellers: its limits are the reseller's **total** allocation (every customer's
   sites count), with prices, and *An order creates: a reseller account*. Make it public to sell it on
   the order page, or give it to accounts yourself.
2. Mark the plans a reseller may hand out to its customers as **Resellers may assign it**. A customer's
   plan must fit within the reseller's, limit by limit.

A reseller signs in to the same panel and sees only its own world. It can:

- create customer accounts and their users, assign them resellable plans, and suspend, unsuspend
  or close them;
- manage its customers' sites like its own;
- sign its customers in with one click, and connect its own billing system with an API token;
- answer its customers' support tickets, and escalate them to you;
- see and pay **its own** invoices in its Billing tab.

**You bill the reseller. The reseller bills its customers.** WPGenie never invoices a reseller's
customer: their Billing tab says they are billed through their provider. A reseller's customers can't
be switched to *billed by invoice*, and invoices can't be created for them. To bill them
automatically, the reseller can connect its own WHMCS with the WPGenie module and an API token of its
reseller user ([integrations/whmcs](../integrations/whmcs)). WHMCS then creates, suspends and closes
the customer accounts as they pay or don't.

If a reseller's account is suspended for an unpaid invoice, its customers' sites are suspended too.

## 9. Burst minute packs

Burst gives busy sites extra capacity, counted in burst minutes. Each plan includes some each month,
and clients billed by invoice can buy more:

1. **Billing → Settings → Burst minutes**: add packs, each with an ID, a number of minutes and a price
   (for example `boost-600`: 600 minutes for 9.00).
2. Clients see **Buy more minutes** in their Billing overview and next to a site's burst settings.
   They choose a pack and pay by any method, or from their credit.

A pack is an invoice like any other. Once it's paid, its minutes are added to the account's burst
credit, exactly once. Bought minutes never expire, and they are used after the plan's monthly minutes.
Sites paused for lack of minutes start bursting again by themselves. (Resellers sell packs to their own
customers from their WHMCS: see the module's *Add burst minutes* button.)

## 10. What clients see

Customers and resellers get a **Billing** tab (the client area):

- **Overview**: the balance due with **Pay now**, credit, current plan with the next due date and price,
  **Change plan**, automatic payment and the saved card, **Cancel service**, and burst packs to buy.
  Pending orders and accounts suspended for billing see a banner leading them to pay.
- **Invoices**: every invoice (never your drafts) with its lines, taxes and payments. Each has **Pay**
  (choose a method, then go to Stripe or Razorpay, or read your bank instructions), **Apply credit** and
  **Print / PDF**. The printable invoice is a clean page with your logo, and the browser's *Save as PDF*
  makes the PDF.
- **Payments**, **Billing details** (name, company, address, country and state, tax ID: these decide
  their taxes) and **E-mails** (every message sent to them).

**Changing plan.** A client can move to any public plan of the same kind (customer or reseller) that
has a price for the chosen cycle. The quote shows the unused part of what they paid as a credit line and
the new plan's price pro rata until their next due date (or a whole new cycle if they change the cycle).
The plan switches when that invoice is paid. A change that costs nothing (a downgrade) applies at once,
and anything left over goes to credit. A client with an overdue invoice must pay it first. Staff can
also move an account to a plan that isn't public.

**Cancelling.** Clients cancel *at the end of the period they paid for*. No renewal invoice is made
after that date. On that day the account is closed (sites deleted only if *Delete the sites of closed
accounts* is on), and they get `account.cancelled`. They can withdraw the cancellation until then.
Staff can also cancel immediately.

**Your side** (**Billing**): the **Overview** shows monthly recurring revenue, income this month against
last, outstanding and overdue amounts, pending orders, a 12-month income chart, recent payments,
overdue invoices (with **Send reminder**) and upcoming renewals. **Invoices** has filters, search,
**New invoice** (a draft or issued now, e-mailed or not), and on each invoice: **Record payment**,
**Refund**, **Send reminder**, **Cancel**. A refund goes back through Stripe or Razorpay, or to the
account's credit. A payment recorded by hand is only marked refunded: send the money back yourself.
**Transactions** lists payments. **Invoices** and **Transactions** both export to CSV for your
accountant (admins).

## 11. Support tickets

The **Support** tab is a help desk for your clients. It works out of the box. Set it up under
**Support → Manage**:

1. **Departments** (e.g. *Billing*, *Technical*): each with a description, an optional **notify
   address** (it gets an e-mail for every new ticket and client reply in that department), and whether
   clients can see it. A *General* department exists from the start. A department with tickets can be
   hidden but not deleted.
2. **Settings** (admins):
   - **Notify** addresses: your team's addresses, e-mailed about every new ticket and client reply.
   - **Close answered tickets after** N days without a reply from the client (default **7**; 0:
     never). The client is e-mailed when it closes.
   - Attachments: how many files per message (default 5; 0 turns attachments off), their size (default
     5 MB each) and allowed types (default: images, PDF, text, logs, CSV, zip). Web pages, SVG and
     programs are always refused.
   - A **Reply-To** address for ticket e-mails.
   - Whether clients can open new tickets.
3. **Canned replies**: saved answers your team inserts with one click.

Clients open tickets from **Support → New ticket** (a department, an optional site, a priority) and
reply in the conversation. Suspended and pending clients can still write. Your team replies, sets the
status (*New, Customer replied, In progress, On hold, Answered, Closed*), the priority, the department
and who handles it, and writes **internal notes** that the client never sees. **Reply & close** answers
and closes at once. A client's reply reopens a closed ticket.

**Resellers' customers.** Their tickets go to their reseller first, not to you. The reseller answers
them, and can **Escalate** a ticket to you with a note, which e-mails your notify addresses. Internal
notes on those tickets are shared between your team and the reseller. The customer still never sees
them.

Clients are e-mailed when a ticket is opened, answered and closed by you (`ticket.opened`,
`ticket.reply`, `ticket.closed`). Your team (or the reseller) is e-mailed about new tickets,
escalations and client replies (`ticket.new_staff`, `ticket.customer_reply`). Replying to these
e-mails doesn't add to the ticket yet: people reply in the panel.

## 12. Logs in your own storage

Logs can be kept in S3-compatible storage (Amazon S3, Cloudflare R2, Backblaze B2, Wasabi,
DigitalOcean Spaces, MinIO…) instead of filling the server's disk. Open the **Logs** tab:

1. Create a bucket and an access key that may put, list, get and delete objects in it (the provider
   hint explains where).
2. Choose the **storage provider**, then fill in the endpoint, region, bucket, **folder in the bucket**
   (default `logs/`), access key ID and secret. **Test connection** writes and deletes a small file.
3. **Save and turn on shipping.**
4. Choose which logs ship. Visitor requests, PHP errors, firewall matches, security events, panel
   activity, background jobs, account activity, e-mail sent (never the message itself), the mail server
   and WPGenie's own log are on by default. Container output is off (detailed and noisy).
5. Retention: how long the bucket keeps logs (default **3 months**; the panel deletes older ones every
   day), and on this server how many old access log files Caddy keeps (default 2) and how big each
   container's log may grow (default 10 MB).

Logs appear in the bucket as `logs/<server>/<kind>/YYYY/MM/DD/HH-….log.gz`, one JSON object per
line. **Archive** in the Logs tab lists a day's files and opens them (zstd files are downloaded,
not shown). Shipping starts from the moment it's turned on: older logs aren't sent. If the storage is
unreachable, logs wait on the server (up to 1 GB by default, oldest dropped first) and are sent once
it's back. With several servers, each ships its own logs with the panel's settings.

## 13. Troubleshooting

**A client paid but the invoice is still unpaid.** Payments are confirmed by webhooks.

- *Stripe*: in Stripe's **Developers → Webhooks**, open your endpoint and look at the event
  deliveries. A `400` means the signing secret in WPGenie doesn't match that endpoint's; a `404`
  means the webhook secret isn't set in WPGenie. Check the URL is your panel's and the events include
  `checkout.session.completed` and `payment_intent.succeeded`. Stripe retries failed deliveries, and you
  can resend one from its page. Each payment counts once, however often it arrives.
- *Razorpay*: check the webhook in **Account & Settings → Webhooks** (URL, `payment_link.paid`, the same
  secret as in WPGenie). Refused deliveries are in the panel's audit log (`razorpay_webhook_refused`).
- A payment in another currency than the invoice's isn't recorded. The account's activity says so:
  record it by hand.
- A payment that arrives for an invoice already paid or cancelled goes to the account's credit.
- If nothing else works, **Record payment** by hand with the gateway's reference.

**E-mails aren't arriving.** Open **Billing → E-mail → Sent e-mail**. *Pending* messages mean sending
is off or not configured (step 1). *Failed* messages show the mail server's error. Fix the settings,
**Send test**, then **Resend**. If they are *sent* but not received, look in the spam folder and
check your domain's SPF and DKIM records for the provider you send through. Billing e-mails go to the
account's billing contact (its e-mail address if none): check it under Accounts → the account → Billing.

**A renewal invoice wasn't made.** The account must be *billed by invoice*, have a next due date,
and have a price for its cycle (or a price override). It must not be cancelled by then. Look at
**Reminders & suspension → the runs list** for errors, or click **Run now**.

**A client was suspended but has paid.** A billing suspension lifts once none of the account's
invoices is overdue. Check for another overdue invoice (an overage or burst pack invoice, for example).
A suspension by an administrator is only lifted by an administrator.

**The order page says no plans are for sale.** Built-in billing must be on, and at least one plan must
be public with a price.
