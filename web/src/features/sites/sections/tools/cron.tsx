import { useState } from "react"
import { ClockIcon, RefreshCwIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, Banner, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtAgo, fmtTime, plural } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import { Note } from "../speed/shared"
import type { CronEvent, CronInfo } from "./types"

// Scheduled tasks (WP-Cron): what WordPress and its plugins have planned,
// whether WPGenie's every-minute cron is running them, and "Run now" for
// one (GET /sites/{id}/tools/cron, POST …/cron/run).

const SHOWN = 50

function RunnerBanner({ info }: { info: CronInfo }) {
  const r = info.runner
  switch (r.state) {
    case "ok":
      return (
        <Banner tone="ok" title="Scheduled tasks are running">
          WPGenie runs them every minute; the last run was {fmtAgo(r.last_run)}.
        </Banner>
      )
    case "waiting":
      return (
        <Banner title="Waiting for the first run">
          WPGenie runs scheduled tasks every minute; the server restarted recently, so there's no run to report yet.
        </Banner>
      )
    case "staging":
      return (
        <Banner tone="warn" title="Staging sites don't run scheduled tasks">
          So a copy never sends e-mail or charges renewals. Tasks pile up here as late; that's expected.
        </Banner>
      )
    case "old_image":
      return (
        <Banner tone="warn" title="WordPress runs its own scheduled tasks">
          This site's PHP containers predate WPGenie's cron: tasks only run when visitors load pages. Changing the site's resources (or a server update)
          brings them up to date.
        </Banner>
      )
    default:
      return (
        <Banner tone="bad" title="Scheduled tasks are failing">
          {r.last_run ? `Last attempt ${fmtAgo(r.last_run)}` : "No recent run"}
          {r.error ? `: ${r.error}` : "."}
        </Banner>
      )
  }
}

export function CronCard({ site }: { site: Site }) {
  const s = useSession()
  const path = `/sites/${site.id}/tools/cron`
  const q = useApi<CronInfo>(path)
  const [all, setAll] = useState(false)
  const can = s.canChangeSite(site) && !site.parent_id
  const events = q.data?.events ?? []
  const shown = all ? events : events.slice(0, SHOWN)

  const runNow = async (e: CronEvent) => {
    if (
      !(await ask(
        `Run ${e.hook} now? WordPress runs it within a few seconds, along with any other task that is due${e.recurrence ? `, and it goes on repeating every ${e.recurrence}` : ""}.`,
        { ok: "Run now" }
      ))
    )
      return
    await api("POST", `${path}/run`, { hook: e.hook, time: Math.round(new Date(e.next_run).getTime() / 1000), sig: e.sig })
    notify(`${e.hook} is running`)
    setTimeout(() => invalidate(path), 5000)
  }

  return (
    <Section
      icon={ClockIcon}
      tint="teal"
      title="Scheduled tasks"
      description="What WordPress and its plugins do on a schedule: publishing scheduled posts, checking for updates, sending newsletters, cleaning up. WPGenie runs them every minute instead of waiting for visitors."
      action={
        <Button variant="tinted" size="sm" onClick={() => q.refetch()} disabled={q.isFetching}>
          <RefreshCwIcon data-icon="inline-start" className={cn(q.isFetching && "animate-spin")} />
          Refresh
        </Button>
      }
    >
      {q.isPending ? (
        <div className="flex flex-col gap-2">
          <Skeleton className="h-14 rounded-2xl" />
          <Skeleton className="h-40 rounded-xl" />
        </div>
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <div className="flex flex-col gap-2">
          <RunnerBanner info={q.data} />
          {q.data.overdue > 0 && !site.parent_id && (
            <p className="text-sm text-warning">
              {plural(q.data.overdue, "task is", "tasks are")} more than 10 minutes late.
            </p>
          )}
          <SimpleTable
            headers={["Task", "Next run", "Repeats", ""]}
            empty="Nothing is scheduled."
            rowKey={(i) => `${shown[i].hook}-${shown[i].sig}-${shown[i].next_run}`}
            rows={shown.map((e) => [
              <div className="flex flex-col gap-0.5">
                <code className="text-xs [overflow-wrap:anywhere]">{e.hook}</code>
                {e.args > 0 && <span className="text-xs font-normal text-muted-foreground">with {plural(e.args, "argument")}</span>}
              </div>,
              <span title={fmtTime(e.next_run)} className={cn(e.overdue && "text-warning")}>
                {fmtAgo(e.next_run)}
                {e.overdue && (
                  <Badge variant="destructive" className="ml-1.5">
                    Late
                  </Badge>
                )}
              </span>,
              <span className="text-muted-foreground">{e.recurrence ? `Every ${e.recurrence}` : "Once"}</span>,
              can && e.sig ? (
                <div className="flex justify-end">
                  <ActionButton size="sm" run={() => runNow(e)}>
                    Run now
                  </ActionButton>
                </div>
              ) : null,
            ])}
          />
          {events.length > SHOWN && (
            <Button variant="link" className="self-start px-0" onClick={() => setAll(!all)}>
              {all ? "Show fewer" : `Show all ${events.length}`}
            </Button>
          )}
          <Note>Times are in your browser's time zone.</Note>
        </div>
      )}
    </Section>
  )
}
