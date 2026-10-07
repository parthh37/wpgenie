import { useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { CheckIcon, RefreshCwIcon, ShieldCheckIcon, TriangleAlertIcon, type LucideIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { ChoiceCard } from "@/components/app/blocks"
import { SimpleTable } from "@/components/app/data-table"
import { ask } from "@/components/app/confirm"
import { notify, showError } from "@/components/app/toaster"
import { api, errorMessage } from "@/lib/api"
import { invalidate } from "@/lib/query"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import type { DNSCheck } from "./types"

// Connecting a domain before it's used (the legacy wizard.js). A domain
// whose DNS doesn't point here gets no certificate from Let's Encrypt,
// and a site redirecting to it sends every visitor to a TLS error: adding
// a domain checks it first (GET /sites/{id}/dns-check), and re-checks
// while DNS propagates.

type Tone = "ok" | "warn" | "bad"
const DNS_STATES: Record<string, { icon: LucideIcon; tone: Tone; title: string }> = {
  ok: { icon: CheckIcon, tone: "ok", title: "Connected" },
  proxied: { icon: ShieldCheckIcon, tone: "ok", title: "Behind Cloudflare" },
  partly: { icon: TriangleAlertIcon, tone: "warn", title: "Partly connected" },
  unknown: { icon: TriangleAlertIcon, tone: "warn", title: "Couldn't verify" },
  elsewhere: { icon: TriangleAlertIcon, tone: "bad", title: "Points to another server" },
  missing: { icon: TriangleAlertIcon, tone: "bad", title: "Not connected yet" },
}
const DNS_RECHECK_MS = 10_000

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
function recordName(domain: string) {
  const labels = domain.split(".")
  return labels.length <= 2 ? "@" : labels.slice(0, -2).join(".")
}

const checkPath = (siteId: string, domain: string) => `/sites/${siteId}/dns-check?domain=${encodeURIComponent(domain)}`

// checkDNS: a one-off check of where a domain points.
export const checkDNS = (siteId: string, domain: string) => api<DNSCheck>("GET", checkPath(siteId, domain))

// useDNSCheck checks a domain and keeps re-checking every few seconds until
// it points here.
export function useDNSCheck(siteId: string, domain: string, enabled = true) {
  return useQuery<DNSCheck>({
    queryKey: [checkPath(siteId, domain)],
    queryFn: () => checkDNS(siteId, domain),
    enabled: enabled && !!domain,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchInterval: (q) => (q.state.status === "error" || dnsReady(q.state.data) ? false : DNS_RECHECK_MS),
  })
}

// DNSPanel shows a domain's check: its verdict, where it resolves to, and
// the records to create while it doesn't point here.
export function DNSPanel({ domain, check }: { domain: string; check: ReturnType<typeof useDNSCheck> }) {
  const { data: r, error, isPending, isFetching, refetch } = check
  if (isPending && !error) {
    return (
      <p aria-live="polite" className="flex items-center gap-2 text-sm text-muted-foreground">
        <Spinner />
        Looking up {domain}…
      </p>
    )
  }
  if (error || !r) {
    return (
      <div aria-live="polite" className="flex flex-col items-start gap-3">
        <p className="text-sm text-danger">{errorMessage(error)}</p>
        <Button type="button" variant="tinted" size="sm" disabled={isFetching} onClick={() => refetch()}>
          <RefreshCwIcon data-icon="inline-start" />
          Check again
        </Button>
      </div>
    )
  }
  const st = DNS_STATES[r.status] || DNS_STATES.unknown
  const ready = dnsReady(r)
  return (
    <div aria-live="polite" className="flex flex-col gap-3">
      <div
        className={cn(
          "flex items-start gap-3 rounded-2xl p-3.5",
          st.tone === "ok" && "bg-success-fill/12",
          st.tone === "warn" && "bg-warning-fill/12",
          st.tone === "bad" && "bg-danger-fill/12"
        )}
      >
        <span
          aria-hidden
          className={cn(
            "flex size-8 shrink-0 items-center justify-center rounded-full text-white [&_svg]:size-4",
            st.tone === "ok" && "bg-success-fill",
            st.tone === "warn" && "bg-warning-fill",
            st.tone === "bad" && "bg-danger-fill"
          )}
        >
          <st.icon strokeWidth={2.5} />
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
            At your DNS provider, create {r.expected.length > 1 ? "these records" : "this record"}
            {r.addresses.length ? " (and remove the others):" : ":"}
          </p>
          <SimpleTable
            headers={["Type", "Name", "Value"]}
            rows={r.expected.map((a) => {
              const v6 = a.includes(":")
              return [
                v6 ? "AAAA" : "A",
                <>
                  <code>{recordName(r.domain)}</code>
                  <div className="text-xs text-muted-foreground">{r.domain}</div>
                </>,
                <>
                  <code className="[overflow-wrap:anywhere]">{a}</code>
                  {v6 && <span className="text-xs text-muted-foreground"> (optional)</span>}
                </>,
              ]
            })}
          />
        </>
      )}
      {!ready && (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-muted-foreground">DNS changes usually show up within minutes. Checking again every 10 seconds…</p>
          <Button type="button" variant="tinted" size="sm" disabled={isFetching} onClick={() => refetch()}>
            {isFetching ? <Spinner data-icon="inline-start" /> : <RefreshCwIcon data-icon="inline-start" />}
            Check again
          </Button>
        </div>
      )}
    </div>
  )
}

// checkBeforePrimary: making a domain primary redirects every visitor to
// it, so it must point here. Resolves to whether to go ahead.
export async function checkBeforePrimary(site: Site, d: string) {
  let r: DNSCheck | null = null
  try {
    r = await checkDNS(site.id, d)
  } catch (e) {
    showError(e)
  }
  const change = `Links in the database are rewritten to it and ${site.primary_domain} redirects to it.`
  if (dnsReady(r)) return ask(`Make ${d} the primary domain? ${change}`)
  return ask(
    `${r ? r.message : "Its DNS couldn't be checked."}\nEvery visitor to ${site.primary_domain} would be redirected to ${d} and get a certificate error until it points here. ` +
      `${change}\nMake it primary anyway?`,
    { title: `${d} isn't connected`, ok: "Make primary anyway", danger: true }
  )
}

// ---- Adding a domain to a site ----

export interface DomainPreset {
  domain: string
  redirect: boolean
}

// AddDomainDialog adds a domain to a site, serving it or redirecting to
// the primary domain, once its DNS is checked. A preset starts at the
// check.
export function AddDomainDialog({
  site,
  preset,
  open,
  onOpenChange,
}: {
  site: Site
  preset?: DomainPreset
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-xl">
        {/* Inside the popup: the steps start afresh each time it opens. */}
        <AddDomainSteps site={site} preset={preset} close={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  )
}

const STEPS = ["Domain", "Connect DNS"]
const WARN = "Caddy keeps trying to get its certificate; once DNS points here it works. Until then, visitors to it get a certificate error."

function AddDomainSteps({ site, preset, close }: { site: Site; preset?: DomainPreset; close: () => void }) {
  const [step, setStep] = useState(preset ? 1 : 0)
  const [domain, setDomain] = useState(preset?.domain ?? "")
  const [redirect, setRedirect] = useState(!preset || preset.redirect !== false)
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const d = cleanDomain(domain)
  const check = useDNSCheck(site.id, d, step === 1)
  const result = check.data
  const ready = dnsReady(result)

  const go = (n: number) => {
    setError("")
    setConfirming(false)
    setStep(n)
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (busy) return
    setError("")
    if (step === 0) {
      if (!validDomain(d)) {
        setError("Enter a domain like example.org")
        return
      }
      setDomain(d)
      go(1)
      return
    }
    if (!result) return
    // Not pointing here: the button asks once more, in place.
    if (!ready && !confirming) {
      setConfirming(true)
      return
    }
    setBusy(true)
    try {
      await api("POST", `/sites/${site.id}/domains`, { domain: d, redirect })
      notify(`${d} ${redirect ? "now redirects to " + site.primary_domain : "now serves the site"}`)
      await invalidate("/sites")
      close()
    } catch (ex) {
      setError(errorMessage(ex))
    } finally {
      setBusy(false)
    }
  }

  const label = step === 0 ? "Next" : !result || ready ? "Add domain" : confirming ? "Continue anyway" : "Add domain anyway"

  return (
    <form onSubmit={submit} noValidate className="flex flex-col gap-5">
      <DialogHeader>
        <DialogTitle>Add a domain to {site.primary_domain}</DialogTitle>
      </DialogHeader>
      <ol className="flex flex-wrap gap-x-5 gap-y-2 text-sm" aria-label="Steps">
        {STEPS.map((s, i) => (
          <li key={s} aria-current={i === step ? "step" : undefined} className={cn("flex items-center gap-2", i !== step && "text-muted-foreground")}>
            <span
              className={cn(
                "flex size-6 items-center justify-center rounded-full text-xs font-semibold",
                i < step ? "bg-success-fill text-white" : i === step ? "bg-primary text-primary-foreground" : "bg-muted"
              )}
            >
              {i < step ? <CheckIcon className="size-3.5" strokeWidth={3} /> : i + 1}
            </span>
            {s}
          </li>
        ))}
      </ol>

      {step === 0 ? (
        <div className="flex flex-col gap-4">
          <Field>
            <FieldLabel htmlFor="add-domain">Domain</FieldLabel>
            <Input
              id="add-domain"
              autoFocus
              autoComplete="off"
              spellCheck={false}
              placeholder="example.org"
              value={domain}
              aria-invalid={error ? true : undefined}
              onChange={(e) => setDomain(e.target.value)}
            />
          </Field>
          <div role="radiogroup" aria-label="What the domain does" className="grid gap-2">
            <ChoiceCard name="mode" value="redirect" title={`Redirect to ${site.primary_domain}`} checked={redirect} onChange={() => setRedirect(true)}>
              For www, old names and typos: visitors land on the primary domain (301, path kept).
            </ChoiceCard>
            <ChoiceCard name="mode" value="serve" title="Serve the site on it too" checked={!redirect} onChange={() => setRedirect(false)}>
              The site answers on both names. Search engines prefer one: redirecting is usually better.
            </ChoiceCard>
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          <DNSPanel domain={d} check={check} />
          {confirming && result && !ready && (
            <div role="alert" className="rounded-2xl bg-danger-fill/10 p-3.5 text-sm">
              <strong className="font-semibold">{d} isn't connected yet</strong>
              <p className="mt-1">{result.message}</p>
              <p className="mt-1 text-muted-foreground">{WARN}</p>
              <p className="mt-1">Continue anyway?</p>
            </div>
          )}
          <FieldDescription>
            {redirect ? `${d} will redirect to ${site.primary_domain}.` : `The site will answer on ${d} too.`}
          </FieldDescription>
        </div>
      )}

      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}

      <DialogFooter>
        <Button type="button" variant="tinted" onClick={close}>
          Cancel
        </Button>
        {step > 0 && (
          <Button type="button" variant="tinted" onClick={() => go(step - 1)}>
            Back
          </Button>
        )}
        <Button
          type="submit"
          variant={step === 1 && result && !ready ? "destructive-solid" : "default"}
          disabled={busy || (step === 1 && !result)}
        >
          {label}
        </Button>
      </DialogFooter>
    </form>
  )
}
