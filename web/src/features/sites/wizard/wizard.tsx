import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react"
import { CheckIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { errorMessage } from "@/lib/api"
import { cn } from "@/lib/utils"

// A step-by-step dialog. Steps are built by the caller on every render (so
// they see its state), and the caller keeps which one is shown (step,
// onStep), so it knows which step is current. A step's next()
// resolves to false to stay (it showed why), or throws (shown under the
// step); after the last step the wizard closes, done. The step being shown
// is the only one mounted: mounting is entering it, unmounting leaving.

export interface WizardStep {
  label: string
  content: ReactNode
  next?: () => boolean | void | Promise<boolean | void>
  // The Next button, when it says or does something else.
  button?: { label?: string; danger?: boolean; disabled?: boolean }
}

interface WizardProps {
  open: boolean
  title: ReactNode
  steps: WizardStep[]
  step: number
  onStep: (i: number) => void
  // done: the last step's next() succeeded.
  onClose: (done: boolean) => void
}

export function Wizard(props: WizardProps) {
  const { open, onClose } = props
  return (
    <Dialog open={open} disablePointerDismissal onOpenChange={(o) => !o && onClose(false)}>
      <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-xl">
        {/* Inside the popup: a fresh start each time it opens. */}
        {open && <Steps {...props} />}
      </DialogContent>
    </Dialog>
  )
}

function Steps({ title, steps, step: i, onStep, onClose }: WizardProps) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const body = useRef<HTMLDivElement>(null)
  const nextRef = useRef<HTMLButtonElement>(null)
  const step = steps[i]
  const b = step.button ?? {}
  const last = i === steps.length - 1
  const disabled = busy || !!b.disabled

  // Entering a step: its first field gets the focus, else Next.
  useEffect(() => {
    const first = body.current?.querySelector<HTMLElement>("input:not([type=radio]):not([type=checkbox]), select, textarea")
    if (first) first.focus()
    else nextRef.current?.focus()
  }, [i])

  // A Next that was disabled can't have had the focus: give it once it's
  // usable, so Enter works on a step without fields.
  useEffect(() => {
    const a = document.activeElement
    if (!disabled && (!a || a === document.body || a.getAttribute("role") === "dialog")) nextRef.current?.focus()
  }, [disabled])

  const go = (n: number) => {
    setError("")
    onStep(n)
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (disabled) return
    setBusy(true)
    setError("")
    let ok: boolean | void
    try {
      ok = step.next ? await step.next() : true
    } catch (err) {
      setError(errorMessage(err))
      ok = false
    }
    setBusy(false)
    if (ok === false) return
    if (last) onClose(true)
    else go(i + 1)
  }

  return (
    // A form, so Enter in a field moves on.
    <form noValidate onSubmit={submit} className="flex flex-col gap-5">
      <DialogHeader>
        <DialogTitle className="text-lg font-semibold">{title}</DialogTitle>
      </DialogHeader>
      <ol aria-label="Steps" className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
        {steps.map((s, k) => (
          <li
            key={s.label}
            aria-current={k === i ? "step" : undefined}
            className={cn("flex items-center gap-2", k === i ? "font-semibold text-foreground" : "text-muted-foreground")}
          >
            {k > 0 && <span aria-hidden className="h-px w-5 bg-border" />}
            <span
              aria-hidden
              className={cn(
                "flex size-6 items-center justify-center rounded-full text-xs font-semibold tabular-nums",
                k < i ? "bg-success-fill text-white" : k === i ? "bg-primary text-primary-foreground" : "bg-muted text-muted-foreground"
              )}
            >
              {k < i ? <CheckIcon className="size-3.5" strokeWidth={3} /> : k + 1}
            </span>
            {s.label}
          </li>
        ))}
      </ol>
      <div ref={body} className="flex flex-col gap-4">
        {step.content}
      </div>
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
      <DialogFooter>
        <Button type="button" variant="tinted" onClick={() => onClose(false)}>
          Cancel
        </Button>
        {i > 0 && (
          <Button type="button" variant="tinted" disabled={busy} onClick={() => go(i - 1)}>
            Back
          </Button>
        )}
        <Button ref={nextRef} type="submit" variant={b.danger ? "destructive-solid" : "default"} disabled={disabled}>
          {b.label || (last ? "Finish" : "Next")}
        </Button>
      </DialogFooter>
    </form>
  )
}
