// What the Tools endpoints answer (internal/site: maintenance.go,
// debug.go, tools.go, themes.go).

export interface Maintenance {
  on: boolean
  // What visitors read ("" = the default sentence).
  message: string
}

export interface Debug {
  on: boolean
  until?: string
}

export interface DebugLog {
  lines: string[]
  size: number
  // There is more before lines.
  truncated: boolean
}

export interface SearchReplaceResult {
  dry_run: boolean
  search: string
  replace: string
  total: number
  tables: { table: string; replacements: number }[]
  // The backup taken first (short ID), if any; backup_first: a real run
  // takes one (backups are set up on the site's server).
  backup?: string
  backup_first: boolean
}

export interface CronEvent {
  hook: string
  next_run: string
  // How often it repeats, in words ("" = once).
  recurrence: string
  schedule?: string
  interval?: number
  sig: string
  args: number
  overdue: boolean
}

export interface CronInfo {
  events: CronEvent[]
  overdue: number
  runner: {
    state: "ok" | "failing" | "waiting" | "staging" | "old_image" | string
    last_run?: string
    error?: string
  }
}

export interface Theme {
  slug: string
  title: string
  version: string
  status: "active" | "parent" | "inactive" | string
  update_version?: string
}

export interface WPSettings {
  title: string
  tagline: string
  // An IANA zone (or "UTC"); "" when WordPress uses a fixed offset.
  timezone: string
  utc_offset: number
  discourage_search: boolean
  comments_open: boolean
}

// MAX_MESSAGE matches site.MaxMaintenanceMessage.
export const MAX_MESSAGE = 300
