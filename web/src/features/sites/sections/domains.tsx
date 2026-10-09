import { useState } from "react"
import { LinkIcon, LockIcon, PlusIcon, UploadIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { ActionButton, BTable, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { KeyValues } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "@/features/sites/sections"
import { AddDomainDialog, checkBeforePrimary, type DomainPreset } from "./data/dns"
import { useSiteJob } from "./data/jobs"
import type { SiteCert } from "./data/types"
import { Redirects } from "./domains/redirects"
import { SiteLock } from "./domains/site-lock"

// Domains & SSL: the names a site answers on (serving it, or redirecting
// to the primary one), changing the primary domain, and its certificate:
// automatic (Let's Encrypt, through Caddy) or uploaded. Then who may see
// it (password protection) and where its old paths lead (redirects).

export default function DomainsSection({ site }: SectionProps) {
  return (
    <div className="flex flex-col">
      <Domains site={site} />
      <Certificate site={site} />
      <SiteLock site={site} />
      <Redirects site={site} />
    </div>
  )
}

// ---- Domains ----

function Domains({ site }: { site: Site }) {
  const s = useSession()
  const primary = useSiteJob(site.id, ["primary-domain"])
  const [wizard, setWizard] = useState<{ open: boolean; preset?: DomainPreset }>({ open: false })
  const domainPath = (d: string) => `/sites/${site.id}/domains/${encodeURIComponent(d)}`

  const makePrimary = async (d: string) => {
    if (!(await checkBeforePrimary(site, d))) return
    await primary.run("PUT", `/sites/${site.id}/primary-domain`, { domain: d })
  }
  const setRedirect = async (d: string, redirect: boolean) => {
    await api("PUT", domainPath(d), { redirect })
    notify(redirect ? `${d} now redirects to ${site.primary_domain}` : `${d} now serves the site`)
    await invalidate("/sites")
  }
  const remove = async (d: string) => {
    if (!(await ask(`Stop answering on ${d}?`))) return
    await api("DELETE", domainPath(d))
    notify(`${d} removed`)
    await invalidate("/sites")
  }

  const actions = (d: string, redirecting: boolean) =>
    s.canChangeSite(site, "manager") && (
      <div className="flex flex-wrap justify-end gap-1.5">
        <ActionButton run={() => makePrimary(d)} size="sm" disabled={primary.running}>
          Make primary
        </ActionButton>
        <ActionButton run={() => setRedirect(d, !redirecting)} size="sm" variant="ghost">
          {redirecting ? "Serve instead" : "Redirect instead"}
        </ActionButton>
        <ActionButton run={() => remove(d)} size="sm" variant="destructive">
          Remove
        </ActionButton>
      </div>
    )

  const aliases = site.domains.filter((x) => x !== site.primary_domain)
  const rows = [
    {
      key: site.primary_domain,
      cells: [
        <span className="flex flex-wrap items-center gap-2">
          <strong className="font-semibold [overflow-wrap:anywhere]">{site.primary_domain}</strong>
          <Badge>primary</Badge>
          {primary.running && <Badge variant="secondary">changing…</Badge>}
        </span>,
        <span className="text-muted-foreground">serves the site</span>,
        null,
      ],
    },
    ...aliases.map((d) => ({
      key: d,
      cells: [<span className="[overflow-wrap:anywhere]">{d}</span>, <span className="text-muted-foreground">serves the site</span>, actions(d, false)],
    })),
    ...site.redirect_domains.map((d) => ({
      key: d,
      cells: [
        <span className="[overflow-wrap:anywhere]">{d}</span>,
        <span className="text-muted-foreground">redirects to {site.primary_domain}</span>,
        actions(d, true),
      ],
    })),
  ]

  const apex = site.primary_domain.replace(/^www\./, "")
  const twin = site.primary_domain.startsWith("www.") ? apex : "www." + site.primary_domain
  const known = [...site.domains, ...site.redirect_domains]
  const more = aliases.length + site.redirect_domains.length

  return (
    <Section
      icon={LinkIcon}
      tint="blue"
      title="Domains"
      description={more ? `${site.primary_domain} and ${more} more domain(s).` : `The site answers on ${site.primary_domain} only.`}
      action={
        s.canChangeSite(site, "manager") && (
          <div className="flex flex-wrap gap-2">
            {!known.includes(twin) && (
              <Button variant="tinted" onClick={() => setWizard({ open: true, preset: { domain: twin, redirect: true } })}>
                Redirect {twin} here
              </Button>
            )}
            <Button onClick={() => setWizard({ open: true })}>
              <PlusIcon data-icon="inline-start" />
              Add domain
            </Button>
          </div>
        )
      }
    >
      <BTable caption={`Domains of ${site.primary_domain}`} cols={["Domain", "", { label: <span className="sr-only">Actions</span> }]} rows={rows} />
      <FieldDescription className="mt-4">
        Point every domain's DNS here first. Redirects keep the path (301). Changing the primary domain rewrites the site's links (like www ↔ bare domain).
      </FieldDescription>
      <AddDomainDialog site={site} preset={wizard.preset} open={wizard.open} onOpenChange={(open) => setWizard((w) => ({ ...w, open }))} />
    </Section>
  )
}

// ---- TLS certificate ----

const daysUntil = (t: string) => Math.round((new Date(t).getTime() - Date.now()) / 864e5)

function Certificate({ site }: { site: Site }) {
  const s = useSession()
  const path = `/sites/${site.id}/certificate`
  const { data: cert, error, isPending, refetch } = useApi<SiteCert | null>(path)
  const [uploading, setUploading] = useState(false)

  const automatic = async () => {
    if (!(await ask("Remove the uploaded certificate? Caddy obtains one from Let's Encrypt again."))) return
    await api("DELETE", path)
    notify("Back to automatic certificates")
    await invalidate(path)
  }

  let body
  if (error) body = <LoadError error={error} retry={() => refetch()} className="shadow-none" />
  else if (isPending) body = <Skeleton className="h-24 rounded-xl" />
  else if (cert) {
    const days = daysUntil(cert.not_after)
    body = (
      <KeyValues
        items={[
          ["Certificate", <>Your own (uploaded): {cert.names.join(", ")}</>],
          ["Issuer", cert.issuer],
          [
            "Expires",
            <span className={days < 14 ? "text-danger" : undefined}>
              {fmtTime(cert.not_after)} ({days} days) — not renewed automatically
            </span>,
          ],
          ["Trusted by browsers", cert.trusted ? "yes" : "no (fine behind Cloudflare with an origin certificate)"],
        ]}
      />
    )
  } else body = <p className="text-sm">Certificates: automatic (Let's Encrypt), renewed by Caddy.</p>

  return (
    <Section
      icon={LockIcon}
      tint="green"
      title="TLS certificate"
      action={
        s.canChangeSite(site, "manager") &&
        !error &&
        !isPending && (
          <div className="flex flex-wrap gap-2">
            {cert && (
              <ActionButton run={automatic} variant="destructive">
                Use automatic certificates
              </ActionButton>
            )}
            <Button variant="tinted" onClick={() => setUploading(true)}>
              <UploadIcon data-icon="inline-start" />
              {cert ? "Replace the certificate" : "Use your own certificate instead"}
            </Button>
          </div>
        )
      }
    >
      {body}
      <FormDialog
        open={uploading}
        onOpenChange={setUploading}
        wide
        title={cert ? "Replace the certificate" : "Use your own certificate"}
        intro="It must cover every domain the site serves (not the redirects). PEM format."
        ok="Upload certificate"
        onSubmit={async (f) => {
          await api("PUT", path, { certificate: String(f.get("certificate") ?? ""), key: String(f.get("key") ?? "") })
          notify("Certificate uploaded")
          await invalidate(path)
        }}
      >
        <Field>
          <FieldLabel htmlFor={`cert-${site.id}`}>Certificate chain</FieldLabel>
          <Textarea
            id={`cert-${site.id}`}
            name="certificate"
            required
            spellCheck={false}
            className="max-h-60 min-h-28 font-mono text-xs"
            placeholder="-----BEGIN CERTIFICATE-----  (the certificate, then any intermediates)"
          />
        </Field>
        <Field>
          <FieldLabel htmlFor={`key-${site.id}`}>Private key</FieldLabel>
          <Textarea
            id={`key-${site.id}`}
            name="key"
            required
            spellCheck={false}
            autoComplete="off"
            className="max-h-60 min-h-28 font-mono text-xs"
            placeholder="-----BEGIN PRIVATE KEY-----"
          />
        </Field>
      </FormDialog>
    </Section>
  )
}
