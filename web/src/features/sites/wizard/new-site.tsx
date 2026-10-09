import { useEffect, useId, useRef, useState } from "react"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldContent, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { useDivi } from "@/lib/divi"
import { followJob } from "@/lib/jobs"
import { invalidate, useClustered, useNodes } from "@/lib/query"
import { navigate, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { showCredentials } from "./credentials"
import { cleanDomain, dnsReady, dnsStep, DnsPanel, useDnsCheck, validDomain, type DNSCheck } from "./dns"
import { Wizard } from "./wizard"

// Creating a WordPress site: its domain (and, on a cluster, the server),
// the DNS check, then its details. Provisioning runs as a job (progress in
// the jobs tray); the admin credentials come with it when it's done.

export function NewSiteWizard({ open, onClose }: { open: boolean; onClose: () => void }) {
  const s = useSession()
  const clustered = useClustered()
  const { data: nodes } = useNodes()
  const pickNode = s.isAdmin && clustered
  const ids = useId()

  const [domain, setDomain] = useState("")
  const [node, setNode] = useState("")
  const [step, setStep] = useState(0)
  const [name, setName] = useState("")
  const [email, setEmail] = useState("")
  const [user, setUser] = useState("")
  // null: the license's default for new sites.
  const [divi, setDivi] = useState<boolean | null>(null)
  const license = useDivi().data
  const offerDivi = !!license?.configured
  const withDivi = divi ?? !!license?.new_sites
  const emailRef = useRef<HTMLInputElement>(null)

  // A fresh form each time it opens.
  const [wasOpen, setWasOpen] = useState(open)
  if (open !== wasOpen) {
    setWasOpen(open)
    if (open) {
      setDomain("")
      setNode("")
      setStep(0)
      setName("")
      setEmail("")
      setUser("")
      setDivi(null)
    }
  }

  const target = pickNode ? node : ""
  const check = (d: string) =>
    api<DNSCheck>("GET", `/dns-check?domain=${encodeURIComponent(d)}${target ? "&node=" + encodeURIComponent(target) : ""}`)

  const domainStep = {
    label: "Domain",
    content: (
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor={ids + "domain"}>Domain</FieldLabel>
          <Input
            id={ids + "domain"}
            name="domain"
            placeholder="example.com"
            autoComplete="off"
            spellCheck={false}
            required
            value={domain}
            onChange={(e) => setDomain(e.target.value)}
          />
          <FieldDescription>The site's address. Next, WPGenie checks its DNS points to this server, so the HTTPS certificate can be issued.</FieldDescription>
        </Field>
        {pickNode && (
          <Field>
            <FieldLabel htmlFor={ids + "node"}>Server</FieldLabel>
            <NativeSelect id={ids + "node"} className="w-full" value={node} onChange={(e) => setNode(e.target.value)}>
              <NativeSelectOption value="">Automatic (the server with the most room)</NativeSelectOption>
              {(nodes ?? [])
                .filter((n) => n.status === "active" && n.up)
                .map((n) => (
                  <NativeSelectOption key={n.id} value={n.id}>
                    {n.name} ({n.id})
                  </NativeSelectOption>
                ))}
            </NativeSelect>
          </Field>
        )}
      </FieldGroup>
    ),
    next() {
      const d = cleanDomain(domain)
      if (!validDomain(d)) throw new Error("Enter a domain like example.com")
      setDomain(d)
      return true
    },
  }

  // Checked while its step is shown (again each time it's entered).
  const dns = useDnsCheck(check, cleanDomain(domain), open && step === 1)
  const checked = dns.checked === dns.domain ? dns.result : null
  const dnsContent = (
    <DnsPanel dns={dns} many={pickNode}>
      <WwwTwin domain={dns.domain} result={checked} check={check} />
    </DnsPanel>
  )

  const detailsStep = {
    label: "Details",
    content: (
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor={ids + "name"}>Site title</FieldLabel>
          <Input id={ids + "name"} placeholder="My Blog" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field>
          <FieldLabel htmlFor={ids + "email"}>Admin e-mail</FieldLabel>
          <Input
            id={ids + "email"}
            ref={emailRef}
            type="email"
            required
            autoComplete="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </Field>
        <Field>
          <FieldLabel htmlFor={ids + "user"}>
            Admin username <span className="font-normal text-muted-foreground">(optional)</span>
          </FieldLabel>
          <Input id={ids + "user"} placeholder="random if empty" autoComplete="off" value={user} onChange={(e) => setUser(e.target.value)} />
        </Field>
        {offerDivi && (
          <Field orientation="horizontal">
            <Checkbox id={ids + "divi"} checked={withDivi} onCheckedChange={(v) => setDivi(!!v)} aria-describedby={ids + "divi-help"} />
            <FieldContent>
              <FieldLabel htmlFor={ids + "divi"}>Install Divi</FieldLabel>
              <FieldDescription id={ids + "divi-help"}>The Divi theme, switched on and licensed for updates and premade layouts.</FieldDescription>
            </FieldContent>
          </Field>
        )}
      </FieldGroup>
    ),
    button: { label: "Create site" },
    async next() {
      if (!email.trim() || !emailRef.current?.checkValidity()) {
        emailRef.current?.focus()
        throw new Error("Enter the admin's e-mail address.")
      }
      const body: Record<string, string | boolean> = { domain, name: name.trim(), admin_email: email.trim(), admin_user: user.trim() }
      // Said either way once offered: what the person saw is what they get.
      if (offerDivi) body.divi = withDivi
      // Automatic placement: the site goes where the domain points.
      if (pickNode) body.node = node || checked?.server || ""
      const res = await api<{ site: Site; job_id: string | number }>("POST", "/sites", body)
      const jobId = String(res.job_id)
      notify(`Creating ${res.site.primary_domain}: progress is in the jobs panel`)
      followJob(jobId, async (v) => {
        if (v.secret) showCredentials(res.site.primary_domain, v.secret, jobId)
        else if (v.job.status === "failed") showError(new Error(`Creating ${res.site.primary_domain} failed: ${v.job.error}`))
        await invalidate("/sites")
      })
      // The workspace needs the site in the list before it opens.
      await invalidate("/sites")
      navigate(sitePath(res.site.id))
      return true
    },
  }

  return (
    <Wizard
      open={open}
      title="Create a WordPress site"
      step={step}
      onStep={setStep}
      onClose={onClose}
      steps={[domainStep, dnsStep(dns, dnsContent), detailsStep]}
    />
  )
}

// The www twin, for information: it can redirect here once the site exists.
function WwwTwin({ domain, result, check }: { domain: string; result: DNSCheck | null; check: (d: string) => Promise<DNSCheck> }) {
  const [note, setNote] = useState<{ domain: string; text: string } | null>(null)
  const checkRef = useRef(check)
  useEffect(() => {
    checkRef.current = check
  })
  const checked = !!result
  useEffect(() => {
    if (!checked || domain.startsWith("www.") || domain.split(".").length > 2) return
    let cancelled = false
    checkRef.current("www." + domain).then(
      (t) => {
        if (cancelled) return
        setNote({
          domain,
          text: dnsReady(t)
            ? `www.${domain} points here too: add it under Domains once the site is live to redirect it.`
            : `Tip: www.${domain} isn't set up. To have it redirect here later, point it here too (a CNAME to ${domain} works).`,
        })
      },
      () => {
        /* only a hint */
      }
    )
    return () => {
      cancelled = true
    }
  }, [checked, domain])
  if (!note || note.domain !== domain) return null
  return <p className="text-sm text-muted-foreground">{note.text}</p>
}
