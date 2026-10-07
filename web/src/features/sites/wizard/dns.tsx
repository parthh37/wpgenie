import { useEffect, useRef, useState } from "react"
import { CheckIcon, RefreshCwIcon, ShieldIcon, TriangleAlertIcon, type LucideIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { ask } from "@/components/app/confirm"
import { errorMessage } from "@/lib/api"
import { cn } from "@/lib/utils"
import type { WizardStep } from "./wizard"

// Connecting a domain before it's used. A domain whose DNS doesn't point
// here gets no certificate from Let's Encrypt, and a site redirecting to it
// sends every visitor to a TLS error. Creating a site and adding a domain
// both go through a check first (GET /dns-check, GET /sites/{id}/dns-check),
// which re-runs while DNS propagates.

export interface DNSCheck {
  domain: string
  status: "ok" | "proxied" | "partly" | "unknown" | "elsewhere" | "missing" | string
  addresses: string[]
  expected: string[]
  // The cluster server the domain points to (automatic placement).
  server?: string
  message: string
}

type Tone = "ok" | "warn" | "bad"
const DNS_STATES: Record<string, { icon: LucideIcon; tone: Tone; title: string }> = {
  ok: { icon: CheckIcon, tone: "ok", title: "Connected" },
  proxied: { icon: ShieldIcon, tone: "ok", title: "Behind Cloudflare" },
  partly: { icon: TriangleAlertIcon, tone: "warn", title: "Partly connected" },
  unknown: { icon: TriangleAlertIcon, tone: "warn", title: "Couldn't verify" },
  elsewhere: { icon: TriangleAlertIcon, tone: "bad", title: "Points to another server" },
  missing: { icon: TriangleAlertIcon, tone: "bad", title: "Not connected yet" },
}
export const DNS_RECHECK_MS = 10_000
export const dnsReady = (r: DNSCheck | null | undefined) => !!r && (r.status === "ok" || r.status === "proxied")

// cleanDomain accepts what people paste: a URL, a trailing dot or slash.
export const cleanDomain = (v: string) =>
  v
    .trim()
    .toLowerCase()
    .replace(/^[a-z]+:\/\//, "")
    .replace(/[/?#].*$/, "")
    .replace(/\.$/, "")

export const validDomain = (d: string) => /^[a-z0-9-]+(\.[a-z0-9-]+)+$/.test(d)

// recordName is what goes in a DNS provider's Name field, assuming the zone
// is the last two labels: "@" for the zone itself.
export function recordName(domain: string) {
  const labels = domain.split(".")
  return labels.length <= 2 ? "@" : labels.slice(0, -2).join(".")
}

export interface DnsState {
  // The domain being checked; result is for it once checked equals it.
  domain: string
  checked: string
  result: DNSCheck | null
  error: string
  busy: boolean
  again: () => void
}

// useDnsCheck checks a domain while enabled, and keeps checking every few
// seconds until it points here (or it's disabled, or the component goes
// away). Each time it's enabled, it checks afresh.
export function useDnsCheck(check: (domain: string) => Promise<DNSCheck>, domain: string, enabled = true): DnsState {
  const [state, setState] = useState<Omit<DnsState, "again" | "domain">>({ checked: "", result: null, error: "", busy: true })
  const [nonce, setNonce] = useState(0)
  const checkRef = useRef(check)
  useEffect(() => {
    checkRef.current = check
  })

  useEffect(() => {
    if (!enabled) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const run = async (quiet: boolean) => {
      if (!quiet) setState((s) => ({ ...s, error: "", busy: true }))
      try {
        const r = await checkRef.current(domain)
        if (cancelled) return
        setState({ checked: domain, result: r, error: "", busy: false })
        if (!dnsReady(r)) timer = setTimeout(() => run(true), DNS_RECHECK_MS)
      } catch (e) {
        if (cancelled) return
        setState({ checked: domain, result: null, error: errorMessage(e), busy: false })
      }
    }
    // After the render: the first lookup isn't a state change of this one.
    timer = setTimeout(() => run(false), 0)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [domain, nonce, enabled])

  return { ...state, domain, again: () => setNonce((n) => n + 1) }
}

const TONE: Record<Tone, string> = {
  ok: "bg-success-fill/12 [&_[data-badge]]:bg-success-fill",
  warn: "bg-warning-fill/12 [&_[data-badge]]:bg-warning-fill",
  bad: "bg-danger-fill/12 [&_[data-badge]]:bg-danger-fill",
}

// DnsPanel shows a domain's check: its state, and while it doesn't point
// here, the records to create.
export function DnsPanel({ dns, many = false, children }: { dns: DnsState; many?: boolean; children?: React.ReactNode }) {
  const { domain, error, busy } = dns
  const r = dns.checked === domain ? dns.result : null
  let body: React.ReactNode
  if (busy && !r) {
    body = (
      <p className="flex items-center gap-2 text-sm text-muted-foreground">
        <Spinner className="size-4" />
        Looking up {domain}…
      </p>
    )
  } else if (error) {
    body = <p className="text-sm text-danger">{error}</p>
  } else if (r) {
    const st = DNS_STATES[r.status] || DNS_STATES.unknown
    const ready = dnsReady(r)
    body = (
      <>
        <div className={cn("flex items-start gap-3 rounded-2xl p-3.5", TONE[st.tone])}>
          <span data-badge aria-hidden className="flex size-8 shrink-0 items-center justify-center rounded-full text-white">
            <st.icon className="size-4" strokeWidth={2.5} />
          </span>
          <div className="min-w-0">
            <strong className="font-semibold">{st.title}</strong>
            <p className="text-sm text-muted-foreground">{r.message}</p>
          </div>
        </div>
        {r.addresses.length > 0 && (
          <p className="text-sm text-muted-foreground">
            {r.domain} resolves to <code>{r.addresses.join(", ")}</code>
          </p>
        )}
        {!ready && r.expected.length > 0 && (
          <>
            <p className="text-sm">
              {many
                ? "At your DNS provider, point it at one of these servers (the site is created on the one it points to):"
                : `At your DNS provider, create ${r.expected.length > 1 ? "these records" : "this record"}` + (r.addresses.length ? " (and remove the others):" : ":")}
            </p>
            <RecordTable r={r} />
          </>
        )}
        {!ready && (
          <div className="flex flex-wrap items-center gap-3">
            <p className="min-w-0 flex-1 text-sm text-muted-foreground">DNS changes usually show up within minutes. Checking again every 10 seconds…</p>
            <Button type="button" variant="tinted" size="sm" disabled={busy} onClick={dns.again}>
              <RefreshCwIcon data-icon="inline-start" />
              Check again
            </Button>
          </div>
        )}
      </>
    )
  }
  return (
    <div aria-live="polite" className="flex flex-col gap-3">
      {body}
      {children}
    </div>
  )
}

function RecordTable({ r }: { r: DNSCheck }) {
  return (
    <div className="overflow-hidden rounded-xl ring-1 ring-border">
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="pl-3">Type</TableHead>
            <TableHead>Name</TableHead>
            <TableHead>Value</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {r.expected.map((a) => {
            const v6 = a.includes(":")
            return (
              <TableRow key={a}>
                <TableCell className="pl-3 align-top font-medium">{v6 ? "AAAA" : "A"}</TableCell>
                <TableCell className="align-top">
                  <code>{recordName(r.domain)}</code>
                  <div className="text-xs text-muted-foreground">{r.domain}</div>
                </TableCell>
                <TableCell className="align-top">
                  <code className="select-all">{a}</code>
                  {v6 && <span className="text-xs text-muted-foreground"> (optional)</span>}
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </div>
  )
}

// dnsStep is the wizard step that checks a domain and won't go on without
// a confirmation while it doesn't point here.
export function dnsStep(
  dns: DnsState,
  content: React.ReactNode,
  { finish, warn, after }: { finish?: string; warn?: string; after?: () => boolean | void | Promise<boolean | void> } = {}
): WizardStep {
  const r = dns.checked === dns.domain ? dns.result : null
  return {
    label: "Connect DNS",
    content,
    button: !r ? { label: finish || "Next", disabled: true } : dnsReady(r) ? { label: finish || "Next" } : { label: (finish || "Continue") + " anyway", danger: true },
    async next() {
      if (
        r &&
        !dnsReady(r) &&
        !(await ask(
          `${r.message}\n${warn || "Its HTTPS certificate is issued once the domain points here; until then visitors get a certificate error."}\nContinue anyway?`,
          { title: `${dns.domain} isn't connected yet`, ok: "Continue anyway", danger: true }
        ))
      )
        return false
      return after ? after() : true
    },
  }
}

