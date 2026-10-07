import { useState, useSyncExternalStore } from "react"
import { CheckIcon, CopyIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

// showSecret(title, lines): something shown only once (an API token, a new
// password, recovery codes), with a copy button. Callable from anywhere.

interface Secret {
  title: string
  lines: string[]
  note?: string
}
let current: Secret | null = null
const subs = new Set<() => void>()
const set = (s: Secret | null) => {
  current = s
  subs.forEach((f) => f())
}

export function showSecret(title: string, lines: string[], note = "Save this now — it is shown only once.") {
  set({ title, lines, note })
}

export function SecretHost() {
  const s = useSyncExternalStore((f) => (subs.add(f), () => void subs.delete(f)), () => current)
  const [copied, setCopied] = useState(false)
  if (!s) return null
  const text = s.lines.join("\n")
  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) {
          set(null)
          setCopied(false)
        }
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="pr-8">{s.title}</DialogTitle>
          {s.note && <DialogDescription>{s.note}</DialogDescription>}
        </DialogHeader>
        <pre className="overflow-x-auto rounded-xl bg-muted p-3 font-mono text-sm whitespace-pre-wrap select-all [overflow-wrap:anywhere]">{text}</pre>
        <DialogFooter>
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
          <Button
            onClick={() => {
              set(null)
              setCopied(false)
            }}
          >
            I saved it
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
