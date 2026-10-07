// CONTRACT (owner: accounts-plans agent). A plan's prices per billing
// cycle (uncontrolled fields for a <form>), reading them back into the
// API's shape, and the price in words ("$12.00 / month").
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type PlanPrices = Record<string, any>

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function PriceEditor(_props: { plan?: Record<string, any> }) {
  return null
}
export function readPriceEditor(_form: HTMLFormElement): PlanPrices {
  return {}
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function planPriceText(_plan: Record<string, any>): string {
  return ""
}
