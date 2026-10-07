import { Fragment, useEffect, useRef, useState, type DragEvent, type ReactNode } from "react"
import { keepPreviousData, useQuery } from "@tanstack/react-query"
import {
  ArrowUpIcon, CopyIcon, DownloadIcon, EllipsisIcon, EyeIcon, FileArchiveIcon, FileIcon, FilePlusIcon, FolderIcon, FolderPlusIcon,
  KeyRoundIcon, LinkIcon, PackageOpenIcon, PencilIcon, RefreshCwIcon, Trash2Icon, UploadIcon, type LucideIcon,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import {
  Breadcrumb, BreadcrumbItem, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import { ContextMenu, ContextMenuContent, ContextMenuItem, ContextMenuSeparator, ContextMenuTrigger } from "@/components/ui/context-menu"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { LoadError } from "@/components/app/blocks"
import { ask, askText } from "@/components/app/confirm"
import { notify, showError } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import { fmtBytes, fmtTime } from "@/lib/format"
import { invalidate, queryClient } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import type { SectionProps } from "../sections"
import { fileAction, type FileAct } from "./files/actions"
import { EditorHost, openEditor } from "./files/editor"
import {
  FILE_DIR, IMAGE_FILE, TEXT_FILE, ZIP_FILE, download, filesPath, joinPath, listPath, listPrefix, parentDir, preview, putFile, resolvePath,
  type Entry, type Listing,
} from "./files/lib"

// File manager: browse a site's files, upload (drop them on the list), edit
// text files, download files and folders (as zip), rename, copy, change
// permissions, extract zip archives and delete.

export default function FilesSection({ site }: SectionProps) {
  const s = useSession()
  const [dir, setDir] = useState(() => FILE_DIR.get(site.id) || "/")
  // The folder shown stays up while the next one loads.
  const key = listPath(site, dir)
  const q = useQuery<Listing>({ queryKey: [key], queryFn: () => api<Listing>("GET", key), placeholderData: keepPreviousData })
  const list = q.data
  const settled = !!list && !q.isPlaceholderData

  // The folder shown is remembered for the next visit, in the server's own
  // spelling.
  useEffect(() => {
    if (settled && list) FILE_DIR.set(site.id, list.path)
  }, [settled, list, site.id])

  // The folder went away (deleted, renamed elsewhere): back to the top.
  const gone = q.error instanceof ApiError && q.error.status === 404 && dir !== "/"
  if (gone) {
    FILE_DIR.delete(site.id)
    setDir("/")
  }

  // A plan without the file manager: say so here rather than as an error.
  if (q.error instanceof ApiError && q.error.status === 403) return <p className="text-sm text-muted-foreground">{q.error.message}</p>
  if (q.error && !gone) return <LoadError error={q.error} retry={() => q.refetch()} />
  if (!list || gone) {
    return (
      <div className="flex flex-col gap-3">
        <Skeleton className="h-10 rounded-2xl" />
        <Skeleton className="h-72 rounded-2xl" />
      </div>
    )
  }
  return (
    <>
      <Browser site={site} list={list} loading={q.isFetching} canWrite={s.canChange} go={setDir} />
      <EditorHost />
    </>
  )
}

interface Upload {
  id: number
  name: string
  size: number
  progress: number
}
let uploadSeq = 0

function Browser({ site, list, loading, canWrite, go }: { site: Site; list: Listing; loading: boolean; canWrite: boolean; go: (dir: string) => void }) {
  const dir = list.path
  const writable = canWrite && list.writable && site.status === "active"
  const reload = () => invalidate(listPrefix(site))
  const picker = useRef<HTMLInputElement>(null)
  const [uploads, setUploads] = useState<Upload[]>([])
  const [over, setOver] = useState(false)

  const run = (fn: () => unknown) => async () => {
    try {
      await fn()
    } catch (e) {
      showError(e)
    }
  }

  // uploadAll uploads files into this folder one after the other, asking
  // once before replacing any that exist.
  async function uploadAll(files: File[]) {
    if (!files.length) return
    const names = new Set(list.entries.map((x) => x.name))
    const clash = files.filter((f) => names.has(f.name))
    let replace = false
    if (clash.length) {
      const shown = clash.slice(0, 5).map((f) => f.name).join(", ") + (clash.length > 5 ? ` and ${clash.length - 5} more` : "")
      const question =
        clash.length === 1 ? `Replace ${clash[0].name}? It already exists in ${dir}.` : `Replace ${clash.length} files that already exist? ${shown}.`
      replace = await ask(`${question} ${files.length > clash.length ? "Cancel uploads only the new ones." : ""}`.trim(), { ok: "Replace", danger: true })
    }
    let ok = 0
    for (const f of files) {
      if (names.has(f.name) && !replace) continue
      const id = ++uploadSeq
      setUploads((u) => [...u, { id, name: f.name, size: f.size, progress: 0 }])
      try {
        await putFile(site, joinPath(dir, f.name), f, { overwrite: names.has(f.name) ? 1 : null }, (v) =>
          setUploads((u) => u.map((x) => (x.id === id ? { ...x, progress: v } : x)))
        )
        ok++
      } catch (e) {
        showError(e)
      }
      setUploads((u) => u.filter((x) => x.id !== id))
    }
    if (ok) notify(`Uploaded ${ok === 1 ? files.find((f) => !names.has(f.name) || replace)!.name : ok + " files"} to ${dir}`)
    await reload()
  }

  const newFile = run(async () => {
    const name = await askText(`In ${dir}. A path like inc/x.php creates it in that (existing) folder.`, {
      title: "New file",
      label: "Name",
      placeholder: "example.php",
      ok: "Create",
    })
    if (!name) return
    const path = resolvePath(dir, name)
    await putFile(site, path, new Blob([]))
    await reload()
    await openEditor(site, path, { canWrite, onSaved: reload })
  })
  const newFolder = run(async () => {
    const name = await askText(`In ${dir}.`, { title: "New folder", label: "Name", ok: "Create" })
    if (!name) return
    await api("POST", filesPath(site, "/folder", { path: resolvePath(dir, name) }))
    await reload()
  })

  // Dropped files upload into the folder shown. (Folders: zip them, upload
  // the archive and extract it.)
  const dropProps = writable
    ? {
        onDragOver: (e: DragEvent) => {
          if ([...e.dataTransfer.types].includes("Files")) {
            e.preventDefault()
            setOver(true)
          }
        },
        onDragLeave: (e: DragEvent) => {
          if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOver(false)
        },
        onDrop: (e: DragEvent) => {
          e.preventDefault()
          setOver(false)
          const items = [...(e.dataTransfer.items || [])]
          const isDir = (i: number) => !!items[i]?.webkitGetAsEntry?.()?.isDirectory
          if (items.some((_, i) => isDir(i))) {
            showError(new Error("Folders can't be dropped: zip the folder, upload the archive, then choose Extract."))
          }
          uploadAll([...e.dataTransfer.files].filter((_, i) => !isDir(i)))
        },
      }
    : {}

  const parts = dir.split("/").filter(Boolean)

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <Breadcrumb aria-label="Folder" className="min-w-0 flex-1">
          <BreadcrumbList className="gap-1 sm:gap-1.5">
            <BreadcrumbItem>
              {parts.length ? (
                <button type="button" className="font-medium text-link hover:underline" onClick={() => go("/")}>
                  {site.primary_domain}
                </button>
              ) : (
                <BreadcrumbPage className="font-semibold">{site.primary_domain}</BreadcrumbPage>
              )}
            </BreadcrumbItem>
            {parts.map((p, i) => (
              <Crumb key={i} last={i === parts.length - 1} onClick={() => go("/" + parts.slice(0, i + 1).join("/"))}>
                {p}
              </Crumb>
            ))}
          </BreadcrumbList>
        </Breadcrumb>
        <div className="flex flex-wrap items-center gap-1.5">
          {dir !== "/" && (
            <Tool icon={ArrowUpIcon} onClick={() => go(parentDir(dir))}>
              Up
            </Tool>
          )}
          <Tool icon={RefreshCwIcon} onClick={run(reload)}>
            Refresh
          </Tool>
          <Tool icon={DownloadIcon} onClick={() => download(site, dir)}>
            Download folder
          </Tool>
          {writable && (
            <>
              <Tool icon={FolderPlusIcon} onClick={newFolder}>
                New folder
              </Tool>
              <Tool icon={FilePlusIcon} onClick={newFile}>
                New file
              </Tool>
              <Button type="button" size="sm" onClick={() => picker.current?.click()}>
                <UploadIcon data-icon="inline-start" />
                Upload
              </Button>
            </>
          )}
        </div>
      </div>

      <input
        ref={picker}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          const fs = [...(e.target.files ?? [])]
          e.target.value = ""
          uploadAll(fs)
        }}
      />

      {uploads.length > 0 && (
        <ul aria-label="Uploads" className="flex flex-col gap-2 rounded-2xl bg-card p-3 card-shadow">
          {uploads.map((u) => (
            <li key={u.id} className="grid grid-cols-[minmax(0,1fr)_minmax(6rem,14rem)_3rem] items-center gap-3 text-sm">
              <span className="truncate">
                {u.name}
                <span className="text-muted-foreground"> · {fmtBytes(u.size)}</span>
              </span>
              <Progress value={Math.round(u.progress * 100)} aria-label={`Uploading ${u.name}`} />
              <span className="text-right text-xs text-muted-foreground tabular-nums">{Math.round(u.progress * 100)}%</span>
            </li>
          ))}
        </ul>
      )}

      <div
        {...dropProps}
        className={cn(
          "@container overflow-hidden rounded-2xl bg-card card-shadow transition-[box-shadow,opacity]",
          loading && "opacity-70",
          over && "ring-2 ring-primary ring-offset-2 ring-offset-background"
        )}
      >
        {list.entries.length ? (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full pl-4">Name</TableHead>
                <TableHead className="text-right">Size</TableHead>
                <TableHead className="@max-lg:hidden">Modified</TableHead>
                <TableHead className="@max-xl:hidden">Permissions</TableHead>
                <TableHead className="w-10">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.entries.map((x) => (
                <FileRow key={x.name} site={site} dir={dir} x={x} writable={writable} canWrite={canWrite} go={go} reload={reload} />
              ))}
            </TableBody>
          </Table>
        ) : (
          <p className="px-4 py-10 text-center text-sm text-muted-foreground">
            This folder is empty.{writable ? " Drop files here to upload them." : ""}
          </p>
        )}
      </div>

      {list.truncated && (
        <p className="text-sm text-muted-foreground">Only the first {list.entries.length} entries are shown; use SFTP for folders this large.</p>
      )}
      <p className="max-w-[78ch] text-sm text-muted-foreground">
        {!canWrite
          ? "Read-only: your role can look, not change."
          : site.status !== "active"
            ? "Read-only while the site is suspended."
            : !list.writable
              ? "This folder belongs to WPGenie, not the site: it can't be changed here."
              : "Files you add belong to the site, as WordPress's own do. wp-config.php is kept outside this folder, " +
                "where neither the site nor this file manager can change it. Uploads up to 1 GB; larger files over SFTP."}
      </p>
    </div>
  )
}

function Crumb({ last, onClick, children }: { last: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <>
      <BreadcrumbSeparator>/</BreadcrumbSeparator>
      <BreadcrumbItem>
        {last ? (
          <BreadcrumbPage className="font-semibold">{children}</BreadcrumbPage>
        ) : (
          <button type="button" className="text-link hover:underline" onClick={onClick}>
            {children}
          </button>
        )}
      </BreadcrumbItem>
    </>
  )
}

function Tool({ icon: Icon, onClick, children }: { icon: LucideIcon; onClick: () => void; children: ReactNode }) {
  return (
    <Button type="button" variant="tinted" size="sm" onClick={onClick}>
      <Icon data-icon="inline-start" />
      {children}
    </Button>
  )
}

interface Act {
  key: FileAct
  label: string
  icon: LucideIcon
  danger?: boolean
}

// FileRow is one entry: its name opens it (a folder, the editor, an image
// preview or a download); a menu (and right-click) holds everything else.
function FileRow({
  site, dir, x, writable, canWrite, go, reload,
}: {
  site: Site
  dir: string
  x: Entry
  writable: boolean
  canWrite: boolean
  go: (dir: string) => void
  reload: () => Promise<unknown>
}) {
  const path = joinPath(dir, x.name)
  const isDir = x.type === "dir"
  const isFile = x.type === "file"
  const isLink = x.type === "link"
  const editable = isFile && TEXT_FILE.test(x.name)
  const changeable = writable && x.owned

  const open = async () => {
    if (isDir) return go(path)
    if (isLink) {
      // A link to a folder opens it.
      const l = await api<Listing>("GET", listPath(site, path))
      queryClient.setQueryData([listPath(site, l.path)], l)
      return go(l.path)
    }
    if (editable) return openEditor(site, path, { canWrite, onSaved: reload })
    if (IMAGE_FILE.test(x.name)) return preview(site, path)
    download(site, path)
  }

  const acts: Act[] = [
    editable && { key: "edit", label: changeable && parseInt(x.mode, 8) & 0o200 ? "Edit" : "View", icon: changeable && parseInt(x.mode, 8) & 0o200 ? PencilIcon : EyeIcon },
    (isFile || isDir) && { key: "download", label: isDir ? "Download as zip" : "Download", icon: DownloadIcon },
    changeable && { key: "rename", label: "Rename / move", icon: PencilIcon },
    writable && (isFile || isDir) && { key: "copy", label: "Copy", icon: CopyIcon },
    changeable && (isFile || isDir) && { key: "mode", label: "Permissions", icon: KeyRoundIcon },
    writable && isFile && ZIP_FILE.test(x.name) && { key: "extract", label: "Extract here", icon: PackageOpenIcon },
    changeable && { key: "delete", label: "Delete", icon: Trash2Icon, danger: true },
  ].filter(Boolean) as Act[]

  const act = async (a: FileAct) => {
    try {
      await fileAction(site, dir, x, a, { open, reload })
    } catch (e) {
      showError(e)
    }
  }

  const Glyph = isDir ? FolderIcon : isLink ? LinkIcon : ZIP_FILE.test(x.name) ? FileArchiveIcon : FileIcon
  const items = (Item: typeof DropdownMenuItem | typeof ContextMenuItem, Sep: typeof DropdownMenuSeparator | typeof ContextMenuSeparator) =>
    acts.map((a) => (
      <Fragment key={a.key}>
        {a.danger && acts.length > 1 && <Sep />}
        <Item variant={a.danger ? "destructive" : "default"} onClick={() => act(a.key)}>
          <a.icon />
          {a.label}
        </Item>
      </Fragment>
    ))

  const cells = (
    <>
      <TableCell className="w-full max-w-0 pl-4">
        <div className="flex min-w-0 items-center gap-2">
          <Glyph className={cn("size-4 shrink-0", isDir ? "text-link" : "text-muted-foreground")} aria-hidden />
          <button
            type="button"
            title={path}
            className="min-w-0 truncate text-left font-medium text-foreground hover:text-link hover:underline"
            onClick={() => open().catch(showError)}
          >
            {x.name}
          </button>
          {isLink && <span className="truncate text-xs text-muted-foreground">→ {x.target}</span>}
          {!x.owned && (
            <Badge variant="secondary" title="Managed by WPGenie: read-only" className="shrink-0">
              WPGenie
            </Badge>
          )}
        </div>
      </TableCell>
      <TableCell className="text-right text-xs tabular-nums">{isDir ? "–" : fmtBytes(x.size)}</TableCell>
      <TableCell className="text-xs text-muted-foreground @max-lg:hidden">{fmtTime(x.modified)}</TableCell>
      <TableCell className="@max-xl:hidden">
        <code className="text-xs">{x.mode}</code>
      </TableCell>
      <TableCell className="pr-2 text-right">
        {acts.length > 0 && (
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="ghost" size="icon-sm" aria-label={`Actions for ${x.name}`} />}>
              <EllipsisIcon />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-auto">
              {items(DropdownMenuItem, DropdownMenuSeparator)}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </TableCell>
    </>
  )

  if (!acts.length) return <TableRow>{cells}</TableRow>
  return (
    <ContextMenu>
      <ContextMenuTrigger render={<TableRow />} className="select-auto">
        {cells}
      </ContextMenuTrigger>
      <ContextMenuContent className="w-auto min-w-44">{items(ContextMenuItem, ContextMenuSeparator)}</ContextMenuContent>
    </ContextMenu>
  )
}
