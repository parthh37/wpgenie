import { useState } from "react"
import { ExternalLinkIcon, KeyRoundIcon, LogInIcon, UsersIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { askText } from "@/components/app/confirm"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { plural } from "@/lib/format"
import { useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "../sections"
import type { WPUser } from "./protect/types"

// WordPress admin: sign in to wp-admin as any administrator without their
// password, and reset an administrator's password (wordpress.js
// renderWordPress/showAdmins).

// openFresh opens a tab now, in the click (one opened after the request
// would be blocked as a pop-up), then sends it to the URL get() resolves
// to. Without a tab (pop-ups blocked), the link is shown to copy.
async function openFresh(get: () => Promise<string>, fallbackTitle: string) {
  const win = window.open("about:blank", "_blank")
  try {
    const url = await get()
    if (win) {
      win.opener = null
      win.location.href = url
    } else showSecret(fallbackTitle, [url], "Open it in this browser within 2 minutes: it works once.")
  } catch (e) {
    win?.close()
    showError(e)
  }
}

// wpLogin signs in to a site's wp-admin as an administrator (0: the oldest).
const wpLogin = (site: Site, userID = 0) =>
  openFresh(
    async () => (await api<{ url: string }>("POST", `/sites/${site.id}/wp-admin/login`, { user_id: userID })).url,
    "wp-admin sign-in link (single use, 2 minutes)"
  )

async function resetPassword(site: Site, u: WPUser) {
  const pw = await askText(
    `Choose a new password, or leave it empty for a strong random one (shown once). Every session of ${u.login} ends: anyone signed in as them is signed out.`,
    {
      title: `Reset ${u.login}'s password on ${site.primary_domain}?`,
      label: "New password (12+ characters, optional)",
      type: "password",
      autocomplete: "new-password",
      ok: "Reset password",
      danger: true,
    }
  )
  if (pw === null) return
  if (pw && pw.length < 12) {
    showError(new Error("The password must be at least 12 characters, or empty for a random one."))
    return
  }
  try {
    const r = await api<{ user: string; password: string }>("POST", `/sites/${site.id}/wp-admin/password`, { user_id: u.id, password: pw })
    if (pw) notify(`Password of ${r.user} changed; their sessions ended`)
    else showSecret(`New WordPress password on ${site.primary_domain}`, [`Username: ${r.user}`, `Password: ${r.password}`])
  } catch (e) {
    showError(e)
  }
}

export default function WordpressSection({ site }: SectionProps) {
  const s = useSession()
  const [busy, setBusy] = useState(false)
  const adminURL = `https://${site.primary_domain}/wp-admin/`

  const signIn = async () => {
    setBusy(true)
    await wpLogin(site)
    setBusy(false)
  }

  return (
    <div className="flex flex-col">
      <Section
        icon={KeyRoundIcon}
        tint="yellow"
        title="Sign in without a password"
        description="Sign in to wp-admin as any administrator without their password: a one-time link on the site's own domain opens a normal WordPress session (listed in that user's sessions, ended by logging out). Resetting a password ends all of that administrator's sessions; WordPress sends no e-mail about it."
      >
        <div className="flex flex-wrap gap-2">
          {s.canChange ? (
            <Button disabled={busy} onClick={signIn} title="Sign in to wp-admin (no WordPress password needed)">
              <LogInIcon data-icon="inline-start" />
              Sign in to wp-admin
            </Button>
          ) : (
            // Viewers can't make sessions: for them it's the plain sign-in page.
            <Button render={<a href={adminURL} target="_blank" rel="noopener" />} nativeButton={false}>
              <ExternalLinkIcon data-icon="inline-start" />
              Open wp-admin
            </Button>
          )}
        </div>
      </Section>
      <Administrators site={site} />
    </div>
  )
}

function Administrators({ site }: { site: Site }) {
  const s = useSession()
  const q = useApi<WPUser[]>(`/sites/${site.id}/wp-admin/users`)
  const users = q.data ?? []
  return (
    <Section
      icon={UsersIcon}
      tint="blue"
      title="Administrators"
      description={q.data ? plural(users.length, "administrator") : undefined}
    >
      {q.isPending ? (
        <div className="flex flex-col gap-2">
          <Skeleton className="h-10 rounded-xl" />
          <Skeleton className="h-10 rounded-xl" />
        </div>
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <SimpleTable
          headers={["Administrator", "E-mail", ""]}
          empty="No administrators found."
          rowKey={(i) => users[i].id}
          rows={users.map((u) => [
            <div className="flex flex-col">
              <strong className="font-semibold">{u.login}</strong>
              {u.name && u.name !== u.login && <span className="text-sm font-normal text-muted-foreground">{u.name}</span>}
            </div>,
            u.email,
            s.canChange ? (
              <div className="flex justify-end gap-2">
                <Button variant="tinted" size="sm" onClick={() => wpLogin(site, u.id)}>
                  Sign in as {u.login}
                </Button>
                <Button variant="tinted" size="sm" onClick={() => resetPassword(site, u)}>
                  Reset password
                </Button>
              </div>
            ) : null,
          ])}
        />
      )}
    </Section>
  )
}
