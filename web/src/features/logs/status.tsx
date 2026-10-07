import { useState, type ReactNode } from "react"
import { CheckIcon, HardDriveIcon, RefreshCwIcon, TriangleAlertIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Switch } from "@/components/ui/switch"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { ActionButton } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { IconTile, type Tint } from "@/components/app/icon-tile"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { fmtBytes, fmtNum, fmtTime } from "@/lib/format"
import { cn } from "@/lib/utils"
import {
  destinationReady, logAgo, providerOf, saveLogSettings, typeInfo, type DayVolume, type LogSettings, type LogStatus,
} from "./data"

// The shipping status (the hero) and what's collected (a card per kind of
// log, with its switch for administrators).

const HEALTH: Record<string, { icon: typeof CheckIcon; label: string; tint: Tint; bg: string }> = {
  ok: { icon: CheckIcon, label: "Shipping", tint: "green", bg: "bg-success-fill/10" },
  warning: { icon: TriangleAlertIcon, label: "Needs a look", tint: "orange", bg: "bg-warning-fill/10" },
  error: { icon: TriangleAlertIcon, label: "Not shipping", tint: "red", bg: "bg-danger-fill/10" },
  off: { icon: HardDriveIcon, label: "Off", tint: "gray", bg: "bg-card" },
}

const dot = (health: string | undefined) =>
  health === "ok" ? "bg-success-fill" : health === "warning" ? "bg-warning-fill" : health === "error" ? "bg-danger-fill" : "bg-muted-foreground"

// Shows the destination form and puts the cursor in it.
function goToDestination() {
  const f = document.getElementById("logs-dest")
  f?.scrollIntoView({ behavior: "smooth", block: "start" })
  document.getElementById("logs-endpoint")?.focus({ preventScroll: true })
}

export function LogsHero({ st, set, onRefresh, refreshing }: { st: LogStatus; set: LogSettings | null; onRefresh: () => void; refreshing: boolean }) {
  const hl = HEALTH[st.health] ?? HEALTH.warning
  const d = st.destination
  const where = d.bucket ? `${providerOf(d.provider).name} · ${d.bucket}/${d.prefix || ""}` : "no destination yet"
  const spoolPct = st.spool.cap ? Math.min(100, (st.spool.bytes / st.spool.cap) * 100) : 0
  const version = st.shipper.version || (st.shipper.image.split("@")[0].split(":")[1] || "").replace("-alpine", "")

  return (
    <section aria-labelledby="logs-hero-title" className={cn("mb-4 rounded-2xl p-4 card-shadow sm:p-5", st.enabled ? hl.bg : "bg-card")}>
      <div className="flex flex-wrap items-start gap-4">
        <IconTile icon={hl.icon} tint={st.enabled ? hl.tint : "gray"} size="lg" />
        <div className="min-w-0 flex-[1_1_320px]">
          <h2 id="logs-hero-title" className="text-[1.0625rem] font-semibold [overflow-wrap:anywhere]">
            {st.enabled ? `${hl.label} to ${where}` : "Log shipping is off"}
          </h2>
          <p className="mt-0.5 max-w-[72ch] text-sm" role={st.health === "error" ? "alert" : undefined}>
            {st.health_message}
          </p>
          {!st.enabled && (
            <p className="mt-1 max-w-[72ch] text-sm text-muted-foreground">
              Turn it on to keep every log in your own storage (Amazon S3, Cloudflare R2, Backblaze B2…) instead of on this server's disk: nothing
              fills it up, and you can look back months later.
            </p>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-2 self-center">
          <Button variant="tinted" onClick={onRefresh} disabled={refreshing}>
            <RefreshCwIcon data-icon="inline-start" className={cn(refreshing && "animate-spin")} />
            Refresh
          </Button>
          {set && (
            <ActionButton
              variant={st.enabled ? "destructive" : "default"}
              run={async () => {
                const on = !st.enabled
                if (on) {
                  if (!destinationReady(set.destination)) {
                    notify('First tell WPGenie where logs go, then "Save and turn on shipping".')
                    goToDestination()
                    return
                  }
                } else if (
                  !(await ask(
                    "Turn off log shipping? Logs stay on this server only from now on (and old ones are rotated away as before). What is already in the bucket stays there."
                  ))
                )
                  return
                await saveLogSettings({ enabled: on })
                notify(on ? "Log shipping is on. The shipper starts in a moment." : "Log shipping is off.")
              }}
            >
              {st.enabled ? "Turn off" : "Turn on shipping"}
            </ActionButton>
          )}
        </div>
      </div>

      {st.enabled && (
        <div className="mt-4 grid grid-cols-[repeat(auto-fit,minmax(170px,1fr))] gap-3">
          <Fact
            k="Last upload"
            v={st.last_upload ? logAgo(st.last_upload, fmtTime) : "None yet"}
            s={st.last_upload ? fmtTime(st.last_upload) : "objects are written every few minutes"}
          />
          <Fact
            k="Uploaded"
            v={`${fmtNum(st.sent_events)} lines`}
            s={st.sent_bytes ? `${fmtBytes(st.sent_bytes)} since the shipper started` : "since the shipper started"}
          />
          <Fact
            k="Waiting on this server"
            v={fmtBytes(st.spool.bytes)}
            s={
              `of ${fmtBytes(st.spool.cap)} allowed` +
              (st.dropped_today ? ` · ${fmtNum(st.dropped_today)} dropped today` : "") +
              (st.buffer_bytes ? ` · ${fmtBytes(st.buffer_bytes)} more in the shipper's buffer (up to 256 MB)` : "")
            }
          >
            <div
              role="meter"
              aria-label="Space for waiting logs used"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={Math.round(spoolPct)}
              className="my-1 h-1.5 overflow-hidden rounded-full bg-muted"
            >
              <div
                className={cn("h-full rounded-full", spoolPct >= 80 ? "bg-danger-fill" : spoolPct >= 50 ? "bg-warning-fill" : "bg-success-fill")}
                style={{ width: `${spoolPct}%` }}
              />
            </div>
          </Fact>
          <Fact
            k="Shipper"
            v={st.shipper.state ? st.shipper.state[0].toUpperCase() + st.shipper.state.slice(1) : "Starting"}
            s={`Vector ${version}`}
          />
        </div>
      )}

      {!!st.servers?.length && (
        <div className="mt-4 flex flex-wrap items-center gap-1.5">
          <span className="mr-1 text-sm text-muted-foreground">Every server ships its own logs:</span>
          <Chip health={st.health}>{st.server} (this panel)</Chip>
          {st.servers.map((x) => (
            <Tooltip key={x.id}>
              <TooltipTrigger render={<span />}>
                <Chip health={x.status ? x.status.health : "error"}>
                  {x.name || x.id}
                  {x.status ? "" : " · unreachable"}
                </Chip>
              </TooltipTrigger>
              {(x.status?.health_message || x.error) && <TooltipContent className="max-w-sm">{x.status ? x.status.health_message : x.error}</TooltipContent>}
            </Tooltip>
          ))}
        </div>
      )}

      {st.retention?.error && <p className="mt-3 text-sm text-danger">Deleting old archives failed: {st.retention.error}</p>}
      {st.export_error && <p className="mt-2 text-sm text-warning">{st.export_error}</p>}
    </section>
  )
}

function Fact({ k, v, s, children }: { k: string; v: ReactNode; s: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5 rounded-xl bg-card/80 p-3 ring-1 ring-border/60">
      <span className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{k}</span>
      <span className="truncate text-xl font-bold tracking-[-0.01em] tabular-nums">{v}</span>
      {children}
      <span className="text-xs text-muted-foreground">{s}</span>
    </div>
  )
}

function Chip({ health, children }: { health: string; children: ReactNode }) {
  return (
    <span className="inline-flex h-7 items-center gap-1.5 rounded-full bg-secondary px-2.5 text-sm">
      <span aria-hidden className={cn("size-[7px] rounded-full", dot(health))} />
      {children}
    </span>
  )
}

// ---- What's collected ----

export function LogTypes({ st, set }: { st: LogStatus; set: LogSettings | null }) {
  // A switch being saved shows the new position until the status catches up.
  const [pending, setPending] = useState<Record<string, boolean>>({})

  async function toggle(name: string, title: string, checked: boolean) {
    setPending((p) => ({ ...p, [name]: checked }))
    try {
      await saveLogSettings({ types: { [name]: checked } })
      notify(`${title}: ${checked ? "shipped" : "not shipped any more"}`)
    } catch (e) {
      showError(e)
    } finally {
      setPending((p) => {
        const next = { ...p }
        delete next[name]
        return next
      })
    }
  }

  return (
    <Section
      title="What's collected"
      description="Each kind of log can be shipped or not. The numbers are this server's: what was collected today, and the last two weeks."
    >
      <div className="grid grid-cols-[repeat(auto-fill,minmax(250px,1fr))] gap-3">
        {st.types.map((t) => {
          const info = typeInfo(t.name)
          const checked = pending[t.name] ?? t.enabled
          const on = t.enabled && st.enabled
          return (
            <article
              key={t.name}
              className={cn(
                "flex flex-col gap-2 rounded-2xl p-4 ring-1 transition-colors",
                on ? "bg-primary/6 ring-primary/25" : "bg-muted/40 ring-border/70",
                !t.available && "opacity-75"
              )}
            >
              <div className="flex items-center gap-2.5">
                <IconTile icon={info.icon} tint={on ? info.tint : "gray"} />
                <h3 className="flex-1 text-sm font-semibold">{info.title}</h3>
                {set ? (
                  <Switch
                    aria-label={`Ship ${info.title}`}
                    checked={checked}
                    disabled={!t.available || t.name in pending}
                    onCheckedChange={(c) => toggle(t.name, info.title, c)}
                  />
                ) : (
                  <Badge variant="secondary" className={cn(on && "bg-primary/12 text-primary")}>
                    {on ? "shipped" : "not shipped"}
                  </Badge>
                )}
              </div>
              <p className="text-sm text-muted-foreground">{info.desc}</p>
              {!t.available && (
                <p className="text-sm text-warning">
                  {t.name === "mail"
                    ? "No mail server runs on this server."
                    : t.name === "containers"
                      ? "Needs Docker's default json-file logging."
                      : "Not available on this server."}
                </p>
              )}
              <div className="mt-auto flex items-end justify-between gap-3 pt-1 max-sm:flex-col max-sm:items-start">
                <div className="flex min-w-0 flex-col">
                  <span className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">Today</span>
                  <span className="text-sm font-semibold tabular-nums">{t.today.events ? `${fmtNum(t.today.events)} lines` : "Nothing yet"}</span>
                  {!!t.today.events && <span className="text-xs text-muted-foreground">{fmtBytes(t.today.bytes)}</span>}
                  {!!t.today.dropped && <span className="text-xs text-danger">{fmtNum(t.today.dropped)} dropped</span>}
                </div>
                <VolumeBars history={t.history ?? []} title={info.title} />
              </div>
            </article>
          )
        })}
      </div>
    </Section>
  )
}

// VolumeBars is a small bar chart of a kind's last days (bytes), with a
// text alternative and a tooltip per day.
function VolumeBars({ history, title }: { history: DayVolume[]; title: string }) {
  const w = 112
  const hgt = 28
  const gap = 2
  const n = history.length
  if (!n) return null
  const max = Math.max(1, ...history.map((d) => d.bytes))
  const bw = (w - gap * (n - 1)) / n
  const total = history.reduce((a, d) => a + d.bytes, 0)
  return (
    <svg viewBox={`0 0 ${w} ${hgt}`} className="h-7 w-28 shrink-0" role="img" aria-label={`${title}, last ${n} days: ${fmtBytes(total)} in total`}>
      {history.map((d, i) => {
        const bh = d.bytes ? Math.max(2, (d.bytes / max) * (hgt - 2)) : 1
        return (
          <rect
            key={d.day}
            x={(i * (bw + gap)).toFixed(1)}
            y={(hgt - bh).toFixed(1)}
            width={bw.toFixed(1)}
            height={bh.toFixed(1)}
            rx={1}
            className={d.bytes ? (i === n - 1 ? "fill-primary" : "fill-primary/45") : "fill-border"}
          >
            <title>{`${d.day}: ${fmtNum(d.events)} lines, ${fmtBytes(d.bytes)}` + (d.dropped ? `, ${fmtNum(d.dropped)} dropped` : "")}</title>
          </rect>
        )
      })}
    </svg>
  )
}
