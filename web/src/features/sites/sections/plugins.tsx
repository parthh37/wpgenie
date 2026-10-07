import { useState } from "react"
import { PlugIcon, ScanSearchIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime, plural } from "@/lib/format"
import { queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import { cn } from "@/lib/utils"
import type { SectionProps } from "../sections"
import { age, Note, Partial, WRAP } from "./protect/shared"
import type { PluginInfo, PluginReport } from "./protect/types"

// Plugins: each one checked against wordpress.org, its files against the
// published release, and what it costs on the front page (app.js
// renderPlugins/showPlugins). The last report loads on opening.

const DIRECTORY: Record<string, string> = { listed: "listed", closed: "CLOSED", not_listed: "not listed", unknown: "?" }
const CHECKSUMS: Record<string, string> = { verified: "verified", modified: "MODIFIED", unavailable: "can't verify", not_checked: "–" }
// Findings that are about security (the rest are upkeep).
const SECURITY_FLAG = /^(closed|contains code|\d+ known|\d+ file)/

export default function PluginsSection({ site }: SectionProps) {
  const s = useSession()
  const path = `/sites/${site.id}/plugins`
  const q = useApi<PluginReport | null>(path)
  const [busy, setBusy] = useState(false)

  const analyse = async () => {
    setBusy(true)
    try {
      queryClient.setQueryData([path], await api<PluginReport>("POST", path))
    } catch (e) {
      showError(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={PlugIcon}
      tint="purple"
      title="Plugin check"
      description="Checks each plugin against wordpress.org (closed or abandoned), compares its files with the published release (modified or nulled copies) and measures what it costs to load the front page. Runs nightly; takes 10–30 seconds."
      action={
        s.canChange && (
          <Button variant="tinted" size="sm" disabled={busy} onClick={analyse}>
            <ScanSearchIcon data-icon="inline-start" />
            {busy ? "Analysing… (up to a minute)" : "Analyse now"}
          </Button>
        )
      }
    >
      {q.isPending ? (
        <div className="flex flex-col gap-2">
          <Skeleton className="h-5 w-2/3 rounded-md" />
          <Skeleton className="h-40 rounded-xl" />
        </div>
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <Report rep={q.data} />
      )}
    </Section>
  )
}

function Report({ rep }: { rep: PluginReport | null | undefined }) {
  if (!rep) return <Note>Not analysed yet.</Note>
  const plugins = rep.plugins ?? []
  const bad = plugins.filter((p) => (p.flags ?? []).some((f) => SECURITY_FLAG.test(f)))
  const prof = rep.profile
  const others = Object.entries(prof?.others ?? {})
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        <p className="text-sm">
          {plural(plugins.length, "plugin")} installed
          {bad.length > 0 && <span className="font-medium text-danger"> · {bad.length} need attention</span>}
        </p>
        <p className="text-sm text-muted-foreground">
          Analysed {fmtTime(rep.analysed_at)}
          {prof &&
            `: front page rendered in ${prof.total_ms.toFixed(0)} ms with ${prof.queries} database queries and ${(prof.peak_memory_kb / 1024).toFixed(0)} MB of memory`}
          {prof && (prof.status ?? 0) >= 400 && <span className="text-danger">{` (HTTP ${prof.status}: the page is broken)`}</span>}
        </p>
      </div>
      <SimpleTable
        headers={["Plugin", "Version", "Status", "wordpress.org", "Files", "Cost", "Findings"]}
        rows={plugins.map((p) => row(p))}
        rowKey={(i) => plugins[i].slug}
        empty="No plugins installed."
      />
      {prof && others.length > 0 && (
        <Note>
          {"Also: " +
            others.map(([k, v]) => `${k} ${(v.load_ms + v.hook_ms).toFixed(1)} ms, ${v.queries} queries`).join(" · ")}
        </Note>
      )}
      {(rep.theme_signatures ?? []).length > 0 && (
        <Note tone="bad">{"Suspicious theme files: " + (rep.theme_signatures ?? []).join(", ")}</Note>
      )}
      <Partial label="Partial analysis" errors={rep.errors} />
    </div>
  )
}

function row(p: PluginInfo) {
  const flags = p.flags ?? []
  const modified = p.modified ?? []
  return [
    <div className="flex flex-col">
      <strong className="font-semibold">{p.title || p.slug}</strong>
      <span className="text-xs font-normal text-muted-foreground">{p.slug}</span>
    </div>,
    p.version + (p.update_version ? ` → ${p.update_version}` : ""),
    p.status,
    <div className="flex flex-col">
      <span className={cn(p.directory === "closed" && "font-semibold text-danger")}>{DIRECTORY[p.directory] || p.directory}</span>
      {p.last_updated && <span className="text-xs text-muted-foreground">updated {age(p.last_updated)}</span>}
    </div>,
    <div className="flex flex-col">
      <span className={cn(p.checksums === "modified" && "font-semibold text-danger")} title={modified.join("\n") || undefined}>
        {CHECKSUMS[p.checksums] || p.checksums}
      </span>
      {modified.length > 0 && (
        <details className="text-xs text-muted-foreground">
          <summary className="cursor-pointer">{modified.length === 1 ? "1 file differs" : `${plural(modified.length, "file")} differ`}</summary>
          <ul className="mt-1 flex flex-col gap-0.5">
            {modified.map((m) => (
              <li key={m} className={cn(WRAP, "font-mono")}>
                {m}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>,
    p.perf ? (
      <div
        className="flex flex-col"
        title={`load ${p.perf.load_ms} ms (${p.perf.load_kb} KB), hooks ${p.perf.hook_ms} ms in ${p.perf.calls} calls, ${p.perf.queries} queries`}
      >
        <span className="tabular-nums">{(p.perf.load_ms + p.perf.hook_ms).toFixed(1)} ms</span>
        <span className="text-xs text-muted-foreground">{p.perf.queries} queries</span>
      </div>
    ) : (
      "–"
    ),
    <div className="flex flex-col gap-0.5">
      {flags.map((f) => (
        <span key={f} className={cn(WRAP, "text-xs", SECURITY_FLAG.test(f) && "font-medium text-danger")}>
          {f}
        </span>
      ))}
      {(p.signatures ?? []).map((sig) => (
        <span key={sig} className={cn(WRAP, "font-mono text-xs")}>
          {sig}
        </span>
      ))}
    </div>,
  ]
}
