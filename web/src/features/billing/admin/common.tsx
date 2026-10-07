import { createContext, useContext, useId, useState, type FormEvent, type ReactNode } from "react"
import { InfoIcon, type LucideIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldContent, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { NativeSelect } from "@/components/ui/native-select"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { LoadError } from "@/components/app/blocks"
import type { Tint } from "@/components/app/icon-tile"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import { currencySymbol, friendly } from "@/lib/money"
import { invalidate, queryClient, useApi } from "@/lib/query"
import { cn } from "@/lib/utils"

// What the admin views share: the invoicing settings (load, strip, merge,
// save), a card whose form saves one part of them, labelled fields,
// switches, segmented choices, countries. The legacy billing-admin.js and
// billing.js helpers (settingsForm, toggle, field, fieldError…).

// ---- The invoicing settings (GET/PUT /billing/invoicing) ----

export interface Currency {
  code?: string
  symbol?: string
  decimals?: number
}
export interface Company {
  name?: string
  address?: string
  email?: string
  phone?: string
  tax_id?: string
  website?: string
}
export interface InvoiceOptions {
  prefix?: string
  next_number?: number
  number_on?: "issue" | "payment" | string
  days_before_due?: number
  footer?: string
  terms_url?: string
}
export interface TaxOptions {
  enabled?: boolean
  inclusive?: boolean
  exempt_with_tax_id?: boolean
}
export interface LateFee {
  type: "none" | "fixed" | "percent" | string
  amount: number
}
export interface Automation {
  reminder_days_before?: number
  overdue_reminder_days?: number[]
  suspend_after_days?: number
  terminate_after_days?: number
  terminate_deletes_sites?: boolean
  late_fee?: LateFee
  late_fee_after_days?: number
  auto_apply_credit?: boolean
  orders_need_approval?: boolean
  autocharge?: boolean
}
export interface StripeMethod {
  enabled?: boolean
  name?: string
}
export interface RazorpayMethod {
  enabled?: boolean
  name?: string
  key_id?: string
  key_secret?: string
  webhook_secret?: string
  key_secret_set?: boolean
  webhook_secret_set?: boolean
  webhook_url?: string
}
export interface ManualMethod {
  enabled?: boolean
  name?: string
  instructions?: string
}
export interface Methods {
  stripe?: StripeMethod
  razorpay?: RazorpayMethod
  manual?: ManualMethod
}
export interface BurstPack {
  id: string
  minutes: number
  price: number
}
export interface InvoicingSettings {
  enabled?: boolean
  currency?: Currency
  company?: Company
  invoice?: InvoiceOptions
  tax?: TaxOptions
  automation?: Automation
  methods?: Methods
  burst_packs?: BurstPack[]
  stripe_ready?: boolean
  [k: string]: unknown
}

export const INVOICING = "/billing/invoicing"
export const useInvoicing = () => useApi<InvoicingSettings>(INVOICING)

// WithInvoicing loads the settings, then shows what needs them.
export function WithInvoicing({ children }: { children: (s: InvoicingSettings) => ReactNode }) {
  const q = useInvoicing()
  if (q.error) return <LoadError error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-64 rounded-2xl" />
  return <>{children(q.data)}</>
}

// invoicingBody is the settings without their output-only fields: the API
// rejects them back.
export function invoicingBody(s: InvoicingSettings): InvoicingSettings {
  const body = JSON.parse(JSON.stringify(s)) as InvoicingSettings
  delete body.stripe_ready
  for (const m of Object.values(body.methods || {}) as Array<Record<string, unknown> | undefined>) {
    if (!m) continue
    for (const k of Object.keys(m)) if (k.endsWith("_set") || k === "webhook_url") delete m[k]
  }
  return body
}

// The settings as last loaded or saved (what a change merges into).
async function currentInvoicing() {
  return queryClient.getQueryData<InvoicingSettings>([INVOICING]) ?? (await api<InvoicingSettings>("GET", INVOICING))
}

// The store's configuration the rest of the panel sees (currency,
// methods…) changed: reload it.
export const billingConfigChanged = () => invalidate("/billing/config")

// saveInvoicing merges a change into the settings and saves them; objects
// merge one level deep ({company: {name}} keeps the company's other fields).
export async function saveInvoicing(change: Partial<InvoicingSettings>) {
  const body = invoicingBody(await currentInvoicing())
  const b = body as Record<string, unknown>
  for (const [k, v] of Object.entries(change)) {
    b[k] = v && typeof v === "object" && !Array.isArray(v) ? { ...((b[k] as object) || {}), ...v } : v
  }
  const saved = await api<InvoicingSettings | null>("PUT", INVOICING, body)
  queryClient.setQueryData([INVOICING], saved || body)
  billingConfigChanged()
  notify("Settings saved")
  return saved || body
}

// saveMethod changes one payment method, keeping its other fields.
export async function saveMethod(id: "stripe" | "razorpay" | "manual", change: Record<string, unknown>) {
  const methods = invoicingBody(await currentInvoicing()).methods || {}
  return saveInvoicing({ methods: { ...methods, [id]: { ...(methods[id] || {}), ...change } } })
}

// fieldError is a failure about one field: forms focus it.
export const fieldError = (name: string, message: string) => new ApiError(message, 400, { field: name })

export const SECRET_SET = "set (unchanged if empty)"

// ---- SettingsCard: a card whose form saves one part of the settings ----

export function SettingsCard({
  icon,
  tint,
  title,
  intro,
  onSave,
  ok = "Save",
  children,
  disabled,
}: {
  icon?: LucideIcon
  tint?: Tint
  title: ReactNode
  intro?: ReactNode
  onSave: (form: HTMLFormElement) => unknown | Promise<unknown>
  ok?: ReactNode
  children: ReactNode
  disabled?: boolean
}) {
  const [busy, setBusy] = useState(false)
  const [status, setStatus] = useState("")
  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const form = e.currentTarget
    if (!form.reportValidity()) return
    form.querySelectorAll("[aria-invalid]").forEach((x) => x.removeAttribute("aria-invalid"))
    setBusy(true)
    setStatus("Saving…")
    try {
      await onSave(form)
      setStatus("Saved.")
    } catch (err) {
      setStatus("")
      showError(friendly(err))
      const name = err instanceof ApiError ? (err.data.field as string | undefined) : undefined
      const el = name ? (form.elements.namedItem(name.split(".").pop()!) as HTMLElement | null) : null
      if (el && "focus" in el) {
        el.setAttribute("aria-invalid", "true")
        el.focus()
      }
    } finally {
      setBusy(false)
    }
  }
  return (
    <Section icon={icon} tint={tint} title={title} description={intro}>
      <form onSubmit={submit} onInput={() => setStatus("")} className="flex flex-col gap-5">
        {children}
        <div className="flex flex-wrap items-center justify-end gap-3">
          <span className="text-sm text-muted-foreground" aria-live="polite">
            {status}
          </span>
          <Button type="submit" disabled={busy || disabled}>
            {ok}
          </Button>
        </div>
      </form>
    </Section>
  )
}

// ---- Labelled fields ----
// <Labeled label help><TextField …/></Labeled>: the control takes the
// label's id and is described by the help.

const FieldIds = createContext<{ id: string; helpId?: string } | null>(null)
const useFieldIds = () => useContext(FieldIds)

export function Labeled({ label, help, children, className }: { label: ReactNode; help?: ReactNode; children: ReactNode; className?: string }) {
  const id = useId()
  const helpId = help ? `${id}-help` : undefined
  return (
    <Field className={cn("min-w-0", className)}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <FieldIds.Provider value={{ id, helpId }}>{children}</FieldIds.Provider>
      {help && <FieldDescription id={helpId}>{help}</FieldDescription>}
    </Field>
  )
}

const ids = (f: { id: string; helpId?: string } | null) => (f ? { id: f.id, "aria-describedby": f.helpId } : {})

export function TextField(props: React.ComponentProps<"input">) {
  return <Input {...ids(useFieldIds())} {...props} />
}
export function AreaField({ className, ...props }: React.ComponentProps<"textarea">) {
  return <Textarea {...ids(useFieldIds())} className={cn("field-sizing-fixed resize-y", className)} {...props} />
}
export function SelectField({ className, ...props }: React.ComponentProps<"select">) {
  return <NativeSelect {...ids(useFieldIds())} className={cn("w-full", className)} {...(props as React.ComponentProps<typeof NativeSelect>)} />
}

// An input with a word after it ("7 days before due").
export function SuffixField({ suffix, ...props }: { suffix: ReactNode } & React.ComponentProps<"input">) {
  return (
    <InputGroup>
      <InputGroupInput {...ids(useFieldIds())} {...props} />
      <InputGroupAddon align="inline-end">
        <InputGroupText>{suffix}</InputGroupText>
      </InputGroupAddon>
    </InputGroup>
  )
}

// MoneyField and PercentField: controlled text, read back with toMinor()
// and toRate() from @/lib/money.
export function MoneyField({ value, onValue, ...props }: { value: string; onValue: (v: string) => void } & Omit<React.ComponentProps<"input">, "value">) {
  return (
    <InputGroup>
      <InputGroupAddon>
        <InputGroupText>{currencySymbol()}</InputGroupText>
      </InputGroupAddon>
      <InputGroupInput {...ids(useFieldIds())} inputMode="decimal" autoComplete="off" value={value} onChange={(e) => onValue(e.target.value)} {...props} />
    </InputGroup>
  )
}
export function PercentField({ value, onValue, ...props }: { value: string; onValue: (v: string) => void } & Omit<React.ComponentProps<"input">, "value">) {
  return (
    <InputGroup>
      <InputGroupInput {...ids(useFieldIds())} inputMode="decimal" autoComplete="off" value={value} onChange={(e) => onValue(e.target.value)} {...props} />
      <InputGroupAddon align="inline-end">
        <InputGroupText>%</InputGroupText>
      </InputGroupAddon>
    </InputGroup>
  )
}
// A rate (hundredths of a percent) as a percent field shows it ("18").
export const rateText = (r: number | null | undefined) => (r == null ? "" : String(r / 100))

// ---- ToggleField: an on/off switch with its label and optional help ----

export function ToggleField({
  name,
  label,
  checked,
  onChange,
  help,
  disabled,
  className,
}: {
  name?: string
  label: ReactNode
  checked: boolean
  onChange: (v: boolean) => void
  help?: ReactNode
  disabled?: boolean
  className?: string
}) {
  const id = useId()
  return (
    <Field orientation="horizontal" className={className}>
      <Switch id={id} name={name} checked={checked} disabled={disabled} onCheckedChange={(v) => onChange(!!v)} aria-describedby={help ? `${id}-help` : undefined} />
      <FieldContent>
        <FieldLabel htmlFor={id}>{label}</FieldLabel>
        {help && <FieldDescription id={`${id}-help`}>{help}</FieldDescription>}
      </FieldContent>
    </Field>
  )
}

// ---- Segmented: a few radios as one control ----

export function Segmented<T extends string>({
  name,
  label,
  value,
  onChange,
  options,
}: {
  name: string
  label: string
  value: T
  onChange: (v: T) => void
  options: Array<[T, ReactNode]>
}) {
  return (
    <div role="radiogroup" aria-label={label} className="inline-flex w-fit max-w-full flex-wrap gap-1 rounded-full bg-muted p-1">
      {options.map(([v, text]) => (
        <label
          key={v}
          className={cn(
            "relative inline-flex cursor-pointer items-center rounded-full px-3.5 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground has-focus-visible:ring-[3px] has-focus-visible:ring-ring/50",
            v === value && "bg-card text-foreground shadow-sm"
          )}
        >
          <input type="radio" name={name} value={v} checked={v === value} onChange={() => onChange(v)} className="sr-only" />
          {text}
        </label>
      ))}
    </div>
  )
}

// A label over a group of controls that aren't one input.
export function GroupLabel({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cn("text-sm font-medium", className)}>{children}</span>
}

// ---- CheckList: ticks in a wrapping list ----

export function CheckList({
  name,
  items,
  selected,
  onChange,
  small,
}: {
  name: string
  items: Array<[value: string, label: ReactNode]>
  selected: string[]
  onChange: (v: string[]) => void
  small?: boolean
}) {
  return (
    <div className="flex flex-wrap gap-x-5 gap-y-2.5">
      {items.map(([v, text]) => (
        <Label key={v} className={cn("cursor-pointer font-normal", small && "text-xs")}>
          <Checkbox
            name={name}
            value={v}
            checked={selected.includes(v)}
            onCheckedChange={(c) => onChange(c ? [...selected, v] : selected.filter((x) => x !== v))}
          />
          {text}
        </Label>
      ))}
    </div>
  )
}

// ---- Hints ----

// LiveHint: a sentence that follows the form as it changes.
export function LiveHint({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <p aria-live="polite" className={cn("rounded-xl bg-muted px-3.5 py-2.5 text-sm text-muted-foreground", className)}>
      {children}
    </p>
  )
}

// ProviderHint: how to set a provider up, folded away.
export function ProviderHint({ title, steps }: { title: ReactNode; steps: ReactNode[] }) {
  return (
    <details className="group rounded-xl bg-muted px-3.5 py-2.5 text-sm">
      <summary className="flex cursor-pointer items-center gap-2 font-medium text-link select-none">
        <InfoIcon className="size-4" />
        {title}
      </summary>
      <ol className="mt-2 ml-5 list-decimal space-y-1 text-muted-foreground">
        {steps.map((x, i) => (
          <li key={i}>{x}</li>
        ))}
      </ol>
    </details>
  )
}

export function Advanced({ title, children }: { title: ReactNode; children: ReactNode }) {
  return (
    <details className="rounded-xl ring-1 ring-border">
      <summary className="cursor-pointer px-3.5 py-2.5 text-sm font-medium select-none">{title}</summary>
      <div className="flex flex-col gap-4 px-3.5 pt-1 pb-3.5">{children}</div>
    </details>
  )
}

export function SubHeading({ children }: { children: ReactNode }) {
  return <h3 className="mt-1 text-[0.9375rem] font-semibold">{children}</h3>
}

export const grid = "grid gap-4 sm:grid-cols-2"
export const grid3 = "grid gap-4 sm:grid-cols-3"

// ---- Countries: the panel's list ----

export { countryList, countryName } from "@/lib/countries"

// A status capsule for a connection: green when set up, orange when not.
export function ConnState({ ok, yes = "Connected", no = "Needs keys" }: { ok: boolean; yes?: string; no?: string }) {
  return (
    <span
      className={cn(
        "inline-flex h-6 items-center gap-1.5 rounded-full px-2.5 text-xs font-semibold before:size-1.5 before:rounded-full before:bg-current",
        ok ? "bg-success-fill/16 text-success" : "bg-warning-fill/16 text-warning"
      )}
    >
      {ok ? yes : no}
    </span>
  )
}
