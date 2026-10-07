import { useSyncExternalStore } from "react"
import { useQuery } from "@tanstack/react-query"
import { api, ApiError } from "@/lib/api"
import type { BillingConfig } from "@/lib/money"
import { invalidate, queryClient } from "@/lib/query"
import type { Invoice } from "@/features/billing/shared/invoice-document"
import type { Plan } from "@/features/billing/shared/plan-dialogs"

// The client area's data: the signed-in tenant's account and its billing
// profile, as the legacy clientarea.js loaded them. The server scopes every
// call to the user's own account.

export interface ClientAccount {
  id: number
  name: string
  status: string
  suspend_reason?: string
  plan_id: string
  plan?: Plan | null
  parent_id?: number
}

export interface SavedCard {
  brand?: string
  last4: string
  exp_month: number
  exp_year: number
}

export interface Profile {
  mode: string // invoice, whmcs, stripe_subscription, none
  cycle: string
  next_due_at?: string | null
  auto_pay: boolean
  card?: SavedCard | null
  credit: number
  cancel_at?: string | null
  contact?: Record<string, unknown>
  balance_due: number
  overdue: boolean
  recurring_amount?: number | null
  upcoming?: { period_start: string; period_end: string; amount: number } | null
}

export interface BurstBalance {
  allowed: boolean
  included: number
  unlimited: boolean
  used: number
  credit: number
  remaining?: number
}

export interface Payment {
  id: number
  invoice_id?: number
  invoice_number?: string
  gateway?: string
  method?: string
  reference?: string
  amount: number
  refunded?: number
  at: string
}

export interface MailMessage {
  id: number
  subject: string
  status: string
  created_at: string
  sent_at?: string
}

// What a payment answers: off to a payment page, bank details, or paid.
export interface PayNext {
  redirect_url?: string
  instructions?: string
  reference?: string
  paid?: boolean
  error?: string
}

export interface BurstPack {
  id: string
  minutes: number
  price: number
}

export type ClientInvoice = Invoice & {
  number?: string
  status: string
  overdue?: boolean
  kind?: string
  total: number
  balance: number
  issued_at?: string
  due_at?: string
}

// The account (tenants get theirs with GET /account).
export function useAccount() {
  return useQuery({
    queryKey: ["/account"],
    queryFn: () => api<{ account?: ClientAccount | null }>("GET", "/account"),
  })
}

// The account's billing profile; null when billing isn't set up for it (404).
export const profileKey = (accountId: number) => [`/accounts/${accountId}/billing`]
export function useProfile(accountId: number | undefined) {
  return useQuery({
    queryKey: profileKey(accountId ?? 0),
    enabled: accountId != null,
    queryFn: () =>
      api<Profile>("GET", `/accounts/${accountId}/billing`).catch((e) => {
        if (e instanceof ApiError && e.status === 404) return null
        throw e
      }),
  })
}

// Burst minutes this month; null on servers or plans without them.
export function useBurst(accountId: number) {
  return useQuery({
    queryKey: [`/accounts/${accountId}/burst`],
    queryFn: () => api<BurstBalance>("GET", `/accounts/${accountId}/burst`).catch(() => null),
  })
}

// reloadBilling refetches everything a payment or a change can touch: the
// account (its status), its profile, burst minutes, invoices, payments, and
// the shell's idea of whether the account must pay first.
export function reloadBilling() {
  return Promise.all([
    invalidate("/account"),
    invalidate("/invoices"),
    invalidate("/transactions"),
    queryClient.invalidateQueries({ queryKey: ["tenant-billing"] }),
  ])
}

// ---- Burst minute packs ----

export const burstPacks = (cfg: BillingConfig | undefined): BurstPack[] =>
  ((cfg?.burst_packs as BurstPack[] | undefined) || []).filter((p) => p.minutes > 0)

// Packs are sold to accounts billed here, when there are any to sell.
export const canBuyBurst = (profile: Profile | null | undefined, cfg: BillingConfig | undefined) =>
  !!profile && profile.mode === "invoice" && burstPacks(cfg).length > 0

// ---- The Invoices screen's filter (kept while moving between screens) ----

let invoiceFilter = ""
const subs = new Set<() => void>()
export function setInvoiceFilter(f: string) {
  invoiceFilter = f
  subs.forEach((fn) => fn())
}
export const useInvoiceFilter = () =>
  useSyncExternalStore(
    (f) => (subs.add(f), () => void subs.delete(f)),
    () => invoiceFilter,
    () => invoiceFilter
  )

export const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

// What every screen of the client area gets: the account, its billing
// profile (null: billing isn't set up for it), and paying.
export interface ClientCtx {
  acct: ClientAccount
  profile: Profile | null
  pay: (inv: ClientInvoice) => void
  payDue: () => Promise<void>
}
