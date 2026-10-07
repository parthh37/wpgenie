import { useId } from "react"
import { fmtPct } from "@/lib/format"
import { cn } from "@/lib/utils"

// Activity rings: three goals per site, full when all is well, as on the
// Watch: responses without a server error (24 h), CPU headroom (now) and
// protection. A value not known yet draws the ring's track alone; the
// numbers are written beside the rings too, so nothing depends on colour.

export const RING_NAMES = ["Healthy responses", "CPU headroom", "Protection"] as const
const R = [44, 32, 20]

export type RingValues = [number | null | undefined, number | null | undefined, number | null | undefined]

// ringSaid: what each ring says in words ("99.8%", "On").
export function ringSaid(values: RingValues, protectionAsShare = false): string[] {
  return values.map((v, i) =>
    i === 2 && !protectionAsShare ? (v == null ? "–" : v ? "On" : "Off") : fmtPct(v)
  )
}

export function ActivityRings({
  values,
  className,
  protectionAsShare,
}: {
  values: RingValues
  className?: string
  protectionAsShare?: boolean
}) {
  const id = useId().replace(/:/g, "")
  const said = ringSaid(values, protectionAsShare)
  const label = RING_NAMES.map((n, i) => `${n}: ${said[i] === "–" ? "not known yet" : said[i]}`).join(", ")
  return (
    <svg viewBox="0 0 100 100" role="img" aria-label={label} className={cn("rings block shrink-0 overflow-visible", className)}>
      <defs>
        {[1, 2, 3].map((n) => (
          <linearGradient key={n} id={`${id}-g${n}`} x1="0" y1="0" x2="1" y2="1">
            <stop offset="0" className={`g${n}a`} />
            <stop offset="1" className={`g${n}b`} />
          </linearGradient>
        ))}
      </defs>
      {R.map((r, i) => {
        const v = values[i]
        return (
          <g key={i}>
            <circle cx={50} cy={50} r={r} strokeWidth={10.5} className={`track r${i + 1}`} />
            {v != null && (
              <circle
                cx={50}
                cy={50}
                r={r}
                strokeWidth={10.5}
                pathLength={100}
                strokeDasharray={`${Math.max(0, Math.min(100, v))} 100`}
                transform="rotate(-90 50 50)"
                stroke={`url(#${id}-g${i + 1})`}
                className={`arc r${i + 1}`}
              />
            )}
          </g>
        )
      })}
    </svg>
  )
}

const LEGEND_DOT = ["bg-[var(--ring-1a)]", "bg-[var(--ring-2a)]", "bg-[var(--ring-3a)]"]
const LEGEND_VALUE = ["dark:text-[var(--ring-1a)]", "dark:text-[var(--ring-2a)]", "dark:text-[var(--ring-3a)]"]

export function RingLegend({ values, protectionAsShare, className }: { values: RingValues; protectionAsShare?: boolean; className?: string }) {
  const said = ringSaid(values, protectionAsShare)
  return (
    <dl aria-hidden className={cn("m-0 grid gap-1.5 text-[0.8125rem]", className)}>
      {RING_NAMES.map((name, i) => (
        <div key={name} className="grid grid-cols-[auto_1fr] items-baseline gap-x-2">
          <dt className={cn("row-span-2 mt-1 size-2.5 rounded-full", LEGEND_DOT[i])} />
          <dd className={cn("m-0 font-heading text-[1.0625rem] font-bold tracking-[-0.02em] tabular-nums", LEGEND_VALUE[i])}>{said[i]}</dd>
          <dd className="m-0 text-xs text-muted-foreground">{name}</dd>
        </div>
      ))}
    </dl>
  )
}
