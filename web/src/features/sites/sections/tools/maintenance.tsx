import { useState } from "react"
import { ConstructionIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { ActionButton, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { queryClient, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { Note } from "../speed/shared"
import { MAX_MESSAGE, type Maintenance } from "./types"

// Maintenance mode: visitors see a "back soon" page (PUT
// /sites/{id}/tools/maintenance); signed-in editors see the site.

export function MaintenanceCard({ site }: { site: Site }) {
  const s = useSession()
  const path = `/sites/${site.id}/tools/maintenance`
  const q = useApi<Maintenance>(path, { refetchOnWindowFocus: false })
  const [draft, setDraft] = useState<string | null>(null)
  const message = draft ?? q.data?.message ?? ""
  const can = s.canChangeSite(site)

  const save = async (on: boolean) => {
    if (on && !q.data?.on) {
      const ok = await ask(
        `Put ${site.primary_domain} into maintenance mode? Visitors see a maintenance page instead of the site until you turn it off. You and anyone signed in to WordPress who can edit posts still see the site, and wp-admin keeps working.`,
        { ok: "Turn on" }
      )
      if (!ok) return
    }
    const m = await api<Maintenance>("PUT", path, { on, message: on ? message : "" })
    queryClient.setQueryData([path], m)
    setDraft(null)
    notify(on ? (q.data?.on ? "Maintenance message saved" : `${site.primary_domain} is in maintenance mode`) : `${site.primary_domain} is open to visitors again`)
  }

  return (
    <Section
      icon={ConstructionIcon}
      tint="orange"
      title={
        <>
          Maintenance mode
          {q.data?.on && <Badge variant="destructive">On</Badge>}
        </>
      }
      description="Close the site to visitors while you work on it: they see a short message saying it's down for maintenance, and search engines are told to come back later. You keep seeing the site while signed in to WordPress."
    >
      {q.isPending ? (
        <Skeleton className="h-24 rounded-xl" />
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <div className="flex flex-col gap-4">
          <Field>
            <FieldLabel htmlFor={`maint-msg-${site.id}`}>Message for visitors</FieldLabel>
            <Textarea
              id={`maint-msg-${site.id}`}
              value={message}
              onChange={(e) => setDraft(e.target.value)}
              maxLength={MAX_MESSAGE}
              rows={3}
              disabled={!can}
              placeholder="We're doing some maintenance on this site. Please check back soon."
            />
            <FieldDescription>
              Optional, up to {MAX_MESSAGE} characters ({message.length} used). Shown under the site's title; left empty, the sentence above is used.
            </FieldDescription>
          </Field>
          {can && (
            <div className="flex flex-wrap gap-2">
              {q.data?.on ? (
                <>
                  <ActionButton variant="default" run={() => save(false)}>
                    Turn off maintenance mode
                  </ActionButton>
                  <ActionButton run={() => save(true)} disabled={draft === null || draft === q.data.message}>
                    Save message
                  </ActionButton>
                </>
              ) : (
                <ActionButton variant="default" run={() => save(true)}>
                  Turn on maintenance mode
                </ActionButton>
              )}
            </div>
          )}
          <Note>
            The page cache is emptied when you turn it on or off, so nobody gets an old copy of a page. Scheduled tasks, wp-admin and the sign-in page keep
            working.
          </Note>
        </div>
      )}
    </Section>
  )
}
