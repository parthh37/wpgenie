import { useSyncExternalStore } from "react"
import { CheckIcon, TriangleAlertIcon, XIcon } from "lucide-react"
import { cn } from "@/lib/utils"

// Dynamic Island toasts: a black pill at the top centre. Errors stay until
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
      className={cn("pointer-events-none fixed top-2.5 left-1/2 z-[100] flex w-max max-w-[min(520px,calc(100vw-1.5rem))] -translate-x-1/2 flex-col items-center gap-1.5", withSidebar && "md:left-[calc(50%+8rem)]")}
    >
      {list.map((t) => (
        <div
          key={t.id}
          role={t.kind === "error" ? "alert" : undefined}
          onPointerEnter={() => t.kind !== "error" && clearTimeout(timers.get(t.id))}
          onPointerLeave={() => t.kind !== "error" && !t.leaving && arm(t.id, TOAST_MS / 2)}
          className={cn(
            "pointer-events-auto flex min-h-11 max-w-full items-center gap-2.5 rounded-full bg-black py-2 pr-2 pl-3.5 text-white shadow-[0_10px_40px_rgba(0,0,0,.35),0_0_0_.5px_rgba(255,255,255,.12)]",
            t.leaving ? "animate-island-out" : "animate-island-in"
          )}
        >
          <span
            aria-hidden
            className={cn(
              "flex size-[22px] shrink-0 items-center justify-center rounded-full",
              t.kind === "error" ? "bg-[#ff453a] text-white" : "bg-[#30d158] text-black"
            )}
          >
            {t.kind === "error" ? <TriangleAlertIcon className="size-3.5" strokeWidth={2.5} /> : <CheckIcon className="size-3.5" strokeWidth={3} />}
          </span>
          <p className="m-0 flex-1 text-[0.9375rem] font-medium tracking-[-0.01em] [overflow-wrap:anywhere]">{t.message}</p>
          <button
            type="button"
            aria-label="Dismiss"
            onClick={() => dismiss(t.id)}
            className="flex size-7 shrink-0 items-center justify-center rounded-full bg-white/12 text-white/55 transition-colors hover:bg-white/22 hover:text-white"
          >
            <XIcon className="size-3.5" strokeWidth={2.5} />
          </button>
        </div>
      ))}
    </div>
  )
}
