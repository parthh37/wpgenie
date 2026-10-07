import { useEffect, useMemo, useRef, useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { ArchiveIcon, DownloadIcon, EyeIcon, SearchIcon, XIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { BTable, EmptyState } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { fmtBytes, fmtNum, fmtTime } from "@/lib/format"
import { useApi } from "@/lib/query"
import { todayUTC, typeInfo, type ArchiveObject, type LogSettings, type LogStatus } from "./data"

// The archive browser: what's in the bucket for a server, kind of log and
// day; view a file here (up to 20 MB of it, with a filter) or download it.

interface Archives {
  server: string
  type: string
  date: string
  servers: string[]
  objects: ArchiveObject[] | null
}

const objectURL = (key: string, download?: boolean) =>
  `/api/v1/logs/archives/object?${new URLSearchParams(download ? { key, download: "1" } : { key })}`

export function ArchiveCard({ st, set }: { st: LogStatus; set: LogSettings }) {
  const servers = [st.server, ...(st.servers ?? []).map((x) => x.id)]
  const [server, setServer] = useState(set.server)
  const [type, setType] = useState("access")
  const [date, setDate] = useState(todayUTC)
  // The listing shown: set by "Show files".
  const [shown, setShown] = useState<string | null>(null)
  const list = useApi<Archives>(shown)
  const [viewing, setViewing] = useState<ArchiveObject | null>(null)

  function show(e: FormEvent) {
    e.preventDefault()
    const path = `/logs/archives?${new URLSearchParams({ type, date, server })}`
    setViewing(null)
    if (path === shown) list.refetch()
    else setShown(path)
  }

  const objects = list.data?.objects ?? []

  return (
    <Section
      icon={ArchiveIcon}
      tint="brown"
      title="Archive"
      description="What's in the bucket: pick a kind of log and a day, then view a file here or download it."
    >
      <form onSubmit={show} className="mb-4 flex flex-wrap items-end gap-3">
        {servers.length > 1 && (
          <div className="grid gap-1.5">
            <Label htmlFor="archive-server">Server</Label>
            <NativeSelect id="archive-server" value={server} onChange={(e) => setServer(e.target.value)}>
              {servers.map((x) => (
                <NativeSelectOption key={x} value={x}>
                  {x}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </div>
        )}
        <div className="grid gap-1.5">
          <Label htmlFor="archive-type">Kind of log</Label>
          <NativeSelect id="archive-type" value={type} onChange={(e) => setType(e.target.value)}>
            {st.types.map((t) => (
              <NativeSelectOption key={t.name} value={t.name}>
                {typeInfo(t.name).title}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="archive-date">Day (UTC)</Label>
          <Input id="archive-date" type="date" required max={todayUTC()} value={date} onChange={(e) => setDate(e.target.value)} className="w-44" />
        </div>
        <Button type="submit">
          <SearchIcon data-icon="inline-start" />
          Show files
        </Button>
      </form>

      <div aria-live="polite">
        {list.isFetching && !list.data && (
          <div className="flex flex-col gap-2">
            <p className="text-sm text-muted-foreground">Listing the bucket…</p>
            <Skeleton className="h-20 rounded-xl" />
          </div>
        )}
        {list.error && (
          <p role="alert" className="text-sm text-danger">
            {(list.error as Error).message}
          </p>
        )}
        {list.data && !list.error && (
          <BTable
            caption="Archive files"
            empty={
              <EmptyState icon={ArchiveIcon} tint="gray" title="No files that day" className="shadow-none ring-1 ring-border/60">
                {st.enabled
                  ? "Nothing of this kind was uploaded that day (files appear a few minutes after the logs)."
                  : "Log shipping is off: nothing is uploaded."}
              </EmptyState>
            }
            cols={["Hour (UTC)", "File", { label: "Size", num: true }, "Uploaded", { label: <span className="sr-only">Actions</span> }]}
            rows={objects.map((o) => ({
              key: o.key,
              className: viewing?.key === o.key ? "bg-primary/6" : undefined,
              cells: [
                <span className="tabular-nums">{o.name.slice(0, 2)}:00</span>,
                <span className="font-mono text-xs [overflow-wrap:anywhere] whitespace-normal">{o.name}</span>,
                fmtBytes(o.size),
                o.modified ? fmtTime(o.modified) : "–",
                <div className="flex flex-wrap justify-end gap-1.5">
                  <Button size="sm" variant="tinted" onClick={() => setViewing(o)}>
                    <EyeIcon data-icon="inline-start" />
                    View
                  </Button>
                  <Button size="sm" variant="tinted" nativeButton={false} render={<a href={objectURL(o.key, true)} download />}>
                    <DownloadIcon data-icon="inline-start" />
                    Download
                  </Button>
                </div>,
              ],
            }))}
          />
        )}
      </div>

      {viewing && <Viewer key={viewing.key} obj={viewing} onClose={() => setViewing(null)} />}
    </Section>
  )
}

// ---- Viewer ----

const MAX_LINES = 2000

type Loaded = { kind: "loading" } | { kind: "zstd" } | { kind: "error"; message: string } | { kind: "ok"; lines: string[] }

// loadObject fetches a file of the archive as text (decompressed by the
// server; zstd can't be, so it's only downloaded).
async function loadObject(key: string): Promise<Loaded> {
  try {
    const res = await fetch(objectURL(key), { credentials: "same-origin", headers: { "X-Requested-With": "wpgenie" } })
    if (!res.ok) {
      const data = (await res.json().catch(() => ({}))) as { error?: string }
      return { kind: "error", message: data.error || res.statusText }
    }
    if ((res.headers.get("Content-Type") || "").includes("zstd")) return { kind: "zstd" }
    return { kind: "ok", lines: (await res.text()).split("\n").filter(Boolean) }
  } catch (e) {
    return { kind: "error", message: e instanceof Error ? e.message : String(e) }
  }
}

function Viewer({ obj, onClose }: { obj: ArchiveObject; onClose: () => void }) {
  const [filter, setFilter] = useState("")
  const box = useRef<HTMLDivElement>(null)
  const q = useQuery<Loaded>({
    queryKey: ["log-archive-object", obj.key],
    queryFn: () => loadObject(obj.key),
    staleTime: Infinity,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
  })
  const state: Loaded = q.data ?? { kind: "loading" }

  useEffect(() => {
    box.current?.scrollIntoView({ behavior: "smooth", block: "nearest" })
  }, [])

  const lines = state.kind === "ok" ? state.lines : null
  const shown = useMemo(() => {
    if (!lines) return []
    const needle = filter.trim().toLowerCase()
    return needle ? lines.filter((l) => l.toLowerCase().includes(needle)) : lines
  }, [lines, filter])

  return (
    <div ref={box} className="mt-5 scroll-mt-6 rounded-2xl bg-muted/40 p-4 ring-1 ring-border/60">
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <strong className="font-mono text-sm [overflow-wrap:anywhere]">{obj.name}</strong>
        {lines && (
          <span className="text-sm text-muted-foreground">
            {fmtNum(shown.length)} of {fmtNum(lines.length)} lines
            {shown.length > MAX_LINES ? ` (first ${fmtNum(MAX_LINES)} shown)` : ""}
          </span>
        )}
        <Button size="sm" variant="ghost" className="ml-auto" onClick={onClose}>
          <XIcon data-icon="inline-start" />
          Close
        </Button>
      </div>

      {state.kind === "loading" && <p className="text-sm text-muted-foreground">Fetching {obj.name}…</p>}
      {state.kind === "zstd" && <p className="text-sm">This file is compressed with zstd: download it to read it (zstd -d).</p>}
      {state.kind === "error" && (
        <p role="alert" className="text-sm text-danger">
          {state.message}
        </p>
      )}
      {lines && (
        <>
          <InputGroup className="mb-2">
            <InputGroupAddon>
              <SearchIcon />
            </InputGroupAddon>
            <InputGroupInput
              type="search"
              placeholder='Filter lines (e.g. a site, an IP, "status":500)'
              aria-label="Filter lines"
              autoComplete="off"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </InputGroup>
          <pre
            tabIndex={0}
            aria-label={`Contents of ${obj.name}`}
            className="max-h-[32rem] overflow-auto rounded-xl bg-card p-3 font-mono text-[0.78rem] leading-normal break-all whitespace-pre-wrap ring-1 ring-border/60"
          >
            {shown.slice(0, MAX_LINES).map((l, i) => (
              <PrettyLine key={i} line={l} />
            ))}
          </pre>
        </>
      )}
    </div>
  )
}

// PrettyLine is a line of the file: its time and gist when it's JSON (an
// access log line: method, address and status), then the line itself.
function PrettyLine({ line }: { line: string }) {
  let gist = ""
  let when = ""
  try {
    const o = JSON.parse(line)
    const t = o.time || o.ts || o.timestamp
    when = typeof t === "number" ? new Date(t * 1000).toISOString() : String(t || "")
    const r = o.request
    gist =
      r && r.method
        ? `${r.method} ${r.host || ""}${r.uri || ""} → ${o.status}` + (o.site ? ` (${o.site})` : "")
        : o.msg || o.message || o.reason || o.action || o.subject || o.kind || ""
  } catch {
    /* not JSON: the line as it is */
  }
  const head = `${when.replace("T", " ").replace(/\.\d+Z$/, "Z")}  ${gist}`.trim()
  return (
    <span className="block border-b border-border/60 py-1 last:border-0">
      {(gist || when) && <span className="block font-semibold text-foreground">{head}</span>}
      <span className="block text-muted-foreground">{line}</span>
    </span>
  )
}
