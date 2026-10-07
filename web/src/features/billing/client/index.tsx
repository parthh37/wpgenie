import { useQuery } from "@tanstack/react-query"
import { Building2Icon, LayoutDashboardIcon, MailIcon, ReceiptIcon, WalletIcon, type LucideIcon } from "lucide-react"
import { LoadError, SubNav } from "@/components/app/blocks"
import { Page, PageHeader } from "@/components/app/page"
import { Skeleton } from "@/components/ui/skeleton"
import { loadBillingConfig } from "@/lib/money"
import { billingPath, type BillingRoute } from "../route"
import { useAccount, useProfile, type ClientCtx } from "./data"
import { ClientInvoices } from "./invoices"
import { ClientNotices } from "./notices"
import { ClientDetails, ClientEmails, ClientPayments } from "./others"
import { ClientOverview } from "./overview"
import { usePayFlow } from "./pay"

// The client area: a customer's or reseller's own billing (the Billing tab
// for tenants). What's due and paying it, the plan and changing it, the
// saved card, invoices, payments, billing details and e-mails. The server
// scopes every call to the user's account.

const VIEWS: Array<[key: string, label: string, icon: LucideIcon]> = [
  ["", "Overview", LayoutDashboardIcon],
  ["invoices", "Invoices", ReceiptIcon],
  ["transactions", "Payments", WalletIcon],
  ["details", "Billing details", Building2Icon],
  ["emails", "E-mails", MailIcon],
]

export function ClientArea({ route }: { route: BillingRoute }) {
  const key = VIEWS.find(([k]) => k === route.view)?.[0] ?? ""
  // Opened afresh, the store's settings are read again (they may have changed).
  const cfg = useQuery({ queryKey: ["/billing/config"], queryFn: loadBillingConfig, staleTime: 5 * 60_000, refetchOnMount: "always" })
  const account = useAccount()
  const acct = account.data?.account ?? null
  const profile = useProfile(acct?.id)
  const flow = usePayFlow(profile.data)

  const error =
    cfg.error || account.error || profile.error || (account.data && !acct ? new Error("This user doesn't belong to an account.") : null)
  const ready = !error && !!cfg.data && !!acct && profile.data !== undefined
  const ctx: ClientCtx | null = ready ? { acct: acct!, profile: profile.data ?? null, pay: flow.pay, payDue: flow.payDue } : null

  return (
    <Page>
      <PageHeader icon={ReceiptIcon} tint="mint" title="Billing" description="Your plan, invoices and payments." />
      {ctx && <ClientNotices ctx={ctx} />}
      <SubNav
        label="Billing sections"
        items={VIEWS.map(([k, label, icon]) => ({ key: k, label, icon, href: billingPath(k) }))}
        current={key}
      />
      {error ? (
        <LoadError
          error={error}
          retry={() => {
            void cfg.refetch()
            void account.refetch()
            if (acct) void profile.refetch()
          }}
        />
      ) : !ctx ? (
        <div aria-busy="true" className="flex flex-col gap-4">
          <span className="sr-only">Loading…</span>
          <Skeleton className="h-24 rounded-2xl" />
          <Skeleton className="h-56 rounded-2xl" />
        </div>
      ) : key === "invoices" ? (
        <ClientInvoices ctx={ctx} id={route.id} query={route.query} />
      ) : key === "transactions" ? (
        <ClientPayments />
      ) : key === "details" ? (
        <ClientDetails ctx={ctx} />
      ) : key === "emails" ? (
        <ClientEmails ctx={ctx} />
      ) : (
        <ClientOverview ctx={ctx} />
      )}
      {flow.dialog}
    </Page>
  )
}
