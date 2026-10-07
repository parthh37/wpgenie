// CONTRACT (owner: billing-core agent). Changing an account's plan (with a
// quote) and cancelling it: used by staff (account billing) and tenants
// (client area). Plan limits in words.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type Plan = { id: string; name: string; [k: string]: any }

export function PlanChangeDialog(_props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  accountId: number
  planId?: string
  cycle?: string
  tenant?: boolean
  onChanged?: () => void
}) {
  return null
}

export function CancelDialog(_props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  accountId: number
  name: string
  admin?: boolean
  nextDue?: string
  onDone?: () => void
}) {
  return null
}

export function PlanLimits(_props: { plan: Plan }) {
  return null
}
