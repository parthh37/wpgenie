import { useEffect, useState } from "react"
import { useInfiniteQuery, useQuery } from "@tanstack/react-query"
import { CheckIcon, InboxIcon, LifeBuoyIcon, PlusIcon, SearchIcon, SlidersHorizontalIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { Banner, EmptyState, Kpi, LoadError } from "@/components/app/blocks"
import { Page, PageHeader } from "@/components/app/page"
import { api } from "@/lib/api"
import { fmtNum } from "@/lib/format"
import { href } from "@/lib/router"
import { useSession } from "@/lib/session"
import { cn } from "@/lib/utils"
import {
  ACTIVE_STATUSES, PRIORITIES, Priority, TicketStatus, TimeAgo, duration, useSummary,
  type Agent, type Department, type Overview, type Summary, type Ticket,
} from "./shared"

// #/support: the staff's queue, with its numbers and filters; a reseller's
// own tickets and their customers'; a customer's tickets.

type ChipKey = "awaiting" | "active" | "on_hold" | "closed" | "all"
const CHIPS: Array<[key: ChipKey, label: string, query: Record<string, string>, providerOnly?: boolean]> = [
  ["awaiting", "Needs your reply", { awaiting: "1" }],
  ["active", "Open", { status: ACTIVE_STATUSES.join(",") }],
  ["on_hold", "On hold", { status: "on_hold" }, true],
  ["closed", "Closed", { status: "closed" }],
  ["all", "All", {}],
]
const PAGE = 50

// The list's filters outlive the page: back from a ticket, they're as left.
interface Filters {
  filter: ChipKey | null
  q: string
  dept: string
  prio: string
  assigned: string
}
let kept: Filters = { filter: null, q: "", dept: "", prio: "", assigned: "" }

export function TicketList() {
  const s = useSession()
  const staff = s.isStaff
  const provider = staff || s.isReseller
  const summary = useSummary()
  const sum: Summary = summary.data ?? { enabled: true, awaiting: 0, active: 0, total: 0 }
  const summaryKnown = !summary.isPending

  const [f, setF] = useState<Filters>(kept)
  const update = (patch: Partial<Filters>) => setF((cur) => ({ ...cur, ...patch }))
  // Waiting tickets first, if there are any; then the choice stays.
  const filter: ChipKey | null = f.filter ?? (summaryKnown ? (sum.awaiting ? "awaiting" : "active") : null)
  if (f.filter == null && filter != null) setF({ ...f, filter })
  useEffect(() => {
    kept = f
  }, [f])

  // The search waits for a pause in the typing.
  const [search, setSearch] = useState(f.q)
  useEffect(() => {
    const t = setTimeout(() => setF((cur) => (cur.q === search.trim() ? cur : { ...cur, q: search.trim() })), 300)
    return () => clearTimeout(t)
  }, [search])

  const depts = useQuery({ queryKey: ["/support/departments"], enabled: staff, queryFn: () => api<Department[]>("GET", "/support/departments") })
  const overview = useQuery({
    queryKey: ["/support/overview"],
    enabled: staff,
    queryFn: () => api<Overview>("GET", "/support/overview").catch(() => null),
  })
  const agents = useQuery({
    queryKey: ["/support/agents"],
    enabled: staff,
    queryFn: () => api<Agent[]>("GET", "/support/agents").catch(() => [] as Agent[]),
  })

  const params = new URLSearchParams({ ...(CHIPS.find(([k]) => k === filter)?.[2] ?? {}), limit: String(PAGE) })
  if (f.q) params.set("q", f.q)
  if (f.dept) params.set("department", f.dept)
  if (f.prio) params.set("priority", f.prio)
  if (f.assigned) params.set("assigned", f.assigned)
  const path = "/tickets?" + params
  const tickets = useInfiniteQuery({
    queryKey: [path],
    enabled: filter != null,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api<Ticket[]>("GET", pageParam ? `${path}&before=${pageParam}` : path),
    getNextPageParam: (last) => (last.length === PAGE ? last[last.length - 1].id : undefined),
    placeholderData: (prev) => prev,
  })
  const items = tickets.data?.pages.flat() ?? []

  const ov = overview.data
  const showAll = () => {
    setSearch("")
    update({ filter: "all", q: "", dept: "", prio: "", assigned: "" })
  }
  const filtered = !!(f.q || f.dept || f.prio || f.assigned || (filter && !["all", "active"].includes(filter)))
  const canOpen = s.canChange && (staff || sum.enabled)

  return (
    <Page>
      <PageHeader
        icon={LifeBuoyIcon}
        tint="pink"
        title="Support"
        description={
          staff
            ? "Your customers' questions and problems, in one place."
            : provider
              ? "Your own tickets, and your customers' tickets to answer."
              : "Ask us anything about your sites, e-mail or account. We'll e-mail you when we reply."
        }
        actions={
          <>
            {staff && s.atLeast("operator") && (
              <Button variant="tinted" nativeButton={false} render={<a href={href("/support/manage")} />}>
                <SlidersHorizontalIcon data-icon="inline-start" />
                Manage
              </Button>
            )}
            {canOpen && (
              <Button nativeButton={false} render={<a href={href("/support/new")} />}>
                <PlusIcon data-icon="inline-start" />
                {staff ? "Open a ticket for a customer" : "New ticket"}
              </Button>
            )}
          </>
        }
      />

      {staff && ov && (
        <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
          <Kpi label="Needs a reply" value={fmtNum(ov.awaiting)} sub={ov.awaiting ? "waiting for your team" : "all caught up"} tone={ov.awaiting ? "warn" : "ok"} />
          <Kpi
            label="Open"
            value={fmtNum(ACTIVE_STATUSES.reduce((n, k) => n + (ov.by_status[k] || 0), 0))}
            sub={`${ov.by_status.on_hold || 0} on hold`}
          />
          <Kpi label="Unassigned" value={fmtNum(ov.unassigned)} sub="open tickets nobody owns" />
          <Kpi label="First reply" value={duration(ov.avg_first_response_seconds)} sub={`on average · ${ov.opened_30d} opened in 30 days`} />
        </div>
      )}

      {!staff && summary.data && !sum.enabled && (
        <Banner tone="warn" title="New tickets are turned off at the moment.">
          You can still read and reply to the ones you have.
        </Banner>
      )}

      <div className="overflow-hidden rounded-2xl bg-card card-shadow">
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-3 border-b border-border/60 px-4 py-3.5 sm:px-5">
          <Segmented
            label="Show tickets"
            items={CHIPS.filter(([, , , providerOnly]) => !providerOnly || provider).map(([k, l]) => [k, l, k === "awaiting" ? sum.awaiting : 0])}
            current={filter}
            onPick={(k) => update({ filter: k })}
          />
          <div className="flex flex-wrap items-center gap-2 max-sm:w-full">
            <InputGroup className="w-64 max-sm:w-full">
              <InputGroupAddon>
                <SearchIcon />
              </InputGroupAddon>
              <InputGroupInput
                type="search"
                placeholder="Search tickets"
                aria-label="Search tickets"
                autoComplete="off"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
              />
            </InputGroup>
            {staff && (
              <>
                <NativeSelect aria-label="Department" value={f.dept} onChange={(e) => update({ dept: e.target.value })} className="max-w-48 max-sm:flex-1">
                  <NativeSelectOption value="">All departments</NativeSelectOption>
                  {(depts.data ?? []).map((d) => (
                    <NativeSelectOption key={d.id} value={String(d.id)}>
                      {d.name}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
                <NativeSelect aria-label="Priority" value={f.prio} onChange={(e) => update({ prio: e.target.value })} className="max-w-48 max-sm:flex-1">
                  <NativeSelectOption value="">Any priority</NativeSelectOption>
                  {PRIORITIES.map(([k, l]) => (
                    <NativeSelectOption key={k} value={k}>
                      {l}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
                <NativeSelect aria-label="Assigned to" value={f.assigned} onChange={(e) => update({ assigned: e.target.value })} className="max-w-48 max-sm:flex-1">
                  <NativeSelectOption value="">Anyone</NativeSelectOption>
                  <NativeSelectOption value="me">Me</NativeSelectOption>
                  <NativeSelectOption value="none">Nobody</NativeSelectOption>
                  {(agents.data ?? []).map((a) => (
                    <NativeSelectOption key={a.id} value={String(a.id)}>
                      {a.username}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              </>
            )}
          </div>
        </div>

        <div aria-live="polite" aria-busy={tickets.isFetching} className={cn("transition-opacity", tickets.isFetching && !tickets.isFetchingNextPage && items.length > 0 && "opacity-65")}>
          {tickets.isError && !items.length ? (
            <LoadError error={tickets.error} retry={() => tickets.refetch()} className="rounded-none shadow-none" />
          ) : !tickets.data ? (
            <div className="flex flex-col gap-px p-4" role="status" aria-label="Loading tickets">
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-16 rounded-xl" />
              ))}
            </div>
          ) : items.length ? (
            <>
              <ul className="divide-y divide-border/60">
                {items.map((t) => (
                  <li key={t.id}>
                    <TicketRow t={t} provider={provider} />
                  </li>
                ))}
              </ul>
              {tickets.hasNextPage && (
                <div className="flex justify-center py-3">
                  <Button variant="ghost" disabled={tickets.isFetchingNextPage} onClick={() => tickets.fetchNextPage()}>
                    {tickets.isFetchingNextPage && <Spinner data-icon="inline-start" />}
                    Show older tickets
                  </Button>
                </div>
              )}
            </>
          ) : filtered ? (
            <EmptyState
              icon={filter === "awaiting" ? CheckIcon : SearchIcon}
              tint={filter === "awaiting" && !f.q ? "green" : "gray"}
              title={filter === "awaiting" && !f.q ? "All caught up" : "No tickets match"}
              className="rounded-none shadow-none"
              actions={
                <Button variant="tinted" onClick={showAll}>
                  Show all tickets
                </Button>
              }
            >
              {filter === "awaiting" && !f.q ? "Nothing is waiting for your reply right now." : "Try another filter or search."}
            </EmptyState>
          ) : provider ? (
            <EmptyState
              icon={InboxIcon}
              tint="pink"
              title="No tickets yet"
              className="rounded-none shadow-none"
              actions={
                !staff && (
                  <Button nativeButton={false} render={<a href={href("/support/new")} />}>
                    <PlusIcon data-icon="inline-start" />
                    Open a ticket of your own
                  </Button>
                )
              }
            >
              {staff
                ? "When customers ask for help, their tickets show up here, and whoever you set up in Manage gets an e-mail."
                : "When your customers ask for help, their tickets show up here for you to answer."}
            </EmptyState>
          ) : (
            <EmptyState
              icon={LifeBuoyIcon}
              tint="pink"
              title="Need a hand?"
              className="rounded-none shadow-none"
              actions={
                sum.enabled && (
                  <Button nativeButton={false} render={<a href={href("/support/new")} />}>
                    <PlusIcon data-icon="inline-start" />
                    Open your first ticket
                  </Button>
                )
              }
            >
              {sum.enabled
                ? "Open a ticket and tell us what's going on. You'll get an e-mail when we reply, and the whole conversation stays here."
                : "You have no tickets."}
            </EmptyState>
          )}
        </div>
      </div>
    </Page>
  )
}

// Segmented: the status filter, as a segmented control.
function Segmented<K extends string>({
  label,
  items,
  current,
  onPick,
}: {
  label: string
  items: Array<[key: K, text: string, count: number]>
  current: K | null
  onPick: (k: K) => void
}) {
  return (
    <div role="group" aria-label={label} className="inline-flex max-w-full flex-wrap gap-0.5 rounded-[10px] bg-muted p-0.5">
      {items.map(([key, text, count]) => (
        <button
          key={key}
          type="button"
          aria-pressed={key === current}
          onClick={() => onPick(key)}
          className={cn(
            "inline-flex h-7 items-center gap-1.5 rounded-[8px] px-3.5 text-[0.8125rem] font-medium transition-[background-color,box-shadow] hover:bg-accent",
            key === current && "bg-card font-semibold shadow-[0_1px_3px_rgba(0,0,0,.14),0_0_0_.5px_rgba(0,0,0,.06)] hover:bg-card dark:bg-[color-mix(in_oklch,var(--foreground)_30%,var(--card))] dark:hover:bg-[color-mix(in_oklch,var(--foreground)_30%,var(--card))]"
          )}
        >
          {text}
          {count > 0 && (
            <span className="inline-grid h-5 min-w-5 place-items-center rounded-full bg-primary px-1.5 text-[0.72rem] leading-none font-bold text-primary-foreground tabular-nums">
              {fmtNum(count)}
            </span>
          )}
        </button>
      ))}
    </div>
  )
}

function TicketRow({ t, provider }: { t: Ticket; provider: boolean }) {
  const meta = [t.mask, provider && t.you !== "customer" ? t.account_name : null, t.department, t.assigned_to ? `→ ${t.assigned_to}` : null]
    .filter(Boolean)
    .join(" · ")
  return (
    <a
      href={href(`/support/${t.id}`)}
      className={cn(
        "grid grid-cols-1 items-center gap-1.5 px-4 py-3.5 text-foreground no-underline transition-colors hover:bg-accent/60 hover:no-underline focus-visible:-outline-offset-2 sm:grid-cols-[minmax(0,1fr)_auto] sm:gap-4 sm:px-5",
        t.awaiting && "shadow-[inset_3px_0_0_var(--primary)]"
      )}
    >
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
          <span className={cn("font-semibold [overflow-wrap:anywhere]", t.status === "closed" && "font-medium text-muted-foreground")}>{t.subject}</span>
          {t.awaiting && (
            <span className="rounded-full bg-primary/12 px-2 py-px text-[0.72rem] font-bold whitespace-nowrap text-link">Needs your reply</span>
          )}
          {t.you === "handler" && t.escalated && <Badge variant="secondary">Escalated</Badge>}
          {t.you === "staff" && t.handler === "reseller" && (
            <Tooltip>
              <TooltipTrigger render={<Badge variant="secondary" />}>Reseller</TooltipTrigger>
              <TooltipContent>The customer's reseller answers it unless they escalate it</TooltipContent>
            </Tooltip>
          )}
        </div>
        {t.preview && <p className="mt-0.5 truncate text-sm text-muted-foreground">{t.preview}</p>}
        <p className="mt-1 font-mono text-xs [overflow-wrap:anywhere] text-muted-foreground">{meta}</p>
      </div>
      <div className="flex items-center justify-between gap-2 sm:flex-col sm:items-end sm:justify-center sm:text-right">
        <div className="flex flex-wrap justify-end gap-1">
          <TicketStatus ticket={t} />
          {t.priority !== "medium" && <Priority priority={t.priority} />}
        </div>
        <TimeAgo at={t.updated_at} className="text-xs text-muted-foreground" />
      </div>
    </a>
  )
}
