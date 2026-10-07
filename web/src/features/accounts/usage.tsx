import type { ReactNode } from "react"
import { BTable } from "@/components/app/blocks"
import { fmtBytes, fmtNum, fmtTime } from "@/lib/format"
import { useSites } from "@/lib/query"
import { href, sitePath } from "@/lib/router"
import { cn } from "@/lib/utils"
import type { BurstBalance, Usage } from "./types"

// An account's month: sites, disk and bandwidth against its plan, its
// burst minutes, and each site's share (the legacy usageBars).

// UsageRow is a label, a meter against the limit (none when unlimited) and
// the numbers. The meter turns orange from 80% and red when it's full.
export function UsageRow({
  label,
  used,
  limit,
  fmt,
  extra,
}: {
  label: ReactNode
  used: number
  limit: number
  fmt: (n: number) => string
  extra?: string
}) {
  const text = (limit ? `${fmt(used)} of ${fmt(limit)}` : `${fmt(used)} (unlimited)`) + (extra ?? "")
  const pct = limit ? Math.min(100, (Math.min(used, limit) / limit) * 100) : 0
  const tone = used >= limit * 0.99 ? "bg-danger-fill" : used >= limit * 0.8 ? "bg-warning-fill" : "bg-success-fill"
  return (
    <div className="grid grid-cols-[8rem_1fr] items-center gap-x-4 gap-y-1 py-1.5 sm:grid-cols-[9rem_minmax(8rem,1fr)_auto]">
      <span className="text-sm font-medium">{label}</span>
      {limit ? (
        <div
          role="meter"
          aria-label={typeof label === "string" ? label : undefined}
          aria-valuemin={0}
          aria-valuemax={limit}
          aria-valuenow={Math.min(used, limit)}
          aria-valuetext={text}
          className="h-2 overflow-hidden rounded-full bg-muted"
        >
          <div className={cn("h-full rounded-full transition-[width]", tone)} style={{ width: `${pct}%` }} />
        </div>
      ) : (
        <span className="max-sm:hidden" />
      )}
      <span className="text-sm text-muted-foreground tabular-nums max-sm:col-start-2">{text}</span>
    </div>
  )
}

// BurstRow: burst minutes used this month against the plan's, and the
// bought minutes left.
export function BurstRow({ burst: b }: { burst: BurstBalance }) {
  return <UsageRow label="Burst minutes" used={b.used} limit={b.unlimited ? 0 : b.included} fmt={fmtNum} extra={b.credit ? ` · ${fmtNum(b.credit)} extra` : ""} />
}

export const monthOf = (t: string) => new Date(t).toLocaleDateString([], { month: "long", year: "numeric", timeZone: "UTC" })

// UsageBars shows an account's month; burst is its balance (optional).
export function UsageBars({ usage: u, burst: b }: { usage: Usage; burst?: BurstBalance | null }) {
  const { data: sites } = useSites()
  // A site the signed-in user can open links to it.
  const site = (id: string) => {
    const x = sites?.find((y) => y.id === id)
    return x ? <a href={href(sitePath(x.id))}>{x.primary_domain}</a> : id
  }
  const perSite = u.per_site ?? []
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-muted-foreground">
        {monthOf(u.month_start)}
        {u.includes_customers ? " · totals include every customer account" : ""}
      </p>
      <div className="flex flex-col">
        <UsageRow label="Sites" used={u.sites} limit={u.max_sites} fmt={String} />
        <UsageRow label="Disk" used={u.disk_bytes} limit={u.disk_limit_bytes} fmt={fmtBytes} />
        <UsageRow label="Bandwidth" used={u.bandwidth_bytes} limit={u.bandwidth_limit_bytes} fmt={fmtBytes} />
        {b && b.allowed && <BurstRow burst={b} />}
      </div>
      {perSite.length > 0 && (
        <div className="overflow-x-auto">
          <BTable
            caption="Usage per site"
            cols={["Site", { label: "Files", num: true }, { label: "Database", num: true }, { label: "Bandwidth", num: true }, "Measured"]}
            rows={perSite.map((s) => ({
              key: s.site_id,
              cells: [
                site(s.site_id),
                fmtBytes(s.files_bytes),
                fmtBytes(s.db_bytes),
                fmtBytes(s.bandwidth_bytes),
                s.measured_at ? fmtTime(s.measured_at) : "not yet",
              ],
            }))}
          />
        </div>
      )}
    </div>
  )
}
