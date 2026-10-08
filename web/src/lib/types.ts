// The API's shapes the shell and several screens share. Screens declare
// their own types for what only they use.

export type Role = "admin" | "operator" | "viewer" | "customer" | "reseller"

export interface User {
  id: number
  username: string
  role: Role
  disabled: boolean
  account_id?: number
  totp_enabled: boolean
  recovery_codes_left: number
  created_at: string
  last_login_at?: string
}

export interface AuthState {
  setup: boolean
  user: User | null
  require_2fa: boolean
  account?: { id: number; name: string; kind: string; status: string }
}

// What a user of another account may do on a site shared with them, least
// to most. Only owners delete a site or share it.
export type SiteAccess = "viewer" | "developer" | "manager"

// SiteGrant: a site shared with a user (GET /sites/<id>/access).
export interface SiteGrant {
  site_id: string
  user_id: number
  username: string
  access: SiteAccess
  granted_by: string
  created_at: string
}

export type SiteStatus = "provisioning" | "active" | "failed" | "suspended" | string

export interface Site {
  id: string
  name: string
  primary_domain: string
  domains: string[]
  redirect_domains: string[]
  php_version: string
  db_name: string
  status: SiteStatus
  shield_mode: "auto" | "standard" | "under_attack" | "off" | string
  block_ai_bots: boolean
  memory_mb: number
  cpus: number
  replicas: number
  page_cache: boolean
  object_cache: boolean
  cache_mobile: boolean
  upstream_ports: number[]
  spread_nodes: string[]
  node?: string
  image_formats: string[]
  optimize: string[]
  waf: boolean
  admin_allow: string[]
  trusted_ips: string[]
  deny_ips: string[]
  xmlrpc: boolean
  rate_rps: number
  rate_burst: number
  login_per_min: number
  challenge_bits: number
  reputation: string
  country_mode: string
  countries: string[]
  country_action: string
  body_waf: string
  autoscale: boolean
  min_replicas: number
  max_replicas: number
  target_cpu: number
  target_workers: number
  target_response_ms: number
  burst_mode: string
  burst_until?: string
  burst_paused: boolean
  auto_update: string
  smtp: boolean
  parent_id: string
  account_id?: number
  // The level another account shared this site with you at; absent when
  // it's yours (or you're staff).
  access?: SiteAccess
  // How many people its owners shared it with (for its owners and staff).
  shared_with?: number
  php: { memory_limit_mb: number; upload_max_mb: number; max_execution_time: number; max_input_vars: number }
  created_at: string
  updated_at: string
}

export interface SiteStats {
  since: string
  unique_visitors: number
  totals: {
    requests: number
    page_views: number
    bytes_out: number
    blocked: number
    bot_hits: number
    errors_5xx?: number
    [k: string]: number | undefined
  }
  series: Array<{ hour: string; page_views?: number; blocked?: number; [k: string]: number | string | undefined }>
}

// Job IDs are numbers in the API (store.Job.ID is an int64).
export type JobID = number | string

export interface Job {
  id: JobID
  kind: string
  site_id?: string
  status: "queued" | "running" | "succeeded" | "failed" | string
  progress: number
  step?: string
  error?: string
  created_at?: string
  finished_at?: string
}

export interface Node {
  id: string
  name: string
  status: string
  up: boolean
  last_error?: string
  public_ip?: string
  // This panel's own server.
  local?: boolean
  sites?: number
  [k: string]: unknown
}
