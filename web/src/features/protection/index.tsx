import { useState, type FormEvent, type ReactNode } from "react"
import { BanIcon, EyeIcon, HistoryIcon, ListIcon, RefreshCwIcon, ShieldIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, Kpi, LoadError } from "@/components/app/blocks"
import { Page, PageHeader, Section } from "@/components/app/page"
import { StatusText } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtAgo, fmtNum, fmtTime, humanize, splitList, sum } from "@/lib/format"
import { invalidate, queryClient, useApi, useSites } from "@/lib/query"
import { useSession } from "@/lib/session"

// The Protection page (#/security): what protects every site on the
// server at once. IP reputation lists, the server-wide allow and block
// lists, banned addresses and the latest blocks.

interface Feed {
  name: string
  title: string
  entries: number
  updated_at?: string
  error?: string
}

interface GeoStatus {
  loaded: boolean
  month?: string
  ranges: number
  updated_at?: string
  error?: string
  attribution: string
}

interface Reputation {
  lists: Feed[]
  countries: GeoStatus | null
  waf_available: boolean
}

interface GlobalLists {
  allow: string[] | null
  deny: string[] | null
}

interface Ban {
  addr: string
  until: string
  reason: string
  manual: boolean
}

interface ShieldEvent {
  time: string
  site: string
  ip: string
  verdict: string
  reason: string
  path: string
}

const BANS = "/security/bans"
const EVENTS = "/security/events?limit=100"
const REPUTATION = "/security/reputation"
const SETTINGS = "/security/settings"

export default function ProtectionPage() {
  const bans = useApi<Ban[]>(BANS, { refetchInterval: 30_000 })
  const events = useApi<ShieldEvent[]>(EVENTS, { refetchInterval: 30_000 })
  const rep = useApi<Reputation>(REPUTATION)
  const lists = useApi<GlobalLists>(SETTINGS)

  return (
    <Page>
      <PageHeader icon={ShieldIcon} tint="green" title="Protection" description="Rules that protect every site on this server at once." />

      <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Kpi icon={BanIcon} label="Banned now" value={bans.data ? fmtNum(bans.data.length) : "–"} sub="on every site" />
        <Kpi
          icon={EyeIcon}
          label="Blocklist entries"
          value={rep.data ? fmtNum(sum(rep.data.lists.map((l) => l.entries))) : "–"}
          tone={rep.data?.lists.some((l) => l.error) ? "warn" : undefined}
          sub={rep.data ? (rep.data.lists.some((l) => l.error) ? "a list didn't refresh" : `in ${fmtNum(rep.data.lists.length)} reputation lists`) : undefined}
        />
        <Kpi
          icon={ListIcon}
          label="Server-wide lists"
          value={lists.data ? `${fmtNum(lists.data.allow?.length ?? 0)} / ${fmtNum(lists.data.deny?.length ?? 0)}` : "–"}
          sub="allowed / blocked"
        />
        <Kpi
          icon={HistoryIcon}
          label="Latest block"
          value={events.data ? (events.data[0] ? fmtAgo(events.data[0].time) : "none") : "–"}
          sub={events.data?.[0] ? humanize(events.data[0].verdict) : undefined}
        />
      </div>

      <ReputationCard q={rep} />
      <ListsCard q={lists} />
      <BansCard q={bans} />
      <EventsCard q={events} />
    </Page>
  )
}

type Q<T> = ReturnType<typeof useApi<T>>

function Loading<T>({ q, children }: { q: Q<T>; children: (data: T) => ReactNode }) {
  if (q.isLoading) return <Skeleton className="h-24 rounded-xl" />
  if (q.error && !q.data) return <LoadError error={q.error} retry={() => q.refetch()} />
  return q.data !== undefined ? <>{children(q.data)}</> : null
}

// ---- IP reputation ----

function ReputationCard({ q }: { q: Q<Reputation> }) {
  const s = useSession()
  const [refreshing, setRefreshing] = useState(false)

  async function refresh() {
    setRefreshing(true)
    try {
      queryClient.setQueryData([REPUTATION], await api<Reputation>("POST", "/security/reputation/refresh"))
      notify("Reputation lists refreshed")
    } catch (e) {
      showError(e)
    } finally {
      setRefreshing(false)
    }
  }

  return (
    <Section
      icon={EyeIcon}
      tint="green"
      title="IP reputation"
      description="Public blocklists of hijacked networks and addresses attacking servers right now, refreshed every 6 hours. Each site chooses whether listed clients are challenged or blocked (Site → Protection). Verified search engines are never affected."
      action={
        s.isAdmin && (
          <Button variant="tinted" size="sm" onClick={refresh} disabled={refreshing}>
            <RefreshCwIcon data-icon="inline-start" className={refreshing ? "animate-spin" : undefined} />
            {refreshing ? "Refreshing…" : "Refresh now"}
          </Button>
        )
      }
    >
      <Loading q={q}>
        {(rep) => {
          const c = rep.countries
          return (
            <>
              <BTable
                caption="IP reputation lists"
                cols={["List", { label: "Entries", num: true }, "Updated", ""]}
                empty={<p className="py-2 text-sm text-muted-foreground">No reputation lists on this server.</p>}
                rows={rep.lists.map((l) => ({
                  key: l.name,
                  cells: [
                    <span className="font-medium">{l.title}</span>,
                    fmtNum(l.entries),
                    l.updated_at ? fmtTime(l.updated_at) : "never",
                    l.error ? <span className="block max-w-md text-xs whitespace-normal text-danger">{l.error}</span> : "",
                  ],
                }))}
              />
              <div className="mt-4 flex flex-col gap-2 text-sm">
                <p>
                  <span className="text-muted-foreground">Country database: </span>
                  {c && c.loaded ? `${c.month} release, ${fmtNum(c.ranges)} ranges` : "downloaded when a site first uses country rules"}
                  {c?.error && <span className="text-danger"> ({c.error})</span>}
                  {c && <span className="text-muted-foreground">. {c.attribution}</span>}
                </p>
                <p>
                  <span className="text-muted-foreground">Request-body inspection (Coraza + OWASP CRS): </span>
                  {rep.waf_available ? (
                    <span className="text-success">available</span>
                  ) : (
                    <span className="text-warning">not detected yet: it is checked when a site turns it on; Caddy needs the wpgenie/caddy image</span>
                  )}
                </p>
              </div>
            </>
          )
        }}
      </Loading>
    </Section>
  )
}

// ---- Server-wide lists ----

function ListsCard({ q }: { q: Q<GlobalLists> }) {
  const s = useSession()
  return (
    <Section
      icon={ListIcon}
      tint="green"
      title="Server-wide lists"
      description="Apply to every site. Allowed addresses are never challenged, blocked or banned (your office, monitoring); blocked ones get nothing from any site."
    >
      <Loading q={q}>
        {(lists) =>
          s.isAdmin ? (
            // Fresh fields whenever the saved lists change.
            <ListsForm key={JSON.stringify(lists)} lists={lists} />
          ) : (
            <dl className="grid gap-3 text-sm sm:grid-cols-2">
              <div>
                <dt className="text-muted-foreground">Always allow</dt>
                <dd className="font-mono text-xs break-all">{lists.allow?.join(", ") || "–"}</dd>
              </div>
              <div>
                <dt className="text-muted-foreground">Block everywhere</dt>
                <dd className="font-mono text-xs break-all">{lists.deny?.join(", ") || "–"}</dd>
              </div>
            </dl>
          )
        }
      </Loading>
    </Section>
  )
}

function ListsForm({ lists }: { lists: GlobalLists }) {
  const [busy, setBusy] = useState(false)
  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const data = new FormData(e.currentTarget)
    setBusy(true)
    try {
      await api("PUT", SETTINGS, { allow: splitList(String(data.get("allow") ?? "")), deny: splitList(String(data.get("deny") ?? "")) })
      notify("Lists saved: every site uses them now")
      await invalidate("/security")
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={save}>
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="sec-allow">Always allow</FieldLabel>
          <Input id="sec-allow" name="allow" defaultValue={(lists.allow ?? []).join(", ")} placeholder="203.0.113.7, 198.51.100.0/24" autoComplete="off" />
          <FieldDescription>Addresses or ranges, separated by commas.</FieldDescription>
        </Field>
        <Field>
          <FieldLabel htmlFor="sec-deny">Block everywhere</FieldLabel>
          <Input id="sec-deny" name="deny" defaultValue={(lists.deny ?? []).join(", ")} placeholder="192.0.2.0/24" autoComplete="off" />
          <FieldDescription>Addresses or ranges, separated by commas.</FieldDescription>
        </Field>
      </FieldGroup>
      <div className="mt-4 flex justify-end">
        <Button type="submit" disabled={busy}>
          Save lists
        </Button>
      </div>
    </form>
  )
}

// ---- Banned addresses ----

function BansCard({ q }: { q: Q<Ban[]> }) {
  const s = useSession()
  const can = s.atLeast("operator")
  const [busy, setBusy] = useState(false)

  async function ban(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const form = e.currentTarget
    const data = new FormData(form)
    const addr = String(data.get("addr") ?? "").trim()
    setBusy(true)
    try {
      await api("POST", BANS, { addr, hours: Number(String(data.get("hours") ?? "").trim() || 24) })
      form.reset()
      notify(`${addr} is banned on every site`)
      await invalidate("/security")
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  async function unban(b: Ban) {
    await api("DELETE", `${BANS}?addr=${encodeURIComponent(b.addr)}`)
    notify(`${b.addr} is no longer banned`)
    await invalidate("/security")
  }

  return (
    <Section
      icon={BanIcon}
      tint="red"
      title="Banned addresses"
      description="Clients that send attacks (SQL injection, scanners, brute force) are banned on every site automatically: 1 hour, doubling for repeat offenders."
    >
      {can && (
        <form onSubmit={ban} className="mb-4 flex flex-wrap gap-2">
          <Input name="addr" placeholder="IP address" aria-label="IP address" required autoComplete="off" className="w-56 max-sm:w-full" />
          <Input name="hours" type="number" min={1} placeholder="Hours (default 24)" aria-label="Hours (default 24)" className="w-44 max-sm:w-full" />
          <Button type="submit" variant="destructive" disabled={busy}>
            <BanIcon data-icon="inline-start" />
            Ban
          </Button>
        </form>
      )}
      <Loading q={q}>
        {(bans) => (
          <BTable
            caption="Banned addresses"
            cols={["Address", "Until", "Reason", ""]}
            empty={<p className="py-2 text-sm text-muted-foreground">No address is banned right now.</p>}
            rows={bans.map((b) => ({
              key: b.addr,
              cells: [
                <span className="font-mono text-xs">{b.addr}</span>,
                fmtTime(b.until),
                <span className="block max-w-md whitespace-normal">
                  {b.reason}
                  {b.manual && <span className="text-muted-foreground"> (manual)</span>}
                </span>,
                can ? (
                  <div className="flex justify-end">
                    <ActionButton size="sm" run={() => unban(b)}>
                      Unban
                    </ActionButton>
                  </div>
                ) : null,
              ],
            }))}
          />
        )}
      </Loading>
    </Section>
  )
}

// ---- Recent blocks ----

function EventsCard({ q }: { q: Q<ShieldEvent[]> }) {
  const { data: sites } = useSites()
  const siteName = (id: string) => sites?.find((x) => x.id === id)?.primary_domain ?? id
  return (
    <Section icon={HistoryIcon} tint="gray" title="Recent blocks">
      <Loading q={q}>
        {(events) => (
          <BTable
            caption="Recent blocks"
            cols={["Time", "Site", "Client", "Action", "Reason", "Path"]}
            empty={<p className="py-2 text-sm text-muted-foreground">Nothing blocked lately.</p>}
            rows={events.map((e, i) => ({
              key: `${e.time}-${i}`,
              cells: [
                fmtTime(e.time),
                e.site ? siteName(e.site) : "–",
                <span className="font-mono text-xs">{e.ip || "–"}</span>,
                <StatusText status={e.verdict === "ban" ? "failed" : e.verdict}>{humanize(e.verdict)}</StatusText>,
                <span className="block max-w-xs whitespace-normal">{e.reason}</span>,
                <span className="block max-w-xs font-mono text-xs break-all whitespace-normal">{e.path}</span>,
              ],
            }))}
          />
        )}
      </Loading>
    </Section>
  )
}
