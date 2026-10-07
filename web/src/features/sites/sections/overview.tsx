import { useState } from "react"
import { ChevronRightIcon, ShieldIcon, Trash2Icon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Switch } from "@/components/ui/switch"
import { VitalBars } from "@/components/app/vitals"
import { askText } from "@/components/app/confirm"
import { IconTile } from "@/components/app/icon-tile"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtBytes, fmtNum, fmtTime } from "@/lib/format"
import { invalidate, queryClient, useClustered } from "@/lib/query"
import { href, navigate, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { useAttack, useSiteCPU, useSiteStats, vitals } from "../data"
import { Spark } from "../list"
import { sectionsFor, SECTIONS, type SectionProps } from "../sections"

// The everyday jobs on a site, in the words an owner would use, each
// opening the section that does it. Only the first six this user can reach.
const TASKS: Array<[key: string, title: string, sub: string]> = [
  ["backups", "Back up or restore", "Save a copy now, or roll back to one"],
  ["wordpress", "Sign in to WordPress", "Open wp-admin without a password"],
  ["files", "Edit files", "Browse, upload and change your site’s files"],
  ["domains", "Add a domain", "Point a domain here, HTTPS set up for you"],
  ["staging", "Try changes safely", "Work on a private copy, then publish"],
  ["updates", "Update WordPress", "Core, plugins and themes in one place"],
  ["performance", "Speed up the site", "Caching and room for busy days"],
]

export default function OverviewSection({ site }: SectionProps) {

  const clustered = useClustered()
  const { data: stats } = useSiteStats(site)
  const { data: cpu } = useSiteCPU(site)
  const vs = vitals(site, stats, cpu)
  const have = new Set(sectionsFor(site, clustered).map((x) => x.key))
  const tasks = TASKS.filter(([k]) => have.has(k)).slice(0, 6)

  const cpuLine = cpu
    ? [
        `CPU ${cpu.percent}% of each instance's allowance across ${cpu.replicas} instance(s)`,
        cpu.workers_percent != null ? `PHP workers ${cpu.workers_percent}% busy` + (cpu.queued ? `, ${cpu.queued} request(s) waiting` : "") : null,
        cpu.p95_ms != null ? `95% of the last minute's ${cpu.responses} responses within ${Math.round(cpu.p95_ms)} ms` : null,
      ]
        .filter(Boolean)
        .join(" · ") + `; sampled ${fmtTime(cpu.at)}.`
    : ""

  return (
    <div className="flex flex-col gap-4">
      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_18rem]">
        <Card>
          <CardContent className="flex flex-col gap-5">
            <dl className="m-0 grid grid-cols-[repeat(auto-fill,minmax(6.5rem,1fr))] gap-x-4 gap-y-3">
              <Stat k="Visitors 24h" v={stats ? fmtNum(stats.unique_visitors) : "–"} />
              <Stat k="Page views" v={stats ? fmtNum(stats.totals.page_views) : "–"} />
              <Stat k="Bandwidth" v={stats ? fmtBytes(stats.totals.bytes_out) : "–"} />
              <Stat k="Blocked" v={stats ? fmtNum(stats.totals.blocked) : "–"} />
              <Stat k="Bot hits" v={stats ? fmtNum(stats.totals.bot_hits) : "–"} />
              <Stat k="CPU now" v={cpu ? `${cpu.percent}%` : "–"} />
            </dl>
            {site.status === "active" ? <Spark stats={stats} height={110} /> : <p className="text-sm text-muted-foreground">Numbers appear once the site is live.</p>}
            {cpuLine && <p className="text-xs text-muted-foreground">{cpuLine}</p>}
          </CardContent>
        </Card>
        <Card>
          <CardContent className="flex h-full flex-col gap-4">
            <h3 className="text-[1.0625rem] font-semibold">Vitals</h3>
            <VitalBars values={vs} className="flex-1 justify-evenly" />
          </CardContent>
        </Card>
      </div>

      {tasks.length > 0 && (
        <nav aria-label="Common tasks">
          <h3 className="mb-3 text-[1.0625rem] font-semibold">Common tasks</h3>
          <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
            {tasks.map(([key, title, sub]) => {
              const def = SECTIONS.find((x) => x.key === key)!
              return (
                <a
                  key={key}
                  href={href(sitePath(site.id, key))}
                  onClick={(e) => {
                    e.preventDefault()
                    navigate(sitePath(site.id, key), { replace: true })
                  }}
                  className="group grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3.5 rounded-2xl bg-card px-4 py-3.5 text-foreground no-underline card-shadow transition-transform hover:-translate-y-0.5 hover:no-underline active:scale-[.97]"
                >
                  <IconTile icon={def.icon} tint={def.tint} size="lg" className="row-span-2" />
                  <strong className="text-[0.9375rem] font-semibold">{title}</strong>
                  <ChevronRightIcon className="row-span-2 size-4 text-muted-foreground" />
                  <span className="text-sm text-muted-foreground">{sub}</span>
                </a>
              )
            })}
          </div>
        </nav>
      )}

      {site.status === "active" && <ProtectionControls site={site} />}

      <DangerZone site={site} />
    </div>
  )
}

const Stat = ({ k, v }: { k: string; v: string }) => (
  <div className="flex flex-col">
    <dt className="truncate text-xs text-muted-foreground">{k}</dt>
    <dd className="m-0 font-heading text-xl font-bold tracking-[-0.02em] whitespace-nowrap tabular-nums">{v}</dd>
  </div>
)

// The visitor check and AI crawlers, the two protection choices everyone
// makes; the rest is in Protection.
function ProtectionControls({ site }: { site: Site }) {
  const s = useSession()
  const { data: attack, refetch } = useAttack(site)
  const [busy, setBusy] = useState(false)

  const save = async (mode: string, ai: boolean) => {
    setBusy(true)
    // Optimistic: the chip and rings follow at once.
    queryClient.setQueryData<Site[]>(["/sites"], (xs) => xs?.map((x) => (x.id === site.id ? { ...x, shield_mode: mode, block_ai_bots: ai } : x)))
    try {
      await api("PUT", `/sites/${site.id}/shield`, { mode, block_ai_bots: ai })
    } catch (e) {
      showError(e)
    }
    await invalidate("/sites")
    setBusy(false)
  }

  const over = async () => {
    try {
      await api("DELETE", `/sites/${site.id}/attack`)
      notify("Visitors are no longer all checked")
    } catch (e) {
      showError(e)
    }
    refetch()
  }

  return (
    <Section icon={ShieldIcon} tint="green" title="Protection" className="mb-0">
      {attack?.active && attack.attack && (
        <div role="status" className="mb-4 flex flex-wrap items-center gap-3 rounded-xl bg-danger-fill/12 p-3 text-sm">
          <p className="min-w-0 flex-1">
            <strong className="font-semibold text-danger">Under attack</strong> since {fmtTime(attack.attack.since)}: {attack.attack.reason}. Every visitor is
            checked automatically, and your site stays online. This ends by itself once the attack stops.
          </p>
          {s.canChangeSite(site, "manager") && (
            <Button variant="tinted" size="sm" onClick={over}>
              It's over
            </Button>
          )}
        </div>
      )}
      <div className="flex flex-wrap items-end gap-x-8 gap-y-4">
        <Field className="w-auto min-w-64">
          <FieldLabel htmlFor={`mode-${site.id}`}>Visitor check</FieldLabel>
          <NativeSelect
            id={`mode-${site.id}`}
            value={site.shield_mode}
            disabled={busy || !s.canChangeSite(site, "manager")}
            onChange={(e) => save(e.target.value, site.block_ai_bots)}
          >
            <NativeSelectOption value="auto">Automatic</NativeSelectOption>
            <NativeSelectOption value="standard">Suspicious visitors only</NativeSelectOption>
            <NativeSelectOption value="under_attack">Everyone (under attack)</NativeSelectOption>
            <NativeSelectOption value="off">Off: no protection</NativeSelectOption>
          </NativeSelect>
        </Field>
        <Label className="h-9 font-normal">
          <Switch checked={site.block_ai_bots} disabled={busy || !s.canChangeSite(site, "manager")} onCheckedChange={(v) => save(site.shield_mode, v)} />
          Block AI crawlers
        </Label>
      </div>
      <FieldDescription className="mt-3">
        <em>Automatic</em> checks suspicious visitors, and every visitor while an attack is detected: a quick check in the browser, no CAPTCHA puzzle. More
        under{" "}
        <a
          href={href(sitePath(site.id, "protection"))}
          onClick={(e) => {
            e.preventDefault()
            navigate(sitePath(site.id, "protection"), { replace: true })
          }}
        >
          Protection
        </a>
        .
      </FieldDescription>
    </Section>
  )
}

function DangerZone({ site }: { site: Site }) {
  const s = useSession()
  // Operators may delete staging sites; live ones need an admin (or tenant).
  // A site shared with you is its owner's to delete.
  if (site.access || (!s.canCreate && !(site.parent_id && s.canChange))) return null
  const del = async () => {
    const typed = await askText(
      `This permanently deletes ${site.primary_domain}, its files and database (its backups stay in their destinations and can be restored as a new site).`,
      { title: `Delete ${site.primary_domain}?`, label: "Type the domain to confirm", match: site.primary_domain, ok: "Delete site" }
    )
    if (typed !== site.primary_domain) return
    try {
      await api("DELETE", `/sites/${site.id}`)
      notify(`${site.primary_domain} deleted`)
      navigate("/sites")
      await invalidate("/sites")
    } catch (e) {
      showError(e)
    }
  }
  return (
    <div className="flex flex-wrap items-center gap-4 rounded-2xl border border-danger/30 bg-danger-fill/5 p-4">
      <div className="min-w-0 flex-1">
        <h3 className="text-[0.9375rem] font-semibold">Delete this site</h3>
        <p className="max-w-[60ch] text-sm text-muted-foreground">
          Removes its files and database for good. Its backups stay in their destinations and can be restored as a new site.
        </p>
      </div>
      <Button variant="destructive" onClick={del}>
        <Trash2Icon data-icon="inline-start" />
        Delete site
      </Button>
    </div>
  )
}
