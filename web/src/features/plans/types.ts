// The API's plans (store.Plan) and the limits an account gets from one
// (billing.Limits): a reseller's customers are narrowed to the reseller's.

export interface PlanPrice {
  price: number
  setup_fee: number
}

export interface PlanLimits {
  max_sites: number
  disk_mb: number
  bandwidth_gb: number
  max_replicas: number
  max_memory_mb: number
  max_cpus: number
  max_domains: number
  burst_minutes: number
  features: string[] | null
  backup_repos: string[] | null
}

export interface Plan extends PlanLimits {
  id: string
  name: string
  overage: "notify" | "suspend" | string
  resellable: boolean
  description?: string
  public?: boolean
  sort?: number
  account_kind?: "customer" | "reseller" | string
  prices?: Record<string, PlanPrice> | null
  overage_gb_price?: number
  created_at?: string
  updated_at?: string
}

// The optional capabilities a plan may include (billing.Features).
export const FEATURES = ["staging", "backups", "sftp", "files", "phpmyadmin", "certificates", "cdn", "smtp", "burst"]
