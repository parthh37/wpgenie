import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { ArrowUpIcon, CheckIcon, InboxIcon, LockIcon, RotateCcwIcon, SendIcon } from "lucide-react"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { ActionButton, EmptyState, LoadError } from "@/components/app/blocks"
import { ask, askText } from "@/components/app/confirm"
import { Page } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { ApiError, api } from "@/lib/api"
import { invalidate, useApi, useSites } from "@/lib/query"
import { href, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import { cn } from "@/lib/utils"
import { AttachBar, AttachmentList, DropOverlay, UploadProgress, sendForm, useAttachments } from "./attach"
import {
  BackLink, Linkify, NO_LIMITS, PRIORITIES, PROVIDER_STATUS, Priority, TicketStatus, TimeAgo, afterChange, draft, duration, keepThread,
  noteAudience, useSummary,
  type Agent, type Canned, type Department, type Message, type Thread, type Ticket,
} from "./shared"

// #/support/<id>: a ticket as a conversation (the other side on the left,
// yours on the right), its reply box, and its details with the controls
// of whoever may change them. New messages show up while it's open.

export function TicketPage({ id }: { id: number }) {
  const q = useApi<Thread>(`/tickets/${id}`, { refetchInterval: 30_000 })
  if (q.isError && !q.data) {
    if (q.error instanceof ApiError && q.error.status === 404) {
      return (
        <Page>
          <BackLink />
          <EmptyState
            icon={InboxIcon}
            tint="pink"
            title="Ticket not found"
            actions={
              <Button variant="tinted" nativeButton={false} render={<a href={href("/support")} />}>
                All tickets
              </Button>
            }
          >
            It may have been opened by another account, or the link is wrong.
          </EmptyState>
        </Page>
      )
    }
    return (
      <Page>
        <BackLink />
        <LoadError error={q.error} retry={() => q.refetch()} />
      </Page>
    )
  }
  if (!q.data) {
    return (
      <Page>
        <BackLink />
        <div role="status" aria-label="Loading the ticket" className="flex flex-col gap-3">
          <Skeleton className="h-10 w-2/3 rounded-xl" />
          <Skeleton className="h-6 w-1/3 rounded-xl" />
          <div className="mt-4 grid gap-6 lg:grid-cols-[minmax(0,1fr)_300px]">
            <Skeleton className="h-64 rounded-2xl" />
            <Skeleton className="h-64 rounded-2xl" />
          </div>
        </div>
      </Page>
    )
  }
  return <TicketView key={id} th={q.data} />
}

// change sends a change to the ticket and keeps what comes back.
async function change(id: number, body: Record<string, unknown>) {
  const th = await api<Thread>("PUT", `/tickets/${id}`, body)
  keepThread(th)
  afterChange()
  if (th.warning) showError(new Error(th.warning))
  return th
}

function TicketView({ th }: { th: Thread }) {
  const s = useSession()
  const t = th.ticket
  const provider = t.you !== "customer"
  const staff = t.you === "staff"
  const depts = useQuery({
    queryKey: ["/support/departments"],
    enabled: provider,
    queryFn: () => api<Department[]>("GET", "/support/departments").catch(() => [] as Department[]),
  })
  const agents = useQuery({
    queryKey: ["/support/agents"],
    enabled: staff,
    queryFn: () => api<Agent[]>("GET", "/support/agents").catch(() => [] as Agent[]),
  })
  const canned = useQuery({
    queryKey: ["/support/canned"],
    enabled: staff && s.atLeast("operator"),
    queryFn: () => api<Canned[]>("GET", "/support/canned").catch(() => [] as Canned[]),
  })

  // The subject takes the focus, so a screen reader starts at the ticket.
  const title = useRef<HTMLHeadingElement>(null)
  useEffect(() => {
    title.current?.focus({ preventScroll: true })
    invalidate("/support/summary")
  }, [])

  // After sending, the new message scrolls into view.
  const convo = useRef<HTMLOListElement>(null)
  const sent = useRef(false)
  useEffect(() => {
    if (!sent.current) return
    sent.current = false
    convo.current?.lastElementChild?.scrollIntoView({ block: "nearest", behavior: "smooth" })
  }, [th.messages.length])

  return (
    <Page>
      <BackLink />
      <div className="mb-6">
        <h1 ref={title} tabIndex={-1} className="text-[1.75rem] leading-tight font-bold tracking-[-0.02em] [overflow-wrap:anywhere] outline-none sm:text-[2.125rem]">
          {t.subject}
        </h1>
        <div className="mt-2.5 flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
          <span className="rounded-full bg-muted px-2.5 py-0.5 font-mono text-xs font-medium">{t.mask}</span>
          <TicketStatus ticket={t} />
          <Priority priority={t.priority} />
          <span className="text-sm text-muted-foreground">
            Opened <TimeAgo at={t.created_at} />
            {t.opened_by ? ` by ${t.opened_by}` : ""}
          </span>
        </div>
      </div>

      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0 max-lg:order-2">
          <ol ref={convo} role="log" aria-label="Conversation" aria-live="polite" className="mb-5 flex flex-col gap-4">
            {th.messages.map((m) => (
              <MessageItem key={m.id} m={m} t={t} />
            ))}
          </ol>
          {s.canChange ? (
            <Composer th={th} canned={canned.data ?? []} onSent={() => (sent.current = true)} />
          ) : (
            <p className="text-sm text-muted-foreground">Read-only: your role can look, not reply.</p>
          )}
        </div>
        <Details th={th} depts={depts.data ?? []} agents={agents.data ?? []} />
      </div>
    </Page>
  )
}

// ---- The conversation ----

function MessageItem({ m, t }: { m: Message; t: Ticket }) {
  if (m.side === "system") {
    const [first, ...rest] = m.body.split("\n\n")
    const Icon = m.internal ? LockIcon : CheckIcon
    return (
      <li
        className={cn(
          "mx-auto flex max-w-[90%] flex-wrap items-center justify-center gap-x-2 gap-y-1 rounded-2xl px-3.5 py-1.5 text-center text-[0.8125rem] animate-in fade-in-0",
          m.internal ? "bg-warning-fill/12 text-warning" : "bg-muted text-muted-foreground"
        )}
      >
        <Icon className="size-3.5 shrink-0" aria-hidden />
        <span>{first}</span>
        {rest.length > 0 && <q className="basis-full italic">{rest.join(" ")}</q>}
        <TimeAgo at={m.at} className="opacity-75" />
      </li>
    )
  }
  const mine = t.you === "customer" ? m.side === "customer" : m.side !== "customer"
  const role = m.side === "customer" ? (t.you === "customer" ? "" : t.account_name) : t.you === "customer" ? "Support" : m.side === "handler" ? "Reseller" : "Staff"
  const noteFor = m.staff_only ? "Only staff see this: not the customer, not the reseller" : noteAudience(t)
  return (
    <li className={cn("flex items-start gap-2.5 animate-in fade-in-0 slide-in-from-bottom-1 duration-200", mine && "flex-row-reverse")}>
      <Avatar className="mt-0.5 max-sm:hidden" aria-hidden>
        <AvatarFallback className={cn("font-semibold", m.staff ? "bg-success-fill/16 text-success" : mine ? "bg-primary/12 text-link" : "")}>
          {(m.author || "?").slice(0, 1).toUpperCase()}
        </AvatarFallback>
      </Avatar>
      <div
        className={cn(
          "max-w-full min-w-0 rounded-2xl px-4 py-3 sm:max-w-[min(46rem,85%)]",
          mine ? "rounded-tr-md" : "rounded-tl-md",
          m.internal
            ? "border border-dashed border-warning/60 bg-warning-fill/10"
            : mine
              ? "bg-[color-mix(in_oklch,var(--primary)_9%,var(--card))] ring-1 ring-primary/15"
              : "bg-card card-shadow"
        )}
      >
        <div className="mb-1 flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-[0.8125rem]">
          <strong className="font-semibold">{m.author || "Someone"}</strong>
          {role && <span className="text-muted-foreground">{role}</span>}
          {m.internal && (
            <span title={noteFor} className="inline-flex items-center gap-1 text-[0.72rem] font-semibold tracking-wide text-warning uppercase">
              <LockIcon className="size-3" aria-hidden />
              {m.staff_only ? "Staff-only note" : "Internal note"}
            </span>
          )}
          <TimeAgo at={m.at} className="ml-auto text-xs text-muted-foreground" />
        </div>
        {m.body && (
          <div className="text-[0.9375rem] leading-relaxed [overflow-wrap:anywhere] whitespace-pre-wrap">
            <Linkify text={m.body} />
          </div>
        )}
        <AttachmentList ticketId={t.id} files={m.attachments} />
      </div>
    </li>
  )
}

// ---- The details, and the controls ----

function Details({ th, depts, agents }: { th: Thread; depts: Department[]; agents: Agent[] }) {
  const s = useSession()
  const sites = useSites()
  const t = th.ticket
  const provider = t.you !== "customer"
  const act = s.canChange
  const edit = provider && act
  const set = (body: Record<string, unknown>) => change(t.id, body)

  const deptOptions = (depts.some((d) => d.id === t.department_id) ? depts : [...depts, { id: t.department_id, name: t.department, description: "" }]).map(
    (d): [string, string] => [String(d.id), d.name + (d.hidden ? " (hidden)" : "")]
  )
  const rows: Array<[string, ReactNode]> = [
    [
      "Status",
      edit ? (
        <Pick label="Status" value={t.status} options={Object.entries(PROVIDER_STATUS)} onPick={(v) => set({ status: v })} />
      ) : (
        <TicketStatus ticket={t} />
      ),
    ],
    [
      "Priority",
      edit ? (
        <Pick label="Priority" value={t.priority} options={PRIORITIES.map(([k, l]) => [k, l])} onPick={(v) => set({ priority: v })} />
      ) : (
        <Priority priority={t.priority} />
      ),
    ],
    ["Department", edit ? <Pick label="Department" value={String(t.department_id)} options={deptOptions} onPick={(v) => set({ department_id: Number(v) })} /> : t.department],
  ]
  if (t.you === "staff") {
    rows.push([
      "Assigned to",
      act ? (
        <Pick
          label="Assigned to"
          value={String(t.assigned_user_id || 0)}
          options={[["0", "Nobody"], ...agents.map((a): [string, string] => [String(a.id), a.username + (a.id === s.me?.id ? " (you)" : "")])]}
          onPick={(v) => set({ assigned_user_id: Number(v) })}
        />
      ) : (
        t.assigned_to || "Nobody"
      ),
    ])
  }
  if (provider) rows.push(["Customer", `${t.account_name || "Account"} (#${t.account_id})`])
  if (t.site_id) {
    const domain = t.site_domain || sites.data?.find((x) => x.id === t.site_id)?.primary_domain || t.site_id
    rows.push([
      "Site",
      <a href={href(sitePath(t.site_id))} className="text-link">
        {domain}
      </a>,
    ])
  }
  rows.push(["Last reply", <TimeAgo at={t.last_reply_at} />])
  if (provider && t.first_response_at) {
    rows.push(["First reply", duration((new Date(t.first_response_at).getTime() - new Date(t.created_at).getTime()) / 1000) + " after opening"])
  }
  if (t.closed_at) rows.push(["Closed", <TimeAgo at={t.closed_at} />])

  let handling: string | null = null
  if (t.you === "handler") {
    handling =
      t.handler === "reseller"
        ? "Your customer's ticket: you answer it. Escalate it if your provider needs to step in."
        : t.escalated
          ? "You escalated this ticket: your provider's staff handle it now. You can still follow it and reply."
          : "Your provider's staff opened this ticket and handle it. You can follow it and reply."
  } else if (t.you === "staff" && t.handler === "reseller") {
    handling = `A reseller's customer: the reseller (account #${t.handler_account_id}) answers it unless they escalate it.`
  } else if (t.you === "staff" && t.escalated) {
    handling = "Escalated to you by the customer's reseller."
  }

  const closed = t.status === "closed"
  const actions: ReactNode[] = []
  if (act && t.you === "customer") {
    actions.push(
      <ActionButton
        key="close"
        variant="tinted"
        className="w-full max-lg:w-auto"
        run={async () => {
          if (!closed && !(await ask("You can reopen it any time by replying.", { title: "Close this ticket?", ok: "Close ticket", danger: false })))
            return
          await set({ status: closed ? "open" : "closed" })
          notify(closed ? "Ticket reopened" : "Ticket closed. Glad we could help!")
        }}
      >
        {closed ? <RotateCcwIcon data-icon="inline-start" /> : <CheckIcon data-icon="inline-start" />}
        {closed ? "Reopen ticket" : "Close ticket"}
      </ActionButton>
    )
  }
  if (act && th.can_escalate) {
    actions.push(
      <ActionButton
        key="escalate"
        variant="tinted"
        className="w-full max-lg:w-auto"
        run={async () => {
          const reason = await askText(
            "Your provider's staff take over and get an e-mail. You stay on the ticket and can still reply. Your customer isn't told.",
            { title: "Escalate this ticket?", label: "What do they need to know? (optional)", ok: "Escalate" }
          )
          if (reason === null) return
          const next = await api<Thread>("POST", `/tickets/${t.id}/escalate`, { reason })
          keepThread(next)
          afterChange()
          notify("Escalated: your provider has been notified")
        }}
      >
        <ArrowUpIcon data-icon="inline-start" />
        Escalate to provider
      </ActionButton>
    )
  }

  return (
    <aside aria-label="Ticket details" className="rounded-2xl bg-card p-5 card-shadow max-lg:order-1 lg:sticky lg:top-6">
      <h2 className="mb-3 text-[1.0625rem] font-semibold tracking-[-0.017em]">Details</h2>
      <dl className="grid grid-cols-[max-content_minmax(0,1fr)] items-center gap-x-4 gap-y-2.5 text-sm">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className="min-w-0 [overflow-wrap:anywhere]">{v}</dd>
          </div>
        ))}
      </dl>
      {handling && <p className="mt-4 border-t border-border/60 pt-3 text-sm text-muted-foreground">{handling}</p>}
      {actions.length > 0 && <div className="mt-4 flex flex-col gap-2 max-lg:flex-row max-lg:flex-wrap">{actions}</div>}
    </aside>
  )
}

// Pick: a select that changes the ticket. It shows the choice while the
// change is sent, and goes back if it fails.
function Pick({ label, value, options, onPick }: { label: string; value: string; options: Array<[string, string]>; onPick: (v: string) => Promise<unknown> }) {
  const [pending, setPending] = useState<string | null>(null)
  return (
    <NativeSelect
      size="sm"
      aria-label={label}
      className="w-full"
      value={pending ?? value}
      disabled={pending != null}
      onChange={async (e) => {
        const v = e.target.value
        setPending(v)
        try {
          await onPick(v)
        } catch (err) {
          showError(err)
        } finally {
          setPending(null)
        }
      }}
    >
      {options.map(([v, l]) => (
        <NativeSelectOption key={v} value={v}>
          {l}
        </NativeSelectOption>
      ))}
    </NativeSelect>
  )
}

// ---- The reply box ----

function Composer({ th, canned, onSent }: { th: Thread; canned: Canned[]; onSent: () => void }) {
  const t = th.ticket
  const id = t.id
  const provider = t.you !== "customer"
  const summary = useSummary()
  const att = useAttachments(summary.data?.limits ?? NO_LIMITS)
  const draftKey = `wpgenie_ticket_draft_${id}`
  const [text, setText] = useState(() => draft.get(draftKey))
  const [note, setNote] = useState(false)
  // Staff may keep a note from the reseller too.
  const [staffOnly, setStaffOnly] = useState(false)
  const [problem, setProblem] = useState("")
  const [progress, setProgress] = useState<number | null>(null)
  const [busy, setBusy] = useState(false)
  const area = useRef<HTMLTextAreaElement>(null)
  const caret = useRef<number | null>(null)

  const internal = provider && note
  const privateNote = internal && t.you === "staff" && staffOnly && !!t.reseller

  // A canned reply goes in at the cursor; the cursor after it.
  useEffect(() => {
    if (caret.current == null || !area.current) return
    area.current.focus()
    area.current.selectionStart = area.current.selectionEnd = caret.current
    caret.current = null
  }, [text])

  const write = (v: string) => {
    setText(v)
    setProblem("")
    draft.set(draftKey, v)
  }

  const insertCanned = (cid: string) => {
    const c = canned.find((x) => String(x.id) === cid)
    if (!c) return
    const el = area.current
    const at = el?.selectionStart ?? text.length
    const before = text.slice(0, at)
    const after = text.slice(el?.selectionEnd ?? at)
    const sep = before && !before.endsWith("\n") ? "\n\n" : ""
    caret.current = (before + sep + c.body).length
    write(before + sep + c.body + after)
  }

  async function submit(status?: string) {
    if (busy) return
    const body = text.trim()
    const files = att.files
    setProblem("")
    if (!body && !files.length) {
      setProblem("Write a message (or attach a file) first.")
      area.current?.focus()
      return
    }
    setBusy(true)
    setProgress(files.length ? 0 : null)
    try {
      const next = await sendForm<Thread>(
        `/tickets/${id}/replies`,
        { body, internal, ...(privateNote ? { staff_only: true } : {}), ...(status ? { status } : {}) },
        files,
        (v) => setProgress(v)
      )
      write("")
      att.reset()
      setNote(false)
      setStaffOnly(false)
      onSent()
      keepThread(next)
      afterChange()
      if (next.warning) showError(new Error(next.warning))
      notify(
        privateNote
          ? "Staff-only note added"
          : internal
            ? "Note added"
            : status === "closed"
              ? "Reply sent and ticket closed"
              : provider
                ? "Reply sent: the customer gets an e-mail"
                : "Reply sent. We'll e-mail you when we answer."
      )
    } catch (e) {
      setProblem(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
      setProgress(null)
    }
  }

  const label = privateNote ? "Staff-only note" : internal ? "Internal note" : provider ? "Reply to the customer" : "Your reply"
  const placeholder = internal
    ? privateNote
      ? "Only staff will see this."
      : t.you === "handler"
        ? "Only you and your provider's staff will see this."
        : t.reseller
          ? `Staff and ${t.reseller} will see this; the customer won't.`
          : "Only staff will see this: customers never do."
    : t.status === "closed" && !provider
      ? "This ticket is closed. Write here to reopen it."
      : provider
        ? "Write your reply… (Ctrl+Enter sends)"
        : "Add more details or answer our questions… (Ctrl+Enter sends)"

  return (
    <form
      aria-label="Reply"
      onSubmit={(e: FormEvent) => {
        e.preventDefault()
        submit()
      }}
      {...att.target}
      className={cn(
        "relative flex flex-col gap-3 rounded-2xl p-4 transition-colors card-shadow sm:p-5",
        internal ? "bg-[linear-gradient(180deg,color-mix(in_oklch,var(--warning-fill)_12%,var(--card)),var(--card)_60%)] ring-1 ring-warning/50" : "bg-card"
      )}
    >
      <div>
        <Label htmlFor="tk-reply-text" className={cn("text-[0.9375rem] font-semibold", internal && "text-warning")}>
          {label}
        </Label>
        {internal && (
          <p aria-live="polite" className="mt-0.5 text-sm text-muted-foreground">
            {privateNote ? `Only staff see this note: not the customer, not ${t.reseller}.` : noteAudience(t)}
          </p>
        )}
      </div>
      <Textarea
        id="tk-reply-text"
        ref={area}
        rows={5}
        maxLength={20000}
        value={text}
        placeholder={placeholder}
        readOnly={busy}
        aria-invalid={problem ? true : undefined}
        onChange={(e) => write(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
            e.preventDefault()
            submit()
          }
        }}
        className="min-h-28 resize-y bg-card"
      />
      <DropOverlay show={att.dragging} />
      <AttachBar att={att} disabled={busy} />
      {problem && (
        <p role="alert" className="text-sm font-medium text-danger">
          {problem}
        </p>
      )}
      <UploadProgress value={progress} />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          {provider && (
            <Label className="cursor-pointer font-normal">
              <Checkbox checked={note} onCheckedChange={(c) => setNote(!!c)} disabled={busy} />
              <LockIcon className="size-3.5 text-warning" aria-hidden />
              Internal note
            </Label>
          )}
          {t.you === "staff" && internal && t.reseller && (
            <Label className="cursor-pointer font-normal">
              <Checkbox checked={staffOnly} onCheckedChange={(c) => setStaffOnly(!!c)} disabled={busy} />
              Staff only
            </Label>
          )}
          {canned.length > 0 && (
            <NativeSelect aria-label="Insert a canned reply" value="" onChange={(e) => insertCanned(e.target.value)} className="max-w-64" disabled={busy}>
              <NativeSelectOption value="">Insert a canned reply…</NativeSelectOption>
              {canned.map((c) => (
                <NativeSelectOption key={c.id} value={String(c.id)}>
                  {c.title}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </div>
        <div className="flex gap-2 max-sm:w-full max-sm:*:flex-1">
          {provider && !internal && t.status !== "closed" && (
            <Button type="button" variant="tinted" disabled={busy} onClick={() => submit("closed")}>
              <CheckIcon data-icon="inline-start" />
              Reply &amp; close
            </Button>
          )}
          <Button type="submit" disabled={busy}>
            <SendIcon data-icon="inline-start" />
            {internal ? "Add note" : "Send reply"}
          </Button>
        </div>
      </div>
    </form>
  )
}
