import { XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Progress } from "@/components/ui/progress"
import { StatusText } from "@/components/app/status"
import { JOB_NAMES, dismissJob, siteLabel, useJobs } from "@/lib/jobs"
import type { Job } from "@/lib/types"

// The jobs running now, and those that ended while followed, bottom right.
export function JobTray() {
  const { active, finished } = useJobs()
  if (!active.length && !finished.length) return null
  return (
    <div aria-live="polite" className="fixed right-4 bottom-4 z-40 flex w-[min(360px,calc(100vw-2rem))] flex-col gap-2">
      {active.map((j) => (
        <JobRow key={j.id} job={j} />
      ))}
      {finished.map((j) => (
        <JobRow key={"f" + j.id} job={j} done />
      ))}
    </div>
  )
}

function JobRow({ job: j, done }: { job: Job; done?: boolean }) {
  const detail =
    j.status === "failed" ? j.error : j.status === "succeeded" ? "Done" : j.status === "queued" ? "Waiting for another operation on this site…" : j.step || "Starting…"
  return (
    <div className="flex items-start gap-3 rounded-lg bg-popover p-3 text-base shadow-lg ring-1 ring-border">
      <div className="min-w-0 flex-1">
        <div className="truncate">
          <strong className="font-semibold">{JOB_NAMES[j.kind] || j.kind}</strong>
          {j.site_id && <span className="text-muted-foreground"> · {siteLabel(j.site_id)}</span>}
        </div>
        <div className={j.status === "failed" ? "text-xs text-danger" : "text-xs text-muted-foreground"}>{detail}</div>
        {!done && <Progress value={Math.max(0, j.progress || 0)} className="mt-2" />}
      </div>
      {done ? (
        <Button variant="ghost" size="icon-xs" aria-label="Dismiss" onClick={() => dismissJob(j.id)}>
          <XIcon />
        </Button>
      ) : (
        <StatusText status={j.status} className="text-xs font-medium" />
      )}
    </div>
  )
}
