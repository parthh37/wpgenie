import { useState, type FormEvent, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { ActivityIcon, HardDriveIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ask } from "@/components/app/confirm"
import { LoadError } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { showError } from "@/components/app/toaster"
import type { SectionProps } from "@/features/sites/sections"
import { api } from "@/lib/api"
import { fmtBytes, fmtNum, fmtTime } from "@/lib/format"
import { startJob } from "@/lib/jobs"
import { queryClient } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { Note, Steps, Summary, Warning } from "./speed/shared"

// Uploads offload: the media library copied to S3-compatible storage
// (GET/PUT /sites/{id}/offload). The secret key is write-only: the API
// never returns it, and the access key ID comes back masked.

interface OffloadStatus {
  enabled: boolean
  endpoint: string
  region: string
  bucket: string
  prefix: string
  access_key_id: string
  secret_set: boolean
  public_url: string
  acl: string
  local_days: number
  syncing: boolean
  last_incremental: string | null
  last_full: string | null
  last_attempt: string | null
  last_cleanup: string | null
  last_error: string
  failures: number
  last_objects: number
  last_bytes: number
  total_objects: number
  total_bytes: number
  total_deleted: number
  // Uploads that only exist in the bucket now (local_days).
  removed_local: number
  removed_bytes: number
  pending_deletes: number
  // A staging site without offload of its own serves missing uploads from
  // its live site's storage (read-only).
  parent_public_url: string
}

interface Fields {
  endpoint: string
  region: string
  bucket: string
  prefix: string
  access_key_id: string
  secret_key: string
  public_url: string
  acl: string
  local_days: string
}

const EMPTY: Fields = { endpoint: "", region: "", bucket: "", prefix: "", access_key_id: "", secret_key: "", public_url: "", acl: "", local_days: "0" }

const when = (t: string | null) => (t ? fmtTime(t) : "never")

export default function UploadsSection({ site }: SectionProps) {
  const path = `/sites/${site.id}/offload`
  // While a copy runs, it's followed until it ends.
  const q = useQuery<OffloadStatus>({
    queryKey: [path],
    queryFn: () => api<OffloadStatus>("GET", path),
    refetchInterval: (x) => (x.state.data?.syncing ? 5000 : false),
  })
  const st = q.data
  const show = (next: OffloadStatus) => queryClient.setQueryData([path], next)
  const refresh = async () => {
    await q.refetch()
  }

  return (
    <div>
      {st && <OffloadSummary st={st} />}
      <OffloadForm site={site} st={st} show={show} refresh={refresh} />
      {q.isLoading ? (
        <Skeleton className="h-40 rounded-2xl" />
      ) : q.error && !st ? (
        <LoadError error={q.error} retry={() => q.refetch()} />
      ) : st ? (
        <OffloadStatusView st={st} />
      ) : null}
    </div>
  )
}

function OffloadSummary({ st }: { st: OffloadStatus }) {
  if (!st.enabled) {
    return (
      <Summary>
        <StatusPill status="off" tone="neutral">
          Off
        </StatusPill>
        {st.parent_public_url && <span>missing uploads come from the live site's bucket</span>}
      </Summary>
    )
  }
  return (
    <Summary>
      <StatusPill status={st.last_error ? "failed" : "active"}>{st.last_error ? "Failing" : "On"}</StatusPill>
      <code className="text-xs">{`${st.bucket}/${st.prefix}`}</code>
      {st.local_days > 0 && <span>· local copies {st.local_days} days</span>}
    </Summary>
  )
}

function OffloadStatusView({ st }: { st: OffloadStatus }) {
  if (!st.enabled) {
    if (!st.parent_public_url) return null
    return (
      <Section icon={ActivityIcon} tint="green" title="Status">
        <Note>
          This staging site serves uploads it doesn't have from its live site's bucket (read-only): <code>{st.parent_public_url}</code>
        </Note>
      </Section>
    )
  }
  return (
    <Section icon={ActivityIcon} tint="green" title="Status">
      <div className="flex flex-col gap-3">
        <SimpleTable
          headers={["", "Last", ""]}
          rows={[
            ["Copy of new files", when(st.last_incremental), st.syncing ? "running now" : ""],
            ["Full comparison", when(st.last_full), "nightly"],
            ["Last copy", st.last_objects ? `${fmtNum(st.last_objects)} files, ${fmtBytes(st.last_bytes)}` : "nothing new", ""],
            [
              "In total",
              `${fmtNum(st.total_objects)} files, ${fmtBytes(st.total_bytes)} copied; ${fmtNum(st.total_deleted)} deleted`,
              st.pending_deletes ? `${fmtNum(st.pending_deletes)} deletes waiting` : "",
            ],
            [
              "Only in the bucket",
              st.removed_local ? `${fmtNum(st.removed_local)} uploads, ${fmtBytes(st.removed_bytes)}` : "none",
              st.last_cleanup ? `local copies last removed ${fmtTime(st.last_cleanup)}` : "",
            ],
          ]}
          className="[&_td]:whitespace-normal"
        />
        <Note>
          Uploads missing on this server are served from <code>{st.public_url}</code>.
        </Note>
        {st.last_error && (
          <Warning tone="bad">
            Last attempt {when(st.last_attempt)} failed (retried with backoff): {st.last_error}
          </Warning>
        )}
      </div>
    </Section>
  )
}

function OffloadForm({
  site,
  st,
  show,
  refresh,
}: {
  site: Site
  st: OffloadStatus | undefined
  show: (st: OffloadStatus) => void
  refresh: () => Promise<void>
}) {
  const s = useSession()
  const active = site.status === "active"
  const [f, setF] = useState<Fields>(EMPTY)
  const [saving, setSaving] = useState(false)
  const [syncing, setSyncing] = useState(false)
  const set = (k: keyof Fields) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF((x) => ({ ...x, [k]: e.target.value }))

  // A new status (loaded, saved): when on, its settings in the form.
  const [seen, setSeen] = useState(st)
  if (seen !== st) {
    setSeen(st)
    if (st?.enabled) {
      setF((x) => ({
        ...x,
        endpoint: st.endpoint || "",
        region: st.region || "",
        bucket: st.bucket || "",
        prefix: st.prefix || "",
        public_url: st.public_url || "",
        acl: st.acl || "",
        local_days: String(st.local_days),
      }))
    }
  }

  const on = !!st?.enabled
  const removed = st?.removed_local || 0
  const locked = !active || !s.canChangeSite(site)

  const save = async (e: FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      show(
        await api<OffloadStatus>("PUT", `/sites/${site.id}/offload`, {
          enabled: true,
          local_days: Number(f.local_days) || 0,
          endpoint: f.endpoint.trim(),
          region: f.region.trim(),
          bucket: f.bucket.trim(),
          prefix: f.prefix.trim(),
          access_key_id: f.access_key_id.trim(),
          secret_key: f.secret_key.trim(),
          public_url: f.public_url.trim(),
          acl: f.acl.trim(),
        })
      )
      setF((x) => ({ ...x, secret_key: "", access_key_id: "" }))
    } catch (err) {
      showError(err)
    }
    setSaving(false)
  }

  const sync = async () => {
    setSyncing(true)
    try {
      await startJob("POST", `/sites/${site.id}/offload/sync`, undefined, refresh)
    } catch (err) {
      showError(err)
    }
    setSyncing(false)
  }

  const download = async () => {
    if (!(await ask(`Copy the ${removed} uploads that only exist in the bucket back to this server?`, { ok: "Copy back" }))) return
    try {
      await startJob("POST", `/sites/${site.id}/offload/download`, undefined, refresh)
    } catch (err) {
      showError(err)
    }
  }

  const off = async () => {
    let force = false
    if (removed) {
      if (
        !(await ask(
          `${removed} uploads only exist in the bucket. Turning offload off makes them unavailable on the site (use "Copy back" first to keep them). Turn it off anyway?`,
          { ok: "Turn off", danger: true }
        ))
      )
        return
      force = true
    } else if (
      !(await ask("Stop copying uploads to the bucket? The objects already there stay (delete them from the bucket if they're no longer needed)."))
    )
      return
    try {
      show(await api<OffloadStatus>("PUT", `/sites/${site.id}/offload`, { enabled: false, force }))
    } catch (err) {
      showError(err)
    }
  }

  const field = (k: keyof Fields, label: ReactNode, input: React.ComponentProps<typeof Input> = {}) => (
    <Field>
      <FieldLabel htmlFor={`off-${k}-${site.id}`}>{label}</FieldLabel>
      <Input id={`off-${k}-${site.id}`} value={f[k]} disabled={locked} onChange={set(k)} {...input} />
    </Field>
  )
  const hint = (text: string) => <span className="font-normal text-muted-foreground">{text}</span>

  return (
    <Section
      icon={HardDriveIcon}
      tint="teal"
      title="Storage"
      description="Copies the media library to S3-compatible storage (AWS S3, Cloudflare R2, Backblaze B2, MinIO, …): new uploads within a minute, everything nightly; deleted uploads are deleted there too. Uploads missing on this server are served from the storage's public URL, so local copies can be removed after a while to save disk."
    >
      <form onSubmit={save} className="flex flex-col gap-5">
        <Steps>
          <li>Create a bucket and a key that may write, read and delete objects under the prefix (and nothing else).</li>
          <li>
            Make the prefix publicly readable (a bucket policy allowing <code>s3:GetObject</code>, not listing), or put a CDN in front of it.
          </li>
          <li>Saving writes a test object, fetches it through the public URL and deletes it; settings that don't work aren't saved.</li>
        </Steps>

        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {field("endpoint", "Endpoint", { placeholder: "https://s3.eu-central-1.amazonaws.com", inputMode: "url" })}
          {field("region", <>Region {hint("(if needed)")}</>, { placeholder: "eu-central-1" })}
          {field("bucket", "Bucket")}
          {field("prefix", "Prefix", { placeholder: `${site.id}/uploads/` })}
          {field("access_key_id", "Access key ID", {
            autoComplete: "off",
            placeholder: on && st ? `${st.access_key_id} (unchanged if empty)` : undefined,
          })}
          {field("secret_key", "Secret access key", {
            type: "password",
            autoComplete: "off",
            placeholder: on && st ? (st.secret_set ? "unchanged if empty" : "secret access key") : undefined,
          })}
          {field("public_url", <>Public URL {hint("(the prefix, or a CDN in front of it)")}</>, {
            placeholder: "https://media.example.com/uploads",
            inputMode: "url",
          })}
          <Field>
            <FieldLabel htmlFor={`off-acl-${site.id}`}>Object ACL</FieldLabel>
            <NativeSelect id={`off-acl-${site.id}`} value={f.acl} disabled={locked} onChange={set("acl")} className="w-full">
              <NativeSelectOption value="">None (a bucket policy makes them public)</NativeSelectOption>
              <NativeSelectOption value="public-read">public-read</NativeSelectOption>
            </NativeSelect>
          </Field>
          {field("local_days", <>Remove local copies after {hint("(days, 0 = keep)")}</>, { type: "number", min: 0, max: 3650, inputMode: "numeric" })}
        </div>

        {s.canChangeSite(site) && (
          <div className="flex flex-wrap gap-2">
            <Button type="submit" disabled={!active || saving}>
              {saving ? "Checking…" : "Save"}
            </Button>
            {on && (
              <Button type="button" variant="tinted" disabled={!active || syncing} onClick={sync}>
                Sync now
              </Button>
            )}
            {on && removed > 0 && (
              <Button type="button" variant="tinted" disabled={!active} onClick={download}>
                Copy back
              </Button>
            )}
            {on && (
              <Button type="button" variant="destructive" disabled={!active} onClick={off}>
                Turn off
              </Button>
            )}
          </div>
        )}

        <Note>
          Local copies are only removed once the bucket holds an identical copy that the public URL serves. Offloaded images whose local copy was removed
          are served in their original format (AVIF/WebP negotiation needs the file on disk). Deleting the site leaves the bucket's objects in place.
        </Note>
      </form>
    </Section>
  )
}
