import { useEffect, useState } from "react"
import { CheckIcon, ChevronLeftIcon, ClockIcon, CreditCardIcon, LoaderCircleIcon, PrinterIcon, TagIcon } from "lucide-react"
import { ActionButton, Banner, Chips, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { InvoiceDocument, PaymentsTable } from "@/features/billing/shared/invoice-document"
import { api } from "@/lib/api"
import { dueLine, dueText, INVOICE_KINDS, invoiceTitle, listOf, money } from "@/lib/money"
import { queryClient, useApi } from "@/lib/query"
import { href, navigate } from "@/lib/router"
import { billingPath } from "../route"
import { reloadBilling, setInvoiceFilter, useInvoiceFilter, type ClientCtx, type ClientInvoice } from "./data"
import { ClientInvoiceTable, InvoicePill } from "./parts"

// Invoices: the list (all, to pay, paid) and an invoice with its document,
// payments, and paying it. Back from a payment page (?paid=1) it waits for
// the provider to confirm the payment.

const FILTERS: Array<[string, string]> = [
  ["", "All"],
  ["unpaid", "To pay"],
  ["paid", "Paid"],
]

export function ClientInvoices({ ctx, id, query }: { ctx: ClientCtx; id: string; query: URLSearchParams }) {
  if (id) return <InvoiceDetail key={id} ctx={ctx} id={id} returned={query.get("paid") === "1"} />
  return <InvoiceList ctx={ctx} />
}

function InvoiceList({ ctx }: { ctx: ClientCtx }) {
  const filter = useInvoiceFilter()
  const q = useApi<unknown>("/invoices?limit=100" + (filter ? "&status=" + filter : ""))
  return (
    <Section>
      <Chips label="Show invoices" items={FILTERS} current={filter} onPick={setInvoiceFilter} className="mb-3" />
      {q.error ? (
        <LoadError error={q.error} retry={() => void q.refetch()} />
      ) : q.isPending ? (
        <TableSkeleton />
      ) : (
        <ClientInvoiceTable list={listOf<ClientInvoice>(q.data, "invoices")} pay={ctx.pay} />
      )}
    </Section>
  )
}

export function TableSkeleton() {
  return (
    <div aria-busy="true" className="flex flex-col gap-2">
      {[0, 1, 2, 3].map((i) => (
        <Skeleton key={i} className="h-10 rounded-lg" />
      ))}
    </div>
  )
}

type Confirming = "waiting" | "ok" | "late" | null
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

function InvoiceDetail({ ctx, id, returned }: { ctx: ClientCtx; id: string; returned: boolean }) {
  const path = `/invoices/${encodeURIComponent(id)}`
  const q = useApi<ClientInvoice>(path)
  // Back from the payment page: the payment is confirmed to us by the
  // provider, usually within seconds. Wait for it here.
  const [back] = useState(returned)
  const [confirming, setConfirming] = useState<Confirming>(back ? "waiting" : null)

  useEffect(() => {
    if (!back) return
    let live = true
    void (async () => {
      for (let i = 0; i < 31 && live; i++) {
        if (i > 0) await sleep(2000)
        if (!live) return
        let inv: ClientInvoice
        try {
          inv = await api<ClientInvoice>("GET", path)
        } catch {
          continue
        }
        if (inv.status === "paid") {
          queryClient.setQueryData([path], inv)
          navigate(billingPath("invoices", inv.id), { replace: true })
          // Everything changes: banners, balance, perhaps the account's status.
          await reloadBilling()
          if (live) setConfirming("ok")
          return
        }
      }
      if (live) setConfirming("late")
    })()
    return () => {
      live = false
    }
  }, [back, path])

  if (q.error) return <LoadError error={q.error} retry={() => void q.refetch()} />
  const inv = q.data
  const credit = ctx.profile?.credit || 0

  return (
    <>
      <a
        href={href(billingPath("invoices"))}
        className="mb-3 inline-flex items-center gap-1 text-sm font-medium text-link no-underline hover:underline"
      >
        <ChevronLeftIcon className="size-4" />
        All invoices
      </a>
      <div aria-live="polite">
        {confirming === "waiting" && (
          <Banner tone="info" icon={LoaderCircleIcon} title="Thank you! Confirming your payment…" className="[&_svg]:animate-spin">
            This usually takes a few seconds.
          </Banner>
        )}
        {confirming === "ok" && (
          <Banner tone="ok" icon={CheckIcon} title="Payment received, thank you!">
            A receipt is on its way to your inbox.
          </Banner>
        )}
        {confirming === "late" && (
          <Banner tone="warn" icon={ClockIcon} title="We haven't heard back from the payment provider yet">
            If you completed the payment, it will show up here shortly and you'll get a receipt by e-mail. There's no need to pay again.
          </Banner>
        )}
      </div>
      {!inv ? (
        <div aria-busy="true" className="flex flex-col gap-4">
          <Skeleton className="h-16 rounded-2xl" />
          <Skeleton className="h-96 rounded-2xl" />
        </div>
      ) : (
        <>
          <div className="mb-4 flex flex-wrap items-end justify-between gap-x-4 gap-y-3">
            <div className="min-w-0">
              <h2 className="flex flex-wrap items-center gap-2.5 text-[1.375rem] leading-tight font-bold tracking-[-0.02em]">
                {invoiceTitle(inv)}
                <InvoicePill invoice={inv} />
              </h2>
              <p className="mt-0.5 text-sm text-muted-foreground">
                {(inv.kind && INVOICE_KINDS[inv.kind]) || "Invoice"}
                {dueText(inv) ? ` · ${dueLine(inv)}` : ""}
              </p>
            </div>
            <div className="flex flex-wrap gap-2">
              {inv.status === "unpaid" && (
                <ActionButton variant="default" run={() => ctx.pay(inv)}>
                  <CreditCardIcon data-icon="inline-start" />
                  Pay {money(inv.balance)}
                </ActionButton>
              )}
              {inv.status === "unpaid" && credit > 0 && (
                <ActionButton
                  run={async () => {
                    if (
                      !(await ask(`Pay ${invoiceTitle(inv)} from your credit? Up to ${money(Math.min(credit, inv.balance))} is used.`, {
                        ok: "Use credit",
                      }))
                    )
                      return
                    await api("POST", `/invoices/${inv.id}/apply-credit`, { amount: null })
                    notify("Credit applied")
                    await reloadBilling()
                  }}
                >
                  <TagIcon data-icon="inline-start" />
                  Use my credit ({money(credit)})
                </ActionButton>
              )}
              <Button variant="ghost" nativeButton={false} render={<a href={`/api/v1/invoices/${inv.id}/print`} target="_blank" rel="noopener" />}>
                <PrinterIcon data-icon="inline-start" />
                Print / PDF
              </Button>
            </div>
          </div>
          <div className="grid grid-cols-1 items-start gap-4 xl:grid-cols-[minmax(0,1fr)_340px]">
            <InvoiceDocument invoice={inv} />
            <aside>
              <Section title="Payments">
                <PaymentsTable invoice={inv} />
              </Section>
            </aside>
          </div>
        </>
      )}
    </>
  )
}
