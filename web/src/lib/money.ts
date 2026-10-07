import { useQuery } from "@tanstack/react-query"
import { api, ApiError } from "@/lib/api"

// Billing's shared vocabulary (the legacy billing.js helpers): the store's
// configuration, money, rates, due dates and the words for API values.
// Amounts are integer minor units as the API has them (11800 is 118.00):
// only divided to be shown, multiplied back from inputs.

export interface BillingConfig {
  unavailable?: boolean
  enabled?: boolean
  currency?: { code?: string; symbol?: string; decimals?: number }
  company?: { name?: string }
  company_name?: string
  methods?: Array<{ id: string; name?: string; description?: string; enabled?: boolean }> | Record<string, { enabled?: boolean; name?: string; description?: string }>
  tax_inclusive?: boolean
  [k: string]: unknown
}

// The last configuration loaded: money() formats in its currency.
let BILLING: BillingConfig | null = null

export async function loadBillingConfig(): Promise<BillingConfig> {
  try {
    BILLING = await api<BillingConfig>("GET", "/billing/config")
  } catch (e) {
    if (!(e instanceof ApiError) || e.status !== 404) throw e
    BILLING = { unavailable: true }
  }
  FORMATS.clear()
  return BILLING
}

// useBillingConfig loads (and caches) the store's configuration; render
// money only once it's loaded (data != undefined).
export const useBillingConfig = () =>
  useQuery({ queryKey: ["/billing/config"], queryFn: loadBillingConfig, staleTime: 5 * 60_000 })

export function currency() {
  const c = BILLING?.currency || {}
  return { code: c.code || "USD", symbol: c.symbol || "", decimals: Number.isInteger(c.decimals) ? (c.decimals as number) : 2 }
}

export const companyName = () => BILLING?.company?.name || BILLING?.company_name || ""

// billingMethods: the payment methods clients may choose.
export function billingMethods(): Array<{ id: string; name?: string; description?: string }> {
  const m = BILLING?.methods
  if (Array.isArray(m)) return m.filter((x) => x.enabled !== false)
  return Object.entries(m || {})
    .filter(([, v]) => v && v.enabled !== false)
    .map(([id, v]) => ({ id, ...v }))
}

type Fmt = { format: (v: number) => string; formatToParts: (v: number) => Intl.NumberFormatPart[] }
const FORMATS = new Map<string, Fmt>()

function moneyFormat(compact: boolean): Fmt {
  const c = currency()
  const key = `${c.code}:${c.decimals}:${compact ? 1 : 0}`
  let f = FORMATS.get(key)
  if (!f) {
    const opts: Intl.NumberFormatOptions = compact
      ? { style: "currency", currency: c.code, notation: "compact", maximumFractionDigits: 1 }
      : { style: "currency", currency: c.code, minimumFractionDigits: c.decimals, maximumFractionDigits: c.decimals }
    try {
      // "$", not "US$": the store has one currency, so the short symbol is clear.
      f = new Intl.NumberFormat(undefined, { ...opts, currencyDisplay: "narrowSymbol" })
    } catch {
      try {
        f = new Intl.NumberFormat(undefined, opts)
      } catch {
        // A code Intl doesn't know: the symbol and the number.
        f = { format: (v) => `${c.symbol || c.code + " "}${v.toFixed(compact ? 0 : c.decimals)}`, formatToParts: () => [] }
      }
    }
    FORMATS.set(key, f)
  }
  return f
}

// money(11800) is "$118.00" in the store's currency; compact: "$1.2K".
export const money = (minor: number | null | undefined, compact = false) =>
  minor == null || Number.isNaN(minor) ? "–" : moneyFormat(compact).format(minor / 10 ** currency().decimals)

export function currencySymbol() {
  const part = moneyFormat(false).formatToParts(0).find((p) => p.type === "currency")
  return part?.value || currency().symbol || currency().code
}

// fromMinor is an amount as an input shows it ("118.00").
export const fromMinor = (minor: number | null | undefined | "") =>
  minor == null || minor === "" ? "" : (minor / 10 ** currency().decimals).toFixed(currency().decimals)

// toMinor reads an amount typed by a person ("1,234.50", "1.234,50", "$ 12"):
// minor units, null when empty, NaN when it isn't a number.
export function toMinor(text: string | number | null | undefined): number | null {
  let s = String(text ?? "").trim().replace(/[\s\u00a0']/g, "")
  if (!s) return null
  if (s.includes(",") && s.includes(".")) {
    s = s.lastIndexOf(",") > s.lastIndexOf(".") ? s.replace(/\./g, "").replace(",", ".") : s.replace(/,/g, "")
  } else {
    s = s.replace(",", ".")
  }
  s = s.replace(/[^\d.-]/g, "")
  const n = s === "" || s === "-" ? NaN : Number(s)
  return Number.isFinite(n) ? Math.round(n * 10 ** currency().decimals) : NaN
}

// Rates are hundredths of a percent (1800 is 18%).
export const fmtRate = (r: number) => `${Number((r / 100).toFixed(3))}%`
export function toRate(text: string | null | undefined) {
  const n = Number(String(text ?? "").trim().replace(",", ".").replace("%", ""))
  return String(text ?? "").trim() && Number.isFinite(n) ? Math.round(n * 100) : NaN
}

// Due dates are days: shown in UTC so midnight isn't the day before.
export const fmtDueDate = (t: string | number | Date | null | undefined) =>
  t ? new Date(t).toLocaleDateString([], { dateStyle: "medium", timeZone: "UTC" }) : "–"
export const dateInput = (t: string | null | undefined) => (t ? new Date(t).toISOString().slice(0, 10) : "")
export const fromDateInput = (v: string) => (v ? new Date(v + "T00:00:00Z").toISOString() : null)
export const todayInput = (plusDays = 0) => new Date(Date.now() + plusDays * 864e5).toISOString().slice(0, 10)
// Count calendar days, not 24-hour spans, so "due 21 Sept" is "today" all that day.
const utcDay = (d: Date) => Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate())
export const daysUntil = (t: string | number | Date) => Math.round((utcDay(new Date(t)) - utcDay(new Date())) / 864e5)

export function monthLabel(ym: string, long = false) {
  const [y, m] = String(ym).split("-").map(Number)
  return new Date(Date.UTC(y, (m || 1) - 1, 1)).toLocaleDateString([], { month: long ? "long" : "short", year: long ? "numeric" : undefined, timeZone: "UTC" })
}

// ---- Words for the API's values ----

export const CYCLES = [
  { id: "monthly", months: 1, label: "Monthly", per: "month" },
  { id: "quarterly", months: 3, label: "Every 3 months", per: "3 months" },
  { id: "semiannually", months: 6, label: "Every 6 months", per: "6 months" },
  { id: "annually", months: 12, label: "Yearly", per: "year" },
  { id: "biennially", months: 24, label: "Every 2 years", per: "2 years" },
  { id: "triennially", months: 36, label: "Every 3 years", per: "3 years" },
]
export const cycleOf = (id: string) => CYCLES.find((c) => c.id === id) || { id, months: 1, label: id || "–", per: id || "" }

const METHOD_NAMES: Record<string, string> = { stripe: "Card", razorpay: "Razorpay", manual: "Bank transfer", bank: "Bank transfer", cash: "Cash", cheque: "Cheque", other: "Other", credit: "Account credit" }
export const methodName = (m: string | null | undefined) => (m && METHOD_NAMES[m]) || m || "–"

export const INVOICE_KINDS: Record<string, string> = { order: "New order", renewal: "Renewal", plan_change: "Plan change", overage: "Bandwidth overage", manual: "Invoice", burst_topup: "Burst minutes" }
export const INVOICE_STATES: Record<string, string> = { draft: "Draft", unpaid: "Unpaid", overdue: "Overdue", paid: "Paid", cancelled: "Cancelled", refunded: "Refunded", partially_refunded: "Partly refunded" }

export interface InvoiceLike {
  id: number
  number?: string
  status: string
  overdue?: boolean
}
// An unpaid invoice past its due date is overdue (the API derives it).
export const invoiceState = (inv: InvoiceLike) => (inv.status === "unpaid" && inv.overdue ? "overdue" : inv.status)
export const invoiceTitle = (inv: InvoiceLike) => inv.number || `Proforma #${inv.id}`

// dueText says when an unpaid invoice is due: "in 3 days", "5 days late".
export function dueText(inv: { status: string; due_at?: string }) {
  if (inv.status !== "unpaid" || !inv.due_at) return ""
  const d = daysUntil(inv.due_at)
  if (d > 1) return `in ${d} days`
  if (d === 1) return "tomorrow"
  if (d === 0) return "today"
  return d === -1 ? "1 day late" : `${-d} days late`
}
// dueLine is dueText in a sentence: "due in 3 days", "5 days late".
export const dueLine = (inv: { status: string; due_at?: string }) => (inv.due_at && daysUntil(inv.due_at) < 0 ? dueText(inv) : `due ${dueText(inv)}`)

// listOf is a list response, bare or wrapped ({invoices: […], counts}).
export function listOf<T>(res: unknown, key: string): T[] {
  if (Array.isArray(res)) return res as T[]
  const r = res as Record<string, unknown> | null
  return ((r && (r[key] || r.items)) as T[]) || []
}

// friendly turns a route the server doesn't have (an older server, or a
// part of billing not installed) into words; other errors are as they are.
export function friendly(err: unknown) {
  if (err instanceof ApiError && (err.status === 405 || (err.status === 404 && /^not found$/i.test(err.message || "")))) {
    return new ApiError("This isn't available on this server yet (it may need an update).", err.status, err.data)
  }
  return err
}
