import type { SectionProps } from "@/features/sites/sections"
import { SectionFallback } from "@/features/sites/section-fallback"

export default function ActivitySection({ site }: SectionProps) {
  return <SectionFallback site={site} section="activity" />
}
