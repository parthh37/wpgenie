// The API's shapes the Protection, Plugins, Updates, Health and WordPress
// sections read (internal/site, internal/store, internal/shield). Go's nil
// slices arrive as null, so every list is optional here.

// A known vulnerability of an installed version.
export interface Vuln {
  title: string
  severity?: string // critical | high | medium | low
  score?: string
  fixed_in?: string
  unfixed?: boolean
  cve?: string
  link?: string
}

// Core, a plugin or a theme.
export interface WPComponent {
  type: "core" | "plugin" | "theme" | string
  slug: string
  status?: string
  version: string
  update_version?: string
  vulns?: Vuln[] | null
  // The available update has none of the vulnerabilities.
  update_fixes?: boolean
}

export interface Inventory {
  core: WPComponent
  plugins: WPComponent[] | null
  themes: WPComponent[] | null
}

export const componentsOf = (inv: Inventory | null | undefined): WPComponent[] =>
  inv ? [inv.core, ...(inv.plugins ?? []), ...(inv.themes ?? [])].filter(Boolean) : []

export interface ScanReport {
  scanned_at: string
  inventory: Inventory | null
  integrity: {
    core_modified: string[] | null
    plugins_modified: string[] | null
    uploads_php: string[] | null
  }
  vulnerable: number
  // Compared with the previous scan; absent in reports from before it.
  intrusion?: Intrusion | null
  errors?: string[] | null
}

export interface AdminAccount {
  id: number
  login: string
  email?: string
  registered?: string
  role?: string
  super?: boolean
}

export interface FileChange {
  path: string
  change: "changed" | "added" | string
  component: string
}

export interface Intrusion {
  admins: AdminAccount[] | null
  new_admins: AdminAccount[] | null
  admins_baseline?: string
  file_changes: FileChange[] | null
  file_changes_total: number
  updated?: string[] | null
  files_checked: number
  files_truncated?: boolean
  files_baseline?: string
}

export interface ProtectionLevel {
  id: "basic" | "recommended" | "strict" | string
  waf: boolean
  body_waf: string
  reputation: string
  rate_rps: number
  rate_burst: number
  login_per_min: number
  challenge_bits: number
}

// One thing protection did (GET /security/events).
export interface ShieldEvent {
  time: string
  site: string
  ip: string
  verdict: string
  reason: string
  path: string
}

export interface PluginPerf {
  load_ms: number
  load_kb: number
  hook_ms: number
  calls: number
  queries: number
}

export interface PluginInfo {
  slug: string
  title?: string
  version: string
  status: string
  update_version?: string
  directory: "listed" | "closed" | "not_listed" | "unknown" | string
  last_updated?: string
  abandoned?: boolean
  closed_date?: string
  closed_reason?: string
  active_installs?: number
  tested_up_to?: string
  checksums: "verified" | "modified" | "unavailable" | "not_checked" | string
  modified?: string[] | null
  signatures?: string[] | null
  vulns?: number
  perf?: PluginPerf | null
  flags: string[] | null
}

export interface PluginReport {
  analysed_at: string
  plugins: PluginInfo[] | null
  profile?: {
    total_ms: number
    peak_memory_kb: number
    queries: number
    output_bytes: number
    status?: number
    redirect?: string
    others?: Record<string, PluginPerf> | null
  } | null
  theme_signatures?: string[] | null
  errors?: string[] | null
}

export interface UpdateRun {
  id: number
  site_id: string
  trigger: "manual" | "auto" | "analyser" | string
  status: "running" | "updated" | "rolled_back" | "failed" | "up_to_date" | string
  summary: string
  started_at: string
  finished_at?: string
}

export interface UpdateHistoryEntry {
  run: UpdateRun
  details: unknown
}

export interface Finding {
  id: string
  severity: "critical" | "high" | "medium" | "low" | "info" | string
  category: string
  title: string
  detail?: string
  fix?: string
}

export interface Analysis {
  analysed_at: string
  score: number
  grade: string
  facts?: {
    wp_version: string
    php_version: string
    plugins_active: number
    db_bytes: number
    autoload_bytes: number
  } | null
  components: WPComponent[] | null
  scanned_at?: string | null
  findings: Finding[] | null
  errors?: string[] | null
}

export interface FixResult {
  message: string
  // The fix goes on in the background: a job (the tray follows it) or an
  // update run (Updates shows it). Job IDs arrive as numbers although
  // lib/jobs types them as strings; they are passed on untouched.
  job_id?: number | string
  update_run?: number
}

export interface WPUser {
  id: number
  login: string
  email: string
  name: string
  role: "administrator" | "editor"
  // The site's first user: never deleted; deleted users' content moves to it.
  owner?: boolean
}
