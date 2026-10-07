import type { Plan, PlanLimits } from "@/features/plans/types"
import type { Role } from "@/lib/types"

// The API's accounts (api.accountView), their usage this month
// (billing.Usage), burst minutes (billing.BurstBalance), users and events.

export interface Account {
  id: number
  name: string
  kind: "customer" | "reseller" | string
  status: "active" | "suspended" | "terminated" | string
  // Who suspended it: admin, billing, overage or reseller.
  suspend_reason?: string
  plan_id: string
  parent_id?: number
  email?: string
  whmcs_service_id?: string
  stripe_customer_id?: string
  stripe_subscription_id?: string
  burst_credit: number
  created_at: string
  updated_at: string
  suspended_at?: string
  plan: Plan
  limits: PlanLimits
  // Suspended itself, or through its reseller.
  effectively_suspended: boolean
  sites: number
  parent_name?: string
}

export interface SiteUsage {
  site_id: string
  account_id: number
  files_bytes: number
  db_bytes: number
  bandwidth_bytes: number
  measured_at?: string
}

export interface Usage {
  account_id: number
  month_start: string
  sites: number
  max_sites: number
  disk_bytes: number
  disk_limit_bytes: number
  bandwidth_bytes: number
  bandwidth_limit_bytes: number
  includes_customers: boolean
  per_site: SiteUsage[] | null
}

export interface BurstBalance {
  month_start: string
  allowed: boolean
  included: number
  unlimited: boolean
  used: number
  credit: number
  remaining: number
  per_site?: Record<string, number> | null
}

export interface AccountUser {
  id: number
  username: string
  role: Role
  disabled: boolean
  account_id?: number
  totp_enabled: boolean
  created_at: string
  last_login_at?: string
}

export interface AccountEvent {
  id: number
  time: string
  kind: string
  message: string
}

export const accountPath = (id: number | string) => `/accounts/${encodeURIComponent(String(id))}`
