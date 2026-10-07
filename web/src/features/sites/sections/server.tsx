import { useState } from "react"
import { ArrowRightLeftIcon, CopyIcon, ServerIcon, TruckIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldLabel } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, Banner } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { IconTile } from "@/components/app/icon-tile"
import { Section } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { startJob } from "@/lib/jobs"
import { invalidate, useApi, useNodes } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Node, Site } from "@/lib/types"
import type { SectionProps } from "../sections"

// Where a site lives in a cluster: its server, moving it to another, and
// replicas on other servers. (The rail only offers this section on a
// cluster.)

// The tray's names for the jobs started here.

interface Move {
  from: string
  to: string
  at: string
  point_dns_to?: string
}

type ClusterNode = Node

export default function ServerSection({ site }: SectionProps) {
  const s = useSession()
  const { data: nodes, isLoading } = useNodes()
  const all = (nodes ?? []) as ClusterNode[]
  const nodeName = (id?: string) => all.find((n) => n.id === (id || "local"))?.name ?? id ?? "local"
  const here = site.node || "local"
  const node = all.find((n) => n.id === here)
  const others = all.filter((n) => n.id !== here && n.status === "active" && n.up)
  const spread = site.spread_nodes ?? []

  if (isLoading) return <Skeleton className="h-40 rounded-2xl" />

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-4 rounded-2xl bg-card p-4 card-shadow">
        <IconTile icon={ServerIcon} tint="graphite" size="lg" />
        <div className="min-w-0 flex-1">
          <p className="text-[0.9375rem]">
            Lives on <strong className="font-semibold">{nodeName(here)}</strong>
            {node?.public_ip ? (
              <>
                {" "}
                — its domains should point at <code>{node.public_ip}</code>.
              </>
            ) : (
              "."
            )}
          </p>
          {spread.length > 0 && (
            <p className="text-sm text-muted-foreground">Replicas also on {spread.map((id) => nodeName(id)).join(", ")}.</p>
          )}
        </div>
        {node && (node.up ? <StatusPill status={node.status} /> : <StatusPill status="unreachable" tone="bad" />)}
      </div>

      <LastMove site={site} nodeName={nodeName} admin={s.isAdmin} />

      {s.isAdmin && site.status === "active" && !site.parent_id && (
        <>
          <MoveSite site={site} others={others} nodeName={nodeName} />
          <Spread site={site} others={others} />
        </>
      )}
    </div>
  )
}

// LastMove: a move whose old copy may still exist, and finishing it.
function LastMove({ site, nodeName, admin }: { site: Site; nodeName: (id?: string) => string; admin: boolean }) {
  const q = useApi<{ move: Move | null }>(`/sites/${site.id}/move`)
  if (q.error) {
    return (
      <p role="alert" className="text-sm text-danger">
        {q.error.message}
      </p>
    )
  }
  const move = q.data?.move
  if (!move) return null
  const finish = async () => {
    if (!(await ask(`Delete the old copy on ${nodeName(move.from)} now? Do this once the domains point at ${move.point_dns_to || "the new server"}.`))) return
    try {
      await api("POST", `/sites/${site.id}/move/finish`)
      await Promise.all([invalidate("/sites"), invalidate("/nodes"), q.refetch()])
    } catch (e) {
      showError(e)
    }
  }
  return (
    <Banner
      tone="warn"
      icon={TruckIcon}
      title={`Moved from ${nodeName(move.from)} ${fmtTime(move.at)}`}
      actions={
        admin ? (
          <ActionButton run={finish} variant="destructive">
            Finish move
          </ActionButton>
        ) : undefined
      }
      className="mb-0"
    >
      {move.point_dns_to ? (
        <>
          Point its domains at <code>{move.point_dns_to}</code>;{" "}
        </>
      ) : null}
      until then the old server passes visitors on (for a week at most).
    </Banner>
  )
}

function MoveSite({ site, others, nodeName }: { site: Site; others: ClusterNode[]; nodeName: (id?: string) => string }) {
  const [target, setTarget] = useState("")
  const [moving, setMoving] = useState(false)
  const to = others.some((n) => n.id === target) ? target : (others[0]?.id ?? "")

  const go = async () => {
    if (
      !(await ask(
        `Move ${site.primary_domain} to ${nodeName(to)}? Files and database are copied while the site runs; ` +
          "it shows a maintenance page only during the final copy. Its old server then passes visitors on until DNS points at the new one."
      ))
    )
      return
    setMoving(true)
    try {
      await startJob("POST", `/sites/${site.id}/migrate`, { node: to }, async () => {
        setMoving(false)
        await Promise.all([invalidate("/nodes"), invalidate(`/sites/${site.id}/move`)])
      })
    } catch (e) {
      showError(e)
      setMoving(false)
    }
  }

  return (
    <Section icon={ArrowRightLeftIcon} tint="blue" title="Move to another server" className="mb-0">
      <div className="flex flex-wrap items-end gap-3">
        <Field className="w-auto min-w-64">
          <FieldLabel htmlFor={`move-${site.id}`}>Move to</FieldLabel>
          <NativeSelect id={`move-${site.id}`} value={to} disabled={!others.length || moving} onChange={(e) => setTarget(e.target.value)}>
            {others.map((n) => (
              <NativeSelectOption key={n.id} value={n.id}>
                {n.name} ({n.id})
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        <Button variant="tinted" disabled={!others.length || moving} onClick={go}>
          Move
        </Button>
      </div>
      {!others.length && <p className="mt-3 text-sm text-muted-foreground">No other server is available.</p>}
    </Section>
  )
}

// Spread: replicas on other servers too (needs uploads offload).
function Spread({ site, others }: { site: Site; others: ClusterNode[] }) {
  const initial = site.spread_nodes ?? []
  const [picked, setPicked] = useState<string[] | null>(null)
  const [saving, setSaving] = useState(false)
  const nodes = picked ?? initial
  const changed = picked != null && (picked.length !== initial.length || picked.some((x) => !initial.includes(x)))

  const save = async () => {
    setSaving(true)
    try {
      await api("PUT", `/sites/${site.id}/spread`, { nodes: others.filter((n) => nodes.includes(n.id)).map((n) => n.id) })
      await invalidate("/sites")
      setPicked(null)
    } catch (e) {
      showError(e)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Section
      icon={CopyIcon}
      tint="teal"
      title="Replicas on other servers"
      description={
        <>
          Page views are also served by replicas on the servers ticked here (the site's <em>replicas</em> are shared out); wp-admin, sign-ins and
          every change still run on its own server. Needs uploads offload.
        </>
      }
      className="mb-0"
    >
      {others.length ? (
        <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
          {others.map((n) => (
            <Label key={n.id} className="font-normal">
              <Checkbox
                checked={nodes.includes(n.id)}
                disabled={saving}
                onCheckedChange={(c) => setPicked(c ? [...nodes.filter((x) => x !== n.id), n.id] : nodes.filter((x) => x !== n.id))}
              />
              {n.name}
            </Label>
          ))}
          <Button variant="tinted" size="sm" disabled={saving || !changed} onClick={save}>
            Save
          </Button>
        </div>
      ) : (
        <p className="text-sm text-muted-foreground">No other server is available.</p>
      )}
    </Section>
  )
}
