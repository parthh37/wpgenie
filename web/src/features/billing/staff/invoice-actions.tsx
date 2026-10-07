import { useState } from "react"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Textarea } from "@/components/ui/textarea"
import { ChoiceCard, MoneyInput } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { dateInput, fmtDueDate, fromDateInput, fromMinor, methodName, money, toMinor, todayInput } from "@/lib/money"
import { BillingDialog, DialogForm, Hint, LabeledField, fieldError } from "../shared/dialog-form"
import { titleOf } from "../shared/invoice-document"
import { refreshBilling } from "../shared/refresh"
import type { Invoice, Payment } from "../shared/types"

// What staff do to an invoice: remind, record a payment, refund, apply
// credit, change an issued one's due date and notes.

const value = (f: HTMLFormElement, name: string) =>
  ((f.elements.namedItem(name) as HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement | null)?.value ?? "")

export async function remindInvoice(inv: Invoice) {
  if (
    !(await ask(`Send a reminder for ${titleOf(inv)} now? The client gets the reminder e-mail with a link to pay ${money(inv.balance)}.`, {
      ok: "Send reminder",
    }))
  )
    return
  await api("POST", `/invoices/${inv.id}/remind`)
  notify(`Reminder sent for ${titleOf(inv)}`)
  refreshBilling(inv.account_id)
}

interface DialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  invoice: Invoice
  onDone?: () => void
}

// ---- Record a payment received outside the panel ----

export function RecordPaymentDialog({ open, onOpenChange, invoice, onDone }: DialogProps) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange}>
      <RecordPaymentForm inv={invoice} onDone={onDone} onClose={() => onOpenChange(false)} />
    </BillingDialog>
  )
}

function RecordPaymentForm({ inv, onDone, onClose }: { inv: Invoice; onDone?: () => void; onClose: () => void }) {
  const balance = inv.balance || 0
  const [text, setText] = useState(fromMinor(balance))
  const a = toMinor(text)
  const hint =
    a == null || Number.isNaN(a) || a <= 0
      ? "Enter the amount you received."
      : a === balance
        ? "This pays the invoice in full."
        : a < balance
          ? `${money(balance - a)} will still be due.`
          : `${money(a - balance)} more than due goes to the client's credit.`
  return (
    <DialogForm
      title={`Record a payment for ${titleOf(inv)}`}
      ok="Record payment"
      intro="For money received outside the panel: a bank transfer, cash or a cheque. Card and Razorpay payments are recorded by themselves."
      onClose={onClose}
      onSubmit={async (f) => {
        const amount = toMinor(value(f, "amount"))
        if (amount == null || Number.isNaN(amount) || amount <= 0) throw fieldError("amount", "Enter an amount above zero.")
        await api("POST", `/invoices/${inv.id}/payments`, {
          amount,
          method: value(f, "method"),
          reference: value(f, "reference").trim(),
          at: fromDateInput(value(f, "at")),
          note: value(f, "note").trim(),
        })
        notify(`Payment of ${money(amount)} recorded`)
        refreshBilling(inv.account_id)
        onDone?.()
      }}
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <LabeledField label="Amount">{(p) => <MoneyInput {...p} name="amount" minor={balance} required onChange={(e) => setText(e.target.value)} />}</LabeledField>
        <LabeledField label="Method">
          {(p) => (
            <NativeSelect {...p} name="method" defaultValue="bank" className="w-full">
              <NativeSelectOption value="bank">Bank transfer</NativeSelectOption>
              <NativeSelectOption value="cash">Cash</NativeSelectOption>
              <NativeSelectOption value="cheque">Cheque</NativeSelectOption>
              <NativeSelectOption value="other">Other</NativeSelectOption>
            </NativeSelect>
          )}
        </LabeledField>
        <LabeledField label="Received on">{(p) => <Input {...p} type="date" name="at" defaultValue={todayInput()} required />}</LabeledField>
        <LabeledField label="Reference">{(p) => <Input {...p} name="reference" placeholder="e.g. the bank's transaction ID" />}</LabeledField>
      </div>
      <LabeledField label="Note">{(p) => <Input {...p} name="note" placeholder="Only staff see it" />}</LabeledField>
      <Hint>{hint}</Hint>
    </DialogForm>
  )
}

// ---- Refund a payment, to where it came from or as credit ----

const refundable = (p: Payment) => p.amount - (p.refunded || 0)

export function RefundDialog({ open, onOpenChange, invoice, onDone }: DialogProps) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange}>
      <RefundForm inv={invoice} onDone={onDone} onClose={() => onOpenChange(false)} />
    </BillingDialog>
  )
}

function RefundForm({ inv, onDone, onClose }: { inv: Invoice; onDone?: () => void; onClose: () => void }) {
  const pays = (inv.payments || []).filter((p) => p.amount > (p.refunded || 0))
  const [payId, setPayId] = useState(String(pays[0]?.id ?? ""))
  const p = pays.find((x) => String(x.id) === payId) || pays[0]
  const online = !!p && ["stripe", "razorpay"].includes(p.gateway || "")
  const [to, setTo] = useState(online ? "gateway" : "credit")
  const [text, setText] = useState(p ? fromMinor(refundable(p)) : "")
  const toNow = online ? to : "credit"
  const a = toMinor(text)

  if (!p) {
    return (
      <DialogForm title={`Refund ${titleOf(inv)}`} noOk cancel="Close" onClose={onClose}>
        <p className="text-sm text-muted-foreground">No payments yet.</p>
      </DialogForm>
    )
  }
  return (
    <DialogForm
      title={`Refund ${titleOf(inv)}`}
      ok="Refund"
      danger
      onClose={onClose}
      onSubmit={async (f) => {
        const amount = toMinor(value(f, "amount"))
        if (amount == null || Number.isNaN(amount) || amount <= 0) throw fieldError("amount", "Enter an amount above zero.")
        await api("POST", `/invoices/${inv.id}/refund`, { payment_id: Number(payId), amount, to: toNow })
        notify(`${money(amount)} refunded`)
        refreshBilling(inv.account_id)
        onDone?.()
      }}
    >
      <LabeledField label="Payment">
        {(a11y) => (
          <NativeSelect
            {...a11y}
            name="payment_id"
            className="w-full"
            value={payId}
            onChange={(e) => {
              const next = pays.find((x) => String(x.id) === e.target.value)
              setPayId(e.target.value)
              if (next) setText(fromMinor(refundable(next)))
            }}
          >
            {pays.map((x) => (
              <NativeSelectOption key={x.id} value={String(x.id)}>
                {`${fmtDueDate(x.at)} · ${methodName(x.gateway)} · ${money(refundable(x))} refundable`}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </LabeledField>
      <LabeledField label="Amount">
        {(a11y) => <MoneyInput {...a11y} key={payId} name="amount" minor={refundable(p)} required onChange={(e) => setText(e.target.value)} />}
      </LabeledField>
      <fieldset className="flex flex-col gap-2">
        <legend className="mb-2 text-sm font-medium">Refund to</legend>
        {online && (
          <ChoiceCard name="to" value="gateway" title={`Back to the ${methodName(p.gateway).toLowerCase()}`} checked={toNow === "gateway"} onChange={setTo}>
            The money goes back the way it came; it takes a few days to arrive.
          </ChoiceCard>
        )}
        <ChoiceCard name="to" value="credit" title="As account credit" checked={toNow === "credit"} onChange={setTo}>
          Kept on the account and used for its next invoices.
        </ChoiceCard>
      </fieldset>
      <Hint>{a != null && !Number.isNaN(a) && a > refundable(p) ? `At most ${money(refundable(p))} of this payment can be refunded.` : ""}</Hint>
    </DialogForm>
  )
}

// ---- Pay from the account's credit ----

export function ApplyCreditDialog({ open, onOpenChange, invoice, onDone }: DialogProps) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange}>
      <DialogForm
        title={`Apply credit to ${titleOf(invoice)}`}
        ok="Apply credit"
        intro="Pays the invoice from the credit held on the account (refunds, goodwill, over-payments)."
        onClose={() => onOpenChange(false)}
        onSubmit={async (f) => {
          const a = toMinor(value(f, "amount"))
          if (Number.isNaN(a) || (a != null && a <= 0)) throw fieldError("amount", "Enter an amount above zero, or leave it empty.")
          await api("POST", `/invoices/${invoice.id}/apply-credit`, { amount: a })
          notify("Credit applied")
          refreshBilling(invoice.account_id)
          onDone?.()
        }}
      >
        <LabeledField label="Amount" help={`Leave empty to use up to ${money(invoice.balance)}.`}>
          {(p) => <MoneyInput {...p} name="amount" minor={null} placeholder="as much as is available" />}
        </LabeledField>
      </DialogForm>
    </BillingDialog>
  )
}

// ---- An issued invoice: its due date and notes ----

export function EditUnpaidDialog({ open, onOpenChange, invoice, onDone }: DialogProps) {
  return (
    <BillingDialog open={open} onOpenChange={onOpenChange}>
      <DialogForm
        title={`Edit ${titleOf(invoice)}`}
        intro="An issued invoice keeps its lines; its due date and notes can change."
        onClose={() => onOpenChange(false)}
        onSubmit={async (f) => {
          await api("PUT", `/invoices/${invoice.id}`, { due_at: fromDateInput(value(f, "due_at")), notes: value(f, "notes") })
          notify("Invoice updated")
          refreshBilling(invoice.account_id)
          onDone?.()
        }}
      >
        <LabeledField label="Due date">{(p) => <Input {...p} type="date" name="due_at" defaultValue={dateInput(invoice.due_at)} required className="sm:w-56" />}</LabeledField>
        <LabeledField label="Notes" help="Printed on the invoice.">
          {(p) => <Textarea {...p} name="notes" rows={3} defaultValue={invoice.notes || ""} />}
        </LabeledField>
      </DialogForm>
    </BillingDialog>
  )
}
