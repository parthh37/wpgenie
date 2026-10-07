import { useState } from "react"
import { BugIcon, ScanSearchIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import { Note, Partial, sevClass, WRAP } from "./shared"
import { componentsOf, type ScanReport } from "./types"

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
      description="Checks what's installed against known vulnerabilities, and looks for signs of a break-in (changed WordPress or plugin files, PHP hidden in uploads). Runs every night."
      action={
        s.canChange && (
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
  const issues: Array<[string, string]> = [
    ...(integ?.core_modified ?? []).map((f): [string, string] => ["Modified core file", f]),
    ...(integ?.plugins_modified ?? []).map((f): [string, string] => ["Modified plugin file", f]),
    ...(integ?.uploads_php ?? []).map((f): [string, string] => ["PHP file in uploads", f]),
  ]
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
        {issues.length ? (
          <span className="font-medium text-danger">{issues.length} integrity issue(s)</span>
        ) : (
          <span className="text-success">files intact</span>
        )}
      </p>
      {rows.length > 0 && <SimpleTable headers={["Component", "Severity", "Vulnerability", "Fix"]} rows={rows} />}
      {issues.length > 0 && (
        <SimpleTable
          headers={["Finding", "File"]}
          rows={issues.map(([a, b]) => [a, <span className={cn(WRAP, "font-mono text-xs")}>{b}</span>])}
        />
      )}
      <Partial label="Partial scan" errors={rep.errors} />
    </div>
  )
}
