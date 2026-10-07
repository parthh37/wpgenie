import { CheckIcon, ChartColumnIcon, InfoIcon, ReceiptIcon, SendIcon, ShoppingCartIcon, TagIcon, TrendingDownIcon, TrendingUpIcon, TriangleAlertIcon, WalletIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, Banner, BTable, EmptyState, Kpi, LoadError } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { fmtNum } from "@/lib/format"
import { cycleOf, dueText, fmtDueDate, listOf, methodName, money, type BillingConfig } from "@/lib/money"
import { useApi } from "@/lib/query"
import { href, navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { billingPath } from "../route"
import { InvoiceLink, SubLine } from "../shared/invoice-document"
import type { Invoice, Overview, Payment } from "../shared/types"
import { BarChart } from "./bar-chart"
import { INVOICE_FILTER } from "./filters"
import { remindInvoice } from "./invoice-actions"

// The staff Billing overview: the numbers that matter, income by month,
// and what needs doing (overdue invoices, renewals coming up).
export function StaffOverview({ config }: { config: BillingConfig }) {
  const s = useSession()
  const admin = s.atLeast("admin")
  if (config.unavailable) {
    // An older server: what it has (Stripe subscriptions, webhooks) is in Settings.
    return (
      <EmptyState
        icon={ReceiptIcon}
        tint="mint"
        title="Built-in billing isn't on this server yet"
        actions={
          admin && (
            <>
              <Button onClick={() => navigate(billingPath("settings", "payments"))}>Stripe settings</Button>
              <Button variant="tinted" onClick={() => navigate(billingPath("settings", "webhooks"))}>
                Webhooks
              </Button>
            </>
          )
        }
      >
        Invoices, payments and the order page come with an update. Stripe subscriptions and outgoing webhooks work as before.
      </EmptyState>
    )
  }
  return <OverviewBody config={config} />
}

function OverviewBody({ config }: { config: BillingConfig }) {
  const s = useSession()
  const admin = s.atLeast("admin")
  const q = useApi<Overview>("/billing/overview")
  if (q.isError) return <LoadError error={q.error} retry={() => q.refetch()} />
  const o = q.data
  if (!o) return <OverviewSkeleton />

  const month = o.income_this_month || 0
  const last = o.income_last_month || 0
  let change
  if (last > 0) {
    const pct = Math.round(((month - last) / last) * 100)
    const up = pct >= 0
    change = (
      <span className="inline-flex flex-wrap items-center gap-1">
        <span className={up ? "inline-flex items-center gap-0.5 font-semibold text-success" : "inline-flex items-center gap-0.5 font-semibold text-danger"}>
          {up ? <TrendingUpIcon className="size-3.5" /> : <TrendingDownIcon className="size-3.5" />}
          {`${up ? "+" : ""}${pct}%`}
        </span>
        <span>{` vs ${money(last)} last month`}</span>
      </span>
    )
  } else {
    change = last === 0 && month === 0 ? "Nothing received yet" : "Nothing last month"
  }
  const showInvoices = (status: string) => {
    INVOICE_FILTER.status = status
    navigate(billingPath("invoices"))
  }

  const overdue = listOf<Invoice>(o.overdue_invoices, "invoices")
  const recent = listOf<Payment>(o.recent_payments, "payments")
  const upcoming = listOf<NonNullable<Overview["upcoming_renewals"]>[number]>(o.upcoming_renewals, "renewals")

  return (
    <>
      {config.enabled === false && (
        <Banner
          tone="info"
          icon={InfoIcon}
          title="Built-in billing is off"
          actions={admin && <Button onClick={() => navigate(billingPath("settings"))}>Set up billing</Button>}
        >
          Turn it on to send invoices, take card and bank payments and run reminders for the accounts you bill. Your order page opens with it.
        </Banner>
      )}

      <div className="mb-4 grid grid-cols-1 gap-3 min-[480px]:grid-cols-2 lg:grid-cols-3">
        <Kpi label="Monthly recurring revenue" icon={TrendingUpIcon} value={money(o.mrr)} sub={`${fmtNum(o.billed_accounts || 0)} billed account${o.billed_accounts === 1 ? "" : "s"}`} />
        <Kpi label="Income this month" icon={WalletIcon} value={money(month)} sub={change} />
        <Kpi
          label="Outstanding"
          icon={ReceiptIcon}
          value={money(o.outstanding)}
          sub={`${fmtNum(o.unpaid_count || 0)} unpaid invoice${o.unpaid_count === 1 ? "" : "s"}`}
          onClick={() => showInvoices("unpaid")}
        />
        <Kpi
          label="Overdue"
          icon={TriangleAlertIcon}
          tone={o.overdue_count ? "bad" : undefined}
          value={money(o.overdue_total)}
          sub={`${fmtNum(o.overdue_count || 0)} invoice${o.overdue_count === 1 ? "" : "s"} past due`}
          onClick={() => showInvoices("overdue")}
        />
        <Kpi
          label="Pending orders"
          icon={ShoppingCartIcon}
          tone={o.pending_orders ? "warn" : undefined}
          value={fmtNum(o.pending_orders || 0)}
          sub="waiting for payment or approval"
          href={s.atLeast("operator") ? billingPath("orders") : undefined}
        />
        <Kpi label="Client credit" icon={TagIcon} value={money(o.credit_total)} sub="held on accounts" />
      </div>

      <Section icon={ChartColumnIcon} tint="mint" title="Income, last 12 months" description="payments received, less refunds">
        <BarChart points={listOf(o.income_by_month, "months")} title="Income by month, last 12 months" />
      </Section>

      <div className="grid items-start gap-4 lg:grid-cols-2 2xl:grid-cols-3">
        <Section title="Overdue invoices" className="mb-0">
          <BTable
            cols={["Invoice", "Late", { label: "Balance", num: true }, ""]}
            rows={overdue.map((inv) => ({
              key: inv.id,
              onOpen: () => navigate(billingPath("invoices", inv.id)),
              cells: [
                <>
                  <InvoiceLink invoice={inv} />
                  <SubLine>{inv.account_name || `#${inv.account_id}`}</SubLine>
                </>,
                <span className="text-danger">{dueText({ status: inv.status || "", due_at: inv.due_at || undefined }) || "late"}</span>,
                money(inv.balance),
                admin ? (
                  <ActionButton size="sm" run={() => remindInvoice(inv)}>
                    <SendIcon data-icon="inline-start" />
                    Remind
                  </ActionButton>
                ) : null,
              ],
            }))}
            empty={
              <p className="flex items-center gap-1.5 py-2 text-sm text-muted-foreground">
                <CheckIcon className="size-4 text-success" />
                Nothing overdue. Well done.
              </p>
            }
          />
        </Section>
        <Section title="Recent payments" className="mb-0">
          <BTable
            cols={["Client", "Method", { label: "Amount", num: true }]}
            rows={recent.map((p) => ({
              key: p.id,
              onOpen: p.invoice_id ? () => navigate(billingPath("invoices", p.invoice_id)) : undefined,
              cells: [
                <>
                  {p.invoice_id ? (
                    <a href={href(billingPath("invoices", p.invoice_id))} className="font-medium">
                      {p.account_name || (p.account_id ? `#${p.account_id}` : "–")}
                    </a>
                  ) : (
                    p.account_name || (p.account_id ? `#${p.account_id}` : "–")
                  )}
                  <SubLine>{fmtDueDate(p.at)}</SubLine>
                </>,
                methodName(p.gateway || p.method),
                money(p.amount),
              ],
            }))}
            empty={<p className="py-2 text-sm text-muted-foreground">Payments show up here as they come in.</p>}
          />
        </Section>
        <Section title="Upcoming renewals" className="mb-0">
          <BTable
            cols={["Client", "Due", { label: "Amount", num: true }]}
            rows={upcoming.map((r, i) => ({
              key: `${r.account_id}-${i}`,
              cells: [
                <>
                  <a href={href("/accounts/" + r.account_id)} className="font-medium">
                    {r.account_name || `#${r.account_id}`}
                  </a>
                  <SubLine>{[r.plan_name || r.plan_id || "", r.cycle ? cycleOf(r.cycle).label.toLowerCase() : ""].filter(Boolean).join(" · ")}</SubLine>
                </>,
                fmtDueDate(r.next_due_at || r.due_at),
                money(r.amount ?? r.recurring_amount),
              ],
            }))}
            empty={<p className="py-2 text-sm text-muted-foreground">No renewals in the coming weeks.</p>}
          />
        </Section>
      </div>
    </>
  )
}

function OverviewSkeleton() {
  return (
    <div aria-busy="true" aria-label="Loading…">
      <div className="mb-4 grid grid-cols-1 gap-3 min-[480px]:grid-cols-2 lg:grid-cols-3">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="h-[104px] rounded-2xl" />
        ))}
      </div>
      <Skeleton className="mb-4 h-[300px] rounded-2xl" />
    </div>
  )
}
