import { useRef, useState, type ReactNode } from "react"
import { ChoiceCard, CopyField, FormDialog } from "@/components/app/blocks"
import { notify, showError } from "@/components/app/toaster"
import { Switch } from "@/components/ui/switch"
import { api } from "@/lib/api"
import { fmtNum } from "@/lib/format"
import { billingMethods, invoiceTitle, listOf, methodName, money, useBillingConfig } from "@/lib/money"
import { queryClient } from "@/lib/query"
import { navigate } from "@/lib/router"
import { billingPath } from "../route"
import { burstPacks, reloadBilling, setInvoiceFilter, type ClientInvoice, type PayNext, type Profile } from "./data"

// Paying: choose how, then off to the payment page (Stripe, Razorpay), or
// the bank details to transfer to; account credit first when there is
// some. Burst minute packs are bought the same way (the purchase is an
// invoice).

const METHOD_TEXT: Record<string, string> = {
  stripe: "Pay securely on Stripe's page.",
  razorpay: "UPI, cards, netbanking and wallets.",
  manual: "We show you our bank details.",
}

type Stage = { kind: "form" } | { kind: "redirect" } | { kind: "bank"; instructions: string; reference?: string; amount: number }

// followPayNext acts on the answer to a payment: off to the payment page,
// bank details shown in the dialog, or paid. Returns what the dialog's
// submit should (false: stay open).
function followPayNext(r: PayNext, amount: number, setStage: (s: Stage) => void): boolean {
  if (r.redirect_url) {
    const url = new URL(r.redirect_url, location.href)
    if (url.protocol !== "https:" && url.origin !== location.origin) throw new Error("The payment page address looks wrong; please contact us.")
    setStage({ kind: "redirect" })
    location.assign(url.href)
    return false
  }
  if (r.paid) {
    notify("Paid, thank you!")
    return true
  }
  if (r.instructions != null) {
    setStage({ kind: "bank", instructions: r.instructions, reference: r.reference, amount })
    return false
  }
  // The gateway failed to start: the invoice can be paid later.
  if (r.error) throw new Error(r.error)
  return true
}

function Redirecting() {
  return <p className="text-sm">Taking you to the payment page…</p>
}

// Bank transfer: the details, and the reference to quote.
function BankDetails({ stage }: { stage: Extract<Stage, { kind: "bank" }> }) {
  return (
    <div className="flex flex-col gap-3 text-sm">
      <p>Please transfer {money(stage.amount)} using these details:</p>
      <pre className="m-0 rounded-xl bg-muted px-4 py-3 font-mono text-sm leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]">
        {stage.instructions}
      </pre>
      {stage.reference && (
        <div className="flex flex-col gap-1">
          <span className="font-medium">Payment reference</span>
          <CopyField text={stage.reference} className="rounded-xl bg-muted px-3" />
          <span className="text-muted-foreground">Quote it so we can match your payment.</span>
        </div>
      )}
      <p className="text-muted-foreground">
        We'll mark the invoice paid and e-mail a receipt when the money arrives, usually within 1–2 working days.
      </p>
    </div>
  )
}

// A switch with its label and an optional line of help under it.
function Toggle({
  checked,
  onChange,
  label,
  help,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label: ReactNode
  help?: ReactNode
  disabled?: boolean
}) {
  return (
    <label className="flex cursor-pointer items-start gap-3 has-data-disabled:cursor-not-allowed">
      <Switch checked={checked} disabled={disabled} onCheckedChange={(v) => onChange(v)} className="mt-0.5" />
      <span className="flex min-w-0 flex-col gap-0.5">
        <span className="text-sm font-medium">{label}</span>
        {help && <span className="text-sm text-muted-foreground">{help}</span>}
      </span>
    </label>
  )
}

function Choices({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <fieldset className="m-0 flex min-w-0 flex-col gap-2 border-0 p-0">
      <legend className="mb-2 text-sm font-medium">{label}</legend>
      <div className={className ?? "flex flex-col gap-2"}>{children}</div>
    </fieldset>
  )
}

// ---- Paying an invoice ----

function PayDialog({
  invoice,
  profile,
  open,
  onClose,
}: {
  invoice: ClientInvoice
  profile: Profile | null | undefined
  open: boolean
  onClose: (changed: boolean) => void
}) {
  const methods = billingMethods()
  const credit = profile?.credit || 0
  const [inv, setInv] = useState(invoice)
  const [method, setMethod] = useState(methods[0]?.id ?? "")
  const [saveCard, setSaveCard] = useState(!profile?.card)
  const [creditShown, setCreditShown] = useState(credit > 0)
  const [useCredit, setUseCredit] = useState(credit > 0)
  const [stage, setStage] = useState<Stage>({ kind: "form" })
  // Something changed (paid, or credit used): the screens reload.
  const changed = useRef(false)

  const fromCredit = creditShown && useCredit ? Math.min(credit, inv.balance) : 0
  const due = inv.balance - fromCredit

  async function submit() {
    let current = inv
    if (creditShown && useCredit) {
      await api("POST", `/invoices/${current.id}/apply-credit`, { amount: null })
      changed.current = true
      current = await api<ClientInvoice>("GET", `/invoices/${current.id}`)
      setInv(current)
      if (current.status === "paid" || current.balance <= 0) {
        notify("Paid from your credit, thank you!")
        return true
      }
      setUseCredit(false)
      setCreditShown(false)
    }
    if (!method) throw new Error("Choose how to pay.")
    const r = await api<PayNext>("POST", `/invoices/${current.id}/pay`, { method, save_card: method === "stripe" && saveCard })
    const done = followPayNext(r, current.balance, setStage)
    if (done) changed.current = true
    return done
  }

  return (
    <FormDialog
      open={open}
      onOpenChange={(o) => !o && onClose(changed.current)}
      title={`Pay ${invoiceTitle(inv)}`}
      ok="Continue"
      cancel={stage.kind === "bank" ? "Done" : "Cancel"}
      noOk={stage.kind === "bank"}
      okDisabled={stage.kind === "redirect"}
      onSubmit={submit}
    >
      {stage.kind === "redirect" ? (
        <Redirecting />
      ) : stage.kind === "bank" ? (
        <BankDetails stage={stage} />
      ) : (
        <div className="flex flex-col gap-4">
          {methods.length > 0 && (
            <Choices label="How would you like to pay?">
              {methods.map((m) => (
                <ChoiceCard
                  key={m.id}
                  name="method"
                  value={m.id}
                  title={m.name || methodName(m.id)}
                  checked={method === m.id}
                  onChange={setMethod}
                  disabled={due <= 0}
                >
                  {m.description || METHOD_TEXT[m.id] || ""}
                </ChoiceCard>
              ))}
            </Choices>
          )}
          {method === "stripe" && (
            <Toggle
              checked={saveCard}
              onChange={setSaveCard}
              label="Save my card for automatic payments"
              help="Your card details stay with Stripe."
            />
          )}
          {creditShown && <Toggle checked={useCredit} onChange={setUseCredit} label={`Use my credit first (${money(credit)} available)`} />}
          <p aria-live="polite" className="m-0 flex flex-wrap items-baseline justify-between gap-x-3 rounded-xl bg-muted px-4 py-3">
            <span>{due > 0 ? "You pay" : "Paid from your credit"}</span>
            <strong className="text-lg font-semibold tabular-nums">{money(due > 0 ? due : fromCredit)}</strong>
          </p>
        </div>
      )}
    </FormDialog>
  )
}

// usePayFlow: pay(invoice) opens the payment dialog; payDue() pays the one
// invoice due, or shows them when there are several. Render `dialog`.
export function usePayFlow(profile: Profile | null | undefined, onChanged: () => unknown = reloadBilling) {
  const [target, setTarget] = useState<{ inv: ClientInvoice; seq: number } | null>(null)
  const [open, setOpen] = useState(false)
  const seq = useRef(0)

  const pay = (inv: ClientInvoice) => {
    if (!billingMethods().length && !(profile?.credit || 0)) {
      showError(new Error("No payment methods are set up yet. Please contact us to pay this invoice."))
      return
    }
    setTarget({ inv, seq: ++seq.current })
    setOpen(true)
  }

  const payDue = async () => {
    const unpaid = listOf<ClientInvoice>(await api("GET", "/invoices?status=unpaid&limit=50"), "invoices")
    if (unpaid.length === 1) {
      pay(unpaid[0])
    } else {
      setInvoiceFilter("unpaid")
      navigate(billingPath("invoices"))
    }
  }

  const dialog = target && (
    <PayDialog
      key={target.seq}
      invoice={target.inv}
      profile={profile}
      open={open}
      onClose={(changed) => {
        setOpen(false)
        if (changed) onChanged()
      }}
    />
  )
  return { pay, payDue, dialog }
}

// ---- Buying burst minute packs ----

function BuyBurstDialog({
  accountId,
  profile,
  open,
  onClose,
}: {
  accountId: number
  profile: Profile | null | undefined
  open: boolean
  onClose: (paid: boolean) => void
}) {
  const { data: cfg } = useBillingConfig()
  const packs = burstPacks(cfg)
    .slice()
    .sort((a, b) => a.minutes - b.minutes)
  const credit = profile?.credit || 0
  const methods = billingMethods()
  const [packId, setPackId] = useState(packs[0]?.id ?? "")
  const [picked, setPicked] = useState("")
  const [stage, setStage] = useState<Stage>({ kind: "form" })
  const paid = useRef(false)

  const pack = packs.find((p) => p.id === packId) || packs[0]
  const opts = [...(pack && credit >= pack.price ? [{ id: "credit", name: "My credit", description: `${money(credit)} available` }] : []), ...methods]
  // The method chosen stays when it's still on offer; otherwise the first.
  const method = opts.some((m) => m.id === picked) ? picked : opts[0]?.id

  async function submit() {
    if (!pack) throw new Error("No burst minute packs are for sale right now.")
    if (!method) throw new Error("No payment method is set up yet: please contact us.")
    const r = await api<{ invoice?: ClientInvoice; next?: PayNext }>("POST", `/accounts/${accountId}/burst/buy`, { pack: pack.id, method })
    // An invoice now exists (paid or not): the screens show it.
    void reloadBilling()
    void queryClient.invalidateQueries({ predicate: (q) => typeof q.queryKey[0] === "string" && q.queryKey[0].includes("/burst") })
    const next = r.next || {}
    if (next.paid || method === "credit") {
      notify(`${fmtNum(pack.minutes)} burst minutes added, thank you!`)
      paid.current = true
      return true
    }
    const done = followPayNext(next, r.invoice?.balance ?? pack.price, setStage)
    if (done) paid.current = true
    return done
  }

  return (
    <FormDialog
      open={open}
      onOpenChange={(o) => !o && onClose(paid.current)}
      title="Buy burst minutes"
      intro="Bought minutes never expire: they're used once the month's included minutes run out, and your sites keep their extra capacity under load."
      ok="Buy"
      wide
      cancel={stage.kind === "bank" ? "Done" : "Cancel"}
      noOk={stage.kind === "bank"}
      okDisabled={stage.kind === "redirect"}
      onSubmit={submit}
    >
      {stage.kind === "redirect" ? (
        <Redirecting />
      ) : stage.kind === "bank" ? (
        <BankDetails stage={stage} />
      ) : (
        <div className="flex flex-col gap-5">
          <Choices label="Pack" className="grid gap-2 sm:grid-cols-2">
            {packs.map((p) => (
              <ChoiceCard
                key={p.id}
                name="pack"
                value={p.id}
                title={`${fmtNum(p.minutes)} minutes`}
                checked={pack?.id === p.id}
                onChange={(v) => {
                  if (method) setPicked(method)
                  setPackId(v)
                }}
              >
                {`${money(p.price)} · ${money(Math.round((p.price / p.minutes) * 60))} an hour of extra capacity`}
              </ChoiceCard>
            ))}
          </Choices>
          <Choices label="Pay with" className="grid gap-2 sm:grid-cols-2">
            {opts.length ? (
              opts.map((m) => (
                <ChoiceCard key={m.id} name="method" value={m.id} title={m.name || methodName(m.id)} checked={method === m.id} onChange={setPicked}>
                  {m.description || ""}
                </ChoiceCard>
              ))
            ) : (
              <p className="text-sm text-muted-foreground">No payment method is set up yet: please contact us.</p>
            )}
          </Choices>
        </div>
      )}
    </FormDialog>
  )
}

// useBuyBurst: open() sells a pack of burst minutes; onPaid runs once
// they're paid for. Render `dialog`.
export function useBuyBurst(accountId: number | undefined, profile: Profile | null | undefined, onPaid: () => unknown) {
  const { data: cfg } = useBillingConfig()
  const [seq, setSeq] = useState(0)
  const [open, setOpen] = useState(false)

  const start = () => {
    if (!burstPacks(cfg).length) {
      showError(new Error("No burst minute packs are for sale right now."))
      return
    }
    setSeq((n) => n + 1)
    setOpen(true)
  }

  const dialog =
    seq > 0 && accountId != null ? (
      <BuyBurstDialog
        key={seq}
        accountId={accountId}
        profile={profile}
        open={open}
        onClose={(paid) => {
          setOpen(false)
          if (paid) onPaid()
        }}
      />
    ) : null
  return { open: start, dialog }
}
