import { useEffect, useRef, useState, useSyncExternalStore } from "react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "@/components/ui/dialog"
import { Kbd } from "@/components/ui/kbd"
import { ask } from "@/components/app/confirm"
import { showError } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import { baseName, filesPath, putFile, type TextFile } from "./lib"

// The text editor. Saves carry the version that was read: if the file
// changed in between (WordPress, SFTP, someone else here), the save stops
// and asks before overwriting.

interface Open {
  site: Site
  path: string
  file: TextFile
  readOnly: boolean
  onSaved?: () => unknown
  seq: number
}

let current: Open | null = null
let seq = 0
// The open editor's close (asks first when there are unsaved changes).
let requestClose: (() => void) | null = null
const subs = new Set<() => void>()
const set = (v: Open | null) => {
  current = v
  subs.forEach((f) => f())
}

// openEditor reads a text file and opens it (throws if it can't be read:
// too large, gone, not text).
export async function openEditor(site: Site, path: string, opts: { canWrite: boolean; onSaved?: () => unknown }) {
  const file = await api<TextFile>("GET", filesPath(site, "/content", { path }))
  const readOnly = !file.writable || !opts.canWrite || site.status !== "active"
  set({ site, path, file, readOnly, onSaved: opts.onSaved, seq: ++seq })
}

export function EditorHost() {
  const cur = useSyncExternalStore(
    (f) => (subs.add(f), () => void subs.delete(f)),
    () => current
  )
  // Leaving the section closes the editor.
  useEffect(() => () => set(null), [])
  return (
    <Dialog
      open={!!cur}
      disablePointerDismissal
      onOpenChange={(o) => {
        // Closing is handled inside, where the unsaved changes are known.
        if (!o) requestClose?.()
      }}
    >
      <DialogContent showCloseButton={false} className="flex h-[calc(100svh-2rem)] flex-col gap-4 sm:max-w-5xl">
        {cur && <Editor key={cur.seq} cur={cur} />}
      </DialogContent>
    </Dialog>
  )
}

function Editor({ cur }: { cur: Open }) {
  const { site, path, file, readOnly } = cur
  const ta = useRef<HTMLTextAreaElement>(null)
  // A text box's value always has \n line endings: files written with \r\n
  // get theirs back on save.
  const crlf = file.content.includes("\r\n")
  const initial = file.content.replace(/\r\n/g, "\n")
  const saved = useRef(initial)
  const version = useRef(file.version)
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const asking = useRef(false)

  const show = () => setDirty(!!ta.current && ta.current.value !== saved.current)
  const state = readOnly ? (file.writable ? "read-only" : `read-only (${file.mode})`) : saving ? "saving…" : dirty ? "unsaved changes" : "saved"

  useEffect(() => {
    const t = ta.current
    if (!t) return
    t.focus()
    t.setSelectionRange(0, 0)
    t.scrollTop = 0
  }, [])

  const doSave = async (overwrite: boolean): Promise<void> => {
    const t = ta.current
    if (!t || readOnly) return
    setSaving(true)
    const text = t.value
    try {
      const body = crlf ? text.replace(/\n/g, "\r\n") : text
      const r = await putFile(site, path, new Blob([body]), overwrite ? { overwrite: 1 } : { version: version.current })
      version.current = r.version ?? version.current
      saved.current = text
      setSaving(false)
      show()
      if (cur.onSaved) Promise.resolve(cur.onSaved()).catch(() => {})
    } catch (e) {
      setSaving(false)
      show()
      if (e instanceof ApiError && e.status === 409 && !overwrite) {
        if (
          await ask(
            `Overwrite the newer version? ${path} changed since you opened it (WordPress, SFTP or someone else). ` +
              "Saving replaces those changes with yours; Cancel keeps editing (copy your changes out, then reopen the file).",
            { ok: "Overwrite", danger: true }
          )
        )
          return doSave(true)
        return
      }
      showError(e)
    }
  }

  // Closing (Close, Escape) with unsaved changes asks first.
  const close = async () => {
    if (asking.current) return
    if (!readOnly && ta.current && ta.current.value !== saved.current) {
      asking.current = true
      const ok = await ask(`Discard unsaved changes? Your edits to ${baseName(path)} aren't saved.`, { ok: "Discard", danger: true })
      asking.current = false
      if (!ok) {
        ta.current?.focus()
        return
      }
    }
    if (ta.current) ta.current.value = ""
    set(null)
  }

  // The dialog's own dismissals (Escape) come here.
  useEffect(() => {
    requestClose = close
    return () => {
      if (requestClose === close) requestClose = null
    }
  })

  // A page being left with unsaved edits asks the browser's question.
  useEffect(() => {
    if (!dirty || readOnly) return
    const on = (e: BeforeUnloadEvent) => e.preventDefault()
    addEventListener("beforeunload", on)
    return () => removeEventListener("beforeunload", on)
  }, [dirty, readOnly])

  const onKeyDown = (e: React.KeyboardEvent<HTMLElement>) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
      e.preventDefault()
      if (!readOnly && !saving) doSave(false)
    } else if (e.key === "Tab" && e.target === ta.current && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey && !readOnly) {
      e.preventDefault()
      const t = ta.current!
      t.setRangeText("\t", t.selectionStart, t.selectionEnd, "end")
      show()
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4" onKeyDown={onKeyDown}>
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 pr-2">
        <DialogTitle id="editor-title" className="min-w-0 font-mono text-[0.9375rem] font-semibold [overflow-wrap:anywhere]">
          {path}
        </DialogTitle>
        <DialogDescription
          aria-live="polite"
          className={cn("text-xs", !readOnly && dirty && !saving && "font-medium text-warning")}
        >
          {state}
        </DialogDescription>
      </div>
      <textarea
        ref={ta}
        defaultValue={initial}
        readOnly={readOnly}
        spellCheck={false}
        autoComplete="off"
        autoCapitalize="off"
        wrap="off"
        aria-labelledby="editor-title"
        onInput={show}
        className="min-h-0 flex-1 resize-none rounded-xl border border-input bg-input/30 p-3 font-mono text-[0.8125rem] leading-relaxed [tab-size:4] outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 read-only:bg-muted/50"
      />
      <DialogFooter className="items-center sm:justify-between">
        <span className="hidden items-center gap-1 text-xs text-muted-foreground sm:inline-flex">
          {!readOnly && (
            <>
              <Kbd>Ctrl</Kbd>/<Kbd>⌘</Kbd> <Kbd>S</Kbd> saves · <Kbd>Tab</Kbd> indents
            </>
          )}
        </span>
        <div className="flex flex-col-reverse gap-2 sm:flex-row">
          <Button type="button" variant="tinted" onClick={close}>
            Close
          </Button>
          {!readOnly && (
            <Button type="button" onClick={() => doSave(false)} disabled={saving}>
              Save
            </Button>
          )}
        </div>
      </DialogFooter>
    </div>
  )
}
