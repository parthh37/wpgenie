import { useState } from "react"
import { ArrowDownToLineIcon, PlusIcon, RefreshCwIcon, ServerIcon, Trash2Icon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { ActionButton, BTable, EmptyState, FormDialog } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Page, PageHeader, Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtMem, fmtTime, plural } from "@/lib/format"
import { JOB_NAMES, followJob } from "@/lib/jobs"
import { invalidate, useNodes } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Node } from "@/lib/types"
import { cn } from "@/lib/utils"

// Servers: the panel's own server and the nodes paired with it; drain,
// activate, update, remove, and pairing a new one.

Object.assign(JOB_NAMES, { migrate: "Moving a site to another server", drain: "Moving every site off a server" })

interface NodeInfo {
  version?: string
  mem_total_mb?: number
  committed_mb?: number
  disk_free_gb?: number
  disk_total_gb?: number
}

interface ServerNode extends Node {
  local?: boolean
  sites: number
  address?: string
  public_ip?: string
  cert_not_after?: string
  info?: NodeInfo | string | null
}

function nodeInfo(n: ServerNode | undefined): NodeInfo {
  if (!n) return {}
  try {
    return typeof n.info === "string" ? JSON.parse(n.info) : (n.info ?? {})
  } catch {
    return {}
  }
}

function NodeState({ node }: { node: ServerNode }) {
  if (!node.up)
    return (
      <Tooltip>
        <TooltipTrigger render={<span className="cursor-help font-medium text-danger" />}>unreachable</TooltipTrigger>
        {node.last_error && <TooltipContent className="max-w-sm">{node.last_error}</TooltipContent>}
      </Tooltip>
    )
  return <span className={cn("font-medium", node.status === "active" ? "text-success" : "text-warning")}>{node.status}</span>
}

const reload = () => Promise.all([invalidate("/nodes"), invalidate("/sites")])

export default function ServersPage() {
  const s = useSession()
  const { data, isLoading } = useNodes()
  const [adding, setAdding] = useState(false)
  const nodes = (data ?? []) as ServerNode[]
  const panelVersion = nodeInfo(nodes.find((n) => n.local)).version

  return (
    <Page>
      <PageHeader
        icon={ServerIcon}
        tint="graphite"
        title="Servers"
        description="The machines your sites run on, and how full they are."
        actions={
          s.isAdmin && (
            <Button onClick={() => setAdding(true)}>
              <PlusIcon data-icon="inline-start" />
              Add server
            </Button>
          )
        }
      />

      <Section
        description="Sites can live on several servers. Each runs its own Caddy, PHP, MariaDB and Valkey for the sites placed on it; this panel places new sites (on the server with the most memory to spare, unless you pick one), moves them, and talks to every server over mutual TLS. Nothing but the panel can manage a server once it's paired."
      >
        {isLoading && <Skeleton className="h-32 rounded-xl" />}
        {data && !nodes.length && (
          <EmptyState icon={ServerIcon} tint="graphite" title="No servers listed" className="shadow-none">
            This server doesn't report any servers. On a server paired with a panel, servers are managed from that panel.
          </EmptyState>
        )}
        {nodes.length > 0 && (
          <BTable
            caption="Servers"
            cols={[
              "Server",
              "Address",
              "State",
              { label: "Sites", num: true },
              "Memory promised",
              "Disk free",
              "Version",
              "Certificate until",
              { label: <span className="sr-only">Actions</span> },
            ]}
            rows={nodes.map((n) => {
              const i = nodeInfo(n)
              const mem = i.mem_total_mb ? `${fmtMem(i.committed_mb || 0)} of ${fmtMem(i.mem_total_mb)}` : "–"
              const disk = i.disk_total_gb ? `${(i.disk_free_gb ?? 0).toFixed(0)} of ${i.disk_total_gb.toFixed(0)} GB` : "–"
              const outdated = !!i.version && !!panelVersion && i.version !== panelVersion
              return {
                key: n.id,
                cells: [
                  <div className="flex flex-col">
                    <strong className="font-semibold">{n.name}</strong>
                    <span className="text-xs text-muted-foreground">{n.local ? "the panel" : n.id}</span>
                  </div>,
                  n.local ? (
                    "–"
                  ) : (
                    <div className="flex flex-col whitespace-nowrap">
                      <span className="font-mono text-xs">{n.address}</span>
                      {n.public_ip && <span className="text-xs text-muted-foreground">DNS: {n.public_ip}</span>}
                    </div>
                  ),
                  <NodeState node={n} />,
                  String(n.sites ?? 0),
                  <span className="whitespace-nowrap">{mem}</span>,
                  <span className="whitespace-nowrap">{disk}</span>,
                  <span className="inline-flex items-center gap-1.5">
                    {i.version || "–"}
                    {outdated && <Badge className="bg-warning-fill/16 text-warning" variant="secondary">behind</Badge>}
                  </span>,
                  n.local || !n.cert_not_after || n.cert_not_after.startsWith("0001-") ? "–" : fmtTime(n.cert_not_after),
                  s.isAdmin && !n.local ? <NodeActions node={n} outdated={outdated} /> : null,
                ],
              }
            })}
          />
        )}
      </Section>

      {s.isAdmin && <AddServerDialog open={adding} onOpenChange={setAdding} />}
    </Page>
  )
}

function NodeActions({ node: n, outdated }: { node: ServerNode; outdated: boolean }) {
  return (
    <div className="flex justify-end gap-1.5">
      {n.status === "active" ? (
        <ActionButton
          size="sm"
          run={async () => {
            if (!(await ask(`Move every site off ${n.name}, one after the other? New sites won't be placed on it.`, { ok: "Drain", danger: true })))
              return
            const r = await api<{ job_id: string }>("POST", `/nodes/${n.id}/drain`)
            followJob(r.job_id, () => {
              reload()
            })
            await invalidate("/nodes")
          }}
        >
          <ArrowDownToLineIcon data-icon="inline-start" />
          Drain
        </ActionButton>
      ) : (
        <ActionButton
          size="sm"
          run={async () => {
            await api("PUT", `/nodes/${n.id}`, { status: "active" })
            notify(`${n.name} takes new sites again`)
            await reload()
          }}
        >
          Activate
        </ActionButton>
      )}
      {outdated && (
        <ActionButton
          size="sm"
          run={async () => {
            await api("POST", `/nodes/${n.id}/update`)
            notify(`${n.name} is updating to the panel's version`)
            await reload()
          }}
        >
          <RefreshCwIcon data-icon="inline-start" />
          Update
        </ActionButton>
      )}
      <ActionButton
        size="sm"
        variant="destructive"
        run={async () => {
          const force = n.sites > 0
          if (force) {
            if (
              !(await ask(
                `${plural(n.sites, "site")} still live on ${n.name}. Removing it leaves them running there, out of the panel's reach. Remove anyway?`,
                { ok: "Remove", danger: true }
              ))
            )
              return
          } else if (!(await ask(`Remove ${n.name} from the cluster?`))) return
          await api("DELETE", `/nodes/${n.id}` + (force ? "?force=1" : ""))
          notify(`${n.name} removed`)
          await reload()
        }}
      >
        <Trash2Icon data-icon="inline-start" />
        Remove
      </ActionButton>
    </div>
  )
}

function AddServerDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Add a server"
      wide
      intro={
        <>
          On the new server, install WPGenie as a node (<code>install.sh --agent</code>), then run <code>wpgenie agent pair-code</code> and paste
          the code here with the server's address. The code works once, and proves to the panel it reached that server (and to the server that
          the panel is yours). Port 7443 must be open between the servers.
        </>
      }
      ok="Add server"
      onSubmit={async (data) => {
        const body = Object.fromEntries(data) as Record<string, string>
        await api("POST", "/nodes", body)
        notify(`${body.name} added`)
        await reload()
      }}
    >
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="node-name">Name</FieldLabel>
          <Input id="node-name" name="name" placeholder="web-2" required autoComplete="off" />
        </Field>
        <Field>
          <FieldLabel htmlFor="node-address">Address</FieldLabel>
          <Input id="node-address" name="address" placeholder="203.0.113.7 or node2.example.com:7443" required autoComplete="off" />
        </Field>
        <Field>
          <FieldLabel htmlFor="node-ip">Public IP</FieldLabel>
          <Input id="node-ip" name="public_ip" placeholder="the address's IP" autoComplete="off" />
          <FieldDescription>Optional: where sites' DNS points.</FieldDescription>
        </Field>
        <Field>
          <FieldLabel htmlFor="node-code">Pairing code</FieldLabel>
          <Input id="node-code" name="pairing_code" placeholder="wpg1-…" required autoComplete="off" spellCheck={false} className="font-mono" />
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}
