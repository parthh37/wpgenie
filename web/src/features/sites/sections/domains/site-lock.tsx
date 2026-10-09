import { useId, useState } from "react"
import { LockKeyholeIcon, SparklesIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput } from "@/components/ui/input-group"
import { Textarea } from "@/components/ui/textarea"
import { ActionButton, FormDialog } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { KeyValues } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { invalidate } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"

// Password protection (the site lock): every visitor gets the browser's
// own username-and-password prompt before seeing anything of the site, for
// a site that isn't ready to be seen. The panel never gets the password
// back: it shows the username, and the password once, right after it was
// set, to pass on.

export const MIN_LOCK_PASSWORD = 8
const MAX_LOCK_PASSWORD = 72

// Letters and digits that can't be mistaken for one another (no 0/O, 1/l/I).
const ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// suggestPassword is a random password of n characters, without bias (bytes
// past the last whole multiple of the alphabet are drawn again).
export function suggestPassword(n = 16): string {
  const limit = 256 - (256 % ALPHABET.length)
  let out = ""
  while (out.length < n) {
    for (const x of crypto.getRandomValues(new Uint8Array(n * 2))) {
      if (x < limit && out.length < n) out += ALPHABET[x % ALPHABET.length]
    }
  }
  return out
}

// LockFields: the username and password of a lock (also offered when a
// staging copy is made). keep: a password is set already, so leaving the
// field empty keeps it.
export function LockFields({
  user,
  password,
  onUser,
  onPassword,
  keep,
  disabled,
}: {
  user: string
  password: string
  onUser: (v: string) => void
  onPassword: (v: string) => void
  keep?: boolean
  disabled?: boolean
}) {
  const id = useId()
  return (
    <>
      <Field>
        <FieldLabel htmlFor={`${id}-user`}>Username</FieldLabel>
        <Input
          id={`${id}-user`}
          name="username"
          required
          maxLength={64}
          pattern="[A-Za-z0-9][A-Za-z0-9._@\-]*"
          title="Letters, digits and . _ @ -, starting with a letter or digit"
          autoComplete="off"
          spellCheck={false}
          value={user}
          disabled={disabled}
          onChange={(e) => onUser(e.target.value)}
        />
      </Field>
      <Field>
        <FieldLabel htmlFor={`${id}-pass`}>Password</FieldLabel>
        <InputGroup>
          <InputGroupInput
            id={`${id}-pass`}
            name="password"
            required={!keep}
            minLength={MIN_LOCK_PASSWORD}
            maxLength={MAX_LOCK_PASSWORD}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            placeholder={keep ? "Unchanged" : `At least ${MIN_LOCK_PASSWORD} characters`}
            value={password}
            disabled={disabled}
            onChange={(e) => onPassword(e.target.value)}
          />
          <InputGroupAddon align="inline-end">
            <InputGroupButton disabled={disabled} onClick={() => onPassword(suggestPassword())}>
              <SparklesIcon data-icon="inline-start" />
              Suggest one
            </InputGroupButton>
          </InputGroupAddon>
        </InputGroup>
        <FieldDescription>
          {keep && "Leave it empty to keep the current password. "}Share it with the people who should see the site: the panel can't show it again later.
        </FieldDescription>
      </Field>
    </>
  )
}

// showLockPassword shows a lock's username and new password once, to pass on.
export function showLockPassword(domain: string, user: string, password: string) {
  showSecret(`Password for ${domain}`, [`Username: ${user}`, `Password: ${password}`], "Copy it now to pass it on: the panel doesn't show the password again.")
}

export function SiteLock({ site }: { site: Site }) {
  const s = useSession()
  const id = useId()
  const locked = !!site.site_lock
  // Single addresses without their /32 (/128): as people type them.
  const allow = (site.site_lock_allow ?? []).map((a) => a.replace(a.includes(":") ? /\/128$/ : /\/32$/, ""))
  const [editing, setEditing] = useState(false)
  const [user, setUser] = useState("")
  const [password, setPassword] = useState("")
  const [allowText, setAllowText] = useState("")
  const path = `/sites/${site.id}/lock`
  const canChange = s.canChangeSite(site, "manager")

  const open = () => {
    setUser(site.site_lock_user || "preview")
    setPassword(site.site_lock_user ? "" : suggestPassword())
    setAllowText(allow.join(", "))
    setEditing(true)
  }
  const turnOff = async () => {
    if (!(await ask(`Turn off password protection? Anyone can visit ${site.primary_domain} again. The username and password are kept for next time.`, { ok: "Turn off" })))
      return
    await api("PUT", path, { enabled: false })
    notify(`${site.primary_domain} is open to everyone`)
    await invalidate("/sites")
  }

  return (
    <Section
      icon={LockKeyholeIcon}
      tint="orange"
      title={
        <span className="flex flex-wrap items-center gap-2">
          Password protection
          {locked && <Badge variant="secondary">on</Badge>}
        </span>
      }
      description={
        locked
          ? `Visitors need the username ${site.site_lock_user} and its password to see anything of the site.`
          : "Anyone can visit the site. Turn this on to ask every visitor for a username and password first, while the site isn't ready to be seen."
      }
      action={
        canChange && (
          <div className="flex flex-wrap gap-2">
            {locked && (
              <ActionButton run={turnOff} variant="destructive">
                Turn off
              </ActionButton>
            )}
            <Button variant={locked ? "tinted" : "default"} onClick={open}>
              {locked ? "Change…" : "Turn on…"}
            </Button>
          </div>
        )
      }
    >
      {locked ? (
        <KeyValues
          items={[
            ["Username", <code>{site.site_lock_user}</code>],
            ["Password", <span className="text-muted-foreground">set (not shown)</span>],
            ["No password needed from", allow.length ? allow.join(", ") : <span className="text-muted-foreground">nowhere: everyone is asked</span>],
          ]}
        />
      ) : null}
      <FieldDescription className={locked ? "mt-4" : undefined}>
        Sign-in links from this panel (WordPress admin, phpMyAdmin) still work without the password. Search engines can't see a protected site.
      </FieldDescription>
      <FormDialog
        open={editing}
        onOpenChange={setEditing}
        title={locked ? "Change password protection" : "Turn on password protection"}
        intro={`Everyone visiting ${site.primary_domain} gets their browser's sign-in prompt, and sees the site after entering these.`}
        ok={locked ? "Save" : "Turn on"}
        onSubmit={async () => {
          const pass = password.trim() === "" ? "" : password
          await api("PUT", path, { enabled: true, username: user.trim(), password: pass || undefined, allow: allowText.split(/[\s,]+/).filter(Boolean) })
          await invalidate("/sites")
          notify(locked ? "Password protection changed" : `${site.primary_domain} now asks for a password`)
          if (pass) showLockPassword(site.primary_domain, user.trim(), pass)
        }}
      >
        <LockFields user={user} password={password} onUser={setUser} onPassword={setPassword} keep={!!site.site_lock_user} />
        <Field>
          <FieldLabel htmlFor={`${id}-allow`}>No password needed from (optional)</FieldLabel>
          <Textarea
            id={`${id}-allow`}
            name="allow"
            spellCheck={false}
            className="min-h-16 font-mono text-xs"
            placeholder="203.0.113.7, 198.51.100.0/24"
            value={allowText}
            onChange={(e) => setAllowText(e.target.value)}
          />
          <FieldDescription>Internet addresses (like your office's) that see the site without being asked, separated by commas or spaces. A range like 198.51.100.0/24 works too.</FieldDescription>
        </Field>
      </FormDialog>
    </Section>
  )
}
