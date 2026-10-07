import { useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { ActivityIcon, BellIcon, ChartLineIcon, HistoryIcon, MailIcon, PlusIcon, SendIcon, SlidersHorizontalIcon, Trash2Icon, WebhookIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ActionButton, BTable, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Page, PageHeader, Section, StatusHero } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { Severity, StatusText } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtAgo, fmtTime, splitList } from "@/lib/format"
import { queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"

// The Monitoring page: alerts firing and their history, then (for
// administrators) thresholds, where alerts go, and the Prometheus scrape
// token.

interface Alert {
  key: string
  kind: string
  site_id?: string
  target: string
  severity: string
  state: string
  message: string
  since: string
  updated_at: string
  notified_at?: string
}

interface AlertEvent {
  id: number
  key: string
  kind: string
  site_id?: string
  target: string
  severity: string
  state: string
  message: string
  time: string
}

interface Overview {
  active: Alert[]
  history: AlertEvent[]
  watched: number
  evaluated_at?: string
}

interface Email {
  enabled: boolean
  host: string
  port: number
  tls: string
  username: string
  from: string
  to: string[] | null
  password_set: boolean
}

interface Webhook {
  id: string
  name: string
  enabled: boolean
  url_hint?: string
  secret_set: boolean
}

interface Settings {
  disk_warn_percent: number
  disk_critical_percent: number
  cert_warn_days: number
  cert_critical_days: number
  down_after: number
  renotify_hours: number
  email: Email
  webhooks: Webhook[] | null
  metrics_token: { set: boolean; created_at?: string }
  generated_secrets?: Record<string, string>
}

interface ChannelResult {
  channel: string
  ok: boolean
  error?: string
}

const KIND_LABELS: Record<string, string> = {
  site_down: "Site down",
  certificate: "Certificate",
  disk: "Disk",
  backup: "Backup",
  node: "Server",
  attack: "Attack",
}

// The same address the navigation's dot polls: one request feeds both.
const ALERTS = "/monitoring/alerts?limit=200"
const SETTINGS = "/monitoring/settings"

export default function MonitoringPage() {
  const s = useSession()
  const o = useApi<Overview>(ALERTS, { refetchInterval: 60_000 })

  return (
    <Page>
      <PageHeader icon={ActivityIcon} tint="red" title="Monitoring" description="Uptime, certificates, disk space and backups, checked every minute." />
      {o.isLoading && (
        <div className="flex flex-col gap-4">
          <Skeleton className="h-20 rounded-2xl" />
          <Skeleton className="h-48 rounded-2xl" />
        </div>
      )}
      {o.error && !o.data && <LoadError error={o.error} retry={() => o.refetch()} />}
      {o.data && <Alerts o={o.data} />}
      {s.isAdmin && <AdminSettings />}
    </Page>
  )
}

// ---- Active alerts and history ----

function Alerts({ o }: { o: Overview }) {
  const checked = o.evaluated_at ? `last checked ${fmtTime(o.evaluated_at)}` : "not checked yet since the panel started"
  const watched = `${o.watched} target${o.watched === 1 ? "" : "s"} watched, ${checked}.`
  return (
    <>
      <Section
        icon={BellIcon}
        tint="red"
        title="Active alerts"
        description="Every minute WPGenie loads each live site through Caddy (down after a few failed checks in a row), checks the certificate every domain serves, the free disk space and the backups. Sites that never worked yet (DNS not pointing here) aren't judged."
      >
        {o.active.length ? (
          <StatusHero kind="bad" title={`${o.active.length} alert${o.active.length === 1 ? "" : "s"} firing`} sub={watched} />
        ) : (
          <StatusHero kind="ok" title="All systems normal" sub={watched} />
        )}
        {o.active.length > 0 && (
          <BTable
            caption="Alerts firing"
            cols={["Severity", "Since", "Kind", "Target", "Message"]}
            rows={o.active.map((a) => ({
              key: a.key,
              cells: [
                <Severity level={a.severity} />,
                <span title={fmtTime(a.since)}>{fmtAgo(a.since)}</span>,
                KIND_LABELS[a.kind] ?? a.kind,
                <span className="block max-w-56 break-all whitespace-normal">{a.target}</span>,
                <span className="block max-w-md whitespace-normal">{a.message}</span>,
              ],
            }))}
          />
        )}
      </Section>
      <Section icon={HistoryIcon} tint="gray" title="History">
        <BTable
          caption="Alert history"
          cols={["Time", "State", "Severity", "Target", "Message"]}
          empty={<p className="py-2 text-sm text-muted-foreground">No alerts so far.</p>}
          rows={o.history.map((e) => ({
            key: e.id,
            cells: [
              fmtTime(e.time),
              <StatusText status={e.state === "resolved" ? "ok" : "failed"}>{e.state}</StatusText>,
              <Severity level={e.severity} />,
              <span className="block max-w-56 break-all whitespace-normal">{e.target}</span>,
              <span className="block max-w-md whitespace-normal">{e.message}</span>,
            ],
          }))}
        />
      </Section>
    </>
  )
}

// ---- Thresholds, notifications, Prometheus (administrators) ----

function AdminSettings() {
  // Not refetched on focus: the form below starts afresh when the settings
  // change, and would lose what is being typed.
  const q = useQuery<Settings>({
    queryKey: [SETTINGS],
    queryFn: () => api<Settings>("GET", SETTINGS),
    refetchOnWindowFocus: false,
    staleTime: Infinity,
  })
  if (q.isLoading) return <Skeleton className="h-96 rounded-2xl" />
  if (q.error && !q.data) return <LoadError error={q.error} retry={() => q.refetch()} />
  if (!q.data) return null
  return (
    <>
      {/* Fresh fields whenever the saved settings change (not the token). */}
      <SettingsForm key={JSON.stringify({ ...q.data, metrics_token: null })} set={q.data} />
      <Prometheus token={q.data.metrics_token} />
    </>
  )
}

interface Row {
  k: number
  id: string
  name: string
  enabled: boolean
  url_hint?: string
  secret_set: boolean
}

let nextRow = 1

function SettingsForm({ set }: { set: Settings }) {
  const e = set.email
  const [emailOn, setEmailOn] = useState(e.enabled)
  const [rows, setRows] = useState<Row[]>(() => (set.webhooks ?? []).map((w) => ({ ...w, k: nextRow++ })))
  const [busy, setBusy] = useState(false)
  const [testing, setTesting] = useState(false)
  const [results, setResults] = useState<ChannelResult[] | null>(null)

  async function save(ev: FormEvent<HTMLFormElement>) {
    ev.preventDefault()
    const form = ev.currentTarget
    const data = new FormData(form)
    const val = (k: string) => String(data.get(k) ?? "")
    const num = (k: string) => Number(val(k))
    const body = {
      disk_warn_percent: num("disk_warn_percent"),
      disk_critical_percent: num("disk_critical_percent"),
      cert_warn_days: num("cert_warn_days"),
      cert_critical_days: num("cert_critical_days"),
      down_after: num("down_after"),
      renotify_hours: num("renotify_hours"),
      email: {
        enabled: emailOn,
        host: val("email_host").trim(),
        port: Number(val("email_port") || 0),
        tls: val("email_tls"),
        username: val("email_username"),
        password: val("email_password"),
        from: val("email_from").trim(),
        to: splitList(val("email_to")),
      },
      webhooks: rows.map((r) => ({
        id: r.id,
        name: val(`wh-${r.k}-name`),
        enabled: r.enabled,
        url: val(`wh-${r.k}-url`).trim(),
        secret: val(`wh-${r.k}-secret`),
      })),
    }
    setBusy(true)
    try {
      const saved = await api<Settings>("PUT", SETTINGS, body)
      const gen = saved.generated_secrets ?? {}
      const ids = Object.keys(gen)
      if (ids.length)
        showSecret(
          "Webhook signing secrets",
          ids.map((id) => `${saved.webhooks?.find((x) => x.id === id)?.name || id}: ${gen[id]}`)
        )
      notify("Monitoring settings saved")
      // Passwords and secrets typed aren't kept in the page; the form starts
      // afresh from what was saved.
      form.reset()
      queryClient.setQueryData([SETTINGS], { ...saved, generated_secrets: undefined })
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  async function test() {
    setTesting(true)
    setResults(null)
    try {
      setResults(await api<ChannelResult[]>("POST", "/monitoring/test"))
    } catch (err) {
      showError(err)
    } finally {
      setTesting(false)
    }
  }

  const num = (name: keyof Settings, label: string, min: number, max: number, step?: number) => (
    <Field>
      <FieldLabel htmlFor={`mon-${name}`}>{label}</FieldLabel>
      <Input id={`mon-${name}`} name={name} type="number" min={min} max={max} step={step} defaultValue={String(set[name])} required />
    </Field>
  )

  return (
    <Section icon={SlidersHorizontalIcon} tint="gray" title="Thresholds and notifications">
      <form onSubmit={save} className="flex flex-col gap-6">
        <FieldGroup className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {num("disk_warn_percent", "Disk warning at (% used)", 1, 99, 0.1)}
          {num("disk_critical_percent", "Disk critical at (% used)", 2, 100, 0.1)}
          {num("cert_warn_days", "Certificate warning (days left)", 1, 90)}
          {num("cert_critical_days", "Certificate critical (days left)", 0, 89)}
          {num("down_after", "Site down after (failed checks)", 1, 60)}
          {num("renotify_hours", "Repeat firing alerts every (hours, 0 = never)", 0, 168)}
        </FieldGroup>

        <Separator />

        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-3">
            <h3 className="flex items-center gap-2 text-base font-semibold">
              <MailIcon className="size-4 text-muted-foreground" />
              E-mail
            </h3>
            <Label className="ml-auto font-normal">
              <Switch checked={emailOn} onCheckedChange={setEmailOn} />
              Send alerts by e-mail
            </Label>
          </div>
          <FieldGroup className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <Field>
              <FieldLabel htmlFor="mon-email-host">SMTP server</FieldLabel>
              <Input id="mon-email-host" name="email_host" defaultValue={e.host ?? ""} placeholder="smtp.example.com" autoComplete="off" />
            </Field>
            <Field>
              <FieldLabel htmlFor="mon-email-port">Port</FieldLabel>
              <Input id="mon-email-port" name="email_port" type="number" min={1} max={65535} defaultValue={e.port || ""} placeholder="587" />
            </Field>
            <Field>
              <FieldLabel htmlFor="mon-email-tls">Encryption</FieldLabel>
              <NativeSelect id="mon-email-tls" name="email_tls" className="w-full" defaultValue={e.tls || "starttls"}>
                <NativeSelectOption value="starttls">STARTTLS (port 587)</NativeSelectOption>
                <NativeSelectOption value="tls">TLS (port 465)</NativeSelectOption>
              </NativeSelect>
            </Field>
            <Field>
              <FieldLabel htmlFor="mon-email-user">Username</FieldLabel>
              <Input id="mon-email-user" name="email_username" defaultValue={e.username ?? ""} autoComplete="off" />
            </Field>
            <Field>
              <FieldLabel htmlFor="mon-email-password">Password</FieldLabel>
              <Input
                id="mon-email-password"
                name="email_password"
                type="password"
                autoComplete="new-password"
                placeholder={e.password_set ? "unchanged if empty" : "none"}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="mon-email-from">From</FieldLabel>
              <Input id="mon-email-from" name="email_from" defaultValue={e.from ?? ""} placeholder="WPGenie <alerts@example.com>" autoComplete="off" />
            </Field>
            <Field className="sm:col-span-2 lg:col-span-3">
              <FieldLabel htmlFor="mon-email-to">To</FieldLabel>
              <Input id="mon-email-to" name="email_to" defaultValue={(e.to ?? []).join(", ")} placeholder="ops@example.com, you@example.com" autoComplete="off" />
              <FieldDescription>Comma-separated.</FieldDescription>
            </Field>
          </FieldGroup>
        </div>

        <Separator />

        <div className="flex flex-col gap-3">
          <h3 className="flex items-center gap-2 text-base font-semibold">
            <WebhookIcon className="size-4 text-muted-foreground" />
            Webhooks
          </h3>
          <p className="text-sm text-muted-foreground">
            JSON POST with a <code>text</code> field (Slack, Mattermost), <code>content</code> (Discord) and the alerts, signed:{" "}
            <code className="break-all">X-WPGenie-Signature: sha256=HMAC(secret, X-WPGenie-Timestamp + "." + body)</code>. URLs and secrets are never shown again.
          </p>
          {rows.map((r) => (
            <div key={r.k} className="grid items-end gap-3 rounded-xl p-3 ring-1 ring-border sm:grid-cols-[1fr_2fr_1.2fr_auto]">
              <Field>
                <FieldLabel htmlFor={`wh-${r.k}-name`}>Name</FieldLabel>
                <Input id={`wh-${r.k}-name`} name={`wh-${r.k}-name`} defaultValue={r.name} placeholder="Team chat" autoComplete="off" />
              </Field>
              <Field>
                <FieldLabel htmlFor={`wh-${r.k}-url`}>URL</FieldLabel>
                <Input
                  id={`wh-${r.k}-url`}
                  name={`wh-${r.k}-url`}
                  type="url"
                  autoComplete="off"
                  required={!r.id}
                  placeholder={r.url_hint ? `${r.url_hint} (unchanged if empty)` : "https://hooks.slack.com/services/…"}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={`wh-${r.k}-secret`}>Signing secret</FieldLabel>
                <Input
                  id={`wh-${r.k}-secret`}
                  name={`wh-${r.k}-secret`}
                  type="password"
                  autoComplete="new-password"
                  placeholder={r.secret_set ? "unchanged if empty" : "generated if empty"}
                />
              </Field>
              <div className="flex h-9 items-center gap-3">
                <Label className="font-normal">
                  <Switch checked={r.enabled} onCheckedChange={(v) => setRows((xs) => xs.map((x) => (x.k === r.k ? { ...x, enabled: v } : x)))} />
                  On
                </Label>
                <Button
                  type="button"
                  variant="destructive"
                  size="sm"
                  aria-label={`Remove webhook ${r.name || ""}`.trim()}
                  onClick={() => setRows((xs) => xs.filter((x) => x.k !== r.k))}
                >
                  <Trash2Icon data-icon="inline-start" />
                  Remove
                </Button>
              </div>
            </div>
          ))}
          {!rows.length && <p className="text-sm text-muted-foreground">No webhooks.</p>}
          <div>
            <Button
              type="button"
              variant="tinted"
              size="sm"
              onClick={() => setRows((xs) => [...xs, { k: nextRow++, id: "", name: "", enabled: true, secret_set: false }])}
            >
              <PlusIcon data-icon="inline-start" />
              Add webhook
            </Button>
          </div>
        </div>

        <div className="flex flex-wrap justify-end gap-2">
          <Button type="button" variant="tinted" onClick={test} disabled={testing}>
            <SendIcon data-icon="inline-start" />
            {testing ? "Sending…" : "Send test notification"}
          </Button>
          <Button type="submit" disabled={busy}>
            Save
          </Button>
        </div>
      </form>

      {(testing || results) && (
        <div className="mt-4" aria-live="polite">
          {testing && <p className="text-sm text-muted-foreground">Sending… (save your changes first: the test uses the saved settings)</p>}
          {results &&
            (results.length ? (
              <BTable
                caption="Test notification results"
                cols={["Channel", "Result"]}
                rows={results.map((r, i) => ({
                  key: i,
                  cells: [
                    r.channel,
                    r.ok ? <StatusText status="ok" /> : <span className="block max-w-md whitespace-normal text-danger">{r.error}</span>,
                  ],
                }))}
              />
            ) : (
              <p className="text-sm text-muted-foreground">No channel is configured.</p>
            ))}
        </div>
      )}
    </Section>
  )
}

function Prometheus({ token }: { token: Settings["metrics_token"] }) {
  async function rotate() {
    if (token.set && !(await ask("Replace the scrape token? Prometheus stops getting metrics until it has the new one."))) return
    const r = await api<{ token: string }>("POST", "/monitoring/metrics-token")
    showSecret("Prometheus scrape token", [r.token])
    await queryClient.invalidateQueries({ queryKey: [SETTINGS] })
  }
  const scheme = location.protocol.replace(":", "")
  const config = [
    "scrape_configs:",
    "  - job_name: wpgenie",
    `    scheme: ${scheme}`,
    "    static_configs:",
    `      - targets: ['${location.host}']`,
    "    authorization:",
    "      type: Bearer",
    "      credentials_file: /etc/prometheus/wpgenie-token",
  ].join("\n")
  return (
    <Section
      icon={ChartLineIcon}
      tint="orange"
      title="Prometheus"
      description={
        <>
          Metrics for Prometheus at <code>/metrics</code>: traffic, PHP response times, replicas and load, uptime, certificates, disk, memory, backups,
          shield and jobs. Scrapes need their own token (read-only; the API token doesn't work there).
        </>
      }
      action={
        <ActionButton size="sm" run={rotate}>
          {token.set ? "Replace token" : "Create token"}
        </ActionButton>
      }
    >
      <p className="mb-3 text-sm">
        {token.set ? (
          <>Scrape token created {fmtTime(token.created_at)}.</>
        ) : (
          <span className="text-warning">No scrape token yet: /metrics answers 401 to everyone.</span>
        )}
      </p>
      <pre className="overflow-x-auto rounded-xl bg-muted p-3 font-mono text-xs">{config}</pre>
      <p className="mt-3 text-sm text-muted-foreground">
        Scrape from this server (the daemon listens on loopback), through an SSH tunnel, or via the panel domain if you publish it.
      </p>
    </Section>
  )
}
