import { useHashQuery, useRoute } from "@/lib/router"

// Billing's addresses: #/billing/<view>/<id>/<sub>[?query] (#/billing/invoices/42,
// #/billing/settings/payments, #/billing?paid=1 after a payment page).
export interface BillingRoute {
  view: string
  id: string
  sub: string
  query: URLSearchParams
}

export function useBillingRoute(): BillingRoute {
  const [, view = "", id = "", sub = ""] = useRoute()
  return { view, id, sub, query: useHashQuery() }
}

export const billingPath = (view = "", id?: string | number) =>
  "/billing" + (view ? `/${view}` + (id != null && id !== "" ? `/${encodeURIComponent(String(id))}` : "") : "")
