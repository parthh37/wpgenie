import type { ReactNode } from "react"
import {
  LandmarkIcon, LayoutDashboardIcon, MailIcon, PercentIcon, PlusIcon, ReceiptIcon, ShoppingCartIcon, SlidersHorizontalIcon, WalletIcon, type LucideIcon,
} from "lucide-react"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, LoadError, SubNav } from "@/components/app/blocks"
import { Page, PageHeader } from "@/components/app/page"
import { useBillingConfig, type BillingConfig } from "@/lib/money"
import { useSession } from "@/lib/session"
import { EmailView, PromotionsView, SettingsView, TaxesView } from "./admin"
import { ClientArea } from "./client"
import { billingPath, useBillingRoute, type BillingRoute } from "./route"
import { StaffInvoice } from "./staff/invoice"
import { useInvoiceEditor } from "./staff/invoice-editor"
import { StaffInvoices } from "./staff/invoices"
import { StaffOrders } from "./staff/orders"
import { StaffOverview } from "./staff/overview"
import { StaffTransactions } from "./staff/transactions"

// The Billing tab: the client area for tenants (customers, resellers); for
// staff, invoices, payments, orders and (by role) the store's settings.
export default function BillingPage() {
  const s = useSession()
  const route = useBillingRoute()
  if (s.isTenant) return <ClientArea route={route} />
  return <StaffBilling route={route} />
}

interface StaffView {
  key: string
  label: string
  icon: LucideIcon
  role?: "operator" | "admin"
  render: (route: BillingRoute, config: BillingConfig) => ReactNode
}

const STAFF_VIEWS: StaffView[] = [
  { key: "", label: "Overview", icon: LayoutDashboardIcon, render: (_r, c) => <StaffOverview config={c} /> },
  { key: "invoices", label: "Invoices", icon: ReceiptIcon, render: (r) => (r.id ? <StaffInvoice key={r.id} id={r.id} /> : <StaffInvoices />) },
  { key: "transactions", label: "Transactions", icon: WalletIcon, render: () => <StaffTransactions /> },
  { key: "orders", label: "Orders", icon: ShoppingCartIcon, role: "operator", render: (_r, c) => <StaffOrders config={c} /> },
  { key: "promotions", label: "Promotions", icon: PercentIcon, role: "admin", render: (r) => <PromotionsView route={r} /> },
  { key: "taxes", label: "Tax rules", icon: LandmarkIcon, role: "admin", render: (r) => <TaxesView route={r} /> },
  { key: "email", label: "E-mail", icon: MailIcon, role: "operator", render: (r) => <EmailView route={r} /> },
  { key: "settings", label: "Settings", icon: SlidersHorizontalIcon, role: "admin", render: (r) => <SettingsView route={r} /> },
]

function StaffBilling({ route }: { route: BillingRoute }) {
  const s = useSession()
  const views = STAFF_VIEWS.filter((v) => s.atLeast(v.role || "viewer"))
  const view = views.find((v) => v.key === route.view) || views[0]
  const config = useBillingConfig()
  const editor = useInvoiceEditor()

  return (
    <Page>
      <PageHeader
        icon={ReceiptIcon}
        tint="mint"
        title="Billing"
        description="Invoices, payments, and the reminders that collect them."
        actions={
          s.atLeast("admin") && (
            <ActionButton variant="default" run={() => editor.open({})}>
              <PlusIcon data-icon="inline-start" />
              New invoice
            </ActionButton>
          )
        }
      />
      <SubNav label="Billing sections" items={views.map((v) => ({ key: v.key, label: v.label, icon: v.icon, href: billingPath(v.key) }))} current={view.key} />
      {editor.dialog}
      {config.isError ? (
        <LoadError error={config.error} retry={() => config.refetch()} />
      ) : !config.data ? (
        <div aria-busy="true">
          <p className="sr-only">Loading…</p>
          <div className="mb-4 grid grid-cols-1 gap-3 min-[480px]:grid-cols-2 lg:grid-cols-3">
            {Array.from({ length: 3 }, (_, i) => (
              <Skeleton key={i} className="h-[104px] rounded-2xl" />
            ))}
          </div>
          <Skeleton className="h-[300px] rounded-2xl" />
        </div>
      ) : (
        <div key={view.key}>{view.render(route, config.data)}</div>
      )}
    </Page>
  )
}
