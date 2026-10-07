import { useState, type FormEvent } from "react"
import { PlusIcon, SendIcon, WebhookIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { StatusPill } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { useApi } from "@/lib/query"
import { cn } from "@/lib/utils"
import { CheckList } from "./common"
import { STRIPE, type StripeSettings } from "./payments"

// Outgoing webhooks: endpoints that hear about accounts, plans, invoices,
// usage and sites, and the deliveries made to them.

interface Endpoint {
  id: number
  url: string
  events: string[] | null
  enabled: boolean
  created_at?: string
}
interface Delivery {
  id: number
  endpoint_id: number
  event: string
  status: string
  attempts: number
  last_status?: number
  last_error?: string
  created_at: string
}

const WEBHOOK_EVENTS = [
  "account.created", "account.suspended", "account.unsuspended", "account.terminated", "account.payment_failed",
  "plan.changed", "usage.threshold", "burst.threshold", "site.created", "site.deleted",
]

const DELIVERY_TONE: Record<string, string> = { delivered: "text-success", failed: "text-danger", pending: "text-warning" }

export function Webhooks() {
  // The events come with Stripe's settings; without them, the usual list.
  const settings = useApi<StripeSettings>(STRIPE)
  const hooks = useApi<Endpoint[]>("/billing/webhooks")
  const deliveries = useApi<Delivery[]>("/billing/deliveries?limit=50")
  const [url, setUrl] = useState("")
  const [events, setEvents] = useState<string[]>([])
  const [adding, setAdding] = useState(false)

  const err = hooks.error || deliveries.error
  const reload = () => Promise.all([hooks.refetch(), deliveries.refetch()])
  if (err) return <LoadError error={err} retry={() => void reload()} />
  if (!hooks.data || !deliveries.data || (settings.isLoading && !settings.error)) return <Skeleton className="h-64 rounded-2xl" />
  const all = settings.data?.events || WEBHOOK_EVENTS

  async function add(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!e.currentTarget.reportValidity()) return
    setAdding(true)
    try {
      const r = await api<{ secret: string }>("POST", "/billing/webhooks", { url: url.trim(), events })
      showSecret("Webhook signing secret", [r.secret, "", "Verify X-WPGenie-Signature with it on every delivery."])
      setUrl("")
      setEvents([])
      await reload()
    } catch (ex) {
      showError(ex)
    } finally {
      setAdding(false)
    }
  }
  // Every action reloads the lists afterwards.
  const act = (fn: () => Promise<unknown>) => async () => {
    await fn()
    await reload()
  }

  return (
    <>
      <Section
        icon={WebhookIcon}
        tint="purple"
        title="Webhooks"
        description={
          <>
            Events (accounts, plans, invoices, usage, sites) are posted as JSON, signed with{" "}
            <code>X-WPGenie-Signature: t=…,v1=HMAC-SHA256(secret, "t.body")</code>, and retried with backoff for about a day and a half.
          </>
        }
        contentClassName="flex flex-col gap-5"
      >
        <form onSubmit={add} className="flex flex-col gap-4">
          <div className="flex flex-wrap gap-2">
            <Input
              name="url"
              type="url"
              required
              placeholder="https://billing.example.com/wpgenie"
              aria-label="Endpoint URL"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              className="min-w-0 flex-1 basis-64"
            />
            <Button type="submit" disabled={adding}>
              <PlusIcon data-icon="inline-start" />
              Add endpoint
            </Button>
          </div>
          <fieldset className="flex flex-col">
            <legend className="mb-2.5 text-sm font-medium">Events (none ticked: all)</legend>
            <CheckList name="event" small items={all.map((ev) => [ev, <code className="bg-transparent p-0">{ev}</code>])} selected={events} onChange={setEvents} />
          </fieldset>
        </form>
        <BTable
          caption="Webhook endpoints"
          cols={["URL", "Events", "State", ""]}
          rows={hooks.data.map((ep) => ({
            key: ep.id,
            cells: [
              <span className="block min-w-48 break-all">{ep.url}</span>,
              <span className="text-xs">{ep.events?.length ? ep.events.join(", ") : "all"}</span>,
              <StatusPill status={ep.enabled ? "on" : "off"} tone={ep.enabled ? "ok" : "neutral"}>
                {ep.enabled ? "enabled" : "disabled"}
              </StatusPill>,
              <div className="flex flex-wrap justify-end gap-1.5">
                <ActionButton
                  size="sm"
                  run={act(async () => {
                    await api("POST", `/billing/webhooks/${ep.id}/test`)
                    notify("Test event sent")
                  })}
                >
                  <SendIcon data-icon="inline-start" />
                  Test
                </ActionButton>
                <ActionButton size="sm" run={act(() => api("PUT", `/billing/webhooks/${ep.id}`, { enabled: !ep.enabled }))}>
                  {ep.enabled ? "Disable" : "Enable"}
                </ActionButton>
                <ActionButton
                  size="sm"
                  run={act(async () => {
                    if (!(await ask("Issue a new signing secret? The old one stops working at once.", { ok: "New secret", danger: true }))) return
                    const r = await api<{ secret: string }>("PUT", `/billing/webhooks/${ep.id}`, { rotate_secret: true })
                    showSecret("Webhook signing secret", [r.secret])
                  })}
                >
                  New secret
                </ActionButton>
                <ActionButton
                  size="sm"
                  variant="destructive"
                  run={act(async () => {
                    if (await ask(`Delete ${ep.url}?`)) await api("DELETE", `/billing/webhooks/${ep.id}`)
                  })}
                >
                  Delete
                </ActionButton>
              </div>,
            ],
          }))}
          empty={<p className="text-sm text-muted-foreground">No endpoints yet.</p>}
        />
      </Section>
      <Section icon={SendIcon} tint="gray" title="Deliveries">
        <BTable
          caption="Deliveries"
          cols={["Time", "Event", "Endpoint", "State", { label: "Attempts", num: true }, "Last answer", ""]}
          rows={deliveries.data.map((d) => ({
            key: d.id,
            cells: [
              <span className="whitespace-nowrap">{fmtTime(d.created_at)}</span>,
              <code className="bg-transparent p-0 text-xs whitespace-nowrap">{d.event}</code>,
              String(d.endpoint_id),
              <span className={cn("whitespace-nowrap", DELIVERY_TONE[d.status] || "text-muted-foreground")}>{d.status.replace(/_/g, " ")}</span>,
              String(d.attempts),
              <span className="block min-w-40 text-xs break-words">{d.last_error || (d.last_status ? String(d.last_status) : "")}</span>,
              d.status === "delivered" ? null : (
                <ActionButton size="sm" run={act(() => api("POST", `/billing/deliveries/${d.id}/retry`))}>
                  Retry
                </ActionButton>
              ),
            ],
          }))}
          empty={<p className="text-sm text-muted-foreground">Nothing sent yet.</p>}
        />
      </Section>
    </>
  )
}
