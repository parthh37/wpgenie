import { useQuery } from "@tanstack/react-query"
import { api, ApiError } from "@/lib/api"
import type { Session } from "@/lib/session"
import type { NavFlags } from "@/app/pages"

// What the navigation shows beside its pages: tickets awaiting a reply,
// alerts firing, a WPGenie update; and which pages a tenant gets.

export interface SupportSummary {
  enabled: boolean
  total: number
  awaiting: number
  active: number
  limits?: { max_files: number; max_file_mb: number; extensions: string[] | null }
}

export function useSupportSummary(s: Session) {
  return useQuery<SupportSummary | null>({
    queryKey: ["/support/summary"],
    enabled: !!s.me,
    refetchInterval: 60_000,
    queryFn: async () => {
      try {
        return await api<SupportSummary>("GET", "/support/summary")
      } catch (e) {
        // No help desk on this server.
        if (e instanceof ApiError && e.status === 400) return null
        throw e
      }
    },
  })
}

export function useAlertsFiring(s: Session) {
  return useQuery({
    queryKey: ["/monitoring/alerts?limit=200"],
    enabled: s.isStaff,
    refetchInterval: 60_000,
    queryFn: () => api<{ active: unknown[] }>("GET", "/monitoring/alerts?limit=200"),
    select: (o) => o.active.length,
  })
}

export function useUpdateAvailable(s: Session) {
  return useQuery({
    queryKey: ["/system"],
    enabled: s.isStaff,
    staleTime: 10 * 60_000,
    queryFn: () => api<{ available: boolean }>("GET", "/system"),
    select: (i) => i.available,
  })
}

interface BillingState {
  show: boolean
  mustPay: boolean
}

// Tenants get Billing when their account is billed here (a reseller's
// customers are billed by the reseller); an account that must pay first (a
// new order, or suspended for an unpaid invoice) is taken straight to it.
export function useTenantBilling(s: Session) {
  return useQuery<BillingState>({
    queryKey: ["tenant-billing"],
    enabled: s.isTenant,
    queryFn: async () => {
      try {
        const [cfg, acct] = await Promise.all([
          api<{ enabled?: boolean }>("GET", "/billing/config").catch((e) => {
            if (e instanceof ApiError && e.status === 404) return { unavailable: true, enabled: false }
            throw e
          }),
          api<{ account?: { status: string; suspend_reason?: string; parent_id?: number } }>("GET", "/account"),
        ])
        const a = acct.account
        const mustPay = !!a && (a.status === "pending" || (a.status === "suspended" && a.suspend_reason === "billing"))
        const off = !a || !!a.parent_id || "unavailable" in cfg || (cfg.enabled === false && !mustPay)
        return { show: !off, mustPay: !off && mustPay }
      } catch {
        return { show: false, mustPay: false }
      }
    },
  })
}

export function useNavFlags(s: Session): NavFlags {
  const support = useSupportSummary(s)
  const billing = useTenantBilling(s)
  let supportShown: boolean | null = null
  if (support.isSuccess) {
    const sum = support.data
    supportShown = sum === null ? false : !(s.isTenant && !sum.enabled && sum.total === 0)
  }
  return { support: supportShown, billing: !!billing.data?.show }
}
