import { useId, useState, type FormEvent, type ReactNode } from "react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { ApiError } from "@/lib/api"
import { friendly } from "@/lib/money"
import { cn } from "@/lib/utils"

// Billing's dialogs with forms (the legacy openDialog). BillingDialog is
// the modal; its content (a component holding the form's state) mounts
// afresh each time it opens. DialogForm is the form inside: errors show in
// the dialog, next to their field when the API (or fieldError) names one,
// rather than in a toast behind the backdrop.

export function BillingDialog({
  open,
  onOpenChange,
  wide,
  children,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  wide?: boolean
  children: ReactNode
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className={cn("max-h-[calc(100svh-2rem)] overflow-y-auto", wide ? "sm:max-w-3xl" : "sm:max-w-lg")}>
        {children}
      </DialogContent>
    </Dialog>
  )
}

// fieldError is an error a dialog shows next to its field.
export const fieldError = (name: string, message: string) => new ApiError(message, 400, { field: name })

export function DialogForm({
  title,
  intro,
  ok = "Save",
  cancel = "Cancel",
  danger,
  noOk,
  okDisabled,
  onSubmit,
  onClose,
  children,
}: {
  title: ReactNode
  intro?: ReactNode
  ok?: ReactNode
  cancel?: ReactNode
  danger?: boolean
  noOk?: boolean
  okDisabled?: boolean
  // Resolve false to stay open.
  onSubmit?: (form: HTMLFormElement) => unknown | Promise<unknown>
  onClose: () => void
  children?: ReactNode
}) {
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const errId = useId()

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    e.stopPropagation()
    if (noOk || busy || okDisabled) return
    const form = e.currentTarget
    if (!form.reportValidity()) return
    setError("")
    form.querySelectorAll("[aria-invalid]").forEach((x) => x.removeAttribute("aria-invalid"))
    setBusy(true)
    try {
      const r = onSubmit ? await onSubmit(form) : true
      if (r !== false) onClose()
    } catch (ex) {
      const err = friendly(ex)
      setError(String((err instanceof Error && err.message) || err))
      const name = err instanceof ApiError ? (err.data.field as string | undefined) : undefined
      const target = name ? form.elements.namedItem(name) : null
      if (target instanceof HTMLElement) {
        target.setAttribute("aria-invalid", "true")
        target.setAttribute("aria-errormessage", errId)
        target.focus()
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="flex min-w-0 flex-col gap-5">
      <DialogHeader>
        <DialogTitle className="text-lg font-semibold">{title}</DialogTitle>
        {intro && <DialogDescription>{intro}</DialogDescription>}
      </DialogHeader>
      {children}
      {error && (
        <p id={errId} role="alert" className="rounded-xl bg-danger-fill/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      <DialogFooter>
        <Button type="button" variant="tinted" onClick={onClose}>
          {cancel}
        </Button>
        {!noOk && (
          <Button type="submit" variant={danger ? "destructive-solid" : "default"} disabled={busy || okDisabled}>
            {ok}
          </Button>
        )}
      </DialogFooter>
    </form>
  )
}

// LabeledField is a labelled control with optional help, announced with
// it. children gets the id and aria-describedby to put on the control.
export function LabeledField({
  label,
  help,
  className,
  children,
}: {
  label: ReactNode
  help?: ReactNode
  className?: string
  children: (a11y: { id: string; "aria-describedby"?: string }) => ReactNode
}) {
  const id = useId()
  const helpId = help ? id + "-help" : undefined
  return (
    <Field className={cn("gap-1.5", className)}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      {children({ id, "aria-describedby": helpId })}
      {help && <FieldDescription id={helpId}>{help}</FieldDescription>}
    </Field>
  )
}

// Hint is a line under a form that follows what's typed.
export function Hint({ children }: { children?: ReactNode }) {
  return (
    <p aria-live="polite" className="min-h-5 text-sm text-muted-foreground">
      {children}
    </p>
  )
}
