import {
  BoxIcon, BugIcon, Building2Icon, ClipboardListIcon, FileIcon, GlobeIcon, InboxIcon, ListChecksIcon, MailIcon, ShieldAlertIcon, ShieldIcon,
  TerminalIcon, type LucideIcon,
} from "lucide-react"
import type { Tint } from "@/components/app/icon-tile"
import { api } from "@/lib/api"
import { invalidate, queryClient } from "@/lib/query"

// Log shipping to S3-compatible storage (internal/logship): the API's
// shapes, what each kind of log is, the storage presets, and saving.

export interface DayVolume {
  day: string
  events: number
  bytes: number
  dropped: number
}

export interface TypeStatus {
  name: string
  collect: string
  default: boolean
  enabled: boolean
  available: boolean
  today: DayVolume
  history: DayVolume[]
}

export type Health = "ok" | "warning" | "error" | "off"

export interface ShipStatus {
  enabled: boolean
  server: string
  health: Health | string
  health_message: string
  destination: { provider: string; endpoint: string; bucket: string; prefix: string }
  shipper: { state: string; exit_code?: number; restarts?: number; image: string; version?: string; error?: string }
  last_upload?: string
  sent_events: number
  sent_bytes: number
  upload_errors: number
  buffer_bytes: number
  spool: { bytes: number; files: number; cap: number }
  dropped_today: number
  retention: { days: number; last_run?: string; deleted: number; error?: string }
  export_error?: string
  types: TypeStatus[]
}

export interface LogStatus extends ShipStatus {
  servers?: Array<{ id: string; name: string; status?: ShipStatus; error?: string }>
}

export interface Destination {
  provider: string
  endpoint: string
  region: string
  bucket: string
  prefix: string
  access_key_id: string
  secret_key?: string
  path_style: boolean
  secret_key_set?: boolean
}

export interface LogSettings {
  enabled: boolean
  destination: Destination
  types: Record<string, boolean>
  compression: string
  batch_max_mb: number
  batch_max_seconds: number
  spool_cap_mb: number
  archive_retention_days: number
  local_access_logs: number
  container_log_mb: number
  server: string
  available: Record<string, boolean>
  providers: string[]
}

export interface ArchiveObject {
  key: string
  name: string
  size: number
  modified?: string
}

// What each kind of log is, for people who don't read logs every day.
export const LOG_TYPES: Record<string, { icon: LucideIcon; tint: Tint; title: string; desc: string }> = {
  access: { icon: GlobeIcon, tint: "blue", title: "Visitor requests", desc: "Every request to your sites: the page, the answer, how long it took and who asked (Caddy's access log)." },
  php_errors: { icon: BugIcon, tint: "red", title: "PHP errors", desc: "Warnings and errors from WordPress, plugins and themes, for each site." },
  waf: { icon: ShieldAlertIcon, tint: "orange", title: "Firewall matches", desc: "Requests the web application firewall flagged or blocked, and the rules they matched." },
  security: { icon: ShieldIcon, tint: "green", title: "Protection events", desc: "Blocks, bans and attacks protection caught (the Protection page's log, kept for good)." },
  audit: { icon: ClipboardListIcon, tint: "yellow", title: "Panel activity", desc: "Who changed what in this panel, and from where." },
  jobs: { icon: ListChecksIcon, tint: "purple", title: "Background jobs", desc: "Backups, restores, clones and updates: when they ran and how they went." },
  account_events: { icon: Building2Icon, tint: "cyan", title: "Account activity", desc: "Plan changes, suspensions and billing events of your clients' accounts." },
  email: { icon: MailIcon, tint: "indigo", title: "E-mails sent", desc: "Who the panel e-mailed, about what, and whether it arrived (never the message itself)." },
  mail: { icon: InboxIcon, tint: "teal", title: "Mail server", desc: "The mail server's own log: deliveries, rejections and sign-ins." },
  daemon: { icon: TerminalIcon, tint: "graphite", title: "WPGenie itself", desc: "The panel's own log: what it did, and any trouble it ran into." },
  containers: { icon: BoxIcon, tint: "brown", title: "Container output", desc: "Everything WPGenie's containers print. Detailed and noisy: turn it on to troubleshoot." },
}

export const typeInfo = (name: string) => LOG_TYPES[name] ?? { icon: FileIcon, tint: "gray" as Tint, title: name, desc: "" }

// Where logs can go. {region} in an endpoint is filled in from the region.
export interface Provider {
  name: string
  endpoint: string
  region: string
  pathStyle: boolean
  help: string
}

export const LOG_PROVIDERS: Record<string, Provider> = {
  aws: { name: "Amazon S3", endpoint: "https://s3.{region}.amazonaws.com", region: "us-east-1", pathStyle: false,
    help: "Create a bucket, then an IAM user whose access key may put, list, get and delete objects in it." },
  r2: { name: "Cloudflare R2", endpoint: "https://ACCOUNT_ID.r2.cloudflarestorage.com", region: "auto", pathStyle: true,
    help: "In R2, create an API token with \"Object Read & Write\" on the bucket. Replace ACCOUNT_ID with your account ID (on the R2 overview page)." },
  b2: { name: "Backblaze B2", endpoint: "https://s3.{region}.backblazeb2.com", region: "us-west-004", pathStyle: true,
    help: "The bucket's page shows its endpoint (the region is in it). Create an application key allowed to use the bucket." },
  wasabi: { name: "Wasabi", endpoint: "https://s3.{region}.wasabisys.com", region: "us-east-1", pathStyle: false,
    help: "Create an access key under Access Keys; the region is the bucket's (shown in its settings)." },
  spaces: { name: "DigitalOcean Spaces", endpoint: "https://{region}.digitaloceanspaces.com", region: "nyc3", pathStyle: false,
    help: "Create a Spaces access key limited to the bucket. The region is the Space's datacenter (nyc3, fra1, sgp1…)." },
  minio: { name: "MinIO", endpoint: "https://minio.example.com:9000", region: "us-east-1", pathStyle: true,
    help: "Your MinIO server's API address (not its console, usually port 9000) and an access key that may read and write the bucket." },
  custom: { name: "Other S3-compatible", endpoint: "https://s3.example.com", region: "", pathStyle: true,
    help: "Any storage that speaks the S3 API: its endpoint, region (if it has one) and an access key." },
}

export const providerOf = (id: string | undefined) => LOG_PROVIDERS[id || ""] ?? LOG_PROVIDERS.custom

export const LOG_RETENTION: Array<[number, string]> = [
  [30, "30 days"], [90, "3 months"], [180, "6 months"], [365, "1 year"], [730, "2 years"], [1825, "5 years"], [0, "Forever"],
]

// "just now", "5 min ago", "3 h ago", then the time.
export function logAgo(t: string, fmt: (t: string) => string) {
  const s = Math.round((Date.now() - new Date(t).getTime()) / 1000)
  if (s < 60) return "just now"
  if (s < 3600) return `${Math.round(s / 60)} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  return fmt(t)
}

export type SettingsPatch = Partial<Omit<LogSettings, "destination" | "types">> & {
  destination?: Partial<Destination>
  types?: Record<string, boolean>
}

// settingsBody is what PUT /logs/settings takes: the settings as loaded,
// with changes (the secret only when one was typed).
function settingsBody(set: LogSettings, patch: SettingsPatch) {
  const d = { ...set.destination, ...(patch.destination || {}) }
  return {
    enabled: patch.enabled ?? set.enabled,
    types: { ...set.types, ...(patch.types || {}) },
    compression: patch.compression ?? set.compression,
    batch_max_mb: patch.batch_max_mb ?? set.batch_max_mb,
    batch_max_seconds: patch.batch_max_seconds ?? set.batch_max_seconds,
    spool_cap_mb: patch.spool_cap_mb ?? set.spool_cap_mb,
    archive_retention_days: patch.archive_retention_days ?? set.archive_retention_days,
    local_access_logs: patch.local_access_logs ?? set.local_access_logs,
    container_log_mb: patch.container_log_mb ?? set.container_log_mb,
    destination: {
      provider: d.provider,
      endpoint: d.endpoint,
      region: d.region,
      bucket: d.bucket,
      prefix: d.prefix,
      access_key_id: d.access_key_id,
      secret_key: d.secret_key || "",
      path_style: d.path_style,
    },
  }
}

export const SETTINGS_KEY = ["/logs/settings"]

// saveLogSettings saves a change to the settings as last loaded, then
// refreshes the status.
export async function saveLogSettings(patch: SettingsPatch) {
  const set = queryClient.getQueryData<LogSettings>(SETTINGS_KEY)
  if (!set) throw new Error("The settings haven't loaded yet: try again in a moment")
  const next = await api<LogSettings>("PUT", "/logs/settings", settingsBody(set, patch))
  queryClient.setQueryData(SETTINGS_KEY, next)
  await invalidate("/logs/status")
  return next
}

// The destination is complete enough to ship to.
export const destinationReady = (d: Destination) => !!(d.bucket && d.endpoint && d.access_key_id && d.secret_key_set)

// logTestHint turns the usual storage refusals into what to check.
export function logTestHint(msg: string) {
  if (/403|AccessDenied|Forbidden|SignatureDoesNotMatch|InvalidAccessKeyId/i.test(msg)) return "Check the access key and its secret, and that the key may write to this bucket."
  if (/NoSuchBucket|404/i.test(msg)) return "Check the bucket's name (and that it exists in this region)."
  if (/PermanentRedirect|region|AuthorizationHeaderMalformed/i.test(msg)) return "The region doesn't match the bucket's: check it (and the endpoint)."
  if (/no such host|dial|timeout|connection refused|certificate|x509/i.test(msg)) return "The endpoint can't be reached from this server: check its address."
  return "Check the endpoint, region, bucket and key."
}

export const todayUTC = () => new Date().toISOString().slice(0, 10)
