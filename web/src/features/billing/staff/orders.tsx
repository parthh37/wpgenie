import { useState } from "react"
import { CheckIcon, ExternalLinkIcon, ShoppingCartIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, Chips, EmptyState, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { INVOICE_STATES, cycleOf, listOf, money, type BillingConfig } from "@/lib/money"
import { useApi } from "@/lib/query"
import { href } from "@/lib/router"
import { useSession } from "@/lib/session"
import { BillingPill, InvoiceLink, SubLine } from "../shared/invoice-document"
import { refreshBilling } from "../shared/refresh"
import type { Order } from "../shared/types"
import { ORDER_FILTER } from "./filters"

const STATUSES: Array<[string, string]> = [
  ["pending", "Waiting"],
  ["active", "Activated"],
  ["cancelled", "Cancelled"],
]

// Orders from the order page: waiting ones to accept or cancel.
export function StaffOrders({ config }: { config: BillingConfig }) {
  const admin = useSession().atLeast("admin")
  const [status, setStatus] = useState(ORDER_FILTER.status)
  const q = useApi<Order[] | { orders: Order[] }>(`/orders?status=${status}`)
  const list = listOf<Order>(q.data, "orders")
  const pending = status === "pending"

  const accept = async (o: Order) => {
    const unpaid = o.invoice_status && o.invoice_status !== "paid"
    if (
      !(await ask(
        `Activate ${o.account_name || "this account"} now? ` +
          (unpaid ? "Its invoice isn't paid yet; it stays open for the client to pay." : "The client can create sites straight away."),
        { ok: "Activate account" }
      ))
    )
      return
    await api("POST", `/orders/${o.id}/accept`)
    notify("Order accepted")
    refreshBilling(o.account_id)
  }
  const cancel = async (o: Order) => {
    if (
      !(await ask(`Cancel the order from ${o.account_name || "this client"}? Its invoice is cancelled and the pending account is closed.`, {
        ok: "Cancel order",
        danger: true,
      }))
    )
      return
    await api("POST", `/orders/${o.id}/cancel`)
    notify("Order cancelled")
    refreshBilling(o.account_id)
  }

  return (
    <Section contentClassName="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Chips
          label="Show orders"
          items={STATUSES.map(([k, t]) => [k, t])}
          current={status}
          onPick={(k) => {
            ORDER_FILTER.status = k
            setStatus(k)
          }}
        />
        <Button variant="tinted" nativeButton={false} render={<a href="/order.html" target="_blank" rel="noopener" />}>
          <ExternalLinkIcon data-icon="inline-start" />
          Your order page
        </Button>
      </div>
      {q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="shadow-none" />
      ) : !q.data ? (
        <div aria-busy="true" className="flex flex-col gap-2">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-10 rounded-xl" />
          ))}
        </div>
      ) : (
        <BTable
          caption="Orders"
          cols={["Placed", "Client", "Plan", { label: "Total", num: true }, "Invoice", "From", ""]}
          rows={list.map((o) => ({
            key: o.id,
            cells: [
              <span className="whitespace-nowrap">{fmtTime(o.created_at)}</span>,
              <>
                <a href={href("/accounts/" + o.account_id)} className="font-medium">
                  {o.account_name || `#${o.account_id}`}
                </a>
                {o.email && <SubLine>{o.email}</SubLine>}
              </>,
              `${o.plan_name || o.plan_id} · ${cycleOf(o.cycle).label.toLowerCase()}`,
              money(o.total),
              <span className="inline-flex flex-wrap items-center gap-1.5">
                {o.invoice_id ? <InvoiceLink invoice={{ id: o.invoice_id, number: o.invoice_number }} /> : "–"}
                {o.invoice_status && <BillingPill state={o.invoice_status}>{INVOICE_STATES[o.invoice_status]}</BillingPill>}
              </span>,
              <span className="text-sm whitespace-nowrap">{o.ip || "–"}</span>,
              admin && pending ? (
                <span className="flex justify-end gap-1.5">
                  <ActionButton size="sm" variant="default" run={() => accept(o)}>
                    <CheckIcon data-icon="inline-start" />
                    Accept
                  </ActionButton>
                  <ActionButton size="sm" variant="destructive" run={() => cancel(o)}>
                    Cancel
                  </ActionButton>
                </span>
              ) : null,
            ],
          }))}
          empty={
            <EmptyState icon={ShoppingCartIcon} tint="orange" title={pending ? "No orders waiting" : "Nothing here"} className="shadow-none">
              {pending
                ? "New orders from your order page wait here until they're paid" +
                  (config.orders_need_approval ? " and you approve them." : ". Paid orders activate by themselves.")
                : "Orders move here once they're activated or cancelled."}
            </EmptyState>
          }
        />
      )}
    </Section>
  )
}
