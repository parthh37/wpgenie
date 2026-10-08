import { useEffect, useRef } from "react"
import { BugIcon, RefreshCwIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtAgo, fmtBytes, fmtTime, plural } from "@/lib/format"
import { invalidate, queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import { Note } from "../speed/shared"
import type { Debug, DebugLog } from "./types"

// Debug mode: WordPress logs notices and warnings for a day, to the PHP
// error log outside the site's public files (PUT /sites/{id}/tools/debug),
// and the end of that log (GET …/debug/log, developer access).

export function DebugCard({ site }: { site: Site }) {
  const s = useSession()
  const can = s.canChangeSite(site)
  const path = `/sites/${site.id}/tools/debug`
  const q = useApi<Debug>(path)
  const on = !!q.data?.on

  const set = async (want: boolean) => {
    const d = await api<Debug>("PUT", path, { on: want })
    queryClient.setQueryData([path], d)
    notify(want ? `Debug mode on until ${fmtTime(d.until)}` : "Debug mode off")
    await invalidate(`${path}/log`)
  }

  return (
    <Section
      icon={BugIcon}
      tint="red"
      title={
        <>
          Debug mode
          {on && <Badge variant="destructive">On</Badge>}
        </>
      }
      description="When something misbehaves, turn this on: WordPress then writes every notice and warning to the site's error log, which you can read below. Visitors never see the messages, and the log isn't reachable from the web. It turns itself off after 24 hours."
    >
      {q.isPending ? (
        <Skeleton className="h-10 rounded-xl" />
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <div className="flex flex-col gap-4">
          <p className="text-sm">
            {on ? (
              <>
                On until <strong className="font-semibold">{fmtTime(q.data?.until)}</strong> ({fmtAgo(q.data?.until)}).
              </>
            ) : (
              "Off: only errors and warnings are logged, as usual."
            )}
          </p>
          {can && (
            <div className="flex flex-wrap gap-2">
              {on ? (
                <>
                  <ActionButton variant="default" run={() => set(false)}>
                    Turn off now
                  </ActionButton>
                  <ActionButton run={() => set(true)} title="Keep it on for another 24 hours from now">
                    Keep on for 24 more hours
                  </ActionButton>
                </>
              ) : (
                <ActionButton variant="default" run={() => set(true)}>
                  Turn on for 24 hours
                </ActionButton>
              )}
            </div>
          )}
          {can && <LogViewer site={site} />}
        </div>
      )}
    </Section>
  )
}

function LogViewer({ site }: { site: Site }) {
  const path = `/sites/${site.id}/tools/debug/log`
  const q = useApi<DebugLog>(path, { refetchOnWindowFocus: false })
  const lines = q.data?.lines ?? []
  // The newest lines are at the end: start there.
  const pre = useRef<HTMLPreElement>(null)
  useEffect(() => {
    if (pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [q.data])

  const clear = async () => {
    if (
      !(await ask(
        `Clear the error log of ${site.primary_domain}? Its lines are deleted from the file. Errors already grouped under Insights stay there.`,
        { ok: "Clear log", danger: true }
      ))
    )
      return
    await api("DELETE", path)
    notify("Error log cleared")
    await invalidate(path)
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-sm font-medium">
          Error log
          {q.data && (
            <span className="font-normal text-muted-foreground">
              {" "}
              · {q.data.truncated ? `last ${plural(lines.length, "line")}` : plural(lines.length, "line")} of {fmtBytes(q.data.size)}
            </span>
          )}
        </span>
        <div className="flex gap-2">
          <Button variant="tinted" size="sm" onClick={() => q.refetch()} disabled={q.isFetching}>
            <RefreshCwIcon data-icon="inline-start" className={cn(q.isFetching && "animate-spin")} />
            Refresh
          </Button>
          <ActionButton size="sm" variant="destructive" run={clear} disabled={!lines.length}>
            Clear
          </ActionButton>
        </div>
      </div>
      {q.isPending ? (
        <Skeleton className="h-40 rounded-xl" />
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : lines.length ? (
        <pre
          ref={pre}
          tabIndex={0}
          aria-label="End of the error log"
          className="max-h-96 overflow-auto rounded-xl bg-muted p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]"
        >
          {lines.join("\n")}
        </pre>
      ) : (
        <Note>The error log is empty.</Note>
      )}
    </div>
  )
}
