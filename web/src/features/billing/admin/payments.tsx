import { useState } from "react"
import { BanknoteIcon, CreditCardIcon, InfoIcon, KeyRoundIcon, SmartphoneIcon } from "lucide-react"
import { Skeleton } from "@/components/ui/skeleton"
import { Banner, CopyField, LoadError } from "@/components/app/blocks"
import { notify } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import { queryClient, useApi } from "@/lib/query"
import {
  Advanced, AreaField, ConnState, Labeled, ProviderHint, SECRET_SET, SettingsCard, TextField, ToggleField, billingConfigChanged, grid, saveMethod,
  useInvoicing, type InvoicingSettings,
} from "./common"

// Payment methods: card payments with Stripe (and Stripe's keys, with the
// older subscription setup), Razorpay, bank transfer.

export interface StripeSettings {
  stripe_webhook_secret_set?: boolean
  stripe_secret_key_set?: boolean
  stripe_meter_event?: string
  stripe_prices?: Record<string, string> | null
  stripe_webhook_url?: string
  events?: string[]
  features?: string[]
}
export const STRIPE = "/billing/settings"

export function Payments() {
  const inv = useInvoicing()
  const stripe = useApi<StripeSettings>(STRIPE)
  // Without built-in billing (404) only Stripe's keys are here.
  const missing = inv.error instanceof ApiError && inv.error.status === 404
  if (inv.error && !missing) return <LoadError error={inv.error} retry={() => inv.refetch()} />
  if (stripe.error) return <LoadError error={stripe.error} retry={() => stripe.refetch()} />
  if ((!inv.data && !missing) || !stripe.data) return <Skeleton className="h-64 rounded-2xl" />
  const s = missing ? null : inv.data!
  return (
    <>
      {!s && (
        <Banner tone="info" icon={InfoIcon} title="Built-in billing isn't on this server yet">
          Stripe subscriptions and outgoing webhooks work as before.
        </Banner>
      )}
      {s && <StripeMethodForm s={s} />}
      <StripeKeysForm st={stripe.data} builtIn={!!s} />
      {s && <RazorpaySection />}
      {s && <ManualForm s={s} />}
    </>
  )
}

function StripeMethodForm({ s }: { s: InvoicingSettings }) {
  const st = s.methods?.stripe || {}
  const [enabled, setEnabled] = useState(!!st.enabled)
  const [name, setName] = useState(st.name || "Card")
  return (
    <SettingsCard icon={CreditCardIcon} tint="indigo" title="Card payments with Stripe" onSave={() => saveMethod("stripe", { enabled, name: name.trim() })}>
      <div className="flex flex-wrap items-center gap-2.5">
        <ConnState ok={!!s.stripe_ready} />
        <span className="text-sm text-muted-foreground">
          {s.stripe_ready ? "Clients pay by card on Stripe's page and can save the card." : "Add a secret key and the webhook secret below."}
        </span>
      </div>
      <ToggleField name="enabled" label="Offer card payments" checked={enabled} onChange={setEnabled} />
      <Labeled label="Name clients see">
        <TextField name="name" maxLength={60} value={name} onChange={(e) => setName(e.target.value)} />
      </Labeled>
    </SettingsCard>
  )
}

// StripeKeysForm is Stripe's keys and the older subscription setup (prices
// mapped to plans, metered bandwidth). Secrets are write-only.
function StripeKeysForm({ st, builtIn }: { st: StripeSettings; builtIn: boolean }) {
  const [secretKey, setSecretKey] = useState("")
  const [webhookSecret, setWebhookSecret] = useState("")
  const [prices, setPrices] = useState(
    Object.entries(st.stripe_prices || {})
      .map(([k, v]) => `${k} = ${v}`)
      .join("\n")
  )
  const [meter, setMeter] = useState(st.stripe_meter_event || "")
  return (
    <SettingsCard
      icon={KeyRoundIcon}
      tint="purple"
      title="Stripe keys"
      intro="Secrets are write-only: leave a field empty to keep it."
      ok="Save Stripe keys"
      onSave={async () => {
        const map: Record<string, string> = {}
        for (const line of prices.split("\n")) {
          const [k, v] = line.split("=").map((x) => (x || "").trim())
          if (k && v) map[k] = v
        }
        const body: Record<string, unknown> = { meter_event: meter.trim(), prices: map }
        if (webhookSecret) body.webhook_secret = webhookSecret.trim()
        if (secretKey) body.secret_key = secretKey.trim()
        const saved = await api<StripeSettings>("PUT", STRIPE, body)
        setWebhookSecret("")
        setSecretKey("")
        if (saved) queryClient.setQueryData([STRIPE], saved)
        // Whether cards are ready shows with the methods.
        queryClient.invalidateQueries({ queryKey: ["/billing/invoicing"] })
        billingConfigChanged()
        notify("Stripe settings saved")
      }}
    >
      <div className={grid}>
        <Labeled label="Secret key" help="A restricted key needs write access to Checkout Sessions, Customers, Payment Intents and Refunds.">
          <TextField
            name="secret_key"
            type="password"
            autoComplete="off"
            placeholder={st.stripe_secret_key_set ? SECRET_SET : "sk_… or rk_…"}
            value={secretKey}
            onChange={(e) => setSecretKey(e.target.value)}
          />
        </Labeled>
        <Labeled label="Webhook signing secret">
          <TextField
            name="webhook_secret"
            type="password"
            autoComplete="off"
            placeholder={st.stripe_webhook_secret_set ? SECRET_SET : "whsec_…"}
            value={webhookSecret}
            onChange={(e) => setWebhookSecret(e.target.value)}
          />
        </Labeled>
      </div>
      <div className="flex flex-col gap-2">
        <span className="text-sm font-medium">Webhook URL</span>
        <CopyField text={st.stripe_webhook_url || ""} />
      </div>
      <ProviderHint
        title="Setting up Stripe"
        steps={[
          "In Stripe open Developers → API keys and create a secret (or restricted) key; paste it above.",
          "Open Developers → Webhooks → Add endpoint with the URL above" +
            (builtIn ? ", and the events checkout.session.completed, payment_intent.succeeded and charge.refunded" : "") +
            ". For subscriptions made in Stripe add customer.subscription.created, .updated and .deleted, invoice.paid and invoice.payment_failed too.",
          "Paste the endpoint's signing secret (whsec_…) above.",
        ]}
      />
      <Advanced title="Stripe subscriptions (the older setup)">
        <p className="text-sm text-muted-foreground">Subscriptions created in Stripe become accounts: map each Stripe price to a plan. Metered bandwidth reports usage in MB.</p>
        <div className={grid}>
          <Labeled label="Prices → plans" help="One per line: price_… = plan">
            <AreaField name="prices" rows={3} placeholder="price_123 = pro" value={prices} onChange={(e) => setPrices(e.target.value)} className="font-mono" />
          </Labeled>
          <Labeled label="Meter event name" help="Optional: bandwidth in MB.">
            <TextField name="meter_event" placeholder="wpgenie_bandwidth" value={meter} onChange={(e) => setMeter(e.target.value)} />
          </Labeled>
        </div>
      </Advanced>
    </SettingsCard>
  )
}

// Razorpay starts again from the server after a save (its secrets' state,
// whether it's connected).
function RazorpaySection() {
  const [version, setVersion] = useState(0)
  const inv = useInvoicing()
  if (!inv.data) return null
  return <RazorpayForm key={version} s={inv.data} onSaved={() => setVersion((v) => v + 1)} />
}

function RazorpayForm({ s, onSaved }: { s: InvoicingSettings; onSaved: () => void }) {
  const rp = s.methods?.razorpay || {}
  const [enabled, setEnabled] = useState(!!rp.enabled)
  const [name, setName] = useState(rp.name || "UPI, cards & netbanking")
  const [keyId, setKeyId] = useState(rp.key_id || "")
  const [keySecret, setKeySecret] = useState("")
  const [webhookSecret, setWebhookSecret] = useState("")
  return (
    <SettingsCard
      icon={SmartphoneIcon}
      tint="blue"
      title="Razorpay"
      onSave={async () => {
        const change: Record<string, unknown> = { enabled, name: name.trim(), key_id: keyId.trim() }
        if (keySecret) change.key_secret = keySecret.trim()
        if (webhookSecret) change.webhook_secret = webhookSecret.trim()
        await saveMethod("razorpay", change)
        onSaved()
      }}
    >
      <div className="flex flex-wrap items-center gap-2.5">
        <ConnState ok={!!(rp.key_id && rp.key_secret_set && rp.webhook_secret_set)} />
        <span className="text-sm text-muted-foreground">UPI, cards, netbanking and wallets in India, through a payment link.</span>
      </div>
      <ToggleField name="enabled" label="Offer Razorpay" checked={enabled} onChange={setEnabled} />
      <div className={grid}>
        <Labeled label="Name clients see">
          <TextField name="name" maxLength={60} value={name} onChange={(e) => setName(e.target.value)} />
        </Labeled>
        <Labeled label="Key ID">
          <TextField name="key_id" autoComplete="off" placeholder="rzp_live_…" value={keyId} onChange={(e) => setKeyId(e.target.value)} />
        </Labeled>
        <Labeled label="Key secret">
          <TextField
            name="key_secret"
            type="password"
            autoComplete="off"
            placeholder={rp.key_secret_set ? SECRET_SET : ""}
            value={keySecret}
            onChange={(e) => setKeySecret(e.target.value)}
          />
        </Labeled>
        <Labeled label="Webhook secret">
          <TextField
            name="webhook_secret"
            type="password"
            autoComplete="off"
            placeholder={rp.webhook_secret_set ? SECRET_SET : ""}
            value={webhookSecret}
            onChange={(e) => setWebhookSecret(e.target.value)}
          />
        </Labeled>
      </div>
      {rp.webhook_url && (
        <div className="flex flex-col gap-2">
          <span className="text-sm font-medium">Webhook URL</span>
          <CopyField text={rp.webhook_url} />
        </div>
      )}
      <ProviderHint
        title="Setting up Razorpay"
        steps={[
          "In the Razorpay Dashboard open Account & Settings → API keys and generate a key; paste the Key ID and secret here.",
          "Under Webhooks, add a webhook with the URL above, a secret of your choice (paste it here too), and the events payment_link.paid and refund.processed.",
          "Test mode keys (rzp_test_…) work for trying it out.",
        ]}
      />
    </SettingsCard>
  )
}

function ManualForm({ s }: { s: InvoicingSettings }) {
  const mn = s.methods?.manual || {}
  const [enabled, setEnabled] = useState(mn.enabled ?? true)
  const [name, setName] = useState(mn.name || "Bank transfer")
  const [instructions, setInstructions] = useState(mn.instructions || "")
  return (
    <SettingsCard
      icon={BanknoteIcon}
      tint="green"
      title="Bank transfer"
      onSave={() => saveMethod("manual", { enabled, name: name.trim(), instructions })}
    >
      <ToggleField
        name="enabled"
        label="Offer bank transfer"
        help="Clients see your instructions; you record the payment when it arrives."
        checked={enabled}
        onChange={setEnabled}
      />
      <Labeled label="Name clients see">
        <TextField name="name" maxLength={60} value={name} onChange={(e) => setName(e.target.value)} />
      </Labeled>
      <Labeled label="Instructions" help="Shown with the invoice number to use as the payment reference.">
        <AreaField
          name="instructions"
          rows={5}
          placeholder={"Bank: …\nAccount holder: …\nIBAN / account number: …\nSWIFT / IFSC: …"}
          value={instructions}
          onChange={(e) => setInstructions(e.target.value)}
        />
      </Labeled>
    </SettingsCard>
  )
}
