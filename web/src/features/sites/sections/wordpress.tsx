import { useState } from "react"
import { ExternalLinkIcon, KeyRoundIcon, LogInIcon, PlusIcon, UsersIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, ChoiceCard, FormDialog, LoadError } from "@/components/app/blocks"
import { ask, askText } from "@/components/app/confirm"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { plural } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "../sections"
import type { WPUser } from "./protect/types"

// WordPress admin: sign in to wp-admin as any administrator without their
// password; add and delete administrators and editors (never the site's
// first user); reset a password (wordpress.js renderWordPress/showAdmins).

// openFresh opens a tab now, in the click (one opened after the request
// would be blocked as a pop-up), then sends it to the URL get() resolves
// to. Without a tab (pop-ups blocked), the link is shown to copy.
async function openFresh(get: () => Promise<string>, fallbackTitle: string) {
  const win = window.open("about:blank", "_blank")
  try {
    const url = await get()
    if (win) {
      win.opener = null
      win.location.href = url
    } else showSecret(fallbackTitle, [url], "Open it in this browser within 2 minutes: it works once.")
  } catch (e) {
    win?.close()
    showError(e)
  }
}

// wpLogin signs in to a site's wp-admin as an administrator (0: the oldest).
const wpLogin = (site: Site, userID = 0) =>
  openFresh(
    async () => (await api<{ url: string }>("POST", `/sites/${site.id}/wp-admin/login`, { user_id: userID })).url,
    "wp-admin sign-in link (single use, 2 minutes)"
  )

async function resetPassword(site: Site, u: WPUser) {
  const pw = await askText(
    `Choose a new password, or leave it empty for a strong random one (shown once). Every session of ${u.login} ends: anyone signed in as them is signed out.`,
    {
      title: `Reset ${u.login}'s password on ${site.primary_domain}?`,
      label: "New password (12+ characters, optional)",
      type: "password",
      autocomplete: "new-password",
      ok: "Reset password",
      danger: true,
    }
  )
  if (pw === null) return
  if (pw && pw.length < 12) {
    showError(new Error("The password must be at least 12 characters, or empty for a random one."))
    return
  }
  try {
    const r = await api<{ user: string; password: string }>("POST", `/sites/${site.id}/wp-admin/password`, { user_id: u.id, password: pw })
    if (pw) notify(`Password of ${r.user} changed; their sessions ended`)
    else showSecret(`New WordPress password on ${site.primary_domain}`, [`Username: ${r.user}`, `Password: ${r.password}`])
  } catch (e) {
    showError(e)
  }
}

export default function WordpressSection({ site }: SectionProps) {
  const s = useSession()
  const [busy, setBusy] = useState(false)
  const adminURL = `https://${site.primary_domain}/wp-admin/`

  const signIn = async () => {
    setBusy(true)
    await wpLogin(site)
    setBusy(false)
  }

  return (
    <div className="flex flex-col">
      <Section
        icon={KeyRoundIcon}
        tint="yellow"
        title="Sign in without a password"
        description="Sign in to wp-admin as any administrator without their password: a one-time link on the site's own domain opens a normal WordPress session (listed in that user's sessions, ended by logging out). Resetting a password ends all of that user's sessions; WordPress sends no e-mail about it."
      >
        <div className="flex flex-wrap gap-2">
          {s.canChange ? (
            <Button disabled={busy} onClick={signIn} title="Sign in to wp-admin (no WordPress password needed)">
              <LogInIcon data-icon="inline-start" />
              Sign in to wp-admin
            </Button>
          ) : (
            // Viewers can't make sessions: for them it's the plain sign-in page.
            <Button render={<a href={adminURL} target="_blank" rel="noopener" />} nativeButton={false}>
              <ExternalLinkIcon data-icon="inline-start" />
              Open wp-admin
            </Button>
          )}
        </div>
      </Section>
      <Users site={site} />
    </div>
  )
}

const ROLE_LABEL: Record<WPUser["role"], string> = { administrator: "Administrator", editor: "Editor" }

async function deleteUser(site: Site, u: WPUser, owner?: WPUser) {
  const to = owner ? owner.login : "the site's first user"
  if (!(await ask(`Delete the WordPress ${u.role} ${u.login}? Their posts and pages move to ${to}; their sessions end.`))) return
  await api("DELETE", `/sites/${site.id}/wp-admin/users/${u.id}`)
  notify(`${u.login} deleted; their content now belongs to ${to}`)
  await invalidate(`/sites/${site.id}/wp-admin/users`)
}

function Users({ site }: { site: Site }) {
  const s = useSession()
  const [adding, setAdding] = useState(false)
  const q = useApi<WPUser[]>(`/sites/${site.id}/wp-admin/users`)
  const users = q.data ?? []
  const owner = users.find((u) => u.owner)
  const admins = users.filter((u) => u.role === "administrator").length
  return (
    <Section
      icon={UsersIcon}
      tint="blue"
      title="Administrators & editors"
      description={
        q.data
          ? `${plural(admins, "administrator")}, ${plural(users.length - admins, "editor")}. The site's first user can't be deleted: content of deleted users moves to it.`
          : undefined
      }
      action={
        s.canChange && (
          <Button onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add user
          </Button>
        )
      }
    >
      {q.isPending ? (
        <div className="flex flex-col gap-2">
          <Skeleton className="h-10 rounded-xl" />
          <Skeleton className="h-10 rounded-xl" />
        </div>
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <SimpleTable
          headers={["User", "E-mail", ""]}
          empty="No administrators or editors found."
          rowKey={(i) => users[i].id}
          rows={users.map((u) => [
            <div className="flex flex-col gap-1">
              <div className="flex flex-wrap items-center gap-1.5">
                <strong className="font-semibold">{u.login}</strong>
                <Badge variant={u.role === "administrator" ? "default" : "secondary"}>{ROLE_LABEL[u.role]}</Badge>
                {u.owner && (
                  <Badge variant="outline" title="The site's first user: it can't be deleted">
                    First user
                  </Badge>
                )}
              </div>
              {u.name && u.name !== u.login && <span className="text-sm font-normal text-muted-foreground">{u.name}</span>}
            </div>,
            u.email,
            s.canChange ? (
              <div className="flex flex-wrap justify-end gap-2">
                {u.role === "administrator" && (
                  <Button variant="tinted" size="sm" onClick={() => wpLogin(site, u.id)}>
                    Sign in as {u.login}
                  </Button>
                )}
                <Button variant="tinted" size="sm" onClick={() => resetPassword(site, u)}>
                  Reset password
                </Button>
                {/* The first user and the last administrator stay (the server refuses too). */}
                {!u.owner && !(u.role === "administrator" && admins <= 1) && (
                  <ActionButton run={() => deleteUser(site, u, owner)} size="sm" variant="destructive">
                    Delete
                  </ActionButton>
                )}
              </div>
            ) : null,
          ])}
        />
      )}
      <AddUser site={site} open={adding} onOpenChange={setAdding} />
    </Section>
  )
}

// AddUser adds an administrator or editor. WordPress sends no e-mail: the
// password (typed, or a random one) is shown once.
function AddUser({ site, open, onOpenChange }: { site: Site; open: boolean; onOpenChange: (o: boolean) => void }) {
  const [role, setRole] = useState<WPUser["role"]>("editor")
  const id = (f: string) => `wp-new-${f}-${site.id}`
  return (
    <FormDialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o)
        if (!o) setRole("editor")
      }}
      title={`Add a WordPress user to ${site.primary_domain}`}
      intro="WordPress sends them no e-mail: share the username and password yourself."
      ok="Add user"
      onSubmit={async (f) => {
        const password = String(f.get("password") ?? "")
        const r = await api<{ user: WPUser; password: string }>("POST", `/sites/${site.id}/wp-admin/users`, {
          login: String(f.get("login") ?? "").trim(),
          email: String(f.get("email") ?? "").trim(),
          name: String(f.get("name") ?? "").trim(),
          role,
          password,
        })
        if (password) notify(`${ROLE_LABEL[r.user.role]} ${r.user.login} added`)
        else
          showSecret(`New WordPress ${r.user.role} on ${site.primary_domain}`, [
            `Sign in at: https://${site.primary_domain}/wp-admin/`,
            `Username:   ${r.user.login}`,
            `Password:   ${r.password}`,
          ])
        await invalidate(`/sites/${site.id}/wp-admin/users`)
      }}
    >
      <div role="radiogroup" aria-label="Role" className="grid gap-2 sm:grid-cols-2">
        <ChoiceCard name="role" value="editor" title="Editor" checked={role === "editor"} onChange={() => setRole("editor")}>
          Writes, edits and publishes everyone's posts and pages. No plugins, themes or settings.
        </ChoiceCard>
        <ChoiceCard name="role" value="administrator" title="Administrator" checked={role === "administrator"} onChange={() => setRole("administrator")}>
          Everything, including plugins, themes, users and settings.
        </ChoiceCard>
      </div>
      <Field>
        <FieldLabel htmlFor={id("login")}>Username</FieldLabel>
        <Input
          id={id("login")}
          name="login"
          required
          minLength={3}
          maxLength={60}
          pattern="[A-Za-z0-9_.][A-Za-z0-9_.\-]*"
          autoComplete="off"
          spellCheck={false}
          autoFocus
        />
        <FieldDescription>3 to 60 letters, digits, dots, dashes or underscores. It can't be changed later.</FieldDescription>
      </Field>
      <Field>
        <FieldLabel htmlFor={id("email")}>E-mail</FieldLabel>
        <Input id={id("email")} name="email" type="email" required maxLength={100} autoComplete="off" spellCheck={false} />
      </Field>
      <Field>
        <FieldLabel htmlFor={id("name")}>Display name</FieldLabel>
        <Input id={id("name")} name="name" maxLength={250} autoComplete="off" placeholder="optional" />
      </Field>
      <Field>
        <FieldLabel htmlFor={id("password")}>Password</FieldLabel>
        <Input
          id={id("password")}
          name="password"
          type="password"
          minLength={12}
          maxLength={200}
          autoComplete="new-password"
          placeholder="empty: a strong random one"
        />
        <FieldDescription>12 characters or more. Left empty, a random one is made and shown once.</FieldDescription>
      </Field>
    </FormDialog>
  )
}
