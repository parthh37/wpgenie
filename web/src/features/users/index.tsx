import { useState, type FormEvent } from "react"
import type { UseQueryResult } from "@tanstack/react-query"
import { HistoryIcon, ListIcon, PlusIcon, SearchIcon, ShieldCheckIcon, UsersIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ActionButton, BTable, FormDialog, LoadError } from "@/components/app/blocks"
import { ask, askText } from "@/components/app/confirm"
import { Page, PageHeader, Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { invalidate, queryClient, useApi } from "@/lib/query"
import { navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { User } from "@/lib/types"
import { cn } from "@/lib/utils"
import { ROLE_NAMES } from "@/features/account/roles"
import { SessionsTable, type SessionRow } from "@/features/account/sessions"

// Users (administrators): who can sign in to this panel, what they may do,
// their sessions, and the audit log.

const STAFF_ROLES = ["viewer", "operator", "admin"] as const

interface AuditEntry {
  id: number
  time: string
  actor: string
  ip: string
  action: string
  target?: string
  status?: number
  detail?: string
}

export default function UsersPage() {
  const users = useApi<User[]>("/users")
  const sessions = useApi<SessionRow[]>("/sessions")
  const settings = useApi<{ require_2fa: boolean }>("/settings/auth")
  const [adding, setAdding] = useState(false)

  return (
    <Page>
      <PageHeader
        icon={UsersIcon}
        tint="yellow"
        title="Users"
        description="Who can sign in to this panel, what they may do, and what they did."
        actions={
          <Button onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add user
          </Button>
        }
      />

      <Section
        icon={UsersIcon}
        tint="yellow"
        title="Panel users"
        description={
          <>
            <strong className="text-foreground">Viewers</strong> see everything and change nothing.{" "}
            <strong className="text-foreground">Operators</strong> run sites: protection, scaling, caches, updates, scans, mailboxes.{" "}
            <strong className="text-foreground">Administrators</strong> also create and delete sites, manage users, server-wide security and
            mail settings, and update WPGenie.
          </>
        }
      >
        {users.isLoading && <Skeleton className="h-40 rounded-xl" />}
        {users.error && <LoadError error={users.error} retry={() => users.refetch()} />}
        {users.data && <UsersTable users={users.data} />}
        <Require2FA settings={settings} />
      </Section>

      <Section icon={HistoryIcon} tint="indigo" title="Signed-in sessions">
        {sessions.isLoading && <Skeleton className="h-28 rounded-xl" />}
        {sessions.error && <LoadError error={sessions.error} retry={() => sessions.refetch()} />}
        {sessions.data && <SessionsTable sessions={sessions.data} withUser onChanged={() => invalidate("/sessions")} />}
      </Section>

      <AuditLog />

      <AddUserDialog open={adding} onOpenChange={setAdding} />
    </Page>
  )
}

// ---- Users ----

function UsersTable({ users }: { users: User[] }) {
  const s = useSession()
  const reload = () => Promise.all([invalidate("/users"), invalidate("/sessions"), invalidate("/audit")])
  return (
    <BTable
      caption="Panel users"
      empty={<p className="py-2 text-sm text-muted-foreground">No users yet.</p>}
      cols={["User", "Role", "2FA", "Last sign-in", { label: <span className="sr-only">Actions</span> }]}
      rows={users.map((u) => {
        const self = s.me?.id === u.id
        return {
          key: u.id,
          cells: [
            <span className="inline-flex flex-wrap items-center gap-1.5">
              <span className="font-medium">{u.username}</span>
              {u.disabled && <Badge className="bg-danger-fill/16 text-danger" variant="secondary">disabled</Badge>}
              {self && <span className="text-xs text-muted-foreground">(you)</span>}
            </span>,
            u.account_id ? (
              // A tenant's role follows their account's kind.
              <span className="inline-flex flex-wrap items-center gap-1.5 text-sm">
                {ROLE_NAMES[u.role] ?? u.role}
                <Badge variant="secondary">account #{u.account_id}</Badge>
              </span>
            ) : (
              <RoleSelect key={`${u.id}-${u.role}`} user={u} onChanged={reload} />
            ),
            u.totp_enabled ? <span className="font-medium text-success">on</span> : <span className="font-medium text-warning">off</span>,
            u.last_login_at ? fmtTime(u.last_login_at) : "never",
            <div className="flex flex-wrap justify-end gap-1.5">
              <ActionButton
                size="sm"
                run={async () => {
                  await api("PUT", `/users/${u.id}`, { disabled: !u.disabled })
                  notify(u.disabled ? `${u.username} can sign in again` : `${u.username} is disabled`)
                  await reload()
                }}
              >
                {u.disabled ? "Enable" : "Disable"}
              </ActionButton>
              <ActionButton
                size="sm"
                run={async () => {
                  if (!(await ask(`Give ${u.username} a new password? They are signed out everywhere.`, { ok: "New password" }))) return
                  const r = await api<{ password: string }>("POST", `/users/${u.id}/password`)
                  showSecret(`New password for ${u.username}`, [`Password: ${r.password}`])
                  await invalidate("/sessions")
                }}
              >
                Reset password
              </ActionButton>
              {u.totp_enabled && (
                <ActionButton
                  size="sm"
                  run={async () => {
                    if (
                      !(await ask(`Turn off two-factor authentication for ${u.username} (lost phone)? They are signed out everywhere.`, {
                        ok: "Reset 2FA",
                        danger: true,
                      }))
                    )
                      return
                    await api("DELETE", `/users/${u.id}/totp`)
                    await reload()
                  }}
                >
                  Reset 2FA
                </ActionButton>
              )}
              {!self && (
                <ActionButton
                  size="sm"
                  variant="destructive"
                  run={async () => {
                    const typed = await askText("They are signed out everywhere and can no longer sign in.", {
                      title: `Delete ${u.username}?`,
                      label: "Type the username to confirm",
                      match: u.username,
                      ok: "Delete user",
                    })
                    if (typed !== u.username) return
                    await api("DELETE", `/users/${u.id}`)
                    notify(`${u.username} deleted`)
                    await reload()
                  }}
                >
                  Delete
                </ActionButton>
              )}
            </div>,
          ],
        }
      })}
    />
  )
}

// RoleSelect changes a staff user's role at once; a refused change goes
// back to what it was.
function RoleSelect({ user, onChanged }: { user: User; onChanged: () => unknown }) {
  const [value, setValue] = useState<string>(user.role)
  const [busy, setBusy] = useState(false)
  return (
    <NativeSelect
      size="sm"
      aria-label={`Role of ${user.username}`}
      value={value}
      disabled={busy}
      onChange={async (e) => {
        const role = e.target.value
        setValue(role)
        setBusy(true)
        try {
          await api("PUT", `/users/${user.id}`, { role })
          notify(`${user.username} is now ${(ROLE_NAMES[role] ?? role).toLowerCase()}`)
          await onChanged()
        } catch (err) {
          showError(err)
          setValue(user.role)
        } finally {
          setBusy(false)
        }
      }}
    >
      {STAFF_ROLES.map((r) => (
        <NativeSelectOption key={r} value={r}>
          {ROLE_NAMES[r]}
        </NativeSelectOption>
      ))}
    </NativeSelect>
  )
}

function Require2FA({ settings }: { settings: UseQueryResult<{ require_2fa: boolean }> }) {
  const s = useSession()
  const [busy, setBusy] = useState(false)
  const on = !!settings.data?.require_2fa
  return (
    <div className="mt-5 flex items-start gap-3 rounded-xl bg-muted/60 p-3.5">
      <Switch
        id="require-2fa"
        className="mt-0.5"
        checked={on}
        disabled={!settings.data || busy}
        onCheckedChange={async (checked) => {
          setBusy(true)
          try {
            const r = await api<{ require_2fa: boolean }>("PUT", "/settings/auth", { require_2fa: checked })
            queryClient.setQueryData(["/settings/auth"], r)
            // The shell keeps users without 2FA on Account while it's required.
            await queryClient.invalidateQueries({ queryKey: ["auth-state"] })
            notify(checked ? "Two-factor authentication is now required" : "Two-factor authentication is optional again")
            if (checked && !s.me?.totp_enabled) navigate("/account")
          } catch (err) {
            showError(err)
          } finally {
            setBusy(false)
          }
        }}
      />
      <Label htmlFor="require-2fa" className="flex flex-col items-start gap-0.5 font-normal">
        <span className="inline-flex items-center gap-1.5 font-medium">
          <ShieldCheckIcon className="size-4 text-success" />
          Require two-factor authentication for everyone
        </span>
        <span className="text-sm text-muted-foreground">Accounts without it can only reach Account until they set it up.</span>
      </Label>
      {settings.error && <span className="text-sm text-danger">{(settings.error as Error).message}</span>}
    </div>
  )
}

function AddUserDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Add a user"
      intro="A password is generated and shown once. They can change it and set up two-factor authentication under Account."
      ok="Add user"
      onSubmit={async (data) => {
        const r = await api<{ user: User; password?: string }>("POST", "/users", {
          username: String(data.get("username") || "").trim(),
          role: data.get("role"),
        })
        showSecret(`User ${r.user.username} created`, [
          `Username: ${r.user.username}`,
          `Password: ${r.password ?? ""}`,
          "",
          "They can change the password and set up two-factor authentication under Account.",
        ])
        await invalidate("/users")
      }}
    >
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="new-user-name">Username or email</FieldLabel>
          <Input id="new-user-name" name="username" required autoComplete="off" />
        </Field>
        <Field>
          <FieldLabel htmlFor="new-user-role">Role</FieldLabel>
          <NativeSelect id="new-user-role" name="role" defaultValue="operator" className="w-full">
            {STAFF_ROLES.map((r) => (
              <NativeSelectOption key={r} value={r}>
                {ROLE_NAMES[r]}
              </NativeSelectOption>
            ))}
          </NativeSelect>
          <FieldDescription>Viewers change nothing; operators run sites; administrators also manage users and the server.</FieldDescription>
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}

// ---- Audit log ----

function AuditLog() {
  const [actor, setActor] = useState("")
  const [draft, setDraft] = useState("")
  const audit = useApi<AuditEntry[]>("/audit?limit=200" + (actor ? "&actor=" + encodeURIComponent(actor) : ""))

  function filter(e: FormEvent) {
    e.preventDefault()
    const next = draft.trim()
    if (next === actor) audit.refetch()
    else setActor(next)
  }

  return (
    <Section
      icon={ListIcon}
      tint="gray"
      title="Audit log"
      description={
        <>
          Every change made through the panel, the API or the CLI (as <code>api-token</code>), and every sign-in.
        </>
      }
      action={
        <form onSubmit={filter} className="flex items-center gap-2">
          <InputGroup className="w-48">
            <InputGroupAddon>
              <SearchIcon />
            </InputGroupAddon>
            <InputGroupInput
              type="search"
              placeholder="Filter by user"
              aria-label="Filter by user"
              autoComplete="off"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
            />
          </InputGroup>
          <Button type="submit" variant="tinted">
            Filter
          </Button>
        </form>
      }
    >
      {audit.isLoading && <Skeleton className="h-48 rounded-xl" />}
      {audit.error && <LoadError error={audit.error} retry={() => audit.refetch()} />}
      {audit.data && (
        <BTable
          caption="Audit log"
          empty={<p className="py-2 text-sm text-muted-foreground">{actor ? `Nothing by ${actor}.` : "Nothing yet."}</p>}
          cols={["Time", "User", "From", "Action", "Result", "Detail"]}
          rows={audit.data.map((e) => ({
            key: e.id,
            cells: [
              <span className="whitespace-nowrap">{fmtTime(e.time)}</span>,
              e.actor,
              <span className="font-mono text-xs">{e.ip}</span>,
              <span className="[overflow-wrap:anywhere] whitespace-normal">{e.action}</span>,
              <span className={cn("font-medium tabular-nums", (e.status ?? 0) >= 400 ? "text-danger" : "text-success")}>{e.status || ""}</span>,
              <span className="text-sm [overflow-wrap:anywhere] whitespace-normal text-muted-foreground">{e.detail || ""}</span>,
            ],
          }))}
        />
      )}
    </Section>
  )
}
