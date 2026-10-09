import { useState, type FormEvent } from "react"
import { CodeIcon, PlusIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { showSecret } from "@/components/app/secret"
import { showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"

// API tokens: for scripts and billing systems. Shown once; only a hash is
// kept on the server.

export interface Token {
  id: number
  name: string
  hint: string
  created_at: string
  expires_at?: string
  last_used_at?: string
  last_used_ip?: string
  // An AI assistant's access (assistants.tsx lists those).
  client_id?: string
}

export function ApiTokens({ blocked }: { blocked: boolean }) {
  // Until two-factor authentication is on (on a panel that requires it),
  // the server refuses tokens: a token would carry the session past it.
  const list = useApi<Token[]>(blocked ? null : "/account/tokens")
  const [busy, setBusy] = useState(false)

  async function create(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (busy) return
    const f = e.currentTarget
    const data = new FormData(f)
    setBusy(true)
    try {
      const r = await api<{ token: string; api_token: Token }>("POST", "/account/tokens", {
        name: String(data.get("name") || "").trim(),
        expires_days: Number(data.get("expires_days") || 0),
      })
      f.reset()
      showSecret(`API token "${r.api_token.name}"`, [r.token, "", "Use it as: Authorization: Bearer <token>"])
      await invalidate("/account/tokens")
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={CodeIcon}
      tint="blue"
      title="API tokens"
      description="For scripts and billing systems (the WHMCS module). A token acts as you, with your role and account, and can be revoked at any time. It is shown once; only a hash is kept."
    >
      {blocked ? (
        <p className="text-sm text-muted-foreground">Turn on two-factor authentication first: this panel requires it before API tokens can be made.</p>
      ) : (
        <>
          <form onSubmit={create} className="mb-4 flex flex-wrap items-end gap-2">
            <div className="grid min-w-56 flex-1 gap-1.5">
              <Label htmlFor="token-name">What it is for</Label>
              <Input id="token-name" name="name" placeholder="e.g. WHMCS" maxLength={64} required autoComplete="off" />
            </div>
            <div className="grid w-48 gap-1.5">
              <Label htmlFor="token-days">Expires in days</Label>
              <Input id="token-days" name="expires_days" type="number" min={0} max={3650} placeholder="0 = never" />
            </div>
            <Button type="submit" disabled={busy}>
              <PlusIcon data-icon="inline-start" />
              Create token
            </Button>
          </form>
          {list.isLoading && <Skeleton className="h-24 rounded-xl" />}
          {list.error && <LoadError error={list.error} retry={() => list.refetch()} />}
          {list.data && (
            <BTable
              caption="Your API tokens"
              empty={<p className="py-2 text-sm text-muted-foreground">No tokens yet.</p>}
              cols={["Name", "Token", "Created", "Expires", "Last used", { label: <span className="sr-only">Actions</span>, className: "w-0" }]}
              rows={list.data.filter((t) => !t.client_id).map((t) => ({
                key: t.id,
                cells: [
                  <span className="font-medium">{t.name}</span>,
                  <code className="font-mono text-xs">{t.hint}</code>,
                  fmtTime(t.created_at),
                  t.expires_at ? fmtTime(t.expires_at) : "never",
                  t.last_used_at ? `${fmtTime(t.last_used_at)} from ${t.last_used_ip}` : "never",
                  <ActionButton
                    size="sm"
                    variant="destructive"
                    run={async () => {
                      if (!(await ask(`Revoke the token "${t.name}"? Anything using it stops working at once.`))) return
                      await api("DELETE", `/account/tokens/${t.id}`)
                      await invalidate("/account/tokens")
                    }}
                  >
                    Revoke
                  </ActionButton>,
                ],
              }))}
            />
          )}
        </>
      )}
    </Section>
  )
}
