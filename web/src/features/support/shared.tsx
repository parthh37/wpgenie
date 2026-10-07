import type { ReactNode } from "react"
import { ArrowLeftIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { useSupportSummary } from "@/app/nav-state"
import { invalidate, queryClient } from "@/lib/query"
import { fmtTime } from "@/lib/format"
import { href } from "@/lib/router"
import { useSession } from "@/lib/session"
import { cn } from "@/lib/utils"

// Support tickets: what the screens share. Everything here is also
// enforced by the server: hiding a control is only a convenience, and
// customers never receive internal notes to hide.

// ---- Shapes (internal/support/tickets.go, internal/store/support.go) ----

// How the signed-in user stands to a ticket.
export type Party = "customer" | "handler" | "staff"

export interface Ticket {
  id: number
  mask: string
  account_id: number
  account_name: string
  opened_by: string
  department_id: number
  department: string
  site_id?: string
  site_domain?: string
  subject: string
  status: string
  priority: string
  created_at: string
  updated_at: string
  last_reply_at: string
  closed_at?: string
  preview: string
  awaiting: boolean
  you: Party
  // Providers only:
  handler?: "staff" | "reseller"
  reseller?: string
  handler_account_id?: number
  escalated?: boolean
  staff_opened?: boolean
  escalated_at?: string
  assigned_user_id?: number
  assigned_to?: string
  first_response_at?: string
}

export interface Attachment {
  id: number
  name: string
  size: number
  type: string
  image: boolean
}

export interface Message {
  id: number
  side: "customer" | "handler" | "staff" | "system"
  staff: boolean
  internal: boolean
  staff_only?: boolean
  author: string
  body: string
  at: string
  attachments: Attachment[] | null
}

export interface Thread {
  ticket: Ticket
  messages: Message[]
  can_escalate: boolean
  warning?: string
}

export interface Limits {
  max_files: number
  max_file_mb: number
  extensions: string[] | null
}

export type { SupportSummary as Summary } from "@/app/nav-state"

export interface Department {
  id: number
  name: string
  description: string
  // Staff only:
  notify_email?: string
  hidden?: boolean
  sort?: number
  active?: number
  created_at?: string
}

export interface Agent {
  id: number
  username: string
  role: string
}

export interface Canned {
  id: number
  title: string
  body: string
  created_at: string
  updated_at: string
}

export interface Overview {
  by_status: Record<string, number>
  by_department: { id: number; name: string; active: number }[] | null
  awaiting: number
  unassigned: number
  opened_30d: number
  closed_30d: number
  avg_first_response_seconds: number
}

export interface Settings {
  enabled: boolean
  notify_emails: string[] | null
  auto_close_days: number
  max_files: number
  max_file_mb: number
  extensions: string[] | null
  reply_to: string
}

// ---- Words ----

// Staff and resellers see the ticket's state; customers what it means for them.
export const PROVIDER_STATUS: Record<string, string> = {
  open: "New",
  customer_reply: "Customer replied",
  in_progress: "In progress",
  on_hold: "On hold",
  answered: "Answered",
  closed: "Closed",
}
const CUSTOMER_STATUS: Record<string, string> = {
  open: "Waiting for support",
  customer_reply: "Waiting for support",
  in_progress: "In progress",
  on_hold: "On hold",
  answered: "Support replied",
  closed: "Closed",
}
export const ACTIVE_STATUSES = ["open", "customer_reply", "in_progress", "on_hold", "answered"]

export const PRIORITIES: Array<[key: string, label: string, hint: string]> = [
  ["low", "Low", "A question, no rush"],
  ["medium", "Normal", "Something isn't working right"],
  ["high", "High", "A site is affected"],
  ["urgent", "Urgent", "A site is down"],
]
const PRIO_LABEL: Record<string, string> = Object.fromEntries(PRIORITIES.map(([k, l]) => [k, l]))

export const statusLabel = (t: Pick<Ticket, "you" | "status">) =>
  (t.you === "customer" ? CUSTOMER_STATUS : PROVIDER_STATUS)[t.status] || t.status

// A customer waiting for support is no alarm to the customer.
export function TicketStatus({ ticket: t, className }: { ticket: Pick<Ticket, "you" | "status">; className?: string }) {
  const s = t.you === "customer" && t.status === "customer_reply" ? "open" : t.status
  return (
    <Badge
      variant="secondary"
      className={cn(
        "gap-1.5 font-semibold before:size-1.5 before:rounded-full before:bg-current",
        (s === "open" || s === "in_progress") && "bg-primary/12 text-link",
        s === "customer_reply" && "bg-warning-fill/16 text-warning",
        s === "answered" && "bg-success-fill/16 text-success",
        (s === "on_hold" || s === "closed") && "text-muted-foreground",
        className
      )}
    >
      {statusLabel(t)}
    </Badge>
  )
}

export function Priority({ priority: p, className }: { priority: string; className?: string }) {
  return (
    <Badge
      variant="outline"
      className={cn(
        "font-semibold",
        p === "low" && "text-muted-foreground",
        p === "high" && "border-warning/45 bg-transparent text-warning",
        p === "urgent" && "border-transparent bg-danger-fill text-white",
        className
      )}
    >
      {PRIO_LABEL[p] || p}
    </Badge>
  )
}

// ---- Time ----

const RTF = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" })

// ago: "just now", "5 minutes ago", "yesterday"…
export function ago(t: string): string {
  const s = (new Date(t).getTime() - Date.now()) / 1000
  const a = Math.abs(s)
  if (a < 60) return "just now"
  const steps: Array<[number, Intl.RelativeTimeFormatUnit, number]> = [
    [3600, "minute", 60],
    [86400, "hour", 3600],
    [604800, "day", 86400],
    [2629800, "week", 604800],
    [31557600, "month", 2629800],
    [Infinity, "year", 31557600],
  ]
  for (const [limit, unit, div] of steps) if (a < limit) return RTF.format(Math.round(s / div), unit)
  return fmtTime(t)
}

export function TimeAgo({ at, className }: { at: string | undefined; className?: string }) {
  if (!at) return <span className={className}>–</span>
  return (
    <time dateTime={at} title={fmtTime(at)} className={className}>
      {ago(at)}
    </time>
  )
}

export function duration(secs: number): string {
  if (secs < 0) return "–"
  if (secs < 60) return "under a minute"
  if (secs < 3600) return `${Math.max(1, Math.round(secs / 60))} min`
  if (secs < 86400) return `${(secs / 3600).toFixed(secs < 36000 ? 1 : 0)} h`
  return `${(secs / 86400).toFixed(1)} days`
}

// ---- Text ----

// Linkify turns the URLs of a message into links; the rest stays text
// (never HTML).
export function Linkify({ text }: { text: string }) {
  const out: ReactNode[] = []
  const re = /https?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)]/g
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(text))) {
    out.push(text.slice(last, m.index))
    out.push(
      <a key={m.index} href={m[0]} target="_blank" rel="noopener noreferrer nofollow" className="text-link underline-offset-2 hover:underline">
        {m[0]}
      </a>
    )
    last = m.index + m[0].length
  }
  out.push(text.slice(last))
  return <>{out}</>
}

// noteAudience says who reads a ticket's internal notes: never the
// customer, but the account's reseller as well as staff (it handles the
// ticket, or did), unless staff mark a note staff-only.
export function noteAudience(t: Ticket): string {
  if (t.you === "handler") return "Only you and your provider's staff see notes."
  if (!t.reseller) return "Customers never see notes."
  return t.handler === "reseller"
    ? `Customers never see notes; ${t.reseller} does, as they handle this ticket.`
    : `Customers never see notes; ${t.reseller}, the customer's reseller, does too (unless you tick Staff only).`
}

// ---- Data ----

// useSummary is the navigation's summary (same query, so one request):
// the badge count, whether tickets can be opened, the attachment limits.
// null: no help desk on this server.
export function useSummary() {
  return useSupportSummary(useSession())
}

export const NO_LIMITS: Limits = { max_files: 0, max_file_mb: 0, extensions: [] }

// afterChange refreshes what a change to tickets moves: the badge, the
// lists and the staff's numbers.
export function afterChange() {
  invalidate("/support/summary")
  invalidate("/support/overview")
  invalidate("/tickets?")
}

// keepThread stores a ticket the API returned after a change.
export function keepThread(th: Thread) {
  queryClient.setQueryData([`/tickets/${th.ticket.id}`], th)
}

export function BackLink() {
  return (
    <Button variant="ghost" size="sm" className="mb-3 -ml-2 text-muted-foreground" nativeButton={false} render={<a href={href("/support")} />}>
      <ArrowLeftIcon data-icon="inline-start" />
      All tickets
    </Button>
  )
}

// The session's draft of a reply, kept while the tab is open.
export const draft = {
  get(key: string) {
    try {
      return sessionStorage.getItem(key) || ""
    } catch {
      return ""
    }
  },
  set(key: string, v: string) {
    try {
      if (v) sessionStorage.setItem(key, v)
      else sessionStorage.removeItem(key)
    } catch {
      /* no storage */
    }
  },
}
