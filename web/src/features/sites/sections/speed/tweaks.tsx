import { useState } from "react"
import { SlidersHorizontalIcon } from "lucide-react"
import { Checkbox } from "@/components/ui/checkbox"
import { Skeleton } from "@/components/ui/skeleton"
import { ask } from "@/components/app/confirm"
import { ActionButton, LoadError } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { cn } from "@/lib/utils"
import { Note } from "./shared"

// WordPress tweaks: what performance plugins do on top of caching, applied
// by a must-use plugin (PUT /sites/{id}/optimize).

interface Optimization {
  key: string
  title: string
  description: string
  // Default: new sites get it, and "Apply recommended" turns it on.
  default: boolean
}

interface CleanupResult {
  transients: number
  auto_drafts: number
  spam: number
  revisions: number
}

export function TweaksCard({ site }: { site: Site }) {
  const s = useSession()
  // The catalogue is the same for every site: loaded once.
  const { data: catalogue, error, isLoading, refetch } = useApi<Optimization[]>("/optimizations")
  const saved = site.optimize || []
  const savedKey = saved.join(",")
  const [on, setOn] = useState(() => new Set(saved))
  const [seen, setSeen] = useState(savedKey)
  if (seen !== savedKey) {
    setSeen(savedKey)
    setOn(new Set(saved))
  }

  const put = async (keys: string[], msg: string) => {
    await api("PUT", `/sites/${site.id}/optimize`, { optimizations: keys })
    notify(msg)
    // The health analysis judges these too: it reruns.
    await invalidate("/sites")
  }

  const cleanup = async () => {
    if (
      !(await ask(
        `Clean ${site.primary_domain}'s database now? Expired transients, auto-drafts older than a week, spam older than a month and revisions older than a month beyond the newest five per post are deleted.`,
        { ok: "Clean up" }
      ))
    )
      return
    const r = await api<CleanupResult>("POST", `/sites/${site.id}/optimize/cleanup`)
    notify(`Removed ${r.transients} expired transients, ${r.auto_drafts} auto-drafts, ${r.spam} spam comments, ${r.revisions} old revisions`)
    await invalidate(`/sites/${site.id}/analysis`)
  }

  return (
    <Section icon={SlidersHorizontalIcon} tint="indigo" title="WordPress tweaks">
      <div className="flex flex-col gap-4">
        {isLoading ? (
          <div className="grid gap-3 sm:grid-cols-2">
            {Array.from({ length: 6 }, (_, i) => (
              <Skeleton key={i} className="h-14" />
            ))}
          </div>
        ) : error ? (
          <LoadError error={error} retry={() => refetch()} className="py-8" />
        ) : (
          <div className="grid gap-x-6 gap-y-4 sm:grid-cols-2">
            {(catalogue || []).map((o) => (
              <label
                key={o.key}
                title={o.description}
                className={cn("flex items-start gap-3", s.canChange ? "cursor-pointer" : "cursor-not-allowed")}
              >
                <Checkbox
                  className="mt-0.5"
                  checked={on.has(o.key)}
                  disabled={!s.canChange}
                  onCheckedChange={(c) =>
                    setOn((prev) => {
                      const next = new Set(prev)
                      if (c) next.add(o.key)
                      else next.delete(o.key)
                      return next
                    })
                  }
                />
                <span className="flex min-w-0 flex-col gap-0.5">
                  <span className="text-sm font-medium">
                    {o.title}
                    {!o.default && <span className="font-normal text-muted-foreground"> (optional)</span>}
                  </span>
                  <span className="text-[0.8125rem] leading-snug text-muted-foreground">{o.description}</span>
                </span>
              </label>
            ))}
          </div>
        )}
        {s.canChange && (
          <div className="flex flex-wrap gap-2">
            <ActionButton run={cleanup}>Clean database now</ActionButton>
            <ActionButton
              disabled={!catalogue}
              run={() => {
                const keys = new Set([...saved, ...(catalogue || []).filter((o) => o.default).map((o) => o.key)])
                return put([...keys], `Recommended tweaks applied to ${site.primary_domain}`)
              }}
            >
              Apply recommended
            </ActionButton>
            <ActionButton
              variant="default"
              disabled={!catalogue}
              // In the catalogue's order, as the legacy list read them.
              run={() => put((catalogue || []).filter((o) => on.has(o.key)).map((o) => o.key), `Tweaks saved for ${site.primary_domain}`)}
            >
              Save tweaks
            </ActionButton>
          </div>
        )}
        <Note>
          What performance plugins do on top of caching, applied by a WPGenie must-use plugin that site code can't change or switch off. No plugin to
          install or keep updated; the page cache is purged when they change.
        </Note>
      </div>
    </Section>
  )
}
