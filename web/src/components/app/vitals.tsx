import { CpuIcon, HeartPulseIcon, ShieldCheckIcon, type LucideIcon } from "lucide-react"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { IconTile, type Tint } from "@/components/app/icon-tile"
import { fmtPct } from "@/lib/format"
import { cn } from "@/lib/utils"

// A site's vitals: responses without a server error (24 h), CPU headroom
// (now) and protection, each 0–100 or null while not known. Shown as
// labelled bars coloured by how they're doing (green, orange,
// red), never by colour alone: every bar has its words and its number.

export type VitalValues = [number | null | undefined, number | null | undefined, number | null | undefined]
export type VitalTone = "ok" | "warn" | "bad" | "none"

const VITALS: { name: string; icon: LucideIcon; tint: Tint }[] = [
  { name: "Healthy responses", icon: HeartPulseIcon, tint: "pink" },
  { name: "CPU headroom", icon: CpuIcon, tint: "blue" },
  { name: "Protection", icon: ShieldCheckIcon, tint: "green" },
]

// toneOfVital: below 99% healthy responses some visitors see errors; below
// 30% headroom the site is busy; protection off (or not on every site) is a
// choice worth a second look, never an emergency.
export function toneOfVital(i: number, v: number | null | undefined): VitalTone {
  if (v == null) return "none"
  if (i === 0) return v >= 99 ? "ok" : v >= 95 ? "warn" : "bad"
  if (i === 1) return v >= 30 ? "ok" : v >= 10 ? "warn" : "bad"
  return v >= 100 ? "ok" : "warn"
}

// vitalSaid: what each vital says in words ("99.8%", "On").
export function vitalSaid(values: VitalValues, protectionAsShare = false): string[] {
  return values.map((v, i) =>
    v == null ? "Not measured yet" : i === 2 && !protectionAsShare ? (v ? "On" : "Off") : fmtPct(v)
  )
}

const FILL: Record<VitalTone, string> = {
  ok: "bg-success-fill",
  warn: "bg-warning-fill",
  bad: "bg-danger-fill",
  none: "",
}
const TEXT: Record<VitalTone, string> = { ok: "text-foreground", warn: "text-warning", bad: "text-danger", none: "text-faint" }

function Gauge({ value, tone, className }: { value: number | null | undefined; tone: VitalTone; className?: string }) {
  return (
    <span className={cn("relative block h-1.5 overflow-hidden rounded-full bg-fill", className)}>
      {value != null && (
        <span
          className={cn("vital-fill absolute inset-y-0 left-0 rounded-full", FILL[tone])}
          style={{ width: `${Math.max(0, Math.min(100, value))}%` }}
        />
      )}
    </span>
  )
}

// VitalBars: the three vitals, each a labelled gauge, for cards.
export function VitalBars({ values, protectionAsShare, className }: { values: VitalValues; protectionAsShare?: boolean; className?: string }) {
  const said = vitalSaid(values, protectionAsShare)
  return (
    <ul aria-label="Vitals" className={cn("m-0 flex list-none flex-col gap-3.5 p-0", className)}>
      {VITALS.map((x, i) => {
        const tone = toneOfVital(i, values[i])
        return (
          <li key={x.name} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-2.5 gap-y-2">
            <IconTile icon={x.icon} tint={x.tint} size="xs" />
            <span className="truncate text-sm font-medium">{protectionAsShare && i === 2 ? "Sites protected" : x.name}</span>
            <span className={cn("text-right text-sm font-semibold tabular-nums", TEXT[tone], tone === "none" && "text-xs font-medium")}>{said[i]}</span>
            <Gauge value={values[i]} tone={tone} className="col-span-3" />
          </li>
        )
      })}
    </ul>
  )
}

// VitalMeter: the same three gauges, small and stacked, for a row; the
// tooltip (and the accessible name) says them in words.
export function VitalMeter({ values, className }: { values: VitalValues; className?: string }) {
  const said = vitalSaid(values)
  const label = VITALS.map((x, i) => `${x.name}: ${said[i]}`).join(", ")
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <span role="img" aria-label={label} className={cn("flex w-12 shrink-0 flex-col gap-[3px] py-1", className)}>
            {values.map((v, i) => (
              <Gauge key={i} value={v} tone={toneOfVital(i, v)} className="h-1" />
            ))}
          </span>
        }
      />
      <TooltipContent className="flex flex-col gap-0.5">
        {VITALS.map((x, i) => (
          <span key={x.name}>
            {x.name}: <strong>{said[i]}</strong>
          </span>
        ))}
      </TooltipContent>
    </Tooltip>
  )
}
