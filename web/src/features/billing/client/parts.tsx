import { CreditCardIcon, ReceiptIcon } from "lucide-react"
import { ActionButton, BTable, EmptyState } from "@/components/app/blocks"
import { StatusPill, type Tone } from "@/components/app/status"
import { InvoiceLink } from "@/features/billing/shared/invoice-document"
import { navigate } from "@/lib/router"
import { dueText, fmtDueDate, INVOICE_STATES, invoiceState, money } from "@/lib/money"
import { cn } from "@/lib/utils"
import { billingPath } from "../route"
import type { ClientInvoice } from "./data"

// Pieces the client area's screens share: an invoice's state as a pill,
// and the table of invoices with a Pay button on each unpaid one.

const INVOICE_TONES: Record<string, Tone> = { paid: "ok", unpaid: "warn", overdue: "bad", draft: "neutral", cancelled: "neutral" }

export function InvoicePill({ invoice, className }: { invoice: ClientInvoice; className?: string }) {
  const st = invoiceState(invoice)
  const refunded = st === "refunded" || st === "partially_refunded"
  return (
    <StatusPill status={st} tone={INVOICE_TONES[st] ?? "neutral"} className={cn(refunded && "bg-primary/12 text-link", className)}>
      {INVOICE_STATES[st] || st.replace(/_/g, " ")}
    </StatusPill>
  )
}

// Inside a card: no second card around the empty state.
export const flatEmpty = "bg-transparent py-6 shadow-none!"

export function ClientInvoiceTable({ list, pay }: { list: ClientInvoice[]; pay: (inv: ClientInvoice) => void }) {
  return (
    <BTable
      caption="Invoices"
      cols={["Invoice", "Date", "Due", { label: "Total", num: true }, "Status", { label: <span className="sr-only">Actions</span> }]}
      rows={list.map((inv) => {
        const due = dueText(inv)
        return {
          key: inv.id,
          onOpen: () => navigate(billingPath("invoices", inv.id)),
          className: cn(invoiceState(inv) === "overdue" && "bg-danger-fill/8 [&>td:first-child]:shadow-[inset_3px_0_0_var(--danger-fill)]"),
          cells: [
            <InvoiceLink invoice={inv} />,
            fmtDueDate(inv.issued_at),
            <>
              {fmtDueDate(inv.due_at)}
              {due && <span className="block text-xs text-muted-foreground">{due}</span>}
            </>,
            money(inv.total),
            <InvoicePill invoice={inv} />,
            inv.status === "unpaid" ? (
              <div className="flex justify-end">
                <ActionButton variant="default" size="sm" run={() => pay(inv)}>
                  <CreditCardIcon data-icon="inline-start" />
                  Pay {money(inv.balance)}
                </ActionButton>
              </div>
            ) : null,
          ],
        }
      })}
      empty={
        <EmptyState icon={ReceiptIcon} title="No invoices yet" className={flatEmpty}>
          Your invoices appear here as soon as they're issued, and we e-mail each one.
        </EmptyState>
      }
    />
  )
}
