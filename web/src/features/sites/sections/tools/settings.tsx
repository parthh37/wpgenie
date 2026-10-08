import { useMemo, useState } from "react"
import { Settings2Icon } from "lucide-react"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, LoadError } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { SwitchRow } from "../speed/shared"
import type { WPSettings } from "./types"

// A few WordPress settings without going to wp-admin (GET/PUT
// /sites/{id}/tools/settings): title, tagline, time zone, search engine
// visibility and comments on new posts.

// The time zones this browser knows (WordPress knows the same IANA names).
function timeZones(current: string): string[] {
  let zones: string[] = []
  try {
    zones = Intl.supportedValuesOf("timeZone")
  } catch {
    /* an old browser: the current zone and UTC only */
  }
  const out = ["UTC", ...zones.filter((z) => z !== "UTC")]
  if (current && !out.includes(current)) out.unshift(current)
  return out
}

const offsetLabel = (h: number) => {
  const sign = h < 0 ? "−" : "+"
  const abs = Math.abs(h)
  const min = Math.round((abs % 1) * 60)
  return `UTC${sign}${Math.floor(abs)}${min ? `:${String(min).padStart(2, "0")}` : ""}`
}

export function SettingsCard({ site }: { site: Site }) {
  const s = useSession()
  const can = s.canChangeSite(site)
  const path = `/sites/${site.id}/tools/settings`
  const q = useApi<WPSettings>(path, { refetchOnWindowFocus: false })
  const [draft, setDraft] = useState<Partial<WPSettings>>({})
  const cur = q.data
  const v = { ...cur, ...draft } as WPSettings
  const zones = useMemo(() => timeZones(cur?.timezone ?? ""), [cur?.timezone])
  const changed = !!cur && (Object.keys(draft) as (keyof WPSettings)[]).some((k) => draft[k] !== cur[k])
  const set = <K extends keyof WPSettings>(k: K, val: WPSettings[K]) => setDraft((d) => ({ ...d, [k]: val }))
  const id = (f: string) => `wp-set-${f}-${site.id}`

  const save = async () => {
    if (!cur) return
    const body: Record<string, unknown> = {}
    for (const k of ["title", "tagline", "timezone", "discourage_search", "comments_open"] as const) {
      if (k in draft && draft[k] !== cur[k]) body[k] = draft[k]
    }
    const next = await api<WPSettings>("PUT", path, body)
    queryClient.setQueryData([path], next)
    setDraft({})
    notify(`WordPress settings saved for ${site.primary_domain}`)
  }

  return (
    <Section
      icon={Settings2Icon}
      tint="graphite"
      title="WordPress settings"
      description="The basics from WordPress's Settings screens, without signing in to wp-admin."
    >
      {q.isPending ? (
        <div className="grid gap-4 sm:grid-cols-2">
          <Skeleton className="h-16 rounded-xl" />
          <Skeleton className="h-16 rounded-xl" />
        </div>
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <div className="flex flex-col gap-5">
          <div className="grid gap-4 sm:grid-cols-2">
            <Field>
              <FieldLabel htmlFor={id("title")}>Site title</FieldLabel>
              <Input id={id("title")} value={v.title} maxLength={200} disabled={!can} onChange={(e) => set("title", e.target.value)} />
            </Field>
            <Field>
              <FieldLabel htmlFor={id("tagline")}>Tagline</FieldLabel>
              <Input
                id={id("tagline")}
                value={v.tagline}
                maxLength={300}
                disabled={!can}
                placeholder="In a few words, what this site is about"
                onChange={(e) => set("tagline", e.target.value)}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor={id("tz")}>Time zone</FieldLabel>
              <NativeSelect id={id("tz")} className="w-full" value={v.timezone} disabled={!can} onChange={(e) => set("timezone", e.target.value)}>
                {!v.timezone && <NativeSelectOption value="">{offsetLabel(cur?.utc_offset ?? 0)} (a fixed offset)</NativeSelectOption>}
                {zones.map((z) => (
                  <NativeSelectOption key={z} value={z}>
                    {z.replace(/_/g, " ")}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
              <FieldDescription>Used for post dates and scheduled posts. A city follows daylight saving time; a fixed offset doesn't.</FieldDescription>
            </Field>
          </div>
          <div className="flex flex-col gap-4">
            <SwitchRow
              checked={v.discourage_search}
              disabled={!can}
              onChange={(c) => set("discourage_search", c)}
              title="Ask search engines not to index this site"
            >
              For a site that isn't ready yet. Search engines usually respect it; it doesn't hide the site from anyone.
            </SwitchRow>
            <SwitchRow checked={v.comments_open} disabled={!can} onChange={(c) => set("comments_open", c)} title="Allow comments on new posts">
              Existing posts keep their own setting.
            </SwitchRow>
          </div>
          {can && (
            <div className="flex flex-wrap gap-2">
              <ActionButton variant="default" run={save} disabled={!changed}>
                Save settings
              </ActionButton>
              {changed && (
                <ActionButton variant="ghost" run={() => setDraft({})}>
                  Undo changes
                </ActionButton>
              )}
            </div>
          )}
        </div>
      )}
    </Section>
  )
}
