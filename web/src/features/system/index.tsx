import { useRef, useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { ArrowUpCircleIcon, ExternalLinkIcon, ImageIcon, PaletteIcon, RefreshCwIcon, SlidersHorizontalIcon, Trash2Icon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Banner, LoadError } from "@/components/app/blocks"
import { KeyValues } from "@/components/app/data-table"
import { ask } from "@/components/app/confirm"
import { Page, PageHeader, Section, StatusHero } from "@/components/app/page"
import { StatusText } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtAgo, fmtBytes, fmtTime, humanize } from "@/lib/format"
import { queryClient } from "@/lib/query"
import { useSession } from "@/lib/session"

// The System page: WPGenie's version and self-update, and (for
// administrators) the branding every site's WordPress admin shows.

interface Release {
  version: string
  notes: string
  url: string
  published_at: string
}

interface UpdateStatus {
  // staged | applying | restarting | done | rolled_back | failed
  phase: string
  from: string
  to: string
  message: string
  at: string
}

interface SystemInfo {
  current: string
  latest: Release | null
  available: boolean
  checked_at?: string
  check_error?: string
  signing_key: boolean
  status: UpdateStatus | null
}

interface Branding {
  name: string
  url: string
  enabled: boolean
  has_logo: boolean
  logo_type?: string
  logo_version?: string
}

// The same address the navigation's dot reads: one request feeds both.
const SYSTEM = "/system"
const BRANDING = "/settings/branding"
const RUNNING = ["staged", "applying", "restarting"]
const updating = (i: SystemInfo | undefined) => !!i?.status && RUNNING.includes(i.status.phase)
// recent: within the last day (an old failed update isn't news).
const recent = (t: string) => Date.now() - new Date(t).getTime() < 24 * 36e5
// Release data isn't signed: only ever link to GitHub.
const safeURL = (u: unknown) => (typeof u === "string" && u.startsWith("https://github.com/") ? u : undefined)

export default function SystemPage() {
  const s = useSession()
  return (
    <Page>
      <PageHeader icon={SlidersHorizontalIcon} tint="gray" title="System" description="WPGenie’s version, updates and your branding." />
      <Updates />
      {s.isAdmin && <BrandingCard />}
    </Page>
  )
}

// ---- WPGenie and its updates ----

function Updates() {
  const s = useSession()
  // While an update runs the daemon restarts: keep asking through the gap.
  const q = useQuery<SystemInfo>({
    queryKey: [SYSTEM],
    queryFn: () => api<SystemInfo>("GET", SYSTEM),
    refetchInterval: (x) => (updating(x.state.data) ? 3000 : false),
  })
  const [checking, setChecking] = useState(false)
  const [starting, setStarting] = useState(false)
  // The version this page came with: after an update, the new panel needs a reload.
  const [loadedWith] = useState(() => queryClient.getQueryData<SystemInfo>([SYSTEM])?.current)

  if (q.isLoading) return <Skeleton className="h-64 rounded-2xl" />
  if (q.error && !q.data) return <LoadError error={q.error} retry={() => q.refetch()} />
  const info = q.data
  if (!info) return null

  const st = info.status
  const active = updating(info)
  const latest = info.latest

  async function check() {
    setChecking(true)
    try {
      const fresh = await api<SystemInfo>("POST", "/system/update/check")
      queryClient.setQueryData([SYSTEM], fresh)
      if (fresh.check_error) showError(new Error(`Checking for updates failed: ${fresh.check_error}`))
      else notify(fresh.available && fresh.latest ? `WPGenie ${fresh.latest.version} is available` : "WPGenie is up to date")
    } catch (e) {
      showError(e)
    } finally {
      setChecking(false)
    }
  }

  async function update() {
    if (!latest) return
    if (
      !(await ask(
        `Update WPGenie to ${latest.version}? The panel restarts; sites keep serving. If the new version does not start, the previous one is restored automatically.`
      ))
    )
      return
    setStarting(true)
    try {
      await api("POST", "/system/update")
      notify(`Updating to ${latest.version}: the panel restarts in a moment`)
      await q.refetch()
    } catch (e) {
      showError(e)
    } finally {
      setStarting(false)
    }
  }

  const releaseURL = safeURL(latest?.url)

  return (
    <>
      {active && st ? (
        <Banner tone="info" icon={RefreshCwIcon} title={`Updating WPGenie to ${st.to}`}>
          {st.message || humanize(st.phase)}. Sites keep serving; this page follows along while the panel restarts.
        </Banner>
      ) : loadedWith && loadedWith !== info.current ? (
        <Banner
          tone="ok"
          icon={ArrowUpCircleIcon}
          title={`WPGenie ${info.current} is running`}
          actions={
            <Button variant="tinted" onClick={() => location.reload()}>
              Reload the page
            </Button>
          }
        >
          Reload to use the new version of the panel.
        </Banner>
      ) : st && (st.phase === "failed" || st.phase === "rolled_back") && st.to !== info.current && recent(st.at) ? (
        <StatusHero kind="bad" title={`The update to ${st.to} didn't go through`} sub={`${st.message} (${fmtTime(st.at)})`} />
      ) : info.available && latest ? (
        <Banner tone="info" icon={ArrowUpCircleIcon} title={`WPGenie ${latest.version} is available`}>
          You're running {info.current}.{!info.signing_key && " This build can't update itself."}
        </Banner>
      ) : (
        <StatusHero
          kind="ok"
          title="WPGenie is up to date"
          sub={`Running ${info.current}${info.checked_at ? `, checked ${fmtAgo(info.checked_at)}` : ""}`}
        />
      )}

      <Section
        icon={SlidersHorizontalIcon}
        tint="gray"
        title="WPGenie"
        action={
          <div className="flex flex-wrap gap-2">
            {s.atLeast("operator") && (
              <Button variant="tinted" size="sm" onClick={check} disabled={checking}>
                <RefreshCwIcon data-icon="inline-start" className={checking ? "animate-spin" : undefined} />
                {checking ? "Checking…" : "Check now"}
              </Button>
            )}
            {s.isAdmin && (
              <Button size="sm" onClick={update} disabled={!info.available || !info.signing_key || active || starting}>
                <ArrowUpCircleIcon data-icon="inline-start" />
                {active || starting ? "Updating…" : info.available && latest ? `Update to ${latest.version}` : "Up to date"}
              </Button>
            )}
          </div>
        }
      >
        <KeyValues
          items={[
            ["Running", <span className="font-mono">{info.current}</span>],
            !!latest && [
              "Latest release",
              releaseURL ? (
                <a href={releaseURL} target="_blank" rel="noopener" className="inline-flex items-center gap-1 font-mono text-link">
                  {latest.version}
                  <ExternalLinkIcon className="size-3.5" />
                </a>
              ) : (
                <span className="font-mono">{latest.version}</span>
              ),
            ],
            !!info.checked_at && ["Checked", <span title={fmtTime(info.checked_at)}>{fmtAgo(info.checked_at)}</span>],
            !!info.check_error && ["Last check", <span className="text-danger">{info.check_error}</span>],
            !!st && [
              "Last update",
              <span>
                <StatusText status={st.phase} /> {st.from} → {st.to}: {st.message} ({fmtTime(st.at)})
              </span>,
            ],
          ]}
        />
        {!info.signing_key && <p className="mt-3 text-sm text-muted-foreground">This build has no release signing key, so it cannot update itself.</p>}
        {latest && info.available && (
          <Collapsible className="mt-4">
            <CollapsibleTrigger render={<Button variant="link" className="h-auto px-0" />}>Release notes</CollapsibleTrigger>
            <CollapsibleContent>
              <pre className="mt-2 max-h-96 overflow-auto rounded-xl bg-muted p-3 text-xs whitespace-pre-wrap">{latest.notes}</pre>
            </CollapsibleContent>
          </Collapsible>
        )}
      </Section>
    </>
  )
}

// ---- Branding (administrators) ----

function BrandingCard() {
  const q = useQuery<Branding>({
    queryKey: [BRANDING],
    queryFn: () => api<Branding>("GET", BRANDING),
    refetchOnWindowFocus: false,
  })
  return (
    <Section
      icon={PaletteIcon}
      tint="purple"
      title="Branding"
      description="Your brand instead of WordPress's in every site's admin: the logo and link on the login page, the logo menu in the admin bar, the admin footer and page titles. WordPress's news widget and welcome panel go too. Leave the name empty and remove the logo to show WordPress's own again."
    >
      {q.isLoading && <Skeleton className="h-40 rounded-xl" />}
      {q.error && !q.data && <LoadError error={q.error} retry={() => q.refetch()} />}
      {/* Fresh fields whenever the saved brand changes. */}
      {q.data && <BrandingForm key={JSON.stringify(q.data)} b={q.data} />}
    </Section>
  )
}

const MAX_LOGO = 256 * 1024

function BrandingForm({ b }: { b: Branding }) {
  const [name, setName] = useState(b.name)
  const [url, setURL] = useState(b.url)
  // undefined: unchanged; "": remove it; a data: URI: the new one.
  const [logo, setLogo] = useState<string | undefined>(undefined)
  const [picked, setPicked] = useState<{ name: string; size: number } | null>(null)
  const [busy, setBusy] = useState(false)
  const file = useRef<HTMLInputElement>(null)

  function pick(f: File | undefined) {
    if (!f) return
    if (f.size > MAX_LOGO) {
      showError(new Error("The logo must be 256 KB or smaller"))
      if (file.current) file.current.value = ""
      return
    }
    const r = new FileReader()
    // The page's CSP keeps it from showing a local file: it's previewed once saved.
    r.onload = () => {
      setLogo(String(r.result))
      setPicked({ name: f.name, size: f.size })
    }
    r.onerror = () => showError(new Error("That file couldn't be read"))
    r.readAsDataURL(f)
  }

  function removeLogo() {
    setLogo("")
    setPicked(null)
    if (file.current) file.current.value = ""
  }

  async function save(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      const body: { name: string; url: string; logo?: string } = { name: name.trim(), url: url.trim() }
      if (logo !== undefined) body.logo = logo
      const v = await api<Branding>("PUT", BRANDING, body)
      notify(v.enabled ? "Branding saved: every site's WordPress admin shows it now" : "Branding removed: sites show WordPress's own again")
      queryClient.setQueryData([BRANDING], v)
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={save} className="flex flex-col gap-5">
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="brand-name">Brand name</FieldLabel>
          <Input id="brand-name" value={name} onChange={(e) => setName(e.target.value)} maxLength={80} placeholder="Acme Hosting" autoComplete="off" />
        </Field>
        <Field>
          <FieldLabel htmlFor="brand-url">Link</FieldLabel>
          <Input id="brand-url" type="url" value={url} onChange={(e) => setURL(e.target.value)} placeholder="https://acme.example" autoComplete="off" />
          <FieldDescription>Where the logo points; empty: the site itself.</FieldDescription>
        </Field>
        <Field className="sm:col-span-2">
          <FieldLabel htmlFor="brand-logo">Logo</FieldLabel>
          <Input
            id="brand-logo"
            ref={file}
            type="file"
            accept="image/png,image/jpeg,image/gif,image/webp,image/svg+xml"
            onChange={(e) => pick(e.target.files?.[0])}
          />
          <FieldDescription>PNG, JPEG, WebP, GIF or SVG, up to 256 KB; wide logos look best.</FieldDescription>
        </Field>
      </FieldGroup>

      <div className="flex min-h-20 items-center gap-4 rounded-xl bg-muted/60 p-4" aria-live="polite">
        {logo === "" ? (
          <span className="text-sm text-muted-foreground">The logo is removed when you save.</span>
        ) : picked ? (
          <span className="flex items-center gap-2 text-sm">
            <ImageIcon className="size-4 text-muted-foreground" />
            New logo: {picked.name} ({fmtBytes(picked.size)}), shown once saved
          </span>
        ) : b.has_logo ? (
          <img src={`/api/v1/settings/branding/logo?v=${encodeURIComponent(b.logo_version ?? "")}`} alt="Current logo" className="max-h-16 max-w-72 object-contain" />
        ) : (
          <span className="text-sm text-muted-foreground">No logo: WordPress shows the name instead.</span>
        )}
      </div>

      <div className="flex flex-wrap justify-end gap-2">
        <Button type="button" variant="destructive" onClick={removeLogo} disabled={logo === "" || (!b.has_logo && !picked)}>
          <Trash2Icon data-icon="inline-start" />
          Remove logo
        </Button>
        <Button type="submit" disabled={busy}>
          Save branding
        </Button>
      </div>
    </form>
  )
}
