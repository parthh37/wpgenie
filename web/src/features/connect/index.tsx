import { useState } from "react"
import { CheckIcon, ShieldCheckIcon, SparklesIcon, TriangleAlertIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Banner } from "@/components/app/blocks"
import { IconTile } from "@/components/app/icon-tile"
import { showError } from "@/components/app/toaster"
import { ROLE_NAMES } from "@/features/account/roles"
import { AuthLayout } from "@/features/auth/auth-layout"
import { api, ApiError } from "@/lib/api"
import { useApi } from "@/lib/query"
import { href, useHashQuery } from "@/lib/router"

// The consent screen of an AI assistant connecting over MCP: the server's
// /oauth/authorize sends the browser here (#/connect?client_id=...), after
// signing in if needed. Approving asks the API for a code with this
// session and sends the browser back to the assistant.

interface RequestInfo {
  client_name: string
  redirect_host: string
  user: string
  role: string
}

const PARAMS = ["response_type", "client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "scope", "resource"]

export default function Connect() {
  const q = useHashQuery()
  const req = Object.fromEntries(PARAMS.map((k) => [k, q.get(k) ?? ""]))
  const info = useApi<RequestInfo>("/oauth/authorize?" + new URLSearchParams(req).toString())
  const [busy, setBusy] = useState<"" | "approve" | "deny">("")

  async function answer(approve: boolean) {
    setBusy(approve ? "approve" : "deny")
    try {
      const r = await api<{ redirect: string }>("POST", "/oauth/authorize", { ...req, approve })
      location.assign(r.redirect)
    } catch (err) {
      showError(err)
      setBusy("")
    }
  }

  const needs2FA = info.error instanceof ApiError && info.error.code === "totp_required"

  return (
    <AuthLayout aside={false}>
      <div className="flex flex-col gap-5">
        <div className="flex items-center gap-3">
          <IconTile icon={SparklesIcon} tint="purple" size="lg" />
          <h1 className="text-xl font-semibold">Connect an AI assistant</h1>
        </div>

        {info.isLoading && <Skeleton className="h-40 rounded-2xl" />}

        {needs2FA && (
          <Banner tone="warn" icon={TriangleAlertIcon} title="Two-factor authentication first">
            This panel requires it before an assistant can connect. <a href={href("/account")}>Set it up</a>, then start connecting again from
            the assistant.
          </Banner>
        )}
        {info.error && !needs2FA && (
          <Banner tone="bad" icon={TriangleAlertIcon} title="This connection request can't be used.">
            {info.error.message}. Start connecting again from the assistant.
          </Banner>
        )}

        {info.data && (
          <>
            <p>
              <strong className="font-semibold">{info.data.client_name}</strong> wants to manage your sites as{" "}
              <strong className="font-semibold">{info.data.user}</strong> ({ROLE_NAMES[info.data.role] ?? info.data.role}).
            </p>
            <ul className="flex list-none flex-col gap-2 p-0 text-sm">
              <Point>See your sites, their traffic, updates, backups and security reports.</Point>
              {info.data.role !== "viewer" && (
                <Point>Back up, update WordPress, clear the cache, make staging copies, switch themes and more, as far as your role allows.</Point>
              )}
              <Point>Never delete sites or files, restore over a site, or see passwords.</Point>
            </ul>
            <p className="text-sm text-muted-foreground">
              Approving sends you back to <strong className="font-semibold text-foreground">{info.data.redirect_host}</strong>. Only approve if
              you started this from that assistant. You can disconnect it any time under Your account.
            </p>
            <div className="flex flex-wrap gap-2">
              <Button onClick={() => answer(true)} disabled={busy !== ""}>
                <ShieldCheckIcon data-icon="inline-start" />
                {busy === "approve" ? "Connecting…" : "Approve"}
              </Button>
              <Button variant="secondary" onClick={() => answer(false)} disabled={busy !== ""}>
                Decline
              </Button>
            </div>
          </>
        )}
      </div>
    </AuthLayout>
  )
}

function Point({ children }: { children: React.ReactNode }) {
  return (
    <li className="flex items-start gap-2">
      <CheckIcon className="mt-0.5 size-4 shrink-0 text-success" />
      <span>{children}</span>
    </li>
  )
}
