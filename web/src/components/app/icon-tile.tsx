import type { LucideIcon } from "lucide-react"
import { cn } from "@/lib/utils"

// Settings-style icon tiles: each place in the panel has a colour, its
// symbol white on a lit rounded square.

export type Tint =
  | "indigo" | "blue" | "green" | "orange" | "red" | "pink" | "purple" | "cyan"
  | "teal" | "mint" | "yellow" | "brown" | "graphite" | "gray"

// Static class names (Tailwind only sees whole strings).
const TINT: Record<Tint, string> = {
  indigo: "[--tile:var(--t-indigo)]",
  blue: "[--tile:var(--t-blue)]",
  green: "[--tile:var(--t-green)]",
  orange: "[--tile:var(--t-orange)]",
  red: "[--tile:var(--t-red)]",
  pink: "[--tile:var(--t-pink)]",
  purple: "[--tile:var(--t-purple)]",
  cyan: "[--tile:var(--t-cyan)]",
  teal: "[--tile:var(--t-teal)]",
  mint: "[--tile:var(--t-mint)]",
  yellow: "[--tile:var(--t-yellow)]",
  brown: "[--tile:var(--t-brown)]",
  graphite: "[--tile:var(--t-graphite)]",
  gray: "[--tile:var(--t-gray)]",
}

const SIZE = {
  xs: "size-[22px] rounded-[6px] [&_svg]:size-3.5",
  sm: "size-[26px] rounded-[7px] [&_svg]:size-4",
  md: "size-7 rounded-[7px] [&_svg]:size-4",
  lg: "size-10 rounded-[10px] [&_svg]:size-5",
  xl: "size-12 rounded-[12px] [&_svg]:size-6",
}

export function IconTile({
  icon: Icon,
  tint = "gray",
  size = "md",
  className,
}: {
  icon: LucideIcon
  tint?: Tint
  size?: keyof typeof SIZE
  className?: string
}) {
  return (
    <span aria-hidden className={cn("icon-tile", TINT[tint], SIZE[size], className)}>
      <Icon strokeWidth={2.2} />
    </span>
  )
}
