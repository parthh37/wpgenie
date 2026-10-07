import { useId, useState, type ReactNode } from "react"
import { PlusIcon, Trash2Icon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { ChoiceCard } from "@/components/app/blocks"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { currencySymbol, dateInput, fromDateInput, fromMinor, loadBillingConfig, money, toMinor, todayInput, useBillingConfig } from "@/lib/money"
import { queryClient } from "@/lib/query"
import { navigate } from "@/lib/router"
import { billingPath } from "../route"
import { BillingDialog, DialogForm, LabeledField } from "../shared/dialog-form"
import { titleOf } from "../shared/invoice-document"
import { refreshBilling } from "../shared/refresh"
import type { Invoice } from "../shared/types"

// The invoice editor: a new invoice (or a draft's lines) with a total that
// follows the lines; tax is added from the tax rules when it's saved.

interface AccountOption {
  id: number
  name: string
  parent_id?: number | null
  status?: string
}

interface EditorState {
  accountId?: number
  invoice?: Invoice
  accounts?: AccountOption[]
}

// useInvoiceEditor: open({accountId}) for a new invoice (for that client
// when given), open({invoice}) for a draft. Render .dialog somewhere.
export function useInvoiceEditor(onSaved?: () => void) {
  const [state, setState] = useState<EditorState | null>(null)
  const [open, setOpen] = useState(false)

  async function openEditor(opts: { accountId?: number; invoice?: Invoice } = {}) {
    await queryClient.ensureQueryData({ queryKey: ["/billing/config"], queryFn: loadBillingConfig })
    let accounts: AccountOption[] | undefined
    if (!opts.invoice) {
      accounts = (await api<AccountOption[]>("GET", "/accounts"))
        .filter((a) => !a.parent_id && a.status !== "terminated")
        .sort((a, b) => a.name.localeCompare(b.name))
      if (!accounts.length) {
        showError(new Error("There are no accounts to invoice yet: create one under Accounts."))
        return
      }
    }
    setState({ ...opts, accounts })
    setOpen(true)
  }

  const dialog: ReactNode = state ? (
    <BillingDialog open={open} onOpenChange={setOpen} wide>
      <EditorForm state={state} onSaved={onSaved} onClose={() => setOpen(false)} />
    </BillingDialog>
  ) : null

  return { open: openEditor, dialog }
}

interface Line {
  key: number
  description: string
  quantity: string
  unit_price: string
  taxable: boolean
}

let LINE_KEY = 0
const newLine = (it: Partial<{ description: string; quantity: number; unit_price: number; taxable: boolean }> = {}): Line => ({
  key: ++LINE_KEY,
  description: it.description || "",
  quantity: String(it.quantity || 1),
  unit_price: fromMinor(it.unit_price),
  taxable: it.taxable !== false,
})

const lineAmount = (l: Line) => {
  const q = Number(l.quantity || 0)
  const p = toMinor(l.unit_price)
  return p == null || Number.isNaN(p) ? null : Math.round(q * p)
}

function EditorForm({ state, onSaved, onClose }: { state: EditorState; onSaved?: () => void; onClose: () => void }) {
  const { invoice, accounts } = state
  const { data: config } = useBillingConfig()
  const uid = useId()
  const [lines, setLines] = useState<Line[]>(() => {
    const from = invoice ? (invoice.items || []).filter((it) => !["discount", "credit"].includes(it.kind || "")) : [{}]
    const ls = from.map((it) => newLine(it))
    return ls.length ? ls : [newLine()]
  })
  const [focusKey, setFocusKey] = useState<number | null>(null)
  const [mode, setMode] = useState("issue")
  const [sendEmail, setSendEmail] = useState(true)
  const draft = mode === "draft"
  const subtotal = lines.reduce((s, l) => s + (lineAmount(l) || 0), 0)

  const update = (key: number, patch: Partial<Line>) => setLines((ls) => ls.map((l) => (l.key === key ? { ...l, ...patch } : l)))
  const addLine = () => {
    const l = newLine()
    setLines((ls) => [...ls, l])
    setFocusKey(l.key)
  }

  return (
    <DialogForm
      title={invoice ? `Edit ${titleOf(invoice)}` : "New invoice"}
      ok={invoice ? "Save draft" : "Create invoice"}
      onClose={onClose}
      onSubmit={async (f) => {
        const items = lines
          .map((l) => ({
            description: l.description.trim(),
            quantity: Math.max(1, Math.trunc(Number(l.quantity || 1))),
            unit_price: toMinor(l.unit_price),
            taxable: l.taxable,
          }))
          .filter((it) => it.description || it.unit_price != null)
        if (!items.length) throw new Error("Add at least one line.")
        const bad = items.findIndex((it) => !it.description || it.unit_price == null || Number.isNaN(it.unit_price))
        if (bad >= 0) {
          // The lines left after empty ones are skipped: find the row it came from.
          const row = lines.filter((l) => l.description.trim() || toMinor(l.unit_price) != null)[bad]
          document.getElementById(`${uid}-${items[bad].description ? "price" : "desc"}-${row.key}`)?.focus()
          throw new Error(`Line ${bad + 1} needs a description and a price.`)
        }
        const get = (n: string) => (f.elements.namedItem(n) as HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement | null)?.value ?? ""
        const body = { items, due_at: fromDateInput(get("due_at")), notes: get("notes").trim() }
        if (invoice) {
          await api("PUT", `/invoices/${invoice.id}`, body)
          notify("Draft saved")
          refreshBilling(invoice.account_id)
          onSaved?.()
          return true
        }
        const created = await api<Invoice | { invoice: Invoice }>("POST", "/invoices", {
          account_id: Number(get("account_id")),
          ...body,
          draft,
          send_email: !draft && sendEmail,
        })
        const inv = "invoice" in created && created.invoice ? created.invoice : (created as Invoice)
        notify(draft ? "Draft saved" : `Invoice ${titleOf(inv)} created`)
        refreshBilling(Number(get("account_id")))
        onSaved?.()
        if (inv && inv.id) navigate(billingPath("invoices", inv.id))
        return true
      }}
    >
      {accounts ? (
        <LabeledField label="Client">
          {(p) => (
            <NativeSelect {...p} name="account_id" required className="w-full" defaultValue={state.accountId != null ? String(state.accountId) : ""}>
              <NativeSelectOption value="">Choose a client…</NativeSelectOption>
              {accounts.map((a) => (
                <NativeSelectOption key={a.id} value={String(a.id)}>
                  {`${a.name} (#${a.id})`}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </LabeledField>
      ) : (
        <p className="text-sm">
          For <strong>{invoice?.account_name || `account #${invoice?.account_id}`}</strong>
        </p>
      )}

      <div className="flex flex-col gap-2">
        <div
          aria-hidden
          className="hidden grid-cols-[minmax(0,1fr)_4.5rem_8.5rem_6.5rem_4.5rem_2.25rem] gap-2 px-1 text-xs font-semibold tracking-wide text-muted-foreground uppercase sm:grid"
        >
          <span>Description</span>
          <span>Qty</span>
          <span>Unit price</span>
          <span className="text-right">Amount</span>
          <span />
          <span />
        </div>
        <div role="list" className="flex flex-col gap-2">
          {lines.map((l, idx) => {
            const i = idx + 1
            const amt = lineAmount(l)
            return (
              <div
                key={l.key}
                role="listitem"
                className="grid grid-cols-[4.5rem_minmax(0,1fr)_2.25rem] items-center gap-2 rounded-2xl bg-muted/40 p-2 sm:grid-cols-[minmax(0,1fr)_4.5rem_8.5rem_6.5rem_4.5rem_2.25rem] sm:bg-transparent sm:p-0"
              >
                <Input
                  id={`${uid}-desc-${l.key}`}
                  aria-label={`Line ${i}: description`}
                  placeholder="Description"
                  maxLength={200}
                  value={l.description}
                  autoFocus={focusKey === l.key}
                  onChange={(e) => update(l.key, { description: e.target.value })}
                  className="col-span-3 sm:col-span-1"
                />
                <Input
                  type="number"
                  min={1}
                  step={1}
                  aria-label={`Line ${i}: quantity`}
                  value={l.quantity}
                  onChange={(e) => update(l.key, { quantity: e.target.value })}
                />
                <InputGroup>
                  <InputGroupAddon>
                    <InputGroupText>{currencySymbol()}</InputGroupText>
                  </InputGroupAddon>
                  <InputGroupInput
                    id={`${uid}-price-${l.key}`}
                    inputMode="decimal"
                    autoComplete="off"
                    aria-label={`Line ${i}: unit price`}
                    placeholder={fromMinor(0)}
                    value={l.unit_price}
                    onChange={(e) => update(l.key, { unit_price: e.target.value })}
                  />
                </InputGroup>
                <span aria-label={`Line ${i}: amount`} className="text-right text-sm font-medium tabular-nums max-sm:hidden">
                  {amt == null ? "–" : money(amt)}
                </span>
                <label title="Tax applies to this line" className="flex items-center gap-1.5 text-sm max-sm:order-last max-sm:col-span-3">
                  <Checkbox checked={l.taxable} onCheckedChange={(c) => update(l.key, { taxable: !!c })} />
                  Taxed
                  <span className="ml-auto text-sm font-medium tabular-nums sm:hidden">{amt == null ? "–" : money(amt)}</span>
                </label>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Remove line ${i}`}
                  disabled={lines.length <= 1}
                  onClick={() => setLines((ls) => (ls.length > 1 ? ls.filter((x) => x.key !== l.key) : ls))}
                >
                  <Trash2Icon />
                </Button>
              </div>
            )
          })}
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 pt-1">
          <Button type="button" variant="tinted" size="sm" onClick={addLine}>
            <PlusIcon data-icon="inline-start" />
            Add a line
          </Button>
          <div aria-live="polite" className="flex flex-col items-end text-right">
            <span className="flex items-baseline gap-2">
              <span className="text-sm text-muted-foreground">Subtotal</span>
              <strong className="text-lg tabular-nums">{money(subtotal)}</strong>
            </span>
            <span className="text-xs text-muted-foreground">
              {config?.tax_inclusive ? "Prices include tax." : "Tax is added from your tax rules when it's saved."}
            </span>
          </div>
        </div>
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <LabeledField label="Due date">
          {(p) => <Input {...p} type="date" name="due_at" required defaultValue={invoice ? dateInput(invoice.due_at) : todayInput(7)} />}
        </LabeledField>
        <LabeledField label="Notes">
          {(p) => <Textarea {...p} name="notes" rows={2} placeholder="Printed on the invoice" defaultValue={invoice ? invoice.notes || "" : ""} />}
        </LabeledField>
      </div>

      {!invoice && (
        <>
          <div role="radiogroup" aria-label="When" className="grid gap-2 sm:grid-cols-2">
            <ChoiceCard name="mode" value="issue" title="Issue it now" checked={mode === "issue"} onChange={setMode}>
              It gets its number and the client can pay it.
            </ChoiceCard>
            <ChoiceCard name="mode" value="draft" title="Save as a draft" checked={mode === "draft"} onChange={setMode}>
              Nobody sees it until you issue it.
            </ChoiceCard>
          </div>
          <EmailToggle checked={sendEmail} disabled={draft} onChange={setSendEmail} />
        </>
      )}
    </DialogForm>
  )
}

function EmailToggle({ checked, disabled, onChange }: { checked: boolean; disabled: boolean; onChange: (v: boolean) => void }) {
  const id = useId()
  return (
    <div className="flex items-start gap-3">
      <Switch id={id} checked={checked} disabled={disabled} onCheckedChange={onChange} aria-describedby={id + "-help"} className="mt-0.5" />
      <div className="flex flex-col">
        <label htmlFor={id} className="text-sm font-medium">
          E-mail it to the client
        </label>
        <span id={id + "-help"} className="text-sm text-muted-foreground">
          With a link to pay online.
        </span>
      </div>
    </div>
  )
}
