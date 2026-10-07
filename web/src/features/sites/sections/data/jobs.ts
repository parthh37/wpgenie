import { useState } from "react"
import { showError } from "@/components/app/toaster"
import { startJob, useJobs, type JobResult } from "@/lib/jobs"

type Method = "POST" | "PUT" | "DELETE"
type OnDone<R> = (v: JobResult, res: R) => void | Promise<void>

// useSiteJob runs a long operation on a site (202 {job_id}) and says
// whether one of those kinds is under way for it: started here and not
// ended yet, or running in the tray (started elsewhere, or before this
// section was opened).
export function useSiteJob(siteId: string, kinds: string[]) {
  const { active } = useJobs()
  const [starting, setStarting] = useState(false)
  const running = starting || active.some((j) => j.site_id === siteId && kinds.includes(j.kind))

  // start starts the job; a refused request throws (for a dialog to show).
  const start = async <R extends { job_id: string }>(method: Method, path: string, body?: unknown, onDone?: OnDone<R>): Promise<R> => {
    setStarting(true)
    try {
      return await startJob<R>(method, path, body, async (v, res) => {
        setStarting(false)
        if (onDone) await onDone(v, res)
      })
    } catch (e) {
      setStarting(false)
      throw e
    }
  }

  // run is start with the error shown as a toast; null if it failed.
  const run = async <R extends { job_id: string }>(method: Method, path: string, body?: unknown, onDone?: OnDone<R>): Promise<R | null> => {
    try {
      return await start<R>(method, path, body, onDone)
    } catch (e) {
      showError(e)
      return null
    }
  }

  return { running, start, run }
}
