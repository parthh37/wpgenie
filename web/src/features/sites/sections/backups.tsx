import { useState } from "react"
import { ArchiveIcon, CalendarClockIcon, CopyPlusIcon, DownloadIcon, EllipsisIcon, HardDriveIcon, HistoryIcon, Trash2Icon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, Banner, ChoiceCard, FormDialog, LoadError } from "@/components/app/blocks"
import { ask, askText } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtBytes, fmtTime } from "@/lib/format"
import { startJob } from "@/lib/jobs"
import { invalidate, useApi } from "@/lib/query"
import { navigate, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "@/features/sites/sections"
import { useSiteJob } from "./data/jobs"
import type { BackupDestination, BackupInfo, BackupPolicy, SiteBackups } from "./data/types"

// A site's backups: where and how often they're taken and how many are
// kept, a backup now, and every backup there is, to restore (all of it,
// the files or the database), download or delete.

const INTERVALS: Array<[number, string]> = [
  [0, "Manual only"],
  [1, "Every hour"],
  [2, "Every 2 hours"],
  [4, "Every 4 hours"],
  [6, "Every 6 hours"],
  [12, "Every 12 hours"],
  [24, "Daily"],
  [48, "Every 2 days"],
  [168, "Weekly"],
]

const DEFAULT_POLICY: BackupPolicy = { site_id: "", repo_id: "", interval_hours: 24, keep_last: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 6 }

const KIND_LABEL: Record<string, string> = { scheduled: "Scheduled", manual: "Manual", safety: "Safety" }

export default function BackupsSection({ site }: SectionProps) {
  const info = useApi<SiteBackups>(`/sites/${site.id}/backups`)
  const repos = useApi<BackupDestination[]>(`/sites/${site.id}/backups/destinations`)

  if (info.error || repos.error) {
    return (
      <LoadError
        error={info.error || repos.error}
        retry={() => {
          info.refetch()
          repos.refetch()
        }}
      />
    )
  }
  if (!info.data || !repos.data) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-56 rounded-2xl" />
        <Skeleton className="h-72 rounded-2xl" />
      </div>
    )
  }

  const data = info.data
  const list = repos.data
  const repoName = (id: string) => list.find((r) => r.id === id)?.name ?? id
  const p = data.policy
  const summary = p
    ? `${INTERVALS.find(([v]) => v === p.interval_hours)?.[1] || p.interval_hours + "h"} to ${repoName(p.repo_id)}`
    : "Not scheduled"

  return (
    <div className="flex flex-col">
      {p?.last_error && (
        <Banner tone="bad" icon={CalendarClockIcon} title="Last scheduled backup failed">
          {fmtTime(p.last_attempt_at)}: {p.last_error}
        </Banner>
      )}
      <Schedule
        // A fresh form whenever the saved schedule changes (not when a
        // backup ran: unsaved edits stay).
        key={p ? [p.repo_id, p.interval_hours, p.keep_last, p.keep_daily, p.keep_weekly, p.keep_monthly].join() : "none"}
        site={site}
        policy={p}
        repos={list}
        summary={summary}
      />
      <BackupList site={site} data={data} repoName={repoName} />
    </div>
  )
}

// ---- Schedule and retention ----

function Schedule({ site, policy, repos, summary }: { site: Site; policy: BackupPolicy | null; repos: BackupDestination[]; summary: string }) {
  const s = useSession()
  const p = policy ?? DEFAULT_POLICY
  const [repo, setRepo] = useState(policy ? p.repo_id : "")
  const [every, setEvery] = useState(p.interval_hours)
  const [keep, setKeep] = useState({
    keep_last: String(p.keep_last),
    keep_daily: String(p.keep_daily),
    keep_weekly: String(p.keep_weekly),
    keep_monthly: String(p.keep_monthly),
  })
  const ro = !s.canChange
  const id = (k: string) => `bk-${k}-${site.id}`

  const save = async () => {
    try {
      await api("PUT", `/sites/${site.id}/backups/policy`, {
        repo_id: repo,
        interval_hours: every,
        keep_last: Number(keep.keep_last),
        keep_daily: Number(keep.keep_daily),
        keep_weekly: Number(keep.keep_weekly),
        keep_monthly: Number(keep.keep_monthly),
      })
      notify(repo ? "Backup schedule saved" : "Scheduled backups turned off")
      await invalidate(`/sites/${site.id}/backups`)
    } catch (e) {
      showError(e)
    }
  }

  const keepField = (k: keyof typeof keep, label: string) => (
    <Field className="w-28">
      <FieldLabel htmlFor={id(k)}>{label}</FieldLabel>
      <Input
        id={id(k)}
        type="number"
        min={0}
        max={1000}
        inputMode="numeric"
        value={keep[k]}
        disabled={ro}
        onChange={(e) => setKeep({ ...keep, [k]: e.target.value })}
      />
    </Field>
  )

  return (
    <Section
      icon={CalendarClockIcon}
      tint="orange"
      title="Schedule"
      description={<>Now: {summary}.</>}
      action={
        s.canChange && (
          <ActionButton run={save} variant="default">
            Save schedule
          </ActionButton>
        )
      }
    >
      <div className="flex flex-col gap-5">
        <div className="flex flex-wrap gap-4">
          <Field className="w-auto min-w-56">
            <FieldLabel htmlFor={id("repo")}>Destination</FieldLabel>
            <NativeSelect id={id("repo")} className="w-full" value={repo} disabled={ro} onChange={(e) => setRepo(e.target.value)}>
              <NativeSelectOption value="">No scheduled backups</NativeSelectOption>
              {repos.map((r) => (
                <NativeSelectOption key={r.id} value={r.id}>
                  {r.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </Field>
          <Field className="w-auto min-w-48">
            <FieldLabel htmlFor={id("every")}>Schedule</FieldLabel>
            <NativeSelect id={id("every")} className="w-full" value={String(every)} disabled={ro} onChange={(e) => setEvery(Number(e.target.value))}>
              {INTERVALS.map(([v, l]) => (
                <NativeSelectOption key={v} value={String(v)}>
                  {l}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </Field>
        </div>
        <fieldset className="flex flex-col gap-3">
          <legend className="mb-3 text-sm font-medium">Keep</legend>
          <div className="flex flex-wrap gap-4">
            {keepField("keep_last", "Last")}
            {keepField("keep_daily", "Daily")}
            {keepField("keep_weekly", "Weekly")}
            {keepField("keep_monthly", "Monthly")}
          </div>
        </fieldset>
        <FieldDescription>
          Daily and weekly backups run in the nightly maintenance window. Scheduled backups follow the "keep" rules; manual ones stay until deleted;
          safety backups (taken before restores and pushes) are kept 7 days.
        </FieldDescription>
      </div>
    </Section>
  )
}

// ---- The backups there are ----

function BackupList({ site, data, repoName }: { site: Site; data: SiteBackups; repoName: (id: string) => string }) {
  const s = useSession()
  const backup = useSiteJob(site.id, ["backup"])
  const errs = Object.entries(data.errors || {})
  const now = () => backup.run("POST", `/sites/${site.id}/backups`, undefined, () => invalidate(`/sites/${site.id}/backups`))

  return (
    <Section
      icon={ArchiveIcon}
      tint="orange"
      title="Backups"
      action={
        s.canChange && (
          <Button onClick={now} disabled={backup.running}>
            <HardDriveIcon data-icon="inline-start" />
            {backup.running ? "Backing up…" : "Back up now"}
          </Button>
        )
      }
    >
      {errs.map(([r, e]) => (
        <p key={r} role="alert" className="mb-3 text-sm text-danger">
          {repoName(r)} unreadable: {e}
        </p>
      ))}
      <BTable
        caption={`Backups of ${site.primary_domain}`}
        cols={[
          "Time",
          "Kind",
          "Where",
          { label: "Size", num: true },
          { label: <span title="New data this backup stored (compressed, deduplicated)">New data</span>, num: true },
          { label: <span className="sr-only">Actions</span> },
        ]}
        rows={data.backups.map((b) => ({
          key: b.repo_id + "/" + b.id,
          cells: [
            <span className="whitespace-nowrap">{fmtTime(b.time)}</span>,
            <StatusPill status={b.kind || "manual"}>{KIND_LABEL[b.kind || "manual"] ?? b.kind}</StatusPill>,
            repoName(b.repo_id),
            fmtBytes(b.size),
            <span title="New data this backup stored (compressed, deduplicated)">{fmtBytes(b.added)}</span>,
            <BackupActions site={site} b={b} />,
          ],
        }))}
        empty={
          <p className="py-2 text-sm text-muted-foreground">
            No backups yet.{s.canChange && " Back up now, or choose a destination and a schedule above."}
          </p>
        }
      />
    </Section>
  )
}

type What = "both" | "files" | "db"
const WHAT: Array<[What, string, string]> = [
  ["both", "Files + database", "The whole site as it was."],
  ["files", "Files only", "WordPress, plugins, themes and uploads; the database stays as it is."],
  ["db", "Database only", "Posts, pages, settings, users and orders; the files stay as they are."],
]

function BackupActions({ site, b }: { site: Site; b: BackupInfo }) {
  const s = useSession()
  const [restoring, setRestoring] = useState(false)
  const [what, setWhat] = useState<What>("both")
  const restore = useSiteJob(site.id, ["restore"])
  if (!s.canChange) return null
  const base = `/sites/${site.id}/backups/${encodeURIComponent(b.repo_id)}/${b.id}`

  const restoreAsNew = async () => {
    const domain = await askText(`A new site from the backup of ${b.domain || site.primary_domain} (${fmtTime(b.time)}). Its DNS must point here.`, {
      title: "Restore as a new site",
      label: "Domain for the new site",
      placeholder: "example.com",
      ok: "Restore",
    })
    if (!domain) return
    try {
      const res = await startJob<{ job_id: string; site?: Site }>("POST", "/backups/restore-new", { repo_id: b.repo_id, backup_id: b.id, domain })
      notify(`Restoring as ${res.site?.primary_domain ?? domain}: progress is in the jobs panel`)
      await invalidate("/sites")
      if (res.site?.id) navigate(sitePath(res.site.id))
    } catch (e) {
      showError(e)
    }
  }

  const del = async () => {
    if (!(await ask(`Delete the backup of ${fmtTime(b.time)}? This can't be undone.`))) return
    try {
      await api("DELETE", base)
      notify("Backup deleted")
      await invalidate(`/sites/${site.id}/backups`)
    } catch (e) {
      showError(e)
    }
  }

  return (
    <div className="flex items-center justify-end gap-1.5">
      <Button
        variant="tinted"
        size="sm"
        disabled={restore.running}
        onClick={() => {
          setWhat("both")
          setRestoring(true)
        }}
      >
        <HistoryIcon data-icon="inline-start" />
        Restore
      </Button>
      <DropdownMenu>
        <DropdownMenuTrigger render={<Button variant="ghost" size="icon-sm" aria-label={`More for the backup of ${fmtTime(b.time)}`} />}>
          <EllipsisIcon />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-auto">
          <DropdownMenuItem render={<a href={`/api/v1${base}/download`} download />}>
            <DownloadIcon />
            Download
          </DropdownMenuItem>
          {s.isAdmin && (
            <>
              <DropdownMenuItem onClick={restoreAsNew}>
                <CopyPlusIcon />
                Restore as a new site…
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onClick={del}>
                <Trash2Icon />
                Delete
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>

      <FormDialog
        open={restoring}
        onOpenChange={setRestoring}
        danger
        title={`Restore ${site.primary_domain} to ${fmtTime(b.time)}?`}
        intro="The site as it is now is backed up first (kept 7 days), so this can be undone."
        ok={`Restore ${WHAT.find(([k]) => k === what)![1].toLowerCase()}`}
        onSubmit={() =>
          restore.start(
            "POST",
            `/sites/${site.id}/backups/restore`,
            { repo_id: b.repo_id, backup_id: b.id, files: what !== "db", database: what !== "files" },
            () => invalidate(`/sites/${site.id}/backups`)
          )
        }
      >
        <div role="radiogroup" aria-label="What to restore" className="grid gap-2">
          {WHAT.map(([k, title, sub]) => (
            <ChoiceCard key={k} name={`restore-what-${b.id}`} value={k} title={title} checked={what === k} onChange={() => setWhat(k)}>
              {sub}
            </ChoiceCard>
          ))}
        </div>
      </FormDialog>
    </div>
  )
}
