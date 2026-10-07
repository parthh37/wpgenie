import { invalidate } from "@/lib/query"

// refreshBilling refetches what a change in billing can move: the lists,
// the overview, and the account's billing (profile, credit, e-mails).
export function refreshBilling(accountId?: number | null) {
  invalidate("/invoices")
  invalidate("/transactions")
  invalidate("/orders")
  invalidate("/billing/overview")
  if (accountId) invalidate(`/accounts/${accountId}/`)
}
