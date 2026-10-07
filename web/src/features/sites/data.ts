import { useQuery } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { Site, SiteStats } from "@/lib/types"
import type { RingValues } from "@/components/app/activity-rings"

// A site's live numbers: traffic (24 h), CPU now (the autoscaler's
// sampling, every 15 s) and whether an attack is under way (every minute).

export interface CPU {
  percent: number
  replicas: number
  workers_percent?: number | null
  queued?: number
  p95_ms?: number | null
  responses?: number
  at: string
}

export interface Attack {
  active: boolean
  attack?: { since: string; reason: string }
}

export const useSiteStats = (site: Site) =>
  useQuery<SiteStats>({
    queryKey: [`/sites/${site.id}/stats?hours=24`],
    queryFn: () => api<SiteStats>("GET", `/sites/${site.id}/stats?hours=24`),
    enabled: site.status === "active",
    staleTime: 60_000,
  })

export const useSiteCPU = (site: Site) =>
  useQuery<CPU | null>({
    queryKey: [`/sites/${site.id}/metrics`],
    queryFn: async () => (await api<{ cpu: CPU | null }>("GET", `/sites/${site.id}/metrics`)).cpu,
    enabled: site.status === "active",
    refetchInterval: 15_000,
    retry: false,
  })

export const useAttack = (site: Site) =>
  useQuery<Attack>({
    queryKey: [`/sites/${site.id}/attack`],
    queryFn: () => api<Attack>("GET", `/sites/${site.id}/attack`),
    enabled: site.status === "active",
    refetchInterval: 60_000,
    retry: false,
  })

// Vitals: [healthy responses %, CPU headroom %, protection 0/100].
export function vitals(site: Site, stats: SiteStats | undefined, cpu: CPU | null | undefined): RingValues {
  const healthy = stats && stats.totals.requests ? 100 * (1 - (stats.totals.errors_5xx || 0) / stats.totals.requests) : null
  const headroom = cpu ? Math.max(0, 100 - cpu.percent) : null
  return [healthy, headroom, site.shield_mode === "off" ? 0 : 100]
}

export const useVitals = (site: Site): RingValues => {
  const { data: stats } = useSiteStats(site)
  const { data: cpu } = useSiteCPU(site)
  return vitals(site, stats, cpu)
}

export const SHIELD_LABELS: Record<string, string> = {
  off: "Protection off",
  auto: "Protection on",
  standard: "Protection on",
  under_attack: "Checking everyone",
}

// A site's monogram hue: the same site, the same colour, every time.
const HUES = ["bg-[var(--t-indigo)]", "bg-[var(--t-pink)]", "bg-[var(--t-orange)]", "bg-[var(--t-teal)]", "bg-[var(--t-purple)]", "bg-[var(--t-blue)]"]
export const siteHue = (id: string) => HUES[[...id].reduce((n, c) => n + c.charCodeAt(0), 0) % 6]

// attentionFor says why a site needs a look and which section to open.
// "bad": broken or under fire now; "warn": a choice worth a second look.
export function attentionFor(site: Site, underAttack: boolean): { level: "bad" | "warn"; text: string; section: string } | null {
  if (site.status === "failed") return { level: "bad", text: "Setting it up failed. Activity says what went wrong.", section: "activity" }
  if (site.status === "suspended") return { level: "bad", text: "Suspended: visitors can’t reach it.", section: "overview" }
  if (underAttack) return { level: "bad", text: "Under attack. Every visitor is being checked and the site stays online.", section: "protection" }
  if (site.status !== "active") return null
  if (site.shield_mode === "off") return { level: "warn", text: "Protection is off: nothing is checked or blocked.", section: "protection" }
  if (site.shield_mode === "under_attack") return { level: "warn", text: "Every visitor is checked until you switch it back.", section: "protection" }
  return null
}
