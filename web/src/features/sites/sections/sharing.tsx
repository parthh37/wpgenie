import { useState } from "react"
import { Building2Icon, PlusIcon, UserPlusIcon, UsersIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Banner, BTable, ChoiceCard, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import type { Account } from "@/features/accounts/types"
import { api, ApiError } from "@/lib/api"
import { fmtAgo, fmtDate } from "@/lib/format"
import { invalidate, queryClient, useApi } from "@/lib/query"
import { href } from "@/lib/router"
import { ACCESS_LABELS, useSession } from "@/lib/session"
import type { Site, SiteAccess, SiteGrant } from "@/lib/types"
import type { SectionProps } from "@/features/sites/sections"

// Sharing: the site's owners give people with their own login on this
// panel (a developer, an agency) access to it, at a level. They see it
// with their own sites, under this site's plan; they never delete it or
// share it further. A site is shared under the account it belongs to, so
// staff first give a staff-only site to an account (the Owner card).

const LEVELS: { value: SiteAccess; text: string }[] = [
  {
    value: "viewer",
    text: "Sees how the site is doing: traffic, health, protection, updates and activity. Can’t open its files or backups, or change anything.",
  },
  {
    value: "developer",
    text: "Works on the site: files, SFTP and database, backups, staging, caches, plugins and updates. Not its domains, protection, mail or anything that costs money.",
  },
  {
    value: "manager",
    text: "Everything you can do on this site, domains, SSL, CDN, protection and scaling included, except deleting it or sharing it.",
  },
]

// A user staff could share the site with (GET /sites/<id>/access/candidates).
interface Candidate {
  user_id: number
  username: string
  account_id: number
  account_name: string
}

// refreshShares: the people list, the staff picker and the site list's
// "Shared with N" badge.
async function refreshShares(siteID: string) {
  await Promise.all([invalidate(`/sites/${siteID}/access`), queryClient.invalidateQueries({ queryKey: ["/sites"], exact: true })])
}

export default function SharingSection({ site }: SectionProps) {
  const s = useSession()
  const owned = !!site.account_id
  const path = `/sites/${site.id}/access`
  const { data: grants, error, refetch } = useApi<SiteGrant[]>(path)
  const [adding, setAdding] = useState(false)
  if (error) return <LoadError error={error} retry={() => refetch()} />
  if (!grants) return <Skeleton className="h-64 rounded-2xl" />

  const remove = async (g: SiteGrant) => {
    if (
      !(await ask(
        `Stop sharing ${site.primary_domain} with ${g.username}? They can’t reach it or its staging copies from now on, and the SFTP logins they added are deleted. Files they changed stay, and so do WordPress users they made: see WordPress admin.`
      ))
    )
      return
    await api("DELETE", `${path}/${g.user_id}`)
    notify(`${g.username} no longer has access`)
    await refreshShares(site.id)
  }

  return (
    <div className="flex flex-col">
      {s.isStaff && <OwnerCard site={site} />}

      <Section
        icon={UsersIcon}
        tint="indigo"
        title="People with access"
        description="Share this site with someone who has their own login on this panel, such as a developer or an agency. They see it next to their own sites, and what they do counts towards this site’s plan."
        action={
          s.canChange &&
          owned && (
            <Button onClick={() => setAdding(true)}>
              <PlusIcon data-icon="inline-start" />
              Share
            </Button>
          )
        }
      >
        {!owned ? (
          <p className="py-2 text-sm text-muted-foreground">
            This site doesn’t belong to an account yet, so only staff can open it. Choose its account above, then share it with anyone.
          </p>
        ) : (
          <BTable
            caption={`People ${site.primary_domain} is shared with`}
            cols={["Person", "Access", { label: <span className="sr-only">Actions</span> }]}
            rows={grants.map((g) => ({
              key: String(g.user_id),
              cells: [
                <div className="flex flex-col">
                  <span className="font-medium">{g.username}</span>
                  <span className="text-xs text-muted-foreground" title={fmtDate(g.created_at)}>
                    Shared by {g.granted_by}, {fmtAgo(g.created_at)}
                  </span>
                </div>,
                s.canChange ? <AccessSelect site={site} grant={g} /> : ACCESS_LABELS[g.access],
                s.canChange && (
                  <div className="flex justify-end">
                    <Button variant="destructive" size="sm" onClick={() => remove(g).catch(showError)}>
                      Remove
                    </Button>
                  </div>
                ),
              ],
            }))}
            empty={
              <div className="flex flex-wrap items-center justify-between gap-3 py-2">
                <p className="text-sm text-muted-foreground">
                  Not shared with anyone: only {s.isTenant ? "your account" : "the account it belongs to"} can reach it.
                </p>
                {s.canChange && (
                  <Button variant="tinted" size="sm" onClick={() => setAdding(true)}>
                    <UserPlusIcon data-icon="inline-start" />
                    Share with someone
                  </Button>
                )}
              </div>
            }
          />
        )}
      </Section>

      <Section icon={UserPlusIcon} tint="graphite" title="Access levels">
        <dl className="grid gap-3 sm:grid-cols-3">
          {LEVELS.map((l) => (
            <div key={l.value} className="rounded-xl bg-muted/50 p-3">
              <dt className="text-sm font-semibold">{ACCESS_LABELS[l.value]}</dt>
              <dd className="mt-1 text-sm text-muted-foreground">{l.text}</dd>
            </div>
          ))}
        </dl>
      </Section>

      <ShareDialog site={site} open={adding} onOpenChange={setAdding} />
    </div>
  )
}

// OwnerCard (staff): the account the site belongs to. Sharing happens
// under it, so a staff-only site gets one here first. Administrators
// change it; giving the site to another account ends its sharing.
function OwnerCard({ site }: { site: Site }) {
  const s = useSession()
  const { data: accounts } = useApi<Account[]>("/accounts")
  const [choice, setChoice] = useState<string>(site.account_id ? String(site.account_id) : "")
  const [busy, setBusy] = useState(false)
  const current = accounts?.find((a) => a.id === site.account_id)
  const open = accounts?.filter((a) => a.status !== "terminated") ?? []

  const save = async () => {
    const id = Number(choice) || 0
    if (id === (site.account_id ?? 0)) return
    if (site.account_id && (site.shared_with ?? 0) > 0) {
      const ok = await ask(
        `Move ${site.primary_domain} to ${id ? open.find((a) => a.id === id)?.name : "staff only"}? It stops being shared with the ${site.shared_with} people it’s shared with now.`
      )
      if (!ok) return
    }
    setBusy(true)
    try {
      await api("PUT", `/sites/${site.id}/account`, { account_id: id })
      notify(id ? "The site now belongs to " + (open.find((a) => a.id === id)?.name ?? "the account") : "The site is staff only")
      await Promise.all([refreshShares(site.id), invalidate("/accounts")])
    } catch (e) {
      showError(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={Building2Icon}
      tint="blue"
      title="Owner"
      description="The account this site belongs to. People you share it with work under that account’s plan."
    >
      {!site.account_id && (
        <Banner tone="warn" title="Choose an owner to share this site">
          Only sites that belong to an account can be shared. Pick the account (yours, or a customer’s) and the Share button appears below.
        </Banner>
      )}
      {s.isAdmin ? (
        accounts && !open.length ? (
          <p className="text-sm text-muted-foreground">
            There are no accounts yet.{" "}
            <a href={href("accounts")} className="text-link">
              Create one under Accounts
            </a>
            , then come back to give it this site.
          </p>
        ) : (
          <div className="flex flex-wrap items-end gap-3">
            <Field className="w-72 max-w-full">
              <FieldLabel htmlFor={`owner-${site.id}`}>Belongs to</FieldLabel>
              <NativeSelect id={`owner-${site.id}`} value={choice} disabled={!accounts || busy} onChange={(e) => setChoice(e.target.value)}>
                <NativeSelectOption value="">Staff only (no account)</NativeSelectOption>
                {open.map((a) => (
                  <NativeSelectOption key={a.id} value={String(a.id)}>
                    {a.name}
                    {a.kind === "reseller" ? " (reseller)" : a.parent_name ? ` (${a.parent_name}’s customer)` : ""}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            </Field>
            <Button onClick={save} disabled={busy || choice === (site.account_id ? String(site.account_id) : "")}>
              Save
            </Button>
          </div>
        )
      ) : (
        <p className="text-sm">
          {current ? (
            <>
              Belongs to <strong>{current.name}</strong>.
            </>
          ) : site.account_id ? (
            `Belongs to account #${site.account_id}.`
          ) : (
            "Staff only: no account. An administrator can give it to one."
          )}
        </p>
      )}
    </Section>
  )
}

function AccessSelect({ site, grant }: { site: Site; grant: SiteGrant }) {
  const [busy, setBusy] = useState(false)
  const change = async (access: SiteAccess) => {
    setBusy(true)
    try {
      await api("PUT", `/sites/${site.id}/access/${grant.user_id}`, { access })
      notify(
        access === "viewer" && grant.access !== "viewer"
          ? `${grant.username} is now a Viewer; the SFTP logins they added were deleted`
          : `${grant.username} is now a ${ACCESS_LABELS[access]}`
      )
      await invalidate(`/sites/${site.id}/access`)
    } catch (e) {
      showError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <NativeSelect
      className="w-36"
      aria-label={`Access for ${grant.username}`}
      value={grant.access}
      disabled={busy}
      onChange={(e) => change(e.target.value as SiteAccess)}
    >
      {LEVELS.map((l) => (
        <NativeSelectOption key={l.value} value={l.value}>
          {ACCESS_LABELS[l.value]}
        </NativeSelectOption>
      ))}
    </NativeSelect>
  )
}

// ShareDialog: who, and at what level. Staff pick from the people who
// could have it; tenants type the username they were given (who has a
// login here isn't theirs to browse).
export function ShareDialog({ site, open, onOpenChange }: { site: Site; open: boolean; onOpenChange: (o: boolean) => void }) {
  const s = useSession()
  const [username, setUsername] = useState("")
  const { data: candidates } = useApi<Candidate[]>(`/sites/${site.id}/access/candidates`, { enabled: open && s.isStaff })
  const q = username.trim().toLowerCase()
  const picks = (candidates ?? []).filter((c) => !q || c.username.toLowerCase().includes(q) || c.account_name.toLowerCase().includes(q)).slice(0, 8)
  const exact = candidates?.some((c) => c.username.toLowerCase() === q)

  return (
    <FormDialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setUsername("")
        onOpenChange(o)
      }}
      wide
      title={`Share ${site.primary_domain}`}
      intro={
        s.isStaff
          ? "Pick someone with a login on this panel, or type their username."
          : "They need their own login on this panel: ask them for their username."
      }
      ok="Share"
      onSubmit={async (f) => {
        const name = String(f.get("username") ?? "").trim()
        let g: SiteGrant
        try {
          g = await api<SiteGrant>("POST", `/sites/${site.id}/access`, { username: name, access: f.get("access") })
        } catch (e) {
          // "bad request: no user…", "conflict: the site is already…": the sentence alone.
          if (e instanceof ApiError) throw new ApiError(e.message.replace(/^(bad request|conflict): /, ""), e.status, e.data)
          throw e
        }
        notify(`Shared with ${g.username}. They’ll see ${site.primary_domain} under Sites next time they open the panel.`)
        setUsername("")
        await refreshShares(site.id)
      }}
    >
      <Field>
        <FieldLabel htmlFor={`share-user-${site.id}`}>Username</FieldLabel>
        <Input
          id={`share-user-${site.id}`}
          name="username"
          required
          autoComplete="off"
          spellCheck={false}
          autoCapitalize="none"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <FieldDescription>Their own login, not an email address.</FieldDescription>
        {s.isStaff && candidates && !exact && (
          <div className="mt-1 flex flex-wrap gap-1.5" aria-label="People you can share it with">
            {picks.map((c) => (
              <button
                key={c.user_id}
                type="button"
                onClick={() => setUsername(c.username)}
                className="flex items-center gap-1.5 rounded-full bg-muted px-3 py-1 text-sm transition-colors hover:bg-primary/12 focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
              >
                <span className="font-medium">{c.username}</span>
                <span className="text-xs text-muted-foreground">{c.account_name}</span>
              </button>
            ))}
            {!picks.length && (
              <p className="text-sm text-muted-foreground">
                {candidates.length ? "No one matches." : "No one else has a login yet: create an account for them under Accounts."}
              </p>
            )}
          </div>
        )}
      </Field>
      <FieldSet>
        <FieldLegend variant="label">Access</FieldLegend>
        <div className="grid gap-2.5">
          {LEVELS.map((l) => (
            <ChoiceCard key={l.value} name="access" value={l.value} title={ACCESS_LABELS[l.value]} defaultChecked={l.value === "developer"}>
              {l.text}
            </ChoiceCard>
          ))}
        </div>
      </FieldSet>
    </FormDialog>
  )
}
