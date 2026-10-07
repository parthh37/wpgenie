import { useState } from "react"
import { PercentIcon, PlusIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ActionButton, BTable, EmptyState, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtDate, fmtNum } from "@/lib/format"
import { CYCLES, cycleOf, dateInput, fmtRate, fromDateInput, fromMinor, listOf, money, toMinor, toRate, useBillingConfig } from "@/lib/money"
import { useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import { cn } from "@/lib/utils"
import { CheckList, GroupLabel, LiveHint, Labeled, MoneyField, PercentField, Segmented, TextField, ToggleField, fieldError, grid, grid3, rateText } from "./common"

// Promotions: codes clients enter on the order page (or staff apply to an
// invoice) for a discount.

export interface Promotion {
  id?: number
  code: string
  description: string
  type: "percent" | "fixed" | string
  value: number
  plans: string[]
  cycles: string[]
  recurring: boolean
  max_uses: number
  uses?: number
  starts_at: string | null
  expires_at: string | null
  new_clients_only: boolean
  enabled: boolean
}
interface Plan {
  id: string
  name: string
}

const promoValue = (p: Promotion) => (p.type === "percent" ? `${fmtRate(p.value)} off` : `${money(p.value)} off`)
const promoDates = (p: Promotion) =>
  p.starts_at || p.expires_at ? `${p.starts_at ? fmtDate(p.starts_at) : "now"} – ${p.expires_at ? fmtDate(p.expires_at) : "no end"}` : "always"

function promoScope(p: Pick<Promotion, "plans" | "cycles" | "new_clients_only">, plans: Plan[], withClients = true) {
  const names = (p.plans || []).map((id) => (plans.find((x) => x.id === id) || { name: id }).name)
  const parts = [names.length ? names.join(", ") : "every plan"]
  if ((p.cycles || []).length) parts.push(p.cycles.map((c) => cycleOf(c).label.toLowerCase()).join(", "))
  if (withClients && p.new_clients_only) parts.push("new clients only")
  return parts.join(" · ")
}

// promoBody is what the API takes (id and uses are the server's).
function promoBody(p: Promotion) {
  const { id: _id, uses: _uses, ...body } = p
  return body
}

export function Promotions() {
  const s = useSession()
  const cfg = useBillingConfig()
  const promos = useApi<unknown>("/billing/promotions")
  const plansQ = useApi<unknown>("/plans")
  const [editing, setEditing] = useState<{ p: Promotion | null; key: number } | null>(null)
  const [open, setOpen] = useState(false)

  if (!s.atLeast("admin")) return <LoadError error={new Error("Promotions are for administrators.")} />
  const err = promos.error || plansQ.error
  if (err) return <LoadError error={err} retry={() => (promos.refetch(), plansQ.refetch())} />
  if (!promos.data || !plansQ.data || !cfg.data) return <Skeleton className="h-64 rounded-2xl" />

  const list = listOf<Promotion>(promos.data, "promotions")
  const plans = listOf<Plan>(plansQ.data, "plans")
  const reload = () => promos.refetch()
  const edit = (p: Promotion | null) => {
    setEditing({ p, key: Date.now() })
    setOpen(true)
  }
  const add = (
    <Button onClick={() => edit(null)}>
      <PlusIcon data-icon="inline-start" />
      New promotion
    </Button>
  )

  return (
    <>
      <Section
        icon={PercentIcon}
        tint="pink"
        title="Promotions"
        description="Codes clients enter on the order page (or you apply to an invoice) for a discount."
        action={list.length ? add : undefined}
      >
        <BTable
          caption="Promotions"
          cols={["Code", "Discount", "For", { label: "Used", num: true }, "Valid", "On", ""]}
          rows={list.map((p) => ({
            key: p.id,
            className: cn(!p.enabled && "opacity-60"),
            cells: [
              <>
                <code className="font-mono tracking-wide uppercase">{p.code}</code>
                {p.description && <span className="block text-xs text-muted-foreground">{p.description}</span>}
              </>,
              <>
                {promoValue(p)}
                <span className="block text-xs text-muted-foreground">{p.recurring ? "every invoice" : "first invoice only"}</span>
              </>,
              <span className="text-xs">{promoScope(p, plans)}</span>,
              `${fmtNum(p.uses || 0)}${p.max_uses ? " of " + fmtNum(p.max_uses) : ""}`,
              <span className="text-xs">{promoDates(p)}</span>,
              <EnabledSwitch key={`${p.id}-${p.enabled}`} p={p} onChanged={reload} />,
              <div className="flex justify-end gap-1.5">
                <Button variant="tinted" size="sm" onClick={() => edit(p)}>
                  Edit
                </Button>
                <ActionButton
                  variant="destructive"
                  size="sm"
                  run={async () => {
                    if (!(await ask(`Delete the promotion ${p.code}? Invoices that used it keep their discount.`))) return
                    await api("DELETE", `/billing/promotions/${p.id}`)
                    reload()
                  }}
                >
                  Delete
                </ActionButton>
              </div>,
            ],
          }))}
          empty={
            <EmptyState icon={PercentIcon} tint="pink" title="No promotions yet" actions={add} className="bg-transparent py-6 shadow-none">
              A code like LAUNCH20 for 20% off the first invoice, or a fixed amount off every renewal for a partner. You choose the plans, billing
              periods, dates and how many times it can be used.
            </EmptyState>
          }
        />
      </Section>
      {editing && <PromotionDialog key={editing.key} open={open} onOpenChange={setOpen} p={editing.p} plans={plans} onSaved={reload} />}
    </>
  )
}

// The On switch saves at once; a failure puts it back.
function EnabledSwitch({ p, onChanged }: { p: Promotion; onChanged: () => void }) {
  const [on, setOn] = useState(!!p.enabled)
  const [busy, setBusy] = useState(false)
  return (
    <Switch
      checked={on}
      disabled={busy}
      aria-label={`${p.code} enabled`}
      onCheckedChange={async (v) => {
        setOn(v)
        setBusy(true)
        try {
          await api("PUT", `/billing/promotions/${p.id}`, promoBody({ ...p, enabled: v }))
          onChanged()
        } catch (e) {
          showError(e)
          setOn(!v)
        } finally {
          setBusy(false)
        }
      }}
    />
  )
}

const NEW: Promotion = {
  code: "",
  description: "",
  type: "percent",
  value: 1000,
  plans: [],
  cycles: [],
  recurring: false,
  max_uses: 0,
  starts_at: null,
  expires_at: null,
  new_clients_only: true,
  enabled: true,
}

// A code to hand out: letters and digits that can't be mistaken (no 0/O, 1/I).
function makeCode() {
  const abc = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
  return [...crypto.getRandomValues(new Uint8Array(8))].map((x) => abc[x % abc.length]).join("")
}

function PromotionDialog({
  open,
  onOpenChange,
  p: given,
  plans,
  onSaved,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  p: Promotion | null
  plans: Plan[]
  onSaved: () => void
}) {
  const p = given || NEW
  const [code, setCode] = useState(p.code)
  const [description, setDescription] = useState(p.description || "")
  const [type, setType] = useState<string>(p.type)
  const [value, setValue] = useState(p.type === "percent" ? rateText(p.value) : fromMinor(p.value))
  const [selPlans, setPlans] = useState<string[]>(p.plans || [])
  const [cycles, setCycles] = useState<string[]>(p.cycles || [])
  const [startsAt, setStartsAt] = useState(dateInput(p.starts_at))
  const [expiresAt, setExpiresAt] = useState(dateInput(p.expires_at))
  const [maxUses, setMaxUses] = useState(String(p.max_uses || 0))
  const [recurring, setRecurring] = useState(!!p.recurring)
  const [newOnly, setNewOnly] = useState(!!p.new_clients_only)
  const [enabled, setEnabled] = useState(p.enabled !== false)

  const read = (): Promotion => ({
    code: code.trim().toUpperCase(),
    description: description.trim(),
    type,
    value: (type === "percent" ? toRate(value) : toMinor(value)) as number,
    plans: selPlans,
    cycles,
    recurring,
    max_uses: Math.max(0, Math.trunc(Number(maxUses || 0))),
    starts_at: fromDateInput(startsAt),
    expires_at: fromDateInput(expiresAt),
    new_clients_only: newOnly,
    enabled,
  })
  const v = read()
  const off = v.value == null || Number.isNaN(v.value) ? "…" : v.type === "percent" ? fmtRate(v.value) : money(v.value)
  const summary =
    `${v.code || "The code"} takes ${off} off ${v.recurring ? "every invoice" : "the first invoice"}` +
    ` of ${v.new_clients_only ? "new clients" : "anyone"}, on ${promoScope(v, plans, false)}` +
    (v.max_uses ? `, ${v.max_uses} time${v.max_uses === 1 ? "" : "s"} at most` : "") +
    (v.expires_at ? `, until ${fmtDate(v.expires_at)}` : "") +
    "."

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      wide
      title={p.id ? `Edit ${p.code}` : "New promotion"}
      onSubmit={async () => {
        const body = read()
        if (body.value == null || Number.isNaN(body.value) || body.value <= 0) throw fieldError("value", "Enter the discount.")
        if (body.type === "percent" && body.value > 10000) throw fieldError("value", "A percentage goes up to 100.")
        if (p.id) await api("PUT", `/billing/promotions/${p.id}`, body)
        else await api("POST", "/billing/promotions", body)
        notify(`Promotion ${body.code} saved`)
        onSaved()
      }}
    >
      <div className={grid}>
        <Labeled label="Code" help="Letters, digits, - and _. Clients type it; case doesn't matter.">
          <div className="flex gap-2">
            <TextField
              name="code"
              required
              value={code}
              onChange={(e) => setCode(e.target.value)}
              pattern="[A-Za-z0-9_\-]{2,32}"
              autoComplete="off"
              spellCheck={false}
              className="font-mono tracking-wide uppercase"
            />
            <Button type="button" variant="tinted" onClick={() => setCode(makeCode())}>
              Make one up
            </Button>
          </div>
        </Labeled>
        <Labeled label="Description (optional)" help="Shown on the invoice line.">
          <TextField name="description" value={description} maxLength={200} onChange={(e) => setDescription(e.target.value)} />
        </Labeled>
      </div>
      <div className={grid}>
        <div className="flex flex-col gap-3">
          <GroupLabel>Discount</GroupLabel>
          <Segmented
            name="type"
            label="Kind of discount"
            value={type}
            onChange={(t) => {
              setType(t)
              setValue("")
            }}
            options={[
              ["percent", "Percentage"],
              ["fixed", "Fixed amount"],
            ]}
          />
        </div>
        <Labeled label={type === "percent" ? "Percent off" : "Amount off"}>
          {type === "percent" ? (
            <PercentField name="value" required value={value} onValue={setValue} />
          ) : (
            <MoneyField name="value" required value={value} onValue={setValue} placeholder={fromMinor(0)} />
          )}
        </Labeled>
      </div>
      <div className={grid}>
        <fieldset className="flex flex-col gap-2.5">
          <legend className="mb-2.5 text-sm font-medium">Plans (none ticked: every plan)</legend>
          {plans.length ? (
            <CheckList name="plans" items={plans.map((pl) => [pl.id, pl.name])} selected={selPlans} onChange={setPlans} />
          ) : (
            <p className="text-sm text-muted-foreground">No plans yet.</p>
          )}
        </fieldset>
        <fieldset className="flex flex-col gap-2.5">
          <legend className="mb-2.5 text-sm font-medium">Billing periods (none ticked: all)</legend>
          <CheckList name="cycles" items={CYCLES.map((c) => [c.id, c.label])} selected={cycles} onChange={setCycles} />
        </fieldset>
      </div>
      <div className={grid3}>
        <Labeled label="Starts" help="Empty: now.">
          <TextField type="date" name="starts_at" value={startsAt} onChange={(e) => setStartsAt(e.target.value)} />
        </Labeled>
        <Labeled label="Ends" help="Empty: no end.">
          <TextField type="date" name="expires_at" value={expiresAt} onChange={(e) => setExpiresAt(e.target.value)} />
        </Labeled>
        <Labeled label="Uses at most" help="0: no limit.">
          <TextField type="number" name="max_uses" min={0} value={maxUses} onChange={(e) => setMaxUses(e.target.value)} />
        </Labeled>
      </div>
      <div className="flex flex-col gap-4">
        <ToggleField name="recurring" label="Also on renewals" checked={recurring} onChange={setRecurring} help="Off: only the first invoice gets the discount." />
        <ToggleField name="new_clients_only" label="New clients only" checked={newOnly} onChange={setNewOnly} />
        <ToggleField name="enabled" label="Enabled" checked={enabled} onChange={setEnabled} />
      </div>
      <LiveHint>{summary}</LiveHint>
    </FormDialog>
  )
}
