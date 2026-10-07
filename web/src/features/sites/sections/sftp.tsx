import { useState } from "react"
import { DatabaseIcon, ExternalLinkIcon, KeyRoundIcon, PlusIcon, ServerIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { InputGroup, InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { ActionButton, BTable, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { KeyValues } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { StatusPill } from "@/components/app/status"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { plural } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "@/features/sites/sections"
import type { SFTPInfo, SFTPUser } from "./data/types"

// SFTP & database: logins that see only this site's files (passwords
// and/or SSH keys), and phpMyAdmin with a temporary database account.

export default function SftpSection({ site }: SectionProps) {
  const path = `/sites/${site.id}/sftp`
  const { data: info, error, refetch } = useApi<SFTPInfo>(path)
  if (error) return <LoadError error={error} retry={() => refetch()} />
  if (!info) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-72 rounded-2xl" />
        <Skeleton className="h-40 rounded-2xl" />
      </div>
    )
  }
  return (
    <div className="flex flex-col">
      <Sftp site={site} info={info} />
      <Database site={site} info={info} />
    </div>
  )
}

// One key per line; the legacy panel separated them with ";".
const splitKeys = (v: string) =>
  v
    .split(/[;\n]/)
    .map((x) => x.trim())
    .filter(Boolean)

// ---- SFTP ----

function Sftp({ site, info }: { site: Site; info: SFTPInfo }) {
  const s = useSession()
  const path = `/sites/${site.id}/sftp`
  const [adding, setAdding] = useState(false)
  // Kept while the dialog closes, so its title doesn't change.
  const [keys, setKeys] = useState<{ open: boolean; user?: SFTPUser }>({ open: false })
  const keysOf = keys.user
  const refresh = () => invalidate(path)

  const newPassword = async (u: SFTPUser) => {
    const r = await api<{ password: string }>("PUT", `${path}/${u.username}/password`, { enabled: true })
    showSecret(`SFTP password for ${u.username}`, [`Login:    ${u.username}`, `Password: ${r.password}`])
    await refresh()
  }
  const keysOnly = async (u: SFTPUser) => {
    await api("PUT", `${path}/${u.username}/password`, { enabled: false })
    notify(`${u.username} signs in with keys only`)
    await refresh()
  }
  const del = async (u: SFTPUser) => {
    if (!(await ask(`Delete the SFTP login ${u.username}? Its open sessions end now.`))) return
    await api("DELETE", `${path}/${u.username}`)
    notify(`${u.username} deleted`)
    await refresh()
  }

  return (
    <Section
      icon={ServerIcon}
      tint="graphite"
      title="SFTP"
      description="Logins only see this site's directory (the WordPress install in public/) and can't run commands. Files they upload belong to the site, like WordPress's own."
      action={
        s.canChange && (
          <Button onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add login
          </Button>
        )
      }
    >
      <div className="flex flex-col gap-5">
        <KeyValues
          items={[
            [
              "Host",
              <>
                <code>{info.host}</code>
                <span className="text-muted-foreground"> (or this server's IP; not through Cloudflare's proxy)</span>
              </>,
            ],
            ["Port", <code>{info.server.port}</code>],
            info.server.host_keys.length > 0 && [
              "Server key",
              <span className="flex flex-col gap-0.5">
                {info.server.host_keys.map((k) => (
                  <code key={k} className="text-xs [overflow-wrap:anywhere]">
                    {k}
                  </code>
                ))}
              </span>,
            ],
            !info.server.running && ["Status", <StatusPill status="stopped" tone="warn">Not running</StatusPill>],
          ]}
        />
        <BTable
          caption={`SFTP logins of ${site.primary_domain}`}
          cols={["Login", "Password", { label: "Keys", num: true }, { label: <span className="sr-only">Actions</span> }]}
          rows={info.users.map((u) => ({
            key: u.username,
            cells: [
              <code>{u.username}</code>,
              u.password ? "yes" : "no",
              String(u.public_keys.length),
              s.canChange && (
                <div className="flex flex-wrap justify-end gap-1.5">
                  <ActionButton run={() => newPassword(u)} size="sm">
                    {u.password ? "New password" : "Add password"}
                  </ActionButton>
                  {u.password && u.public_keys.length > 0 && (
                    <ActionButton run={() => keysOnly(u)} size="sm" variant="ghost">
                      Keys only
                    </ActionButton>
                  )}
                  <Button variant="ghost" size="sm" onClick={() => setKeys({ open: true, user: u })}>
                    <KeyRoundIcon data-icon="inline-start" />
                    Keys
                  </Button>
                  <ActionButton run={() => del(u)} size="sm" variant="destructive">
                    Delete
                  </ActionButton>
                </div>
              ),
            ],
          }))}
          empty={<p className="py-2 text-sm text-muted-foreground">No SFTP logins yet.</p>}
        />
      </div>

      <AddLogin site={site} info={info} open={adding} onOpenChange={setAdding} />
      <FormDialog
        open={keys.open}
        onOpenChange={(open) => setKeys((k) => ({ ...k, open }))}
        wide
        title={`SFTP keys for ${keysOf?.username ?? ""}`}
        intro="One authorized_keys line per key. Empty: no keys (the login then needs a password)."
        ok="Save keys"
        onSubmit={async (f) => {
          await api("PUT", `${path}/${keysOf!.username}/keys`, { public_keys: splitKeys(String(f.get("keys") ?? "")) })
          notify(`Keys of ${keysOf!.username} saved`)
          await refresh()
        }}
      >
        <Field>
          <FieldLabel htmlFor={`sftp-keys-${site.id}`}>Public keys</FieldLabel>
          <Textarea
            id={`sftp-keys-${site.id}`}
            name="keys"
            spellCheck={false}
            className="max-h-72 min-h-28 font-mono text-xs"
            defaultValue={keysOf?.public_keys.join("\n")}
            placeholder="ssh-ed25519 AAAA… you@laptop"
          />
        </Field>
      </FormDialog>
    </Section>
  )
}

function AddLogin({ site, info, open, onOpenChange }: { site: Site; info: SFTPInfo; open: boolean; onOpenChange: (o: boolean) => void }) {
  const [withPw, setWithPw] = useState(true)
  return (
    <FormDialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o)
        if (!o) setWithPw(true)
      }}
      wide
      title="Add an SFTP login"
      intro="Logins only see this site's directory and can't run commands."
      ok="Add login"
      onSubmit={async (f) => {
        const r = await api<{ user: SFTPUser; password?: string }>("POST", `/sites/${site.id}/sftp`, {
          suffix: String(f.get("suffix") ?? "").trim(),
          password: withPw,
          public_keys: [String(f.get("keys") ?? "")],
        })
        if (r.password) {
          showSecret(`SFTP login ${r.user.username}`, [
            `Host:     ${info.host}`,
            `Port:     ${info.server.port}`,
            `Login:    ${r.user.username}`,
            `Password: ${r.password}`,
          ])
        } else notify(`SFTP login ${r.user.username} added`)
        await invalidate(`/sites/${site.id}/sftp`)
      }}
    >
      <Field>
        <FieldLabel htmlFor={`sftp-suffix-${site.id}`}>Login name</FieldLabel>
        <InputGroup>
          <InputGroupAddon>
            <InputGroupText className="font-mono">{site.id}-</InputGroupText>
          </InputGroupAddon>
          <InputGroupInput id={`sftp-suffix-${site.id}`} name="suffix" autoComplete="off" spellCheck={false} placeholder="optional, e.g. dev" />
        </InputGroup>
        <FieldDescription>Up to 16 lowercase letters or digits. Empty: the login is {site.id}.</FieldDescription>
      </Field>
      <Label className="w-fit font-normal">
        <Checkbox checked={withPw} onCheckedChange={(c) => setWithPw(!!c)} />
        Generate a password
      </Label>
      <Field>
        <FieldLabel htmlFor={`sftp-newkeys-${site.id}`}>Public keys</FieldLabel>
        <Textarea
          id={`sftp-newkeys-${site.id}`}
          name="keys"
          spellCheck={false}
          className="max-h-60 min-h-24 font-mono text-xs"
          placeholder="ssh-ed25519 AAAA… you@laptop  (optional)"
        />
        <FieldDescription>One per line.</FieldDescription>
      </Field>
    </FormDialog>
  )
}

// ---- Database: phpMyAdmin ----

function Database({ site, info }: { site: Site; info: SFTPInfo }) {
  const s = useSession()
  const [busy, setBusy] = useState(false)

  const open = async () => {
    // Opened now, in the click: a window opened after the request would be blocked as a pop-up.
    const win = window.open("about:blank", "_blank")
    setBusy(true)
    try {
      const r = await api<{ url: string; expires_at: string }>("POST", `/sites/${site.id}/phpmyadmin`)
      if (win) {
        win.opener = null
        win.location.href = r.url
      } else showSecret("phpMyAdmin link (single use, 2 minutes)", [r.url])
      await invalidate(`/sites/${site.id}/sftp`)
    } catch (e) {
      win?.close()
      showError(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={DatabaseIcon}
      tint="graphite"
      title="Database"
      description={
        "phpMyAdmin opens on the site's own domain with a temporary database account for this site only. " +
        "The link works once, within 2 minutes; the session ends after 15 minutes idle or an hour."
      }
      action={
        s.canChange && (
          <Button onClick={open} disabled={busy}>
            <ExternalLinkIcon data-icon="inline-start" />
            Open phpMyAdmin
          </Button>
        )
      }
    >
      <KeyValues
        items={[
          site.db_name ? ["Database", <code>{site.db_name}</code>] : false,
          ["Open sessions", info.phpmyadmin_sessions ? plural(info.phpmyadmin_sessions, "session") : "None"],
        ]}
      />
    </Section>
  )
}
