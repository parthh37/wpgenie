import { useState, type FormEvent } from "react"
import { KeyRoundIcon, LockIcon, ShieldCheckIcon, SmartphoneIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { ActionButton } from "@/components/app/blocks"
import { askText } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { invalidate, queryClient } from "@/lib/query"
import type { User } from "@/lib/types"

// Two-factor authentication: enrol (a QR code from the server, as an
// image of this origin, so the CSP stays strict; or the key typed in),
// new recovery codes, turn off.

interface Enrolment {
  secret: string
  uri: string
  qr: string
}

// After a change, the page and the shell (which keeps a panel that
// requires 2FA on Account until it's on) see it.
async function refresh() {
  await Promise.all([invalidate("/account"), queryClient.invalidateQueries({ queryKey: ["auth-state"] })])
}

export function TwoFactor({ user }: { user: User }) {
  const [enrol, setEnrol] = useState<Enrolment | null>(null)
  const [code, setCode] = useState("")
  const [busy, setBusy] = useState(false)

  async function confirm(e: FormEvent) {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      const res = await api<{ recovery_codes: string[] }>("PUT", "/account/totp", { code: code.trim() })
      setEnrol(null)
      setCode("")
      await refresh()
      showSecret("Recovery codes: each signs you in once if you lose your phone", res.recovery_codes)
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section icon={LockIcon} tint="green" title="Two-factor authentication">
      {user.totp_enabled ? (
        <div className="flex flex-col gap-4">
          <p className="text-sm">
            <span className="font-semibold text-success">On.</span> Signing in asks for a code from your authenticator app.{" "}
            {user.recovery_codes_left} recovery code{user.recovery_codes_left === 1 ? "" : "s"} left.
          </p>
          <div className="flex flex-wrap gap-2">
            <ActionButton
              run={async () => {
                const password = await askText("The old recovery codes stop working.", {
                  title: "New recovery codes",
                  label: "Your password",
                  type: "password",
                  autocomplete: "current-password",
                  ok: "Create codes",
                })
                if (!password) return
                const r = await api<{ recovery_codes: string[] }>("POST", "/account/recovery-codes", { password })
                showSecret("Recovery codes", r.recovery_codes)
                await refresh()
              }}
            >
              <KeyRoundIcon data-icon="inline-start" />
              New recovery codes
            </ActionButton>
            <ActionButton
              variant="destructive"
              run={async () => {
                const password = await askText("Signing in will only need your password.", {
                  title: "Turn off two-factor authentication?",
                  label: "Your password",
                  type: "password",
                  autocomplete: "current-password",
                  ok: "Turn off",
                })
                if (!password) return
                await api("DELETE", "/account/totp", { password })
                await refresh()
                notify("Two-factor authentication is off")
              }}
            >
              Turn off
            </ActionButton>
          </div>
        </div>
      ) : enrol ? (
        <div className="flex flex-col gap-4">
          <p className="text-sm">Scan this with an authenticator app (1Password, Google Authenticator, Authy, …), then enter the code it shows.</p>
          <div className="flex flex-wrap items-start gap-5">
            {/* Same-origin SVG from the server: allowed by default-src 'self'. */}
            <img
              src={enrol.qr}
              alt="QR code for your authenticator app"
              width={200}
              height={200}
              className="size-[200px] rounded-xl bg-white p-2 ring-1 ring-border"
            />
            <div className="flex min-w-0 flex-1 flex-col gap-3 text-sm">
              <p className="text-muted-foreground">
                Can't scan? Enter this key:{" "}
                <code className="rounded-md bg-muted px-1.5 py-0.5 font-mono text-foreground select-all">{enrol.secret.replace(/(.{4})/g, "$1 ").trim()}</code>
              </p>
              <p className="text-muted-foreground">
                On this phone?{" "}
                <a href={enrol.uri} className="inline-flex items-center gap-1 text-link">
                  <SmartphoneIcon className="size-3.5" />
                  Open it in your authenticator app
                </a>
              </p>
              <form onSubmit={confirm} className="flex flex-wrap items-end gap-2">
                <div className="grid gap-1.5">
                  <Label htmlFor="totp-code">Code from the app</Label>
                  <Input
                    id="totp-code"
                    autoFocus
                    className="w-40 font-mono tracking-widest"
                    placeholder="6-digit code"
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    required
                    value={code}
                    onChange={(e) => setCode(e.target.value)}
                  />
                </div>
                <Button type="submit" disabled={busy}>
                  <ShieldCheckIcon data-icon="inline-start" />
                  Turn on
                </Button>
                <Button type="button" variant="ghost" onClick={() => {
                    setEnrol(null)
                    setCode("")
                  }}>
                  Cancel
                </Button>
              </form>
            </div>
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          <p className="text-sm">
            <span className="font-semibold text-warning">Off.</span> A stolen password is enough to get into your account. Add a code from your
            phone.
          </p>
          <div>
            <ActionButton
              variant="default"
              run={async () => {
                const r = await api<{ secret: string; uri: string }>("POST", "/account/totp")
                setEnrol({ ...r, qr: `/api/v1/account/totp/qr.svg?${Date.now()}` })
              }}
            >
              <ShieldCheckIcon data-icon="inline-start" />
              Set up two-factor authentication
            </ActionButton>
          </div>
        </div>
      )}
    </Section>
  )
}
