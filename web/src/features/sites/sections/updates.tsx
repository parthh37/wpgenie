import { useEffect, useId, useRef, useState } from "react"
import { HistoryIcon, MailIcon, SearchIcon, SettingsIcon } from "lucide-react"
import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { LoadError } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime, humanize } from "@/lib/format"
import { invalidate } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "../sections"
import { Note, WRAP } from "./protect/shared"
import { componentsOf, type Inventory, type UpdateHistoryEntry, type WPComponent } from "./protect/types"

// Updates: the automatic update policy, mail through the mail server, what
// can be updated now (checked live, a few seconds) and the history of
// update runs, refreshed every 5 s while one runs (app.js renderUpdates).

const POLL_MS = 5000
// A run that still says "running" after this long has stuck: stop asking.
const POLL_FOR_MS = 50 * 60_000

export default function UpdatesSection({ site }: SectionProps) {
  return (
    <div className="flex flex-col">
      <Settings site={site} />
      <Available site={site} />
      <History site={site} />
    </div>
  )
}

function Settings({ site }: { site: Site }) {
  const s = useSession()
  const id = useId()
  const [busy, setBusy] = useState<"" | "policy" | "smtp">("")

  const setPolicy = async (policy: string) => {
    setBusy("policy")
    try {
      await api("PUT", `/sites/${site.id}/auto-update`, { policy })
      await invalidate("/sites")
    } catch (e) {
      showError(e)
    } finally {
      setBusy("")
    }
  }

  const setSMTP = async (enabled: boolean) => {
    setBusy("smtp")
    try {
      await api("PUT", `/sites/${site.id}/smtp`, { enabled })
      await invalidate("/sites")
    } catch (e) {
      showError(e)
    } finally {
      setBusy("")
    }
  }

  return (
    <Section icon={SettingsIcon} tint="gray" title="Settings">
      <div className="flex flex-col gap-5">
        <Field className="w-auto max-w-xs">
          <FieldLabel htmlFor={`${id}-policy`}>Automatic updates</FieldLabel>
          <NativeSelect
            id={`${id}-policy`}
            className="w-full"
            value={site.auto_update}
            disabled={!s.canChange || busy === "policy"}
            onChange={(e) => setPolicy(e.target.value)}
          >
            <NativeSelectOption value="off">Off</NativeSelectOption>
            <NativeSelectOption value="security">Security fixes only</NativeSelectOption>
            <NativeSelectOption value="all">Everything</NativeSelectOption>
          </NativeSelect>
          <FieldDescription>Every update takes a snapshot first; if the site stops working afterwards it is restored automatically.</FieldDescription>
        </Field>
        <Label className="items-start font-normal">
          <Switch className="mt-0.5" checked={site.smtp} disabled={!s.canChange || busy === "smtp"} onCheckedChange={setSMTP} />
          <span className="flex items-center gap-1.5">
            <MailIcon className="size-4 text-muted-foreground" />
            Send WordPress mail through the mail server
          </span>
        </Label>
      </div>
    </Section>
  )
}

// Available: "Check for updates" asks WP-CLI what can be updated; the
// chosen ones are updated in one run.
function Available({ site }: { site: Site }) {
  const s = useSession()
  const [inv, setInv] = useState<Inventory | null>(null)
  const [checking, setChecking] = useState(false)
  const [started, setStarted] = useState(false)

  const check = async () => {
    setChecking(true)
    try {
      setInv(await api<Inventory>("GET", `/sites/${site.id}/updates`))
      setStarted(false)
    } catch (e) {
      showError(e)
    } finally {
      setChecking(false)
    }
  }

  return (
    <Section
      icon={SearchIcon}
      tint="blue"
      title="Available updates"
      action={
        <Button variant="tinted" size="sm" disabled={checking} onClick={check}>
          {checking ? "Checking…" : "Check for updates"}
        </Button>
      }
    >
      {checking && !inv ? (
        <Skeleton className="h-24 rounded-xl" />
      ) : started ? (
        <Note>Update running: snapshot, update, health check. The result appears below.</Note>
      ) : inv ? (
        <Pending key={JSON.stringify(inv)} site={site} inv={inv} canChange={s.canChange} onStarted={() => setStarted(true)} />
      ) : (
        <Note>Check to see what WordPress, its plugins and themes can be updated to.</Note>
      )}
    </Section>
  )
}

const keyOf = (c: WPComponent) => `${c.type}:${c.slug}`

function Pending({ site, inv, canChange, onStarted }: { site: Site; inv: Inventory; canChange: boolean; onStarted: () => void }) {
  const pending = componentsOf(inv).filter((c) => c.update_version)
  const [picked, setPicked] = useState(() => new Set(pending.map(keyOf)))
  const [busy, setBusy] = useState(false)
  if (!pending.length) return <Note tone="ok">Everything is up to date.</Note>

  const chosen = pending.filter((c) => picked.has(keyOf(c)))
  const toggle = (k: string, on: boolean) =>
    setPicked((p) => {
      const n = new Set(p)
      if (on) n.add(k)
      else n.delete(k)
      return n
    })

  const run = async () => {
    const req = { core: false, plugins: [] as string[], themes: [] as string[] }
    for (const c of chosen) {
      if (c.type === "core") req.core = true
      else if (c.type === "plugin") req.plugins.push(c.slug)
      else if (c.type === "theme") req.themes.push(c.slug)
    }
    setBusy(true)
    try {
      await api("POST", `/sites/${site.id}/updates`, req)
      onStarted()
      await invalidate(`/sites/${site.id}/updates/history`)
    } catch (e) {
      showError(e)
      setBusy(false)
    }
  }

  const all = chosen.length === pending.length
  return (
    <div className="flex flex-col gap-4">
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="w-8">
              {canChange && (
                <Checkbox
                  aria-label="Select all"
                  checked={all}
                  indeterminate={!all && chosen.length > 0}
                  onCheckedChange={(v) => setPicked(v ? new Set(pending.map(keyOf)) : new Set())}
                />
              )}
            </TableHead>
            <TableHead>Component</TableHead>
            <TableHead>Installed</TableHead>
            <TableHead>Available</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {pending.map((c) => {
            const k = keyOf(c)
            const name = `${c.type === "core" ? "WordPress" : c.slug} (${c.type})`
            return (
              <TableRow key={k}>
                <TableCell>
                  {canChange && <Checkbox aria-label={`Update ${name}`} checked={picked.has(k)} onCheckedChange={(v) => toggle(k, v)} />}
                </TableCell>
                <TableCell className="font-medium">{name}</TableCell>
                <TableCell>{c.version}</TableCell>
                <TableCell>{c.update_version}</TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
      {canChange && (
        <div>
          <Button disabled={busy || !chosen.length} onClick={run}>
            Update {chosen.length} selected
          </Button>
        </div>
      )}
    </div>
  )
}

// History: the last ten runs, polled while the newest is running.
function History({ site }: { site: Site }) {
  const path = `/sites/${site.id}/updates/history?limit=10`
  const q = useQuery<UpdateHistoryEntry[]>({
    queryKey: [path],
    queryFn: () => api<UpdateHistoryEntry[]>("GET", path),
    refetchInterval: (query) => {
      const run = query.state.data?.[0]?.run
      return run?.status === "running" && Date.now() - new Date(run.started_at).getTime() < POLL_FOR_MS ? POLL_MS : false
    },
  })

  // A run that ends changes what's installed: the health report and the
  // scan are out of date.
  const newest = q.data?.[0]?.run
  const was = useRef<string | undefined>(undefined)
  useEffect(() => {
    const now = newest ? `${newest.id}:${newest.status}` : undefined
    if (was.current?.endsWith(":running") && now !== was.current) {
      invalidate(`/sites/${site.id}/analysis`)
      invalidate(`/sites/${site.id}/plugins`)
    }
    was.current = now
  }, [newest, site.id])

  return (
    <Section icon={HistoryIcon} tint="brown" title="History">
      {q.isPending ? (
        <Skeleton className="h-24 rounded-xl" />
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <SimpleTable
          headers={["Started", "Trigger", "Result", "Summary"]}
          empty="No updates have run yet."
          rowKey={(i) => q.data[i].run.id}
          rows={q.data.map(({ run }) => [
            <span className="whitespace-nowrap">{fmtTime(run.started_at)}</span>,
            humanize(run.trigger),
            <StatusPill status={run.status} />,
            <span className={WRAP}>{run.summary}</span>,
          ])}
        />
      )}
    </Section>
  )
}
