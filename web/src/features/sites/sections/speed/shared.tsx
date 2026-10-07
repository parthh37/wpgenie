import { useEffect, useRef, useState, type ReactNode } from "react"
import { TriangleAlertIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Switch } from "@/components/ui/switch"
import { showError } from "@/components/app/toaster"
import { followJob, JOB_NAMES, type JobResult } from "@/lib/jobs"
import { invalidate } from "@/lib/query"
import type { JobID } from "@/lib/types"
import { cn } from "@/lib/utils"

// Pieces the Speed sections (performance, CDN, insights, uploads offload)
// share.

export type { JobID } from "@/lib/types"

// followSiteJob follows a job started by a request that didn't go through
// startJob (PUT /images answers 200 with an optional job_id): its failure
// is shown, then the sites refresh.
export function followSiteJob(id: JobID, onDone?: (v: JobResult) => void | Promise<void>) {
  followJob(id, async (v) => {
    if (v.job.status === "failed") showError(new Error(`${JOB_NAMES[v.job.kind] || v.job.kind} failed: ${v.job.error}`))
    if (onDone) await onDone(v)
    await invalidate("/sites")
  })
}

// "850 ms", "1.2 s", "14 s".
export const fmtMS = (ms: number) => (ms >= 1000 ? `${(ms / 1000).toFixed(ms >= 10000 ? 0 : 1)} s` : `${Math.round(ms)} ms`)

// Meter: a horizontal gauge (the legacy <meter>, which can't be themed).
// With optimum at the top: under low is red, under high orange, else
// green; without low/high it's the tint.
export function Meter({
  value,
  max,
  low,
  high,
  label,
  className,
}: {
  value: number
  max: number
  low?: number
  high?: number
  label: string
  className?: string
}) {
  const pct = max > 0 ? Math.min(100, Math.max(0, (100 * value) / max)) : 0
  const tone =
    low == null || high == null ? "bg-primary" : value < low ? "bg-danger-fill" : value < high ? "bg-warning-fill" : "bg-success-fill"
  return (
    <div
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={max}
      aria-valuenow={value}
      className={cn("h-2 w-full min-w-16 overflow-hidden rounded-full bg-muted", className)}
    >
      {/* CSSOM (React's style prop), not a style attribute: the CSP allows it. */}
      <div className={cn("h-full rounded-full", tone)} style={{ width: `${pct}%` }} />
    </div>
  )
}

// FlashButton runs an action and says it worked ("Purged ✓") for a moment;
// it stays disabled meanwhile, as the legacy purge buttons did.
export function FlashButton({
  run,
  children,
  done,
  variant = "tinted",
  disabled,
}: {
  run: () => Promise<unknown>
  children: ReactNode
  done: ReactNode
  variant?: React.ComponentProps<typeof Button>["variant"]
  disabled?: boolean
}) {
  const [state, setState] = useState<"idle" | "busy" | "done">("idle")
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  return (
    <Button
      type="button"
      variant={variant}
      disabled={disabled || state !== "idle"}
      onClick={async () => {
        setState("busy")
        let ok = false
        try {
          await run()
          ok = true
        } catch (e) {
          showError(e)
        }
        if (ok) setState("done")
        timer.current = setTimeout(() => setState("idle"), 1500)
      }}
    >
      {state === "done" ? done : children}
    </Button>
  )
}

// SwitchRow: an on/off setting, its name and a line under it.
export function SwitchRow({
  checked,
  onChange,
  disabled,
  title,
  children,
  className,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  disabled?: boolean
  title: ReactNode
  children?: ReactNode
  className?: string
}) {
  return (
    <label className={cn("flex cursor-pointer items-start gap-3 has-data-disabled:cursor-not-allowed", className)}>
      <Switch checked={checked} disabled={disabled} onCheckedChange={(v) => onChange(v)} className="mt-0.5" />
      <span className={cn("flex min-w-0 flex-col gap-0.5", disabled && "opacity-60")}>
        <span className="text-sm font-medium">{title}</span>
        {children && <span className="text-sm text-muted-foreground">{children}</span>}
      </span>
    </label>
  )
}

// Note: the small print under a group of controls.
export const Note = ({ children, className }: { children: ReactNode; className?: string }) => (
  <p className={cn("max-w-[80ch] text-sm text-muted-foreground", className)}>{children}</p>
)

// Steps: a numbered list of what to do elsewhere first.
export const Steps = ({ children }: { children: ReactNode }) => (
  <ol className="max-w-[80ch] list-decimal space-y-1 pl-5 text-sm text-muted-foreground">{children}</ol>
)

// Warning: a line that needs attention.
export const Warning = ({ children, tone = "warn" }: { children: ReactNode; tone?: "warn" | "bad" }) => (
  <p className={cn("flex items-start gap-2 text-sm", tone === "bad" ? "text-danger" : "text-warning")}>
    <TriangleAlertIcon className="mt-0.5 size-4 shrink-0" />
    <span className="min-w-0 [overflow-wrap:anywhere]">{children}</span>
  </p>
)

// Summary: the one-line state of a section (the legacy <summary>'s tail),
// over its content.
export const Summary = ({ children }: { children: ReactNode }) => (
  <div className="-mt-2 mb-4 flex flex-wrap items-center gap-1.5 text-sm text-muted-foreground">{children}</div>
)
