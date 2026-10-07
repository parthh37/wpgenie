import { useState } from "react"
import { HeartPulseIcon, PackageIcon, StethoscopeIcon } from "lucide-react"
import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtBytes, fmtTime, plural } from "@/lib/format"
import { invalidate } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import type { SectionProps } from "../sections"
import { followFixJob, Note, panelWords, Partial, sevClass, worstSeverity, WRAP } from "./protect/shared"
import type { Analysis, Finding, FixResult } from "./protect/types"

// Health: the site analyser's one report on security, performance and
// upkeep, with one-click fixes (wordpress.js renderHealth/showHealth). It
// runs on opening and is kept until something changes or "Analyse now".

const FIX_LABELS: Record<string, string> = {
  page_cache: "Turn on",
  object_cache: "Turn on",
  images: "Convert to WebP",
  optimize: "Apply tweaks",
  db_cleanup: "Clean up",
  scan: "Scan now",
  update_security: "Update",
  update_all: "Update all",
  auto_update: "Turn on",
  default_role: "Make subscriber",
  search_visible: "Allow indexing",
  shield: "Turn on",
  waf: "Turn on",
}

const GRADE_FILL: Record<string, string> = {
  A: "bg-success-fill",
  B: "bg-tint-mint",
  C: "bg-warning-fill",
  D: "bg-warning-fill",
  F: "bg-danger-fill",
}

// The analysis: a few seconds of WP-CLI, so not refetched on focus.
const useAnalysis = (site: Site) =>
  useQuery<Analysis>({
    queryKey: [`/sites/${site.id}/analysis`],
    queryFn: () => api<Analysis>("GET", `/sites/${site.id}/analysis`),
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    retry: false,
  })

export default function HealthSection({ site }: SectionProps) {
  const q = useAnalysis(site)

  const analyse = async () => {
    const r = await q.refetch()
    // With a report already shown, a failed re-run keeps it and says why.
    if (r.error && q.data) showError(r.error)
  }

  return (
    <div className="flex flex-col">
      <Section
        icon={StethoscopeIcon}
        tint="red"
        title="Site report"
        description={
          <>
            One report on the site's security, performance and upkeep: installed versions against known vulnerabilities (from the nightly scan),
            WordPress's own settings, database clutter and what WPGenie does for the site. Findings with a <em>Fix</em> button are fixed in one click.
          </>
        }
        action={
          <Button variant="tinted" size="sm" disabled={q.isFetching} onClick={analyse}>
            <HeartPulseIcon data-icon="inline-start" />
            {q.isFetching ? "Analysing…" : "Analyse now"}
          </Button>
        }
      >
        {q.data ? (
          <Report site={site} a={q.data} />
        ) : q.isError ? (
          <LoadError error={q.error} retry={analyse} className="py-6 shadow-none" />
        ) : (
          <div className="flex flex-col gap-3" aria-busy>
            <div className="flex items-center gap-4">
              <Skeleton className="size-15 rounded-[22.5%]" />
              <div className="flex flex-1 flex-col gap-2">
                <Skeleton className="h-5 w-24 rounded-md" />
                <Skeleton className="h-4 w-2/3 rounded-md" />
              </div>
            </div>
            <Note>Analysing… (a few seconds)</Note>
          </div>
        )}
      </Section>
      {q.data && (q.data.components ?? []).length > 0 && <Components a={q.data} />}
    </div>
  )
}

function Report({ site, a }: { site: Site; a: Analysis }) {
  const findings = a.findings ?? []
  const todo = findings.filter((f) => f.severity !== "info").length
  const f = a.facts
  const facts = f
    ? [
        `WordPress ${f.wp_version}`,
        `PHP ${f.php_version}`,
        plural(f.plugins_active, "active plugin"),
        `database ${fmtBytes(f.db_bytes)}`,
        `autoloaded options ${fmtBytes(f.autoload_bytes)}`,
      ]
    : []
  const when = `Analysed ${fmtTime(a.analysed_at)}` + (a.scanned_at ? ` · vulnerabilities as of the scan of ${fmtTime(a.scanned_at)}` : " · never scanned")

  return (
    <div className="flex flex-col gap-5">
      <div className="flex items-center gap-4">
        <div
          role="img"
          aria-label={`Grade ${a.grade}`}
          className={cn(
            "grid size-15 shrink-0 place-items-center rounded-[22.5%] font-heading text-3xl font-bold text-white",
            GRADE_FILL[a.grade] ?? "bg-tint-gray"
          )}
        >
          {a.grade}
        </div>
        <div className="flex min-w-0 flex-col gap-0.5">
          <strong className="text-[1.0625rem] font-semibold tabular-nums">
            {a.score} / 100
            {todo > 0 && <span className="ml-2 text-sm font-normal text-muted-foreground">· {todo} to look at</span>}
          </strong>
          {facts.length > 0 && <span className="text-sm text-muted-foreground">{facts.join(" · ")}</span>}
          <span className="text-sm text-muted-foreground">{when}</span>
        </div>
      </div>
      {findings.length ? (
        <SimpleTable
          headers={["Severity", "Area", "Finding", ""]}
          rowKey={(i) => findings[i].id || i}
          rows={findings.map((x) => [
            <span className={sevClass(x.severity)}>{x.severity}</span>,
            x.category,
            <div className={cn(WRAP, "min-w-56")}>
              <strong className="font-semibold">{x.title}</strong>
              {x.detail && <p className="text-sm text-muted-foreground">{x.detail}</p>}
            </div>,
            x.fix ? <FixButton site={site} f={x} /> : null,
          ])}
        />
      ) : (
        <Note tone="ok">Nothing to fix.</Note>
      )}
      <Partial label="Partial analysis" errors={a.errors} />
    </div>
  )
}

function FixButton({ site, f }: { site: Site; f: Finding }) {
  const s = useSession()
  const [busy, setBusy] = useState(false)
  if (!s.canChangeSite(site) || !f.fix) return null
  const fix = async () => {
    if (
      (f.fix === "update_all" || f.fix === "update_security") &&
      !(await ask(`Update ${site.primary_domain} now? A snapshot is taken first and restored automatically if the site stops working.`, { ok: "Update" }))
    )
      return
    setBusy(true)
    try {
      const r = await api<FixResult>("POST", `/sites/${site.id}/analysis/fix`, { fix: f.fix })
      const msg = panelWords(r.message)
      notify(r.update_run ? `${msg}. Follow it under Updates.` : msg)
      if (r.job_id) followFixJob(r.job_id)
      // The site and this report (re-analysed, as it's open) change.
      await invalidate("/sites")
    } catch (e) {
      showError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Button variant="tinted" size="sm" disabled={busy} onClick={fix}>
      {FIX_LABELS[f.fix] || "Fix"}
    </Button>
  )
}

function Components({ a }: { a: Analysis }) {
  const cs = a.components ?? []
  return (
    <Section icon={PackageIcon} tint="indigo" title="Installed versions">
      <SimpleTable
        headers={["Component", "Type", "Installed", "Available", "Known vulnerabilities"]}
        rowKey={(i) => `${cs[i].type}:${cs[i].slug}`}
        rows={cs.map((c) => {
          const vs = c.vulns ?? []
          const worst = vs.length ? worstSeverity(vs) : ""
          return [
            c.type === "core" ? "WordPress" : c.slug,
            c.type + (c.status && c.status !== "active" ? ` (${c.status})` : ""),
            c.version,
            c.update_version || "–",
            vs.length ? (
              <span className={sevClass(worst)} title={vs.map((v) => v.title).join("\n")}>
                {`${vs.length} (${worst})`}
              </span>
            ) : (
              <span className="text-success">none</span>
            ),
          ]
        })}
      />
    </Section>
  )
}
