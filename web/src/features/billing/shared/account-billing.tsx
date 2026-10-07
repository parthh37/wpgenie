// CONTRACT (owner: billing-core agent). An account's billing for staff
// (profile, credit, contacts, plan and cycle, recent invoices): shown in
// the Accounts page's account detail.
import { useId, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { BanIcon, LayersIcon, PencilIcon, PlusIcon, ReceiptIcon, TagIcon, UndoIcon, UsersIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ActionButton, BTable, ChoiceCard, MoneyInput } from "@/components/app/blocks"
import { KeyValues } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import { CYCLES, cycleOf, dateInput, fmtDueDate, fromDateInput, listOf, money, toMinor, useBillingConfig } from "@/lib/money"
import { invalidate, useApi } from "@/lib/query"
import { navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { billingPath } from "../route"
import { useInvoiceEditor } from "../staff/invoice-editor"
import { ContactFields, readContact } from "./contact-fields"
import { BillingDialog, DialogForm, LabeledField, fieldError } from "./dialog-form"
import { BillingPill, InvoiceLink, InvoicePill } from "./invoice-document"
import { CancelDialog, PlanChangeDialog } from "./plan-dialogs"
import { refreshBilling } from "./refresh"
import type { BillingProfile, Contact, CreditEntry, Invoice } from "./types"

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type AccountLike = { id: number; name: string; parent_id?: number | null; parent_name?: string; plan_id?: string; [k: string]: any }

export const BILLING_MODES: Record<string, [title: string, text: string]> = {
  none: ["Not billed here", "No invoices from WPGenie (free, or billed some other way)."],
  invoice: ["Invoices", "WPGenie invoices it every period and collects payment."],
  stripe_subscription: ["Stripe subscription", "Billed by a subscription in Stripe (the older setup)."],
  whmcs: ["WHMCS", "Billed by your WHMCS; WPGenie follows what it says."],
}

type DialogKind = "profile" | "credit" | "plan" | "contact" | "cancel"

// AccountBilling is the Billing panel of an account's detail. A
// reseller's customers are billed by the reseller. onChanged runs after a
// change the account itself shows (its plan).
export function AccountBilling({ account: a, onChanged }: { account: AccountLike; onChanged?: () => void }) {
  if (a.parent_id) {
    return (
      <Section icon={ReceiptIcon} tint="mint" title="Billing">
        <p className="text-sm text-muted-foreground">{`Billed by its reseller${a.parent_name ? " (" + a.parent_name + ")" : ""}, not by you.`}</p>
      </Section>
    )
  }
  return <AccountBillingPanel a={a} onChanged={onChanged} />
}

function AccountBillingPanel({ a, onChanged }: { a: AccountLike; onChanged?: () => void }) {
  const admin = useSession().atLeast("admin")
  const config = useBillingConfig()
  const prof = useApi<BillingProfile>(config.data ? `/accounts/${a.id}/billing` : null)
  const invoices = useQuery({
    queryKey: [`/invoices?account=${a.id}&limit=10`],
    queryFn: () => api<unknown>("GET", `/invoices?account=${a.id}&limit=10`).then((r) => listOf<Invoice>(r, "invoices")).catch(() => [] as Invoice[]),
  })
  const credit = useQuery({
    queryKey: [`/accounts/${a.id}/credit`],
    queryFn: () => api<unknown>("GET", `/accounts/${a.id}/credit`).then((r) => listOf<CreditEntry>(r, "credit")).catch(() => [] as CreditEntry[]),
  })
  const [dlg, setDlg] = useState<DialogKind | null>(null)
  const close = (o: boolean) => !o && setDlg(null)
  const editor = useInvoiceEditor()
  const reload = () => {
    prof.refetch()
    invoices.refetch()
    credit.refetch()
  }

  const err = config.error || prof.error
  if (err) {
    return (
      <Section icon={ReceiptIcon} tint="mint" title="Billing">
        <p className="text-sm text-muted-foreground">
          {err instanceof ApiError && err.status === 404 ? "Billing details aren't available on this server yet." : String(err.message)}
        </p>
      </Section>
    )
  }
  const p = prof.data
  if (!config.data || !p) {
    return (
      <Section icon={ReceiptIcon} tint="mint" title="Billing">
        <p className="sr-only">Loading…</p>
        <div aria-busy="true" className="flex flex-col gap-2">
          {Array.from({ length: 5 }, (_, i) => (
            <Skeleton key={i} className="h-7 rounded-lg" />
          ))}
        </div>
      </Section>
    )
  }

  const card = p.card
    ? `${p.card.brand ? p.card.brand[0].toUpperCase() + p.card.brand.slice(1) : "Card"} •••• ${p.card.last4}, expires ${p.card.exp_month}/${String(p.card.exp_year).slice(-2)}`
    : "none saved"
  const rows: Array<[string, React.ReactNode]> = [
    ["Billed by", (BILLING_MODES[p.mode] || [p.mode])[0]],
    [
      "Price",
      p.recurring_amount != null ? `${money(p.recurring_amount)} / ${cycleOf(p.cycle).per}` + (p.price_override != null ? " (custom price)" : "") : "–",
    ],
    ["Next due", p.next_due_at ? fmtDueDate(p.next_due_at) : "–"],
    [
      "Balance due",
      <span className={p.overdue ? "text-danger" : undefined}>
        {money(p.balance_due || 0)}
        {p.overdue ? " · overdue" : ""}
      </span>,
    ],
    ["Credit", money(p.credit || 0)],
    ["Card", card + (p.auto_pay ? " · pays automatically" : "")],
    ["Tax", p.tax_exempt ? "exempt" : p.tax_exempt_by_tax_id ? `exempt: tax ID ${(p.contact || {}).tax_id || ""}` : "charged by the tax rules"],
  ]
  if (p.cancel_at) {
    rows.push(["Cancellation", <span className="text-warning">{`ends ${fmtDueDate(p.cancel_at)}${p.cancel_reason ? " — " + p.cancel_reason : ""}`}</span>])
  }
  const creditList = credit.data || []

  return (
    <Section icon={ReceiptIcon} tint="mint" title="Billing" action={p.overdue ? <BillingPill state="overdue">Overdue</BillingPill> : undefined}>
      <KeyValues items={rows} />
      {admin && (
        <div className="mt-4 flex flex-wrap gap-2">
          <Button variant="tinted" size="sm" onClick={() => setDlg("profile")}>
            <PencilIcon data-icon="inline-start" />
            Edit billing
          </Button>
          <ActionButton size="sm" run={() => editor.open({ accountId: a.id })}>
            <PlusIcon data-icon="inline-start" />
            New invoice
          </ActionButton>
          <Button variant="tinted" size="sm" onClick={() => setDlg("credit")}>
            <TagIcon data-icon="inline-start" />
            Add credit
          </Button>
          <Button variant="tinted" size="sm" onClick={() => setDlg("plan")}>
            <LayersIcon data-icon="inline-start" />
            Change plan
          </Button>
          <Button variant="tinted" size="sm" onClick={() => setDlg("contact")}>
            <UsersIcon data-icon="inline-start" />
            Billing contact
          </Button>
          {p.cancel_at ? (
            <ActionButton
              size="sm"
              run={async () => {
                await api("DELETE", `/accounts/${a.id}/cancel`)
                notify("Cancellation withdrawn")
                refreshBilling(a.id)
                reload()
              }}
            >
              <UndoIcon data-icon="inline-start" />
              Withdraw cancellation
            </ActionButton>
          ) : (
            <Button variant="destructive" size="sm" onClick={() => setDlg("cancel")}>
              <BanIcon data-icon="inline-start" />
              Cancel service…
            </Button>
          )}
        </div>
      )}

      <h3 className="mt-6 mb-1 text-[0.9375rem] font-semibold">Invoices</h3>
      <BTable
        cols={["Invoice", "Issued", { label: "Total", num: true }, "Status"]}
        rows={(invoices.data || []).map((inv) => ({
          key: inv.id,
          onOpen: () => navigate(billingPath("invoices", inv.id)),
          cells: [<InvoiceLink invoice={inv} />, fmtDueDate(inv.issued_at), money(inv.total), <InvoicePill invoice={inv} />],
        }))}
        empty={<p className="py-2 text-sm text-muted-foreground">{invoices.isLoading ? "Loading…" : "No invoices yet."}</p>}
      />

      {creditList.length > 0 && (
        <details className="mt-4">
          <summary className="cursor-pointer text-sm font-medium text-link select-none">Credit history</summary>
          <BTable
            cols={["Date", "Description", { label: "Amount", num: true }, { label: "Balance", num: true }]}
            rows={creditList.map((c) => ({
              key: c.id,
              cells: [fmtDueDate(c.at), <span className="whitespace-normal">{c.description + (c.by ? ` (${c.by})` : "")}</span>, money(c.amount), money(c.balance)],
            }))}
          />
        </details>
      )}

      {editor.dialog}
      <EditBillingProfileDialog open={dlg === "profile"} onOpenChange={close} account={a} profile={p} onDone={reload} />
      <AddCreditDialog open={dlg === "credit"} onOpenChange={close} account={a} onDone={reload} />
      <EditContactDialog open={dlg === "contact"} onOpenChange={close} accountId={a.id} contact={p.contact} onDone={reload} />
      <PlanChangeDialog
        open={dlg === "plan"}
        onOpenChange={close}
        accountId={a.id}
        planId={a.plan_id}
        cycle={p.cycle}
        onChanged={() => {
          invalidate("/accounts")
          reload()
          onChanged?.()
        }}
      />
      <CancelDialog open={dlg === "cancel"} onOpenChange={close} accountId={a.id} name={a.name} admin nextDue={p.next_due_at} onDone={reload} />
    </Section>
  )
}

// ---- How an account is billed ----

export function EditBillingProfileDialog({
  open,
  onOpenChange,
  account,
  profile,
  onDone,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  account: AccountLike
  profile: BillingProfile
  onDone?: () => void
}) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange} wide>
      <ProfileForm a={account} p={profile} onDone={onDone} onClose={() => onOpenChange(false)} />
    </BillingDialog>
  )
}

function ProfileForm({ a, p, onDone, onClose }: { a: AccountLike; p: BillingProfile; onDone?: () => void; onClose: () => void }) {
  const [mode, setMode] = useState(p.mode || "none")
  const [taxExempt, setTaxExempt] = useState(!!p.tax_exempt)
  const [autoPay, setAutoPay] = useState(!!p.auto_pay)
  return (
    <DialogForm
      title={`Billing for ${a.name}`}
      onClose={onClose}
      onSubmit={async (f) => {
        const get = (n: string) => (f.elements.namedItem(n) as HTMLInputElement | HTMLSelectElement | null)?.value ?? ""
        const price = toMinor(get("price_override"))
        if (Number.isNaN(price)) throw fieldError("price_override", "Enter an amount, or leave it empty.")
        await api("PUT", `/accounts/${a.id}/billing`, {
          mode,
          cycle: get("cycle"),
          price_override: price,
          next_due_at: fromDateInput(get("next_due_at")),
          tax_exempt: taxExempt,
          auto_pay: autoPay,
        })
        notify("Billing saved")
        refreshBilling(a.id)
        onDone?.()
      }}
    >
      <fieldset className="flex flex-col gap-2">
        <legend className="mb-2 text-sm font-medium">How it's billed</legend>
        <div className="grid gap-2 sm:grid-cols-2">
          {Object.entries(BILLING_MODES).map(([k, [t, text]]) => (
            <ChoiceCard key={k} name="mode" value={k} title={t} checked={mode === k} onChange={setMode}>
              {text}
            </ChoiceCard>
          ))}
        </div>
      </fieldset>
      <div className="grid gap-4 sm:grid-cols-3">
        <LabeledField label="Billing period">
          {(x) => (
            <NativeSelect {...x} name="cycle" defaultValue={p.cycle || "monthly"} className="w-full">
              {CYCLES.map((c) => (
                <NativeSelectOption key={c.id} value={c.id}>
                  {c.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </LabeledField>
        <LabeledField label="Custom price" help="Per period; empty uses the plan's price.">
          {(x) => <MoneyInput {...x} name="price_override" minor={p.price_override} placeholder="the plan's price" />}
        </LabeledField>
        <LabeledField label="Next due date" help="Later renewals keep this day of the month.">
          {(x) => <Input {...x} type="date" name="next_due_at" defaultValue={dateInput(p.next_due_at)} />}
        </LabeledField>
      </div>
      <SwitchField label="Tax exempt" help="No tax on its invoices (e.g. a registered business abroad)." checked={taxExempt} onChange={setTaxExempt} />
      <SwitchField
        label="Charge the saved card automatically"
        help={p.card ? "On the due date." : "Needs a saved card: the client saves one when paying by card."}
        checked={autoPay}
        onChange={setAutoPay}
      />
    </DialogForm>
  )
}

function SwitchField({ label, help, checked, onChange }: { label: string; help?: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <LabeledFieldInline label={label} help={help}>
      {(x) => <Switch {...x} checked={checked} onCheckedChange={onChange} className="mt-0.5" />}
    </LabeledFieldInline>
  )
}

// A switch with its label beside it and help under the label.
function LabeledFieldInline({
  label,
  help,
  children,
}: {
  label: string
  help?: string
  children: (a11y: { id: string; "aria-describedby"?: string }) => React.ReactNode
}) {
  const id = useId()
  return (
    <div className="flex items-start gap-3">
      {children({ id, "aria-describedby": help ? id + "-help" : undefined })}
      <div className="flex flex-col">
        <label htmlFor={id} className="text-sm font-medium">
          {label}
        </label>
        {help && (
          <span id={id + "-help"} className="text-sm text-muted-foreground">
            {help}
          </span>
        )}
      </div>
    </div>
  )
}

// ---- Credit ----

export function AddCreditDialog({
  open,
  onOpenChange,
  account,
  onDone,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  account: AccountLike
  onDone?: () => void
}) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange}>
      <DialogForm
        title={`Add credit to ${account.name}`}
        ok="Add credit"
        intro="Credit pays the account's next invoices. A negative amount takes credit away."
        onClose={() => onOpenChange(false)}
        onSubmit={async (f) => {
          const get = (n: string) => (f.elements.namedItem(n) as HTMLInputElement | null)?.value ?? ""
          const amt = toMinor(get("amount"))
          if (!amt || Number.isNaN(amt)) throw fieldError("amount", "Enter an amount (negative to remove credit).")
          await api("POST", `/accounts/${account.id}/credit`, { amount: amt, description: get("description").trim() })
          notify(`${money(amt)} credit ${amt > 0 ? "added" : "removed"}`)
          refreshBilling(account.id)
          onDone?.()
        }}
      >
        <LabeledField label="Amount">{(x) => <MoneyInput {...x} name="amount" minor={null} required />}</LabeledField>
        <LabeledField label="Description" help="The client sees it.">
          {(x) => <Input {...x} name="description" required placeholder="e.g. Goodwill for the outage on 3 May" maxLength={200} />}
        </LabeledField>
      </DialogForm>
    </BillingDialog>
  )
}

// ---- Billing contact ----

export function EditContactDialog({
  open,
  onOpenChange,
  accountId,
  contact,
  onDone,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  accountId: number
  contact?: Contact | null
  onDone?: () => void
}) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange} wide>
      <DialogForm
        title="Billing contact"
        intro="The name and address on invoices, and where they're e-mailed."
        onClose={() => onOpenChange(false)}
        onSubmit={async (f) => {
          await api("PUT", `/accounts/${accountId}/billing/contact`, readContact(f))
          notify("Billing contact saved")
          refreshBilling(accountId)
          onDone?.()
        }}
      >
        <ContactFields contact={contact || {}} />
      </DialogForm>
    </BillingDialog>
  )
}
