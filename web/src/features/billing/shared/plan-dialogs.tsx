// CONTRACT (owner: billing-core agent). Changing an account's plan (with a
// quote) and cancelling it: used by staff (account billing) and tenants
// (client area). Plan limits in words.
import { useEffect, useState, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { CheckIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { ChoiceCard } from "@/components/app/blocks"
import { notify } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import { fmtNum } from "@/lib/format"
import { CYCLES, cycleOf, fmtDueDate, fmtRate, money, useBillingConfig } from "@/lib/money"
import { invalidate, useApi } from "@/lib/query"
import { cn } from "@/lib/utils"
import { BillingDialog, DialogForm, LabeledField } from "./dialog-form"
import { discountLabel, notDiscount, titleOf } from "./invoice-document"
import type { Plan, PlanChangeResult, Quote } from "./types"

export type { Plan, PlanChangeResult, Quote } from "./types"

export const planPrice = (p: Plan, cycle: string) => p.prices?.[cycle] || null
export const planCycles = (p: Plan) => CYCLES.filter((c) => planPrice(p, c.id))

// planLimits is a plan's headline limits in plain words.
export function planLimits(p: Plan): string[] {
  const n = (v: number | undefined, unit: string) => (v ? `${fmtNum(v)} ${unit}` : `Unlimited ${unit}`)
  return [
    n(p.max_sites, p.max_sites === 1 ? "site" : "sites"),
    p.disk_mb ? `${p.disk_mb % 1024 === 0 ? `${fmtNum(p.disk_mb / 1024)} GB` : `${fmtNum(p.disk_mb)} MB`} disk` : "Unlimited disk",
    p.bandwidth_gb ? `${fmtNum(p.bandwidth_gb)} GB bandwidth / month` : "Unlimited bandwidth",
  ]
}

// PlanLimits lists them, each with a check.
export function PlanLimits({ plan, className }: { plan: Plan; className?: string }) {
  return (
    <ul className={cn("flex flex-col gap-1.5 text-sm", className)}>
      {planLimits(plan).map((x) => (
        <li key={x} className="flex items-center gap-2">
          <CheckIcon aria-hidden className="size-4 shrink-0 text-success" strokeWidth={2.5} />
          {x}
        </li>
      ))}
    </ul>
  )
}

// QuoteLines shows a quote (plan change or order): lines, subtotal,
// discount, taxes, total.
export function QuoteLines({ quote: q, totalLabel = "Due now", className }: { quote: Quote; totalLabel?: string; className?: string }) {
  const rows: Array<[ReactNode, string]> = []
  for (const it of (q.items || []).filter(notDiscount)) rows.push([it.description, money(it.amount)])
  if (!(q.items || []).length) {
    if (q.credit) rows.push(["Credit for the unused time", "−" + money(q.credit)])
    if (q.charge != null) rows.push(["New plan", money(q.charge)])
  }
  if (q.subtotal != null && ((q.tax_lines || []).length || q.discount)) rows.push(["Subtotal", money(q.subtotal)])
  if (q.discount) rows.push([discountLabel(q.items), "−" + money(q.discount)])
  for (const t of q.tax_lines || []) rows.push([`${t.name} ${fmtRate(t.rate)}`, money(t.amount)])
  return (
    <dl className={cn("flex flex-col text-sm tabular-nums", className)}>
      {rows.map(([k, v], i) => (
        <div key={i} className="flex justify-between gap-4 border-b border-border/60 py-1.5">
          <dt className="text-muted-foreground">{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
      <div className="mt-1.5 flex justify-between gap-4 rounded-xl bg-muted px-3 py-2 text-base font-bold">
        <dt>{totalLabel}</dt>
        <dd>{money(Math.max(0, q.total || 0))}</dd>
      </div>
    </dl>
  )
}

// ---- Change plan ----

// PlanChangeDialog compares plans and quotes the switch as the choice
// changes. onChanged gets the API's answer: {invoice} to pay (the plan
// changes once it's paid), or {applied}.
export function PlanChangeDialog({
  open,
  onOpenChange,
  accountId,
  planId,
  cycle,
  tenant,
  onChanged,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  accountId: number
  planId?: string
  cycle?: string
  tenant?: boolean
  onChanged?: (result: PlanChangeResult) => void
}) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange} wide>
      <PlanChangeForm accountId={accountId} planId={planId} cycle={cycle} tenant={tenant} onChanged={onChanged} onClose={() => onOpenChange(false)} />
    </BillingDialog>
  )
}

function PlanChangeForm({
  accountId,
  planId,
  cycle,
  tenant,
  onChanged,
  onClose,
}: {
  accountId: number
  planId?: string
  cycle?: string
  tenant?: boolean
  onChanged?: (result: PlanChangeResult) => void
  onClose: () => void
}) {
  const config = useBillingConfig()
  const plansQ = useApi<Plan[]>("/plans")
  const plans = (plansQ.data || []).filter((p) => planCycles(p).length && (p.id === planId || !tenant || p.public !== false))
  const startCycle = cycle || "monthly"
  const [sel, setSel] = useState({ plan: planId || "", cycle: startCycle })
  const current = plans.find((p) => p.id === sel.plan)
  // The period follows the plan: one it's sold for.
  const avail = current ? planCycles(current) : []
  const effCycle = avail.some((c) => c.id === sel.cycle) ? sel.cycle : avail[0]?.id || sel.cycle
  const changed = !!current && (sel.plan !== planId || effCycle !== startCycle)

  // Quote a moment after the choice settles.
  const [asked, setAsked] = useState<{ plan: string; cycle: string } | null>(null)
  useEffect(() => {
    const t = setTimeout(() => setAsked(changed ? { plan: sel.plan, cycle: effCycle } : null), 250)
    return () => clearTimeout(t)
  }, [changed, sel.plan, effCycle])
  const quoteQ = useQuery({
    queryKey: ["plan-change-quote", accountId, asked?.plan, asked?.cycle],
    queryFn: () => api<Quote>("POST", `/accounts/${accountId}/plan-change/quote`, { plan_id: asked!.plan, cycle: asked!.cycle }),
    enabled: !!asked,
    retry: false,
    staleTime: 30_000,
    gcTime: 60_000,
  })
  const settled = changed && asked?.plan === sel.plan && asked?.cycle === effCycle
  const q = settled ? quoteQ.data : undefined

  if (plansQ.isError || config.isError) {
    return (
      <DialogForm title="Change plan" noOk cancel="Close" onClose={onClose}>
        <p role="alert" className="text-sm text-danger">
          {String((plansQ.error || config.error)?.message)}
        </p>
      </DialogForm>
    )
  }
  if (!plansQ.data || !config.data) {
    return (
      <DialogForm title="Change plan" intro="What's left of the current period counts towards the new plan." noOk onClose={onClose}>
        <div className="grid gap-3 sm:grid-cols-2" aria-busy="true">
          <Skeleton className="h-28 rounded-2xl" />
          <Skeleton className="h-28 rounded-2xl" />
        </div>
      </DialogForm>
    )
  }
  if (!plans.some((p) => p.id !== planId)) {
    return (
      <DialogForm title="Change plan" noOk cancel="Close" onClose={onClose}>
        <p className="text-sm text-muted-foreground">
          {tenant ? "There are no other plans to switch to right now. Contact us if you need more." : "No other plan has prices: add them under Plans."}
        </p>
      </DialogForm>
    )
  }

  const pick = (id: string) => {
    const p = plans.find((x) => x.id === id)
    const av = p ? planCycles(p) : []
    setSel({ plan: id, cycle: av.some((c) => c.id === effCycle) ? effCycle : av[0]?.id || effCycle })
  }
  let quoteView: ReactNode
  if (!changed) {
    quoteView = <p className="text-sm text-muted-foreground">Choose another plan or billing period to see what changes.</p>
  } else if (!settled || quoteQ.isFetching) {
    quoteView = <p className="text-sm text-muted-foreground">Working out the price…</p>
  } else if (quoteQ.isError) {
    const e = quoteQ.error
    quoteView = (
      <p className="text-sm text-danger">{e instanceof ApiError && e.status === 404 ? "Plan changes aren't available on this server yet." : e.message}</p>
    )
  } else if (q) {
    const total = q.total || 0
    quoteView = (
      <>
        <QuoteLines quote={q} />
        <p className="mt-2 text-sm text-muted-foreground">
          {total <= 0
            ? `Nothing to pay now${total < 0 ? `: ${money(-total)} goes to the account's credit` : ""}. The plan changes straight away.`
            : "The new plan starts as soon as this is paid."}
          {q.new_next_due_at ? ` Next renewal ${fmtDueDate(q.new_next_due_at)}.` : ""}
        </p>
      </>
    )
  }

  return (
    <DialogForm
      title="Change plan"
      intro="What's left of the current period counts towards the new plan."
      ok="Change plan"
      okDisabled={!q || quoteQ.isFetching}
      onClose={onClose}
      onSubmit={async () => {
        const res = await api<PlanChangeResult | null>("POST", `/accounts/${accountId}/plan-change`, { plan_id: sel.plan, cycle: effCycle })
        if (res && res.invoice) notify(`Invoice ${titleOf(res.invoice)} created: the plan changes once it's paid`)
        else notify("Plan changed")
        invalidate("/accounts")
        invalidate("/invoices")
        invalidate("/billing/overview")
        onChanged?.(res || { applied: true })
      }}
    >
      <div role="radiogroup" aria-label="Plans" className="grid gap-3 sm:grid-cols-2">
        {plans.map((p) => {
          const c = planPrice(p, effCycle) ? cycleOf(effCycle) : planCycles(p)[0]
          const pr = planPrice(p, c.id)!
          return (
            <ChoiceCard
              key={p.id}
              name="plan_id"
              value={p.id}
              checked={p.id === sel.plan}
              onChange={pick}
              title={
                <span className="flex items-center gap-2">
                  {p.name}
                  {p.id === planId && <Badge variant="secondary">current</Badge>}
                </span>
              }
              extra={
                <span className="mt-1 flex flex-col gap-0.5 text-sm text-muted-foreground">
                  {p.description && <span>{p.description}</span>}
                  <span>{planLimits(p).join(" · ")}</span>
                </span>
              }
            >
              <span className="text-lg font-bold text-foreground tabular-nums">{money(pr.price)}</span>
              <span> / {c.per}</span>
            </ChoiceCard>
          )
        })}
      </div>
      <LabeledField label="Billing period">
        {(a) => (
          <NativeSelect {...a} name="cycle" className="w-full sm:w-80" value={effCycle} disabled={!current} onChange={(e) => setSel((s) => ({ ...s, cycle: e.target.value }))}>
            {avail.map((c) => (
              <NativeSelectOption key={c.id} value={c.id}>
                {`${c.label} · ${money(planPrice(current!, c.id)!.price)}`}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </LabeledField>
      <div aria-live="polite" className="rounded-2xl bg-muted/50 p-4">
        {quoteView}
      </div>
    </DialogForm>
  )
}

// ---- Cancel the service ----

// CancelDialog: staff choose when (the end of the period, or now); a
// client cancels at the end of the period they paid for.
export function CancelDialog({
  open,
  onOpenChange,
  accountId,
  name,
  admin,
  nextDue,
  onDone,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  accountId: number
  name: string
  admin?: boolean
  nextDue?: string | null
  onDone?: () => void
}) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange}>
      <CancelForm accountId={accountId} name={name} admin={admin} nextDue={nextDue} onDone={onDone} onClose={() => onOpenChange(false)} />
    </BillingDialog>
  )
}

function CancelForm({
  accountId,
  name,
  admin,
  nextDue,
  onDone,
  onClose,
}: {
  accountId: number
  name: string
  admin?: boolean
  nextDue?: string | null
  onDone?: () => void
  onClose: () => void
}) {
  const [when, setWhen] = useState("end_of_period")
  return (
    <DialogForm
      title={admin ? `Cancel ${name}'s service?` : "Cancel your service?"}
      intro={
        admin
          ? undefined
          : `Your sites keep working until ${nextDue ? fmtDueDate(nextDue) : "the end of the period you paid for"}; then the account is closed ` +
            "and nothing more is billed. You can change your mind until then."
      }
      ok="Cancel service"
      cancel="Keep it"
      danger
      onClose={onClose}
      onSubmit={async (f) => {
        const reason = ((f.elements.namedItem("reason") as HTMLTextAreaElement | null)?.value || "").trim()
        await api("POST", `/accounts/${accountId}/cancel`, { when: admin ? when : "end_of_period", reason })
        notify("Cancellation scheduled")
        invalidate(`/accounts/${accountId}`)
        onDone?.()
      }}
    >
      {admin && (
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-2 text-sm font-medium">When</legend>
          <ChoiceCard name="when" value="end_of_period" title="At the end of the period" checked={when === "end_of_period"} onChange={setWhen}>
            {nextDue ? `On ${fmtDueDate(nextDue)}; nothing more is billed.` : "Nothing more is billed."}
          </ChoiceCard>
          <ChoiceCard name="when" value="immediately" title="Right away" checked={when === "immediately"} onChange={setWhen}>
            The account is closed now; unpaid invoices are cancelled.
          </ChoiceCard>
        </fieldset>
      )}
      <LabeledField label={admin ? "Reason (optional)" : "Why are you leaving? (optional)"}>
        {(a) => <Textarea {...a} name="reason" rows={3} maxLength={1000} />}
      </LabeledField>
    </DialogForm>
  )
}
