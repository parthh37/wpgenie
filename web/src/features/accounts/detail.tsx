import { useState, type FormEvent, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import {
  ActivityIcon, Building2Icon, ChartColumnIcon, ChevronLeftIcon, IdCardIcon, KeyRoundIcon, LayersIcon, LinkIcon,
  RulerIcon, SearchXIcon, Trash2Icon, UserPlusIcon, UsersIcon, ZapIcon,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, Banner, BTable, EmptyState, LoadError } from "@/components/app/blocks"
import { ask, askText, askTextCheck } from "@/components/app/confirm"
import { KeyValues } from "@/components/app/data-table"
import { Page, PageHeader, Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { StatusPill, StatusText, toneOf } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { AccountBilling } from "@/features/billing/shared/account-billing"
import { planSummary } from "@/features/plans/summary"
import type { Plan } from "@/features/plans/types"
import { api, ApiError } from "@/lib/api"
import { fmtNum, fmtTime } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { href, navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { accountState } from "./list"
import { accountPath, type Account, type AccountEvent, type AccountUser, type BurstBalance, type Usage } from "./types"
import { UsageBars } from "./usage"

// An account (#/accounts/<id>): its plan and limits, this month's usage,
// what can be done to it (suspend, terminate, delete, change plan, add
// burst minutes), its billing (staff), its customers (resellers), users
// and activity. A reseller's own account is read-only here.
export function AccountDetail({ id }: { id: string }) {
  const s = useSession()
  const path = accountPath(id)
  const acct = useApi<Account>(path)
  const a = acct.data

  if (acct.isLoading) {
    return (
      <Page>
        <BackLink />
        <Skeleton className="mb-6 h-12 w-72" />
        <Skeleton className="mb-4 h-40 w-full rounded-2xl" />
        <Skeleton className="h-64 w-full rounded-2xl" />
      </Page>
    )
  }
  if (acct.error || !a) {
    const missing = acct.error instanceof ApiError && acct.error.status === 404
    return (
      <Page>
        <BackLink />
        {missing ? (
          <EmptyState
            icon={SearchXIcon}
            title="No such account"
            actions={
              <Button variant="tinted" render={<a href={href("/accounts")} />} nativeButton={false}>
                All accounts
              </Button>
            }
          >
            Account {id} doesn't exist, or isn't one you can see.
          </EmptyState>
        ) : (
          <LoadError error={acct.error} retry={() => acct.refetch()} />
        )}
      </Page>
    )
  }
  return <Detail key={a.id} a={a} path={path} own={s.isTenant && s.me?.account_id === a.id} />
}

function BackLink() {
  return (
    <Button variant="link" className="mb-2 -ml-3 px-3" render={<a href={href("/accounts")} />} nativeButton={false}>
      <ChevronLeftIcon data-icon="inline-start" />
      Accounts
    </Button>
  )
}

// refresh refetches the account everywhere it shows (the list, this page,
// its usage, users and billing).
const refresh = () => invalidate("/accounts")

function Detail({ a, path, own }: { a: Account; path: string; own: boolean }) {
  const s = useSession()
  // Administrators manage every account; a reseller their customers.
  const canManage = s.isAdmin || (s.isReseller && !own)
  const usage = useApi<Usage>(`${path}/usage`)
  const events = useApi<AccountEvent[]>(`${path}/events?limit=20`)
  const burst = useQuery({
    queryKey: [`${path}/burst`],
    queryFn: () => api<BurstBalance>("GET", `${path}/burst`).catch(() => null),
  })
  const b = burst.data

  const actions: ReactNode[] = []
  if (canManage) {
    if (a.status === "active") {
      actions.push(
        <ActionButton
          key="suspend"
          variant="destructive"
          run={async () => {
            const ok = await ask(`Its sites show a "temporarily unavailable" page until it is unsuspended; nothing is deleted.`, {
              title: `Suspend ${a.name}?`,
              ok: "Suspend",
              danger: true,
            })
            if (!ok) return
            await api("POST", `${path}/suspend`, {})
            await refresh()
            notify(`${a.name} suspended`)
          }}
        >
          Suspend
        </ActionButton>
      )
    } else if (a.status === "suspended" || s.isAdmin) {
      const label = a.status === "terminated" ? "Reactivate" : "Unsuspend"
      actions.push(
        <ActionButton
          key="unsuspend"
          run={async () => {
            await api("POST", `${path}/unsuspend`)
            await refresh()
            notify(a.status === "terminated" ? `${a.name} reactivated` : `${a.name} unsuspended`)
          }}
        >
          {label}
        </ActionButton>
      )
    }
    actions.push(
      <ActionButton
        key="terminate"
        variant="destructive"
        run={async () => {
          const res = await askTextCheck("Its users can no longer sign in and its sites are suspended.", {
            title: `Terminate ${a.name}?`,
            label: `Type the account's ID (${a.id}) to confirm`,
            match: String(a.id),
            ok: "Terminate",
            check: "Also delete its sites (files and databases; their backups stay)",
          })
          if (!res || res.value !== String(a.id)) return
          await api("POST", `${path}/terminate`, { confirm: res.value, delete_sites: res.checked })
          await refresh()
          if (res.checked) invalidate("/sites")
          notify(`${a.name} terminated`)
        }}
      >
        Terminate…
      </ActionButton>
    )
    if (s.isAdmin && a.status === "terminated") {
      actions.push(
        <ActionButton
          key="delete"
          variant="destructive-solid"
          run={async () => {
            if (!(await ask("", { title: `Delete ${a.name} and its users for good?`, ok: "Delete", danger: true }))) return
            await api("DELETE", path)
            navigate("/accounts", { replace: true })
            await refresh()
            notify(`${a.name} deleted`)
          }}
        >
          <Trash2Icon data-icon="inline-start" />
          Delete account
        </ActionButton>
      )
    }
  }

  // Measuring is for operators and up, and for tenants (their own accounts).
  const canMeasure = s.isTenant || s.atLeast("operator")
  const usageActions = (
    <div className="flex flex-wrap gap-2">
      {s.isAdmin && b && b.allowed && !b.unlimited && (
        <ActionButton
          size="sm"
          run={async () => {
            const n = await askText(
              `${a.name} has ${fmtNum(b.remaining)} burst minutes left (${fmtNum(b.credit)} of them bought). ` +
                "Bought minutes never expire and are used once the month's are gone. A negative number takes minutes away.",
              { title: "Add burst minutes", label: "Minutes", type: "number", value: "600", ok: "Add", danger: false }
            )
            if (!n || !Number(n)) return
            const minutes = Math.trunc(Number(n))
            await api("POST", `${path}/burst-credit`, { minutes })
            await refresh()
            notify(`${fmtNum(minutes)} burst minutes added to ${a.name}`)
          }}
        >
          <ZapIcon data-icon="inline-start" />
          Add burst minutes…
        </ActionButton>
      )}
      {canMeasure && (
        <ActionButton
          size="sm"
          run={async () => {
            await api("POST", `${path}/usage/measure`)
            await refresh()
            notify("Disk measured")
          }}
        >
          <RulerIcon data-icon="inline-start" />
          Measure disk now
        </ActionButton>
      )}
    </div>
  )

  const statusWords = a.status + (a.suspend_reason ? ` · ${a.suspend_reason}` : "")

  return (
    <Page>
      <BackLink />
      <PageHeader
        icon={Building2Icon}
        tint="cyan"
        title={
          <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
            {a.name}
            <StatusPill status={a.status} tone={a.effectively_suspended ? "bad" : toneOf(a.status)} className="text-sm">
              {statusWords}
            </StatusPill>
          </span>
        }
        description={`Account #${a.id}, ${a.kind}` + (a.parent_name ? `, a customer of ${a.parent_name}` : "") + (own ? " (yours)" : "")}
        actions={actions.length ? actions : undefined}
      />

      {a.status === "terminated" ? (
        <Banner tone="bad" title="Terminated">
          Its users can no longer sign in and its sites are suspended.
        </Banner>
      ) : a.effectively_suspended ? (
        <Banner tone="bad" title={a.status === "active" ? "Suspended through its reseller" : "Suspended"}>
          Its sites show a "temporarily unavailable" page and nothing can be changed until it is unsuspended; nothing is deleted.
        </Banner>
      ) : null}

      <div className="grid gap-x-4 md:grid-cols-2">
        <Section icon={LayersIcon} tint="purple" title={`Plan ${a.plan.name}`} description={planSummary(a.limits)}>
          {canManage ? <PlanPicker a={a} path={path} /> : <p className="text-sm text-muted-foreground">Plan ID: {a.plan_id}</p>}
        </Section>
        <Section icon={IdCardIcon} tint="gray" title="Details">
          <KeyValues
            items={[
              ["E-mail", a.email || "–"],
              ["Reseller", a.parent_id ? <a href={href(accountPath(a.parent_id))}>{a.parent_name || `#${a.parent_id}`}</a> : "–"],
              ["WHMCS service", a.whmcs_service_id || "–"],
              ["Stripe customer", a.stripe_customer_id || "–"],
              ["Created", fmtTime(a.created_at)],
            ]}
          />
        </Section>
      </div>

      <Section icon={ChartColumnIcon} tint="blue" title="Usage" action={usageActions}>
        {usage.isLoading ? (
          <Skeleton className="h-28 w-full" />
        ) : usage.error ? (
          <LoadError error={usage.error} retry={() => usage.refetch()} className="py-6" />
        ) : usage.data ? (
          <UsageBars usage={usage.data} burst={b} />
        ) : null}
      </Section>

      {/* Staff bill accounts here; resellers bill their customers elsewhere. */}
      {!s.isTenant && <AccountBilling account={a} />}

      {a.kind === "reseller" && <Customers a={a} />}

      <Users a={a} path={path} canManage={canManage} />

      <Section icon={ActivityIcon} tint="red" title="Activity">
        {events.isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : events.error ? (
          <LoadError error={events.error} retry={() => events.refetch()} className="py-6" />
        ) : events.data?.length ? (
          <ol className="flex flex-col">
            {events.data.map((ev) => (
              <li key={ev.id} className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 border-b border-border/60 py-2 text-sm last:border-0">
                <time dateTime={ev.time} className="w-36 shrink-0 text-muted-foreground tabular-nums">
                  {fmtTime(ev.time)}
                </time>
                <Badge variant="secondary">{ev.kind}</Badge>
                <span className="min-w-0 flex-1 [overflow-wrap:anywhere]">{ev.message}</span>
              </li>
            ))}
          </ol>
        ) : (
          <p className="text-sm text-muted-foreground">Nothing yet.</p>
        )}
      </Section>
    </Page>
  )
}

// PlanPicker moves the account to another plan (a reseller: one they may
// hand out, or the one it's on).
function PlanPicker({ a, path }: { a: Account; path: string }) {
  const s = useSession()
  const plans = useApi<Plan[]>("/plans")
  const [plan, setPlan] = useState(a.plan_id)
  if (plans.isLoading) return <Skeleton className="h-9 w-full" />
  if (plans.error) return <LoadError error={plans.error} retry={() => plans.refetch()} className="py-6" />
  const offered = (plans.data ?? []).filter((p) => !s.isTenant || p.resellable || p.id === a.plan_id)
  return (
    <div className="flex flex-wrap items-end gap-2">
      <div className="flex min-w-48 flex-1 flex-col gap-2">
        <Label htmlFor="acct-plan-picker">Plan</Label>
        <NativeSelect id="acct-plan-picker" value={plan} onChange={(e) => setPlan(e.target.value)} className="w-full">
          {offered.map((p) => (
            <NativeSelectOption key={p.id} value={p.id}>
              {p.name} ({p.id})
            </NativeSelectOption>
          ))}
        </NativeSelect>
      </div>
      <ActionButton
        disabled={plan === a.plan_id}
        run={async () => {
          await api("PUT", path, { plan_id: plan })
          await refresh()
          notify(`${a.name} is now on ${offered.find((p) => p.id === plan)?.name ?? plan}`)
        }}
      >
        Change plan
      </ActionButton>
    </div>
  )
}

// Customers of a reseller account.
function Customers({ a }: { a: Account }) {
  const list = useApi<Account[]>(`/accounts?parent=${a.id}`)
  return (
    <Section icon={UsersIcon} tint="cyan" title="Customers" description="The accounts this reseller looks after." contentClassName="overflow-x-auto">
      {list.isLoading ? (
        <Skeleton className="h-16 w-full" />
      ) : list.error ? (
        <LoadError error={list.error} retry={() => list.refetch()} className="py-6" />
      ) : (
        <BTable
          caption="Customers"
          empty={<p className="text-sm text-muted-foreground">No customers yet.</p>}
          cols={["Name", "Status", "Plan", { label: "Sites", num: true }]}
          rows={(list.data ?? [])
            .filter((c) => c.id !== a.id)
            .map((c) => ({
              key: c.id,
              onOpen: () => navigate(accountPath(c.id)),
              cells: [
                <a key="name" href={href(accountPath(c.id))} className="font-medium text-foreground">
                  {c.name}
                </a>,
                <StatusText key="status" status={c.effectively_suspended ? "suspended" : c.status}>
                  {accountState(c)}
                </StatusText>,
                c.plan_id,
                fmtNum(c.sites),
              ],
            }))}
        />
      )}
    </Section>
  )
}

// Users of the account: who signs in to it. Staff and resellers see them;
// those who manage the account reset passwords, make one-time sign-in
// links (for a billing portal or support), disable and add users.
function Users({ a, path, canManage }: { a: Account; path: string; canManage: boolean }) {
  const s = useSession()
  const visible = s.isReseller || !s.isTenant
  const users = useQuery({
    queryKey: [`${path}/users`],
    queryFn: () => api<AccountUser[]>("GET", `${path}/users`).catch(() => [] as AccountUser[]),
    enabled: visible,
  })
  const list = users.data ?? []
  if (!visible || (!list.length && !canManage && !users.isLoading)) return null
  const addable = canManage && a.status !== "terminated"

  return (
    <Section icon={UsersIcon} tint="indigo" title="Users" contentClassName="flex flex-col gap-4 overflow-x-auto">
      {users.isLoading ? (
        <Skeleton className="h-16 w-full" />
      ) : list.length ? (
        <BTable
          caption="Users"
          cols={["User", "Role", "2FA", "State", ...(canManage ? [{ label: <span className="sr-only">Actions</span> }] : [])]}
          rows={list.map((u) => ({
            key: u.id,
            cells: [
              u.username,
              u.role,
              u.totp_enabled ? <StatusText key="2fa" status="on" /> : <span key="2fa" className="text-muted-foreground">off</span>,
              <StatusText key="state" status={u.disabled ? "disabled" : "active"} />,
              ...(canManage ? [<UserActions key="actions" a={a} path={path} u={u} />] : []),
            ],
          }))}
        />
      ) : (
        <p className="text-sm text-muted-foreground">No users yet: add one so someone can sign in to this account.</p>
      )}
      {addable && <AddUser path={path} />}
    </Section>
  )
}

function UserActions({ a, path, u }: { a: Account; path: string; u: AccountUser }) {
  const userPath = `${path}/users/${u.id}`
  return (
    <div className="flex flex-wrap justify-end gap-1.5">
      <ActionButton
        size="sm"
        run={async () => {
          const ok = await ask(`They get a new password (shown once), are signed out everywhere and their API tokens stop working.`, {
            title: `Reset the password for ${u.username}?`,
            ok: "Reset password",
          })
          if (!ok) return
          const r = await api<{ password: string }>("POST", `${userPath}/password`, {})
          showSecret(`New password for ${u.username}`, [r.password, "", "They were signed out everywhere."])
        }}
      >
        <KeyRoundIcon data-icon="inline-start" />
        Reset password
      </ActionButton>
      <ActionButton
        size="sm"
        disabled={u.disabled || a.status === "terminated"}
        title={u.disabled ? "Enable the user first" : undefined}
        run={async () => {
          const r = await api<{ url: string }>("POST", `${path}/sso`, { username: u.username })
          showSecret(`One-time sign-in link for ${u.username} (2 minutes)`, [r.url])
        }}
      >
        <LinkIcon data-icon="inline-start" />
        Sign-in link
      </ActionButton>
      <ActionButton
        size="sm"
        variant={u.disabled ? "tinted" : "destructive"}
        run={async () => {
          if (!u.disabled) {
            const ok = await ask("They are signed out everywhere and can't sign in until enabled again.", {
              title: `Disable ${u.username}?`,
              ok: "Disable",
            })
            if (!ok) return
          }
          await api("PUT", userPath, { disabled: !u.disabled })
          await invalidate(`${path}/users`)
          notify(u.disabled ? `${u.username} enabled` : `${u.username} disabled`)
        }}
      >
        {u.disabled ? "Enable" : "Disable"}
      </ActionButton>
    </div>
  )
}

function AddUser({ path }: { path: string }) {
  const [name, setName] = useState("")
  const [busy, setBusy] = useState(false)
  async function submit(e: FormEvent) {
    e.preventDefault()
    const username = name.trim()
    if (!username) return
    setBusy(true)
    try {
      const r = await api<{ user: AccountUser; password?: string }>("POST", `${path}/users`, { username })
      setName("")
      if (r.password) showSecret(`User ${r.user.username}`, [`Password: ${r.password}`])
      await invalidate(`${path}/users`)
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="flex flex-wrap items-center gap-2">
      <Label htmlFor="acct-new-user" className="sr-only">
        New user
      </Label>
      <Input
        id="acct-new-user"
        name="username"
        required
        placeholder="new user"
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        value={name}
        onChange={(e) => setName(e.target.value)}
        className="w-full sm:w-56"
      />
      <Button type="submit" variant="tinted" disabled={busy}>
        <UserPlusIcon data-icon="inline-start" />
        Add user
      </Button>
    </form>
  )
}
