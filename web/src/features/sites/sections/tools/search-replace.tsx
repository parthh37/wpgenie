import { useState } from "react"
import { ReplaceIcon } from "lucide-react"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ActionButton } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { fmtNum, plural } from "@/lib/format"
import { startJob } from "@/lib/jobs"
import { invalidate } from "@/lib/query"
import { href, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { Meter, Note, Warning } from "../speed/shared"
import type { SearchReplaceResult } from "./types"

// Search & replace in the site's database (POST
// /sites/{id}/tools/search-replace): always a preview first, which counts
// what would change per table; then the real run, which backs the site up
// first when it can. Both are jobs, their counts the job's result.

const MAX = 1000

// check mirrors the server's rules, to say what's wrong before asking it.
function check(search: string, replace: string): string {
  if (!search.trim() || !replace.trim()) return ""
  if ([...search].length < 3) return "Search for at least 3 characters."
  if (search === replace) return "The search and the replacement are the same."
  if (search.startsWith("-") || replace.startsWith("-")) return "Neither can start with a dash."
  return ""
}

export function SearchReplaceCard({ site }: { site: Site }) {
  const s = useSession()
  const can = s.canChangeSite(site)
  const [search, setSearch] = useState("")
  const [replace, setReplace] = useState("")
  const [preview, setPreview] = useState<SearchReplaceResult | null>(null)
  const [haveBackup, setHaveBackup] = useState(false)
  const [running, setRunning] = useState(false)
  const problem = check(search, replace)
  const ready = !!search.trim() && !!replace.trim() && !problem
  // The preview counts for exactly these two strings.
  const current = preview && preview.search === search && preview.replace === replace ? preview : null
  const path = `/sites/${site.id}/tools/search-replace`

  const run = (dryRun: boolean) =>
    new Promise<void>((resolve, reject) => {
      setRunning(true)
      startJob("POST", path, { search, replace, dry_run: dryRun, have_backup: haveBackup }, async (v) => {
        setRunning(false)
        resolve()
        if (v.job.status !== "succeeded" || !v.job.result) return
        const r = JSON.parse(v.job.result) as SearchReplaceResult
        if (dryRun) {
          setPreview(r)
          return
        }
        setPreview(null)
        setHaveBackup(false)
        notify(`Replaced ${plural(r.total, "occurrence")} in ${plural(r.tables.length, "table")}`)
        await invalidate(`/sites/${site.id}/events`)
      }).catch((e) => {
        setRunning(false)
        reject(e)
      })
    })

  const replaceNow = async () => {
    if (!current) return
    const backup = current.backup_first
      ? "WPGenie backs the site up first; the backup is listed under Backups if you need to undo it."
      : "WPGenie can't back the site up first on this server: undoing it means restoring your own backup."
    const ok = await ask(
      `Replace ${plural(current.total, "occurrence")} of "${search}" with "${replace}" in ${site.primary_domain}'s database? ${backup}`,
      { ok: "Replace", danger: true }
    )
    if (ok) await run(false)
  }

  const most = Math.max(1, ...(current?.tables ?? []).map((t) => t.replacements))

  return (
    <Section
      icon={ReplaceIcon}
      tint="indigo"
      title="Search & replace"
      description="Replace a piece of text everywhere in the site's database: an old address, a renamed product, a typo repeated across posts. Text inside plugin and theme settings is handled safely. Preview first: it counts what would change without changing anything."
    >
      <div className="flex flex-col gap-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field>
            <FieldLabel htmlFor={`sr-search-${site.id}`}>Search for</FieldLabel>
            <Input
              id={`sr-search-${site.id}`}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              maxLength={MAX}
              disabled={!can || running}
              spellCheck={false}
              autoComplete="off"
              placeholder="http://old-domain.com"
            />
          </Field>
          <Field>
            <FieldLabel htmlFor={`sr-replace-${site.id}`}>Replace with</FieldLabel>
            <Input
              id={`sr-replace-${site.id}`}
              value={replace}
              onChange={(e) => setReplace(e.target.value)}
              maxLength={MAX}
              disabled={!can || running}
              spellCheck={false}
              autoComplete="off"
              placeholder="https://new-domain.com"
            />
          </Field>
        </div>
        {problem && <Warning>{problem}</Warning>}
        {can && (
          <div className="flex flex-wrap items-center gap-2">
            <ActionButton variant={current ? "tinted" : "default"} run={() => run(true)} disabled={!ready || running}>
              {running ? "Working…" : "Preview changes"}
            </ActionButton>
            {current && current.total > 0 && (
              <ActionButton
                variant="destructive-solid"
                run={replaceNow}
                disabled={running || (!current.backup_first && !haveBackup)}
              >
                Replace {fmtNum(current.total)}
              </ActionButton>
            )}
          </div>
        )}
        {current && (
          <div className="flex flex-col gap-3">
            {current.total === 0 ? (
              <Note>Nothing to replace: "{current.search}" isn't in the database.</Note>
            ) : (
              <>
                <p className="text-sm">
                  <strong className="font-semibold">{plural(current.total, "occurrence")}</strong> in {plural(current.tables.length, "table")} would
                  change.
                </p>
                <SimpleTable
                  headers={["Table", "Replacements", ""]}
                  rowKey={(i) => current.tables[i].table}
                  rows={current.tables.map((t) => [
                    <code className="text-xs">{t.table}</code>,
                    <span className="tabular-nums">{fmtNum(t.replacements)}</span>,
                    <Meter value={t.replacements} max={most} label={`${t.table}: ${t.replacements} replacements`} className="w-40" />,
                  ])}
                />
                {!current.backup_first && (
                  <label className="flex cursor-pointer items-start gap-3 text-sm">
                    <Checkbox className="mt-0.5" checked={haveBackup} onCheckedChange={(c) => setHaveBackup(!!c)} />
                    <span>
                      I have a recent backup of this site. WPGenie can't take one first on this server; see{" "}
                      <a href={href(sitePath(site.id, "backups"))}>Backups</a>.
                    </span>
                  </label>
                )}
              </>
            )}
          </div>
        )}
        <Note>
          Moving the site to a new domain? Add it under Domains & SSL and make it the primary domain: WPGenie replaces the links for you. The posts'
          permanent IDs (guid) are never changed.
        </Note>
      </div>
    </Section>
  )
}
