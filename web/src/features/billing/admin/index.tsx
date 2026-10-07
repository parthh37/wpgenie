// The staff Billing tab's admin views, rendered by features/billing/index.tsx
// under its sub-navigation: #/billing/promotions, #/billing/taxes,
// #/billing/email[/templates|/log], #/billing/settings[/<part>].
// The server checks every setting again; nothing here is trusted.
import type { BillingRoute } from "../route"
import { Email } from "./email"
import { Promotions } from "./promotions"
import { Settings } from "./settings"
import { Taxes } from "./taxes"

export function PromotionsView(_props: { route: BillingRoute }) {
  return <Promotions />
}
export function TaxesView(_props: { route: BillingRoute }) {
  return <Taxes />
}
export function EmailView({ route }: { route: BillingRoute }) {
  return <Email route={route} />
}
export function SettingsView({ route }: { route: BillingRoute }) {
  return <Settings route={route} />
}
