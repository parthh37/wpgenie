import type { LucideIcon } from "lucide-react"
import { cn } from "@/lib/utils"

// An icon as Cloudflare's dashboard draws them: in the subtle text colour,
// alone at list sizes (xs, sm) and in a small neutral square, ringed by a
// hairline, from md up. Every place looks the same; the tint is accepted
// (callers still pass one) but no longer colours anything.

export type Tint =
  | "indigo" | "blue" | "green" | "orange" | "red" | "pink" | "purple" | "cyan"
  | "teal" | "mint" | "yellow" | "brown" | "graphite" | "gray"

const SIZE = {
  xs: "size-4 bg-transparent shadow-none [&_svg]:size-4",
  sm: "size-5 bg-transparent shadow-none [&_svg]:size-4",
  md: "size-7 rounded-md [&_svg]:size-4",
  lg: "size-9 rounded-lg [&_svg]:size-[18px]",
  xl: "size-11 rounded-lg [&_svg]:size-5",
}

export function IconTile({
  icon: Icon,
  size = "md",
  className,
}: {
  icon: LucideIcon
  tint?: Tint
  size?: keyof typeof SIZE
  className?: string
}) {
  return (
    <span aria-hidden className={cn("icon-tile", SIZE[size], className)}>
      <Icon strokeWidth={1.75} />
    </span>
  )
}
