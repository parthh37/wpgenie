import { useRef, useState, type FormEvent } from "react"
import { Building2Icon, MailIcon, WalletIcon } from "lucide-react"
import { BTable, EmptyState, LoadError } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { ContactFields, readContact } from "@/features/billing/shared/contact-fields"
import { InvoiceLink } from "@/features/billing/shared/invoice-document"
import { api, ApiError } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { fmtDueDate, friendly, listOf, methodName, money } from "@/lib/money"
import { invalidate, useApi } from "@/lib/query"
import type { ClientCtx, MailMessage, Payment } from "./data"
import { flatEmpty } from "./parts"
import { TableSkeleton } from "./invoices"

// Payments, billing details and the e-mails sent to the client.

export function ClientPayments() {
  const q = useApi<unknown>("/transactions?limit=100")
  if (q.error) return <LoadError error={q.error} retry={() => void q.refetch()} />
  return (
    <Section>
      {q.isPending ? (
        <TableSkeleton />
      ) : (
        <BTable
          caption="Payments"
          cols={["Date", "Invoice", "Method", "Reference", { label: "Amount", num: true }]}
          rows={listOf<Payment>(q.data, "transactions").map((t) => ({
            key: t.id,
            cells: [
              fmtDueDate(t.at),
              t.invoice_id ? <InvoiceLink invoice={{ id: t.invoice_id, number: t.invoice_number }} /> : "–",
              methodName(t.gateway || t.method),
              <span className="[overflow-wrap:anywhere] whitespace-normal">{t.reference || "–"}</span>,
              <>
                {money(t.amount)}
                {t.refunded ? <span className="block text-xs text-muted-foreground">{money(t.refunded)} refunded</span> : null}
              </>,
            ],
          }))}
          empty={
            <EmptyState icon={WalletIcon} title="No payments yet" className={flatEmpty}>
              Your payments and refunds show up here.
            </EmptyState>
          }
        />
      )}
    </Section>
  )
}

// ---- Billing details: the name and address on invoices ----

export function ClientDetails({ ctx }: { ctx: ClientCtx }) {
  const { acct, profile: p } = ctx
  const [busy, setBusy] = useState(false)
  const [status, setStatus] = useState("")

  if (!p) {
    return (
      <EmptyState icon={Building2Icon} title="Billing isn't set up here">
        There are no billing details to keep.
      </EmptyState>
    )
  }

  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const form = e.currentTarget
    if (!form.reportValidity()) return
    form.querySelectorAll("[aria-invalid]").forEach((x) => x.removeAttribute("aria-invalid"))
    setBusy(true)
    setStatus("Saving…")
    try {
      await api("PUT", `/accounts/${acct.id}/billing/contact`, readContact(form))
      setStatus("Saved.")
      notify("Billing details saved")
      void invalidate(`/accounts/${acct.id}/billing`)
    } catch (err) {
      setStatus("")
      showError(friendly(err))
      // The API names the field at fault ("contact.email"): show it there.
      const name = err instanceof ApiError ? (err.data.field as string | undefined) : undefined
      const el = name ? (form.elements.namedItem(name.split(".").pop()!) as HTMLElement | null) : null
      if (el && "focus" in el) {
        el.setAttribute("aria-invalid", "true")
        el.focus()
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section title="Billing details" description="The name and address on your invoices, and where we send them. Taxes depend on your country.">
      <form onSubmit={save} onInput={() => setStatus("")} className="flex flex-col gap-5">
        <ContactFields contact={p.contact || {}} />
        <div className="flex flex-wrap items-center justify-end gap-3">
          <span aria-live="polite" className="text-sm text-muted-foreground">
            {status}
          </span>
          <Button type="submit" disabled={busy}>
            Save details
          </Button>
        </div>
      </form>
    </Section>
  )
}

// ---- E-mails: the messages sent to the client ----

export function ClientEmails({ ctx }: { ctx: ClientCtx }) {
  const { acct } = ctx
  const q = useApi<MailMessage[]>(`/accounts/${acct.id}/emails?limit=100`)
  const [shown, setShown] = useState<MailMessage | null>(null)
  const [open, setOpen] = useState(false)
  const show = (m: MailMessage) => {
    setShown(m)
    setOpen(true)
  }

  if (q.error) return <LoadError error={q.error} retry={() => void q.refetch()} />
  return (
    <Section description="Messages we've sent you: invoices, receipts and reminders.">
      {q.isPending ? (
        <TableSkeleton />
      ) : (
        <BTable
          caption="E-mails"
          cols={["Sent", "Subject", { label: <span className="sr-only">Delivery</span> }]}
          rows={listOf<MailMessage>(q.data, "emails").map((m) => ({
            key: m.id,
            onOpen: () => show(m),
            cells: [
              <span className="whitespace-nowrap">{fmtTime(m.sent_at || m.created_at)}</span>,
              <button
                type="button"
                className="text-left font-medium whitespace-normal text-link underline-offset-4 hover:underline"
                onClick={() => show(m)}
              >
                {m.subject}
              </button>,
              m.status === "sent" ? null : (
                <StatusPill status={m.status} tone={m.status === "failed" ? "bad" : "warn"}>
                  {m.status === "failed" ? "not delivered" : "sending"}
                </StatusPill>
              ),
            ],
          }))}
          empty={
            <EmptyState icon={MailIcon} title="No e-mails yet" className={flatEmpty}>
              Invoices and receipts we e-mail you also appear here.
            </EmptyState>
          }
        />
      )}
      {shown && <MailFrame accountId={acct.id} mail={shown} open={open} onOpenChange={setOpen} />}
    </Section>
  )
}

// MailFrame shows a message in a sandboxed frame: no scripts, no access to
// the panel; its links open outside it.
function MailFrame({ accountId, mail, open, onOpenChange }: { accountId: number; mail: MailMessage; open: boolean; onOpenChange: (o: boolean) => void }) {
  const closeRef = useRef<HTMLButtonElement>(null)
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent initialFocus={closeRef} className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{mail.subject}</DialogTitle>
        </DialogHeader>
        <iframe
          title={mail.subject}
          src={`/api/v1/accounts/${accountId}/emails/${mail.id}/html`}
          sandbox="allow-popups allow-popups-to-escape-sandbox"
          referrerPolicy="no-referrer"
          className="block h-[min(62vh,640px)] w-full rounded-xl border border-border bg-white"
        />
        <DialogFooter>
          <Button ref={closeRef} type="button" variant="tinted" onClick={() => onOpenChange(false)}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
