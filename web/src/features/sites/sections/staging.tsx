import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { ArrowUpFromLineIcon, ExternalLinkIcon, GitBranchIcon, PlusIcon, Trash2Icon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ChoiceCard } from "@/components/app/blocks"
import { ask, askText } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api, errorMessage } from "@/lib/api"
import { invalidate, useSites } from "@/lib/query"
import { href, navigate, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "@/features/sites/sections"
import { useSiteJob } from "./data/jobs"

// Staging: a private copy of a live site on its own domain. On a live site:
// create one (or open it); on a staging site: push files, the database or
// some of its tables to the live site.

export default function StagingSection({ site }: SectionProps) {
  const { data: sites } = useSites()
  if (site.parent_id) return <StagingCopy site={site} parent={sites?.find((s) => s.id === site.parent_id)} />
  const staging = sites?.find((s) => s.parent_id === site.id)
  return staging ? <HasStaging site={site} staging={staging} /> : <CreateStaging site={site} />
}

// A link to another site's workspace.
function SiteLink({ id, label, section }: { id: string; label: string; section?: string }) {
  return (
    <a
      href={href(sitePath(id, section))}
      onClick={(e) => {
        e.preventDefault()
        navigate(sitePath(id, section))
      }}
      className="font-semibold"
    >
      {label}
    </a>
  )
}

// deleteStaging deletes a staging site, the domain typed to confirm (as the
// overview's Delete site).
async function deleteStaging(staging: Site) {
  const typed = await askText(
    `This permanently deletes the staging site ${staging.primary_domain}, its files and database. The live site isn't touched. You can create a fresh copy afterwards.`,
    { title: `Delete ${staging.primary_domain}?`, label: "Type the domain to confirm", match: staging.primary_domain, ok: "Delete staging site" }
  )
  if (typed !== staging.primary_domain) return false
  try {
    await api("DELETE", `/sites/${staging.id}`)
    notify(`${staging.primary_domain} deleted`)
    return true
  } catch (e) {
    showError(e)
    return false
  }
}

// ---- A live site without a staging copy ----

function CreateStaging({ site }: { site: Site }) {
  const s = useSession()
  const job = useSiteJob(site.id, ["staging"])
  const [domain, setDomain] = useState("")
  const placeholder = "staging." + site.primary_domain.replace(/^www\./, "")

  const create = async () => {
    const res = await job.run<{ job_id: string; site?: Site }>("POST", `/sites/${site.id}/staging`, { domain: domain.trim() }, async (v) => {
      if (v.job.status === "succeeded") notify("Staging site ready")
    })
    // The copy is listed (being created) at once.
    if (res) await invalidate("/sites")
  }

  return (
    <Section
      icon={GitBranchIcon}
      tint="teal"
      title="Create a staging site"
      description="A full copy of this site (files and database) on its own domain, to try updates, themes and changes safely. Point the domain's DNS at this server first."
    >
      {s.canChange ? (
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            create()
          }}
        >
          <Field className="w-auto min-w-72 flex-1 sm:max-w-md">
            <FieldLabel htmlFor={`stg-domain-${site.id}`}>Domain</FieldLabel>
            <Input
              id={`stg-domain-${site.id}`}
              autoComplete="off"
              spellCheck={false}
              placeholder={placeholder}
              value={domain}
              disabled={job.running}
              onChange={(e) => setDomain(e.target.value)}
            />
          </Field>
          <Button type="submit" disabled={job.running}>
            <PlusIcon data-icon="inline-start" />
            {job.running ? "Creating staging site…" : "Create staging site"}
          </Button>
          <FieldDescription className="basis-full">Empty: {placeholder}.</FieldDescription>
        </form>
      ) : (
        <p className="text-sm text-muted-foreground">This site has no staging copy.</p>
      )}
    </Section>
  )
}

// ---- A live site with its staging copy ----

function HasStaging({ site, staging }: { site: Site; staging: Site }) {
  const s = useSession()
  return (
    <Section
      icon={GitBranchIcon}
      tint="teal"
      title="Staging site"
      description="Push changes from the staging site's own Staging section; delete it to create a fresh copy."
    >
      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center gap-3">
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-2">
              <SiteLink id={staging.id} label={staging.primary_domain} section="staging" />
              <StatusPill status={staging.status} />
            </div>
            <p className="font-mono text-xs text-muted-foreground">{staging.id}</p>
          </div>
          <Button variant="tinted" render={<a href={"https://" + staging.primary_domain} target="_blank" rel="noopener" />} nativeButton={false}>
            <ExternalLinkIcon data-icon="inline-start" />
            Visit
          </Button>
          <Button variant="tinted" onClick={() => navigate(sitePath(staging.id, "staging"))}>
            <ArrowUpFromLineIcon data-icon="inline-start" />
            {s.canChange ? "Push to live…" : "Open"}
          </Button>
          {s.canChange && (
            <Button
              variant="destructive"
              onClick={async () => {
                if (await deleteStaging(staging)) await invalidate("/sites")
              }}
            >
              <Trash2Icon data-icon="inline-start" />
              Delete
            </Button>
          )}
        </div>
        <p className="text-sm text-muted-foreground">
          A copy of {site.primary_domain}. Search engines are asked not to index it, it sends no mail through the mail server and WPGenie runs no cron for it.
        </p>
      </div>
    </Section>
  )
}

// ---- A staging site: push to live ----

type Files = "" | "code" | "all"
const FILES: Array<[Files, string, string]> = [
  ["", "No files", "Leave the live site's files as they are."],
  ["code", "Code: core, plugins, themes (not uploads)", "What you changed or updated; the live site's media stay."],
  ["all", "All files, uploads too", "The live site's files become the staging site's."],
]

function StagingCopy({ site, parent }: { site: Site; parent?: Site }) {
  const s = useSession()
  const parentLabel = parent?.primary_domain ?? site.parent_id
  const job = useSiteJob(site.id, ["push"])
  const [files, setFiles] = useState<Files>("code")
  const [db, setDb] = useState(false)
  const [picked, setPicked] = useState<string[]>([])
  const tables = useQuery<string[]>({
    queryKey: [`/sites/${site.id}/tables`],
    queryFn: () => api<string[]>("GET", `/sites/${site.id}/tables`),
    enabled: db,
    staleTime: 60_000,
  })

  const push = async () => {
    const chosen = db ? picked.filter((t) => tables.data?.includes(t)) : []
    const what = [files && FILES.find(([v]) => v === files)![1], db && (chosen.length ? `${chosen.length} table(s)` : "the whole database")].filter(Boolean)
    if (!what.length) {
      showError(new Error("Choose files, the database or both."))
      return
    }
    if (
      !(await ask(
        `Push ${what.join(" and ")} to the LIVE site ${parentLabel}?\n\nThe live site is backed up first, so this can be undone from its Backups.`
      ))
    )
      return
    await job.run("POST", `/sites/${site.id}/push`, { files, database: db, tables: chosen })
  }

  const toggle = (t: string, on: boolean) => setPicked((xs) => (on ? [...xs, t] : xs.filter((x) => x !== t)))

  return (
    <div className="flex flex-col">
      <Section icon={GitBranchIcon} tint="teal" title="Staging copy">
        <p className="text-sm">
          A staging copy of {parent ? <SiteLink id={parent.id} label={parentLabel} /> : <strong>{parentLabel}</strong>}. Search engines are asked not to index
          it, it sends no mail through the mail server and WPGenie runs no cron for it.
        </p>
      </Section>

      {s.canChange && (
        <Section
          icon={ArrowUpFromLineIcon}
          tint="teal"
          title={`Push to ${parentLabel}`}
          description="Links are rewritten to the live domain on the way; the live site's search engine setting is kept."
          action={
            <Button onClick={push} disabled={job.running}>
              <ArrowUpFromLineIcon data-icon="inline-start" />
              {job.running ? "Pushing…" : `Push to ${parentLabel}`}
            </Button>
          }
        >
          <div className="flex flex-col gap-5">
            <fieldset className="flex flex-col gap-2">
              <legend className="mb-2 text-sm font-medium">Files</legend>
              <div role="radiogroup" aria-label="Files" className="grid gap-2 md:grid-cols-3">
                {FILES.map(([v, title, sub]) => (
                  <ChoiceCard key={v || "none"} name={`push-files-${site.id}`} value={v} title={title} checked={files === v} onChange={() => setFiles(v)}>
                    {sub}
                  </ChoiceCard>
                ))}
              </div>
            </fieldset>

            <div className="flex flex-col gap-3">
              <Label className="w-fit font-medium">
                <Switch checked={db} onCheckedChange={setDb} />
                Database
              </Label>
              {db && (
                <div className="flex flex-col gap-3 rounded-2xl bg-muted/50 p-3.5">
                  <p className="text-sm text-muted-foreground">
                    Tables: none ticked = the whole database (live tables staging doesn't have are dropped). Tick some to push only those (e.g. posts, not
                    orders).
                  </p>
                  {tables.isPending ? (
                    <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
                      {Array.from({ length: 6 }, (_, i) => (
                        <Skeleton key={i} className="h-5" />
                      ))}
                    </div>
                  ) : tables.error ? (
                    <p role="alert" className="text-sm text-danger">
                      {errorMessage(tables.error)}
                    </p>
                  ) : (
                    <>
                      <div className="flex flex-wrap items-center gap-2 text-sm">
                        <span className="text-muted-foreground">{picked.length ? `${picked.length} ticked` : "The whole database"}</span>
                        {picked.length > 0 && (
                          <Button variant="ghost" size="xs" onClick={() => setPicked([])}>
                            Untick all
                          </Button>
                        )}
                      </div>
                      <div className="grid gap-x-4 gap-y-2 sm:grid-cols-2 lg:grid-cols-3">
                        {tables.data!.map((t) => (
                          <Label key={t} className="min-w-0 font-normal">
                            <Checkbox checked={picked.includes(t)} onCheckedChange={(c) => toggle(t, !!c)} />
                            <span className="truncate font-mono text-xs">{t}</span>
                          </Label>
                        ))}
                      </div>
                    </>
                  )}
                </div>
              )}
            </div>
          </div>
        </Section>
      )}

      {s.canChange && (
        <div className="flex flex-wrap items-center gap-4 rounded-2xl border border-danger/30 bg-danger-fill/5 p-4">
          <div className="min-w-0 flex-1">
            <h3 className="text-[0.9375rem] font-semibold">Delete this staging site</h3>
            <p className="max-w-[60ch] text-sm text-muted-foreground">Removes the copy's files and database; {parentLabel} isn't touched. You can create a fresh copy afterwards.</p>
          </div>
          <Button
            variant="destructive"
            onClick={async () => {
              if (!(await deleteStaging(site))) return
              navigate(parent ? sitePath(parent.id, "staging") : "/sites")
              await invalidate("/sites")
            }}
          >
            <Trash2Icon data-icon="inline-start" />
            Delete staging site
          </Button>
        </div>
      )}
    </div>
  )
}
