import { CalendarIcon, ClockIcon, CreditCardIcon, LockIcon, ReceiptIcon, TriangleAlertIcon } from "lucide-react"
import { ActionButton, Banner } from "@/components/app/blocks"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtDueDate, money } from "@/lib/money"
import { reloadBilling, type ClientCtx } from "./data"

// ClientNotices: what needs doing, above every Billing screen.
export function ClientNotices({ ctx }: { ctx: ClientCtx }) {
  const { acct, profile: p, payDue } = ctx
  const pay = (label = "Pay now") => (
    <ActionButton variant="default" run={payDue}>
      <CreditCardIcon data-icon="inline-start" />
      {label}
    </ActionButton>
  )

  let due = null
  if (acct.status === "pending") {
    due = (
      <Banner tone="warn" icon={ClockIcon} title="Your order is waiting for payment" actions={pay("Pay and activate")}>
        Pay the invoice to activate your account: you can create sites as soon as it's paid.
      </Banner>
    )
  } else if (acct.status === "suspended" && acct.suspend_reason === "billing") {
    due = (
      <Banner tone="bad" icon={LockIcon} title="Your sites are suspended for an unpaid invoice" actions={pay()}>
        Pay what's due and they're back online within a minute.
      </Banner>
    )
  } else if (p?.overdue) {
    due = (
      <Banner tone="bad" icon={TriangleAlertIcon} title={`${money(p.balance_due)} is overdue`} actions={pay()}>
        Please pay it to keep your sites online.
      </Banner>
    )
  } else if (p && p.balance_due > 0) {
    due = <Banner tone="info" icon={ReceiptIcon} title={`${money(p.balance_due)} to pay`} actions={pay()} />
  }

  return (
    <>
      {due}
      {p?.cancel_at && (
        <Banner
          tone="info"
          icon={CalendarIcon}
          title={`Your service ends on ${fmtDueDate(p.cancel_at)}`}
          actions={
            <ActionButton
              run={async () => {
                await api("DELETE", `/accounts/${acct.id}/cancel`)
                notify("Your service continues")
                await reloadBilling()
              }}
            >
              Keep my service
            </ActionButton>
          }
        >
          Nothing more is billed. Changed your mind?
        </Banner>
      )}
    </>
  )
}
