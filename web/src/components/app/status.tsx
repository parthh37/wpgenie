import type { ReactNode } from "react"
import { Badge } from "@/components/ui/badge"
import { cn } from "@/lib/utils"
import { humanize } from "@/lib/format"

// The colour a status word gets, as the legacy panel's .st-* classes.
const OK = new Set(["ok", "active", "running", "updated", "done", "succeeded", "scheduled", "manual", "paid", "open", "answered", "verified", "listed", "healthy", "up", "valid", "on"])
const BAD = new Set(["missing", "mismatch", "error", "failed", "rolled_back", "block", "throttle", "suspended", "unreachable", "overdue", "closed", "critical", "high", "down", "expired", "modified", "void", "cancelled", "terminated"])
const WARN = new Set(["warning", "pending", "waiting_certificate", "waiting_mailbox", "unknown", "running_update", "queued", "safety", "detect", "challenge", "provisioning", "medium", "draft", "partly", "awaiting", "unpaid", "due"])

export type Tone = "ok" | "bad" | "warn" | "neutral"

export function toneOf(status: string | null | undefined): Tone {
  if (!status) return "neutral"
  const s = status.toLowerCase()
  if (OK.has(s)) return "ok"
  if (BAD.has(s)) return "bad"
  if (WARN.has(s)) return "warn"
  return "neutral"
}

export const toneText: Record<Tone, string> = {
  ok: "text-success",
  bad: "text-danger",
  warn: "text-warning",
  neutral: "text-muted-foreground",
}

// StatusText: the word, coloured.
export function StatusText({ status, children, className }: { status: string; children?: ReactNode; className?: string }) {
  return <span className={cn(toneText[toneOf(status)], className)}>{children ?? humanize(status)}</span>
}

// StatusPill: the word in a tinted capsule with a dot (site and job states).
export function StatusPill({ status, tone, children, className }: { status: string; tone?: Tone; children?: ReactNode; className?: string }) {
  const t = tone ?? toneOf(status)
  return (
    <Badge
      variant="secondary"
      className={cn(
        "gap-1.5 font-medium before:size-1.5 before:rounded-full before:bg-current",
        t === "ok" && "bg-success-fill/16 text-success",
        t === "bad" && "bg-danger-fill/16 text-danger",
        t === "warn" && "bg-warning-fill/16 text-warning",
        t === "neutral" && "text-muted-foreground",
        className
      )}
    >
      {children ?? humanize(status)}
    </Badge>
  )
}

// Severity of a vulnerability or alert.
export function Severity({ level }: { level?: string }) {
  if (!level) return <span className="text-muted-foreground">–</span>
  const l = level.toLowerCase()
  return (
    <span
      className={cn(
        (l === "critical" || l === "high") && "font-semibold text-danger",
        l === "medium" && "text-warning",
        l === "warning" && "font-semibold text-warning"
      )}
    >
      {level}
    </span>
  )
}
