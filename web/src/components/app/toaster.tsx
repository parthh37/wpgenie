import { useSyncExternalStore } from "react"
import { CircleCheckIcon, TriangleAlertIcon, XIcon } from "lucide-react"
import { cn } from "@/lib/utils"

// Toasts: cards stacked in the top-right corner. Errors stay until
// dismissed (they matter, and may be long); the rest go on their own, not
// while the pointer is on them. Callable from anywhere: toast(), notify(),
// showError().

const TOAST_MS = 4500
type Kind = "ok" | "error"
interface Toast {
  id: number
  message: string
  kind: Kind
  leaving: boolean
}

let toasts: Toast[] = []
let nextId = 1
const subs = new Set<() => void>()
const set = (next: Toast[]) => {
  toasts = next
  subs.forEach((f) => f())
}
const timers = new Map<number, ReturnType<typeof setTimeout>>()

function dismiss(id: number) {
  clearTimeout(timers.get(id))
  timers.delete(id)
  set(toasts.map((t) => (t.id === id ? { ...t, leaving: true } : t)))
  setTimeout(() => set(toasts.filter((t) => t.id !== id)), 300)
}

function arm(id: number, ms: number) {
  clearTimeout(timers.get(id))
  timers.set(id, setTimeout(() => dismiss(id), ms))
}

export function toast(message: string, kind: Kind = "ok") {
  // A retried action shows once.
  const kept = toasts.filter((t) => t.message !== message).slice(-3)
  const t = { id: nextId++, message, kind, leaving: false }
  set([...kept, t])
  if (kind !== "error") arm(t.id, TOAST_MS)
  return t.id
}

export const notify = (message: string) => toast(message, "ok")

// showError(err) shows a failure; showError(null) clears them (switching
// pages does).
export function showError(err: unknown) {
  if (err == null) {
    toasts.filter((t) => t.kind === "error").forEach((t) => dismiss(t.id))
    return
  }
  toast(err instanceof Error ? err.message : String(err), "error")
}

export function Toaster({ withSidebar = false }: { withSidebar?: boolean }) {
  const list = useSyncExternalStore(
    (f) => (subs.add(f), () => void subs.delete(f)),
    () => toasts
  )
  return (
    <div
      aria-live="polite"
      className={cn(
        "pointer-events-none fixed right-4 z-[100] flex w-[min(380px,calc(100vw-2rem))] flex-col gap-2",
        // In the panel, under the top bar; signed out, the top corner.
        withSidebar ? "top-[4.375rem]" : "top-4"
      )}
    >
      {list.map((t) => (
        <div
          key={t.id}
          role={t.kind === "error" ? "alert" : undefined}
          onPointerEnter={() => t.kind !== "error" && clearTimeout(timers.get(t.id))}
          onPointerLeave={() => t.kind !== "error" && !t.leaving && arm(t.id, TOAST_MS / 2)}
          className={cn(
            "pointer-events-auto flex w-full items-start gap-2.5 rounded-lg bg-popover py-3 pr-2 pl-3.5 text-popover-foreground shadow-lg ring-1 ring-border",
            t.kind === "error" && "ring-danger-fill/40",
            t.leaving ? "animate-toast-out" : "animate-toast-in"
          )}
        >
          {t.kind === "error" ? (
            <TriangleAlertIcon aria-hidden className="mt-px size-[18px] shrink-0 text-danger" />
          ) : (
            <CircleCheckIcon aria-hidden className="mt-px size-[18px] shrink-0 text-success" />
          )}
          <p className="m-0 min-w-0 flex-1 text-base font-medium [overflow-wrap:anywhere]">{t.message}</p>
          <button
            type="button"
            aria-label="Dismiss"
            onClick={() => dismiss(t.id)}
            className="-my-1 flex size-7 shrink-0 cursor-pointer items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <XIcon className="size-4" />
          </button>
        </div>
      ))}
    </div>
  )
}
