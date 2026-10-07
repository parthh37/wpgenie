import { LegacyFallback } from "@/components/app/legacy-fallback"

// Owner: billing-core agent (router: tenants -> ClientArea, staff -> views).
export default function BillingPage() {
  return <LegacyFallback path="billing" title="Billing" />
}
