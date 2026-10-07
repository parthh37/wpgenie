import { useState } from "react"
import { Building2Icon, CoinsIcon, FileTextIcon, LandmarkIcon } from "lucide-react"
import { NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ChoiceCard, LoadError, SubNav } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { showError } from "@/components/app/toaster"
import { useBillingConfig } from "@/lib/money"
import { href } from "@/lib/router"
import { useSession } from "@/lib/session"
import { billingPath, type BillingRoute } from "../route"
import { Automation } from "./automation"
import { BurstPacks } from "./burst"
import {
  AreaField, GroupLabel, Labeled, LiveHint, SelectField, SettingsCard, SuffixField, TextField, ToggleField, grid, grid3, saveInvoicing, WithInvoicing,
  type InvoicingSettings,
} from "./common"
import { Payments } from "./payments"
import { Webhooks } from "./webhooks"

// Billing settings: company, numbering, taxes, reminders and suspension,
// payment methods, burst minute packs, outgoing webhooks. Each part is a
// sub-section at #/billing/settings/<part>.

const SETTINGS_SECTIONS: Array<[key: string, label: string]> = [
  ["", "Company"],
  ["invoices", "Invoices"],
  ["taxes", "Taxes"],
  ["automation", "Reminders & suspension"],
  ["payments", "Payment methods"],
  ["burst", "Burst minutes"],
  ["webhooks", "Webhooks"],
]
// Older addresses for the same parts.
const ALIASES: Record<string, string> = { methods: "payments", company: "" }

export function Settings({ route }: { route: BillingRoute }) {
  const s = useSession()
  const cfg = useBillingConfig()
  if (!s.atLeast("admin")) return <LoadError error={new Error("Billing settings are for administrators.")} />
  if (cfg.error) return <LoadError error={cfg.error} retry={() => cfg.refetch()} />
  if (!cfg.data) return <Skeleton className="h-64 rounded-2xl" />
  const id = ALIASES[route.id] ?? route.id
  // Without built-in billing only Stripe and webhooks have settings.
  const section = SETTINGS_SECTIONS.some(([k]) => k === id) ? id : cfg.data.unavailable ? "payments" : ""
  return (
    <>
      <SubNav
        label="Settings sections"
        current={section}
        items={SETTINGS_SECTIONS.map(([key, label]) => ({ key, label, href: billingPath("settings", key) }))}
      />
      <div>
        {section === "" && <CompanySettings />}
        {section === "invoices" && <InvoiceSettings />}
        {section === "taxes" && <TaxSettings />}
        {section === "automation" && <Automation />}
        {section === "payments" && <Payments />}
        {section === "burst" && <BurstPacks />}
        {section === "webhooks" && <Webhooks />}
      </div>
    </>
  )
}

// ---- Company and currency ----

const CURRENCIES = ["USD", "EUR", "GBP", "INR", "CAD", "AUD", "NZD", "SGD", "AED", "CHF", "SEK", "NOK", "DKK", "PLN", "CZK", "JPY", "BRL", "MXN", "ZAR"]

function CompanySettings() {
  return <WithInvoicing>{(s) => <CompanyForms s={s} />}</WithInvoicing>
}

function CompanyForms({ s }: { s: InvoicingSettings }) {
  const c = s.company || {}
  const cur = s.currency || {}
  const [enabled, setEnabled] = useState(!!s.enabled)
  const [busy, setBusy] = useState(false)
  const [company, setCompany] = useState({
    name: c.name || "",
    email: c.email || "",
    phone: c.phone || "",
    website: c.website || "",
    tax_id: c.tax_id || "",
    address: c.address || "",
  })
  const set = (k: keyof typeof company) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => setCompany({ ...company, [k]: e.target.value })
  const [code, setCode] = useState(cur.code || "USD")
  const [symbol, setSymbol] = useState(cur.symbol || "")
  const [decimals, setDecimals] = useState(String(cur.decimals ?? 2))

  let example = ""
  try {
    const f = new Intl.NumberFormat(undefined, {
      style: "currency",
      currency: code,
      minimumFractionDigits: Number(decimals),
      maximumFractionDigits: Number(decimals),
    })
    example = `Prices look like ${f.format(1234.5)}.`
  } catch {
    // A code Intl doesn't know (or decimals out of range): no example.
  }

  return (
    <>
      <Section>
        <ToggleField
          name="enabled"
          label="Built-in billing"
          help="Invoices for the accounts billed by invoice, card and bank payments, reminders, and your public order page."
          checked={enabled}
          disabled={busy}
          onChange={async (on) => {
            if (
              !on &&
              !(await ask(
                "Turn off built-in billing? No invoices or reminders are sent and the order page closes. Existing invoices stay; clients can still see them.",
                { ok: "Turn off", danger: true }
              ))
            )
              return
            setEnabled(on)
            setBusy(true)
            try {
              await saveInvoicing({ enabled: on })
            } catch (e) {
              showError(e)
              setEnabled(!on)
            } finally {
              setBusy(false)
            }
          }}
        />
      </Section>
      <SettingsCard
        icon={Building2Icon}
        tint="blue"
        title="Your company"
        intro="Printed at the top of invoices and in the footer of e-mails."
        onSave={() =>
          saveInvoicing({
            company: Object.fromEntries(Object.entries(company).map(([k, v]) => [k, v.trim()])),
          })
        }
      >
        <div className={grid}>
          <Labeled label="Company name">
            <TextField name="name" required autoComplete="organization" value={company.name} onChange={set("name")} />
          </Labeled>
          <Labeled label="E-mail" help="Where clients reply about invoices.">
            <TextField name="email" type="email" value={company.email} onChange={set("email")} />
          </Labeled>
          <Labeled label="Phone">
            <TextField name="phone" type="tel" value={company.phone} onChange={set("phone")} />
          </Labeled>
          <Labeled label="Website">
            <TextField name="website" type="url" placeholder="https://" value={company.website} onChange={set("website")} />
          </Labeled>
          <Labeled label="Tax ID" help="Your VAT, GST or company number.">
            <TextField name="tax_id" value={company.tax_id} onChange={set("tax_id")} />
          </Labeled>
          <Labeled label="Address">
            <AreaField name="address" rows={4} value={company.address} onChange={set("address")} />
          </Labeled>
        </div>
      </SettingsCard>
      <SettingsCard
        icon={CoinsIcon}
        tint="green"
        title="Currency"
        intro="One currency for the whole store. Set it before the first invoice: amounts aren't converted when it changes."
        onSave={() => saveInvoicing({ currency: { code, symbol: symbol.trim(), decimals: Number(decimals) } })}
      >
        <div className={grid3}>
          <Labeled label="Currency">
            <SelectField
              name="code"
              value={code}
              onChange={(e) => {
                const next = e.target.value
                setCode(next)
                const f = new Intl.NumberFormat("en", { style: "currency", currency: next })
                setSymbol(f.formatToParts(0).find((p) => p.type === "currency")?.value || next)
                setDecimals(String(f.resolvedOptions().maximumFractionDigits))
              }}
            >
              {[...new Set([...CURRENCIES, cur.code || "USD"])].map((x) => (
                <NativeSelectOption key={x} value={x}>
                  {x}
                </NativeSelectOption>
              ))}
            </SelectField>
          </Labeled>
          <Labeled label="Symbol">
            <TextField name="symbol" maxLength={5} value={symbol} onChange={(e) => setSymbol(e.target.value)} />
          </Labeled>
          <Labeled label="Decimals">
            <TextField name="decimals" type="number" min={0} max={3} value={decimals} onChange={(e) => setDecimals(e.target.value)} />
          </Labeled>
        </div>
        {example && <LiveHint>{example}</LiveHint>}
      </SettingsCard>
    </>
  )
}

// ---- Invoices and numbering ----

function InvoiceSettings() {
  return <WithInvoicing>{(s) => <InvoiceForm s={s} />}</WithInvoicing>
}

function InvoiceForm({ s }: { s: InvoicingSettings }) {
  const inv = s.invoice || {}
  const [prefix, setPrefix] = useState(inv.prefix ?? "INV-")
  const [next, setNext] = useState(String(inv.next_number || 1))
  const [days, setDays] = useState(String(inv.days_before_due ?? 7))
  const [numberOn, setNumberOn] = useState(inv.number_on || "issue")
  const [terms, setTerms] = useState(inv.terms_url || "")
  const [footer, setFooter] = useState(inv.footer || "")
  return (
    <SettingsCard
      icon={FileTextIcon}
      tint="indigo"
      title="Invoices and numbering"
      onSave={() =>
        saveInvoicing({
          invoice: {
            prefix,
            next_number: Number(next),
            number_on: numberOn,
            days_before_due: Number(days),
            terms_url: terms.trim(),
            footer,
          },
        })
      }
    >
      <div className={grid3}>
        <Labeled label="Number prefix">
          <TextField name="prefix" maxLength={20} value={prefix} onChange={(e) => setPrefix(e.target.value)} />
        </Labeled>
        <Labeled label="Next number" help="Numbers have no gaps; raise it to continue an old series.">
          <TextField name="next_number" type="number" min={1} value={next} onChange={(e) => setNext(e.target.value)} />
        </Labeled>
        <Labeled label="Create renewal invoices" help="So clients have time to pay before the due date.">
          <SuffixField suffix="days before due" name="days_before_due" type="number" min={0} max={60} value={days} onChange={(e) => setDays(e.target.value)} />
        </Labeled>
      </div>
      <LiveHint>{`The next invoice will be ${prefix}${String(Number(next) || 1).padStart(6, "0")}.`}</LiveHint>
      <div role="radiogroup" aria-label="Give invoices their number" className="flex flex-col gap-2.5">
        <GroupLabel>Give invoices their number</GroupLabel>
        <div className="grid gap-2.5 sm:grid-cols-2">
          <ChoiceCard name="number_on" value="issue" title="When they're issued" checked={numberOn === "issue"} onChange={setNumberOn}>
            The usual way.
          </ChoiceCard>
          <ChoiceCard name="number_on" value="payment" title="When they're paid" checked={numberOn === "payment"} onChange={setNumberOn}>
            Unpaid ones are proforma invoices; required in some countries so numbers only go to real sales.
          </ChoiceCard>
        </div>
      </div>
      <div className={grid}>
        <Labeled label="Terms of service URL" help="Clients accept them when they order.">
          <TextField name="terms_url" type="url" placeholder="https://" value={terms} onChange={(e) => setTerms(e.target.value)} />
        </Labeled>
        <Labeled label="Invoice footer" help="e.g. bank details or a thank-you.">
          <AreaField name="footer" rows={3} value={footer} onChange={(e) => setFooter(e.target.value)} />
        </Labeled>
      </div>
    </SettingsCard>
  )
}

// ---- Taxes ----

function TaxSettings() {
  return <WithInvoicing>{(s) => <TaxForm s={s} />}</WithInvoicing>
}

function TaxForm({ s }: { s: InvoicingSettings }) {
  const t = s.tax || {}
  const [enabled, setEnabled] = useState(!!t.enabled)
  const [inclusive, setInclusive] = useState(t.inclusive ? "yes" : "no")
  const [exempt, setExempt] = useState(!!t.exempt_with_tax_id)
  return (
    <SettingsCard
      icon={LandmarkIcon}
      tint="brown"
      title="Taxes"
      onSave={() => saveInvoicing({ tax: { enabled, inclusive: inclusive === "yes", exempt_with_tax_id: exempt } })}
    >
      <ToggleField name="enabled" label="Charge tax" help="Using your tax rules, by where each client is." checked={enabled} onChange={setEnabled} />
      <div role="radiogroup" aria-label="Your prices" className="flex flex-col gap-2.5">
        <GroupLabel>Your prices</GroupLabel>
        <div className="grid gap-2.5 sm:grid-cols-2">
          <ChoiceCard name="inclusive" value="no" title="Don't include tax" checked={inclusive === "no"} onChange={setInclusive}>
            Tax is added on top: a 100.00 plan with 18% tax costs 118.00.
          </ChoiceCard>
          <ChoiceCard name="inclusive" value="yes" title="Include tax" checked={inclusive === "yes"} onChange={setInclusive}>
            The price is what clients pay: a 100.00 plan with 18% tax is 84.75 plus 15.25 tax.
          </ChoiceCard>
        </div>
      </div>
      <ToggleField
        name="exempt_with_tax_id"
        label="No tax for clients with a tax ID"
        help="For businesses that account for the tax themselves (EU reverse charge)."
        checked={exempt}
        onChange={setExempt}
      />
      <p className="text-sm">
        <a href={href(billingPath("taxes"))}>Edit the tax rules →</a>
      </p>
    </SettingsCard>
  )
}

