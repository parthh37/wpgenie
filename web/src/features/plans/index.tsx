import { useState } from "react"
import { LayersIcon, PencilIcon, PlusIcon, Trash2Icon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, EmptyState, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Page, PageHeader, Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { plural } from "@/lib/format"
import { useBillingConfig } from "@/lib/money"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import { PlanDialog } from "./plan-dialog"
import { planCycles, planPriceText } from "./price-editor"
import { planSummary } from "./summary"
import type { Plan } from "./types"

// The Plans page: what each account can use. Everyone on staff sees the
// plans; administrators create, edit and delete them.
export default function PlansPage() {
  const s = useSession()
  const plans = useApi<Plan[]>("/plans")
  const billing = useBillingConfig()
  const [editing, setEditing] = useState<{ plan: Plan | null } | null>(null)

  const list = plans.data ?? []
  // Prices per billing period, when the server has them (older servers
  // reject the fields). With no plans yet, a server with billing has them.
  const pricing = list.some((p) => "prices" in p) || (list.length === 0 && !!billing.data && !billing.data.unavailable)
  // Money is shown in the store's currency: once its configuration is known.
  const moneyReady = billing.isFetched || billing.isError

  return (
    <Page>
      <PageHeader
        icon={LayersIcon}
        tint="purple"
        title="Plans"
        description="What each account can use: sites, disk, bandwidth and burst minutes."
        actions={
          s.isAdmin && (
            <Button onClick={() => setEditing({ plan: null })} disabled={!plans.data || !moneyReady}>
              <PlusIcon data-icon="inline-start" />
              New plan
            </Button>
          )
        }
      />

      {plans.isLoading ? (
        <Section>
          <div className="flex flex-col gap-3">
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} className="h-10 w-full" />
            ))}
          </div>
        </Section>
      ) : plans.error ? (
        <LoadError error={plans.error} retry={() => plans.refetch()} />
      ) : !list.length ? (
        <EmptyState
          icon={LayersIcon}
          tint="purple"
          title="No plans yet"
          actions={
            s.isAdmin && (
              <Button onClick={() => setEditing({ plan: null })} disabled={!moneyReady}>
                <PlusIcon data-icon="inline-start" />
                New plan
              </Button>
            )
          }
        >
          A plan sets what an account can use. Accounts are created on one.
        </EmptyState>
      ) : (
        <Section
          icon={LayersIcon}
          tint="purple"
          title={plural(list.length, "plan")}
          description={<PlansHelp />}
          contentClassName="overflow-x-auto"
        >
          <BTable
            caption="Plans"
            cols={[
              "ID",
              "Name",
              ...(pricing ? ["Price"] : []),
              "Limits",
              "Overage",
              "Resellable",
              ...(s.isAdmin ? [{ label: <span className="sr-only">Actions</span> }] : []),
            ]}
            rows={list.map((p) => ({
              key: p.id,
              cells: [
                <code key="id">{p.id}</code>,
                <div key="name" className="flex flex-col gap-0.5">
                  <span className="flex flex-wrap items-center gap-2 font-medium">
                    {p.name}
                    {p.public && <Badge variant="secondary">on the order page</Badge>}
                  </span>
                  {p.description && <span className="text-sm text-muted-foreground">{p.description}</span>}
                </div>,
                ...(pricing ? [<PriceCell key="price" plan={p} ready={moneyReady} />] : []),
                <span key="limits" className="text-sm">
                  {planSummary(p)}
                </span>,
                p.overage,
                p.resellable ? "yes" : "no",
                ...(s.isAdmin
                  ? [
                      <div key="actions" className="flex justify-end gap-1.5">
                        <Button variant="tinted" size="sm" onClick={() => setEditing({ plan: p })} disabled={!moneyReady}>
                          <PencilIcon data-icon="inline-start" />
                          Edit
                        </Button>
                        <ActionButton
                          variant="destructive"
                          size="sm"
                          run={async () => {
                            if (!(await ask(`Delete plan ${p.id}?`))) return
                            await api("DELETE", `/plans/${encodeURIComponent(p.id)}`)
                            await invalidate("/plans")
                            notify(`Plan ${p.id} deleted`)
                          }}
                        >
                          <Trash2Icon data-icon="inline-start" />
                          Delete
                        </ActionButton>
                      </div>,
                    ]
                  : []),
              ],
            }))}
          />
        </Section>
      )}

      {!list.length && !plans.isLoading && !plans.error && (
        <Section>
          <p className="text-sm text-muted-foreground">
            <PlansHelp />
          </p>
        </Section>
      )}

      {editing && (
        <PlanDialog
          key={editing.plan?.id ?? "new"}
          open
          onOpenChange={(o) => !o && setEditing(null)}
          plan={editing.plan}
          pricing={pricing}
        />
      )}
    </Page>
  )
}

function PriceCell({ plan, ready }: { plan: Plan; ready: boolean }) {
  if (!ready) return <Skeleton className="h-4 w-20" />
  const cs = planCycles(plan)
  if (!cs.length) return <span className="text-muted-foreground">free</span>
  return (
    <div className="flex flex-col gap-0.5 whitespace-nowrap">
      <span className="tabular-nums">{planPriceText(plan)}</span>
      {cs.length > 1 && <span className="text-xs text-muted-foreground">{cs.length} billing periods</span>}
    </div>
  )
}

function PlansHelp() {
  return (
    <>
      Limits per account (sites, staging copies included; disk: files and databases; bandwidth per calendar month, UTC) and per site. 0 means
      unlimited (the server's own limits still apply). Resellers can give their customers resellable plans that fit within their own.{" "}
      <em>Burst</em> lets sites get extra instances under load, up to the replicas per site; each minute a site runs above its normal size
      uses a burst minute. Minutes bought on top (an account's <em>Add burst minutes</em>, or{" "}
      <code>POST /api/v1/accounts/{"{id}"}/burst-credit</code>) never expire.
    </>
  )
}
