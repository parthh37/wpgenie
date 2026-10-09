import { useMemo, useState } from "react"
import { useQueries } from "@tanstack/react-query"
import {
  ActivityIcon, ChartColumnIcon, ChevronRightIcon, GlobeIcon, HeartPulseIcon, PlusIcon, SearchIcon, ShieldIcon, TriangleAlertIcon, UsersIcon,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Skeleton } from "@/components/ui/skeleton"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { LineChart, hourly, hoursSince } from "@/components/app/chart"
import { IconTile } from "@/components/app/icon-tile"
import { Page, PageHeader } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { VitalBars, VitalMeter, type VitalValues } from "@/components/app/vitals"
import { api } from "@/lib/api"
import { fmtBytes, fmtNum, plural, sum } from "@/lib/format"
import { useClustered, useNodes, useSites } from "@/lib/query"
import { href, navigate, sitePath } from "@/lib/router"
import { ACCESS_LABELS, useSession } from "@/lib/session"
import type { Site, SiteStats } from "@/lib/types"
import { cn } from "@/lib/utils"
import { SHIELD_LABELS, attentionFor, siteHue, useAttack, useSiteCPU, useSiteStats, vitals, type Attack, type CPU } from "./data"
import { openNewSite } from "./new-site"

// The Sites page: the fleet as widgets, what needs attention, then a row
// per site (traffic, numbers, vitals) that opens its workspace.
export function SitesList() {
  const s = useSession()
  const { data: sites, isLoading } = useSites()
  const [filter, setFilter] = useState("")
  const list = sites ?? []

  // Every active site's numbers, shared (cached) with its row.
  const active = list.filter((x) => x.status === "active")
  const stats = useQueries({
    queries: active.map((x) => ({
      queryKey: [`/sites/${x.id}/stats?hours=24`],
      queryFn: () => api<SiteStats>("GET", `/sites/${x.id}/stats?hours=24`),
      staleTime: 60_000,
    })),
  })
  const cpus = useQueries({
    queries: active.map((x) => ({
      queryKey: [`/sites/${x.id}/metrics`],
      queryFn: async () => (await api<{ cpu: CPU | null }>("GET", `/sites/${x.id}/metrics`)).cpu,
      refetchInterval: 15_000,
      retry: false,
    })),
  })
  const attacks = useQueries({
    queries: active.map((x) => ({
      queryKey: [`/sites/${x.id}/attack`],
      queryFn: () => api<Attack>("GET", `/sites/${x.id}/attack`),
      refetchInterval: 60_000,
      retry: false,
    })),
  })
  const byId = <T,>(rs: { data?: T }[]) => new Map(active.map((x, i) => [x.id, rs[i]?.data]))
  const statsById = byId(stats)
  const cpuById = byId(cpus)
  const attackById = byId(attacks)

  const attention = list
    .map((site) => ({ site, a: attentionFor(site, !!attackById.get(site.id)?.active) }))
    .filter((x) => x.a)
    .sort((x, y) => (x.a!.level === y.a!.level ? 0 : x.a!.level === "bad" ? -1 : 1))

  const q = filter.trim().toLowerCase()
  const shown = q ? list.filter((x) => x.primary_domain.toLowerCase().includes(q)) : list
  const staging = list.filter((x) => x.parent_id).length

  return (
    <Page>
      <PageHeader
        icon={GlobeIcon}
        tint="indigo"
        title="Sites"
        description={sites ? `${list.length} site${list.length === 1 ? "" : "s"}${staging ? `, ${staging} staging` : ""}` : " "}
        actions={
          <>
            <InputGroup className="w-56 max-sm:w-full">
              <InputGroupAddon>
                <SearchIcon />
              </InputGroupAddon>
              <InputGroupInput type="search" placeholder="Filter sites" aria-label="Filter sites" autoComplete="off" value={filter} onChange={(e) => setFilter(e.target.value)} />
            </InputGroup>
            {s.canCreate && (
              <Button onClick={openNewSite}>
                <PlusIcon data-icon="inline-start" />
                New site
              </Button>
            )}
          </>
        }
      />

      {isLoading && <ListSkeleton />}

      {list.length > 0 && (
        <Fleet
          sites={list}
          stats={[...statsById.values()].filter(Boolean) as SiteStats[]}
          vitals={list.map((x) => vitals(x, statsById.get(x.id), cpuById.get(x.id)))}
        />
      )}

      {attention.length > 0 && (
        <section
          aria-labelledby="attention-title"
          className={cn(
            "mb-6 rounded-lg bg-card p-4 ring-1 ring-border sm:p-5",
            attention.some((x) => x.a!.level === "bad") ? "border-l-2 border-l-danger-fill" : "border-l-2 border-l-warning-fill"
          )}
        >
          <h2 id="attention-title" className="mb-3 flex items-center gap-2 text-[1.0625rem] font-semibold">
            <TriangleAlertIcon className={attention.some((x) => x.a!.level === "bad") ? "size-5 text-danger" : "size-5 text-warning"} />
            Needs attention
          </h2>
          {/* One grid for every row (subgrid), so the domains and the reasons
              line up in columns. */}
          <div className="grid grid-cols-[auto_minmax(0,1fr)_auto] gap-y-1.5 sm:grid-cols-[auto_minmax(0,max-content)_minmax(0,1fr)_auto]">
            {attention.map(({ site, a }) => (
              <div key={site.id} className="col-span-full grid grid-cols-subgrid items-center gap-x-3 rounded-xl bg-card py-2.5 pr-2.5 pl-3.5 card-shadow">
                <span aria-hidden className={cn("size-2.5 shrink-0 rounded-full", a!.level === "bad" ? "bg-danger-fill" : "bg-warning-fill")} />
                <div className="flex min-w-0 flex-col text-sm sm:contents">
                  <strong className="truncate font-semibold sm:max-w-64">{site.primary_domain}</strong>
                  <span className="text-muted-foreground">{a!.text}</span>
                </div>
                <Button variant="tinted" size="sm" aria-label={`Review ${site.primary_domain}`} onClick={() => navigate(sitePath(site.id, a!.section))}>
                  Review
                  <ChevronRightIcon data-icon="inline-end" />
                </Button>
              </div>
            ))}
          </div>
        </section>
      )}

      {attention.length > 0 && list.length > 0 && <h2 className="mb-3 text-[1.0625rem] font-semibold">All sites</h2>}

      {list.length > 0 && (
        <div className="@container">
          {/* Columns are set here and shared by every row (subgrid), so the
              numbers, labels and buttons line up down the list. They follow
              the list's own width, not the window's (the sidebar takes some). */}
          <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-y-2 @md:grid-cols-[minmax(0,1fr)_auto_auto] @3xl:grid-cols-[minmax(13rem,1.5fr)_minmax(6rem,1fr)_auto_auto] @5xl:grid-cols-[minmax(13rem,1.5fr)_minmax(6rem,1fr)_auto_auto_auto]">
            {shown.map((site) => (
              <SiteRow key={site.id} site={site} sites={list} underAttack={!!attackById.get(site.id)?.active} />
            ))}
          </div>
          {!shown.length && <p className="py-8 text-center text-muted-foreground">No sites match the filter.</p>}
        </div>
      )}

      {sites && !list.length && (
        <Empty className="mt-10">
          <EmptyHeader>
            <EmptyMedia>
              <IconTile icon={GlobeIcon} tint="indigo" size="xl" />
            </EmptyMedia>
            <EmptyTitle className="text-xl">No sites yet</EmptyTitle>
            <EmptyDescription>A site is ready in about a minute: WordPress, HTTPS, bot protection and nightly backups included.</EmptyDescription>
          </EmptyHeader>
          {s.canCreate && (
            <EmptyContent>
              <Button onClick={openNewSite}>
                <PlusIcon data-icon="inline-start" />
                Create your first site
              </Button>
            </EmptyContent>
          )}
        </Empty>
      )}
    </Page>
  )
}

function ListSkeleton() {
  return (
    <div className="flex flex-col gap-2">
      <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-28 rounded-2xl" />
        ))}
      </div>
      {[0, 1, 2].map((i) => (
        <Skeleton key={i} className="h-20 rounded-2xl" />
      ))}
    </div>
  )
}

// ---- The fleet, as widgets ----

function Tile({ icon: Icon, label, children, className }: { icon: typeof GlobeIcon; label: string; children: React.ReactNode; className?: string }) {
  return (
    <div className={cn("flex min-w-0 flex-col gap-1 rounded-2xl bg-card p-4 card-shadow", className)}>
      <span className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
        <Icon className="size-3.5" />
        {label}
      </span>
      {children}
    </div>
  )
}

const TileValue = ({ children }: { children: React.ReactNode }) => (
  <span className="font-heading text-2xl leading-tight font-semibold tabular-nums">{children}</span>
)

function Fleet({ sites, stats, vitals: vs }: { sites: Site[]; stats: SiteStats[]; vitals: VitalValues[] }) {
  const live = sites.filter((x) => x.status === "active").length
  const busy = sites.length - live
  const total = (k: string) => sum(stats.map((x) => x.totals[k] || 0))
  const attacked = sites.filter((x) => x.shield_mode === "under_attack").length
  const off = sites.filter((x) => x.shield_mode === "off").length

  const avg = (i: number) => {
    const xs = vs.map((v) => v[i]).filter((x): x is number => x != null)
    return xs.length ? sum(xs) / xs.length : null
  }
  const fleetVitals: VitalValues = [avg(0), avg(1), avg(2)]

  const chart = useMemo(() => {
    if (!stats.length) return null
    const start = Math.min(...stats.map((x) => new Date(x.since).getTime()))
    const n = hoursSince(start)
    const views = new Array<number>(n).fill(0)
    for (const st of stats) hourly(st.series, start, n, "page_views").forEach((v, i) => (views[i] += v))
    return { views, start }
  }, [stats])

  return (
    // A bento: traffic and vitals stand tall at either end, the counts sit
    // between them. Laid out by the page's own width (@container).
    <div className="mb-6 @container">
    <div className="grid grid-cols-2 gap-3 @4xl:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(16rem,1.3fr)]">
      <Tile icon={ChartColumnIcon} label="Page views · 24 h" className="col-span-2 @4xl:col-span-1 @4xl:row-span-2">
        <TileValue>{stats.length ? fmtNum(total("page_views")) : "–"}</TileValue>
        {chart && (
          <div className="mt-auto pt-2">
            <LineChart series={[{ values: chart.views, color: "c1", area: true }]} height={72} label="Page views on every site, last 24 hours" />
          </div>
        )}
      </Tile>
      <Tile icon={ActivityIcon} label="Live sites">
        <TileValue>{fmtNum(live)}</TileValue>
        <span className="text-xs text-muted-foreground">
          of {sites.length}
          {busy ? ` · ${busy} not live` : ""}
        </span>
        {/* A dot per site (the first 60): green live, amber on its way, red broken. */}
        <span aria-hidden className="mt-1 flex flex-wrap gap-[3px]">
          {sites.slice(0, 60).map((x) => (
            <Tooltip key={x.id}>
              <TooltipTrigger
                render={
                  <span
                    className={cn(
                      "size-2 rounded-[3px]",
                      x.status === "active" ? "bg-success-fill" : x.status === "failed" || x.status === "suspended" ? "bg-danger-fill" : "bg-warning-fill",
                      x.parent_id && "opacity-50"
                    )}
                  />
                }
              />
              <TooltipContent>
                {x.primary_domain}: {x.status}
              </TooltipContent>
            </Tooltip>
          ))}
        </span>
      </Tile>
      <Tile icon={UsersIcon} label="Visitors · 24 h">
        <TileValue>{stats.length ? fmtNum(sum(stats.map((x) => x.unique_visitors))) : "–"}</TileValue>
        <span className="text-xs text-muted-foreground">{stats.length ? `${fmtBytes(total("bytes_out"))} served` : ""}</span>
      </Tile>
      <Tile icon={HeartPulseIcon} label="Fleet vitals" className="col-span-2 @4xl:col-span-1 @4xl:row-span-2">
        <VitalBars values={fleetVitals} protectionAsShare className="mt-3 flex-1 justify-evenly" />
      </Tile>
      <Tile icon={ShieldIcon} label="Threats blocked · 24 h" className="col-span-2">
        <TileValue>{stats.length ? fmtNum(total("blocked")) : "–"}</TileValue>
        <span className={cn("text-xs", attacked ? "font-medium text-danger" : off ? "text-warning" : "text-muted-foreground")}>
          {attacked ? `${attacked} in Under attack mode` : off ? `${off} with protection off` : "Protection on for every site"}
        </span>
      </Tile>
    </div>
    </div>
  )
}

// ---- A row per site ----

function SiteRow({ site, sites, underAttack }: { site: Site; sites: Site[]; underAttack: boolean }) {
  const s = useSession()
  const clustered = useClustered()
  const { data: nodes } = useNodes()
  const { data: stats } = useSiteStats(site)
  const { data: cpu } = useSiteCPU(site)
  useAttack(site)
  const parent = site.parent_id ? sites.find((x) => x.id === site.parent_id) : undefined
  const vs = vitals(site, stats, cpu)
  const open = sitePath(site.id)

  return (
    <article
      className="group col-span-full grid cursor-pointer grid-cols-subgrid items-center gap-x-5 rounded-2xl bg-card px-4 py-3 card-shadow transition-[transform,box-shadow] hover:-translate-y-px hover:shadow-[var(--shadow-card-raised)]"
      onClick={(e) => {
        if (!(e.target as HTMLElement).closest("a, button, input, select, label")) navigate(open)
      }}
    >
      <div className="flex min-w-0 items-center gap-3">
        <SiteAvatar site={site} />
        <div className="min-w-0">
          <a
            href={"https://" + site.primary_domain}
            target="_blank"
            rel="noopener"
            className="block truncate text-[0.9375rem] font-semibold text-foreground"
          >
            {site.primary_domain}
          </a>
          <div className="flex min-w-0 items-center gap-1.5 overflow-hidden text-xs whitespace-nowrap text-muted-foreground">
            <span className="shrink-0">PHP {site.php_version}</span>
            {parent && (
              <Badge variant="secondary" className="min-w-0">
                <span className="truncate">staging of {parent.primary_domain}</span>
              </Badge>
            )}
            {site.parent_id && !parent && <Badge variant="secondary">staging</Badge>}
            {clustered && <Badge variant="secondary">{nodes?.find((n) => n.id === (site.node || "local"))?.name ?? site.node ?? "local"}</Badge>}
            {site.access ? (
              <Badge variant="secondary" className="bg-primary/12 text-primary">
                Shared · {ACCESS_LABELS[site.access]}
              </Badge>
            ) : (
              site.account_id != null && s.me?.account_id !== site.account_id && <Badge variant="secondary">account #{site.account_id}</Badge>
            )}
            {!!site.shared_with && (
              <Badge variant="secondary" className="bg-primary/12 text-primary" title={`Shared with ${plural(site.shared_with, "person", "people")}`}>
                <UsersIcon />
                {site.shared_with}
              </Badge>
            )}
          </div>
        </div>
      </div>

      <div className="hidden min-w-0 @3xl:block">
        <Spark stats={stats} compact />
      </div>

      <dl className="m-0 hidden grid-cols-[4.5rem_4.5rem_3rem_3rem] items-start gap-x-4 @5xl:grid">
        <Stat k="Visitors" v={stats ? fmtNum(stats.unique_visitors) : "–"} />
        <Stat k="Views" v={stats ? fmtNum(stats.totals.page_views) : "–"} />
        <Stat k="CPU" v={cpu ? `${cpu.percent}%` : "–"} />
        <div className="flex flex-col items-end">
          <dt className="text-[0.6875rem] text-muted-foreground">Vitals</dt>
          <dd className="m-0 flex h-5 items-center">
            <VitalMeter values={vs} />
          </dd>
        </div>
      </dl>

      {/* One word on the row: what's wrong if something is, else protection.
          The dot on the avatar already says live or not. */}
      <div className="hidden justify-end @md:flex">
        {site.status !== "active" ? (
          <StatusPill status={site.status} />
        ) : (
          <StatusPill
            status={site.shield_mode}
            tone={underAttack || site.shield_mode === "under_attack" ? "bad" : site.shield_mode === "off" ? "warn" : "ok"}
            className="whitespace-nowrap"
          >
            {underAttack ? "Under attack" : SHIELD_LABELS[site.shield_mode] || site.shield_mode}
          </StatusPill>
        )}
      </div>

      <Button variant="tinted" size="sm" render={<a href={href(open)} />} nativeButton={false} aria-label={`Manage ${site.primary_domain}`}>
        Manage
        <ChevronRightIcon data-icon="inline-end" />
      </Button>
    </article>
  )
}

const Stat = ({ k, v }: { k: string; v: string }) => (
  <div className="flex flex-col items-end">
    <dt className="text-[0.6875rem] text-muted-foreground">{k}</dt>
    <dd className="m-0 text-sm font-semibold tabular-nums">{v}</dd>
  </div>
)

export function SiteAvatar({ site, size = "md" }: { site: Site; size?: "md" | "lg" }) {
  const dot =
    site.status === "active" ? "bg-success-fill" : site.status === "failed" || site.status === "suspended" ? "bg-danger-fill" : "bg-warning-fill"
  return (
    <span
      aria-hidden
      className={cn(
        "relative flex shrink-0 items-center justify-center font-semibold text-white uppercase",
        siteHue(site.id),
        size === "md" ? "size-10 rounded-[11px] text-lg" : "size-14 rounded-[15px] text-2xl"
      )}
    >
      {site.primary_domain.replace(/^www\./, "").slice(0, 1)}
      <span className={cn("absolute -right-0.5 -bottom-0.5 size-3 rounded-full ring-2 ring-card", dot)} />
    </span>
  )
}

// Spark: page views, and blocked requests on the same scale (a site under
// attack shows it), with a readout under the pointer.
export function Spark({ stats, compact, height = 60 }: { stats: SiteStats | undefined; compact?: boolean; height?: number }) {
  const data = useMemo(() => {
    if (!stats) return null
    const start = new Date(stats.since).getTime()
    const n = hoursSince(start)
    return { start, views: hourly(stats.series, start, n, "page_views"), blocked: hourly(stats.series, start, n, "blocked") }
  }, [stats])
  if (!data) return <p className="border-t border-dashed border-border py-2 text-xs text-muted-foreground">No traffic yet</p>
  const { start, views, blocked } = data
  const quiet = !sum(views) && !sum(blocked)
  return (
    <div className="min-w-0">
      {!compact && (
        <div className="mb-2 flex flex-wrap gap-4 text-xs text-muted-foreground">
          <span className="inline-flex items-center gap-1.5 before:h-[3px] before:w-3 before:rounded-sm before:bg-chart-1">Page views</span>
          <span className="inline-flex items-center gap-1.5 before:w-3 before:border-t-2 before:border-dashed before:border-chart-2">Blocked</span>
          <span className="ml-auto">last 24 h</span>
        </div>
      )}
      <LineChart
        height={compact ? 36 : height}
        series={[
          { values: views, color: "c1", area: true },
          { values: blocked, color: "c2" },
        ]}
        label={`Last 24 hours: ${fmtNum(sum(views))} page views (at most ${fmtNum(Math.max(...views))} an hour), ${fmtNum(sum(blocked))} requests blocked`}
        readout={
          compact
            ? undefined
            : (i) => `${new Date(start + i * 36e5).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })} · ${fmtNum(views[i])} views · ${fmtNum(blocked[i])} blocked`
        }
      />
      {quiet && !compact && <p className="mt-2 text-sm text-muted-foreground">No visits in the last 24 hours. This fills in as people visit.</p>}
    </div>
  )
}
