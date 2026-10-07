import { useMemo, useState, type FormEvent } from "react"
import {
  ArchiveIcon, CloudIcon, HardDriveIcon, KeyRoundIcon, PlusIcon, RefreshCwIcon, SearchIcon, ServerIcon, Trash2Icon, UndoIcon, type LucideIcon,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, Banner, EmptyState, FormDialog, LoadError } from "@/components/app/blocks"
import { ask, askText } from "@/components/app/confirm"
import { IconTile } from "@/components/app/icon-tile"
import { Page, PageHeader, Section, StatusHero } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { StatusPill } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtAgo, fmtBytes, fmtNum, fmtTime, plural } from "@/lib/format"
import { startJob } from "@/lib/jobs"
import { invalidate, useApi, useSites } from "@/lib/query"
import { navigate } from "@/lib/router"
import { useSession } from "@/lib/session"

// The Backups page: the server's backup destinations (restic
// repositories), and every backup in one of them, deleted sites' included.

interface Repo {
  id: string
  name: string
  kind: "local" | "s3" | "b2" | "sftp" | string
  location: string
  public_key?: string
  host_key?: string
  host_key_fingerprint?: string
  created_at: string
  checked_at?: string
  check_error?: string
  pruned_at?: string
  sites_using: number
}

interface RepoBackup {
  id: string
  short_id: string
  repo_id: string
  site_id: string
  domain: string
  time: string
  kind: string
  size: number
  added: number
  files: number
}

const KINDS: Record<string, string> = {
  s3: "S3-compatible",
  b2: "Backblaze B2",
  sftp: "SFTP server",
  local: "Directory on this server",
}

const KIND_ICONS: Record<string, LucideIcon> = {
  s3: CloudIcon,
  b2: CloudIcon,
  sftp: ServerIcon,
  local: HardDriveIcon,
}

const REPOS = "/backups/repos"
const repoBackupsPath = (id: string) => `${REPOS}/${encodeURIComponent(id)}/backups`

export default function BackupsPage() {
  const s = useSession()
  const repos = useApi<Repo[]>(REPOS)
  const [adding, setAdding] = useState(false)
  const list = repos.data ?? []

  return (
    <Page>
      <PageHeader
        icon={ArchiveIcon}
        tint="orange"
        title="Backups"
        description="Encrypted copies of every site, kept on this server and off it. Any of them restores in one click."
        actions={
          s.isAdmin && (
            <Button onClick={() => setAdding(true)}>
              <PlusIcon data-icon="inline-start" />
              Add destination
            </Button>
          )
        }
      />

      {repos.isLoading && (
        <div className="flex flex-col gap-4">
          <Skeleton className="h-20 rounded-2xl" />
          <Skeleton className="h-56 rounded-2xl" />
          <Skeleton className="h-40 rounded-2xl" />
        </div>
      )}
      {repos.error && !repos.data && <LoadError error={repos.error} retry={() => repos.refetch()} />}

      {repos.data && (
        <>
          <Verdict repos={list} onAdd={s.isAdmin ? () => setAdding(true) : undefined} />
          <Destinations repos={list} />
          <Browse repos={list} />
        </>
      )}

      <AddDestination open={adding} onOpenChange={setAdding} />
    </Page>
  )
}

// ---- The verdict: can the destinations be reached, is anything off the server ----

function Verdict({ repos, onAdd }: { repos: Repo[]; onAdd?: () => void }) {
  const failing = repos.filter((r) => r.check_error)
  const offsite = repos.filter((r) => r.kind === "s3" || r.kind === "b2" || r.kind === "sftp")
  if (failing.length)
    return (
      <StatusHero
        kind="bad"
        title={failing.length === 1 ? `${failing[0].name} can't be reached` : `${failing.length} destinations can't be reached`}
        sub="Backups sent there fail until it answers again: the reason is under Destinations."
      />
    )
  if (!offsite.length)
    return (
      <Banner
        tone="warn"
        icon={CloudIcon}
        title="Backups are only on this server"
        actions={
          onAdd && (
            <Button variant="tinted" onClick={onAdd}>
              <PlusIcon data-icon="inline-start" />
              Add destination
            </Button>
          )
        }
      >
        They cover a broken site or a bad update, not losing the server. Add an off-server destination and keep its password somewhere else.
      </Banner>
    )
  return <StatusHero kind="ok" title="Backups are kept off this server too" sub={`${plural(offsite.length, "off-server destination")}, every one reachable.`} />
}

// ---- Destinations ----

function Destinations({ repos }: { repos: Repo[] }) {
  const s = useSession()

  async function check(r: Repo) {
    try {
      const v = await api<Repo>("POST", `${REPOS}/${encodeURIComponent(r.id)}/check`)
      if (v.check_error) showError(new Error(`${r.name}: ${v.check_error}`))
      else notify(`${r.name} checked: it answers and its backups are intact`)
    } finally {
      await invalidate(REPOS)
    }
  }

  async function password(r: Repo) {
    const p = await api<{ password: string }>("POST", `${REPOS}/${encodeURIComponent(r.id)}/password`)
    showSecret(`Password of ${r.name}`, [
      `Repository: ${r.location}`,
      `Password:   ${p.password}`,
      "",
      "Needed to restore these backups anywhere else (another server, or restic by hand).",
    ], "Keep it somewhere other than this server.")
  }

  async function remove(r: Repo) {
    if (!(await ask(`Forget the destination ${r.name}? Its backups stay where they are.`))) return
    await api("DELETE", `${REPOS}/${encodeURIComponent(r.id)}`)
    notify(`${r.name} removed`)
    await invalidate(REPOS)
  }

  return (
    <Section
      icon={CloudIcon}
      tint="orange"
      title="Destinations"
      description={
        <>
          Backups are restic snapshots: deduplicated (WordPress, themes and plugins are stored once for all sites) and encrypted with the destination's
          password. <strong className="font-semibold text-foreground">This server</strong> protects against a broken site or a bad update; add an off-server
          destination to survive losing the server, and keep its password somewhere else.
        </>
      }
    >
      {!repos.length && <p className="text-sm text-muted-foreground">No destinations yet.</p>}
      <ul className="flex flex-col divide-y divide-border/60" aria-label="Backup destinations">
        {repos.map((r) => (
          <li key={r.id} className="flex flex-wrap items-start gap-x-4 gap-y-3 py-3.5 first:pt-0 last:pb-0">
            <IconTile icon={KIND_ICONS[r.kind] ?? CloudIcon} tint={r.check_error ? "red" : r.kind === "local" ? "gray" : "orange"} size="lg" />
            <div className="flex min-w-0 flex-1 basis-64 flex-col gap-1">
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <strong className="font-semibold">{r.name}</strong>
                <Badge variant="secondary">{KINDS[r.kind] ?? r.kind}</Badge>
                <span className="text-xs text-muted-foreground">used by {plural(r.sites_using, "site")}</span>
              </div>
              <code className="text-xs break-all text-muted-foreground">{r.location}</code>
              {r.public_key && (
                <div className="mt-1 text-xs">
                  <span className="text-muted-foreground">Add to the SFTP server's authorized_keys: </span>
                  <code className="break-all">{r.public_key}</code>
                </div>
              )}
              {r.host_key_fingerprint && <span className="text-xs text-muted-foreground">Server key {r.host_key_fingerprint}</span>}
              <div className="mt-0.5 flex flex-wrap gap-x-3 text-xs">
                {r.check_error ? (
                  <span className="text-danger">Check failed: {r.check_error}</span>
                ) : r.checked_at ? (
                  <span className="text-success" title={fmtTime(r.checked_at)}>
                    Checked OK {fmtAgo(r.checked_at)}
                  </span>
                ) : (
                  <span className="text-muted-foreground">Not checked yet</span>
                )}
                {r.pruned_at && <span className="text-muted-foreground">pruned {fmtTime(r.pruned_at)}</span>}
              </div>
            </div>
            <div className="flex flex-wrap gap-1.5">
              {s.atLeast("operator") && (
                <ActionButton size="sm" run={() => check(r)}>
                  <RefreshCwIcon data-icon="inline-start" />
                  Check
                </ActionButton>
              )}
              {s.isAdmin && (
                <ActionButton size="sm" run={() => password(r)}>
                  <KeyRoundIcon data-icon="inline-start" />
                  Password
                </ActionButton>
              )}
              {s.isAdmin && r.id !== "local" && (
                <ActionButton size="sm" variant="destructive" run={() => remove(r)}>
                  Remove
                </ActionButton>
              )}
            </div>
          </li>
        ))}
      </ul>
    </Section>
  )
}

// ---- Add a destination ----

// The fields each kind of destination asks for.
const SHOWN: Record<string, string[]> = {
  s3: ["endpoint", "bucket", "prefix", "region", "key_id", "secret"],
  b2: ["bucket", "prefix", "key_id", "secret"],
  sftp: ["host", "port", "user", "path"],
  local: ["path"],
}

function AddDestination({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const [kind, setKind] = useState("s3")
  const [connecting, setConnecting] = useState(false)
  const shown = (n: string) => SHOWN[kind]?.includes(n)

  async function submit(data: FormData) {
    const val = (n: string) => String(data.get(n) ?? "").trim()
    setConnecting(true)
    let r: { repo: Repo; password?: string }
    try {
      r = await api("POST", REPOS, {
        name: val("name"),
        kind: val("kind"),
        endpoint: val("endpoint"),
        bucket: val("bucket"),
        prefix: val("prefix"),
        region: val("region"),
        key_id: val("key_id"),
        secret: String(data.get("secret") ?? ""),
        host: val("host"),
        port: Number(val("port") || 0),
        user: val("user"),
        path: val("path"),
        password: String(data.get("password") ?? ""),
      })
    } finally {
      setConnecting(false)
    }
    const lines: string[] = []
    if (r.password)
      lines.push("Repository password (keep it OFF this server: without it these backups can't be restored if the server is lost):", "", r.password)
    if (r.repo.public_key)
      lines.push(
        "",
        `Add this line to ~/.ssh/authorized_keys of ${val("user")} on ${val("host")}, then press Check:`,
        "",
        r.repo.public_key,
        "",
        `Server key pinned: ${r.repo.host_key_fingerprint ?? ""}`
      )
    if (lines.length) showSecret(`Destination ${r.repo.name} added`, lines)
    else notify(`Destination ${r.repo.name} added`)
    await invalidate(REPOS)
  }

  return (
    <FormDialog
      open={open}
      onOpenChange={(o) => {
        if (o) setKind("s3")
        onOpenChange(o)
      }}
      wide
      title="Add a destination"
      intro="WPGenie connects to it and creates an encrypted repository (or opens an existing one with its password)."
      ok={connecting ? "Connecting…" : "Add destination"}
      onSubmit={submit}
    >
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="repo-name">Name</FieldLabel>
          <Input id="repo-name" name="name" placeholder="Offsite (Backblaze)" required maxLength={60} autoComplete="off" />
        </Field>
        <Field>
          <FieldLabel htmlFor="repo-kind">Type</FieldLabel>
          <NativeSelect id="repo-kind" name="kind" className="w-full" value={kind} onChange={(e) => setKind(e.target.value)}>
            <NativeSelectOption value="s3">S3-compatible (AWS, Wasabi, R2, MinIO…)</NativeSelectOption>
            <NativeSelectOption value="b2">Backblaze B2</NativeSelectOption>
            <NativeSelectOption value="sftp">SFTP server</NativeSelectOption>
            <NativeSelectOption value="local">Directory on this server</NativeSelectOption>
          </NativeSelect>
        </Field>
        {shown("endpoint") && (
          <Field>
            <FieldLabel htmlFor="repo-endpoint">Endpoint</FieldLabel>
            <Input id="repo-endpoint" name="endpoint" placeholder="s3.eu-central-003.backblazeb2.com" autoComplete="off" />
            <FieldDescription>Empty: AWS.</FieldDescription>
          </Field>
        )}
        {shown("bucket") && (
          <Field>
            <FieldLabel htmlFor="repo-bucket">Bucket</FieldLabel>
            <Input id="repo-bucket" name="bucket" autoComplete="off" />
          </Field>
        )}
        {shown("prefix") && (
          <Field>
            <FieldLabel htmlFor="repo-prefix">Prefix</FieldLabel>
            <Input id="repo-prefix" name="prefix" placeholder="wpgenie" autoComplete="off" />
            <FieldDescription>Optional.</FieldDescription>
          </Field>
        )}
        {shown("region") && (
          <Field>
            <FieldLabel htmlFor="repo-region">Region</FieldLabel>
            <Input id="repo-region" name="region" placeholder="us-east-1" autoComplete="off" />
            <FieldDescription>Optional.</FieldDescription>
          </Field>
        )}
        {shown("key_id") && (
          <Field>
            <FieldLabel htmlFor="repo-key">Key ID</FieldLabel>
            <Input id="repo-key" name="key_id" autoComplete="off" />
          </Field>
        )}
        {shown("secret") && (
          <Field>
            <FieldLabel htmlFor="repo-secret">Secret key</FieldLabel>
            <Input id="repo-secret" name="secret" type="password" autoComplete="new-password" />
          </Field>
        )}
        {shown("host") && (
          <Field>
            <FieldLabel htmlFor="repo-host">Host</FieldLabel>
            <Input id="repo-host" name="host" placeholder="backup.example.net" autoComplete="off" />
          </Field>
        )}
        {shown("port") && (
          <Field>
            <FieldLabel htmlFor="repo-port">Port</FieldLabel>
            <Input id="repo-port" name="port" type="number" min={1} max={65535} placeholder="22" />
          </Field>
        )}
        {shown("user") && (
          <Field>
            <FieldLabel htmlFor="repo-user">User</FieldLabel>
            <Input id="repo-user" name="user" autoComplete="off" />
          </Field>
        )}
        {shown("path") && (
          <Field>
            <FieldLabel htmlFor="repo-path">Directory</FieldLabel>
            <Input id="repo-path" name="path" placeholder="/srv/backups/wpgenie" autoComplete="off" />
          </Field>
        )}
        <Field className="sm:col-span-2">
          <FieldLabel htmlFor="repo-password">Existing repository password</FieldLabel>
          <Input id="repo-password" name="password" type="password" autoComplete="new-password" />
          <FieldDescription>Only to attach backups made elsewhere. Empty: a new password is created and shown once.</FieldDescription>
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}

// ---- Browse a destination ----

function Browse({ repos }: { repos: Repo[] }) {
  const s = useSession()
  const { data: sites } = useSites()
  const [picked, setPicked] = useState("")
  const [browsing, setBrowsing] = useState<string | null>(null)
  const [filter, setFilter] = useState("")
  const repo = repos.some((r) => r.id === picked) ? picked : (repos[0]?.id ?? "")
  // A destination removed since it was listed isn't listed any more.
  const shownRepo = browsing && repos.some((r) => r.id === browsing) ? browsing : null
  const list = useApi<RepoBackup[]>(shownRepo ? repoBackupsPath(shownRepo) : null)
  const siteIDs = useMemo(() => new Set((sites ?? []).map((x) => x.id)), [sites])

  function submit(e: FormEvent) {
    e.preventDefault()
    if (!repo) return
    setFilter("")
    if (repo === browsing) list.refetch()
    else setBrowsing(repo)
  }

  async function restoreNew(repoID: string, b: RepoBackup) {
    const exists = siteIDs.has(b.site_id)
    const domain = await askText(`A new site from the backup of ${b.domain} (${fmtTime(b.time)}). Its DNS must point here.`, {
      title: "Restore as a new site",
      label: "Domain for the new site",
      value: exists ? "" : b.domain,
      placeholder: "example.com",
      ok: "Restore",
      danger: false,
    })
    if (!domain) return
    await startJob("POST", "/backups/restore-new", { repo_id: repoID, backup_id: b.id, domain })
    notify(`Restoring ${b.domain} as ${domain}: follow it in the activity tray`)
    navigate("/sites")
  }

  async function remove(repoID: string, b: RepoBackup) {
    if (!(await ask(`Delete the backup of ${b.domain} from ${fmtTime(b.time)}? This can't be undone.`))) return
    await api("DELETE", `${repoBackupsPath(repoID)}/${encodeURIComponent(b.id)}`)
    notify("Backup deleted")
    await invalidate(repoBackupsPath(repoID))
  }

  const q = filter.trim().toLowerCase()
  const rows = (list.data ?? []).filter((b) => !q || b.domain.toLowerCase().includes(q) || b.site_id.toLowerCase().includes(q))

  return (
    <Section
      icon={SearchIcon}
      tint="blue"
      title="Browse a destination"
      description="Every WPGenie backup in the destination, including deleted sites'. Any of them can become a new site on another domain."
    >
      <form onSubmit={submit} className="mb-4 flex flex-wrap items-center gap-2">
        <NativeSelect aria-label="Destination" value={repo} onChange={(e) => setPicked(e.target.value)} className="min-w-56 max-sm:w-full" disabled={!repos.length}>
          {repos.map((r) => (
            <NativeSelectOption key={r.id} value={r.id}>
              {r.name}
            </NativeSelectOption>
          ))}
        </NativeSelect>
        <Button type="submit" variant="tinted" disabled={!repo || list.isFetching}>
          {list.isFetching ? "Listing…" : "List backups"}
        </Button>
        {shownRepo && !!list.data?.length && (
          <InputGroup className="ml-auto w-56 max-sm:ml-0 max-sm:w-full">
            <InputGroupAddon>
              <SearchIcon />
            </InputGroupAddon>
            <InputGroupInput type="search" placeholder="Filter by domain" aria-label="Filter backups by domain" value={filter} onChange={(e) => setFilter(e.target.value)} />
          </InputGroup>
        )}
      </form>

      {shownRepo && list.isLoading && (
        <div className="flex flex-col gap-2" aria-busy>
          <p className="text-sm text-muted-foreground">Listing…</p>
          <Skeleton className="h-10" />
          <Skeleton className="h-10" />
        </div>
      )}
      {shownRepo && list.error && <LoadError error={list.error} retry={() => list.refetch()} />}
      {shownRepo && list.data && (
        <>
          {!list.data.length ? (
            <EmptyState icon={ArchiveIcon} tint="orange" title="No backups here yet" className="shadow-none">
              Backups appear here once a site backs up to this destination.
            </EmptyState>
          ) : (
            <>
              <p className="mb-2 text-xs text-muted-foreground">
                {plural(list.data.length, "backup")} in {repos.find((r) => r.id === shownRepo)?.name ?? shownRepo}
                {q ? `, ${fmtNum(rows.length)} shown` : ""}
              </p>
              <BTable
                caption="Backups in the destination"
                cols={["Time", "Site", "Kind", { label: "Size", num: true }, ""]}
                empty={<p className="py-2 text-sm text-muted-foreground">No backups match the filter.</p>}
                rows={rows.map((b) => ({
                  key: b.id,
                  cells: [
                    fmtTime(b.time),
                    <div className="flex flex-col">
                      <span className="font-medium">{b.domain}</span>
                      <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
                        <span className="font-mono">{b.site_id}</span>
                        {!siteIDs.has(b.site_id) && <Badge variant="secondary">deleted</Badge>}
                      </span>
                    </div>,
                    <StatusPill status={b.kind || "manual"} />,
                    fmtBytes(b.size),
                    s.isAdmin ? (
                      <div className="flex justify-end gap-1.5">
                        <ActionButton size="sm" run={() => restoreNew(shownRepo, b)} title="Restore as a new site on another domain">
                          <UndoIcon data-icon="inline-start" />
                          Restore as new site
                        </ActionButton>
                        <ActionButton size="sm" variant="destructive" run={() => remove(shownRepo, b)}>
                          <Trash2Icon data-icon="inline-start" />
                          Delete
                        </ActionButton>
                      </div>
                    ) : null,
                  ],
                }))}
              />
            </>
          )}
        </>
      )}
      {!shownRepo && <p className="text-sm text-muted-foreground">Choose a destination and list its backups.</p>}
    </Section>
  )
}
