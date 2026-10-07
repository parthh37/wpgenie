import { useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { DownloadIcon, WalletIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, EmptyState, LoadError } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { api } from "@/lib/api"
import { fmtDueDate, listOf, methodName, money } from "@/lib/money"
import { navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { billingPath } from "../route"
import { InvoiceLink, SubLine } from "../shared/invoice-document"
import type { Payment } from "../shared/types"
import { TX_FILTER, csvHref, type TxFilter } from "./filters"

const PAGE = 50

const METHODS: Array<[string, string]> = [
  ["", "Every method"],
  ["stripe", "Card (Stripe)"],
  ["razorpay", "Razorpay"],
  ["bank", "Bank transfer"],
  ["cash", "Cash"],
  ["cheque", "Cheque"],
  ["other", "Other"],
  ["credit", "Account credit"],
]

// Every payment, newest first: cards and Razorpay by themselves, the rest
// as staff record them.
export function StaffTransactions() {
  const admin = useSession().atLeast("admin")
  const [f, setF] = useState<TxFilter>(() => ({ ...TX_FILTER }))
  const change = (patch: Partial<TxFilter>) => {
    Object.assign(TX_FILTER, patch)
    setF((x) => ({ ...x, ...patch }))
  }
  const query = (before: string | number) =>
    new URLSearchParams(Object.entries({ ...f, limit: PAGE, before }).filter(([, v]) => v !== "").map(([k, v]) => [k, String(v)])).toString()

  const list = useInfiniteQuery({
    queryKey: [`/transactions?${query("")}`, "pages"],
    queryFn: async ({ pageParam }) => listOf<Payment>(await api("GET", "/transactions?" + query(pageParam)), "transactions"),
    initialPageParam: "" as string | number,
    getNextPageParam: (last) => (last.length === PAGE ? last[last.length - 1].id : undefined),
  })
  const items = list.data?.pages.flat() ?? []

  return (
    <Section contentClassName="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <NativeSelect aria-label="Method" value={f.method} onChange={(e) => change({ method: e.target.value })} className="w-48">
          {METHODS.map(([v, label]) => (
            <NativeSelectOption key={v} value={v}>
              {label}
            </NativeSelectOption>
          ))}
        </NativeSelect>
        <div className="flex items-center gap-2">
          <Input type="date" aria-label="From" value={f.from} onChange={(e) => change({ from: e.target.value })} className="w-40" />
          <span className="text-sm text-muted-foreground">to</span>
          <Input type="date" aria-label="Until" value={f.to} onChange={(e) => change({ to: e.target.value })} className="w-40" />
        </div>
        {admin && (
          <Button variant="tinted" className="sm:ml-auto" nativeButton={false} render={<a href={csvHref("/transactions.csv", f)} download />}>
            <DownloadIcon data-icon="inline-start" />
            Export CSV
          </Button>
        )}
      </div>

      <div aria-live="polite" aria-busy={list.isFetching}>
        {list.isError ? (
          <LoadError error={list.error} retry={() => list.refetch()} className="shadow-none" />
        ) : !list.data ? (
          <div className="flex flex-col gap-2">
            {Array.from({ length: 5 }, (_, i) => (
              <Skeleton key={i} className="h-10 rounded-xl" />
            ))}
          </div>
        ) : (
          <>
            <BTable
              caption="Transactions"
              cols={["Date", "Client", "Invoice", "Method", "Reference", { label: "Amount", num: true }, { label: "Fee", num: true }]}
              rows={items.map((t) => ({
                key: t.id,
                onOpen: t.invoice_id ? () => navigate(billingPath("invoices", t.invoice_id)) : undefined,
                cells: [
                  fmtDueDate(t.at),
                  t.account_name || (t.account_id ? `#${t.account_id}` : "–"),
                  t.invoice_id ? <InvoiceLink invoice={{ id: t.invoice_id, number: t.invoice_number }} /> : "–",
                  methodName(t.gateway || t.method),
                  <span className="whitespace-normal [overflow-wrap:anywhere]">{t.reference || "–"}</span>,
                  <>
                    {money(t.amount)}
                    {t.refunded ? <SubLine>{money(t.refunded)} refunded</SubLine> : null}
                  </>,
                  t.fee ? money(t.fee) : "–",
                ],
              }))}
              empty={
                <EmptyState icon={WalletIcon} tint="green" title={f.method || f.from || f.to ? "No payments match" : "No payments yet"} className="shadow-none">
                  Every payment lands here: cards and Razorpay by themselves, bank transfers and cash when you record them on the invoice.
                </EmptyState>
              }
            />
            {list.hasNextPage && (
              <div className="mt-3 flex justify-center">
                <ActionButton run={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
                  Load more
                </ActionButton>
              </div>
            )}
          </>
        )}
      </div>
    </Section>
  )
}
