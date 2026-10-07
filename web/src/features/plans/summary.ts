import type { PlanLimits } from "./types"

// A limit in words: 0 is unlimited.
export const limitText = (n: number | null | undefined, unit: string) => (n ? `${n}${unit}` : "unlimited")

// planSummary is a plan's (or an account's) limits on one line.
export function planSummary(p: PlanLimits) {
  const features = p.features ?? []
  return (
    `${limitText(p.max_sites, " sites")} · ${limitText(p.disk_mb, " MB disk")} · ${limitText(p.bandwidth_gb, " GB/month")} · ` +
    `per site ${limitText(p.max_replicas, " replicas")} × ${limitText(p.max_memory_mb, " MB")}, ${limitText(p.max_cpus, " CPU")}` +
    (features.includes("burst") ? ` · burst ${limitText(p.burst_minutes, " min/month")}` : "") +
    (features.length ? ` · ${features.join(", ")}` : "")
  )
}
