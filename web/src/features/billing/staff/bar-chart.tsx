import { useEffect, useId, useRef, useState } from "react"
import { BTable } from "@/components/app/blocks"
import { currency, money, monthLabel } from "@/lib/money"
import { cn } from "@/lib/utils"

// BarChart draws amounts by month as bars from a zero baseline, the latest
// month in full colour; the numbers are in its description and in a table
// under it, and each bar shows its value under the pointer. It's drawn at
// its box's width in pixels (again when that changes), so its labels stay
// the size of the text around them instead of growing with a viewBox.
// Inline SVG with classes: nothing the CSP refuses.
export function BarChart({ points, title }: { points: Array<{ month: string; amount: number }>; title: string }) {
  const H = 230,
    L = 64,
    R = 8,
    T = 14,
    B = 30
  const ph = H - T - B
  const most = Math.max(0, ...points.map((p) => p.amount || 0))
  // A "nice" step (1, 2, 2.5 or 5 × 10ⁿ) for four gridlines.
  const raw = Math.max(most, 10 ** currency().decimals) / 4
  const e = 10 ** Math.floor(Math.log10(raw))
  const m = raw / e
  const step = (m <= 1 ? 1 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 5 ? 5 : 10) * e
  const max = step * 4

  const id = useId()
  const wrap = useRef<HTMLDivElement>(null)
  const [W, setW] = useState(640)
  const [hover, setHover] = useState<number | null>(null)
  const [tipLeft, setTipLeft] = useState(0)

  useEffect(() => {
    const el = wrap.current
    if (!el) return
    const ro = new ResizeObserver(([entry]) => {
      const w = Math.round(entry.contentRect.width)
      if (w > 0) setW(w)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const pw = W - L - R
  const slot = pw / Math.max(1, points.length)
  const bw = Math.min(34, slot * 0.6)
  // Every other month's label, where a whole year doesn't fit.
  const thin = slot < 44
  const barH = (p: { amount: number }) => (max ? ((p.amount || 0) / max) * ph : 0)
  const hovered = hover != null ? points[hover] : null

  return (
    <div>
      <div ref={wrap} className="relative w-full min-w-0">
        <svg
          viewBox={`0 0 ${W} ${H}`}
          width={W}
          height={H}
          role="img"
          aria-labelledby={`${id}-t ${id}-d`}
          className="block max-w-full overflow-visible"
          onPointerLeave={() => setHover(null)}
        >
          <title id={`${id}-t`}>{title}</title>
          <desc id={`${id}-d`}>{points.length ? points.map((p) => `${monthLabel(p.month, true)}: ${money(p.amount)}`).join("; ") : "No data yet."}</desc>
          {[0, 1, 2, 3, 4].map((i) => {
            const y = T + ph - (i / 4) * ph
            return (
              <g key={i}>
                <line x1={L} x2={W - R} y1={y} y2={y} className={i ? "stroke-border" : "stroke-muted-foreground/40"} strokeWidth={1} />
                <text x={L - 10} y={y + 4} textAnchor="end" className="fill-muted-foreground text-[11px] tabular-nums">
                  {money(step * i, true)}
                </text>
              </g>
            )
          })}
          {points.map((p, i) => {
            const x = L + slot * i + (slot - bw) / 2
            const bh = barH(p)
            const current = i === points.length - 1
            let bar = null
            if (bh > 0) {
              const hh = Math.max(bh, 2)
              const y = T + ph - hh
              const r = Math.min(4, bw / 2, hh)
              bar = (
                <path
                  d={`M${x} ${y + hh}V${y + r}Q${x} ${y} ${x + r} ${y}H${x + bw - r}Q${x + bw} ${y} ${x + bw} ${y + r}V${y + hh}Z`}
                  className={cn("transition-[fill-opacity]", current ? "fill-primary" : "fill-primary/35", hover === i && "fill-primary/70", hover === i && current && "fill-primary")}
                />
              )
            }
            return (
              <g key={p.month}>
                {bar}
                {(!thin || i % 2 === (points.length - 1) % 2) && (
                  <text x={x + bw / 2} y={H - 9} textAnchor="middle" className={cn("text-[11px]", current ? "fill-foreground font-semibold" : "fill-muted-foreground")}>
                    {monthLabel(p.month)}
                  </text>
                )}
                <rect
                  x={L + slot * i}
                  y={T}
                  width={slot}
                  height={ph}
                  className="fill-transparent"
                  onPointerEnter={(ev) => {
                    const box = wrap.current!.getBoundingClientRect()
                    const r = (ev.currentTarget as SVGRectElement).getBoundingClientRect()
                    // Centred over the bar (translate -50%), kept inside the box.
                    const half = 75
                    setTipLeft(Math.max(half, Math.min(box.width - half, r.left - box.left + r.width / 2)))
                    setHover(i)
                  }}
                />
              </g>
            )
          })}
        </svg>
        <div
          hidden={!hovered}
          // CSSOM (React's style prop), not a style attribute: the CSP allows it.
          style={{ left: tipLeft, top: hovered ? Math.max(0, T + ph - barH(hovered) - 34) : 0 }}
          className="pointer-events-none absolute z-10 -translate-x-1/2 rounded-lg bg-popover px-2.5 py-1 text-xs font-semibold whitespace-nowrap text-popover-foreground tabular-nums shadow-lg ring-1 ring-foreground/10"
        >
          {hovered ? `${monthLabel(hovered.month, true)} · ${money(hovered.amount)}` : ""}
        </div>
      </div>
      {points.length > 0 && !most && <p className="text-sm text-muted-foreground">No income recorded yet: bars appear as payments come in.</p>}
      <details className="group mt-2">
        <summary className="cursor-pointer text-sm font-medium text-link select-none">Show the numbers</summary>
        <BTable
          cols={["Month", { label: "Income", num: true }]}
          rows={points.map((p) => ({ key: p.month, cells: [monthLabel(p.month, true), money(p.amount)] }))}
        />
      </details>
    </div>
  )
}
