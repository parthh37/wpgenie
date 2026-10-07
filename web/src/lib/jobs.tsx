import { useSyncExternalStore } from "react"
import { api } from "@/lib/api"
import { invalidate, queryClient } from "@/lib/query"
import { showError } from "@/components/app/toaster"
import type { Job, Site } from "@/lib/types"

// Long operations (backups, restores, staging, PHP changes…) answer 202
// {job_id} and run as jobs. The tray shows the active ones and those that
// ended while followed; followJob calls back when a job ends.

export const JOB_NAMES: Record<string, string> = {
  create: "Creating site",
  backup: "Backup",
  restore: "Restore",
  "restore-new": "Restore as a new site",
  staging: "Creating staging site",
  push: "Push to live",
  php: "PHP change",
  "primary-domain": "Primary domain change",
  "repo-upkeep": "Backup upkeep",
  images: "Image conversion",
}

export interface JobResult {
  job: Job
  secret?: Record<string, string>
}

type OnDone = ((v: JobResult) => void | Promise<void>) | undefined

const followed = new Map<string, OnDone>()
let active: Job[] = []
let finished: Job[] = [] // ended while followed, newest first
let timer: ReturnType<typeof setTimeout> | undefined
let running = false
let snapshot = { active, finished }
const subs = new Set<() => void>()
const emit = () => {
  snapshot = { active, finished }
  subs.forEach((f) => f())
}

export async function pollJobs() {
  clearTimeout(timer)
  running = true
  try {
    active = await api<Job[]>("GET", "/jobs?active=1&limit=20")
  } catch {
    if (running) timer = setTimeout(pollJobs, 10_000)
    return
  }
  for (const [id, onDone] of followed) {
    if (active.some((j) => j.id === id)) continue
    followed.delete(id)
    try {
      const v = await api<JobResult>("GET", `/jobs/${id}`)
      finished = [v.job, ...finished].slice(0, 5)
      if (onDone) await onDone(v)
    } catch (e) {
      showError(e)
    }
  }
  emit()
  if (running) timer = setTimeout(pollJobs, active.length || followed.size ? 2000 : 30_000)
}

export function stopJobs() {
  running = false
  clearTimeout(timer)
  followed.clear()
  active = []
  finished = []
  emit()
}

export function followJob(id: string, onDone?: OnDone) {
  followed.set(id, onDone)
  pollJobs()
}

// startJob runs a request that answers 202 {job_id}, follows the job and
// refreshes the sites when it ends (showing its error if it failed).
export async function startJob<R extends { job_id: string }>(
  method: "POST" | "PUT" | "DELETE",
  path: string,
  body?: unknown,
  onDone?: (v: JobResult, res: R) => void | Promise<void>
) {
  const res = await api<R>(method, path, body)
  followJob(res.job_id, async (v) => {
    if (v.job.status === "failed") showError(new Error(`${JOB_NAMES[v.job.kind] || v.job.kind} failed: ${v.job.error}`))
    if (onDone) await onDone(v, res)
    await invalidate("/sites")
  })
  return res
}

export function dismissJob(id: string) {
  finished = finished.filter((j) => j.id !== id)
  emit()
}

export const useJobs = () => useSyncExternalStore((f) => (subs.add(f), () => void subs.delete(f)), () => snapshot)

// siteLabel: a site's domain from the loaded list, else its ID.
export function siteLabel(id: string) {
  const sites = queryClient.getQueryData<Site[]>(["/sites"])
  return sites?.find((s) => s.id === id)?.primary_domain ?? id
}
