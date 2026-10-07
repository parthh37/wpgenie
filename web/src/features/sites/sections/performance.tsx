import { Badge } from "@/components/ui/badge"
import type { SectionProps } from "@/features/sites/sections"
import { BURST_NAMES, BurstCard } from "./speed/burst"
import { CacheCard, ImagesCard } from "./speed/caching"
import { Summary } from "./speed/shared"
import { TweaksCard } from "./speed/tweaks"

// Performance & scaling: burst (and, under Advanced, the size and
// autoscaling targets), the caches, image formats and WordPress tweaks.
export default function PerformanceSection({ site }: SectionProps) {
  const mode = site.burst_mode || "off"
  return (
    <div>
      <Summary>
        <Badge variant="secondary">Burst {BURST_NAMES[mode] || mode}</Badge>
        {site.page_cache && <Badge variant="secondary">Page cache</Badge>}
        {site.object_cache && <Badge variant="secondary">Object cache</Badge>}
      </Summary>
      <BurstCard site={site} />
      <CacheCard site={site} />
      <ImagesCard site={site} />
      {site.status === "active" && <TweaksCard site={site} />}
    </div>
  )
}
