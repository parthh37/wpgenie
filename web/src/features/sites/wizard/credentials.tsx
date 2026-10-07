import { useEffect, useState, useSyncExternalStore } from "react"
import { CheckIcon, CopyIcon, ExternalLinkIcon, PartyPopperIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { IconTile } from "@/components/app/icon-tile"
import { showSecret } from "@/components/app/secret"
import { api } from "@/lib/api"

// A new site's admin credentials: the creation job's secret, only in memory
// on the server, for the user who created the site. Shown once; "I saved
// them" drops the secret on the server (it expires anyway).

interface Creds {
  domain: string
  jobId: string
  secret: Record<string, string>
}

let pending: Creds[] = []
let hosts = 0
const subs = new Set<() => void>()
const emit = () => subs.forEach((f) => f())

const lines = (c: Record<string, string>) => [`Admin URL: ${c.admin_url}`, `Username:  ${c.username}`, `Password:  ${c.password}`]

export function showCredentials(domain: string, secret: Record<string, string>, jobId: string) {
  // Away from the Sites page (no host to show them): the panel's shown-once
  // dialog, which can't drop the secret early.
  if (!hosts) {
    showSecret(`${domain} is live`, lines(secret), "Save these credentials now — they are shown only once.")
    return
  }
  pending = [...pending, { domain, jobId, secret }]
  emit()
}

export function CredentialsHost() {
  const cur = useSyncExternalStore(
    (f) => (subs.add(f), () => void subs.delete(f)),
    () => pending[0]
  )
  useEffect(() => {
    hosts++
    return () => void hosts--
  }, [])
  const [copied, setCopied] = useState(false)

  const done = async () => {
    if (!cur) return
    pending = pending.slice(1)
    setCopied(false)
    emit()
    try {
      await api("DELETE", `/jobs/${cur.jobId}/secret`)
    } catch {
      /* expires anyway */
    }
  }

  const text = cur ? lines(cur.secret).join("\n") : ""
  return (
    // Only "I saved them" closes it: they aren't shown again.
    <Dialog open={!!cur} disablePointerDismissal onOpenChange={() => {}}>
      <DialogContent showCloseButton={false} className="sm:max-w-lg">
        {cur && (
          <>
            <DialogHeader className="flex-row items-center gap-3">
              <IconTile icon={PartyPopperIcon} tint="green" size="lg" />
              <div className="flex flex-col gap-1">
                <DialogTitle className="text-lg font-semibold">{cur.domain} is live</DialogTitle>
                <DialogDescription>Save these credentials now — they are shown only once.</DialogDescription>
              </div>
            </DialogHeader>
            <pre className="overflow-x-auto rounded-xl bg-muted p-3 font-mono text-sm whitespace-pre-wrap select-all [overflow-wrap:anywhere]">{text}</pre>
            <DialogFooter className="sm:justify-between">
              {/^https?:\/\//.test(cur.secret.admin_url ?? "") ? (
                <Button variant="tinted" render={<a href={cur.secret.admin_url} target="_blank" rel="noopener noreferrer" />} nativeButton={false}>
                  <ExternalLinkIcon data-icon="inline-start" />
                  Open WordPress admin
                </Button>
              ) : (
                <span />
              )}
              <div className="flex flex-col-reverse gap-2 sm:flex-row">
                <Button
                  variant="tinted"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(text)
                      setCopied(true)
                    } catch {
                      /* select-all on the text still works */
                    }
                  }}
                >
                  {copied ? <CheckIcon data-icon="inline-start" /> : <CopyIcon data-icon="inline-start" />}
                  {copied ? "Copied" : "Copy"}
                </Button>
                <Button onClick={done}>I saved them</Button>
              </div>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}
