import { useRef, useState, type FormEvent } from "react"
import { CheckIcon, ClockIcon, CloudIcon, BoxIcon, HardDriveIcon, TriangleAlertIcon, ZapIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { ChoiceCard } from "@/components/app/blocks"
import { IconTile } from "@/components/app/icon-tile"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtBytes } from "@/lib/format"
import { LOG_PROVIDERS, LOG_RETENTION, logTestHint, providerOf, saveLogSettings, type Destination, type LogSettings } from "./data"

// Where logs go (an S3-compatible bucket) and how long they're kept.

// ---- Destination ----

// The endpoint, region and path-style follow the provider: a value filled
// in for another provider is replaced when the provider changes; one typed
// for this one is left alone.
interface DestState {
  provider: string
  endpoint: string
  region: string
  path_style: boolean
  autoEndpoint: boolean
  autoRegion: boolean
}

const PRESET_HOST = /(amazonaws\.com|r2\.cloudflarestorage\.com|backblazeb2\.com|wasabisys\.com|digitaloceanspaces\.com)$/i

function withProvider(cur: DestState, provider: string): DestState {
  const p = providerOf(provider)
  let { endpoint, region, autoEndpoint, autoRegion } = cur
  let host = ""
  try {
    host = new URL(endpoint).hostname
  } catch {
    /* empty or not a URL yet */
  }
  if (PRESET_HOST.test(host)) autoEndpoint = autoRegion = true
  if (!region || autoRegion) {
    region = p.region
    autoRegion = true
  }
  if (!endpoint || autoEndpoint) {
    endpoint = p.endpoint.includes("ACCOUNT_ID") || p.endpoint.includes("example.com") ? "" : p.endpoint.replace("{region}", region)
    autoEndpoint = true
  }
  return { provider, endpoint, region, autoEndpoint, autoRegion, path_style: p.pathStyle }
}

function initial(d: Destination): DestState {
  const cur: DestState = {
    provider: d.provider || "aws",
    endpoint: d.endpoint || "",
    region: d.region || "",
    path_style: !!d.path_style,
    autoEndpoint: false,
    autoRegion: false,
  }
  // A first run fills in the preset's endpoint and region.
  return d.endpoint ? cur : withProvider(cur, cur.provider)
}

type TestResult = { kind: "busy" } | { kind: "ok" } | { kind: "bad"; error: string; hint?: string }

export function DestinationCard({ set }: { set: LogSettings }) {
  const d = set.destination
  const [dest, setDest] = useState(() => initial(d))
  const [test, setTest] = useState<TestResult | null>(null)
  const [busy, setBusy] = useState(false)
  const formRef = useRef<HTMLFormElement>(null)
  const p = providerOf(dest.provider)

  const values = (): Destination => {
    const f = new FormData(formRef.current!)
    const s = (k: string) => String(f.get(k) ?? "").trim()
    return {
      provider: dest.provider,
      endpoint: dest.endpoint.trim(),
      region: dest.region.trim(),
      bucket: s("bucket"),
      prefix: s("prefix"),
      access_key_id: s("access_key_id"),
      secret_key: String(f.get("secret_key") ?? ""),
      path_style: dest.path_style,
    }
  }

  async function runTest() {
    setTest({ kind: "busy" })
    try {
      const r = await api<{ ok: boolean; error?: string }>("POST", "/logs/test", values())
      setTest(r.ok ? { kind: "ok" } : { kind: "bad", error: r.error || "", hint: logTestHint(r.error || "") })
    } catch (e) {
      setTest({ kind: "bad", error: e instanceof Error ? e.message : String(e) })
    }
  }

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (busy) return
    const submitter = (e.nativeEvent as SubmitEvent).submitter as HTMLButtonElement | null
    const on = submitter?.value === "on"
    setBusy(true)
    try {
      await saveLogSettings(on ? { destination: values(), enabled: true } : { destination: values() })
      notify(
        on ? "Log shipping is on. The shipper starts in a moment." : set.enabled ? "Destination saved." : "Destination saved. Turn shipping on when you're ready."
      )
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  const example = `${d.prefix || ""}${set.server}/access/${new Date().toISOString().slice(0, 10).replaceAll("-", "/")}/14-….log.gz`

  return (
    <Section
      icon={CloudIcon}
      tint="blue"
      title="Where logs go"
      description={
        <>
          Any S3-compatible storage. Logs are grouped by server, kind and day: <code className="[overflow-wrap:anywhere]">{example}</code>. Test the
          connection before saving: it writes a small file and deletes it again.
        </>
      }
    >
      <form id="logs-dest" ref={formRef} onSubmit={submit} autoComplete="off" className="flex scroll-mt-6 flex-col gap-5">
        <fieldset>
          <legend className="mb-2 text-sm font-medium">Storage provider</legend>
          <div role="radiogroup" aria-label="Storage provider" className="grid grid-cols-[repeat(auto-fill,minmax(10.5rem,1fr))] gap-2">
            {Object.entries(LOG_PROVIDERS).map(([id, x]) => (
              <ChoiceCard
                key={id}
                name="provider"
                value={id}
                title={x.name}
                checked={dest.provider === id}
                onChange={(v) => setDest((cur) => withProvider(cur, v))}
              />
            ))}
          </div>
          <p className="mt-2 max-w-[78ch] text-sm text-muted-foreground">{p.help}</p>
        </fieldset>

        <FieldGroup className="grid gap-4 sm:grid-cols-2">
          <Field>
            <FieldLabel htmlFor="logs-endpoint">Endpoint</FieldLabel>
            <Input
              id="logs-endpoint"
              inputMode="url"
              spellCheck={false}
              placeholder={p.endpoint.replace("{region}", p.region || "region")}
              value={dest.endpoint}
              onChange={(e) => {
                const endpoint = e.target.value
                setDest((cur) => ({ ...cur, endpoint, autoEndpoint: false }))
              }}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="logs-region">
              Region <span className="font-normal text-muted-foreground">(if the storage has one)</span>
            </FieldLabel>
            <Input
              id="logs-region"
              spellCheck={false}
              placeholder={p.region || "none"}
              value={dest.region}
              onChange={(e) => {
                const region = e.target.value
                setDest((cur) => {
                  const pr = LOG_PROVIDERS[cur.provider]
                  const endpoint = pr && cur.autoEndpoint && pr.endpoint.includes("{region}") ? pr.endpoint.replace("{region}", region.trim()) : cur.endpoint
                  return { ...cur, region, endpoint, autoRegion: false }
                })
              }}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="logs-bucket">Bucket</FieldLabel>
            <Input id="logs-bucket" name="bucket" spellCheck={false} placeholder="my-logs" defaultValue={d.bucket || ""} />
          </Field>
          <Field>
            <FieldLabel htmlFor="logs-prefix">
              Folder in the bucket <span className="font-normal text-muted-foreground">(optional)</span>
            </FieldLabel>
            <Input id="logs-prefix" name="prefix" spellCheck={false} placeholder="logs/" defaultValue={d.prefix || ""} />
          </Field>
          <Field>
            <FieldLabel htmlFor="logs-key-id">Access key ID</FieldLabel>
            <Input id="logs-key-id" name="access_key_id" spellCheck={false} defaultValue={d.access_key_id || ""} />
          </Field>
          <Field>
            <FieldLabel htmlFor="logs-secret">Secret access key</FieldLabel>
            <Input
              id="logs-secret"
              name="secret_key"
              type="password"
              autoComplete="new-password"
              placeholder={d.secret_key_set ? "saved (unchanged if empty)" : "the key's secret"}
            />
          </Field>
        </FieldGroup>

        <details className="group rounded-xl bg-muted/50 px-3.5 py-2.5">
          <summary className="cursor-pointer text-sm font-medium select-none">Advanced</summary>
          <Label className="mt-3 flex items-start gap-2.5 font-normal">
            <Checkbox className="mt-0.5" checked={dest.path_style} onCheckedChange={(c) => setDest((cur) => ({ ...cur, path_style: !!c }))} />
            <span className="text-sm">Path-style addresses (the bucket in the URL's path: MinIO and most self-hosted storage)</span>
          </Label>
        </details>

        <div aria-live="polite">
          {test?.kind === "busy" && <p className="text-sm text-muted-foreground">Writing a test file to the bucket…</p>}
          {test?.kind === "ok" && (
            <p className="flex items-start gap-2.5 rounded-xl bg-success-fill/12 p-3 text-sm text-success">
              <CheckIcon className="mt-0.5 size-4 shrink-0" />
              It works: WPGenie can write to this bucket and delete from it.
            </p>
          )}
          {test?.kind === "bad" && (
            <div role="alert" className="flex items-start gap-2.5 rounded-xl bg-danger-fill/10 p-3 text-sm ring-1 ring-danger/30">
              <TriangleAlertIcon className="mt-0.5 size-4 shrink-0 text-danger" />
              <div className="min-w-0">
                {test.hint && <strong className="font-semibold">The storage said no.</strong>}
                <p className="[overflow-wrap:anywhere]">{test.error}</p>
                {test.hint && <p className="mt-1 text-muted-foreground">{test.hint}</p>}
              </div>
            </div>
          )}
        </div>

        <div className="flex flex-wrap gap-2">
          <Button type="button" variant="tinted" onClick={runTest} disabled={test?.kind === "busy"}>
            <ZapIcon data-icon="inline-start" />
            Test connection
          </Button>
          {/* While shipping is off, saving can also turn it on (the usual first run). */}
          <Button type="submit" variant={set.enabled ? "default" : "tinted"} disabled={busy}>
            Save destination
          </Button>
          {!set.enabled && (
            <Button type="submit" value="on" disabled={busy}>
              Save and turn on shipping
            </Button>
          )}
        </div>
      </form>
    </Section>
  )
}

// ---- Retention ----

export function RetentionCard({ set }: { set: LogSettings }) {
  const [keepText, setKeep] = useState(String(set.local_access_logs))
  const keep = Number(keepText) || 0
  const [busy, setBusy] = useState(false)
  const custom = !LOG_RETENTION.some(([v]) => v === set.archive_retention_days)

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (busy) return
    const f = new FormData(e.currentTarget)
    const n = (k: string) => Number(f.get(k))
    setBusy(true)
    try {
      await saveLogSettings({
        archive_retention_days: n("archive_retention_days"),
        local_access_logs: n("local_access_logs"),
        container_log_mb: n("container_log_mb"),
        compression: String(f.get("compression")),
        batch_max_seconds: n("batch_max_seconds"),
        batch_max_mb: n("batch_max_mb"),
        spool_cap_mb: n("spool_cap_mb"),
      })
      notify("Retention saved.")
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  const row = "grid items-center gap-x-4 gap-y-1.5 rounded-xl bg-muted/50 p-3.5 sm:grid-cols-[auto_minmax(220px,340px)_1fr]"

  return (
    <Section icon={ClockIcon} tint="orange" title="How long logs are kept">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="grid gap-2.5">
          <div className={row}>
            <IconTile icon={CloudIcon} tint="blue" className="max-sm:hidden" />
            <div className="grid gap-1.5">
              <Label htmlFor="logs-retention">Keep logs in the bucket for</Label>
              <NativeSelect id="logs-retention" name="archive_retention_days" defaultValue={String(set.archive_retention_days)} className="w-full">
                {LOG_RETENTION.map(([v, l]) => (
                  <NativeSelectOption key={v} value={String(v)}>
                    {l}
                  </NativeSelectOption>
                ))}
                {custom && <NativeSelectOption value={String(set.archive_retention_days)}>{set.archive_retention_days} days</NativeSelectOption>}
              </NativeSelect>
            </div>
            <p className="text-sm text-muted-foreground">Older ones are deleted from the bucket every night. "Forever" never deletes anything.</p>
          </div>
          <div className={row}>
            <IconTile icon={HardDriveIcon} tint="gray" className="max-sm:hidden" />
            <div className="grid gap-1.5">
              <Label htmlFor="logs-local">Old access log files kept on this server</Label>
              <Input
                id="logs-local"
                name="local_access_logs"
                type="number"
                min={1}
                max={10}
                required
                value={keepText}
                onChange={(e) => setKeep(e.target.value)}
              />
            </div>
            <p className="text-sm text-muted-foreground">
              Keep {keep} old access log file{keep === 1 ? "" : "s"} (up to 100 MB each, about{" "}
              {fmtBytes(keep * 100 * 1024 * 1024)}) on this server while logs are shipped; 10 are kept when shipping is
              off.
            </p>
          </div>
          <div className={row}>
            <IconTile icon={BoxIcon} tint="brown" className="max-sm:hidden" />
            <div className="grid gap-1.5">
              <Label htmlFor="logs-container">Each container's own log, at most (MB)</Label>
              <Input id="logs-container" name="container_log_mb" type="number" min={1} max={1024} required defaultValue={set.container_log_mb} />
            </div>
            <p className="text-sm text-muted-foreground">Applies to containers started from now on, while logs are shipped (two files of this size).</p>
          </div>
        </div>

        <details className="rounded-xl bg-muted/50 px-3.5 py-2.5">
          <summary className="cursor-pointer text-sm font-medium select-none">Advanced</summary>
          <FieldGroup className="mt-3 grid gap-4 sm:grid-cols-2">
            <Field>
              <FieldLabel htmlFor="logs-compression">Compression</FieldLabel>
              <NativeSelect id="logs-compression" name="compression" defaultValue={set.compression} className="w-full">
                <NativeSelectOption value="gzip">gzip (readable everywhere, and here)</NativeSelectOption>
                <NativeSelectOption value="zstd">zstd (smaller, download to read)</NativeSelectOption>
              </NativeSelect>
            </Field>
            <Field>
              <FieldLabel htmlFor="logs-batch-s">Write a file every (seconds)</FieldLabel>
              <Input id="logs-batch-s" name="batch_max_seconds" type="number" min={30} max={3600} required defaultValue={set.batch_max_seconds} />
            </Field>
            <Field>
              <FieldLabel htmlFor="logs-batch-mb">…or at this size (MB, before compression)</FieldLabel>
              <Input id="logs-batch-mb" name="batch_max_mb" type="number" min={1} max={16} required defaultValue={set.batch_max_mb} />
            </Field>
            <Field>
              <FieldLabel htmlFor="logs-spool">Space for logs waiting on this server (MB)</FieldLabel>
              <Input id="logs-spool" name="spool_cap_mb" type="number" min={64} max={102400} required defaultValue={set.spool_cap_mb} />
            </Field>
          </FieldGroup>
          <FieldDescription className="mt-3">
            When the storage can't be reached, logs wait on this server; past this space the oldest are dropped (and counted). The shipper's own buffer
            (up to 256 MB) comes on top.
          </FieldDescription>
        </details>

        <div>
          <Button type="submit" disabled={busy}>
            Save
          </Button>
        </div>
      </form>
    </Section>
  )
}
