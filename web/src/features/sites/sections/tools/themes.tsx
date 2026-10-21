import { useState } from "react"
import { PaletteIcon, PlusIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { SimpleTable } from "@/components/app/data-table"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { DIVI_SLUG, useDivi } from "@/lib/divi"
import { plural } from "@/lib/format"
import { startJob } from "@/lib/jobs"
import { invalidate, useApi } from "@/lib/query"
import { href, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { Note } from "../speed/shared"
import type { Theme } from "./types"

// Themes: switch, update (through the update manager, which checks the
// site afterwards and undoes a breaking update), delete unused ones and
// install from WordPress.org (GET/POST /sites/{id}/tools/themes, …).

const STATUS: Record<string, { label: string; variant: "default" | "secondary" | "outline" }> = {
  active: { label: "Active", variant: "default" },
  parent: { label: "Parent of the active theme", variant: "secondary" },
}

export function ThemesCard({ site }: { site: Site }) {
  const s = useSession()
  const can = s.canChangeSite(site)
  const path = `/sites/${site.id}/tools/themes`
  const q = useApi<Theme[]>(path)
  const [installing, setInstalling] = useState(false)
  const themes = q.data ?? []
  const updates = themes.filter((t) => t.update_version).length
  // The host's Divi license: offered until the site has Divi.
  const divi = useDivi().data
  const offerDivi = can && !!divi?.configured && !!q.data && !themes.some((t) => t.slug === DIVI_SLUG)

  const installDivi = async () => {
    if (
      !(await ask(
        `Install Divi on ${site.primary_domain}? Divi becomes the active theme, so every page changes look at once. It's licensed for updates and premade layouts.`,
        { ok: "Install Divi" }
      ))
    )
      return
    await startJob("POST", `/sites/${site.id}/divi`, undefined, async (v) => {
      if (v.job.status === "succeeded") notify(`Divi is now the active theme on ${site.primary_domain}`)
      await invalidate(path)
    })
    notify("Installing Divi…")
  }

  const activate = async (t: Theme) => {
    if (!(await ask(`Switch ${site.primary_domain} to ${t.title}? Every page changes look at once; the page cache is emptied.`, { ok: "Switch theme" })))
      return
    await api("POST", `${path}/${encodeURIComponent(t.slug)}/activate`)
    notify(`${t.title} is now the active theme`)
    await invalidate(path)
  }

  const update = async (t: Theme) => {
    await api("POST", `/sites/${site.id}/updates`, { themes: [t.slug] })
    notify(`Updating ${t.title} to ${t.update_version}: WPGenie checks the site afterwards and undoes the update if it breaks it.`)
    await invalidate(`/sites/${site.id}/updates`)
    setTimeout(() => invalidate(path), 15_000)
  }

  const remove = async (t: Theme) => {
    if (!(await ask(`Delete the theme ${t.title}? Its files are removed from the site. Install it again from WordPress.org if you need it back.`))) return
    await api("DELETE", `${path}/${encodeURIComponent(t.slug)}`)
    notify(`${t.title} deleted`)
    await invalidate(path)
  }

  return (
    <Section
      icon={PaletteIcon}
      tint="pink"
      title="Themes"
      description={
        q.data
          ? `${plural(themes.length, "theme")} installed${updates ? `, ${plural(updates, "update")} available` : ""}. Keep one spare default theme in case the active one breaks, and delete the rest: unused themes can still be attacked through their files.`
          : undefined
      }
      action={
        can && (
          <div className="flex flex-wrap gap-2">
            {offerDivi && <ActionButton run={installDivi}>Install Divi</ActionButton>}
            <Button onClick={() => setInstalling(true)}>
              <PlusIcon data-icon="inline-start" />
              Install theme
            </Button>
          </div>
        )
      }
    >
      {q.isPending ? (
        <div className="flex flex-col gap-2">
          <Skeleton className="h-10 rounded-xl" />
          <Skeleton className="h-10 rounded-xl" />
        </div>
      ) : q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="py-6 shadow-none" />
      ) : (
        <SimpleTable
          headers={["Theme", "Version", ""]}
          empty="No themes found."
          rowKey={(i) => themes[i].slug}
          rows={themes.map((t) => [
            <div className="flex flex-col gap-1">
              <div className="flex flex-wrap items-center gap-1.5">
                <strong className="font-semibold">{t.title}</strong>
                {STATUS[t.status] && <Badge variant={STATUS[t.status].variant}>{STATUS[t.status].label}</Badge>}
              </div>
              {t.title !== t.slug && <code className="text-xs font-normal text-muted-foreground">{t.slug}</code>}
            </div>,
            <div className="flex flex-col gap-0.5">
              <span className="tabular-nums">{t.version || "–"}</span>
              {t.update_version && <span className="text-xs text-warning">{t.update_version} available</span>}
            </div>,
            can ? (
              <div className="flex flex-wrap justify-end gap-2">
                {t.update_version && (
                  <ActionButton size="sm" variant="default" run={() => update(t)}>
                    Update
                  </ActionButton>
                )}
                {t.status !== "active" && (
                  <ActionButton size="sm" run={() => activate(t)}>
                    Activate
                  </ActionButton>
                )}
                {/* Never the active theme nor its parent (the server refuses too). */}
                {t.status === "inactive" && (
                  <ActionButton size="sm" variant="destructive" run={() => remove(t)}>
                    Delete
                  </ActionButton>
                )}
              </div>
            ) : null,
          ])}
        />
      )}
      <Note className="mt-3">
        Updates are checked and undone like plugin updates: see <a href={href(sitePath(site.id, "updates"))}>Updates</a> for their history.
      </Note>
      <InstallTheme site={site} open={installing} onOpenChange={setInstalling} />
    </Section>
  )
}

function InstallTheme({ site, open, onOpenChange }: { site: Site; open: boolean; onOpenChange: (o: boolean) => void }) {
  const id = `theme-slug-${site.id}`
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Install a theme on ${site.primary_domain}`}
      intro="From the free WordPress.org directory. It's installed, not switched on: activate it when you're ready."
      ok="Install"
      onSubmit={async (f) => {
        const slug = String(f.get("slug") ?? "").trim()
        await startJob("POST", `/sites/${site.id}/tools/themes`, { slug }, async (v) => {
          if (v.job.status === "succeeded") notify(`${slug} installed`)
          await invalidate(`/sites/${site.id}/tools/themes`)
        })
        notify(`Installing ${slug}…`)
      }}
    >
      <Field>
        <FieldLabel htmlFor={id}>Theme name on WordPress.org</FieldLabel>
        <Input
          id={id}
          name="slug"
          required
          maxLength={100}
          pattern="[a-z0-9][a-z0-9\-]*"
          autoComplete="off"
          spellCheck={false}
          autoFocus
          placeholder="astra"
        />
        <FieldDescription>
          As in the theme's address: for wordpress.org/themes/<strong>twentytwentyfive</strong>/ it's twentytwentyfive. Lowercase letters, digits and
          dashes.
        </FieldDescription>
      </Field>
    </FormDialog>
  )
}
