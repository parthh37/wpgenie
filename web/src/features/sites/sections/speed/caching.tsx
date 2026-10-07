import { useState } from "react"
import { ImageIcon, RocketIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { startJob } from "@/lib/jobs"
import { invalidate } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { FlashButton, followSiteJob, Note, SwitchRow, type JobID } from "./shared"

// Speed: the page and object caches, purging, and smaller image formats.

interface CacheState {
  page: boolean
  object: boolean
  mobile: boolean
}

export function CacheCard({ site }: { site: Site }) {
  const s = useSession()
  const active = site.status === "active"
  const saved: CacheState = { page: site.page_cache, object: site.object_cache, mobile: site.cache_mobile }
  const [cache, setCache] = useState(saved)
  const [busy, setBusy] = useState(false)

  // The saved settings changed: show them.
  const key = `${saved.page}|${saved.object}|${saved.mobile}`
  const [seen, setSeen] = useState(key)
  if (seen !== key) {
    setSeen(key)
    setCache(saved)
  }

  const locked = !active || !s.canChange || busy
  const change = async (next: CacheState) => {
    setCache(next)
    setBusy(true)
    try {
      await api("PUT", `/sites/${site.id}/cache`, { page_cache: next.page, object_cache: next.object, mobile: next.mobile })
    } catch (e) {
      showError(e)
      setCache(saved)
    }
    await invalidate("/sites")
    setBusy(false)
  }

  return (
    <Section
      icon={RocketIcon}
      tint="blue"
      title="Speed"
      action={
        s.canChange && (
          <FlashButton
            done="Purged ✓"
            disabled={!active}
            run={async () => {
              await api("POST", `/sites/${site.id}/cache/purge`)
              notify(`Cache purged on ${site.primary_domain}`)
            }}
          >
            Purge cache
          </FlashButton>
        )
      }
    >
      <div className="flex flex-col gap-4">
        <div className="grid gap-4 sm:grid-cols-3">
          <SwitchRow checked={cache.page} disabled={locked} onChange={(v) => change({ ...cache, page: v })} title="Page cache" />
          <SwitchRow
            checked={cache.mobile}
            disabled={locked || !site.page_cache}
            onChange={(v) => change({ ...cache, mobile: v })}
            title="Separate mobile cache"
          />
          <SwitchRow checked={cache.object} disabled={locked} onChange={(v) => change({ ...cache, object: v })} title="Object cache (Redis)" />
        </div>
        <Note>
          Pages that ask WordPress whether the visitor is on a phone are always cached separately for phones and computers;{" "}
          <em>separate mobile cache</em> does it for every page, for themes that detect phones on their own. Editors also get a <em>Purge cache</em>{" "}
          button in WordPress's admin bar.
        </Note>
      </div>
    </Section>
  )
}

export function ImagesCard({ site }: { site: Site }) {
  const s = useSession()
  const active = site.status === "active"
  const saved = (site.image_formats || []).join(",")
  const [value, setValue] = useState(saved)
  const [busy, setBusy] = useState(false)
  const [converting, setConverting] = useState(false)

  const [seen, setSeen] = useState(saved)
  if (seen !== saved) {
    setSeen(saved)
    setValue(saved)
  }

  const change = async (v: string) => {
    setValue(v)
    setBusy(true)
    try {
      const res = await api<{ job_id?: JobID }>("PUT", `/sites/${site.id}/images`, { formats: v ? v.split(",") : [] })
      // Converting what's there already runs as a job (0: nothing to do).
      if (res?.job_id) followSiteJob(res.job_id)
    } catch (e) {
      showError(e)
      setValue(saved)
    }
    await invalidate("/sites")
    setBusy(false)
  }

  const convert = async () => {
    setConverting(true)
    try {
      await startJob("POST", `/sites/${site.id}/images/convert`, undefined, () => setConverting(false))
    } catch (e) {
      showError(e)
      setConverting(false)
    }
  }

  return (
    <Section icon={ImageIcon} tint="teal" title="Images">
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-end gap-3">
          <Field className="w-auto">
            <FieldLabel htmlFor={`images-${site.id}`} className="sr-only">
              Images
            </FieldLabel>
            <NativeSelect id={`images-${site.id}`} value={value} disabled={!active || !s.canChange || busy} onChange={(e) => change(e.target.value)}>
              <NativeSelectOption value="">Originals only</NativeSelectOption>
              <NativeSelectOption value="webp">WebP</NativeSelectOption>
              <NativeSelectOption value="avif,webp">AVIF, else WebP</NativeSelectOption>
            </NativeSelect>
          </Field>
          {s.canChange && value && (
            <Button type="button" variant="tinted" disabled={!active || converting} onClick={convert}>
              Convert now
            </Button>
          )}
        </div>
        <Note>
          JPEG and PNG uploads get smaller AVIF/WebP copies, served to browsers that accept them at the same URL. New uploads are converted within a
          minute, everything else nightly. AVIF is the smallest but slow to make.
        </Note>
      </div>
    </Section>
  )
}
