import { useEffect, useRef, useState, type FormEvent } from "react"
import { api, ApiError } from "@/lib/api"
import { useSession } from "@/lib/session"
import type { User } from "@/lib/types"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { AuthLayout } from "@/features/auth/auth-layout"

export function SignIn() {
  const { signedIn } = useSession()
  const [needCode, setNeedCode] = useState(false)
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const [orderOpen, setOrderOpen] = useState(false)
  const codeRef = useRef<HTMLInputElement>(null)

  // A link to the order page when the store is open.
  useEffect(() => {
    api<{ enabled?: boolean; plans?: unknown[] }>("GET", "/store/catalog")
      .then((c) => setOrderOpen(!!(c && c.enabled && (c.plans || []).length)))
      .catch(() => {})
  }, [])
  useEffect(() => {
    if (needCode) codeRef.current?.focus()
  }, [needCode])

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const f = new FormData(e.currentTarget)
    setBusy(true)
    setError("")
    try {
      const res = await api<{ user: User }>("POST", "/auth/login", {
        username: String(f.get("username")).trim(),
        password: String(f.get("password")),
        code: String(f.get("code") || "").trim(),
      })
      signedIn(res.user)
    } catch (ex) {
      if (ex instanceof ApiError && ex.data.need_code) setNeedCode(true)
      else setError(ex instanceof Error ? ex.message : String(ex))
    } finally {
      setBusy(false)
    }
  }

  return (
    <AuthLayout>
      <h1 className="text-[2.125rem] leading-tight font-bold tracking-[-0.022em]">Sign in</h1>
      <p className="mt-1 mb-6 text-muted-foreground">Manage your sites, backups and security.</p>
      <form onSubmit={submit}>
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="username">Username</FieldLabel>
            <Input id="username" name="username" autoComplete="username" required autoFocus />
          </Field>
          <Field>
            <FieldLabel htmlFor="password">Password</FieldLabel>
            <Input id="password" name="password" type="password" autoComplete="current-password" required />
          </Field>
          {needCode && (
            <Field>
              <FieldLabel htmlFor="code">Code from your authenticator app (or a recovery code)</FieldLabel>
              <Input id="code" name="code" ref={codeRef} autoComplete="one-time-code" inputMode="numeric" />
            </Field>
          )}
          <Button type="submit" size="lg" disabled={busy}>
            {busy && <Spinner data-icon="inline-start" />}
            Sign in
          </Button>
          {error && <FieldError role="alert">{error}</FieldError>}
        </FieldGroup>
      </form>
      {orderOpen && (
        <p className="mt-6 text-center text-sm text-muted-foreground">
          New here? <a href="/order.html">Order hosting</a>
        </p>
      )}
    </AuthLayout>
  )
}

// The first administrator: proves ownership of the server with the API
// token the installer printed.
export function Setup() {
  const { signedIn } = useSession()
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    setBusy(true)
    setError("")
    try {
      const res = await api<{ user: User }>("POST", "/auth/setup", Object.fromEntries(new FormData(e.currentTarget)))
      signedIn(res.user)
    } catch (ex) {
      setError(ex instanceof Error ? ex.message : String(ex))
    } finally {
      setBusy(false)
    }
  }

  return (
    <AuthLayout aside={false}>
      <h1 className="text-[2.125rem] leading-tight font-bold tracking-[-0.022em]">Welcome to WPGenie</h1>
      <p className="mt-1 mb-6 text-muted-foreground">
        Create the first administrator. To prove you own this server, paste the API token the installer printed (it is in{" "}
        <code>/etc/wpgenie/config.json</code>).
      </p>
      <form onSubmit={submit}>
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="token">API token</FieldLabel>
            <Input id="token" name="token" type="password" autoComplete="off" required />
          </Field>
          <Field>
            <FieldLabel htmlFor="su">Username</FieldLabel>
            <Input id="su" name="username" autoComplete="username" required />
          </Field>
          <Field>
            <FieldLabel htmlFor="sp">
              Password <span className="font-normal text-muted-foreground">(12+ characters)</span>
            </FieldLabel>
            <Input id="sp" name="password" type="password" minLength={12} autoComplete="new-password" required />
          </Field>
          <Button type="submit" size="lg" disabled={busy}>
            {busy && <Spinner data-icon="inline-start" />}
            Create account
          </Button>
          {error && <FieldError role="alert">{error}</FieldError>}
        </FieldGroup>
      </form>
    </AuthLayout>
  )
}

// A one-time sign-in link from a billing portal (#sso=<token>): exchanged
// only after the user confirms, so a link can't sign anyone in behind
// their back.
export function SSO({ token, onDone }: { token: string; onDone: () => void }) {
  const { signedIn } = useSession()
  const [needCode, setNeedCode] = useState(false)
  const [code, setCode] = useState("")
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)

  async function go(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError("")
    try {
      const res = await api<{ user: User }>("POST", "/auth/sso", { token, code: code.trim() })
      onDone()
      signedIn(res.user)
    } catch (ex) {
      if (ex instanceof ApiError && ex.data.need_code) setNeedCode(true)
      else setError("This link is invalid, expired or already used. Sign in with your password, or ask for a new link.")
    } finally {
      setBusy(false)
    }
  }

  return (
    <AuthLayout>
      <h1 className="text-[2.125rem] leading-tight font-bold tracking-[-0.022em]">Sign in</h1>
      <p className="mt-1 mb-6 text-muted-foreground">You followed a one-time sign-in link from your billing portal.</p>
      <form onSubmit={go}>
        <FieldGroup>
          {needCode && (
            <Field>
              <FieldLabel htmlFor="sso-code">Two-factor code</FieldLabel>
              <Input id="sso-code" autoFocus autoComplete="one-time-code" inputMode="numeric" placeholder="Code from your authenticator app" value={code} onChange={(e) => setCode(e.target.value)} />
            </Field>
          )}
          <Button type="submit" size="lg" disabled={busy}>
            {busy && <Spinner data-icon="inline-start" />}
            Continue to the panel
          </Button>
          {error && (
            <>
              <FieldError role="alert">{error}</FieldError>
              <Button type="button" variant="tinted" onClick={onDone}>
                Sign in with a password
              </Button>
            </>
          )}
        </FieldGroup>
      </form>
    </AuthLayout>
  )
}
