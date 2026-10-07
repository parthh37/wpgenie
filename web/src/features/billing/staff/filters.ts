// The staff lists' filters, kept while you move around Billing (to an
// invoice and back), as the legacy panel kept them.

export interface InvoiceFilter {
  status: string // "", unpaid, overdue, paid, draft, cancelled, refunded
  q: string
  from: string
  to: string
}
export const INVOICE_FILTER: InvoiceFilter = { status: "", q: "", from: "", to: "" }

export const INVOICE_CHIPS: Array<[string, string]> = [
  ["", "All"],
  ["unpaid", "Unpaid"],
  ["overdue", "Overdue"],
  ["paid", "Paid"],
  ["draft", "Drafts"],
  ["cancelled", "Cancelled"],
  ["refunded", "Refunded"],
]

// invoiceQuery is GET /invoices's query for a filter (overdue is a flag,
// not a status), plus extras (limit, before, account).
export function invoiceQuery(f: Partial<InvoiceFilter>, extra: Record<string, string | number | null | undefined> = {}) {
  const q = new URLSearchParams()
  if (f.status === "overdue") q.set("overdue", "1")
  else if (f.status) q.set("status", f.status)
  for (const k of ["q", "from", "to"] as const) if (f[k]) q.set(k, f[k]!)
  for (const [k, v] of Object.entries(extra)) if (v != null && v !== "") q.set(k, String(v))
  return q.toString()
}

export interface TxFilter {
  method: string
  from: string
  to: string
}
export const TX_FILTER: TxFilter = { method: "", from: "", to: "" }

export const ORDER_FILTER = { status: "pending" }

// csvHref is an export's address for a date range.
export const csvHref = (path: string, f: { from: string; to: string }) =>
  `/api/v1${path}?` + new URLSearchParams(Object.entries({ from: f.from, to: f.to }).filter(([, v]) => v))
