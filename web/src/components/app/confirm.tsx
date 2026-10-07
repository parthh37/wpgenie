import { useRef, useState, useSyncExternalStore } from "react"
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

// ask() and askText(): confirm() and prompt() in the panel's style, as
// awaitable functions callable from anywhere. One dialog at a time: the
// rest wait their turn.

const DANGER = /\b(delete|remove|terminate|revoke|forget|disable|stop|replace|suspend|turn off|push|restore)\b/i
const VERBS = ["Delete", "Remove", "Move", "Stop", "Restore", "Push", "Switch", "Update", "Turn off", "Forget", "Copy",
  "Issue", "Replace", "Suspend", "Revoke", "Disable"]

interface Field {
  label?: string
  value?: string
  type?: string
  placeholder?: string
  autocomplete?: string
  // The button waits until the text equals this (typing a domain to delete it).
  match?: string
}
interface Request {
  title: string
  body: string
  ok: string
  danger: boolean
  field?: Field
  check?: string
  resolve: (r: { value: string; checked: boolean } | null) => void
}

let queue: Request[] = []
const subs = new Set<() => void>()
const emit = () => subs.forEach((f) => f())

function open(r: Omit<Request, "resolve">) {
  return new Promise<{ value: string; checked: boolean } | null>((resolve) => {
    queue = [...queue, { ...r, resolve }]
    emit()
  })
}

// splitQuestion turns "Move a.com to web-2? Files are copied…" into the
// question (the title) and the explanation around it.
function splitQuestion(message: string) {
  const q = message.indexOf("?")
  if (q < 0) return { title: "Are you sure?", body: message }
  const dot = message.lastIndexOf(". ", q)
  const nl = message.lastIndexOf("\n", q)
  const start = Math.max(dot < 0 ? 0 : dot + 2, nl + 1)
  return { title: message.slice(start, q + 1), body: (message.slice(0, start) + " " + message.slice(q + 1)).trim() }
}

export interface AskOptions {
  title?: string
  ok?: string
  danger?: boolean
}

// ask(message) is an awaitable confirm(). The question in the message is
// the title; its first word the button (Delete, Move…) when it's a verb.
export async function ask(message: string, opts: AskOptions = {}) {
  const { title, body } = opts.title ? { title: opts.title, body: message } : splitQuestion(message)
  const verb = VERBS.find((v) => title.startsWith(v + " "))
  const res = await open({ title, body, ok: opts.ok || verb || "Continue", danger: opts.danger ?? DANGER.test(title) })
  return res !== null
}

export interface AskTextOptions extends AskOptions, Field {
  label?: string
  check?: string
}

// askText(message, opts) is an awaitable prompt(): the text, or null if
// cancelled. With match, OK is enabled once the text equals it.
export async function askText(message: string, opts: AskTextOptions = {}): Promise<string | null> {
  const res = await open({
    title: opts.title || "Confirm",
    body: message,
    ok: opts.ok || "OK",
    danger: opts.danger ?? (opts.match != null || DANGER.test(opts.title || "")),
    check: opts.check,
    field: opts,
  })
  return res ? res.value : null
}

// askTextCheck: askText with a checkbox; {value, checked} or null.
export async function askTextCheck(message: string, opts: AskTextOptions & { check: string }) {
  return open({
    title: opts.title || "Confirm",
    body: message,
    ok: opts.ok || "OK",
    danger: opts.danger ?? (opts.match != null || DANGER.test(opts.title || "")),
    check: opts.check,
    field: opts,
  })
}

let seq = 0
const ids = new WeakMap<Request, number>()
const idOf = (r: Request) => {
  if (!ids.has(r)) ids.set(r, ++seq)
  return ids.get(r)!
}

export function ConfirmHost() {
  const current = useSyncExternalStore(
    (f) => (subs.add(f), () => void subs.delete(f)),
    () => queue[0]
  )
  if (!current) return null
  return <Ask key={idOf(current)} current={current} />
}

function Ask({ current }: { current: Request }) {
  const [value, setValue] = useState(current.field?.value || "")
  const [checked, setChecked] = useState(false)
  const [openState, setOpenState] = useState(true)
  const inputRef = useRef<HTMLInputElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const okRef = useRef<HTMLButtonElement>(null)
  const field = current.field
  const blocked = !!field && field.match != null && value.trim() !== field.match

  const finish = (ok: boolean) => {
    if (ok && blocked) return
    const v = field && field.type !== "password" ? value.trim() : value
    setValue("") // passwords don't linger in the page
    setOpenState(false)
    current.resolve(ok ? { value: v, checked } : null)
    // After the closing animation, the next one.
    setTimeout(() => {
      queue = queue.slice(1)
      emit()
    }, 150)
  }

  return (
    <AlertDialog open={openState} onOpenChange={(o) => !o && finish(false)}>
      <AlertDialogContent
        initialFocus={field ? inputRef : current.danger ? cancelRef : okRef}
        className="sm:max-w-md"
      >
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            finish(true)
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{current.title}</AlertDialogTitle>
            {current.body && (
              <AlertDialogDescription render={<div />} className="space-y-2">
                {current.body
                  .split(/\n+/)
                  .filter(Boolean)
                  .map((p, i) => (
                    <p key={i}>{p}</p>
                  ))}
              </AlertDialogDescription>
            )}
          </AlertDialogHeader>
          {field && (
            <div className="grid gap-2">
              {field.label && <Label htmlFor="ask-field">{field.label}</Label>}
              <Input
                id="ask-field"
                ref={inputRef}
                type={field.type || "text"}
                value={value}
                placeholder={field.placeholder}
                autoComplete={field.autocomplete || "off"}
                onChange={(e) => setValue(e.target.value)}
              />
            </div>
          )}
          {current.check && (
            <Label className="font-normal">
              <Checkbox checked={checked} onCheckedChange={(c) => setChecked(!!c)} />
              {current.check}
            </Label>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel ref={cancelRef} type="button">
              Cancel
            </AlertDialogCancel>
            <Button ref={okRef} type="submit" variant={current.danger ? "destructive-solid" : "default"} disabled={blocked}>
              {current.ok}
            </Button>
          </AlertDialogFooter>
        </form>
      </AlertDialogContent>
    </AlertDialog>
  )
}
