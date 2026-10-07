import { useState, type FormEvent } from "react"
import { ChevronRightIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtMem, fmtTime } from "@/lib/format"
import { invalidate } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { useSiteCPU } from "../../data"
import { Note, SwitchRow } from "./shared"

// Advanced: the instances, memory and CPU a site gets, and the targets
// automatic scaling (what burst does underneath) steers by.

export const MEMORY_MB = [256, 512, 768, 1024, 1536, 2048, 3072, 4096, 6144, 8192]
export const CPUS = [0.25, 0.5, 1, 1.5, 2, 3, 4, 6, 8]
const REPLICAS = [1, 2, 3, 4, 5, 6, 7, 8]

// options: the usual values and the current one (which may be off the
// list), in order.
const options = (values: number[], current: number) => [...new Set([...values, current])].sort((a, b) => a - b)

export function ScalingAdvanced({ site }: { site: Site }) {
  return (
    <Collapsible className="rounded-xl ring-1 ring-border">
      <CollapsibleTrigger className="group flex w-full items-center gap-2 rounded-xl px-3.5 py-2.5 text-left text-sm font-medium hover:bg-muted/60 focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none">
        <ChevronRightIcon className="size-4 text-muted-foreground transition-transform group-data-[panel-open]:rotate-90" />
        Advanced: size and scaling targets
      </CollapsibleTrigger>
      <CollapsibleContent className="flex flex-col gap-4 px-3.5 pt-1 pb-4">
        {/* Each part starts afresh from the saved values once they change. */}
        <SizeForm key={[site.replicas, site.memory_mb, site.cpus, site.autoscale].join("|")} site={site} />
        <AutoscaleForm
          key={[site.autoscale, site.min_replicas, site.max_replicas, site.replicas, site.target_cpu, site.target_workers, site.target_response_ms].join("|")}
          site={site}
        />
        <CPULine site={site} />
      </CollapsibleContent>
    </Collapsible>
  )
}

function SizeForm({ site }: { site: Site }) {
  const s = useSession()
  const active = site.status === "active"
  const [replicas, setReplicas] = useState(String(site.replicas))
  const [memory, setMemory] = useState(String(site.memory_mb))
  const [cpus, setCpus] = useState(String(site.cpus))
  const [busy, setBusy] = useState(false)
  const locked = !active || !s.canChange || busy

  const apply = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      await api("PUT", `/sites/${site.id}/resources`, { replicas: Number(replicas), memory_mb: Number(memory), cpus: Number(cpus) })
      await invalidate("/sites")
    } catch (err) {
      showError(err)
    }
    setBusy(false)
  }

  return (
    <form onSubmit={apply} className="flex flex-col gap-2">
      <div className="flex flex-wrap items-end gap-3">
        <Field className="w-auto">
          <FieldLabel htmlFor={`replicas-${site.id}`}>Instances</FieldLabel>
          {/* While scaling automatically, the normal size is Min. */}
          <NativeSelect id={`replicas-${site.id}`} value={replicas} disabled={locked || site.autoscale} onChange={(e) => setReplicas(e.target.value)}>
            {options(REPLICAS, site.replicas).map((v) => (
              <NativeSelectOption key={v} value={v}>
                {v}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        <Field className="w-auto">
          <FieldLabel htmlFor={`memory-${site.id}`}>Memory / instance</FieldLabel>
          <NativeSelect id={`memory-${site.id}`} value={memory} disabled={locked} onChange={(e) => setMemory(e.target.value)}>
            {options(MEMORY_MB, site.memory_mb).map((v) => (
              <NativeSelectOption key={v} value={v}>
                {fmtMem(v)}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        <Field className="w-auto">
          <FieldLabel htmlFor={`cpus-${site.id}`}>CPU / instance</FieldLabel>
          <NativeSelect id={`cpus-${site.id}`} value={cpus} disabled={locked} onChange={(e) => setCpus(e.target.value)}>
            {options(CPUS, site.cpus).map((v) => (
              <NativeSelectOption key={v} value={v}>
                {v} CPU
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        {s.canChange && (
          <Button type="submit" disabled={locked}>
            {busy ? "Scaling…" : "Apply"}
          </Button>
        )}
      </div>
      <Note>
        Instances (PHP containers) are replaced one set at a time with no downtime. Each gets the full memory and CPU shown. While scaling automatically,{" "}
        <em>Instances</em> is what runs now and <em>Min</em> is the normal size.
      </Note>
    </form>
  )
}

function AutoscaleForm({ site }: { site: Site }) {
  const s = useSession()
  const active = site.status === "active"
  const [on, setOn] = useState(site.autoscale)
  const [min, setMin] = useState(String(site.min_replicas))
  // Suggest room to grow when turning it on; the server enforces its own limit.
  const [max, setMax] = useState(String(site.autoscale ? site.max_replicas : Math.max(site.replicas, site.max_replicas, 2)))
  const [target, setTarget] = useState(String(site.target_cpu))
  const [workers, setWorkers] = useState(String(site.target_workers || 0))
  const [ms, setMs] = useState(String(site.target_response_ms || 0))
  const [busy, setBusy] = useState(false)
  const locked = !active || !s.canChange || busy

  const save = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      await api("PUT", `/sites/${site.id}/autoscale`, {
        enabled: on,
        min_replicas: Number(min),
        max_replicas: Number(max),
        target_cpu: Number(target),
        target_workers: Number(workers || 0),
        target_response_ms: Number(ms || 0),
      })
      notify(on ? `Scaling between ${min} and ${max} instances` : "Automatic scaling off")
      await invalidate("/sites")
    } catch (err) {
      showError(err)
    }
    setBusy(false)
  }

  const num = (id: string, label: React.ReactNode, value: string, set: (v: string) => void, attrs: React.ComponentProps<"input">) => (
    <Field className="w-auto">
      <FieldLabel htmlFor={`${id}-${site.id}`} className="whitespace-nowrap">
        {label}
      </FieldLabel>
      <Input id={`${id}-${site.id}`} className="w-36" type="number" inputMode="numeric" value={value} disabled={locked} onChange={(e) => set(e.target.value)} {...attrs} />
    </Field>
  )

  return (
    <form onSubmit={save} className="flex flex-col gap-3 border-t border-border/60 pt-4">
      <SwitchRow checked={on} onChange={setOn} disabled={locked} title="Scale automatically" />
      <div className="flex flex-wrap items-end gap-3">
        {num("as-min", "Min", min, setMin, { min: 1 })}
        {num("as-max", "Max", max, setMax, { min: 1 })}
        {num("as-target", "Target CPU %", target, setTarget, { min: 20, max: 95 })}
        {num(
          "as-workers",
          <>
            Workers busy %<span className="font-normal text-muted-foreground">(0 = off)</span>
          </>,
          workers,
          setWorkers,
          { min: 0, max: 100 }
        )}
        {num(
          "as-ms",
          <>
            Response ms<span className="font-normal text-muted-foreground">(p95, 0 = off)</span>
          </>,
          ms,
          setMs,
          { min: 0, max: 30000, step: 50 }
        )}
        {s.canChange && (
          <Button type="submit" variant="tinted" disabled={locked}>
            Save
          </Button>
        )}
      </div>
      <Note>
        This is what burst does underneath: <em>Min</em> is the normal size, and instances follow whichever target asks for the most.{" "}
        <em>Workers busy</em> counts requests waiting for a PHP worker too: it catches sites that wait on slow APIs or queries while their CPU stays
        low. <em>Response ms</em> adds an instance while 95% of PHP responses take longer and the site is busy.
      </Note>
    </form>
  )
}

// CPULine: what the autoscaler sampled last (every 15 s).
function CPULine({ site }: { site: Site }) {
  const { data: c } = useSiteCPU(site)
  if (!c) return null
  const parts = [`CPU ${c.percent}% of each instance's allowance across ${c.replicas} instance(s)`]
  if (c.workers_percent != null) parts.push(`PHP workers ${c.workers_percent}% busy` + (c.queued ? `, ${c.queued} request(s) waiting` : ""))
  if (c.p95_ms != null) parts.push(`95% of the last minute's ${c.responses} responses within ${Math.round(c.p95_ms)} ms`)
  return <Note>{`${parts.join(" · ")}; sampled ${fmtTime(c.at)}.`}</Note>
}
