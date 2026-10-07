import { useRef, useState } from "react"
import { cn } from "@/lib/utils"

// LineChart: lines on one scale (so they compare), stretched to the box's
// width, with an optional readout under the pointer. Series colours: c1
// (the tint) to c5; c2 is drawn dashed (blocked, errors).

export interface Series {
  values: number[]
  color: "c1" | "c2" | "c3" | "c4" | "c5"
  area?: boolean
}

export function LineChart({
  series,
  label,
  height = 60,
  className,
  readout,
}: {
  series: Series[]
  label: string
  height?: number
  className?: string
  // readout(i): the text for the i-th point under the pointer.
  readout?: (i: number) => string
}) {
  const W = 300
  const H = height
  const n = Math.max(1, ...series.map((s) => s.values.length))
  const max = Math.max(1, ...series.flatMap((s) => s.values))
  const svgRef = useRef<SVGSVGElement>(null)
  const [hover, setHover] = useState<{ i: number; left: number } | null>(null)
  const readoutRef = useRef<HTMLDivElement>(null)

  return (
    <div className={cn("relative min-w-0", className)}>
      <svg
        ref={svgRef}
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={label}
        className="chart"
        height={H}
        onPointerMove={
          readout
            ? (e) => {
                const r = svgRef.current!.getBoundingClientRect()
                const i = Math.max(0, Math.min(n - 1, Math.round(((e.clientX - r.left) / r.width) * (n - 1))))
                const w = readoutRef.current?.offsetWidth ?? 0
                setHover({ i, left: Math.max(0, Math.min(r.width - w, e.clientX - r.left - w / 2)) })
              }
            : undefined
        }
        onPointerLeave={() => setHover(null)}
      >
        {series.map((s, k) => {
          const step = W / Math.max(1, s.values.length - 1)
          const d = s.values
            .map((v, i) => `${i ? "L" : "M"}${(i * step).toFixed(1)} ${(H - 2 - (v / max) * (H - 6)).toFixed(1)}`)
            .join("")
          return (
            <g key={k}>
              {s.area && <path d={`${d}L${W} ${H}L0 ${H}Z`} className={`area ${s.color}`} />}
              <path d={d} className={`line ${s.color}`} vectorEffect="non-scaling-stroke" />
            </g>
          )
        })}
        {readout && (
          <line
            className={cn("cursor", hover && "on")}
            x1={hover ? (hover.i * W) / Math.max(1, n - 1) : 0}
            x2={hover ? (hover.i * W) / Math.max(1, n - 1) : 0}
            y1={0}
            y2={H}
            vectorEffect="non-scaling-stroke"
          />
        )}
      </svg>
      {readout && (
        <div
          ref={readoutRef}
          hidden={!hover}
          // CSSOM (React's style prop), not a style attribute: the CSP allows it.
          style={{ left: hover?.left ?? 0 }}
          className="pointer-events-none absolute top-0 z-10 -translate-y-full rounded-lg bg-popover px-2.5 py-1 text-xs font-semibold whitespace-nowrap tabular-nums text-popover-foreground shadow-lg ring-1 ring-foreground/10"
        >
          {hover ? readout(hover.i) : ""}
        </div>
      )}
    </div>
  )
}

// hourly lays a sparse hourly series out as n values from start, oldest first.
export function hourly(series: Array<{ hour: string; [k: string]: unknown }>, start: number, n: number, key: string) {
  const out = new Array<number>(n).fill(0)
  for (const p of series) {
    const i = Math.round((new Date(p.hour).getTime() - start) / 36e5)
    if (i >= 0 && i < n) out[i] += Number(p[key] || 0)
  }
  return out
}

export const hoursSince = (start: number) => Math.min(24 * 90, Math.max(2, Math.ceil((Date.now() - start) / 36e5)))
