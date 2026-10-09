import { Fragment, Suspense, useCallback, useEffect, useMemo } from "react"
import { ChevronRightIcon, LogOutIcon, MoonIcon, SearchIcon, SunIcon, UserIcon } from "lucide-react"
import {
  Sidebar, SidebarContent, SidebarGroup, SidebarGroupContent, SidebarGroupLabel, SidebarHeader,
  SidebarInset, SidebarMenu, SidebarMenuBadge, SidebarMenuButton, SidebarMenuItem, SidebarProvider, SidebarTrigger, useSidebar,
} from "@/components/ui/sidebar"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuGroup, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Kbd } from "@/components/ui/kbd"
import { Spinner } from "@/components/ui/spinner"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { ScreenBoundary } from "@/components/app/error-boundary"
import { Logo } from "@/components/app/logo"
import { Toaster, showError } from "@/components/app/toaster"
import { JobTray } from "@/app/job-tray"
import { CommandPalette, openPalette } from "@/app/command-palette"
import { NewSiteDialog } from "@/features/sites/new-site"
import { SECTIONS } from "@/features/sites/sections"
import { GROUP_LABEL, PAGES, type NavGroup, type PageDef } from "@/app/pages"
import { useAlertsFiring, useNavFlags, useSupportSummary, useTenantBilling, useUpdateAvailable } from "@/app/nav-state"
import { pollJobs, stopJobs } from "@/lib/jobs"
import { useSite } from "@/lib/query"
import { href, navigate, sitePath, useLocation, useRoute } from "@/lib/router"
import { useSession } from "@/lib/session"
import { toggleTheme, useTheme } from "@/lib/theme"
import { humanize } from "@/lib/format"

// The panel's frame, as Cloudflare's dashboard: a white sidebar of grouped
// pages on the left, a top bar over the page (where you are, search, theme
// and your account), and the page under it.

const GROUPS: NavGroup[] = ["", "operate", "business", "infrastructure"]
const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform)

export function AppShell() {
  const s = useSession()
  const flags = useNavFlags(s)
  const billing = useTenantBilling(s)
  const [page] = useRoute()
  const location = useLocation()
  const visible = useMemo(() => PAGES.filter((p) => p.visible(s, flags)), [s, flags.support, flags.billing]) // eslint-disable-line react-hooks/exhaustive-deps
  // Whether a page's visibility is settled: Support waits for its summary,
  // a tenant's Billing for their account's billing.
  const decided = useCallback(
    (key: string) => (key === "support" ? flags.support !== null : key === "billing" && s.isTenant ? !billing.isLoading : !s.loading),
    [flags.support, billing.isLoading, s.isTenant, s.loading]
  )

  // Jobs follow the session.
  useEffect(() => {
    pollJobs()
    return stopJobs
  }, [])

  // A page this user can't open (or none) is Sites; an account that must
  // pay goes to Billing; a panel that requires 2FA, to Account until it's on.
  const mustPay = billing.data?.mustPay
  useEffect(() => {
    if (s.mustSetup2FA) {
      if (page !== "account") navigate("/account", { replace: true })
      return
    }
    if (mustPay && (!page || page === "sites")) navigate("/billing", { replace: true })
    else if (!page) navigate("/sites", { replace: true })
    // A page this user doesn't get (once it's known): its address becomes
    // Sites', as the page shown is.
    else if (!PAGES.some((p) => p.key === page) || (decided(page) && !visible.some((p) => p.key === page)))
      navigate("/sites", { replace: true })
  }, [page, s.mustSetup2FA, mustPay, visible, decided])

  // Switching pages clears errors about the last one and scrolls to the top.
  useEffect(() => {
    showError(null)
    window.scrollTo(0, 0)
  }, [page])

  const current: PageDef =
    (s.mustSetup2FA ? PAGES.find((p) => p.key === "account") : visible.find((p) => p.key === page)) ??
    // Not known yet whether this user gets the page: show it rather than
    // flash Sites (the server still decides what it shows).
    (!decided(page ?? "") ? PAGES.find((p) => p.key === page) : undefined) ??
    PAGES[0]
  const Screen = current.component

  return (
    <SidebarProvider>
      <AppSidebar pages={visible} current={current.key} />
      <SidebarInset className="min-w-0">
        <TopBar current={current} />
        <main id="main" tabIndex={-1} className="flex-1 outline-none">
          <ScreenBoundary key={location}>
            <Suspense fallback={<PageSpinner />}>
              <Screen key={current.key} />
            </Suspense>
          </ScreenBoundary>
        </main>
      </SidebarInset>
      <CommandPalette pages={visible} />
      <NewSiteDialog />
      <JobTray />
      <Toaster withSidebar />
    </SidebarProvider>
  )
}

export function PageSpinner() {
  return (
    <div className="flex min-h-[50vh] items-center justify-center text-muted-foreground">
      <Spinner className="size-6" />
    </div>
  )
}

function AppSidebar({ pages, current }: { pages: PageDef[]; current: string }) {
  const s = useSession()
  const { setOpenMobile } = useSidebar()
  const support = useSupportSummary(s)
  const alerts = useAlertsFiring(s)
  const update = useUpdateAvailable(s)

  const badgeAt = "top-1/2! right-2 -translate-y-1/2"
  const badge = (key: string) => {
    if (key === "support" && support.data?.awaiting)
      return (
        <SidebarMenuBadge className={`${badgeAt} h-[18px] min-w-[18px] rounded-full bg-danger-fill px-1.5 text-[11px] font-semibold text-white peer-hover/menu-button:text-white peer-data-active/menu-button:text-white`}>
          {support.data.awaiting}
          <span className="sr-only"> {support.data.awaiting === 1 ? "ticket needs" : "tickets need"} your reply</span>
        </SidebarMenuBadge>
      )
    if (key === "monitoring" && alerts.data)
      return (
        <SidebarMenuBadge className={badgeAt}>
          <span className="size-2 rounded-full bg-danger-fill" />
          <span className="sr-only">(alerts firing)</span>
        </SidebarMenuBadge>
      )
    if (key === "system" && update.data)
      return (
        <SidebarMenuBadge className={badgeAt}>
          <span className="size-2 rounded-full bg-primary" />
          <span className="sr-only">(update available)</span>
        </SidebarMenuBadge>
      )
    return null
  }

  const item = (p: PageDef) => (
    <SidebarMenuItem key={p.key}>
      <SidebarMenuButton
        isActive={p.key === current}
        className="group/nav text-sidebar-foreground no-underline hover:no-underline data-active:font-medium"
        render={<a href={href(p.key)} onClick={() => setOpenMobile(false)} aria-current={p.key === current ? "page" : undefined} />}
      >
        <p.icon strokeWidth={1.75} className="text-muted-foreground group-data-active/nav:text-foreground" />
        <span>{p.label}</span>
      </SidebarMenuButton>
      {badge(p.key)}
    </SidebarMenuItem>
  )

  return (
    <Sidebar className="border-sidebar-border">
      <SidebarHeader className="h-[58px] shrink-0 flex-row items-center gap-2.5 border-b border-sidebar-border px-4 py-0">
        <a
          href={href("sites")}
          onClick={() => setOpenMobile(false)}
          className="flex items-center gap-2.5 rounded-md text-foreground no-underline hover:no-underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <Logo className="size-7" />
          <span className="text-lg font-semibold tracking-[-0.01em]">WPGenie</span>
        </a>
      </SidebarHeader>
      <SidebarContent className="gap-0 px-2 py-3">
        {GROUPS.map((g) => {
          const items = pages.filter((p) => p.group === g && p.key !== "account")
          if (!items.length) return null
          return (
            <SidebarGroup key={g || "main"} className="p-0">
              {g && <SidebarGroupLabel className="mt-4 mb-1 h-auto">{GROUP_LABEL(g, s)}</SidebarGroupLabel>}
              <SidebarGroupContent>
                <SidebarMenu>{items.map(item)}</SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          )
        })}
      </SidebarContent>
    </Sidebar>
  )
}

// The bar over every page: where you are, search, the theme, your account.
function TopBar({ current }: { current: PageDef }) {
  const s = useSession()
  const theme = useTheme()
  const [, siteId, section] = useRoute()
  const group = current.group ? GROUP_LABEL(current.group, s) : ""
  const name = s.me?.username ?? ""

  return (
    <header className="sticky top-0 z-30 flex h-[58px] shrink-0 items-center gap-2 border-b border-border bg-card px-3 sm:px-5">
      <SidebarTrigger aria-label="Open navigation" className="md:hidden" />
      <a href={href("sites")} aria-label="WPGenie" className="flex md:hidden">
        <Logo className="size-6" />
      </a>

      <nav aria-label="You are here" className="min-w-0 flex-1 max-md:sr-only">
        <ol className="m-0 flex min-w-0 list-none items-center gap-1.5 p-0 text-base">
          {group && (
            <>
              <li className="shrink-0 text-muted-foreground">{group}</li>
              <Crumbsep />
            </>
          )}
          <li className="min-w-0 truncate">
            {current.key === "sites" && siteId ? (
              <a href={href(current.key)} className="text-muted-foreground no-underline hover:text-foreground hover:no-underline">
                {current.label}
              </a>
            ) : (
              <span aria-current="page" className="font-medium text-foreground">
                {current.label}
              </span>
            )}
          </li>
          {current.key === "sites" && siteId && <SiteCrumbs id={siteId} section={section} />}
        </ol>
      </nav>
      <div className="flex-1 md:hidden" />

      <button
        type="button"
        onClick={openPalette}
        aria-label="Search or jump to"
        className="flex h-8 shrink-0 cursor-pointer items-center gap-2 rounded-lg bg-card px-2.5 text-sm text-muted-foreground shadow-xs ring-1 ring-border transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-sm:size-8 max-sm:justify-center max-sm:px-0 sm:w-56 lg:w-64"
      >
        <SearchIcon className="size-4 shrink-0" />
        <span className="flex-1 text-left max-sm:hidden">Search or jump to…</span>
        <Kbd className="max-sm:hidden">{isMac ? "⌘K" : "Ctrl K"}</Kbd>
      </button>

      <Tooltip>
        <TooltipTrigger
          render={
            <Button variant="ghost" size="icon-sm" onClick={toggleTheme} aria-label={theme === "light" ? "Switch to dark theme" : "Switch to light theme"} />
          }
        >
          {theme === "light" ? <MoonIcon /> : <SunIcon />}
        </TooltipTrigger>
        <TooltipContent>{theme === "light" ? "Dark theme" : "Light theme"}</TooltipContent>
      </Tooltip>

      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <button
              type="button"
              aria-label={`Your account (${name})`}
              className="flex h-8 shrink-0 cursor-pointer items-center gap-2 rounded-lg pr-1 pl-1 transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none data-popup-open:bg-accent lg:pr-2"
            />
          }
        >
          <span aria-hidden className="flex size-6 items-center justify-center rounded-full bg-fill text-xs font-semibold text-foreground">
            {name.slice(0, 1).toUpperCase()}
          </span>
          <span className="max-w-32 truncate text-base font-medium max-lg:hidden">{name}</span>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-60">
          <DropdownMenuGroup>
            <DropdownMenuLabel className="flex flex-col gap-0.5 px-2 py-1.5">
              <span className="truncate text-base font-medium text-foreground">{name}</span>
              <span className="text-xs text-muted-foreground">{s.me?.role ? humanize(s.me.role) : ""}</span>
            </DropdownMenuLabel>
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuItem render={<a href={href("account")} className="text-foreground no-underline hover:no-underline" />}>
            <UserIcon />
            Your account
          </DropdownMenuItem>
          <DropdownMenuItem onClick={toggleTheme}>
            {theme === "light" ? <MoonIcon /> : <SunIcon />}
            {theme === "light" ? "Dark theme" : "Light theme"}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onClick={() => s.signOut()}>
            <LogOutIcon />
            Sign out
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </header>
  )
}

function Crumbsep() {
  return (
    <li aria-hidden className="flex shrink-0 text-faint">
      <ChevronRightIcon className="size-3.5" />
    </li>
  )
}

// A site's place in the bar: its domain, then the section open in it.
function SiteCrumbs({ id, section }: { id: string; section?: string }) {
  const { site } = useSite(id)
  const sec = section ? SECTIONS.find((x) => x.key === section) : undefined
  return (
    <>
      <Crumbsep />
      <li className="min-w-0 truncate">
        {sec ? (
          <a href={href(sitePath(id))} className="text-muted-foreground no-underline hover:text-foreground hover:no-underline">
            {site?.primary_domain ?? "Site"}
          </a>
        ) : (
          <span aria-current="page" className="font-medium text-foreground">
            {site?.primary_domain ?? "Site"}
          </span>
        )}
      </li>
      {sec && (
        <Fragment>
          <Crumbsep />
          <li className="min-w-0 shrink-0 truncate">
            <span aria-current="page" className="font-medium text-foreground">
              {sec.label}
            </span>
          </li>
        </Fragment>
      )}
    </>
  )
}
