import { useState } from "react"
import { BanIcon, BellRingIcon, CalendarIcon, HistoryIcon, LockIcon, MailIcon, ReceiptIcon, RefreshCwIcon, TagIcon, type LucideIcon } from "lucide-react"
import { NativeSelectOption } from "@/components/ui/native-select"
import { ActionButton, BTable } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { fmtRate, fromMinor, listOf, money, toMinor, toRate } from "@/lib/money"
import { useApi } from "@/lib/query"
import { cn } from "@/lib/utils"
import {
  Labeled, MoneyField, PercentField, SelectField, SettingsCard, SubHeading, SuffixField, TextField, ToggleField, WithInvoicing, fieldError, grid,
  rateText, saveInvoicing, type Automation as AutomationSettings, type InvoicingSettings,
} from "./common"

// Reminders and suspension: what happens when an invoice isn't paid, as a
// timeline that follows the numbers, and the automation's recent runs.

interface Step {
  day: number
  label: string
  icon: LucideIcon
  tone: "info" | "due" | "warn" | "bad"
}

// dunningSteps is what happens to an unpaid invoice, day by day.
function dunningSteps(a: AutomationSettings, daysBeforeDue: number): Step[] {
  const steps: Step[] = [{ day: -daysBeforeDue, label: "Invoice e-mailed", icon: ReceiptIcon, tone: "info" }]
  const before = a.reminder_days_before ?? 0
  if (before > 0 && before < daysBeforeDue) steps.push({ day: -before, label: "Friendly reminder", icon: MailIcon, tone: "info" })
  steps.push({ day: 0, label: a.autocharge ? "Due: saved cards charged" : "Due date", icon: CalendarIcon, tone: "due" })
  for (const d of a.overdue_reminder_days || []) if (d > 0) steps.push({ day: d, label: "Overdue reminder", icon: MailIcon, tone: "warn" })
  const lf = a.late_fee
  if (lf && lf.type !== "none" && lf.amount > 0) {
    steps.push({
      day: a.late_fee_after_days ?? 0,
      label: `Late fee ${lf.type === "percent" ? fmtRate(lf.amount) : money(lf.amount)}`,
      icon: TagIcon,
      tone: "warn",
    })
  }
  if ((a.suspend_after_days ?? 0) > 0) steps.push({ day: a.suspend_after_days!, label: "Sites suspended", icon: LockIcon, tone: "bad" })
  if ((a.terminate_after_days ?? 0) > 0) {
    steps.push({ day: a.terminate_after_days!, label: a.terminate_deletes_sites ? "Closed, sites deleted" : "Account closed", icon: BanIcon, tone: "bad" })
  }
  return steps.sort((x, y) => x.day - y.day)
}

const dayText = (d: number) => (d < 0 ? `${-d} day${d === -1 ? "" : "s"} before` : d === 0 ? "Due date" : `${d} day${d === 1 ? "" : "s"} late`)

const DOT: Record<Step["tone"], string> = {
  info: "border-primary bg-card text-primary",
  due: "border-primary bg-primary text-primary-foreground",
  warn: "border-warning bg-card text-warning",
  bad: "border-danger bg-danger-fill/15 text-danger",
}

// The timeline: across on wide screens, down on narrow ones.
function DunningTimeline({ steps }: { steps: Step[] }) {
  const last = steps.length - 1
  return (
    <div className="overflow-x-auto rounded-2xl bg-muted/60 px-4 pt-4 pb-2">
      <ol aria-label="What happens to an unpaid invoice" className="flex flex-col sm:min-w-max sm:flex-row">
        {steps.map((s, i) => (
          <li
            key={`${s.day}-${s.label}-${i}`}
            className="relative flex items-center gap-3 pb-3.5 sm:flex-[1_0_7.5rem] sm:flex-col sm:gap-1 sm:px-1.5 sm:pb-2 sm:text-center"
          >
            {steps.length > 1 && (
              <span
                aria-hidden
                className={cn(
                  "absolute bg-border",
                  // Down: a vertical line through the dots.
                  "left-[17px] w-0.5",
                  i === 0 ? "top-[18px] bottom-0" : i === last ? "top-0 h-[18px]" : "top-0 bottom-0",
                  // Across: a horizontal one.
                  "sm:top-[17px] sm:bottom-auto sm:h-0.5 sm:w-auto",
                  i === 0 ? "sm:left-1/2 sm:right-0" : i === last ? "sm:left-0 sm:right-1/2" : "sm:left-0 sm:right-0"
                )}
              />
            )}
            <span className={cn("relative z-[1] grid size-9 shrink-0 place-items-center rounded-full border-2", DOT[s.tone])}>
              <s.icon className="size-4" />
            </span>
            <span className="order-2 ml-auto text-[0.72rem] font-semibold tracking-wide text-muted-foreground uppercase tabular-nums sm:order-none sm:ml-0">
              {dayText(s.day)}
            </span>
            <span className="text-sm leading-snug font-medium">{s.label}</span>
          </li>
        ))}
      </ol>
    </div>
  )
}

export function Automation() {
  return (
    <>
      <WithInvoicing>{(s) => <AutomationForm s={s} />}</WithInvoicing>
      <Runs />
    </>
  )
}

function AutomationForm({ s }: { s: InvoicingSettings }) {
  const a = s.automation || {}
  const lf = a.late_fee || { type: "none", amount: 0 }
  const num = (v: number | undefined) => String(v ?? 0)
  const [f, setF] = useState({
    reminder_days_before: num(a.reminder_days_before),
    overdue_reminder_days: (a.overdue_reminder_days || []).join(", "),
    suspend_after_days: num(a.suspend_after_days),
    terminate_after_days: num(a.terminate_after_days),
    late_fee_after_days: num(a.late_fee_after_days),
  })
  const text = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const [feeType, setFeeType] = useState<string>(lf.type || "none")
  const [feeAmount, setFeeAmount] = useState(lf.type === "percent" ? rateText(lf.amount) : lf.type === "fixed" ? fromMinor(lf.amount) : "")
  const [deletes, setDeletes] = useState(!!a.terminate_deletes_sites)
  const [autocharge, setAutocharge] = useState(a.autocharge ?? true)
  const [autoCredit, setAutoCredit] = useState(a.auto_apply_credit ?? true)
  const [approval, setApproval] = useState(!!a.orders_need_approval)

  const read = (): AutomationSettings & { late_fee: { type: string; amount: number } } => {
    const amt = feeType === "percent" ? toRate(feeAmount) : feeType === "fixed" ? toMinor(feeAmount) : 0
    return {
      reminder_days_before: Number(f.reminder_days_before || 0),
      overdue_reminder_days: f.overdue_reminder_days
        .split(/[,\s]+/)
        .map(Number)
        .filter((n) => Number.isInteger(n) && n > 0)
        .sort((x, y) => x - y),
      suspend_after_days: Number(f.suspend_after_days || 0),
      terminate_after_days: Number(f.terminate_after_days || 0),
      terminate_deletes_sites: deletes,
      late_fee: { type: feeType, amount: amt == null || Number.isNaN(amt) ? 0 : amt },
      late_fee_after_days: Number(f.late_fee_after_days || 0),
      auto_apply_credit: autoCredit,
      orders_need_approval: approval,
      autocharge,
    }
  }
  const days = (name: keyof typeof f, max = 365) => (
    <SuffixField suffix="days" name={name} type="number" min={0} max={max} value={f[name]} onChange={text(name)} />
  )

  return (
    <SettingsCard
      icon={BellRingIcon}
      tint="orange"
      title="Reminders and suspension"
      intro="What happens when an invoice isn't paid. The timeline follows the numbers as you change them."
      onSave={async () => {
        const v = read()
        if (v.late_fee.type !== "none" && !v.late_fee.amount) throw fieldError("late_fee_amount", "Enter the late fee, or choose no late fee.")
        await saveInvoicing({ automation: v })
      }}
    >
      <DunningTimeline steps={dunningSteps(read(), s.invoice?.days_before_due ?? 7)} />
      <div className={grid}>
        <Labeled label="Friendly reminder" help="Days before the due date; 0: none.">
          {days("reminder_days_before", 60)}
        </Labeled>
        <Labeled label="Overdue reminders" help="Days after the due date, separated by commas.">
          <TextField name="overdue_reminder_days" placeholder="1, 3, 7" value={f.overdue_reminder_days} onChange={text("overdue_reminder_days")} />
        </Labeled>
        <Labeled label="Suspend sites" help="Days after the due date; 0: never. Paying lifts it by itself.">
          {days("suspend_after_days")}
        </Labeled>
        <Labeled label="Close the account" help="Days after the due date; 0: never.">
          {days("terminate_after_days")}
        </Labeled>
      </div>
      <ToggleField
        name="terminate_deletes_sites"
        label="Delete the sites of closed accounts"
        help="Off: closed accounts' sites stay suspended on the server until you delete them."
        checked={deletes}
        onChange={setDeletes}
      />
      <SubHeading>Late fee</SubHeading>
      <div className="grid gap-4 sm:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)_minmax(0,1fr)]">
        <Labeled label="Late fee">
          <SelectField
            name="late_fee_type"
            value={feeType}
            onChange={(e) => {
              setFeeType(e.target.value)
              setFeeAmount("")
            }}
          >
            <NativeSelectOption value="none">No late fee</NativeSelectOption>
            <NativeSelectOption value="fixed">A fixed amount</NativeSelectOption>
            <NativeSelectOption value="percent">A percentage of the balance</NativeSelectOption>
          </SelectField>
        </Labeled>
        {feeType === "none" ? (
          <div className="max-sm:hidden" />
        ) : (
          <Labeled label={feeType === "percent" ? "Late fee" : "Late fee amount"}>
            {feeType === "percent" ? (
              <PercentField name="late_fee_amount" value={feeAmount} onValue={setFeeAmount} />
            ) : (
              <MoneyField name="late_fee_amount" value={feeAmount} onValue={setFeeAmount} placeholder={fromMinor(0)} />
            )}
          </Labeled>
        )}
        <Labeled label="Added" help="Days after the due date, once.">
          {days("late_fee_after_days")}
        </Labeled>
      </div>
      <SubHeading>Payments and orders</SubHeading>
      <div className="flex flex-col gap-4">
        <ToggleField
          name="autocharge"
          label="Charge saved cards on the due date"
          help="For clients who turned on automatic payment."
          checked={autocharge}
          onChange={setAutocharge}
        />
        <ToggleField name="auto_apply_credit" label="Pay new invoices from account credit" checked={autoCredit} onChange={setAutoCredit} />
        <ToggleField
          name="orders_need_approval"
          label="Approve new orders by hand"
          help="Paid orders wait under Orders until you accept them."
          checked={approval}
          onChange={setApproval}
        />
      </div>
    </SettingsCard>
  )
}

// ---- Recent runs of the automation ----

interface Run {
  id?: number
  started_at?: string
  at?: string
  finished_at?: string
  counts?: Record<string, number>
  errors?: string[] | null
  error?: string
}

// Go's zero time is "no time".
const when = (t: string | undefined) => (t && !t.startsWith("0001-") ? t : "")

function Runs() {
  const q = useApi<unknown>("/billing/automation")
  const list = q.data ? listOf<Run>(q.data, "runs") : []
  return (
    <Section
      icon={HistoryIcon}
      tint="gray"
      title="Recent runs"
      description="Every 15 minutes WPGenie creates renewal invoices, charges saved cards, sends reminders and suspends or unsuspends accounts."
      action={
        <ActionButton
          run={async () => {
            await api("POST", "/billing/automation/run")
            notify("Billing automation ran")
            await q.refetch()
          }}
        >
          <RefreshCwIcon data-icon="inline-start" />
          Run now
        </ActionButton>
      }
    >
      <div aria-live="polite">
        {q.error ? (
          <p className="text-sm text-muted-foreground">
            {q.error instanceof ApiError && q.error.status === 404 ? "Not available on this server yet." : q.error.message}
          </p>
        ) : !q.data ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : (
          <BTable
            cols={["Started", "Took", "Done", "Problems"]}
            rows={list.map((x, i) => {
              const started = when(x.started_at || x.at)
              const finished = when(x.finished_at)
              const problems = x.error || (x.errors || []).join("; ")
              return {
                key: x.id ?? i,
                cells: [
                  fmtTime(started || null),
                  finished && started ? `${Math.max(0, Math.round((new Date(finished).getTime() - new Date(started).getTime()) / 1000))} s` : "–",
                  <span className="text-xs">
                    {Object.entries(x.counts || {})
                      .filter(([, n]) => n)
                      .map(([k, n]) => `${n} ${k.replace(/_/g, " ")}`)
                      .join(", ") || "nothing to do"}
                  </span>,
                  <span className={cn("text-xs", problems ? "text-danger" : "text-muted-foreground")}>{problems || "none"}</span>,
                ],
              }
            })}
            empty={<p className="text-sm text-muted-foreground">It hasn't run yet. It runs every 15 minutes.</p>}
          />
        )}
      </div>
    </Section>
  )
}
