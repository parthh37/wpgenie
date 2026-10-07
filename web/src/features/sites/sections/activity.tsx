import { useMemo, useState } from "react"
import {
  ArchiveIcon, ArrowRightLeftIcon, BotIcon, CloudIcon, CodeIcon, FlameIcon, GaugeIcon, GitBranchIcon, HistoryIcon, ImageIcon,
  KeyRoundIcon, LinkIcon, LockIcon, MailIcon, RefreshCwIcon, ScaleIcon, ShieldIcon, SparklesIcon, UploadCloudIcon, UserIcon,
  type LucideIcon,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Chips, EmptyState, LoadError } from "@/components/app/blocks"
import { IconTile, type Tint } from "@/components/app/icon-tile"
import { fmtAgo, fmtDate, fmtTime, humanize } from "@/lib/format"
import { useApi } from "@/lib/query"
import type { SectionProps } from "../sections"

// The site's activity log (GET /sites/{id}/events): the last 50 events,
// newest first, as a timeline grouped by day.

interface SiteEvent {
  id: number
  time: string
  kind: string
  message: string
}

// Each kind of event gets the symbol and colour of the place it comes from.
const KINDS: Record<string, [LucideIcon, Tint]> = {
  backup: [ArchiveIcon, "orange"],
  staging: [GitBranchIcon, "teal"],
  domain: [LinkIcon, "blue"],
  certificate: [LockIcon, "green"],
  security: [ShieldIcon, "green"],
  cdn: [CloudIcon, "cyan"],
  offload: [UploadCloudIcon, "teal"],
  scale: [ScaleIcon, "pink"],
  autoscale: [GaugeIcon, "pink"],
  burst: [FlameIcon, "orange"],
  migrate: [ArrowRightLeftIcon, "graphite"],
  php: [CodeIcon, "indigo"],
  update: [RefreshCwIcon, "green"],
  optimize: [SparklesIcon, "purple"],
  images: [ImageIcon, "purple"],
  mail: [MailIcon, "blue"],
  account: [UserIcon, "gray"],
  "wp-admin": [KeyRoundIcon, "yellow"],
  sftp: [KeyRoundIcon, "graphite"],
  analyser: [BotIcon, "purple"],
}
const kindOf = (k: string): [LucideIcon, Tint] => KINDS[k] ?? [HistoryIcon, "brown"]
const kindLabel = (k: string) => humanize(k).replace(/^./, (c) => c.toUpperCase())

export default function ActivitySection({ site }: SectionProps) {
  const q = useApi<SiteEvent[]>(`/sites/${site.id}/events?limit=50`)
  const [kind, setKind] = useState("all")
  const events = useMemo(() => q.data ?? [], [q.data])

  const kinds = useMemo(() => {
    const n = new Map<string, number>()
    events.forEach((e) => n.set(e.kind, (n.get(e.kind) ?? 0) + 1))
    return [...n.entries()].sort((a, b) => b[1] - a[1])
  }, [events])

  const shown = kind === "all" ? events : events.filter((e) => e.kind === kind)
  const days = useMemo(() => {
    const out: Array<{ day: string; items: SiteEvent[] }> = []
    for (const e of shown) {
      const day = new Date(e.time).toDateString()
      const last = out[out.length - 1]
      if (last && last.day === day) last.items.push(e)
      else out.push({ day, items: [e] })
    }
    return out
  }, [shown])

  if (q.error) return <LoadError error={q.error} retry={() => q.refetch()} />
  if (q.isLoading) {
    return (
      <div className="flex flex-col gap-3">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-14 rounded-2xl" />
        ))}
      </div>
    )
  }
  if (!events.length) {
    return (
      <EmptyState icon={HistoryIcon} tint="brown" title="No activity yet.">
        Backups, domain changes, updates and other changes to this site show up here.
      </EmptyState>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        {kinds.length > 1 ? (
          <Chips
            label="Show"
            className="min-w-0 flex-1"
            current={kind}
            onPick={setKind}
            items={[["all", "All", events.length], ...kinds.map(([k, n]) => [k, kindLabel(k), n] as [string, string, number])]}
          />
        ) : (
          <p className="min-w-0 flex-1 text-sm text-muted-foreground">The last {events.length} events.</p>
        )}
        <Button variant="tinted" size="sm" onClick={() => q.refetch()} disabled={q.isFetching}>
          <RefreshCwIcon data-icon="inline-start" />
          Refresh
        </Button>
      </div>

      {days.map(({ day, items }) => (
        <section key={day} aria-label={fmtDate(items[0].time)}>
          <h3 className="mb-2 px-1 text-xs font-semibold tracking-wide text-muted-foreground uppercase">{dayLabel(items[0].time)}</h3>
          <ol className="divide-y divide-border/60 overflow-hidden rounded-2xl bg-card card-shadow">
            {items.map((e) => {
              const [Icon, tint] = kindOf(e.kind)
              return (
                <li key={e.id} className="flex gap-3 px-4 py-3">
                  <IconTile icon={Icon} tint={tint} size="sm" className="mt-0.5" />
                  <div className="min-w-0 flex-1">
                    <p className="text-sm [overflow-wrap:anywhere]">{e.message}</p>
                    <p className="mt-0.5 text-xs text-muted-foreground">
                      <span className="font-medium">{kindLabel(e.kind)}</span>
                      {" · "}
                      <time dateTime={e.time} title={fmtTime(e.time)}>
                        {new Date(e.time).toLocaleTimeString([], { timeStyle: "short" })} ({fmtAgo(e.time)})
                      </time>
                    </p>
                  </div>
                </li>
              )
            })}
          </ol>
        </section>
      ))}
    </div>
  )
}

function dayLabel(t: string) {
  const d = new Date(t)
  const today = new Date()
  const yesterday = new Date(Date.now() - 86_400_000)
  if (d.toDateString() === today.toDateString()) return "Today"
  if (d.toDateString() === yesterday.toDateString()) return "Yesterday"
  return fmtDate(t)
}
