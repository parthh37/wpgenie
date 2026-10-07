import { useEffect, useRef, useState, type FormEvent } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { CheckIcon, MailIcon, RotateCcwIcon, SendIcon, ServerIcon, TriangleAlertIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, Chips, EmptyState, LoadError, SubNav } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { notify } from "@/components/app/toaster"
import { api, errorMessage } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { friendly } from "@/lib/money"
import { queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import { cn } from "@/lib/utils"
import { billingPath, type BillingRoute } from "../route"
import { AreaField, Labeled, SelectField, SettingsCard, SubHeading, TextField, ToggleField, grid } from "./common"

// E-mail to people (internal/mailer): the SMTP server the panel sends
// through, the templates staff can reword, and the log of what was sent.
// Sections at #/billing/email[/templates|/log].

const EMAIL_SECTIONS: Array<[key: string, label: string, role: "admin" | "operator"]> = [
  ["", "Sending", "admin"],
  ["templates", "Templates", "admin"],
  ["log", "Sent e-mail", "operator"],
]

export function Email({ route }: { route: BillingRoute }) {
  const s = useSession()
  const sections = EMAIL_SECTIONS.filter(([, , role]) => s.atLeast(role))
  if (!sections.length) return <LoadError error={new Error("E-mail is for staff.")} />
  const section = sections.some(([k]) => k === route.id) ? route.id : sections[0][0]
  return (
    <>
      <SubNav label="E-mail sections" current={section} items={sections.map(([key, label]) => ({ key, label, href: billingPath("email", key) }))} />
      {section === "" && <Sending />}
      {section === "templates" && <Templates />}
      {section === "log" && <MailLog />}
    </>
  )
}

// ---- Sending: the SMTP server ----

interface EmailSettings {
  enabled?: boolean
  host?: string
  port?: number
  tls?: string
  username?: string
  from_name?: string
  from_address?: string
  reply_to?: string
  bcc?: string
  password_set?: boolean
  local_mail_host?: string
}

interface Preset {
  id: string
  name: string
  host?: string
  port?: number
  tls?: string
  hint?: string
}

// Providers' usual settings; the username and password are always theirs.
const SMTP_PRESETS: Preset[] = [
  { id: "custom", name: "Other / my own server" },
  {
    id: "gmail", name: "Gmail / Google Workspace", host: "smtp.gmail.com", port: 587, tls: "starttls",
    hint: "Username: the full address. Password: an app password (Google Account → Security → App passwords); 2-step verification must be on.",
  },
  {
    id: "m365", name: "Microsoft 365 / Outlook", host: "smtp.office365.com", port: 587, tls: "starttls",
    hint: "Username: the full address. SMTP AUTH must be enabled for the mailbox in the Microsoft 365 admin center.",
  },
  {
    id: "ses", name: "Amazon SES", host: "email-smtp.us-east-1.amazonaws.com", port: 587, tls: "starttls",
    hint: "Use your region's endpoint, and SMTP credentials made in the SES console (not your AWS keys). Verify the sending domain first.",
  },
  {
    id: "sendgrid", name: "SendGrid", host: "smtp.sendgrid.net", port: 587, tls: "starttls",
    hint: "Username: apikey (the word). Password: an API key with Mail Send permission.",
  },
  {
    id: "mailgun", name: "Mailgun", host: "smtp.mailgun.org", port: 587, tls: "starttls",
    hint: "EU accounts use smtp.eu.mailgun.org. Credentials: Sending → Domain settings → SMTP credentials.",
  },
  {
    id: "postmark", name: "Postmark", host: "smtp.postmarkapp.com", port: 587, tls: "starttls",
    hint: "Username and password: your server API token (both).",
  },
  {
    id: "brevo", name: "Brevo (Sendinblue)", host: "smtp-relay.brevo.com", port: 587, tls: "starttls",
    hint: "Username: your Brevo login. Password: an SMTP key (SMTP & API → SMTP).",
  },
  {
    id: "zoho", name: "Zoho Mail", host: "smtp.zoho.com", port: 465, tls: "tls",
    hint: "EU and India accounts use smtp.zoho.eu / smtp.zoho.in. Use an app-specific password with 2FA.",
  },
  // host: filled in from the server's mail hostname (its certificate's name).
  { id: "local", name: "This server's Mail (Mail tab)", host: "", port: 587, tls: "starttls", hint: "A mailbox created under Mail: its full address and password." },
]

const EMAIL_SETTINGS = "/settings/email"

function Sending() {
  const q = useApi<EmailSettings>(EMAIL_SETTINGS)
  if (q.error) return <LoadError error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-64 rounded-2xl" />
  return (
    <>
      <SendingForm s={q.data} />
      <SendTest />
    </>
  )
}

function SendingForm({ s }: { s: EmailSettings }) {
  // "This server's Mail" only when the Mail tab's server is on.
  const presets = SMTP_PRESETS.map((p) => (p.id === "local" ? { ...p, host: s.local_mail_host || "" } : p)).filter((p) => p.id !== "local" || p.host)
  const [preset, setPreset] = useState((presets.find((p) => p.host && p.host === s.host) || presets[0]).id)
  const [enabled, setEnabled] = useState(!!s.enabled)
  const [host, setHost] = useState(s.host || "")
  const [port, setPort] = useState(String(s.port || 587))
  const [tls, setTls] = useState(s.tls || "starttls")
  const [f, setF] = useState({
    username: s.username || "",
    password: "",
    from_name: s.from_name || "",
    from_address: s.from_address || "",
    reply_to: s.reply_to || "",
    bcc: s.bcc || "",
  })
  const [passwordSet, setPasswordSet] = useState(!!s.password_set)
  const text = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const hint = presets.find((x) => x.id === preset)?.hint || "Your provider's SMTP settings: host, port, and a username and password."

  return (
    <SettingsCard
      icon={ServerIcon}
      tint="blue"
      title="Sending e-mail"
      intro="Invoices, receipts, reminders and ticket replies go out through this server."
      onSave={async () => {
        const body: Record<string, unknown> = {
          enabled,
          host: host.trim(),
          port: Number(port || 0),
          tls,
          username: f.username.trim(),
          from_name: f.from_name.trim(),
          from_address: f.from_address.trim(),
          reply_to: f.reply_to.trim(),
          bcc: f.bcc.trim(),
        }
        if (f.password) body.password = f.password
        const saved = await api<EmailSettings>("PUT", EMAIL_SETTINGS, body)
        setF((x) => ({ ...x, password: "" }))
        setPasswordSet(!!saved?.password_set)
        if (saved) queryClient.setQueryData([EMAIL_SETTINGS], saved)
        notify("E-mail settings saved")
      }}
    >
      <ToggleField name="enabled" label="Send e-mail" help="Off: messages wait in the log until it's on." checked={enabled} onChange={setEnabled} />
      <Labeled label="Provider">
        <SelectField
          value={preset}
          onChange={(e) => {
            setPreset(e.target.value)
            const p = presets.find((x) => x.id === e.target.value)
            if (p?.host) {
              setHost(p.host)
              setPort(String(p.port))
              setTls(p.tls || "starttls")
            }
          }}
        >
          {presets.map((p) => (
            <NativeSelectOption key={p.id} value={p.id}>
              {p.name}
            </NativeSelectOption>
          ))}
        </SelectField>
      </Labeled>
      <p className="-mt-2 rounded-xl bg-muted px-3.5 py-2.5 text-sm text-muted-foreground" aria-live="polite">
        {hint}
      </p>
      <div className={grid}>
        <Labeled label="SMTP server">
          <TextField name="host" placeholder="smtp.example.com" autoComplete="off" value={host} onChange={(e) => setHost(e.target.value)} />
        </Labeled>
        <Labeled label="Port">
          <TextField name="port" type="number" min={1} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
        </Labeled>
        <Labeled label="Encryption">
          <SelectField name="tls" value={tls} onChange={(e) => setTls(e.target.value)}>
            <NativeSelectOption value="starttls">STARTTLS (usually port 587)</NativeSelectOption>
            <NativeSelectOption value="tls">TLS (usually port 465)</NativeSelectOption>
            <NativeSelectOption value="none">None (a relay on this server only)</NativeSelectOption>
          </SelectField>
        </Labeled>
        <Labeled label="Username">
          <TextField name="username" autoComplete="off" value={f.username} onChange={text("username")} />
        </Labeled>
        <Labeled label="Password">
          <TextField
            name="password"
            type="password"
            autoComplete="new-password"
            placeholder={passwordSet ? "set (unchanged if empty)" : ""}
            value={f.password}
            onChange={text("password")}
          />
        </Labeled>
      </div>
      <SubHeading>From</SubHeading>
      <div className={grid}>
        <Labeled label="Name">
          <TextField name="from_name" placeholder="Acme Hosting" value={f.from_name} onChange={text("from_name")} />
        </Labeled>
        <Labeled label="Address" help="Your provider must allow sending as it (SPF and DKIM for the domain).">
          <TextField name="from_address" type="email" placeholder="billing@example.com" value={f.from_address} onChange={text("from_address")} />
        </Labeled>
        <Labeled label="Replies to (optional)">
          <TextField name="reply_to" type="email" value={f.reply_to} onChange={text("reply_to")} />
        </Labeled>
        <Labeled label="Copy every message to (optional)" help="A BCC for your records.">
          <TextField name="bcc" type="email" value={f.bcc} onChange={text("bcc")} />
        </Labeled>
      </div>
    </SettingsCard>
  )
}

type TestResult = { kind: "sending" } | { kind: "sent"; to: string } | { kind: "refused"; error: string } | { kind: "failed"; message: string }

// A test message, with the server's answer when it fails.
function SendTest() {
  const { me } = useSession()
  const [to, setTo] = useState(((me as { email?: string } | null)?.email as string) || "")
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<TestResult | null>(null)
  async function send(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!e.currentTarget.reportValidity()) return
    const addr = to.trim()
    setBusy(true)
    setResult({ kind: "sending" })
    try {
      const r = await api<{ ok: boolean; error?: string }>("POST", "/settings/email/test", { to: addr })
      setResult(r.ok ? { kind: "sent", to: addr } : { kind: "refused", error: r.error || "" })
    } catch (err) {
      setResult({ kind: "failed", message: errorMessage(friendly(err)) })
    } finally {
      setBusy(false)
    }
  }
  return (
    <Section icon={SendIcon} tint="green" title="Send a test" description="Save first: the test uses the saved settings.">
      <form onSubmit={send} className="flex flex-wrap gap-2">
        <Input
          type="email"
          required
          aria-label="Send a test to"
          placeholder="you@example.com"
          value={to}
          onChange={(e) => setTo(e.target.value)}
          className="min-w-0 flex-1 basis-64"
        />
        <Button type="submit" variant="tinted" disabled={busy}>
          <SendIcon data-icon="inline-start" />
          Send test
        </Button>
      </form>
      <div aria-live="polite" className="mt-3 empty:hidden">
        {result?.kind === "sending" && <p className="text-sm text-muted-foreground">Sending…</p>}
        {result?.kind === "sent" && (
          <p className="flex items-center gap-1.5 text-sm text-success">
            <CheckIcon className="size-4" />
            Sent. Check {result.to} (and its spam folder).
          </p>
        )}
        {result?.kind === "refused" && (
          <div role="alert" className="flex gap-3 rounded-2xl bg-danger-fill/12 p-4">
            <TriangleAlertIcon className="mt-0.5 size-5 shrink-0 text-danger" />
            <div className="min-w-0">
              <strong className="font-semibold">The server said no</strong>
              <pre className="mt-1 text-xs whitespace-pre-wrap [overflow-wrap:anywhere] text-muted-foreground">{result.error}</pre>
            </div>
          </div>
        )}
        {result?.kind === "failed" && <p className="text-sm text-danger">{result.message}</p>}
      </div>
    </Section>
  )
}

// ---- Templates ----

interface Template {
  name: string
  group?: string
  description?: string
  subject: string
  body: string
  vars?: string[] | null
  default_subject?: string
  default_body?: string
  customized?: boolean
}

function Templates() {
  const q = useApi<Template[]>("/email/templates")
  const [editing, setEditing] = useState<{ t: Template; key: number } | null>(null)
  const [open, setOpen] = useState(false)
  if (q.error) return <LoadError error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-64 rounded-2xl" />
  const list = q.data
  if (!list.length) {
    return (
      <EmptyState icon={MailIcon} tint="blue" title="No templates">
        Features register their messages when they're installed.
      </EmptyState>
    )
  }
  const groups = new Map<string, Template[]>()
  for (const t of list) {
    const g = t.group || "Other"
    if (!groups.has(g)) groups.set(g, [])
    groups.get(g)!.push(t)
  }
  return (
    <>
      <p className="mb-4 text-sm text-muted-foreground">
        Every message the panel sends, grouped by what sends it. Change the wording; the layout, your logo and the footer are added around it.
      </p>
      {[...groups].map(([group, ts]) => (
        <Section key={group} title={group} contentClassName="px-2">
          <ul className="divide-y divide-border">
            {ts.map((t) => (
              <li key={t.name}>
                <button
                  type="button"
                  onClick={() => {
                    setEditing({ t, key: Date.now() })
                    setOpen(true)
                  }}
                  className="grid w-full justify-items-start gap-0.5 rounded-xl px-3 py-2.5 text-left transition-colors hover:bg-accent focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                >
                  <span className="flex items-center gap-2 font-semibold">
                    {t.subject || t.name}
                    {t.customized && <Badge variant="secondary">customized</Badge>}
                  </span>
                  <span className="text-sm text-muted-foreground">{t.description || t.name}</span>
                  <code className="bg-transparent p-0 text-xs text-muted-foreground">{t.name}</code>
                </button>
              </li>
            ))}
          </ul>
        </Section>
      ))}
      {editing && <TemplateEditor key={editing.key} t={editing.t} open={open} onOpenChange={setOpen} onSaved={() => q.refetch()} />}
    </>
  )
}

// TemplateEditor: subject and body, placeholders that insert themselves,
// and a preview rendered by the server with sample data as you type.
function TemplateEditor({ t, open, onOpenChange, onSaved }: { t: Template; open: boolean; onOpenChange: (o: boolean) => void; onSaved: () => void }) {
  const [subject, setSubject] = useState(t.subject)
  const [body, setBody] = useState(t.body)
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<{ subject: string; url: string } | null>(null)
  const [pstate, setPstate] = useState<{ text: string; error: boolean }>({ text: "", error: false })
  const subjectRef = useRef<HTMLInputElement>(null)
  const bodyRef = useRef<HTMLTextAreaElement>(null)
  const last = useRef<"subject" | "body">("body") // where a placeholder goes
  const seq = useRef(0)

  // The preview follows the text, half a second after typing stops.
  useEffect(() => {
    if (!open) return
    const n = ++seq.current
    const timer = setTimeout(async () => {
      setPstate({ text: "Updating the preview…", error: false })
      try {
        const r = await api<{ subject: string; html_url: string }>("POST", `/email/templates/${encodeURIComponent(t.name)}/preview`, { subject, body })
        if (n !== seq.current) return
        setPreview({ subject: r.subject, url: r.html_url })
        setPstate({ text: "Preview with sample data.", error: false })
      } catch (e) {
        if (n !== seq.current) return
        setPstate({ text: errorMessage(friendly(e)), error: true })
      }
    }, 500)
    return () => clearTimeout(timer)
  }, [open, subject, body, t.name])

  const vars = [...(t.vars || []), "Brand.Name", "Brand.URL", "PanelURL"].filter((v, i, a) => a.indexOf(v) === i)
  const insert = (v: string) => {
    const el = last.current === "subject" ? subjectRef.current : bodyRef.current
    if (!el) return
    el.setRangeText(`{{.${v}}}`, el.selectionStart ?? el.value.length, el.selectionEnd ?? el.value.length, "end")
    if (last.current === "subject") setSubject(el.value)
    else setBody(el.value)
    el.focus()
  }

  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    e.stopPropagation()
    if (!e.currentTarget.reportValidity()) return
    setBusy(true)
    setError("")
    try {
      await api("PUT", `/email/templates/${encodeURIComponent(t.name)}`, { subject, body })
      notify("Template saved")
      onSaved()
      onOpenChange(false)
    } catch (err) {
      setError(errorMessage(friendly(err)))
    } finally {
      setBusy(false)
    }
  }
  async function reset() {
    if (!(await ask(`Go back to the default wording of "${t.name}"? Your changes to it are lost.`, { ok: "Use the default", danger: true }))) return
    try {
      await api("DELETE", `/email/templates/${encodeURIComponent(t.name)}`)
      notify("Template reset")
      onSaved()
      onOpenChange(false)
    } catch (err) {
      setError(errorMessage(friendly(err)))
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-6xl">
        <form onSubmit={save} className="flex flex-col gap-5">
          <DialogHeader>
            <DialogTitle>{t.description || t.name}</DialogTitle>
          </DialogHeader>
          <div className="grid gap-5 lg:grid-cols-2">
            <div className="flex min-w-0 flex-col gap-4">
              <Labeled label="Subject">
                <TextField
                  ref={subjectRef}
                  name="subject"
                  required
                  spellCheck
                  value={subject}
                  onFocus={() => (last.current = "subject")}
                  onChange={(e) => setSubject(e.target.value)}
                />
              </Labeled>
              <Labeled label="Message" help="A blank line starts a paragraph. A line [[Pay now|{{.Invoice.URL}}]] is a button.">
                <AreaField
                  ref={bodyRef}
                  name="body"
                  rows={14}
                  required
                  spellCheck
                  value={body}
                  onFocus={() => (last.current = "body")}
                  onChange={(e) => setBody(e.target.value)}
                  className="min-h-64 font-mono text-[0.86rem] leading-relaxed"
                />
              </Labeled>
              <div className="flex flex-col gap-2">
                <span className="text-sm font-medium">Placeholders</span>
                <div role="group" aria-label="Insert a placeholder" className="flex flex-wrap gap-1.5">
                  {vars.map((v) => (
                    <button
                      key={v}
                      type="button"
                      title={`Insert {{.${v}}}`}
                      // Keep the caret where it was: don't take the focus on press.
                      onMouseDown={(e) => e.preventDefault()}
                      onClick={() => insert(v)}
                      className="inline-flex h-7 items-center rounded-full bg-primary/10 px-2.5 font-mono text-xs font-medium text-link ring-1 ring-transparent transition-colors hover:ring-primary focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                    >
                      {v}
                    </button>
                  ))}
                </div>
              </div>
              {t.customized && (
                <div>
                  <Button type="button" variant="tinted" onClick={reset}>
                    <RotateCcwIcon data-icon="inline-start" />
                    Back to the default
                  </Button>
                </div>
              )}
            </div>
            <div className="flex min-w-0 flex-col gap-2">
              <span className="text-sm font-medium">Preview</span>
              <p className="text-sm font-semibold">{preview?.subject}</p>
              <MailFrame title="Preview" src={preview?.url} className="min-h-[360px] flex-1" />
              <p aria-live="polite" className={cn("text-sm", pstate.error ? "text-danger" : "text-muted-foreground")}>
                {pstate.text}
              </p>
            </div>
          </div>
          {error && (
            <p role="alert" className="text-sm text-danger">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="tinted" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy}>
              Save template
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// MailFrame shows an e-mail served by the panel (same origin), sandboxed:
// nothing in it runs or reaches the panel; links open outside. E-mail is
// written for a white page, in dark mode too.
function MailFrame({ title, src, className }: { title: string; src?: string; className?: string }) {
  return (
    <iframe
      title={title}
      src={src}
      sandbox="allow-popups allow-popups-to-escape-sandbox"
      referrerPolicy="no-referrer"
      className={cn("block h-[min(62vh,640px)] w-full rounded-xl bg-white ring-1 ring-border", className)}
    />
  )
}

// ---- The log ----

interface MailMessage {
  id: number
  template?: string
  to?: string[] | null
  subject: string
  status: string
  attempts: number
  last_error?: string
  created_at: string
  sent_at?: string
}

// The filter stays while the panel is open.
let MAIL_STATUS = ""
const MAIL_STATES: Record<string, string> = { pending: "Waiting", sent: "Sent", failed: "Failed" }
const MAIL_TONE: Record<string, "ok" | "bad" | "warn"> = { sent: "ok", failed: "bad", pending: "warn" }
const PAGE = 50

function MailLog() {
  const s = useSession()
  const admin = s.atLeast("admin")
  const [status, setStatusState] = useState(MAIL_STATUS)
  const setStatus = (k: string) => {
    MAIL_STATUS = k
    setStatusState(k)
  }
  const [viewing, setViewing] = useState<MailMessage | null>(null)
  const [viewOpen, setViewOpen] = useState(false)
  const key = ["/email/log", status]
  const q = useInfiniteQuery({
    queryKey: key,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => {
      const p = new URLSearchParams({ limit: String(PAGE) })
      if (status) p.set("status", status)
      if (pageParam) p.set("before", String(pageParam))
      return api<MailMessage[]>("GET", "/email/log?" + p)
    },
    getNextPageParam: (last) => (last.length === PAGE ? last[last.length - 1].id : undefined),
  })
  const items = q.data?.pages.flat() ?? []
  const again = () => queryClient.resetQueries({ queryKey: key })

  return (
    <Section contentClassName="flex flex-col gap-4">
      <Chips
        label="Show messages"
        current={status}
        onPick={setStatus}
        items={[
          ["", "All"],
          ["pending", "Waiting"],
          ["sent", "Sent"],
          ["failed", "Failed"],
        ]}
      />
      <div aria-live="polite" aria-busy={q.isFetching || undefined}>
        {q.error ? (
          <LoadError error={q.error} retry={() => void again()} className="shadow-none" />
        ) : !q.data ? (
          <Skeleton className="h-48 rounded-2xl" />
        ) : (
          <>
            <BTable
              caption="Sent e-mail"
              cols={["Time", "To", "Subject", "Status", ""]}
              rows={items.map((m) => ({
                key: m.id,
                cells: [
                  <span className="whitespace-nowrap">{fmtTime(m.sent_at && !m.sent_at.startsWith("0001-") ? m.sent_at : m.created_at)}</span>,
                  <span className="text-xs break-all">{(m.to || []).join(", ")}</span>,
                  <>
                    {m.subject}
                    <span className="block text-xs text-muted-foreground">{m.template || "written by hand"}</span>
                  </>,
                  <>
                    <StatusPill status={m.status} tone={MAIL_TONE[m.status] ?? "neutral"}>
                      {MAIL_STATES[m.status] || m.status}
                    </StatusPill>
                    {m.status !== "sent" && m.attempts ? (
                      <span className="block text-xs text-muted-foreground">{`${m.attempts} attempt${m.attempts === 1 ? "" : "s"}`}</span>
                    ) : null}
                    {m.last_error && (
                      <span className="block max-w-[28ch] text-xs text-danger" title={m.last_error}>
                        {m.last_error.slice(0, 80)}
                      </span>
                    )}
                  </>,
                  <div className="flex justify-end gap-1.5">
                    <Button
                      variant="tinted"
                      size="sm"
                      onClick={() => {
                        setViewing(m)
                        setViewOpen(true)
                      }}
                    >
                      View
                    </Button>
                    {admin && (
                      <ActionButton
                        size="sm"
                        run={async () => {
                          await api("POST", `/email/log/${m.id}/resend`)
                          notify("Queued again")
                          await again()
                        }}
                      >
                        Resend
                      </ActionButton>
                    )}
                  </div>,
                ],
              }))}
              empty={
                <EmptyState icon={MailIcon} tint="blue" title={status ? "Nothing here" : "Nothing sent yet"} className="bg-transparent py-6 shadow-none">
                  Every message the panel sends shows up here, with the server's answer if it failed.
                </EmptyState>
              }
            />
            {q.hasNextPage && (
              <div className="mt-3 flex justify-center">
                <ActionButton run={() => q.fetchNextPage()} disabled={q.isFetchingNextPage}>
                  Load more
                </ActionButton>
              </div>
            )}
          </>
        )}
      </div>
      <Dialog open={viewOpen} onOpenChange={setViewOpen}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-3xl">
          <DialogHeader>
            <DialogTitle>{viewing?.subject}</DialogTitle>
          </DialogHeader>
          {viewing && <MailFrame title={viewing.subject} src={`/api/v1/email/log/${viewing.id}/html`} />}
          <DialogFooter>
            <Button variant="tinted" onClick={() => setViewOpen(false)}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Section>
  )
}

