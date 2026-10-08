import { useState } from "react"
import { BugIcon, ScanSearchIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtDate, fmtNum, fmtTime, humanize, plural } from "@/lib/format"
import { queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import { Note, Partial, sevClass, WRAP } from "./shared"
import { componentsOf, type Intrusion, type ScanReport } from "./types"

// The vulnerability and hack scan (the legacy showScan): the last report,
// loaded on opening, and Scan now.
export function ScanSection({ site }: { site: Site }) {
  const s = useSession()
  const path = `/sites/${site.id}/scan`
  const q = useApi<ScanReport | null>(path)
  const [busy, setBusy] = useState(false)

  const scan = async () => {
    setBusy(true)
    try {
      queryClient.setQueryData([path], await api<ScanReport>("POST", path))
    } catch (e) {
      showError(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={BugIcon}
      tint="red"
      title="Vulnerability and hack scan"
      description="Checks what's installed against known vulnerabilities, and looks for signs of a break-in: changed WordPress or plugin files, PHP hidden in uploads, and administrators nobody added from here. Runs every night."
      action={
        s.canChangeSite(site) && (
          <Button variant="tinted" size="sm" disabled={busy} onClick={scan}>
            <ScanSearchIcon data-icon="inline-start" />
            {busy ? "Scanning… (up to a minute)" : "Scan now"}
          </Button>
        )
      }
    >
      {q.isPending ? (
        <Skeleton className="h-16 rounded-xl" />
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <ScanResult rep={q.data} />
      )}
    </Section>
  )
}

function ScanResult({ rep }: { rep: ScanReport | null | undefined }) {
  if (!rep) return <Note>Not scanned yet. Sites are scanned daily.</Note>
  const vulnerable = componentsOf(rep.inventory).filter((c) => c.vulns?.length)
  const rows = vulnerable.flatMap((c) =>
    (c.vulns ?? []).map((v) => [
      `${c.slug} ${c.version}`,
      <span className={sevClass(v.severity)}>{v.severity || "?"}</span>,
      v.link ? (
        <a href={v.link} target="_blank" rel="noopener noreferrer" className={WRAP}>
          {v.title}
        </a>
      ) : (
        <span className={WRAP}>{v.title}</span>
      ),
      v.unfixed ? "no fix yet" : c.update_fixes ? `update to ${c.update_version}` : v.fixed_in ? `fixed in ${v.fixed_in}` : "",
    ])
  )
  const integ = rep.integrity
  const uploadsPHP = integ?.uploads_php ?? []
  const issues: Array<[string, string]> = [
    ...(integ?.core_modified ?? []).map((f): [string, string] => ["Modified core file", f]),
    ...(integ?.plugins_modified ?? []).map((f): [string, string] => ["Modified plugin file", f]),
    ...uploadsPHP.map((f): [string, string] => ["PHP file in uploads", f]),
  ]
  const x = rep.intrusion
  const newAdmins = x?.new_admins ?? []
  const changed = x?.file_changes_total ?? 0
  const suspicious = issues.length + newAdmins.length + changed
  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm">
        Scanned {fmtTime(rep.scanned_at)}:{" "}
        {vulnerable.length ? (
          <span className="font-medium text-danger">{vulnerable.length} vulnerable component(s)</span>
        ) : (
          <span className="text-success">no known vulnerabilities</span>
        )}
        ,{" "}
        {suspicious ? (
          <span className="font-medium text-danger">{plural(suspicious, "sign")} of a possible break-in</span>
        ) : (
          <span className="text-success">no signs of a break-in</span>
        )}
      </p>
      {rows.length > 0 && <SimpleTable headers={["Component", "Severity", "Vulnerability", "Fix"]} rows={rows} />}
      {newAdmins.length > 0 && <NewAdmins x={x!} />}
      {changed > 0 && <ChangedFiles x={x!} />}
      {issues.length > 0 && (
        <div className="flex flex-col gap-2">
          <SimpleTable
            headers={["Finding", "File"]}
            rows={issues.map(([a, b]) => [a, <span className={cn(WRAP, "font-mono text-xs")}>{b}</span>])}
          />
          <Note>
            {uploadsPHP.length > 0 && "WordPress never puts code in the uploads folder: delete those files (Files, or SFTP). "}
            {issues.length > uploadsPHP.length &&
              "Modified WordPress or plugin files: update or reinstall what they belong to, or restore a backup from before they changed."}
          </Note>
        </div>
      )}
      {x && <Baseline x={x} />}
      <Partial label="Partial scan" errors={rep.errors} />
    </div>
  )
}

// registered is WordPress's user_registered: UTC, "2026-10-09 08:15:00".
const registeredOn = (r?: string) => (r ? fmtDate(new Date(r.replace(" ", "T") + "Z")) : "")

function NewAdmins({ x }: { x: Intrusion }) {
  const admins = x.new_admins ?? []
  return (
    <div className="flex flex-col gap-2 rounded-xl bg-danger-fill/10 p-3.5">
      <p className="text-sm font-medium text-danger">
        {admins.length === 1 ? "A new administrator appeared" : `${admins.length} new administrators appeared`} since the last scan, not added from
        this panel
      </p>
      <SimpleTable
        headers={["Username", "E-mail", "Created"]}
        rows={admins.map((a) => [
          <span className="flex flex-col">
            <span className="font-medium">{a.login}</span>
            <span className="text-xs text-muted-foreground">{a.super ? "network administrator" : humanize(a.role || "administrator")}</span>
          </span>,
          <span className={WRAP}>{a.email || ""}</span>,
          registeredOn(a.registered),
        ])}
      />
      <Note>
        If you don't know who made {admins.length === 1 ? "it" : "them"}: delete {admins.length === 1 ? "it" : "them"} in WordPress (or under
        WordPress in this panel), use Sign everyone out under WordPress hardening, change your passwords, and turn on Lock administrator accounts.
      </Note>
    </div>
  )
}

function ChangedFiles({ x }: { x: Intrusion }) {
  const files = x.file_changes ?? []
  const more = x.file_changes_total - files.length
  return (
    <div className="flex flex-col gap-2 rounded-xl bg-warning-fill/10 p-3.5">
      <p className="text-sm font-medium text-warning">
        {plural(x.file_changes_total, "PHP file")} changed without an update since the last scan
      </p>
      <SimpleTable
        headers={["File", "What happened", "Part of"]}
        rows={files.map((f) => [
          <span className={cn(WRAP, "font-mono text-xs")}>wp-content/{f.path}</span>,
          f.change === "added" ? "new file" : "changed",
          f.component,
        ])}
      />
      <Note>
        {more > 0 && `And ${fmtNum(more)} more. `}
        Fine if you or your developer changed them. If not, this is how most backdoors look: restore a backup from before the change, then update
        everything and change your passwords.
      </Note>
    </div>
  )
}

// Baseline: what the comparison with the previous scan covered, in words.
function Baseline({ x }: { x: Intrusion }) {
  const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)
  const lines: string[] = []
  // The server's reasons: "first check: …", or why it started over.
  const why = (what: string, reason: string) => (reason.startsWith("first check") ? `${reason}.` : `${what} weren't compared: ${reason}.`)
  if (x.admins_baseline) lines.push(why("Administrators", x.admins_baseline))
  if (x.files_baseline) lines.push(why("Files", x.files_baseline))
  else if (x.files_checked)
    lines.push(
      `${plural(x.files_checked, "plugin and theme file")} compared with the last scan${x.files_truncated ? " (only the first ones on a site this large: new files can't be told apart)" : ""}.`
    )
  if (x.updated?.length) lines.push(`Changed by updates, not a concern: ${x.updated.join(", ")}.`)
  if (!lines.length) return null
  return (
    <div className="flex flex-col gap-1">
      {lines.map((l) => (
        <Note key={l} className="text-[0.8125rem]">
          {cap(l)}
        </Note>
      ))}
    </div>
  )
}
