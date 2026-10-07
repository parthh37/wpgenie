import type { ReactNode } from "react"
import { showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { followJob, JOB_NAMES, type JobResult } from "@/lib/jobs"
import { invalidate, queryClient } from "@/lib/query"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import type { ProtectionLevel, Vuln } from "./types"

// Pieces the Protection, Plugins, Updates, Health and WordPress sections
// share (the legacy simple.js, app.js and wordpress.js helpers).

export const LEVEL_NAMES: Record<string, string> = { basic: "Basic", recommended: "Recommended", strict: "Strict" }
export const CHECK_NAMES: Record<string, string> = {
  auto: "automatic",
  under_attack: "every visitor",
  standard: "suspicious visitors only",
  off: "off",
}

// levelOf is the level a site's settings match, or "custom".
export function levelOf(site: Site, levels: ProtectionLevel[]): string {
  const l = levels.find(
    (l) =>
      l.waf === site.waf &&
      l.body_waf === site.body_waf &&
      l.reputation === site.reputation &&
      l.rate_rps === site.rate_rps &&
      l.rate_burst === site.rate_burst &&
      l.login_per_min === site.login_per_min &&
      l.challenge_bits === site.challenge_bits
  )
  return l ? l.id : "custom"
}

export interface ShieldChange {
  mode?: string
  block_ai_bots?: boolean
  level?: string
  waf?: boolean
  body_waf?: string
  xmlrpc?: boolean
  admin_allow?: string[]
  trusted_ips?: string[]
  deny_ips?: string[]
  reputation?: string
  country_mode?: string
  countries?: string[]
  country_action?: string
  rate_rps?: number
  rate_burst?: number
  login_per_min?: number
  challenge_bits?: number
}

// putShield changes a site's protection. The server only touches what is
// sent, except the visitor check and AI crawlers, which every change
// carries: they're the site's current ones unless changed here. The list
// shows the change at once; it's refetched either way. Throws on failure.
export async function putShield(site: Site, change: ShieldChange) {
  const body = { mode: site.shield_mode, block_ai_bots: site.block_ai_bots, ...change }
  queryClient.setQueryData<Site[]>(["/sites"], (xs) =>
    xs?.map((x) =>
      x.id === site.id
        ? { ...x, shield_mode: body.mode, block_ai_bots: body.block_ai_bots, ...(change.xmlrpc !== undefined ? { xmlrpc: change.xmlrpc } : {}) }
        : x
    )
  )
  try {
    await api("PUT", `/sites/${site.id}/shield`, body)
  } finally {
    await invalidate("/sites")
  }
}

// followFixJob follows a job a request started without startJob (a fix
// answering 200 with a job_id): its failure is shown, then the sites
// refresh. The ID is passed on as the API gave it.
export function followFixJob(id: number | string) {
  followJob(id as string, async (v: JobResult) => {
    if (v.job.status === "failed") showError(new Error(`${JOB_NAMES[v.job.kind] || v.job.kind} failed: ${v.job.error}`))
    await invalidate("/sites")
  })
}

// The server's own words, in the panel's: protection is never "Shield".
export const panelWords = (msg: string) => msg.replace(/\bShield\b/g, "Protection").replace(/\bshield\b/g, "protection")

// Long text in a table cell (cells don't wrap by themselves).
export const WRAP = "block min-w-40 whitespace-normal [overflow-wrap:anywhere]"

// Muted small print under a control.
export function Note({ children, className, tone }: { children: ReactNode; className?: string; tone?: "ok" | "warn" | "bad" }) {
  return (
    <p
      className={cn(
        "text-sm",
        tone === "ok" ? "text-success" : tone === "warn" ? "text-warning" : tone === "bad" ? "text-danger" : "text-muted-foreground",
        className
      )}
    >
      {children}
    </p>
  )
}

// "3.2 y ago", "40 d ago" (a plugin's last release).
export function age(t: string) {
  const d = (Date.now() - new Date(t).getTime()) / 864e5
  return d > 365 ? `${(d / 365).toFixed(1)} y ago` : `${Math.round(d)} d ago`
}

const SEV_ORDER = ["critical", "high", "medium", "low"]

// worstSeverity of a component's vulnerabilities ("medium" when unrated).
export const worstSeverity = (vs: Vuln[]) => SEV_ORDER.find((s) => vs.some((v) => v.severity === s)) || "medium"

// The colour of a severity word (the legacy .sev-* classes).
export function sevClass(level: string | undefined) {
  const l = (level || "").toLowerCase()
  if (l === "critical" || l === "high") return "font-semibold text-danger"
  if (l === "medium") return "text-warning"
  if (l === "warning") return "font-semibold text-warning"
  if (l === "info") return "text-muted-foreground"
  return ""
}

// Partial results: what couldn't be checked.
export function Partial({ label, errors }: { label: string; errors?: string[] | null }) {
  if (!errors?.length) return null
  return <Note className="mt-3">{`${label}: ${errors.join("; ")}`}</Note>
}
