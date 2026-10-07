import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react"
import { CheckIcon, CopyIcon, RefreshCwIcon, TriangleAlertIcon, type LucideIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { InputGroup, InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"
import { Table, TableBody, TableCaption, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { IconTile, type Tint } from "@/components/app/icon-tile"
import { notify, showError } from "@/components/app/toaster"
import { ApiError } from "@/lib/api"
import { fmtNum } from "@/lib/format"
import { currencySymbol, friendly, fromMinor } from "@/lib/money"
import { href as hashHref } from "@/lib/router"
import { cn } from "@/lib/utils"

// Building blocks shared by the bigger screens (billing, accounts, support,
// logs): the legacy billing.js helpers, as components.

// ---- Kpi: a number card; with onClick or href it goes somewhere ----

export function Kpi({
  label,
  value,
  sub,
  tone,
  icon: Icon,
  onClick,
  href,
  className,
}: {
  label: ReactNode
  value: ReactNode
  sub?: ReactNode
  tone?: "ok" | "warn" | "bad"
  icon?: LucideIcon
  onClick?: () => void
  href?: string
  className?: string
}) {
  const cls = cn(
    "flex min-w-0 flex-col gap-1 rounded-2xl bg-card p-4 text-left text-foreground no-underline card-shadow",
    (onClick || href) && "cursor-pointer transition-transform hover:-translate-y-0.5 hover:no-underline active:scale-[.98]",
    tone === "bad" && "bg-danger-fill/10",
    tone === "warn" && "bg-warning-fill/10",
    tone === "ok" && "bg-success-fill/10",
    className
  )
  const body = (
    <>
      <span className="flex items-center gap-1.5 text-xs font-semibold tracking-wide text-muted-foreground uppercase">
        {Icon && <Icon className="size-3.5" />}
        {label}
      </span>
      <span
        className={cn(
          "font-heading text-[1.625rem] leading-tight font-bold tracking-[-0.02em] tabular-nums",
          tone === "bad" && "text-danger",
          tone === "warn" && "text-warning"
        )}
      >
        {value}
      </span>
      {sub && <span className="text-xs text-muted-foreground">{sub}</span>}
    </>
  )
  if (href) return <a href={hashHref(href)} className={cls}>{body}</a>
  if (onClick) return <button type="button" onClick={onClick} className={cls}>{body}</button>
  return <div className={cls}>{body}</div>
}

// ---- Banner: a notice across a screen ----

export function Banner({
  tone = "info",
  icon: Icon,
  title,
  children,
  actions,
  className,
}: {
  tone?: "info" | "ok" | "warn" | "bad"
  icon?: LucideIcon
  title: ReactNode
  children?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  const tint: Tint = tone === "ok" ? "green" : tone === "warn" ? "orange" : tone === "bad" ? "red" : "indigo"
  return (
    <div
      role={tone === "bad" ? "alert" : "status"}
      className={cn(
        "mb-4 flex flex-wrap items-center gap-3.5 rounded-2xl p-4",
        tone === "info" && "bg-primary/10",
        tone === "ok" && "bg-success-fill/12",
        tone === "warn" && "bg-warning-fill/12",
        tone === "bad" && "bg-danger-fill/12",
        className
      )}
    >
      {Icon && <IconTile icon={Icon} tint={tint} size="lg" />}
      <div className="min-w-0 flex-1">
        <strong className="font-semibold">{title}</strong>
        {children && <div className="text-sm text-muted-foreground">{children}</div>}
      </div>
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </div>
  )
}

// ---- SubNav: the sections within a page, as links ----

export interface SubNavItem {
  key: string
  label: ReactNode
  href: string // a path ("/billing/invoices")
  icon?: LucideIcon
  count?: number
}

export function SubNav({ label, items, current, className }: { label: string; items: SubNavItem[]; current: string; className?: string }) {
  const ref = useRef<HTMLElement>(null)
  useEffect(() => {
    ref.current?.querySelector("[aria-current]")?.scrollIntoView({ block: "nearest", inline: "nearest" })
  }, [current])
  return (
    <nav
      ref={ref}
      aria-label={label}
      className={cn("-mx-4 mb-5 flex gap-1 overflow-x-auto px-4 pb-1 sm:mx-0 sm:px-0", className)}
    >
      <div className="inline-flex gap-1 rounded-full bg-muted p-1">
        {items.map((it) => (
          <a
            key={it.key}
            href={hashHref(it.href)}
            aria-current={it.key === current ? "page" : undefined}
            className={cn(
              "inline-flex shrink-0 items-center gap-1.5 rounded-full px-3.5 py-1.5 text-sm font-medium whitespace-nowrap text-muted-foreground no-underline transition-colors hover:text-foreground hover:no-underline",
              it.key === current && "bg-card text-foreground shadow-sm"
            )}
          >
            {it.icon && <it.icon className="size-4" />}
            {it.label}
            {it.count ? (
              <span className="rounded-full bg-primary px-1.5 text-xs font-semibold text-primary-foreground tabular-nums">{fmtNum(it.count)}</span>
            ) : null}
          </a>
        ))}
      </div>
    </nav>
  )
}

// ---- Chips: a filter, one pressed at a time, with counts when known ----

export function Chips<K extends string>({
  label,
  items,
  current,
  onPick,
  className,
}: {
  label: string
  items: Array<[key: K, text: ReactNode, count?: number | null]>
  current: K
  onPick: (k: K) => void
  className?: string
}) {
  return (
    <div role="group" aria-label={label} className={cn("flex flex-wrap gap-1.5", className)}>
      {items.map(([key, text, count]) => (
        <button
          key={key}
          type="button"
          aria-pressed={key === current}
          onClick={() => onPick(key)}
          className={cn(
            "inline-flex h-8 items-center gap-1.5 rounded-full bg-secondary px-3 text-sm font-medium text-foreground transition-colors hover:bg-accent",
            key === current && "bg-primary text-primary-foreground hover:bg-primary"
          )}
        >
          {text}
          {count != null && <span className={cn("tabular-nums", key === current ? "opacity-80" : "text-muted-foreground")}>{fmtNum(count)}</span>}
        </button>
      ))}
    </div>
  )
}

// ---- Empty and failed states ----

export function EmptyState({
  icon,
  tint = "gray",
  title,
  children,
  actions,
  className,
}: {
  icon: LucideIcon
  tint?: Tint
  title: ReactNode
  children?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <Empty className={cn("rounded-2xl bg-card py-12 card-shadow", className)}>
      <EmptyHeader>
        <EmptyMedia>
          <IconTile icon={icon} tint={tint} size="xl" />
        </EmptyMedia>
        <EmptyTitle className="text-lg">{title}</EmptyTitle>
        {children && <EmptyDescription>{children}</EmptyDescription>}
      </EmptyHeader>
      {actions && <EmptyContent className="flex-row flex-wrap justify-center">{actions}</EmptyContent>}
    </Empty>
  )
}

// LoadError replaces a screen that couldn't load. A 404 is a server
// without that part (yet): said plainly, not as a failure.
export function LoadError({ error, retry, className }: { error: unknown; retry?: () => void; className?: string }) {
  const missing = error instanceof ApiError && error.status === 404
  return (
    <Empty role="alert" className={cn("rounded-2xl bg-card py-12 card-shadow", className)}>
      <EmptyHeader>
        <EmptyMedia>
          <IconTile icon={TriangleAlertIcon} tint="red" size="xl" />
        </EmptyMedia>
        <EmptyTitle className="text-lg">{missing ? "Not available on this server yet" : "This didn't load"}</EmptyTitle>
        <EmptyDescription>
          {missing
            ? "This server doesn't have this part yet (it may need an update). Everything else keeps working."
            : String((error instanceof Error && error.message) || error)}
        </EmptyDescription>
      </EmptyHeader>
      {retry && (
        <EmptyContent>
          <Button variant="tinted" onClick={retry}>
            <RefreshCwIcon data-icon="inline-start" />
            Try again
          </Button>
        </EmptyContent>
      )}
    </Empty>
  )
}

// ---- ActionButton: runs an action, disabled while it runs ----
// Failures are toasts; a cancelled confirmation (the action returning
// early) is not a failure.

export function ActionButton({
  run,
  children,
  variant = "tinted",
  size,
  disabled,
  className,
  title,
}: {
  run: () => unknown | Promise<unknown>
  children: ReactNode
  variant?: React.ComponentProps<typeof Button>["variant"]
  size?: React.ComponentProps<typeof Button>["size"]
  disabled?: boolean
  className?: string
  title?: string
}) {
  const [busy, setBusy] = useState(false)
  return (
    <Button
      type="button"
      variant={variant}
      size={size}
      title={title}
      className={className}
      disabled={disabled || busy}
      onClick={async () => {
        setBusy(true)
        try {
          await run()
        } catch (e) {
          showError(friendly(e))
        } finally {
          setBusy(false)
        }
      }}
    >
      {children}
    </Button>
  )
}

// ---- CopyField: a value in code with a Copy button ----

export function CopyField({ text, className }: { text: string; className?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className={cn("flex min-w-0 items-center gap-2", className)}>
      <code className="min-w-0 flex-1 truncate py-1.5">{text}</code>
      <Button
        type="button"
        variant="tinted"
        size="sm"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(text)
            setCopied(true)
            notify("Copied to the clipboard")
            setTimeout(() => setCopied(false), 1500)
          } catch {
            showError(new Error("Copying didn't work here: select the text and copy it."))
          }
        }}
      >
        {copied ? <CheckIcon data-icon="inline-start" /> : <CopyIcon data-icon="inline-start" />}
        Copy
      </Button>
    </div>
  )
}

// ---- Amount and percentage inputs (uncontrolled, for forms) ----
// MoneyInput shows minor units as "118.00" with the currency's symbol;
// read it back with toMinor(form.get(name)). PercentInput shows a rate
// (hundredths of a percent) as "18"; read it back with toRate().

export function MoneyInput({ name, minor, id, ...rest }: { name: string; minor?: number | null; id?: string } & Omit<React.ComponentProps<"input">, "name" | "defaultValue">) {
  return (
    <InputGroup>
      <InputGroupAddon>
        <InputGroupText>{currencySymbol()}</InputGroupText>
      </InputGroupAddon>
      <InputGroupInput id={id} name={name} inputMode="decimal" autoComplete="off" defaultValue={fromMinor(minor ?? "")} placeholder={fromMinor(0)} {...rest} />
    </InputGroup>
  )
}

export function PercentInput({ name, rate, id, ...rest }: { name: string; rate?: number | null; id?: string } & Omit<React.ComponentProps<"input">, "name" | "defaultValue">) {
  return (
    <InputGroup>
      <InputGroupInput id={id} name={name} inputMode="decimal" autoComplete="off" defaultValue={rate == null ? "" : String(rate / 100)} {...rest} />
      <InputGroupAddon align="inline-end">
        <InputGroupText>%</InputGroupText>
      </InputGroupAddon>
    </InputGroup>
  )
}

// ---- ChoiceCard: a radio as a card (a title and a line under it) ----
// Plain radios in a group: <div role="radiogroup">…</div>, or a <fieldset>.

export function ChoiceCard({
  name,
  value,
  title,
  children,
  checked,
  defaultChecked,
  onChange,
  disabled,
  extra,
}: {
  name: string
  value: string
  title: ReactNode
  children?: ReactNode
  checked?: boolean
  defaultChecked?: boolean
  onChange?: (value: string) => void
  disabled?: boolean
  extra?: ReactNode
}) {
  return (
    <label
      className={cn(
        "flex cursor-pointer items-start gap-3 rounded-2xl bg-card p-3.5 ring-1 ring-border transition-colors has-checked:bg-primary/8 has-checked:ring-2 has-checked:ring-primary has-disabled:cursor-not-allowed has-disabled:opacity-55"
      )}
    >
      <input
        type="radio"
        name={name}
        value={value}
        checked={checked}
        defaultChecked={defaultChecked}
        disabled={disabled}
        onChange={(e) => e.target.checked && onChange?.(value)}
        className="mt-0.5 size-[18px] shrink-0 accent-primary"
      />
      <span className="flex min-w-0 flex-col gap-0.5">
        <strong className="text-sm font-semibold">{title}</strong>
        {children && <span className="text-sm text-muted-foreground">{children}</span>}
        {extra}
      </span>
    </label>
  )
}

// ---- FormDialog: a form in a modal ----
// onSubmit(data, form) runs when the form is valid; resolve false to stay
// open. Errors show inside the dialog, next to their field when the API
// names one ({field}), rather than in a toast behind the backdrop.

interface FormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: ReactNode
  intro?: ReactNode
  ok?: ReactNode
  cancel?: ReactNode
  danger?: boolean
  wide?: boolean
  noOk?: boolean
  onSubmit?: (data: FormData, form: HTMLFormElement) => unknown | Promise<unknown>
  children?: ReactNode
  okDisabled?: boolean
}

export function FormDialog(props: FormDialogProps) {
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className={cn("max-h-[calc(100svh-2rem)] overflow-y-auto", props.wide ? "sm:max-w-3xl" : "sm:max-w-lg")}>
        {/* Inside the popup: its state (the error) starts afresh each time it opens. */}
        <FormDialogBody {...props} />
      </DialogContent>
    </Dialog>
  )
}

function FormDialogBody({ onOpenChange, title, intro, ok = "Save", cancel = "Cancel", danger, noOk, onSubmit, children, okDisabled }: FormDialogProps) {
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
      const r = onSubmit ? await onSubmit(new FormData(form), form) : true
      if (r !== false) onOpenChange(false)
    } catch (ex) {
      const err = friendly(ex)
      setError(String((err instanceof Error && err.message) || err))
      const name = err instanceof ApiError ? (err.data.field as string | undefined) : undefined
      const target = name ? (form.elements.namedItem(name) as HTMLElement | null) : null
      if (target && "focus" in target) {
        target.setAttribute("aria-invalid", "true")
        target.setAttribute("aria-errormessage", errId)
        target.focus()
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-5">
      <DialogHeader>
        <DialogTitle>{title}</DialogTitle>
        {intro && <DialogDescription>{intro}</DialogDescription>}
      </DialogHeader>
      {children}
      {error && (
        <p id={errId} role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
      <DialogFooter>
        <Button type="button" variant="tinted" onClick={() => onOpenChange(false)}>
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

// ---- BTable: numeric columns right-aligned, rows that open something ----
// The first cell should hold a link so the keyboard gets there too.

export interface BColumn {
  label: ReactNode
  num?: boolean
  className?: string
}
export interface BRow {
  key?: string | number
  cells: ReactNode[]
  onOpen?: () => void
  className?: string
}

export function BTable({ cols, rows, empty, caption, className }: { cols: Array<BColumn | string>; rows: BRow[]; empty?: ReactNode; caption?: string; className?: string }) {
  if (!rows.length) return <>{empty ?? <p className="py-2 text-sm text-muted-foreground">Nothing here yet.</p>}</>
  const c = cols.map((x) => (typeof x === "string" ? { label: x } : x))
  return (
    <Table className={className}>
      {caption && <TableCaption className="sr-only">{caption}</TableCaption>}
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          {c.map((col, i) => (
            <TableHead key={i} scope="col" className={cn(col.num && "text-right", col.className)}>
              {col.label}
            </TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((r, i) => (
          <TableRow
            key={r.key ?? i}
            className={cn(r.onOpen && "cursor-pointer", r.className)}
            onClick={
              r.onOpen
                ? (e) => {
                    if (!(e.target as HTMLElement).closest("a, button, input, select, label, summary")) r.onOpen!()
                  }
                : undefined
            }
          >
            {r.cells.map((cell, j) => (
              <TableCell key={j} className={cn("align-top", c[j]?.num && "text-right tabular-nums")}>
                {cell}
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
