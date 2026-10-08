import { useState } from "react"
import { LockKeyholeIcon, LogOutIcon } from "lucide-react"
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

// WordPress hardening: protection inside WordPress itself (hidden
// usernames, vague sign-in errors, the administrator lock…), applied by a
// must-use plugin (PUT /sites/{id}/hardening); and signing everyone out
// (POST /sites/{id}/hardening/sign-out). Both are protection changes:
// managers only when the site is shared.

interface HardeningOption {
  key: string
  title: string
  description: string
  // Default: new sites get it, and "Apply recommended" turns it on.
  default: boolean
}

export function HardeningCard({ site }: { site: Site }) {
  const s = useSession()
  const can = site.status === "active" && s.canChangeSite(site, "manager")
  // The catalogue is the same for every site: loaded once.
  const { data: catalogue, error, isLoading, refetch } = useApi<HardeningOption[]>("/hardening")
  const saved = site.harden || []
  const savedKey = saved.join(",")
  const [on, setOn] = useState(() => new Set(saved))
  const [seen, setSeen] = useState(savedKey)
  if (seen !== savedKey) {
    setSeen(savedKey)
    setOn(new Set(saved))
  }

  const put = async (keys: string[], msg: string) => {
    await api("PUT", `/sites/${site.id}/hardening`, { hardening: keys })
    notify(msg)
    // The health analysis judges these too: it reruns.
    await invalidate("/sites")
  }

  const signOut = async () => {
    if (
      !(await ask(
        `Sign everyone out of ${site.primary_domain}? Every WordPress sign-in ends at once: administrators, editors and customers have to sign in again with their passwords. Password-reset links already sent stop working. Do this if you think someone else got in; then change the passwords you're unsure of. Nothing else on the site changes.`,
        { ok: "Sign everyone out", danger: true }
      ))
    )
      return
    await api("POST", `/sites/${site.id}/hardening/sign-out`)
    notify(`Everyone was signed out of ${site.primary_domain}`)
    await invalidate(`/sites/${site.id}/events`)
  }

  return (
    <Section
      icon={LockKeyholeIcon}
      tint="green"
      title="WordPress hardening"
      description="Protection inside WordPress itself, where the firewall can't see: who can sign in, how, and what they can change."
    >
      <div className="flex flex-col gap-4">
        {isLoading ? (
          <>
            <Skeleton className="h-10 rounded-xl" />
            <div className="grid gap-3 sm:grid-cols-2">
              {Array.from({ length: 8 }, (_, i) => (
                <Skeleton key={i} className="h-14" />
              ))}
            </div>
          </>
        ) : error ? (
          <LoadError error={error} retry={() => refetch()} className="py-8" />
        ) : (
          <>
            <Score catalogue={catalogue || []} saved={saved} />
            <div className="grid gap-x-6 gap-y-4 sm:grid-cols-2">
              {(catalogue || []).map((o) => (
                <label key={o.key} className={cn("flex items-start gap-3", can ? "cursor-pointer" : "cursor-not-allowed")}>
                  <Checkbox
                    className="mt-0.5"
                    checked={on.has(o.key)}
                    disabled={!can}
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
          </>
        )}
        {can && (
          <div className="flex flex-wrap gap-2">
            <ActionButton
              disabled={!catalogue}
              run={() => {
                const keys = new Set([...saved, ...(catalogue || []).filter((o) => o.default).map((o) => o.key)])
                return put(
                  (catalogue || []).filter((o) => keys.has(o.key)).map((o) => o.key),
                  `Recommended hardening applied to ${site.primary_domain}`
                )
              }}
            >
              Apply recommended
            </ActionButton>
            <ActionButton
              variant="default"
              disabled={!catalogue}
              run={() => put((catalogue || []).filter((o) => on.has(o.key)).map((o) => o.key), `Hardening saved for ${site.primary_domain}`)}
            >
              Save hardening
            </ActionButton>
          </div>
        )}
        <Note>
          Applied by a WPGenie must-use plugin that site code can't change or switch off. What you do from this panel (adding administrators,
          updates) always works.
        </Note>

        {can && (
          <div className="flex flex-wrap items-center gap-3 border-t border-border pt-4">
            <span className="flex min-w-0 flex-1 basis-64 flex-col gap-0.5">
              <span className="text-sm font-medium">Sign everyone out</span>
              <span className="text-[0.8125rem] leading-snug text-muted-foreground">
                Ends every WordPress sign-in on this site at once: anyone who got in with a stolen password or cookie is out.
              </span>
            </span>
            <ActionButton variant="destructive" run={signOut}>
              <LogOutIcon data-icon="inline-start" />
              Sign everyone out
            </ActionButton>
          </div>
        )}
      </div>
    </Section>
  )
}

// Score: how many settings are on, as a bar with its words (never colour
// alone). Green once every recommended one is.
function Score({ catalogue, saved }: { catalogue: HardeningOption[]; saved: string[] }) {
  const total = catalogue.length
  const count = catalogue.filter((o) => saved.includes(o.key)).length
  const missing = catalogue.filter((o) => o.default && !saved.includes(o.key)).length
  const tone = count === 0 ? "bad" : missing > 0 ? "warn" : "ok"
  const pct = total ? Math.round((count / total) * 100) : 0
  const said =
    count === 0
      ? "Nothing on yet: Apply recommended turns on what suits almost every site."
      : missing > 0
        ? `${missing} recommended ${missing === 1 ? "setting is" : "settings are"} off.`
        : "Every recommended setting is on."
  return (
    <div className="flex flex-col gap-2 rounded-xl bg-muted/50 p-3.5">
      <div className="flex items-baseline justify-between gap-3">
        <span className="text-sm font-medium">{said}</span>
        <span
          className={cn(
            "shrink-0 text-sm font-semibold tabular-nums",
            tone === "ok" ? "text-success" : tone === "warn" ? "text-warning" : "text-danger"
          )}
        >
          {count} of {total} on
        </span>
      </div>
      <span
        role="meter"
        aria-label="WordPress hardening settings on"
        aria-valuemin={0}
        aria-valuemax={total}
        aria-valuenow={count}
        aria-valuetext={`${count} of ${total} on`}
        className="relative block h-2 overflow-hidden rounded-full bg-muted"
      >
        <span
          className={cn(
            "vital-fill absolute inset-y-0 left-0 rounded-full bg-linear-to-r",
            tone === "ok"
              ? "from-success-fill/75 to-success-fill"
              : tone === "warn"
                ? "from-warning-fill/75 to-warning-fill"
                : "from-danger-fill/75 to-danger-fill"
          )}
          style={{ width: `${pct}%` }}
        />
      </span>
    </div>
  )
}
