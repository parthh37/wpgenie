// The API's shapes for a site's backups, staging, domains, PHP and SFTP
// sections (internal/api/environments.go and the store structs).

export interface BackupPolicy {
  site_id: string
  repo_id: string
  interval_hours: number // 0 = manual backups only
  keep_last: number
  keep_daily: number
  keep_weekly: number
  keep_monthly: number
  last_backup_at?: string
  last_attempt_at?: string
  last_error?: string
}

export interface BackupInfo {
  id: string
  short_id: string
  repo_id: string
  site_id: string
  domain: string
  time: string
  kind: "scheduled" | "manual" | "safety" | ""
  size: number // bytes backed up (before dedup and compression)
  added: number // new data this backup stored (compressed)
  files: number
}

// GET /sites/{id}/backups
export interface SiteBackups {
  policy: BackupPolicy | null
  backups: BackupInfo[]
  errors: Record<string, string> | null // destination ID -> why it couldn't be read
}

// GET /sites/{id}/backups/destinations: where this site's backups may go.
export interface BackupDestination {
  id: string
  name: string
  kind: string
}

// GET /sites/{id}/certificate: an uploaded certificate (null: automatic).
export interface SiteCert {
  site_id: string
  names: string[]
  issuer: string
  not_after: string
  trusted: boolean
  created_at: string
}

// GET /sites/{id}/dns-check?domain=
export interface DNSCheck {
  domain: string
  status: "ok" | "missing" | "elsewhere" | "partly" | "proxied" | "unknown" | string
  addresses: string[]
  expected: string[]
  server?: string
  message: string
}

// GET /php
export interface PHPVersions {
  versions: string[]
  default: string
}

export interface SFTPUser {
  username: string
  site_id: string
  password: boolean // has a password
  public_keys: string[]
  created_at: string
  added_by?: string // who added it (older logins: unknown)
}

// GET /sites/{id}/sftp
export interface SFTPInfo {
  users: SFTPUser[]
  server: { running: boolean; port: number; host_keys: string[] }
  host: string
  phpmyadmin_sessions: number
}
