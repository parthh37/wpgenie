import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { BanIcon, BanknoteIcon, ChevronLeftIcon, MailIcon, PencilIcon, PrinterIcon, ReceiptIcon, RotateCcwIcon, SendIcon, TagIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { INVOICE_KINDS, dueLine, dueText, listOf } from "@/lib/money"
import { useApi } from "@/lib/query"
import { href } from "@/lib/router"
import { useSession } from "@/lib/session"
import { billingPath } from "../route"
import { EmailFrameDialog } from "../shared/email-frame"
import { COMPACT, InvoiceDocument, InvoicePill, PaymentsTable, titleOf } from "../shared/invoice-document"
import { refreshBilling } from "../shared/refresh"
import type { Invoice, SentEmail } from "../shared/types"
import { ApplyCreditDialog, EditUnpaidDialog, RecordPaymentDialog, RefundDialog, remindInvoice } from "./invoice-actions"
import { useInvoiceEditor } from "./invoice-editor"

type DialogKind = "pay" | "refund" | "credit" | "edit"

// One invoice for staff: the document, its payments, the e-mails about it,
// and what can be done to it.
export function StaffInvoice({ id }: { id: string }) {
  const q = useApi<Invoice>(`/invoices/${encodeURIComponent(id)}`)
  return (
    <>
      <a href={href(billingPath("invoices"))} className="mb-3 inline-flex items-center gap-1 text-sm font-medium">
        <ChevronLeftIcon className="size-4" />
        All invoices
      </a>
      {q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} />
      ) : !q.data ? (
        <div aria-busy="true" className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_340px]">
          <Skeleton className="h-[560px] rounded-3xl" />
          <Skeleton className="h-48 rounded-2xl" />
        </div>
      ) : (
        <InvoiceScreen inv={q.data} reload={() => q.refetch()} />
      )}
    </>
  )
}

function InvoiceScreen({ inv, reload }: { inv: Invoice; reload: () => void }) {
  const s = useSession()
  const admin = s.atLeast("admin")
  const [dlg, setDlg] = useState<DialogKind | null>(null)
  const [mail, setMail] = useState<SentEmail | null>(null)
  const [mailOpen, setMailOpen] = useState(false)
  const editor = useInvoiceEditor(reload)
  const close = (o: boolean) => !o && setDlg(null)
  const done = () => reload()

  // E-mails about it: the account's invoice messages naming its number.
  const mails = useQuery({
    queryKey: [`/accounts/${inv.account_id}/emails?limit=100`],
    queryFn: () => api<SentEmail[]>("GET", `/accounts/${inv.account_id}/emails?limit=100`).catch(() => [] as SentEmail[]),
    enabled: inv.account_id != null,
  })
  const about = listOf<SentEmail>(mails.data, "emails").filter(
    (m) => (m.template || "").startsWith("invoice.") && (!inv.number || (m.subject || "").includes(inv.number))
  )

  const st = inv.status
  const title = titleOf(inv)
  const due = dueText({ status: st || "", due_at: inv.due_at || undefined })

  return (
    <>
      <div className="mb-4 flex flex-wrap items-end justify-between gap-x-4 gap-y-3">
        <div className="min-w-0">
          <h2 className="flex flex-wrap items-center gap-2.5 text-[1.375rem] font-bold tracking-[-0.01em]">
            {title}
            <InvoicePill invoice={inv} />
          </h2>
          <p className="text-sm text-muted-foreground">
            <a href={href("/accounts/" + inv.account_id)} className="font-medium">
              {inv.account_name || `Account #${inv.account_id}`}
            </a>
            {` · ${INVOICE_KINDS[inv.kind || ""] || "Invoice"}`}
            {due ? ` · ${dueLine({ status: st || "", due_at: inv.due_at || undefined })}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="tinted" nativeButton={false} render={<a href={`/api/v1/invoices/${inv.id}/print`} target="_blank" rel="noopener" />}>
            <PrinterIcon data-icon="inline-start" />
            Print / PDF
          </Button>
          {admin && st === "draft" && (
            <>
              <ActionButton run={() => editor.open({ invoice: inv })}>
                <PencilIcon data-icon="inline-start" />
                Edit
              </ActionButton>
              <ActionButton
                variant="default"
                run={async () => {
                  if (!(await ask(`Issue ${title}? It gets its number and is e-mailed to the client with a link to pay.`, { ok: "Issue invoice" }))) return
                  await api("POST", `/invoices/${inv.id}/issue`)
                  notify("Invoice issued")
                  refreshBilling(inv.account_id)
                  reload()
                }}
              >
                <SendIcon data-icon="inline-start" />
                Issue
              </ActionButton>
            </>
          )}
          {admin && st === "unpaid" && (
            <>
              <Button onClick={() => setDlg("pay")}>
                <BanknoteIcon data-icon="inline-start" />
                Record payment
              </Button>
              <ActionButton run={() => remindInvoice(inv)}>
                <SendIcon data-icon="inline-start" />
                Send reminder
              </ActionButton>
              <Button variant="tinted" onClick={() => setDlg("credit")}>
                <TagIcon data-icon="inline-start" />
                Apply credit
              </Button>
              <Button variant="tinted" onClick={() => setDlg("edit")}>
                <PencilIcon data-icon="inline-start" />
                Edit
              </Button>
            </>
          )}
          {admin && (inv.payments || []).some((p) => p.amount > (p.refunded || 0)) && (
            <Button variant="tinted" onClick={() => setDlg("refund")}>
              <RotateCcwIcon data-icon="inline-start" />
              Refund
            </Button>
          )}
          {admin && (st === "draft" || st === "unpaid") && (
            <ActionButton
              variant="destructive"
              run={async () => {
                // A cancelled renewal waives its period (the server moves the next
                // due date past it), so billing carries on with the next one.
                const what =
                  inv.kind === "renewal"
                    ? "Cancelling a renewal invoice waives that period: the client isn't billed for it, and their next due date moves to the end of the period. Billing continues with the next period."
                    : "The client no longer needs to pay it."
                if (!(await ask(`Cancel ${title}? ${what} Payments already made stay recorded.`, { ok: "Cancel invoice", danger: true }))) return
                await api("POST", `/invoices/${inv.id}/cancel`)
                notify("Invoice cancelled")
                refreshBilling(inv.account_id)
                reload()
              }}
            >
              <BanIcon data-icon="inline-start" />
              Cancel invoice
            </ActionButton>
          )}
        </div>
      </div>

      <div className="grid items-start gap-4 xl:grid-cols-[minmax(0,1fr)_340px]">
        <InvoiceDocument invoice={inv} />
        <aside className="flex min-w-0 flex-col gap-4">
          <Section icon={ReceiptIcon} tint="mint" title="Payments" className="mb-0">
            <PaymentsTable invoice={inv} />
          </Section>
          <Section icon={MailIcon} tint="blue" title="E-mails about it" className="mb-0">
            <BTable
              className={COMPACT}
              cols={["Sent", "Subject", ""]}
              rows={about.map((m) => ({
                key: m.id,
                cells: [
                  <span className="whitespace-nowrap">{fmtTime(m.sent_at || m.created_at)}</span>,
                  <span className="whitespace-normal">{m.subject}</span>,
                  <Button
                    variant="link"
                    size="sm"
                    className="h-auto px-0"
                    onClick={() => {
                      setMail(m)
                      setMailOpen(true)
                    }}
                  >
                    View
                  </Button>,
                ],
              }))}
              empty={<p className="py-2 text-sm text-muted-foreground">{mails.isLoading ? "Loading…" : "None yet."}</p>}
            />
          </Section>
        </aside>
      </div>

      {editor.dialog}
      <RecordPaymentDialog open={dlg === "pay"} onOpenChange={close} invoice={inv} onDone={done} />
      <RefundDialog open={dlg === "refund"} onOpenChange={close} invoice={inv} onDone={done} />
      <ApplyCreditDialog open={dlg === "credit"} onOpenChange={close} invoice={inv} onDone={done} />
      <EditUnpaidDialog open={dlg === "edit"} onOpenChange={close} invoice={inv} onDone={done} />
      {mail && (
        <EmailFrameDialog open={mailOpen} onOpenChange={setMailOpen} title={mail.subject} url={`/api/v1/accounts/${inv.account_id}/emails/${mail.id}/html`} />
      )}
    </>
  )
}
