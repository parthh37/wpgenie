// CONTRACT (owner: billing-core agent). An invoice as a document (staff
// invoice screen and the client area), its payments, and a link to it.
import type { ReactNode } from "react"
import { Badge } from "@/components/ui/badge"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { BTable } from "@/components/app/blocks"
import { INVOICE_STATES, fmtDueDate, fmtRate, invoiceState, invoiceTitle, methodName, money, useBillingConfig } from "@/lib/money"
import { href } from "@/lib/router"
import { cn } from "@/lib/utils"
import { billingPath } from "../route"
import { countryName } from "@/lib/countries"
import type { Invoice, InvoiceItem } from "./types"

export type { Invoice } from "./types"

// The words and colour for where an invoice stands (overdue is derived).
export const stateOf = (inv: Pick<Invoice, "id" | "status" | "overdue">) => invoiceState({ id: inv.id, status: inv.status ?? "", overdue: inv.overdue })
export const titleOf = (inv: Pick<Invoice, "id" | "number">) => invoiceTitle({ id: inv.id, number: inv.number, status: "" })

const PILL: Record<string, string> = {
  paid: "bg-success-fill/16 text-success",
  unpaid: "bg-warning-fill/16 text-warning",
  pending: "bg-warning-fill/16 text-warning",
  overdue: "bg-danger-fill/16 text-danger",
  refunded: "bg-primary/12 text-primary",
  partially_refunded: "bg-primary/12 text-primary",
}

// BillingPill is a state in a tinted capsule (bpill).
export function BillingPill({ state, children, className }: { state: string; children?: ReactNode; className?: string }) {
  return (
    <Badge
      variant="secondary"
      className={cn("gap-1.5 font-semibold before:size-1.5 before:rounded-full before:bg-current", PILL[state] ?? "text-muted-foreground", className)}
    >
      {children ?? (INVOICE_STATES[state] || state.replace(/_/g, " "))}
    </Badge>
  )
}

export function InvoicePill({ invoice, className }: { invoice: Pick<Invoice, "id" | "status" | "overdue">; className?: string }) {
  const st = stateOf(invoice)
  return <BillingPill state={st} className={className}>{INVOICE_STATES[st] || st}</BillingPill>
}

// SubLine is a second, quieter line in a cell.
export const SubLine = ({ children, className }: { children: ReactNode; className?: string }) => (
  <span className={cn("block text-xs text-muted-foreground", className)}>{children}</span>
)

// A promotion is an item with a negative amount and also the "discount"
// total: show it once, as the line under the subtotal, named after it.
export const notDiscount = (it: InvoiceItem) => it.kind !== "discount"
export const discountLabel = (items: InvoiceItem[] | undefined) =>
  (items || []).filter((it) => !notDiscount(it)).map((it) => it.description).join(", ") || "Discount"

// InvoiceLink goes to an invoice's page (#/billing/invoices/<id>).
export function InvoiceLink({ invoice, children, className }: { invoice: Pick<Invoice, "id" | "number">; children?: ReactNode; className?: string }) {
  return (
    <a href={href(billingPath("invoices", invoice.id))} className={cn("font-medium", className)}>
      {children ?? titleOf(invoice)}
    </a>
  )
}

const STAMP: Record<string, string> = {
  paid: "Paid",
  overdue: "Overdue",
  cancelled: "Cancelled",
  draft: "Draft",
  refunded: "Refunded",
  partially_refunded: "Part refunded",
}
const STAMP_TONE: Record<string, string> = {
  paid: "text-success",
  overdue: "text-danger",
  refunded: "text-primary",
  partially_refunded: "text-primary",
}

interface Company {
  name?: string
  address?: string
  tax_id?: string
}

// InvoiceDocument shows an invoice like the paper one: who from, who to,
// the lines, the taxes and totals, and a stamp saying where it stands.
export function InvoiceDocument({ invoice: inv, className }: { invoice: Invoice; className?: string }) {
  const { data: config } = useBillingConfig()
  const st = stateOf(inv)
  const stamp = STAMP[st]
  const addr = inv.billing_address || {}
  const company = (config?.company || {}) as Company
  const companyName = company.name || config?.company_name || ""
  const taxLines = inv.tax_lines || []

  const totals: Array<[ReactNode, ReactNode, ("total" | "grand")?]> = [["Subtotal", money(inv.subtotal)]]
  if (inv.discount) totals.push([discountLabel(inv.items), "−" + money(inv.discount)])
  for (const t of taxLines) totals.push([`${t.name} ${fmtRate(t.rate)}`, money(t.amount)])
  totals.push(["Total", money(inv.total), "total"])
  if (inv.credit_applied) totals.push(["Credit applied", "−" + money(inv.credit_applied)])
  if (inv.amount_paid) totals.push(["Paid", "−" + money(inv.amount_paid)])
  if (inv.amount_refunded) totals.push(["Refunded", money(inv.amount_refunded)])
  if (inv.status !== "cancelled" && inv.status !== "draft") totals.push(["Balance due", money(inv.balance), "grand"])

  const meta: Array<[string, string]> = [
    ["Issued", fmtDueDate(inv.issued_at)],
    ["Due", fmtDueDate(inv.due_at)],
  ]
  // period_end is the next period's start: show the last day covered, as
  // the lines, e-mails and the printed invoice do.
  if (inv.period_start && inv.period_end) {
    const last = Math.max(new Date(inv.period_start).getTime(), new Date(inv.period_end).getTime() - 864e5)
    meta.push(["Period", `${fmtDueDate(inv.period_start)} – ${fmtDueDate(last)}`])
  }
  if (inv.paid_at) meta.push(["Paid", fmtDueDate(inv.paid_at)])

  const billTo = [addr.name || inv.account_name, addr.company, ...(addr.lines || []), addr.country ? countryName(addr.country) : ""].filter(Boolean).join("\n")

  return (
    <article
      aria-label={`Invoice ${titleOf(inv)}`}
      className={cn("relative min-w-0 overflow-hidden rounded-3xl bg-card px-5 pt-6 pb-7 text-card-foreground card-shadow sm:px-9 sm:pt-8 sm:pb-9", className)}
    >
      <header className="mb-5 flex flex-wrap justify-between gap-x-8 gap-y-4 border-b-2 border-foreground pb-5">
        <div className="min-w-0">
          <div className="font-heading text-lg font-bold tracking-[-0.01em]">{companyName || "Your provider"}</div>
          {company.address && <div className="text-sm whitespace-pre-line text-muted-foreground">{company.address}</div>}
          {company.tax_id && <div className="text-xs text-muted-foreground">Tax ID {company.tax_id}</div>}
        </div>
        <div className="sm:text-right">
          <div className="font-heading text-2xl leading-none font-extrabold tracking-[-0.02em] uppercase sm:text-[1.75rem]">
            {inv.number ? "Invoice" : "Proforma invoice"}
          </div>
          <div className="mt-1 font-semibold text-muted-foreground tabular-nums">{titleOf(inv)}</div>
        </div>
      </header>

      <div className="mb-5 flex flex-wrap justify-between gap-x-8 gap-y-4">
        <div className="min-w-0">
          <h3 className="mb-1 text-[0.7rem] font-bold tracking-[0.08em] text-muted-foreground uppercase">Bill to</h3>
          <div className="text-sm whitespace-pre-line">{billTo}</div>
          {addr.tax_id && <div className="text-xs text-muted-foreground">Tax ID {addr.tax_id}</div>}
        </div>
        <dl className="grid grid-cols-[max-content_auto] content-start gap-x-5 gap-y-1 text-sm">
          {meta.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="text-right font-semibold tabular-nums">{v}</dd>
            </div>
          ))}
        </dl>
      </div>

      <Table>
        <TableHeader>
          <TableRow className="border-b-[1.5px] hover:bg-transparent">
            <TableHead scope="col">Description</TableHead>
            <TableHead scope="col" className="text-right">Qty</TableHead>
            <TableHead scope="col" className="text-right">Unit price</TableHead>
            <TableHead scope="col" className="text-right">Amount</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(inv.items || []).filter(notDiscount).map((it, i) => (
            <TableRow key={it.id ?? i} className={cn("hover:bg-transparent", it.kind === "credit" && "text-success")}>
              <TableCell className="align-top whitespace-normal">
                {it.description}
                {it.taxable === false && taxLines.length > 0 && <SubLine>not taxed</SubLine>}
              </TableCell>
              <TableCell className="text-right align-top tabular-nums">{String(it.quantity ?? 1)}</TableCell>
              <TableCell className="text-right align-top tabular-nums">{money(it.unit_price)}</TableCell>
              <TableCell className="text-right align-top tabular-nums">{money(it.amount)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

      {/* The stamp sits in the space beside the totals. */}
      <div className="flex flex-wrap-reverse items-center justify-between gap-4">
        {stamp ? (
          <div
            aria-hidden
            className={cn(
              "pointer-events-none mt-4 ml-2 -rotate-12 rounded-lg border-[3px] border-current px-3 py-0.5 text-xl font-extrabold tracking-[0.12em] uppercase opacity-75 select-none",
              STAMP_TONE[st] ?? "text-muted-foreground"
            )}
          >
            {stamp}
          </div>
        ) : (
          <span />
        )}
        <dl className="mt-4 ml-auto w-full flex-none text-sm tabular-nums sm:w-[330px]">
          {totals.map(([k, v, cls], i) => (
            <div
              key={i}
              className={cn(
                "flex justify-between gap-4 px-2.5 py-1",
                cls === "total" && "mt-1 border-t border-border pt-2 font-bold",
                cls === "grand" && "mt-1.5 rounded-xl bg-muted py-2 text-base font-bold"
              )}
            >
              <dt className={cls ? "text-foreground" : "text-muted-foreground"}>{k}</dt>
              <dd>{v}</dd>
            </div>
          ))}
        </dl>
      </div>
      {config?.tax_inclusive && taxLines.length > 0 && <p className="mt-3 text-sm text-muted-foreground">Prices include tax.</p>}
      {inv.notes && (
        <div className="mt-6 rounded-r-lg border-l-[3px] border-border bg-muted/60 px-4 py-3">
          <h3 className="mb-1 text-[0.7rem] font-bold tracking-[0.08em] text-muted-foreground uppercase">Notes</h3>
          <p className="text-sm whitespace-pre-line">{inv.notes}</p>
        </div>
      )}
    </article>
  )
}

// COMPACT lets a table fit a side column: tighter cells that wrap.
export const COMPACT = "[&_td]:px-2 [&_td]:whitespace-normal [&_td:first-child]:whitespace-nowrap [&_th]:px-2"

// PaymentsTable is an invoice's payments, with what was refunded of each.
export function PaymentsTable({ invoice, className }: { invoice: Invoice; className?: string }) {
  return (
    <BTable
      className={cn(COMPACT, className)}
      cols={["Date", "Method", "Reference", { label: "Amount", num: true }]}
      rows={(invoice.payments || []).map((p) => ({
        key: p.id,
        cells: [
          fmtDueDate(p.at),
          methodName(p.gateway),
          <span className="whitespace-normal [overflow-wrap:anywhere]">{p.reference || "–"}</span>,
          <>
            {money(p.amount)}
            {p.refunded ? <SubLine>{money(p.refunded)} refunded</SubLine> : null}
          </>,
        ],
      }))}
      empty={<p className="py-2 text-sm text-muted-foreground">No payments yet.</p>}
    />
  )
}
