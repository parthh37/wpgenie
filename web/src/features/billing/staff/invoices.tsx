import { useRef, useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { DownloadIcon, PlusIcon, ReceiptIcon, SearchIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, Chips, EmptyState, LoadError } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { api } from "@/lib/api"
import { dueText, fmtDueDate, listOf, money } from "@/lib/money"
import { useApi } from "@/lib/query"
import { navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { billingPath } from "../route"
import { InvoiceLink, InvoicePill, SubLine, stateOf } from "../shared/invoice-document"
import type { Invoice, Overview } from "../shared/types"
import { INVOICE_CHIPS, INVOICE_FILTER, csvHref, invoiceQuery, type InvoiceFilter } from "./filters"
import { useInvoiceEditor } from "./invoice-editor"

const PAGE = 50

// The staff invoice list: filter by state, search, dates; 50 at a time.
export function StaffInvoices() {
  const admin = useSession().atLeast("admin")
  const editor = useInvoiceEditor()
  const [f, setF] = useState<InvoiceFilter>(() => ({ ...INVOICE_FILTER }))
  const [search, setSearch] = useState(f.q)
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  const change = (patch: Partial<InvoiceFilter>) => {
    Object.assign(INVOICE_FILTER, patch)
    setF((x) => ({ ...x, ...patch }))
  }

  const base = invoiceQuery(f)
  const list = useInfiniteQuery({
    queryKey: [`/invoices?${base}`, "pages"],
    queryFn: async ({ pageParam }) => {
      // A bare list, or {invoices, counts} (counts per state, for the chips).
      const res = await api<unknown>("GET", "/invoices?" + invoiceQuery(f, { limit: PAGE, before: pageParam }))
      const counts = (res && !Array.isArray(res) && (res as { counts?: Record<string, number> }).counts) || null
      return { items: listOf<Invoice>(res, "invoices"), counts }
    },
    initialPageParam: "" as string | number,
    getNextPageParam: (last) => (last.items.length === PAGE ? last.items[last.items.length - 1].id : undefined),
  })
  // Counts for the chips the list doesn't give: from the overview.
  const overview = useApi<Overview>("/billing/overview")
  const counts: Record<string, number | null | undefined> = { ...list.data?.pages[0]?.counts }
  if (counts.unpaid == null) counts.unpaid = overview.data?.unpaid_count
  if (counts.overdue == null) counts.overdue = overview.data?.overdue_count

  const items = list.data?.pages.flatMap((p) => p.items) ?? []
  const filtered = !!(f.status || f.q || f.from || f.to)

  return (
    <Section contentClassName="flex flex-col gap-4">
      {editor.dialog}
      <div className="flex flex-col gap-3">
        <Chips label="Show invoices" items={INVOICE_CHIPS.map(([k, label]) => [k, label, counts[k]])} current={f.status} onPick={(k) => change({ status: k })} />
        <div className="flex flex-wrap items-center gap-2">
          <InputGroup className="w-64 max-sm:w-full">
            <InputGroupAddon>
              <SearchIcon />
            </InputGroupAddon>
            <InputGroupInput
              type="search"
              placeholder="Number or client"
              aria-label="Search invoices"
              autoComplete="off"
              value={search}
              onChange={(e) => {
                const v = e.target.value
                setSearch(v)
                clearTimeout(timer.current)
                timer.current = setTimeout(() => change({ q: v.trim() }), 300)
              }}
            />
          </InputGroup>
          <div className="flex items-center gap-2">
            <Input type="date" aria-label="Issued from" value={f.from} onChange={(e) => change({ from: e.target.value })} className="w-40" />
            <span className="text-sm text-muted-foreground">to</span>
            <Input type="date" aria-label="Issued until" value={f.to} onChange={(e) => change({ to: e.target.value })} className="w-40" />
          </div>
          {admin && (
            <Button variant="tinted" className="sm:ml-auto" nativeButton={false} render={<a href={csvHref("/invoices.csv", f)} download />}>
              <DownloadIcon data-icon="inline-start" />
              Export CSV
            </Button>
          )}
        </div>
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
              caption="Invoices"
              cols={["Invoice", "Client", "Issued", "Due", { label: "Total", num: true }, { label: "Balance", num: true }, "Status"]}
              rows={items.map((inv) => ({
                key: inv.id,
                className: stateOf(inv) === "overdue" ? "bg-danger-fill/6 shadow-[inset_3px_0_0_var(--danger)] hover:bg-danger-fill/10" : undefined,
                onOpen: () => navigate(billingPath("invoices", inv.id)),
                cells: [
                  <InvoiceLink invoice={inv} />,
                  inv.account_name || `#${inv.account_id}`,
                  fmtDueDate(inv.issued_at),
                  <>
                    {fmtDueDate(inv.due_at)}
                    {dueText({ status: inv.status || "", due_at: inv.due_at || undefined }) && <SubLine>{dueText({ status: inv.status || "", due_at: inv.due_at || undefined })}</SubLine>}
                  </>,
                  money(inv.total),
                  money(inv.balance),
                  <InvoicePill invoice={inv} />,
                ],
              }))}
              empty={
                filtered ? (
                  <EmptyState icon={SearchIcon} title="No invoices match" className="shadow-none">
                    Try another filter, or clear the search and dates.
                  </EmptyState>
                ) : (
                  <EmptyState
                    icon={ReceiptIcon}
                    tint="mint"
                    title="No invoices yet"
                    className="shadow-none"
                    actions={
                      admin && (
                        <ActionButton variant="default" run={() => editor.open({})}>
                          <PlusIcon data-icon="inline-start" />
                          New invoice
                        </ActionButton>
                      )
                    }
                  >
                    Invoices are created automatically a few days before each renewal of the clients you bill, and when someone orders from your order page. You can
                    also write one by hand.
                  </EmptyState>
                )
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
