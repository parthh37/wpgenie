import { lazy, type ComponentType, type LazyExoticComponent } from "react"
import {
  ArchiveIcon, ChartColumnIcon, CloudIcon, CodeIcon, DatabaseIcon, FolderIcon, GaugeIcon, GitBranchIcon,
  HeartPulseIcon, HistoryIcon, KeyRoundIcon, LayoutDashboardIcon, LinkIcon, PlugIcon, RefreshCwIcon,
  ServerIcon, ShieldIcon, UploadCloudIcon, UsersIcon, type LucideIcon,
} from "lucide-react"
import type { Tint } from "@/components/app/icon-tile"
import { accessAllows } from "@/lib/session"
import type { Site, SiteAccess } from "@/lib/types"

// A site's sections: the workspace's rail, in groups, each a lazily loaded
// component taking the site. Addresses are #/sites/<id>/<key>, as in the
// legacy panel ("security" was Protection's key until 2026-10).

export interface SectionProps {
  site: Site
}

export type RailGroup = "" | "Speed" | "Protection" | "Data" | "Settings"

export interface SectionDef {
  key: string
  label: string
  group: RailGroup
  icon: LucideIcon
  tint: Tint
  // Whether the site has this section (by its state; null: always).
  when?: (site: Site, ctx: { clustered: boolean }) => boolean
  // The least a site shared with you must be shared at to show it (what
  // it shows needs that level from the API).
  access?: SiteAccess
  component: LazyExoticComponent<ComponentType<SectionProps>>
}

const active = (s: Site) => s.status === "active"
// Sharing: sites an account owns, for their owners and staff.
const owned = (s: Site) => !!s.account_id && !s.access

export const RAIL_GROUPS: RailGroup[] = ["", "Speed", "Protection", "Data", "Settings"]

export const SECTIONS: SectionDef[] = [
  { key: "overview", label: "Overview", group: "", icon: LayoutDashboardIcon, tint: "indigo", component: lazy(() => import("./sections/overview")) },
  { key: "health", label: "Health", group: "", icon: HeartPulseIcon, tint: "red", component: lazy(() => import("./sections/health")) },
  { key: "wordpress", label: "WordPress admin", group: "", icon: KeyRoundIcon, tint: "yellow", when: active, access: "developer", component: lazy(() => import("./sections/wordpress")) },
  { key: "performance", label: "Performance & scaling", group: "Speed", icon: GaugeIcon, tint: "pink", component: lazy(() => import("./sections/performance")) },
  { key: "cdn", label: "CDN", group: "Speed", icon: CloudIcon, tint: "cyan", component: lazy(() => import("./sections/cdn")) },
  { key: "insights", label: "Insights", group: "Speed", icon: ChartColumnIcon, tint: "purple", when: active, component: lazy(() => import("./sections/insights")) },
  { key: "uploads", label: "Uploads offload", group: "Speed", icon: UploadCloudIcon, tint: "teal", component: lazy(() => import("./sections/uploads")) },
  { key: "protection", label: "Protection", group: "Protection", icon: ShieldIcon, tint: "green", component: lazy(() => import("./sections/protection")) },
  { key: "plugins", label: "Plugins", group: "Protection", icon: PlugIcon, tint: "purple", component: lazy(() => import("./sections/plugins")) },
  { key: "updates", label: "Updates", group: "Protection", icon: RefreshCwIcon, tint: "green", component: lazy(() => import("./sections/updates")) },
  { key: "backups", label: "Backups", group: "Data", icon: ArchiveIcon, tint: "orange", when: active, component: lazy(() => import("./sections/backups")) },
  { key: "staging", label: "Staging", group: "Data", icon: GitBranchIcon, tint: "teal", when: active, component: lazy(() => import("./sections/staging")) },
  { key: "files", label: "Files", group: "Data", icon: FolderIcon, tint: "blue", when: (s) => s.status === "active" || s.status === "suspended", access: "developer", component: lazy(() => import("./sections/files")) },
  { key: "sftp", label: "SFTP & database", group: "Data", icon: DatabaseIcon, tint: "graphite", when: active, component: lazy(() => import("./sections/sftp")) },
  { key: "domains", label: "Domains & SSL", group: "Settings", icon: LinkIcon, tint: "blue", when: active, component: lazy(() => import("./sections/domains")) },
  { key: "php", label: "PHP", group: "Settings", icon: CodeIcon, tint: "indigo", when: active, component: lazy(() => import("./sections/php")) },
  { key: "server", label: "Server", group: "Settings", icon: ServerIcon, tint: "graphite", when: (_s, c) => c.clustered, component: lazy(() => import("./sections/server")) },
  { key: "sharing", label: "Sharing", group: "Settings", icon: UsersIcon, tint: "indigo", when: owned, component: lazy(() => import("./sections/sharing")) },
  { key: "activity", label: "Activity", group: "Settings", icon: HistoryIcon, tint: "brown", component: lazy(() => import("./sections/activity")) },
]

export function sectionsFor(site: Site, clustered: boolean) {
  return SECTIONS.filter((s) => (!s.when || s.when(site, { clustered })) && (!s.access || accessAllows(site.access, s.access)))
}

// resolveSection: the section an address names, if the site has it; else
// the overview.
export function resolveSection(site: Site, clustered: boolean, key: string | undefined) {
  if (key === "security") key = "protection"
  return sectionsFor(site, clustered).find((s) => s.key === key) ?? SECTIONS[0]
}
