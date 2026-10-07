import { useState, type ReactNode } from "react"
import { keepPreviousData, useQuery } from "@tanstack/react-query"
import { BugIcon, RefreshCwIcon, TimerIcon, TurtleIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ask } from "@/components/app/confirm"
import { LoadError } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { StatusText } from "@/components/app/status"
import { showError } from "@/components/app/toaster"
import type { SectionProps } from "@/features/sites/sections"
import { api } from "@/lib/api"
import { fmtNum, fmtTime } from "@/lib/format"
import { useSession } from "@/lib/session"
import { cn } from "@/lib/utils"
import type { CPU } from "../data"
import { fmtMS, Meter, Note, Summary } from "./speed/shared"

// Insights: PHP response times, page cache hits, slow URLs and PHP errors
// (GET /sites/{id}/insights?hours=).

interface Insights {
  perf: {
    since: string
    totals: { php_requests: number; php_ms: number; slow: number; cache_hits: number; cache_misses: number }
    avg_ms: number
    p50_ms: number
    p95_ms: number
    p99_ms: number
    // histogram[i] counts responses up to buckets[i] ms; the last one, slower.
    histogram: number[]
    buckets: number[]
  }
  slow_requests: Array<{ method: string; path: string; count: number; avg_ms: number; max_ms: number; last_status: number; last_seen: string }>
  php_errors: Array<{ fingerprint: string; level: string; message: string; file: string; line: number; source: string; count: number; last_seen: string }>
  live: CPU | null
}

const LEVELS: Record<string, string> = { "fatal error": "failed", "parse error": "failed", warning: "warning", "recoverable fatal error": "failed" }

const PERIODS: Array<[string, string]> = [
  ["1", "Last hour"],
  ["24", "Last 24 hours"],
  ["168", "Last 7 days"],
  ["720", "Last 30 days"],
]

export default function InsightsSection({ site }: SectionProps) {
  const s = useSession()
  const [hours, setHours] = useState("24")
  const path = `/sites/${site.id}/insights?hours=${hours}`
  const q = useQuery<Insights>({
    queryKey: [path],
    queryFn: () => api<Insights>("GET", path),
    // Another period keeps the last numbers up until the new ones arrive.
    placeholderData: keepPreviousData,
  })

  const clear = async () => {
    if (!(await ask("Forget this site's PHP errors? New ones are collected from now on."))) return
    try {
      await api("DELETE", `/sites/${site.id}/insights/errors`)
      await q.refetch()
    } catch (e) {
      showError(e)
    }
  }

  return (
    <div>
      {q.data && <InsightsSummary ins={q.data} />}
      <div className="mb-4 flex flex-wrap items-end gap-2">
        <Field className="w-auto">
          <FieldLabel htmlFor={`insights-hours-${site.id}`}>Period</FieldLabel>
          <NativeSelect id={`insights-hours-${site.id}`} value={hours} onChange={(e) => setHours(e.target.value)}>
            {PERIODS.map(([v, label]) => (
              <NativeSelectOption key={v} value={v}>
                {label}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        <Button variant="tinted" disabled={q.isFetching} onClick={() => q.refetch()}>
          <RefreshCwIcon data-icon="inline-start" className={cn(q.isFetching && "animate-spin")} />
          Refresh
        </Button>
        {s.canChangeSite(site) && (
          <Button variant="destructive" onClick={clear}>
            Clear PHP errors
          </Button>
        )}
      </div>

      {q.isLoading ? (
        <div className="flex flex-col gap-4">
          <Skeleton className="h-40 rounded-2xl" />
          <Skeleton className="h-32 rounded-2xl" />
          <Skeleton className="h-32 rounded-2xl" />
        </div>
      ) : q.error && !q.data ? (
        <LoadError error={q.error} retry={() => q.refetch()} />
      ) : q.data ? (
        <InsightsBody ins={q.data} />
      ) : null}
    </div>
  )
}

function fatalCount(ins: Insights) {
  return ins.php_errors.filter((e) => LEVELS[e.level] === "failed").reduce((n, e) => n + e.count, 0)
}

function hitRateOf(ins: Insights) {
  const t = ins.perf.totals
  const pages = t.cache_hits + t.cache_misses
  return pages ? Math.round((100 * t.cache_hits) / pages) : null
}

function InsightsSummary({ ins }: { ins: Insights }) {
  const p = ins.perf
  const hitRate = hitRateOf(ins)
  const fatal = fatalCount(ins)
  if (!p.totals.php_requests && !fatal) return null
  return (
    <Summary>
      {p.totals.php_requests > 0 && <Badge variant="secondary">p95 {fmtMS(p.p95_ms)}</Badge>}
      {p.totals.php_requests > 0 && hitRate != null && <Badge variant="secondary">{hitRate}% cached</Badge>}
      {fatal > 0 && <Badge variant="destructive">{fmtNum(fatal)} fatal errors</Badge>}
    </Summary>
  )
}

const Stat = ({ k, v, title }: { k: string; v: ReactNode; title?: string }) => (
  <div className="flex flex-col" title={title}>
    <dt className="text-xs text-muted-foreground">{k}</dt>
    <dd className="m-0 font-heading text-xl font-bold tracking-[-0.02em] tabular-nums">{v}</dd>
  </div>
)

function InsightsBody({ ins }: { ins: Insights }) {
  const p = ins.perf
  const t = p.totals
  const hitRate = hitRateOf(ins)
  const any = t.php_requests > 0

  // Response time distribution: one meter per bucket.
  const total = p.histogram.reduce((a, b) => a + b, 0)
  const label = (i: number) =>
    i === 0 ? `≤ ${fmtMS(p.buckets[0])}` : i < p.buckets.length ? `${fmtMS(p.buckets[i - 1])} – ${fmtMS(p.buckets[i])}` : `> ${fmtMS(p.buckets[i - 1])}`

  const live = ins.live
  const now = live
    ? `Now: CPU ${live.percent}%` +
      (live.workers_percent != null ? ` · PHP workers ${live.workers_percent}% busy${live.queued ? `, ${live.queued} waiting` : ""}` : "") +
      (live.p95_ms != null ? ` · last minute p95 ${fmtMS(live.p95_ms)}` : "") +
      ` · ${live.replicas} replica(s)`
    : null

  return (
    <>
      <Section icon={TimerIcon} tint="purple" title="Response times">
        <div className="flex flex-col gap-4">
          <dl className="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
            <Stat k="PHP responses" v={fmtNum(t.php_requests)} title="Requests PHP answered: cache hits and static files are not counted" />
            <Stat k="Median" v={any ? fmtMS(p.p50_ms) : "–"} />
            <Stat k="95%" v={any ? fmtMS(p.p95_ms) : "–"} title="95% of PHP responses were at least this fast" />
            <Stat k="99%" v={any ? fmtMS(p.p99_ms) : "–"} />
            <Stat k="Over 1 s" v={fmtNum(t.slow)} />
            <Stat
              k="Page cache hits"
              v={hitRate != null ? `${hitRate}%` : "–"}
              title={`${fmtNum(t.cache_hits)} pages served from the cache, ${fmtNum(t.cache_misses)} rendered by PHP to be stored`}
            />
          </dl>
          {now && <Note>{now}</Note>}
          {total > 0 && (
            <div className="grid max-w-2xl grid-cols-[auto_minmax(4rem,1fr)_auto] items-center gap-x-3 gap-y-1.5 text-sm">
              {p.histogram.map((n, i) =>
                n ? (
                  <div key={i} className="contents">
                    <span className="tabular-nums">{label(i)}</span>
                    <Meter label={`Responses ${label(i)}`} value={n} max={total} />
                    <span className="text-muted-foreground tabular-nums">
                      {fmtNum(n)} ({((100 * n) / total).toFixed(n * 1000 < total ? 1 : 0)}%)
                    </span>
                  </div>
                ) : null
              )}
            </div>
          )}
        </div>
      </Section>

      <Section icon={TurtleIcon} tint="orange" title="Slowest URLs (over 1 s, most total time first)">
        <SimpleTable
          headers={["Slow URL", "Count", "Average", "Slowest", "Status", "Last"]}
          rows={ins.slow_requests.map((r) => [
            <span className="[overflow-wrap:anywhere]">{`${r.method} ${r.path}`}</span>,
            fmtNum(r.count),
            fmtMS(r.avg_ms),
            fmtMS(r.max_ms),
            <span className={cn(r.last_status >= 500 && "text-danger")}>{String(r.last_status)}</span>,
            fmtTime(r.last_seen),
          ])}
          className="[&_td:first-child]:whitespace-normal"
        />
      </Section>

      <Section icon={BugIcon} tint="red" title="PHP errors (most frequent first)">
        <SimpleTable
          headers={["PHP error", "Whose", "Count", "Last"]}
          rows={ins.php_errors.map((e) => [
            <div className="font-normal">
              <StatusText status={LEVELS[e.level] || "unknown"} className="font-medium">
                {e.level}
              </StatusText>{" "}
              <span className="[overflow-wrap:anywhere]">{e.message}</span>
              {e.file && <div className="text-xs text-muted-foreground [overflow-wrap:anywhere]">{`${e.file}:${e.line}`}</div>}
            </div>,
            e.source.replace(":", ": "),
            fmtNum(e.count),
            fmtTime(e.last_seen),
          ])}
          className="[&_td]:whitespace-normal"
        />
      </Section>

      <Note>
        Response times come from the web server's log and include waiting for a PHP worker. PHP errors are read from the site's own error log, grouped by
        message and place; the plugin or theme is taken from the file, or from the stack trace for errors inside WordPress.
      </Note>
    </>
  )
}
