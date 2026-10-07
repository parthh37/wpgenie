import { useState, type FormEvent, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import {
  CheckIcon, CopyIcon, ExternalLinkIcon, GlobeIcon, InboxIcon, KeyRoundIcon, LinkIcon, MailIcon, PlusIcon, PowerIcon, SearchCheckIcon, SendIcon, ServerIcon,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, Banner, LoadError } from "@/components/app/blocks"
import { KeyValues } from "@/components/app/data-table"
import { ask, askText } from "@/components/app/confirm"
import { Page, PageHeader, Section, StatusHero } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { StatusPill } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtAgo, plural } from "@/lib/format"
import { invalidate, useApi, useSites } from "@/lib/query"
import { useSession } from "@/lib/session"

// The Mail page: docker-mailserver (Postfix, Dovecot, Rspamd, DKIM) and
// Roundcube webmail, with the domains, mailboxes and aliases on it and an
// optional outbound relay.

interface Relay {
  host: string
  port: number
  user: string
}

interface MailStatus {
  enabled: boolean
  hostname?: string
  // running | waiting_certificate | waiting_mailbox | starting | stopped | error
  server: string
  // running | stopped | error
  webmail: string
  detail?: string
  relay?: Relay
  checked: string
  webmail_url?: string
}

interface DNSRecord {
  purpose: string
  type: string
  name: string
  value: string
  // ok | missing | mismatch | pending | unknown
  status: string
  found?: string
}

interface MailDomain {
  domain: string
  records: DNSRecord[]
  mailboxes: number
}

interface Mailbox {
  address: string
  domain: string
  quota_mb: number
  site_id?: string
  created_at: string
}

interface Alias {
  alias: string
  target: string
  domain: string
}

const PURPOSE: Record<string, string> = {
  mx: "where mail for the domain goes",
  spf: "servers allowed to send (SPF)",
  dkim: "signing key (DKIM)",
  dmarc: "policy for failed checks (DMARC)",
}

const enc = encodeURIComponent

// useSubmit runs a form's action: busy while it runs, the form emptied
// when it worked, failures as toasts.
function useSubmit(action: (data: FormData) => Promise<unknown>) {
  const [busy, setBusy] = useState(false)
  const onSubmit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const form = e.currentTarget
    setBusy(true)
    try {
      await action(new FormData(form))
      form.reset()
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }
  return { busy, onSubmit }
}

const str = (data: FormData, k: string) => String(data.get(k) ?? "").trim()

export default function MailPage() {
  // The status refreshes itself while the server is on its way up.
  const st = useQuery<MailStatus>({
    queryKey: ["/mail"],
    queryFn: () => api<MailStatus>("GET", "/mail"),
    refetchInterval: (q) => {
      const d = q.state.data
      return d?.enabled && (d.server !== "running" || d.webmail !== "running") ? 10_000 : false
    },
  })

  return (
    <Page>
      <PageHeader icon={MailIcon} tint="blue" title="Mail" description="Mailboxes, forwarding and reliable delivery for your domains." />
      {st.isLoading && (
        <div className="flex flex-col gap-4">
          <Skeleton className="h-20 rounded-2xl" />
          <Skeleton className="h-40 rounded-2xl" />
          <Skeleton className="h-40 rounded-2xl" />
        </div>
      )}
      {st.error && !st.data && <LoadError error={st.error} retry={() => st.refetch()} />}
      {st.data && !st.data.enabled && <MailOff />}
      {st.data?.enabled && (
        <>
          <Verdict st={st.data} />
          <ServerCard st={st.data} />
          <Domains />
          <Mailboxes />
          <Aliases />
          <RelayCard relay={st.data.relay} />
        </>
      )}
    </Page>
  )
}

// ---- Off: turn it on ----

function MailOff() {
  const s = useSession()
  const { busy, onSubmit } = useSubmit(async (data) => {
    await api("PUT", "/mail", { enabled: true, hostname: str(data, "hostname") })
    notify("Mail is starting: it takes a minute or two")
    await invalidate("/mail")
  })
  return (
    <Section
      icon={MailIcon}
      tint="blue"
      title="Mail is off"
      description="Mailboxes with IMAP/SMTP (docker-mailserver: Postfix, Dovecot, Rspamd spam filtering, DKIM) and Roundcube webmail. Choose a hostname for the mail server and point its A record at this server first."
    >
      {s.isAdmin ? (
        <form onSubmit={onSubmit} className="flex flex-wrap items-end gap-2">
          <Field className="w-72 max-sm:w-full">
            <FieldLabel htmlFor="mail-hostname">Mail server hostname</FieldLabel>
            <Input id="mail-hostname" name="hostname" placeholder="mail.example.com" required autoComplete="off" />
          </Field>
          <Button type="submit" disabled={busy}>
            <PowerIcon data-icon="inline-start" />
            {busy ? "Enabling…" : "Enable mail"}
          </Button>
        </form>
      ) : (
        <p className="text-sm text-muted-foreground">An administrator can turn mail on.</p>
      )}
      <p className="mt-4 text-sm text-muted-foreground">
        Open ports 25, 465, 587 and 993 in your firewall, and ask your provider to set this server's reverse DNS (PTR) to the hostname.
      </p>
    </Section>
  )
}

// ---- On: how it's doing ----

const WAITING: Record<string, string> = {
  waiting_certificate: "Waiting for the mail server's certificate",
  waiting_mailbox: "Waiting for the first mailbox",
  starting: "The mail server is starting",
  stopped: "The mail server is stopped",
}

function Verdict({ st }: { st: MailStatus }) {
  const checked = st.checked && !st.checked.startsWith("0001") ? `checked ${fmtAgo(st.checked)}` : ""
  if (st.server === "running" && st.webmail === "running")
    return <StatusHero kind="ok" title="Mail is running" sub={[st.hostname, checked].filter(Boolean).join(" · ")} />
  if (st.server === "error" || st.webmail === "error")
    return <StatusHero kind="bad" title="Mail isn't working" sub={st.detail || "The mail server or webmail failed to start."} />
  return (
    <Banner tone="warn" icon={ServerIcon} title={WAITING[st.server] ?? (st.webmail !== "running" ? "Webmail is starting" : "Mail is starting")}>
      {st.detail ||
        (st.server === "waiting_mailbox"
          ? "The mail server starts once the first mailbox exists."
          : "This refreshes by itself; it usually takes a minute or two.")}
    </Banner>
  )
}

function ServerCard({ st }: { st: MailStatus }) {
  const s = useSession()
  async function disable() {
    if (!(await ask("Stop the mail server and webmail? Mailboxes and mail are kept on disk."))) return
    await api("PUT", "/mail", { enabled: false })
    notify("Mail is off")
    await invalidate("/mail")
  }
  const host = st.hostname ?? ""
  return (
    <Section
      icon={ServerIcon}
      tint="blue"
      title={host}
      action={
        s.isAdmin && (
          <ActionButton variant="destructive" size="sm" run={disable}>
            <PowerIcon data-icon="inline-start" />
            Disable mail
          </ActionButton>
        )
      }
    >
      <KeyValues
        items={[
          ["Mail server (SMTP/IMAP)", <StatusPill status={st.server} />],
          ["Webmail", <StatusPill status={st.webmail} />],
          !!st.webmail_url && [
            "Webmail address",
            <a href={st.webmail_url} target="_blank" rel="noopener" className="inline-flex items-center gap-1 text-link">
              {st.webmail_url}
              <ExternalLinkIcon className="size-3.5" />
            </a>,
          ],
          [
            "Mail clients",
            <span>
              IMAP <code>{host}:993</code> (SSL), SMTP <code>{host}:587</code> (STARTTLS)
            </span>,
          ],
        ]}
      />
      {st.detail && <p className="mt-3 text-sm text-muted-foreground">{st.detail}</p>}
    </Section>
  )
}

// ---- Domains ----

function Domains() {
  const s = useSession()
  const ds = useApi<MailDomain[]>("/mail/domains")
  const { busy, onSubmit } = useSubmit(async (data) => {
    const domain = str(data, "domain")
    await api("POST", "/mail/domains", { domain })
    notify(`${domain} added: publish its DNS records below`)
    await invalidate("/mail/domains")
  })
  return (
    <Section icon={GlobeIcon} tint="blue" title="Domains">
      {s.atLeast("operator") && (
        <form onSubmit={onSubmit} className="mb-4 flex flex-wrap gap-2">
          <Input name="domain" placeholder="example.com" aria-label="Domain" required autoComplete="off" className="w-64 max-sm:w-full" />
          <Button type="submit" disabled={busy}>
            <PlusIcon data-icon="inline-start" />
            Add domain
          </Button>
        </form>
      )}
      {ds.isLoading && <Skeleton className="h-24 rounded-xl" />}
      {ds.error && <LoadError error={ds.error} retry={() => ds.refetch()} />}
      {ds.data && !ds.data.length && <p className="text-sm text-muted-foreground">Add a domain to create mailboxes on it.</p>}
      <div className="flex flex-col gap-3">
        {ds.data?.map((d) => (
          <DomainCard key={d.domain} d={d} />
        ))}
      </div>
    </Section>
  )
}

function DomainCard({ d }: { d: MailDomain }) {
  const s = useSession()
  const [checked, setChecked] = useState<MailDomain | null>(null)
  const records = (checked ?? d).records
  const bad = records.filter((r) => r.status === "missing" || r.status === "mismatch").length
  const allOK = records.length > 0 && records.every((r) => r.status === "ok")

  async function check() {
    setChecked(await api<MailDomain>("GET", `/mail/domains/${enc(d.domain)}?check=1`))
  }
  async function remove() {
    if (!(await ask(`Stop accepting mail for ${d.domain}?`))) return
    await api("DELETE", `/mail/domains/${enc(d.domain)}`)
    notify(`${d.domain} removed`)
    await invalidate("/mail/domains")
  }

  return (
    <div className="rounded-2xl p-4 ring-1 ring-border">
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <strong className="font-semibold">{d.domain}</strong>
        <span className="text-sm text-muted-foreground">· {plural(d.mailboxes, "mailbox", "mailboxes")}</span>
        {allOK && <StatusPill status="ok">DNS ready</StatusPill>}
        {bad > 0 && (
          <StatusPill status="failed">
            {bad} record{bad === 1 ? "" : "s"} to fix
          </StatusPill>
        )}
        <div className="ml-auto flex gap-1.5">
          <ActionButton size="sm" run={check}>
            <SearchCheckIcon data-icon="inline-start" />
            Check DNS
          </ActionButton>
          {s.atLeast("operator") && (
            <ActionButton size="sm" variant="destructive" run={remove}>
              Remove
            </ActionButton>
          )}
        </div>
      </div>
      <p className="mb-2 text-sm text-muted-foreground">Publish these records at your DNS provider. The DKIM key is long: most providers split it automatically.</p>
      <ul className="flex flex-col divide-y divide-border/60" aria-label={`DNS records for ${d.domain}`}>
        {records.map((r, i) => (
          <li key={`${r.purpose}-${i}`} className="flex flex-col gap-1.5 py-2.5 last:pb-0">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
              <StatusPill status={r.status} />
              <Badge variant="outline">{r.type}</Badge>
              <code className="text-xs break-all">{r.name}</code>
              {PURPOSE[r.purpose] && <span className="text-xs text-muted-foreground">{PURPOSE[r.purpose]}</span>}
            </div>
            <div className="flex items-start gap-1.5" title={r.found ? "found: " + r.found : undefined}>
              <code className="min-w-0 flex-1 rounded-lg bg-muted px-2.5 py-1.5 font-mono text-xs break-all">{r.value}</code>
              <CopyButton text={r.value} label={`Copy the ${r.type} record's value`} />
            </div>
            {r.found && r.status !== "ok" && <p className="text-xs break-all text-danger">Found instead: {r.found}</p>}
          </li>
        ))}
      </ul>
    </div>
  )
}

function CopyButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon-sm"
      aria-label={label}
      title="Copy"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        } catch {
          showError(new Error("Copying didn't work here: select the text and copy it."))
        }
      }}
    >
      {copied ? <CheckIcon /> : <CopyIcon />}
    </Button>
  )
}

// ---- Mailboxes ----

function Mailboxes() {
  const s = useSession()
  const boxes = useApi<Mailbox[]>("/mail/mailboxes")
  const { data: sites } = useSites()
  const siteName = (id: string) => sites?.find((x) => x.id === id)?.primary_domain ?? id
  const can = s.atLeast("operator")

  const { busy, onSubmit } = useSubmit(async (data) => {
    const r = await api<{ mailbox: Mailbox; password: string }>("POST", "/mail/mailboxes", {
      address: str(data, "address"),
      quota_mb: Number(str(data, "quota_mb") || 0),
    })
    showSecret(`Mailbox ${r.mailbox.address} created`, [`Address:  ${r.mailbox.address}`, `Password: ${r.password}`])
    // The status (the first mailbox starts the server) and the domains' counts change too.
    await invalidate("/mail")
  })

  async function newPassword(b: Mailbox) {
    if (!(await ask(`Replace the password of ${b.address}? Mail apps using the old one stop working.`))) return
    const r = await api<{ password: string }>("PUT", `/mail/mailboxes/${enc(b.address)}/password`, {})
    showSecret(`New password for ${b.address}`, [`Password: ${r.password}`])
  }

  async function quota(b: Mailbox) {
    const v = await askText("In MB; 0 means unlimited.", {
      title: `Quota of ${b.address}`,
      label: "Quota (MB)",
      type: "number",
      value: String(b.quota_mb),
      ok: "Save quota",
      danger: false,
    })
    if (v === null) return
    const n = Number(v || 0)
    if (!Number.isInteger(n) || n < 0) throw new Error("The quota is a whole number of MB (0 = unlimited).")
    await api("PUT", `/mail/mailboxes/${enc(b.address)}/quota`, { quota_mb: n })
    notify(n ? `${b.address} can keep ${n} MB` : `${b.address} has no quota`)
    await invalidate("/mail/mailboxes")
  }

  async function remove(b: Mailbox) {
    const typed = await askText(`This deletes ${b.address} and all its mail.`, {
      title: `Delete ${b.address}?`,
      label: "Type the address to confirm",
      match: b.address,
      ok: "Delete mailbox",
    })
    if (typed !== b.address) return
    await api("DELETE", `/mail/mailboxes/${enc(b.address)}`)
    notify(`${b.address} deleted`)
    await invalidate("/mail")
  }

  return (
    <Section icon={InboxIcon} tint="blue" title="Mailboxes">
      {can && (
        <form onSubmit={onSubmit} className="mb-4 flex flex-wrap gap-2">
          <Input name="address" placeholder="jane@example.com" aria-label="Address" required autoComplete="off" className="w-64 max-sm:w-full" />
          <Input name="quota_mb" type="number" min={0} placeholder="Quota MB (0 = unlimited)" aria-label="Quota in MB (0 = unlimited)" className="w-56 max-sm:w-full" />
          <Button type="submit" disabled={busy}>
            <PlusIcon data-icon="inline-start" />
            Create mailbox
          </Button>
        </form>
      )}
      <Listing q={boxes}>
        {(list) => (
          <BTable
            caption="Mailboxes"
            cols={["Address", "Quota", ""]}
            empty={<p className="py-2 text-sm text-muted-foreground">No mailboxes yet.</p>}
            rows={list.map((b) => ({
              key: b.address,
              cells: [
                <span className="inline-flex flex-wrap items-center gap-1.5">
                  <span className="font-medium">{b.address}</span>
                  {b.site_id && <Badge variant="secondary">WordPress sender of {siteName(b.site_id)}</Badge>}
                </span>,
                b.quota_mb ? `${b.quota_mb} MB` : "unlimited",
                can && !b.site_id ? (
                  <div className="flex justify-end gap-1.5">
                    <ActionButton size="sm" run={() => newPassword(b)}>
                      <KeyRoundIcon data-icon="inline-start" />
                      New password
                    </ActionButton>
                    <ActionButton size="sm" run={() => quota(b)}>
                      Quota
                    </ActionButton>
                    <ActionButton size="sm" variant="destructive" run={() => remove(b)}>
                      Delete
                    </ActionButton>
                  </div>
                ) : null,
              ],
            }))}
          />
        )}
      </Listing>
    </Section>
  )
}

// ---- Aliases ----

function Aliases() {
  const s = useSession()
  const aliases = useApi<Alias[]>("/mail/aliases")
  const can = s.atLeast("operator")
  const { busy, onSubmit } = useSubmit(async (data) => {
    const alias = str(data, "alias")
    await api("POST", "/mail/aliases", { alias, target: str(data, "target") })
    notify(`${alias} forwards now`)
    await invalidate("/mail/aliases")
  })

  async function remove(a: Alias) {
    await api("DELETE", `/mail/aliases?alias=${enc(a.alias)}&target=${enc(a.target)}`)
    notify(`${a.alias} no longer forwards to ${a.target}`)
    await invalidate("/mail/aliases")
  }

  return (
    <Section icon={LinkIcon} tint="blue" title="Aliases (forwarding)">
      {can && (
        <form onSubmit={onSubmit} className="mb-4 flex flex-wrap gap-2">
          <Input name="alias" placeholder="info@example.com" aria-label="Alias" required autoComplete="off" className="w-64 max-sm:w-full" />
          <Input name="target" placeholder="jane@example.com or any address" aria-label="Delivers to" required autoComplete="off" className="w-72 max-sm:w-full" />
          <Button type="submit" disabled={busy}>
            <PlusIcon data-icon="inline-start" />
            Add alias
          </Button>
        </form>
      )}
      <Listing q={aliases}>
        {(list) => (
          <BTable
            caption="Aliases"
            cols={["Alias", "Delivers to", ""]}
            empty={<p className="py-2 text-sm text-muted-foreground">No aliases yet.</p>}
            rows={list.map((a) => ({
              key: `${a.alias}>${a.target}`,
              cells: [
                <span className="font-medium">{a.alias}</span>,
                a.target,
                can ? (
                  <div className="flex justify-end">
                    <ActionButton size="sm" variant="destructive" run={() => remove(a)}>
                      Remove
                    </ActionButton>
                  </div>
                ) : null,
              ],
            }))}
          />
        )}
      </Listing>
    </Section>
  )
}

// Listing: a list's skeleton, its failure, or the list.
function Listing<T>({ q, children }: { q: { data?: T[]; isLoading: boolean; error: unknown; refetch: () => unknown }; children: (list: T[]) => ReactNode }) {
  if (q.isLoading) return <Skeleton className="h-24 rounded-xl" />
  if (q.error && !q.data) return <LoadError error={q.error} retry={() => q.refetch()} />
  return <>{children(q.data ?? [])}</>
}

// ---- Outbound relay ----

function RelayCard({ relay }: { relay?: Relay }) {
  const s = useSession()
  return (
    <Section
      icon={SendIcon}
      tint="blue"
      title="Outbound relay"
      description="Many clouds block outbound port 25. Relay through a provider (Amazon SES, Postmark, Mailgun, …) for delivery and reputation."
    >
      {s.isAdmin ? (
        // Fresh fields whenever the saved relay changes.
        <RelayForm key={JSON.stringify(relay ?? null)} relay={relay} />
      ) : relay ? (
        <p className="text-sm">
          Mail goes out through <code>{relay.host}:{relay.port}</code>
          {relay.user ? ` as ${relay.user}` : ""}.
        </p>
      ) : (
        <p className="text-sm text-muted-foreground">No relay: this server delivers mail itself.</p>
      )}
    </Section>
  )
}

function RelayForm({ relay }: { relay?: Relay }) {
  const [busy, setBusy] = useState(false)

  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const data = new FormData(e.currentTarget)
    setBusy(true)
    try {
      await api("PUT", "/mail/relay", {
        host: str(data, "host"),
        port: Number(str(data, "port") || 587),
        user: str(data, "user"),
        password: String(data.get("password") ?? ""),
      })
      notify("Relay saved: mail goes out through it now")
      await invalidate("/mail")
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  async function clear() {
    if (!(await ask("Remove the relay? This server delivers mail itself again (port 25 must be open)."))) return
    await api("DELETE", "/mail/relay")
    notify("Relay removed")
    await invalidate("/mail")
  }

  return (
    <form onSubmit={save}>
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="relay-host">SMTP host</FieldLabel>
          <Input id="relay-host" name="host" defaultValue={relay?.host ?? ""} placeholder="email-smtp.eu-west-1.amazonaws.com" autoComplete="off" required />
        </Field>
        <Field>
          <FieldLabel htmlFor="relay-port">Port</FieldLabel>
          <Input id="relay-port" name="port" type="number" min={1} max={65535} defaultValue={relay?.port ?? ""} placeholder="587" />
        </Field>
        <Field>
          <FieldLabel htmlFor="relay-user">Username</FieldLabel>
          <Input id="relay-user" name="user" defaultValue={relay?.user ?? ""} autoComplete="off" />
        </Field>
        <Field>
          <FieldLabel htmlFor="relay-password">Password</FieldLabel>
          <Input id="relay-password" name="password" type="password" autoComplete="new-password" placeholder={relay ? "unchanged if empty" : ""} />
          {relay && <FieldDescription>Kept only while the host, port and username stay the same.</FieldDescription>}
        </Field>
      </FieldGroup>
      <div className="mt-4 flex flex-wrap justify-end gap-2">
        {relay && (
          <ActionButton variant="destructive" run={clear}>
            Remove relay
          </ActionButton>
        )}
        <Button type="submit" disabled={busy}>
          Save relay
        </Button>
      </div>
    </form>
  )
}
