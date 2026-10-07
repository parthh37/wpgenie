// CONTRACT (owner: billing-core agent). An account's billing for staff
// (profile, credit, contacts, plan and cycle, recent invoices): shown in
// the Accounts page's account detail.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type AccountLike = { id: number; name: string; [k: string]: any }

export function AccountBilling(_props: { account: AccountLike }) {
  return null
}
