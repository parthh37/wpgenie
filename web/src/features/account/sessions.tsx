import { LogOutIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { ActionButton, BTable } from "@/components/app/blocks"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { useSession } from "@/lib/session"

// Signed-in sessions: yours (Account) or everyone's (Users, with the user
// column). Revoking the one this browser uses signs you out.

export interface SessionRow {
  id: string
  user_id: number
  username: string
  created_at: string
  last_seen_at: string
  expires_at: string
  ip: string
  user_agent: string
  current: boolean
}

// shortUA: "Chrome on macOS" from a user agent.
export function shortUA(ua: string | undefined) {
  const m = /(Firefox|Edg|Chrome|Safari)\/[\d.]+/.exec(ua || "")
  const os = /(Windows|Mac OS X|Android|iPhone|Linux)/.exec(ua || "")
  return [m ? m[1].replace("Edg", "Edge") : "unknown", os ? os[1].replace("Mac OS X", "macOS") : ""].filter(Boolean).join(" on ")
}

export function SessionsTable({
  sessions,
  withUser,
  onChanged,
}: {
  sessions: SessionRow[]
  withUser?: boolean
  onChanged: () => unknown
}) {
  const s = useSession()
  return (
    <BTable
      caption={withUser ? "Everyone's signed-in sessions" : "Your signed-in sessions"}
      empty={<p className="py-2 text-sm text-muted-foreground">No sessions.</p>}
      cols={[...(withUser ? ["User"] : []), "Signed in", "Last active", "From", "Browser", { label: <span className="sr-only">Actions</span>, className: "w-0" }]}
      rows={sessions.map((x) => ({
        key: x.id,
        cells: [
          ...(withUser ? [<span className="font-medium">{x.username}</span>] : []),
          fmtTime(x.created_at),
          fmtTime(x.last_seen_at),
          <span className="font-mono text-xs">{x.ip}</span>,
          <span title={x.user_agent} className="inline-flex flex-wrap items-center gap-1.5 text-sm">
            {shortUA(x.user_agent)}
            {x.current && <Badge variant="secondary">this one</Badge>}
          </span>,
          <ActionButton
            size="sm"
            variant={x.current ? "destructive" : "tinted"}
            run={async () => {
              await api("DELETE", (withUser ? "/sessions/" : "/account/sessions/") + encodeURIComponent(x.id))
              if (x.current) {
                await s.signOut()
                return
              }
              await onChanged()
            }}
          >
            {x.current && <LogOutIcon data-icon="inline-start" />}
            {x.current ? "Sign out" : "Revoke"}
          </ActionButton>,
        ],
      }))}
    />
  )
}
