import { useState } from "react"
import { PlusIcon, UserPlusIcon, UsersIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { BTable, ChoiceCard, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import { fmtAgo, fmtDate } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { ACCESS_LABELS, useSession } from "@/lib/session"
import type { Site, SiteAccess, SiteGrant } from "@/lib/types"
import type { SectionProps } from "@/features/sites/sections"

// Sharing: the site's owners give people with their own login on this
// panel (a developer, an agency) access to it, at a level. They see it
// with their own sites, under this site's plan; they never delete it or
// share it further.

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

export default function SharingSection({ site }: SectionProps) {
  const s = useSession()
  const path = `/sites/${site.id}/access`
  const { data: grants, error, refetch } = useApi<SiteGrant[]>(path)
  const [adding, setAdding] = useState(false)
  if (error) return <LoadError error={error} retry={() => refetch()} />
  if (!grants) return <Skeleton className="h-64 rounded-2xl" />

  const remove = async (g: SiteGrant) => {
    if (!(await ask(`Stop sharing ${site.primary_domain} with ${g.username}? They can’t reach it from now on.`))) return
    await api("DELETE", `${path}/${g.user_id}`)
    notify(`${g.username} no longer has access`)
    await invalidate(path)
  }

  return (
    <div className="flex flex-col">
      <Section
        icon={UsersIcon}
        tint="indigo"
        title="People with access"
        description="Share this site with someone who has their own login on this panel, such as a developer or an agency. They see it next to their own sites, and what they do counts towards this site’s plan."
        action={
          s.canChange && (
            <Button onClick={() => setAdding(true)}>
              <PlusIcon data-icon="inline-start" />
              Share
            </Button>
          )
        }
      >
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
            <p className="py-2 text-sm text-muted-foreground">
              Not shared with anyone: only {s.isTenant ? "your account" : "the account it belongs to"} can reach it.
            </p>
          }
        />
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

function AccessSelect({ site, grant }: { site: Site; grant: SiteGrant }) {
  const [busy, setBusy] = useState(false)
  const change = async (access: SiteAccess) => {
    setBusy(true)
    try {
      await api("PUT", `/sites/${site.id}/access/${grant.user_id}`, { access })
      notify(`${grant.username} is now a ${ACCESS_LABELS[access]}`)
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

function ShareDialog({ site, open, onOpenChange }: { site: Site; open: boolean; onOpenChange: (o: boolean) => void }) {
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      wide
      title={`Share ${site.primary_domain}`}
      intro="They need a login on this panel already: ask them for their username."
      ok="Share"
      onSubmit={async (f) => {
        const username = String(f.get("username") ?? "").trim()
        let g: SiteGrant
        try {
          g = await api<SiteGrant>("POST", `/sites/${site.id}/access`, { username, access: f.get("access") })
        } catch (e) {
          // "bad request: no user…", "conflict: the site is already…": the sentence alone.
          if (e instanceof ApiError) throw new ApiError(e.message.replace(/^(bad request|conflict): /, ""), e.status, e.data)
          throw e
        }
        notify(`Shared with ${g.username}`)
        await invalidate(`/sites/${site.id}/access`)
      }}
    >
      <Field>
        <FieldLabel htmlFor={`share-user-${site.id}`}>Username</FieldLabel>
        <Input id={`share-user-${site.id}`} name="username" required autoComplete="off" spellCheck={false} autoCapitalize="none" />
        <FieldDescription>Their own login, not an email address.</FieldDescription>
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
