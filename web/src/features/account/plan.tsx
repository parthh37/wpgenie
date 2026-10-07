import { useQuery } from "@tanstack/react-query"
import { LayersIcon, RulerIcon } from "lucide-react"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, LoadError } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { BuyBurstButton } from "@/features/billing/shared/burst"
import { api } from "@/lib/api"
import { fmtBytes, fmtNum, fmtTime } from "@/lib/format"
import { invalidate, useApi, useSites } from "@/lib/query"
import { cn } from "@/lib/utils"

// A tenant's plan and this month's usage (the legacy loadAccountPlan).

export interface Limits {
  max_sites: number
  disk_mb: number
  bandwidth_gb: number
  max_replicas: number
  max_memory_mb: number
  max_cpus: number
  max_domains: number
  burst_minutes: number
  features: string[] | null
}

export interface TenantAccount {
  id: number
  name: string
  kind: string
  status: string
  plan_id: string
  plan: { id: string; name: string } | null
  limits: Limits
  effectively_suspended: boolean
  sites: number
}

interface Usage {
  month_start: string
  includes_customers: boolean
  sites: number
  max_sites: number
  disk_bytes: number
  disk_limit_bytes: number
  bandwidth_bytes: number
  bandwidth_limit_bytes: number
  per_site: Array<{ site_id: string; files_bytes: number; db_bytes: number; bandwidth_bytes: number; measured_at?: string }> | null
}

interface Burst {
  allowed: boolean
  unlimited: boolean
  included: number
  used: number
  credit: number
  remaining: number
}

const limitText = (n: number, unit: string) => (n ? `${n}${unit}` : "unlimited")

export function planSummary(p: Limits) {
  const features = p.features ?? []
  return (
    `${limitText(p.max_sites, " sites")} · ${limitText(p.disk_mb, " MB disk")} · ${limitText(p.bandwidth_gb, " GB/month")} · ` +
    `per site ${limitText(p.max_replicas, " replicas")} × ${limitText(p.max_memory_mb, " MB")}, ${limitText(p.max_cpus, " CPU")}` +
    (features.includes("burst") ? ` · burst ${limitText(p.burst_minutes, " min/month")}` : "") +
    (features.length ? ` · ${features.join(", ")}` : "")
  )
}

// UsageRow: a label, a meter against the limit (none when unlimited), and
// the numbers. Amber past 80%, red at the limit.
function UsageRow({ label, used, limit, fmt, extra }: { label: string; used: number; limit: number; fmt: (n: number) => string; extra?: string }) {
  const pct = limit ? Math.min(100, (used / limit) * 100) : 0
  const text = (limit ? `${fmt(used)} of ${fmt(limit)}` : `${fmt(used)} (unlimited)`) + (extra ?? "")
  return (
    <div className="grid grid-cols-[7.5rem_1fr] items-center gap-x-4 gap-y-1 py-2 sm:grid-cols-[9rem_1fr_minmax(10rem,auto)]">
      <span className="text-sm font-medium">{label}</span>
      {limit ? (
        <div
          role="meter"
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={limit}
          aria-valuenow={Math.min(used, limit)}
          aria-valuetext={text}
          className="h-2 overflow-hidden rounded-full bg-muted"
        >
          <div
            className={cn("h-full rounded-full", pct >= 99 ? "bg-danger-fill" : pct >= 80 ? "bg-warning-fill" : "bg-primary")}
            style={{ width: `${Math.max(pct, used ? 2 : 0)}%` }}
          />
        </div>
      ) : (
        <span />
      )}
      <span className="col-start-2 text-sm text-muted-foreground tabular-nums sm:col-start-auto sm:text-right">{text}</span>
    </div>
  )
}

export function AccountPlan({ account: a }: { account: TenantAccount }) {
  const sites = useSites()
  const usage = useApi<Usage>(`/accounts/${a.id}/usage`)
  // Burst is optional (older servers, plans without it).
  const burst = useQuery<Burst | null>({
    queryKey: [`/accounts/${a.id}/burst`],
    queryFn: () => api<Burst>("GET", `/accounts/${a.id}/burst`).catch(() => null),
  })
  const u = usage.data
  const b = burst.data
  const domain = (id: string) => sites.data?.find((x) => x.id === id)?.primary_domain || id

  return (
    <Section
      icon={LayersIcon}
      tint="purple"
      title={`${a.name} · plan ${a.plan?.name ?? a.plan_id}`}
      description={planSummary(a.limits)}
      action={<StatusPill status={a.effectively_suspended ? "suspended" : a.status} />}
    >
      {a.effectively_suspended && (
        <p role="alert" className="mb-3 rounded-xl bg-danger-fill/12 p-3 text-sm text-danger">
          This account is suspended: its sites show a "temporarily unavailable" page and nothing can be changed. Contact your provider.
        </p>
      )}
      {usage.isLoading && <Skeleton className="h-32 rounded-xl" />}
      {usage.error && <LoadError error={usage.error} retry={() => usage.refetch()} />}
      {u && (
        <>
          <p className="mb-1 text-sm text-muted-foreground">
            {new Date(u.month_start).toLocaleDateString([], { month: "long", year: "numeric", timeZone: "UTC" })}
            {u.includes_customers ? " · totals include every customer account" : ""}
          </p>
          <div className="divide-y divide-border/60">
            <UsageRow label="Sites" used={u.sites} limit={u.max_sites} fmt={String} />
            <UsageRow label="Disk" used={u.disk_bytes} limit={u.disk_limit_bytes} fmt={fmtBytes} />
            <UsageRow label="Bandwidth" used={u.bandwidth_bytes} limit={u.bandwidth_limit_bytes} fmt={fmtBytes} />
            {b?.allowed && (
              <UsageRow
                label="Burst minutes"
                used={b.used}
                limit={b.unlimited ? 0 : b.included}
                fmt={fmtNum}
                extra={b.credit ? ` · ${fmtNum(b.credit)} extra` : ""}
              />
            )}
          </div>
          {!!u.per_site?.length && (
            <BTable
              className="mt-3"
              caption="Usage per site"
              cols={["Site", { label: "Files", num: true }, { label: "Database", num: true }, { label: "Bandwidth", num: true }, "Measured"]}
              rows={u.per_site.map((x) => ({
                key: x.site_id,
                cells: [
                  domain(x.site_id),
                  fmtBytes(x.files_bytes),
                  fmtBytes(x.db_bytes),
                  fmtBytes(x.bandwidth_bytes),
                  x.measured_at ? fmtTime(x.measured_at) : "not yet",
                ],
              }))}
            />
          )}
          {b?.allowed && !b.unlimited && b.remaining <= 0 && (
            <p className="mt-3 flex flex-wrap items-center gap-2 text-sm text-warning">
              Out of burst minutes: sites stay at their normal size.
              <BuyBurstButton />
            </p>
          )}
        </>
      )}
      <div className="mt-4 flex flex-wrap gap-2">
        <ActionButton
          run={async () => {
            await api("POST", `/accounts/${a.id}/usage/measure`)
            await invalidate(`/accounts/${a.id}/`)
          }}
        >
          <RulerIcon data-icon="inline-start" />
          Measure disk now
        </ActionButton>
      </div>
    </Section>
  )
}
