import { useRef, useState, type ClipboardEvent, type DragEvent, type ReactNode } from "react"
import { ImageIcon, PaperclipIcon, XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Progress } from "@/components/ui/progress"
import { showError } from "@/components/app/toaster"
import { api, upload } from "@/lib/api"
import { fmtBytes } from "@/lib/format"
import { cn } from "@/lib/utils"
import type { Attachment, Limits } from "./shared"

// Attachments: picking, dropping and pasting files onto a message, and
// showing the ones a message has.

// sendForm posts JSON, or multipart (data + files) when there are files,
// with upload progress.
export function sendForm<T>(path: string, data: unknown, files: File[], onProgress?: (fraction: number) => void): Promise<T> {
  if (!files.length) return api<T>("POST", path, data)
  const fd = new FormData()
  fd.append("data", JSON.stringify(data))
  for (const f of files) fd.append("files", f, f.name)
  return upload<T>(path, fd, onProgress)
}

// fileChecker checks picked files against the limits before uploading
// (the server checks again).
function fileChecker(limits: Limits) {
  const exts = new Set((limits.extensions || []).map((e) => e.toLowerCase()))
  return (f: File, count: number) => {
    const ext = f.name.includes(".") ? f.name.split(".").pop()!.toLowerCase() : ""
    if (!limits.max_files) return "Attachments are turned off."
    if (count >= limits.max_files) return `At most ${limits.max_files} files per message.`
    if (!exts.has(ext)) return `${f.name}: this type of file can't be attached (allowed: ${[...exts].join(", ")}).`
    if (f.size > limits.max_file_mb * 1048576) return `${f.name} is larger than ${limits.max_file_mb} MB.`
    if (!f.size) return `${f.name} is empty.`
    return ""
  }
}

const hasFiles = (e: DragEvent) => [...e.dataTransfer.types].includes("Files")

// useAttachments: the pending files of a message, and the handlers that
// make an element a drop (and paste) target.
export function useAttachments(limits: Limits) {
  const [files, setFiles] = useState<File[]>([])
  const [dragging, setDragging] = useState(false)
  const on = limits.max_files > 0

  const add = (picked: File[]) => {
    const check = fileChecker(limits)
    const next = [...files]
    for (const f of picked) {
      const problem = check(f, next.length)
      if (problem) {
        showError(new Error(problem))
        continue
      }
      next.push(f)
    }
    setFiles(next)
  }

  const target = on
    ? {
        onDragOver: (e: DragEvent<HTMLElement>) => {
          if (!hasFiles(e)) return
          e.preventDefault()
          setDragging(true)
        },
        onDragLeave: (e: DragEvent<HTMLElement>) => {
          if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragging(false)
        },
        onDrop: (e: DragEvent<HTMLElement>) => {
          if (!hasFiles(e)) return
          e.preventDefault()
          setDragging(false)
          add([...e.dataTransfer.files])
        },
        // Pasting a screenshot attaches it.
        onPaste: (e: ClipboardEvent<HTMLElement>) => {
          const pasted = [...(e.clipboardData?.files || [])]
          if (!pasted.length) return
          e.preventDefault()
          const stamp = Date.now()
          add(pasted.map((f, i) => (f.name && f.name !== "image.png" ? f : new File([f], `screenshot-${stamp}${i ? "-" + i : ""}.png`, { type: f.type }))))
        },
      }
    : {}

  return {
    files,
    dragging,
    target,
    limits,
    add,
    remove: (i: number) => setFiles((cur) => cur.filter((_, j) => j !== i)),
    reset: () => setFiles([]),
  }
}

export type Attachments = ReturnType<typeof useAttachments>

// AttachBar: the button, the limits, and the files waiting to be sent.
export function AttachBar({ att, disabled }: { att: Attachments; disabled?: boolean }) {
  const picker = useRef<HTMLInputElement>(null)
  const { limits, files } = att
  if (!limits.max_files) return null
  const touch = typeof matchMedia === "function" && matchMedia("(pointer: coarse)").matches
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2.5">
        <Button type="button" variant="tinted" size="sm" disabled={disabled} onClick={() => picker.current?.click()}>
          <PaperclipIcon data-icon="inline-start" />
          Attach files
        </Button>
        <span className="text-xs text-muted-foreground">
          {`${touch ? "" : "or drop them here · "}up to ${limits.max_files} × ${limits.max_file_mb} MB`}
        </span>
        <input
          ref={picker}
          type="file"
          multiple
          hidden
          accept={(limits.extensions || []).map((e) => "." + e).join(",")}
          onChange={(e) => {
            att.add([...(e.target.files || [])])
            e.target.value = ""
          }}
        />
      </div>
      {files.length > 0 && (
        <ul aria-label="Files to attach" className="flex flex-wrap gap-1.5">
          {files.map((f, i) => {
            const Icon = /^image\//.test(f.type) ? ImageIcon : PaperclipIcon
            return (
              <li key={`${f.name}-${f.size}-${i}`} className="inline-flex max-w-80 items-center gap-1.5 rounded-full bg-muted py-0.5 pr-0.5 pl-2.5 text-[0.8125rem]">
                <Icon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
                <span className="min-w-0 truncate">{f.name}</span>
                <span className="shrink-0 text-xs text-muted-foreground">{fmtBytes(f.size)}</span>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  aria-label={`Remove ${f.name}`}
                  disabled={disabled}
                  onClick={() => {
                    att.remove(i)
                    picker.current?.focus()
                  }}
                >
                  <XIcon />
                </Button>
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}

// DropOverlay covers a drop target while files are dragged over it.
export function DropOverlay({ show }: { show: boolean }) {
  if (!show) return null
  return (
    <div
      aria-hidden
      className="pointer-events-none absolute inset-0 z-10 grid place-content-center place-items-center gap-2 rounded-[inherit] border-2 border-dashed border-primary bg-card/85 font-semibold text-link backdrop-blur-sm"
    >
      <PaperclipIcon className="size-7" />
      Drop files to attach them
    </div>
  )
}

export function UploadProgress({ value }: { value: number | null }) {
  if (value == null) return null
  return <Progress value={Math.round(value * 100)} aria-label="Uploading" className="[&_[data-slot=progress-track]]:h-1.5" />
}

// ---- A message's files ----

export function AttachmentList({ ticketId, files, className }: { ticketId: number; files: Attachment[] | null | undefined; className?: string }) {
  if (!files?.length) return null
  return (
    <ul aria-label="Attachments" className={cn("mt-2.5 flex flex-wrap gap-2", className)}>
      {files.map((f) => {
        const url = `/api/v1/tickets/${ticketId}/attachments/${f.id}`
        if (f.image) {
          return (
            <li key={f.id}>
              <a
                href={url + "?inline=1"}
                target="_blank"
                rel="noopener"
                title={`${f.name} · ${fmtBytes(f.size)}`}
                className="block overflow-hidden rounded-xl bg-muted ring-1 ring-border transition-shadow hover:ring-2 hover:ring-primary"
              >
                <img src={url + "?inline=1"} alt={f.name} loading="lazy" className="block h-[92px] w-[132px] object-cover sm:h-28 sm:w-42" />
              </a>
            </li>
          )
        }
        return (
          <li key={f.id}>
            <FileLink href={url} name={f.name} size={f.size} />
          </li>
        )
      })}
    </ul>
  )
}

function FileLink({ href, name, size }: { href: string; name: string; size: number }): ReactNode {
  return (
    <a
      href={href}
      download=""
      className="inline-flex max-w-88 items-center gap-2 rounded-xl bg-card px-3 py-1.5 text-[0.8125rem] text-foreground no-underline ring-1 ring-border transition-shadow hover:no-underline hover:ring-2 hover:ring-primary"
    >
      <PaperclipIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
      <span className="min-w-0 truncate">{name}</span>
      <span className="shrink-0 text-xs text-muted-foreground">{fmtBytes(size)}</span>
    </a>
  )
}
