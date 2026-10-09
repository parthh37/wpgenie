import { lazy, Suspense, useEffect, useRef, useState } from "react"
import { ChevronLeftIcon, ExternalLinkIcon, LayoutDashboardIcon, UsersIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Skeleton } from "@/components/ui/skeleton"
import { IconTile } from "@/components/app/icon-tile"
import { Page } from "@/components/app/page"
import { StatusPill } from "@/components/app/status"
import { PageSpinner } from "@/app/shell"
import { ScreenBoundary } from "@/components/app/error-boundary"
import { useClustered, useNodes, useSite } from "@/lib/query"
import { href, navigate, sitePath } from "@/lib/router"
import { ACCESS_LABELS, useSession } from "@/lib/session"
import { cn } from "@/lib/utils"
import { SHIELD_LABELS, useAttack } from "./data"
import { SiteAvatar } from "./list"
import { RAIL_GROUPS, resolveSection, sectionsFor } from "./sections"

const ShareDialog = lazy(() => import("./sections/sharing").then((m) => ({ default: m.ShareDialog })))

// One site, full page: its header, the sections in a rail (grouped, as in
// Settings), and the open section.
export function SiteWorkspace({ id, section }: { id: string; section?: string }) {
  const { site, data: sites, isLoading } = useSite(id)
  const clustered = useClustered()
  // Whether there's a Server section depends on the nodes: wait for them
  // before deciding an address names a section the site doesn't have.
  const nodesLoaded = !useNodes().isLoading
  const backRef = useRef<HTMLAnchorElement>(null)

  // A site that isn't in the list (deleted, or not yours): back to the list.
  useEffect(() => {
    if (sites && !site) navigate("/sites", { replace: true })
  }, [sites, site])

  // Opening a site focuses "All sites"; Escape goes back.
  useEffect(() => {
    backRef.current?.focus({ preventScroll: true })
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || document.querySelector("[role=dialog],[role=alertdialog]")) return
      if ((e.target as HTMLElement | null)?.closest?.("input, textarea, select, [contenteditable]")) return
      navigate("/sites")
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [id])

  const current = site ? resolveSection(site, clustered, section) : undefined
  // An address naming a section the site doesn't have: the overview's address.
  useEffect(() => {
    if (site && current && nodesLoaded && section && current.key !== section && !(section === "security" && current.key === "protection"))
      navigate(sitePath(site.id, current.key), { replace: true })
  }, [site, current, section, nodesLoaded])

  if (isLoading || !site || !current) {
    return (
      <Page>
        <Skeleton className="mb-6 h-16 w-80 rounded-2xl" />
        <Skeleton className="h-96 rounded-2xl" />
      </Page>
    )
  }

  const sections = sectionsFor(site, clustered)
  const Section = current.component

  return (
    <Page>
      <a
        ref={backRef}
        href={href("sites")}
        className="mb-3 -ml-1.5 inline-flex items-center gap-1 rounded-full py-0.5 pr-2.5 pl-1 text-[0.9375rem] font-medium text-link no-underline hover:no-underline focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
      >
        <ChevronLeftIcon className="size-5" />
        All sites
      </a>
      <SiteHeader siteId={site.id} />

      <div className="grid gap-6 lg:grid-cols-[13.5rem_minmax(0,1fr)]">
        <nav
          aria-label="Site sections"
          className="-mx-4 flex gap-1 overflow-x-auto px-4 pb-1 lg:sticky lg:top-6 lg:mx-0 lg:flex-col lg:self-start lg:overflow-visible lg:px-0"
        >
          {RAIL_GROUPS.map((g) => {
            const items = sections.filter((x) => x.group === g)
            if (!items.length) return null
            return (
              <div key={g || "top"} className="contents lg:flex lg:flex-col lg:gap-0.5">
                {g && <p aria-hidden className="mt-3 mb-1 hidden px-2.5 text-xs font-semibold text-muted-foreground lg:block">{g}</p>}
                {items.map((x) => (
                  <a
                    key={x.key}
                    href={href(sitePath(site.id, x.key))}
                    aria-current={x.key === current.key ? "page" : undefined}
                    onClick={(e) => {
                      // Changing section replaces the address: Back leaves the site.
                      e.preventDefault()
                      navigate(sitePath(site.id, x.key), { replace: true })
                    }}
                    className={cn(
                      "flex shrink-0 items-center gap-2.5 rounded-xl px-2.5 py-1.5 text-sm whitespace-nowrap text-foreground no-underline transition-colors hover:bg-accent hover:no-underline",
                      x.key === current.key && "bg-card font-semibold card-shadow hover:bg-card"
                    )}
                  >
                    <IconTile icon={x.icon} tint={x.tint} size="xs" />
                    {x.label}
                  </a>
                ))}
              </div>
            )
          })}
        </nav>

        <section aria-label={current.label} className="min-w-0">
          {current.key !== "overview" && (
            <h2 className="mb-4 flex items-center gap-2.5 font-heading text-lg font-semibold">
              <IconTile icon={current.icon} tint={current.tint} size="md" />
              {current.label}
            </h2>
          )}
          <ScreenBoundary key={site.id + current.key}>
            <Suspense fallback={<PageSpinner />}>
              <Section site={site} />
            </Suspense>
          </ScreenBoundary>
        </section>
      </div>
    </Page>
  )
}

function SiteHeader({ siteId }: { siteId: string }) {
  const { site } = useSite(siteId)
  const { data: attack } = useAttack(site!)
  const s = useSession()
  const [sharing, setSharing] = useState(false)
  if (!site) return null
  // Share, as in a document: for the site's owners and staff. A site
  // without an account opens Sharing, where staff give it one first.
  const canShare = !site.access && s.canChange
  const shared = site.shared_with ?? 0
  const url = "https://" + site.primary_domain
  return (
    <header className="mb-6 flex flex-wrap items-center gap-x-4 gap-y-3">
      <SiteAvatar site={site} size="lg" />
      <div className="min-w-0 flex-1">
        <h1 className="truncate font-heading text-2xl leading-tight font-semibold">{site.primary_domain}</h1>
        <div className="mt-1 flex flex-wrap items-center gap-1.5 text-sm text-muted-foreground">
          <span className="font-mono text-xs">{site.id}</span>
          <span>· PHP {site.php_version}</span>
          <StatusPill status={site.status} />
          {site.access && (
            <Badge variant="secondary" className="bg-primary/12 text-primary" title="Its owner shared it with you">
              Shared with you · {ACCESS_LABELS[site.access]}
            </Badge>
          )}
          <Badge
            variant="secondary"
            className={cn(
              attack?.active || site.shield_mode === "under_attack" ? "bg-danger-fill/16 text-danger" : site.shield_mode === "off" && "bg-warning-fill/16 text-warning"
            )}
          >
            {attack?.active ? "Under attack" : SHIELD_LABELS[site.shield_mode] || site.shield_mode}
          </Badge>
        </div>
      </div>
      <div className="flex flex-wrap gap-2">
        {canShare && (
          <Button
            onClick={() => (site.account_id ? setSharing(true) : navigate(sitePath(site.id, "sharing"), { replace: true }))}
            title={shared ? `Shared with ${shared} ${shared === 1 ? "person" : "people"}` : "Give someone else access to this site"}
          >
            <UsersIcon data-icon="inline-start" />
            {shared ? `Shared · ${shared}` : "Share"}
          </Button>
        )}
        <Button variant="tinted" render={<a href={url} target="_blank" rel="noopener" />} nativeButton={false}>
          <ExternalLinkIcon data-icon="inline-start" />
          Visit
        </Button>
        <Button variant="tinted" render={<a href={url + "/wp-admin/"} target="_blank" rel="noopener" />} nativeButton={false}>
          <LayoutDashboardIcon data-icon="inline-start" />
          WP Admin
        </Button>
      </div>
      {sharing && (
        <Suspense fallback={null}>
          <ShareDialog site={site} open={sharing} onOpenChange={setSharing} />
        </Suspense>
      )}
    </header>
  )
}
