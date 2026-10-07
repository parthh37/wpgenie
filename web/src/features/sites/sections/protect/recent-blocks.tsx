import { BanIcon, RefreshCwIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { StatusPill, type Tone } from "@/components/app/status"
import { fmtTime } from "@/lib/format"
import { useApi } from "@/lib/query"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import { WRAP } from "./shared"
import type { ShieldEvent } from "./types"

// What protection did on this site lately: the global Protection page's
// event log (GET /security/events), for this site only. Firewall matches
// in "Log only" mode show here as "detect".

const VERDICT: Record<string, [word: string, tone: Tone]> = {
  block: ["blocked", "bad"],
  ban: ["banned", "bad"],
  throttle: ["slowed down", "bad"],
  challenge: ["checked", "warn"],
  detect: ["detect", "warn"],
  attack: ["attack began", "bad"],
  attack_end: ["attack over", "ok"],
  allow: ["allowed", "ok"],
}

export function RecentBlocks({ site }: { site: Site }) {
  const q = useApi<ShieldEvent[]>(`/security/events?site=${encodeURIComponent(site.id)}&limit=50`)
  return (
    <Section
      icon={BanIcon}
      tint="orange"
      title="Recent blocks"
      description="The latest visitors protection blocked, slowed down or checked on this site, newest first."
      action={
        <Button variant="tinted" size="sm" disabled={q.isFetching} onClick={() => q.refetch()}>
          <RefreshCwIcon data-icon="inline-start" />
          Refresh
        </Button>
      }
    >
      {q.isPending ? (
        <Skeleton className="h-24 rounded-xl" />
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <SimpleTable
          headers={["Time", "Visitor", "Action", "Reason", "Path"]}
          empty="Nothing blocked recently."
          rows={(q.data ?? []).map((e) => {
            const [word, tone] = VERDICT[e.verdict] ?? [e.verdict, "neutral" as Tone]
            return [
              <span className="whitespace-nowrap">{fmtTime(e.time)}</span>,
              e.ip ? <span className="font-mono text-xs">{e.ip}</span> : "–",
              <StatusPill status={e.verdict} tone={tone}>
                {word}
              </StatusPill>,
              <span className={WRAP}>{e.reason}</span>,
              e.path ? <span className={cn(WRAP, "font-mono text-xs")}>{e.path}</span> : "–",
            ]
          })}
        />
      )}
    </Section>
  )
}
