import { lazy, type ComponentType, type LazyExoticComponent } from "react"
import {
  ActivityIcon, ArchiveIcon, Building2Icon, GlobeIcon, LayersIcon, LifeBuoyIcon, MailIcon, ReceiptIcon,
  ScrollTextIcon, ServerIcon, ShieldIcon, SlidersHorizontalIcon, UserIcon, UsersIcon, type LucideIcon,
} from "lucide-react"
import type { Tint } from "@/components/app/icon-tile"
import type { Session } from "@/lib/session"

// Every page of the panel: its address (#/<key>), place in the navigation,
// who sees it, and its screen (a lazily loaded feature module, so each
// screen is its own chunk).

export type NavGroup = "" | "operate" | "business" | "infrastructure"

export interface PageDef {
  key: string
  label: string
  icon: LucideIcon
  tint: Tint
  group: NavGroup
  visible: (s: Session, flags: NavFlags) => boolean
  component: LazyExoticComponent<ComponentType>
}

// Facts about the server the navigation depends on.
export interface NavFlags {
  // Help desk configured, or this user has tickets (null: not known yet).
  support: boolean | null
  // Billing shown to this tenant (their account is billed here).
  billing: boolean
}

const staff = (s: Session) => s.isStaff

export const PAGES: PageDef[] = [
  { key: "sites", label: "Sites", icon: GlobeIcon, tint: "indigo", group: "", visible: () => true, component: lazy(() => import("@/features/sites")) },
  { key: "support", label: "Support", icon: LifeBuoyIcon, tint: "pink", group: "operate", visible: (_s, f) => f.support !== false, component: lazy(() => import("@/features/support")) },
  { key: "backups", label: "Backups", icon: ArchiveIcon, tint: "orange", group: "operate", visible: staff, component: lazy(() => import("@/features/backups")) },
  { key: "mail", label: "Mail", icon: MailIcon, tint: "blue", group: "operate", visible: staff, component: lazy(() => import("@/features/mail")) },
  { key: "security", label: "Protection", icon: ShieldIcon, tint: "green", group: "operate", visible: staff, component: lazy(() => import("@/features/protection")) },
  { key: "monitoring", label: "Monitoring", icon: ActivityIcon, tint: "red", group: "operate", visible: staff, component: lazy(() => import("@/features/monitoring")) },
  { key: "accounts", label: "Accounts", icon: Building2Icon, tint: "cyan", group: "business", visible: (s) => s.isStaff || s.isReseller, component: lazy(() => import("@/features/accounts")) },
  { key: "plans", label: "Plans", icon: LayersIcon, tint: "purple", group: "business", visible: staff, component: lazy(() => import("@/features/plans")) },
  { key: "billing", label: "Billing", icon: ReceiptIcon, tint: "mint", group: "business", visible: (s, f) => s.isStaff || f.billing, component: lazy(() => import("@/features/billing")) },
  { key: "servers", label: "Servers", icon: ServerIcon, tint: "graphite", group: "infrastructure", visible: staff, component: lazy(() => import("@/features/servers")) },
  { key: "logs", label: "Logs", icon: ScrollTextIcon, tint: "brown", group: "infrastructure", visible: staff, component: lazy(() => import("@/features/logs")) },
  { key: "users", label: "Users", icon: UsersIcon, tint: "yellow", group: "infrastructure", visible: (s) => s.isAdmin, component: lazy(() => import("@/features/users")) },
  { key: "system", label: "System", icon: SlidersHorizontalIcon, tint: "gray", group: "infrastructure", visible: staff, component: lazy(() => import("@/features/system")) },
  { key: "account", label: "Your account", icon: UserIcon, tint: "gray", group: "", visible: () => true, component: lazy(() => import("@/features/account")) },
]

export const GROUP_LABEL = (g: NavGroup, s: Session) =>
  g === "operate" ? (s.isTenant ? "Help" : "Operate") : g === "business" ? "Business" : g === "infrastructure" ? "Infrastructure" : ""
