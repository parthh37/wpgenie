import { useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { ActivityIcon, CloudIcon, RefreshCwIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ask } from "@/components/app/confirm"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { StatusPill, StatusText } from "@/components/app/status"
import { showError } from "@/components/app/toaster"
import type { SectionProps } from "@/features/sites/sections"
import { api, errorMessage } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { queryClient } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { FlashButton, Note, Steps, Summary, SwitchRow, Warning } from "./speed/shared"

// CDN: Cloudflare in front of the whole site, or a pull zone (bunny.net or
// any other) for its static files (GET/PUT /sites/{id}/cdn).

interface CDNDomain {
  domain: string
  // yes (every address is Cloudflare's), no, partly, or unknown (doesn't resolve).
  proxied: string
  addrs: string[] | null
  // off, flexible, full, strict; "" when unknown.
  ssl_mode: string
}

interface CDNStatus {
  provider: "" | "cloudflare" | "bunny" | "generic" | string
  asset_host: string
  pull_zone: string
  edge_html: boolean
  domains: CDNDomain[] | null
  purged_at: string | null
  last_error: string
  warnings: string[] | null
}

const CDN_NAMES: Record<string, string> = { cloudflare: "Cloudflare", bunny: "bunny.net", generic: "pull zone" }
const PROXIED: Record<string, string> = { yes: "ok", no: "failed", partly: "failed", unknown: "unknown" }
const SSL: Record<string, string> = { strict: "ok", full: "warning", flexible: "failed", off: "failed" }

export default function CDNSection({ site }: SectionProps) {
  const active = site.status === "active"
  const path = `/sites/${site.id}/cdn`
  // A live check of DNS and the zone (a second or two): on opening, not on
  // every return to the window (a token is often copied from another tab).
  const q = useQuery<CDNStatus>({
    queryKey: [path],
    queryFn: () => api<CDNStatus>("GET", path),
    enabled: active,
    refetchOnWindowFocus: false,
  })
  const st = q.data
  const show = (next: CDNStatus) => queryClient.setQueryData([path], next)

  return (
    <div>
      {st && (
        <Summary>
          {!st.provider ? (
            <StatusPill status="off" tone="neutral">
              Off
            </StatusPill>
          ) : st.provider === "cloudflare" ? (
            <StatusPill status="on" tone="ok">
              Cloudflare, purging on{st.edge_html ? ", pages at the edge" : ""}
            </StatusPill>
          ) : (
            <StatusPill status="on" tone="ok">
              {CDN_NAMES[st.provider] || st.provider} at {st.asset_host}
            </StatusPill>
          )}
        </Summary>
      )}
      <CDNForm site={site} st={st} show={show} />
      {active && (
        <Section icon={ActivityIcon} tint="green" title="Status">
          {q.isLoading ? (
            <div className="flex flex-col gap-2">
              <p className="text-sm text-muted-foreground">Checking DNS and the CDN…</p>
              <Skeleton className="h-24" />
            </div>
          ) : q.error && !st ? (
            <div className="flex flex-wrap items-center gap-3">
              <Warning tone="bad">{errorMessage(q.error)}</Warning>
              <Button variant="tinted" size="sm" onClick={() => q.refetch()}>
                <RefreshCwIcon data-icon="inline-start" />
                Try again
              </Button>
            </div>
          ) : st ? (
            <CDNStatusView st={st} />
          ) : null}
        </Section>
      )}
    </div>
  )
}

function CDNStatusView({ st }: { st: CDNStatus }) {
  const on = !!st.provider
  // The Cloudflare table is about the whole site: not what a pull zone is.
  const pull = st.provider === "bunny" || st.provider === "generic"
  return (
    <div className="flex flex-col gap-3">
      {!pull && (
        <SimpleTable
          headers={["Domain", "Through Cloudflare", "SSL/TLS mode"]}
          rows={(st.domains || []).map((d) => [
            d.domain,
            <span title={(d.addrs || []).join(", ")}>
              <StatusText status={PROXIED[d.proxied] || "unknown"}>{d.proxied}</StatusText>
            </span>,
            d.ssl_mode ? <StatusText status={SSL[d.ssl_mode] || "unknown"}>{d.ssl_mode}</StatusText> : "–",
          ])}
        />
      )}
      {on && st.provider !== "generic" && <Note>{st.purged_at ? `Last purged ${fmtTime(st.purged_at)}.` : "Not purged yet."}</Note>}
      {st.last_error && <Warning tone="bad">Last purge failed: {st.last_error}</Warning>}
      {(st.warnings || []).map((w, i) => (
        <Warning key={i}>{w}</Warning>
      ))}
    </div>
  )
}

function CDNForm({ site, st, show }: { site: Site; st: CDNStatus | undefined; show: (st: CDNStatus) => void }) {
  const s = useSession()
  const active = site.status === "active"
  const current = st?.provider || ""
  const [provider, setProvider] = useState(current || "cloudflare")
  const [token, setToken] = useState("")
  const [zone, setZone] = useState(st?.pull_zone || "")
  const [host, setHost] = useState(st?.asset_host || "")
  const [edge, setEdge] = useState(!!st?.edge_html)
  const [saving, setSaving] = useState(false)

  // A new status (loaded, saved, turned off): its settings in the form.
  const [seen, setSeen] = useState(st)
  if (seen !== st) {
    setSeen(st)
    if (st) {
      if (st.provider) setProvider(st.provider)
      setHost(st.asset_host || "")
      setZone(st.pull_zone || "")
      setEdge(!!st.edge_html)
    }
  }

  const locked = !active || !s.canChange
  const cf = provider === "cloudflare"
  const bunny = provider === "bunny"
  const same = provider === current
  const on = !!current

  const save = async (e: FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      show(
        await api<CDNStatus>("PUT", `/sites/${site.id}/cdn`, {
          provider,
          api_token: token.trim(),
          asset_host: host.trim(),
          pull_zone: zone.trim(),
          edge_html: cf && edge,
        })
      )
      setToken("")
    } catch (err) {
      showError(err)
    }
    setSaving(false)
  }

  const off = async () => {
    const msg =
      current === "cloudflare"
        ? "Stop purging Cloudflare's cache for this site? The stored API token is deleted (and the edge cache rule removed). Cloudflare keeps serving the site."
        : "Stop using the CDN? Static files are served from this server again; stored keys are deleted."
    if (!(await ask(msg))) return
    try {
      show(await api<CDNStatus>("PUT", `/sites/${site.id}/cdn`, { provider: "" }))
    } catch (err) {
      showError(err)
    }
  }

  return (
    <Section icon={CloudIcon} tint="cyan" title="Provider">
      <form onSubmit={save} className="flex flex-col gap-4">
        <Field className="w-auto max-w-sm">
          <FieldLabel htmlFor={`cdn-provider-${site.id}`}>Provider</FieldLabel>
          <NativeSelect id={`cdn-provider-${site.id}`} value={provider} disabled={locked} onChange={(e) => setProvider(e.target.value)} className="w-full">
            <NativeSelectOption value="cloudflare">Cloudflare (whole site)</NativeSelectOption>
            <NativeSelectOption value="bunny">bunny.net pull zone (static files)</NativeSelectOption>
            <NativeSelectOption value="generic">Other pull zone (static files)</NativeSelectOption>
          </NativeSelect>
        </Field>

        {cf ? (
          <div className="flex flex-col gap-2">
            <Note>
              Put the site behind Cloudflare's free CDN: static files are served from a data centre near each visitor. Real visitor IPs reach the shield
              and analytics automatically. With an API token, Cloudflare's cache is also purged whenever the site's cache is.
            </Note>
            <Steps>
              <li>Add the domain to Cloudflare and turn the proxy on (orange cloud) for its A/AAAA records.</li>
              <li>
                Set SSL/TLS to <strong>Full (strict)</strong>. Flexible causes an endless redirect.
              </li>
              <li>
                Create an API token (My Profile → API Tokens) with <em>Zone: Read</em>, <em>Cache Purge: Purge</em> and, optionally,{" "}
                <em>Zone Settings: Read</em> (and <em>Cache Rules: Edit</em> for edge caching), limited to this zone.
              </li>
            </Steps>
          </div>
        ) : (
          <div className="flex flex-col gap-2">
            <Note>
              The site stays on this server; its CSS, JavaScript, images and fonts are linked to the CDN's hostname, which fetches them from here once and
              serves them from near each visitor.
            </Note>
            <Steps>
              <li>
                Create a pull zone whose origin is <code>{"https://" + site.primary_domain}</code>.
              </li>
              <li>
                Give it a hostname of its own (e.g. <code>{"cdn." + site.primary_domain}</code>, a CNAME to the zone) with TLS on.
              </li>
              {bunny && <li>For purges, the bunny.net API key (Account settings → API) and the pull zone's ID.</li>}
            </Steps>
          </div>
        )}

        <div className="flex flex-wrap items-end gap-3">
          {provider !== "generic" && (
            <Field className="w-64">
              <FieldLabel htmlFor={`cdn-token-${site.id}`}>{bunny ? "API key" : "API token"}</FieldLabel>
              <Input
                id={`cdn-token-${site.id}`}
                type="password"
                autoComplete="off"
                value={token}
                disabled={locked}
                placeholder={same ? "unchanged if empty" : bunny ? "paste the bunny.net API key" : "paste a Cloudflare API token"}
                onChange={(e) => setToken(e.target.value)}
              />
            </Field>
          )}
          {bunny && (
            <Field className="w-40">
              <FieldLabel htmlFor={`cdn-zone-${site.id}`}>Pull zone ID</FieldLabel>
              <Input id={`cdn-zone-${site.id}`} inputMode="numeric" value={zone} disabled={locked} onChange={(e) => setZone(e.target.value)} />
            </Field>
          )}
          {!cf && (
            <Field className="w-64">
              <FieldLabel htmlFor={`cdn-host-${site.id}`}>CDN hostname</FieldLabel>
              <Input id={`cdn-host-${site.id}`} value={host} disabled={locked} placeholder="cdn.example.com" onChange={(e) => setHost(e.target.value)} />
            </Field>
          )}
        </div>

        {cf && (
          <div className="flex flex-col gap-2">
            <SwitchRow checked={edge} disabled={locked} onChange={setEdge} title="Cache pages at the edge" />
            <FieldDescription className="max-w-[80ch]">
              Edge caching: Cloudflare keeps pages the page cache serves (never for logged-in visitors, carts or other personal cookies) and is purged with
              it. Those visits no longer reach this server, so they skip the shield and aren't counted in the statistics.
            </FieldDescription>
          </div>
        )}

        {s.canChange && (
          <div className="flex flex-wrap gap-2">
            <Button type="submit" disabled={!active || saving}>
              {saving ? "Checking…" : "Save"}
            </Button>
            {on && current !== "generic" && (
              <FlashButton done="Purged ✓" disabled={!active} run={() => api("POST", `/sites/${site.id}/cdn/purge`)}>
                Purge CDN
              </FlashButton>
            )}
            {on && (
              <Button type="button" variant="destructive" disabled={!active} onClick={off}>
                Turn off
              </Button>
            )}
          </div>
        )}
      </form>
    </Section>
  )
}
