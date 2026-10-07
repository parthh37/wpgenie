import { useState } from "react"
import { Building2Icon, ChevronRightIcon, PlusIcon, SearchIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Skeleton } from "@/components/ui/skeleton"
import { BTable, Chips, EmptyState, LoadError } from "@/components/app/blocks"
import { Page, PageHeader, Section } from "@/components/app/page"
import { StatusText } from "@/components/app/status"
import type { Plan } from "@/features/plans/types"
import { fmtNum } from "@/lib/format"
import { useApi } from "@/lib/query"
import { href, navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { CreateAccountDialog } from "./create-dialog"
import { accountPath, type Account } from "./types"

type Filter = "all" | "customer" | "reseller" | "suspended" | "terminated"

// accountState is an account's status in words: a customer whose reseller
// is suspended is suspended too.
export function accountState(a: Account) {
  if (a.effectively_suspended && a.status === "active") return "suspended (reseller)"
  return a.status + (a.suspend_reason ? ` (${a.suspend_reason})` : "")
}

// The Accounts page: customers and resellers, their plans and what they
// use. Staff see every account; a reseller their own and their customers'.
export function AccountsList() {
  const s = useSession()
  const accounts = useApi<Account[]>("/accounts")
  const plans = useApi<Plan[]>("/plans")
  const [creating, setCreating] = useState(false)
  const [filter, setFilter] = useState<Filter>("all")
  const [search, setSearch] = useState("")
  // Administrators and resellers create accounts.
  const canCreate = s.isAdmin || s.isReseller

  const list = accounts.data ?? []
  const count = (f: Filter) => list.filter((a) => matches(a, f)).length
  const q = search.trim().toLowerCase()
  const shown = list.filter(
    (a) =>
      matches(a, filter) &&
      (!q || a.name.toLowerCase().includes(q) || String(a.id) === q || (a.email || "").toLowerCase().includes(q) || a.plan_id.includes(q))
  )

  const newButton = canCreate && (
    <Button onClick={() => setCreating(true)} disabled={!accounts.data || !plans.data}>
      <PlusIcon data-icon="inline-start" />
      New account
    </Button>
  )

  return (
    <Page>
      <PageHeader
        icon={Building2Icon}
        tint="cyan"
        title="Accounts"
        description="Customers and resellers, their plans and what they use."
        actions={newButton}
      />

      {accounts.isLoading ? (
        <Section>
          <div className="flex flex-col gap-3">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-10 w-full" />
            ))}
          </div>
        </Section>
      ) : accounts.error ? (
        <LoadError error={accounts.error} retry={() => accounts.refetch()} />
      ) : !list.length ? (
        <EmptyState icon={Building2Icon} tint="cyan" title="No accounts yet" actions={newButton}>
          An account holds sites for a customer or a reseller, with a plan that sets what they can use.
        </EmptyState>
      ) : (
        <>
          <div className="mb-4 flex flex-wrap items-center gap-3">
            <Chips<Filter>
              label="Show"
              current={filter}
              onPick={setFilter}
              items={[
                ["all", "All", list.length],
                ["customer", "Customers", count("customer")],
                ...(s.isTenant ? [] : ([["reseller", "Resellers", count("reseller")]] as Array<[Filter, string, number]>)),
                ["suspended", "Suspended", count("suspended")],
                ["terminated", "Terminated", count("terminated")],
              ]}
            />
            <InputGroup className="ml-auto w-full sm:w-64">
              <InputGroupAddon>
                <SearchIcon />
              </InputGroupAddon>
              <InputGroupInput type="search" aria-label="Find an account" placeholder="Find an account" value={search} onChange={(e) => setSearch(e.target.value)} />
            </InputGroup>
          </div>
          <Section contentClassName="overflow-x-auto">
            <BTable
              caption="Accounts"
              empty={<p className="py-2 text-sm text-muted-foreground">No accounts match.</p>}
              cols={["ID", "Name", "Kind", "Status", "Plan", { label: "Sites", num: true }, "Reseller", { label: <span className="sr-only">Open</span> }]}
              rows={shown.map((a) => ({
                key: a.id,
                onOpen: () => navigate(accountPath(a.id)),
                cells: [
                  <span key="id" className="text-muted-foreground tabular-nums">
                    {a.id}
                  </span>,
                  <a key="name" href={href(accountPath(a.id))} className="font-medium text-foreground">
                    {a.name}
                  </a>,
                  a.kind,
                  <StatusText key="status" status={a.effectively_suspended ? "suspended" : a.status}>
                    {accountState(a)}
                  </StatusText>,
                  a.plan_id,
                  fmtNum(a.sites),
                  a.parent_name || "",
                  <ChevronRightIcon key="open" aria-hidden className="ml-auto size-4 text-muted-foreground" />,
                ],
              }))}
            />
          </Section>
        </>
      )}

      {creating && accounts.data && plans.data && (
        <CreateAccountDialog open onOpenChange={setCreating} accounts={accounts.data} plans={plans.data} />
      )}
    </Page>
  )
}

function matches(a: Account, f: Filter) {
  switch (f) {
    case "customer":
    case "reseller":
      return a.kind === f
    case "suspended":
      return a.effectively_suspended && a.status !== "terminated"
    case "terminated":
      return a.status === "terminated"
    default:
      return true
  }
}
