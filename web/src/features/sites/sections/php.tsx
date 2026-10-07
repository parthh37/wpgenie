import { useState, type FormEvent } from "react"
import { CodeIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { SectionProps } from "@/features/sites/sections"
import { useSiteJob } from "./data/jobs"
import type { PHPVersions } from "./data/types"

// PHP: the site's version (switching replaces its replicas, and switches
// back by itself if the site stops working) and its per-request limits.

type Key = keyof Site["php"]

// The settings, their image defaults (0 = default) and the server's limits
// (site/php.go).
const SETTINGS: Array<{ key: Key; label: string; unit: string; def: number; lo: number; hi: (site: Site) => number }> = [
  { key: "memory_limit_mb", label: "memory_limit", unit: "MB", def: 256, lo: 64, hi: (s) => Math.min(2048, s.memory_mb || 2048) },
  { key: "upload_max_mb", label: "Max upload", unit: "MB", def: 64, lo: 1, hi: () => 2048 },
  { key: "max_execution_time", label: "max_execution_time", unit: "s", def: 60, lo: 10, hi: () => 600 },
  { key: "max_input_vars", label: "max_input_vars", unit: "", def: 5000, lo: 1000, hi: () => 100000 },
]

export default function PhpSection({ site }: SectionProps) {
  const { data, error, refetch } = useApi<PHPVersions>("/php")
  if (error) return <LoadError error={error} retry={() => refetch()} />
  if (!data) return <Skeleton className="h-64 rounded-2xl" />
  // A fresh form whenever the site's PHP changes (after the job).
  return <PhpForm key={site.php_version + JSON.stringify(site.php)} site={site} versions={data.versions} />
}

function PhpForm({ site, versions }: { site: Site; versions: string[] }) {
  const s = useSession()
  const job = useSiteJob(site.id, ["php"])
  const [version, setVersion] = useState(site.php_version)
  const [vals, setVals] = useState<Record<Key, string>>(() => {
    const p = site.php || ({} as Partial<Site["php"]>)
    return {
      memory_limit_mb: String(p.memory_limit_mb || 0),
      upload_max_mb: String(p.upload_max_mb || 0),
      max_execution_time: String(p.max_execution_time || 0),
      max_input_vars: String(p.max_input_vars || 0),
    }
  })
  const ro = !s.canChangeSite(site) || job.running
  // The site's version may be one this server no longer offers.
  const options = versions.includes(site.php_version) ? versions : [site.php_version, ...versions]

  const apply = async (e: FormEvent) => {
    e.preventDefault()
    const switching = version !== site.php_version
    if (
      switching &&
      !(await ask(
        `Switch ${site.primary_domain} to PHP ${version}? Replicas are replaced with no downtime; ` +
          "if the site stops working on the new version, it is switched back automatically."
      ))
    )
      return
    await job.run("PUT", `/sites/${site.id}/php`, {
      version,
      settings: {
        memory_limit_mb: Number(vals.memory_limit_mb),
        upload_max_mb: Number(vals.upload_max_mb),
        max_execution_time: Number(vals.max_execution_time),
        max_input_vars: Number(vals.max_input_vars),
      },
    })
  }

  return (
    <Section
      icon={CodeIcon}
      tint="indigo"
      title={`PHP ${site.php_version}`}
      description="The first switch to a version builds its image (a few minutes). memory_limit is per request, up to the replica's memory; long requests also need a proxy/CDN that waits for them."
    >
      <form onSubmit={apply} className="flex flex-col gap-5">
        <Field className="w-auto max-w-56">
          <FieldLabel htmlFor={`php-v-${site.id}`}>Version</FieldLabel>
          <NativeSelect id={`php-v-${site.id}`} className="w-full" value={version} disabled={ro} onChange={(e) => setVersion(e.target.value)}>
            {options.map((v) => (
              <NativeSelectOption key={v} value={v}>
                PHP {v}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
          {SETTINGS.map((x) => {
            const id = `php-${x.key}-${site.id}`
            const hi = x.hi(site)
            return (
              <Field key={x.key}>
                <FieldLabel htmlFor={id}>
                  {x.label}
                  {x.unit && <span className="font-normal text-muted-foreground">{x.unit}</span>}
                </FieldLabel>
                <Input
                  id={id}
                  type="number"
                  inputMode="numeric"
                  min={0}
                  max={hi}
                  value={vals[x.key]}
                  disabled={ro}
                  onChange={(e) => setVals({ ...vals, [x.key]: e.target.value })}
                />
                <FieldDescription>
                  0 = {x.def.toLocaleString()} (default), or {x.lo.toLocaleString()} to {hi.toLocaleString()}
                </FieldDescription>
              </Field>
            )
          })}
        </div>
        {s.canChangeSite(site) && (
          <div>
            <Button type="submit" disabled={job.running}>
              {job.running ? "Applying…" : "Apply"}
            </Button>
          </div>
        )}
      </form>
    </Section>
  )
}
