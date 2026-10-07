import { useState, type FormEvent } from "react"
import { HistoryIcon, KeyIcon, ShieldAlertIcon, UserIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Banner, LoadError } from "@/components/app/blocks"
import { KeyValues } from "@/components/app/data-table"
import { Page, PageHeader, Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { User } from "@/lib/types"
import { AccountPlan, type TenantAccount } from "./plan"
import { ROLE_NAMES } from "./roles"
import { SessionsTable, type SessionRow } from "./sessions"
import { ApiTokens } from "./tokens"
import { TwoFactor } from "./two-factor"

// Your account: profile, a tenant's plan, two-factor authentication,
// password, sessions and API tokens.

interface AccountResp {
  user: User
  require_2fa: boolean
  account?: TenantAccount
}

export default function AccountPage() {
  const s = useSession()
  const acct = useApi<AccountResp>("/account")
  const sessions = useApi<SessionRow[]>("/account/sessions")
  const u = acct.data?.user
  const locked = !!acct.data && acct.data.require_2fa && !u?.totp_enabled

  return (
    <Page>
      <PageHeader icon={UserIcon} tint="gray" title="Your account" description="Your profile, sign-in security and API access." />

      {acct.isLoading && (
        <div className="flex flex-col gap-4">
          <Skeleton className="h-36 rounded-2xl" />
          <Skeleton className="h-32 rounded-2xl" />
          <Skeleton className="h-48 rounded-2xl" />
        </div>
      )}
      {acct.error && <LoadError error={acct.error} retry={() => acct.refetch()} />}

      {u && (
        <>
          {locked && (
            <Banner tone="warn" icon={ShieldAlertIcon} title="This panel requires two-factor authentication.">
              Set it up below to continue.
            </Banner>
          )}

          <Section icon={UserIcon} tint="gray" title={u.username}>
            <KeyValues
              items={[
                ["Role", ROLE_NAMES[u.role] ?? u.role],
                ["Member since", fmtTime(u.created_at)],
                ["Last sign-in", u.last_login_at ? fmtTime(u.last_login_at) : "–"],
              ]}
            />
          </Section>

          {/* A tenant's plan (its usage is out of reach until 2FA is on, where required). */}
          {s.isTenant && acct.data?.account && !locked && <AccountPlan account={acct.data.account} />}

          <TwoFactor user={u} />

          <PasswordSection />

          <Section icon={HistoryIcon} tint="indigo" title="Your sessions">
            {sessions.isLoading && <Skeleton className="h-24 rounded-xl" />}
            {sessions.error && <LoadError error={sessions.error} retry={() => sessions.refetch()} />}
            {sessions.data && <SessionsTable sessions={sessions.data} onChanged={() => invalidate("/account/sessions")} />}
          </Section>

          <ApiTokens blocked={locked} />
        </>
      )}
    </Page>
  )
}

function PasswordSection() {
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (busy) return
    const f = e.currentTarget
    const data = new FormData(f)
    setBusy(true)
    try {
      await api("PUT", "/account/password", { current: data.get("current"), new: data.get("new") })
      f.reset()
      notify("Password changed. Your other sessions were signed out.")
      await invalidate("/account/sessions")
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section icon={KeyIcon} tint="orange" title="Password" description="Changing it signs out your other sessions.">
      <form onSubmit={submit}>
        <FieldGroup className="max-w-md">
          <Field>
            <FieldLabel htmlFor="pw-current">Current password</FieldLabel>
            <Input id="pw-current" name="current" type="password" autoComplete="current-password" required />
          </Field>
          <Field>
            <FieldLabel htmlFor="pw-new">New password</FieldLabel>
            <Input id="pw-new" name="new" type="password" minLength={12} autoComplete="new-password" required />
            <FieldDescription>12 characters or more.</FieldDescription>
          </Field>
          <div>
            <Button type="submit" disabled={busy}>
              Change password
            </Button>
          </div>
        </FieldGroup>
      </form>
    </Section>
  )
}
