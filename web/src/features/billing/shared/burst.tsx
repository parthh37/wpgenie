import { ZapIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { notify } from "@/components/app/toaster"
import { useBillingConfig } from "@/lib/money"
import { useSession } from "@/lib/session"
import { canBuyBurst, useAccount, useProfile } from "../client/data"
import { useBuyBurst } from "../client/pay"

// "Buy more minutes" for a tenant's burst status (site Performance section,
// Accounts): renders nothing until it's known the signed-in tenant's account
// may buy packs; null for staff.
export function BuyBurstButton() {
  const { isTenant } = useSession()
  if (!isTenant) return null
  return <TenantBuyBurst />
}

function TenantBuyBurst() {
  const cfg = useBillingConfig()
  const account = useAccount()
  const a = account.data?.account
  const profile = useProfile(a?.id)
  const buy = useBuyBurst(a?.id, profile.data, () => notify("Burst resumes within a minute."))
  const show = !!a && !!cfg.data && canBuyBurst(profile.data, cfg.data)
  return (
    <>
      {show && (
        <Button variant="tinted" onClick={buy.open}>
          <ZapIcon data-icon="inline-start" />
          Buy more minutes
        </Button>
      )}
      {buy.dialog}
    </>
  )
}
