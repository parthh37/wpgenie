import type { SectionProps } from "@/features/sites/sections"
import { SectionFallback } from "@/features/sites/section-fallback"

export default function ProtectionSection({ site }: SectionProps) {
  return <SectionFallback site={site} section="protection" />
}
