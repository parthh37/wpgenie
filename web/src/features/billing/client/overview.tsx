import { useId, useState, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { CalendarIcon, CreditCardIcon, LayersIcon, ReceiptIcon, TagIcon, ZapIcon } from "lucide-react"
import { ActionButton, EmptyState, Kpi } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { CancelDialog, PlanChangeDialog, PlanLimits } from "@/features/billing/shared/plan-dialogs"
import { api } from "@/lib/api"
import { fmtNum } from "@/lib/format"
import { cycleOf, fmtDueDate, friendly, listOf, money, useBillingConfig } from "@/lib/money"
import { queryClient } from "@/lib/query"
import { href, navigate } from "@/lib/router"
import { cn } from "@/lib/utils"
import { billingPath } from "../route"
import {
  canBuyBurst, cap, profileKey, reloadBilling, useBurst, type BurstBalance, type ClientCtx, type ClientInvoice, type Profile,
} from "./data"
import { ClientInvoiceTable } from "./parts"
import { useBuyBurst } from "./pay"

// The Overview: what's due, the plan, the saved card, burst minutes and
// the latest invoices.

export function ClientOverview({ ctx }: { ctx: ClientCtx }) {
  const { acct, profile: p } = ctx
  const recent = useQuery({
    queryKey: ["/invoices?limit=5"],
    queryFn: () =>
      api("GET", "/invoices?limit=5")
        .then((r) => listOf<ClientInvoice>(r, "invoices"))
        .catch(() => [] as ClientInvoice[]),
  })
  const burst = useBurst(acct.id)
  const [changing, setChanging] = useState(false)
  const [cancelling, setCancelling] = useState(false)

  if (!p) {
    return (
      <EmptyState icon={ReceiptIcon} tint="mint" title="Billing isn't set up here">
        Your provider hasn't turned on billing in the panel. Contact them about invoices and payments.
      </EmptyState>
    )
  }
  if (recent.isPending || burst.isPending) return <OverviewSkeleton />

  const billed = p.mode === "invoice"
  const open = billed && acct.status === "active" && !p.cancel_at
  const nextAmount = p.upcoming ? p.upcoming.amount : p.recurring_amount
  const nextDate = p.upcoming ? p.upcoming.period_start : p.next_due_at

  return (
    <>
      <div className="mb-4 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <Kpi
          label="Balance due"
          icon={ReceiptIcon}
          value={money(p.balance_due || 0)}
          sub={p.overdue ? "overdue" : p.balance_due ? "to pay" : "nothing to pay"}
          tone={p.overdue ? "bad" : p.balance_due ? "warn" : "ok"}
          onClick={p.balance_due ? () => ctx.payDue().catch((e) => showError(friendly(e))) : undefined}
        />
        <Kpi label="Credit" icon={TagIcon} value={money(p.credit || 0)} sub={p.credit ? "used for your next invoices" : "none"} />
        {billed && nextDate && <Kpi label="Next payment" icon={CalendarIcon} value={money(nextAmount)} sub={`on ${fmtDueDate(nextDate)}`} />}
      </div>

      <div className="mb-4 grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
        <Panel
          label="Your plan"
          title={acct.plan?.name || acct.plan_id}
          action={
            open && (
              <Button variant="tinted" onClick={() => setChanging(true)}>
                <LayersIcon data-icon="inline-start" />
                Change plan
              </Button>
            )
          }
        >
          {billed && p.recurring_amount != null && (
            <p className="text-xl font-bold tracking-[-0.01em] tabular-nums">
              {money(p.recurring_amount)}
              <span className="text-sm font-normal text-muted-foreground"> / {cycleOf(p.cycle).per}</span>
            </p>
          )}
          <PlanLimits plan={acct.plan || { id: acct.plan_id, name: acct.plan_id }} />
          {billed && p.next_due_at && (
            <p className="text-sm text-muted-foreground">{p.cancel_at ? `Ends ${fmtDueDate(p.cancel_at)}.` : `Renews ${fmtDueDate(p.next_due_at)}.`}</p>
          )}
          {!billed && (
            <p className="text-sm text-muted-foreground">
              {p.mode === "whmcs"
                ? "Billed through your provider's billing system."
                : p.mode === "stripe_subscription"
                  ? "Billed by a Stripe subscription."
                  : "Not billed through this panel."}
            </p>
          )}
        </Panel>
        {billed && <PaymentMethod key={String(p.auto_pay)} accountId={acct.id} p={p} />}
        <BurstCard ctx={ctx} b={burst.data ?? null} />
      </div>

      <Section
        title="Recent invoices"
        action={
          <Button variant="link" nativeButton={false} render={<a href={href(billingPath("invoices"))} />}>
            All invoices
          </Button>
        }
      >
        <ClientInvoiceTable list={recent.data ?? []} pay={ctx.pay} />
      </Section>

      {open && (
        <p className="mt-2 text-right text-sm">
          <button type="button" className="text-muted-foreground underline-offset-4 hover:text-danger hover:underline" onClick={() => setCancelling(true)}>
            Cancel my service…
          </button>
        </p>
      )}

      <PlanChangeDialog
        open={changing}
        onOpenChange={setChanging}
        accountId={acct.id}
        planId={acct.plan_id}
        cycle={p.cycle}
        tenant
        onChanged={(res?: unknown) => {
          void reloadBilling()
          // A change with something to pay is an invoice: open it.
          const inv = (res as { invoice?: { id?: number } } | undefined)?.invoice
          if (inv?.id != null) navigate(billingPath("invoices", inv.id))
        }}
      />
      <CancelDialog
        open={cancelling}
        onOpenChange={setCancelling}
        accountId={acct.id}
        name={acct.name}
        nextDue={p.next_due_at ?? undefined}
        onDone={() => void reloadBilling()}
      />
    </>
  )
}

function OverviewSkeleton() {
  return (
    <div aria-busy="true" className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} className="h-24 rounded-2xl" />
        ))}
      </div>
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Skeleton className="h-56 rounded-2xl" />
        <Skeleton className="h-56 rounded-2xl" />
      </div>
      <Skeleton className="h-48 rounded-2xl" />
    </div>
  )
}

// Panel: a card with a small label over a large title, and an action.
function Panel({ label, title, action, children }: { label: ReactNode; title?: ReactNode; action?: ReactNode; children?: ReactNode }) {
  return (
    <section className="flex min-w-0 flex-col gap-3 rounded-2xl bg-card p-5 card-shadow">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <span className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{label}</span>
          {title && <h2 className="mt-0.5 text-[1.375rem] leading-tight font-bold tracking-[-0.02em]">{title}</h2>}
        </div>
        {action}
      </div>
      {children}
    </section>
  )
}

// ---- The saved card and automatic payment ----

function PaymentMethod({ accountId, p }: { accountId: number; p: Profile }) {
  const helpId = useId()
  const [on, setOn] = useState(!!p.auto_pay)
  const [busy, setBusy] = useState(false)
  const card = p.card

  async function toggle(v: boolean) {
    setOn(v)
    setBusy(true)
    try {
      const res = await api<Profile>("PUT", `/accounts/${accountId}/billing/auto-pay`, { auto_pay: v })
      notify(v ? "Invoices are paid with your card automatically" : "Automatic payment is off")
      if (res) queryClient.setQueryData(profileKey(accountId), res)
    } catch (e) {
      showError(friendly(e))
      setOn(!v)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Panel label="Payment method">
      <div className="flex items-center gap-3 rounded-xl bg-muted px-4 py-3">
        <CreditCardIcon className="size-6 shrink-0 text-link" aria-hidden />
        <div className="flex min-w-0 flex-col">
          <strong className="font-semibold">{card ? `${cap(card.brand || "card")} •••• ${card.last4}` : "No card saved"}</strong>
          {card && (
            <span className="text-sm text-muted-foreground">
              Expires {String(card.exp_month).padStart(2, "0")}/{String(card.exp_year).slice(-2)}
            </span>
          )}
        </div>
      </div>
      <label className={cn("flex items-start gap-3", card ? "cursor-pointer" : "cursor-not-allowed")}>
        <Switch checked={on} disabled={!card || busy} onCheckedChange={(v) => toggle(v)} aria-describedby={helpId} className="mt-0.5" />
        <span className={cn("text-sm font-medium", !card && "opacity-60")}>Pay invoices automatically</span>
      </label>
      <p id={helpId} className="text-sm text-muted-foreground">
        {card ? "Your card is charged on each due date; you get a receipt by e-mail." : 'To save a card, pay an invoice by card and tick "Save my card".'}
      </p>
      {card && (
        <div>
          <ActionButton
            variant="destructive"
            run={async () => {
              if (!(await ask("Remove your saved card? Automatic payment turns off; you can pay each invoice yourself.", { ok: "Remove card", danger: true }))) return
              await api("DELETE", `/accounts/${accountId}/billing/card`)
              notify("Card removed")
              await reloadBilling()
            }}
          >
            Remove card
          </ActionButton>
        </div>
      )}
    </Panel>
  )
}

// ---- Burst minutes ----

// Meter: like <meter> with low, high and optimum at the top: red under
// low, orange under high, green above.
function Meter({ value, max, low, high, label }: { value: number; max: number; low: number; high: number; label: string }) {
  const pct = max > 0 ? Math.min(100, Math.max(0, (value / max) * 100)) : 0
  return (
    <div
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={max}
      aria-valuenow={value}
      className="h-2 w-full overflow-hidden rounded-full bg-muted"
    >
      <div
        className={cn("h-full rounded-full transition-[width]", value < low ? "bg-danger-fill" : value < high ? "bg-warning-fill" : "bg-success-fill")}
        style={{ width: `${pct}%` }}
      />
    </div>
  )
}

function BurstCard({ ctx, b }: { ctx: ClientCtx; b: BurstBalance | null }) {
  const { data: cfg } = useBillingConfig()
  const buy = useBuyBurst(ctx.acct.id, ctx.profile, () => void reloadBilling())
  if (!b || !b.allowed) return null
  const included = b.included || 0
  const left = Math.max(0, included - (b.used || 0))
  // Unlimited plans have nothing to buy.
  const canBuy = !b.unlimited && canBuyBurst(ctx.profile, cfg)
  const max = included || 1
  return (
    <Panel
      label="Burst minutes"
      title={b.unlimited ? "Unlimited" : `${fmtNum(b.remaining ?? left + (b.credit || 0))} left`}
      action={
        canBuy && (
          <Button variant="tinted" onClick={buy.open}>
            <ZapIcon data-icon="inline-start" />
            Buy minutes
          </Button>
        )
      }
    >
      {b.unlimited ? (
        <p className="text-sm text-muted-foreground">Your plan includes unlimited burst minutes.</p>
      ) : (
        <>
          <Meter value={left} max={max} low={max * 0.2} high={max * 0.5} label="Monthly burst minutes left" />
          <p className="text-sm text-muted-foreground">
            {`${fmtNum(left)} of ${fmtNum(included)} this month` + (b.credit ? ` · ${fmtNum(b.credit)} bought minutes that never expire` : "") + "."}
          </p>
          {!b.remaining && !canBuy && (
            <p className="text-sm text-warning">Out of burst minutes: sites stay at their normal size until the month starts again.</p>
          )}
        </>
      )}
      {buy.dialog}
    </Panel>
  )
}
