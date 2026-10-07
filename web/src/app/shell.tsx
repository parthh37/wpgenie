import { Suspense, useEffect } from "react"
import { LogOutIcon, MoonIcon, SearchIcon, SunIcon } from "lucide-react"
import {
  Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupContent, SidebarGroupLabel, SidebarHeader,
  SidebarInset, SidebarMenu, SidebarMenuBadge, SidebarMenuButton, SidebarMenuItem, SidebarProvider, SidebarTrigger, useSidebar,
} from "@/components/ui/sidebar"
import { Button } from "@/components/ui/button"
import { Kbd } from "@/components/ui/kbd"
import { Spinner } from "@/components/ui/spinner"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { ScreenBoundary } from "@/components/app/error-boundary"
import { IconTile } from "@/components/app/icon-tile"
import { Logo } from "@/components/app/logo"
import { Toaster, showError } from "@/components/app/toaster"
import { JobTray } from "@/app/job-tray"
import { CommandPalette, openPalette } from "@/app/command-palette"
import { GROUP_LABEL, PAGES, type NavGroup, type PageDef } from "@/app/pages"
import { useAlertsFiring, useNavFlags, useSupportSummary, useTenantBilling, useUpdateAvailable } from "@/app/nav-state"
import { pollJobs, stopJobs } from "@/lib/jobs"
import { href, navigate, useLocation, useRoute } from "@/lib/router"
import { useSession } from "@/lib/session"
import { toggleTheme, useTheme } from "@/lib/theme"
import { cn } from "@/lib/utils"

const GROUPS: NavGroup[] = ["", "operate", "business", "infrastructure"]
const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform)

export function AppShell() {
  const s = useSession()
  const flags = useNavFlags(s)
  const billing = useTenantBilling(s)
  const [page] = useRoute()
  const location = useLocation()
  const visible = PAGES.filter((p) => p.visible(s, flags))

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
  }, [page, s.mustSetup2FA, mustPay])

  // Switching pages clears errors about the last one and scrolls to the top.
  useEffect(() => {
    showError(null)
    window.scrollTo(0, 0)
  }, [page])

  const current: PageDef =
    (s.mustSetup2FA ? PAGES.find((p) => p.key === "account") : visible.find((p) => p.key === page)) ??
    // Support's visibility isn't known until its summary arrives: don't bounce.
    (page === "support" && flags.support === null ? PAGES.find((p) => p.key === "support")! : PAGES[0])
  const Screen = current.component

  return (
    <SidebarProvider>
      <AppSidebar pages={visible} current={current.key} />
      <SidebarInset className="min-w-0 bg-background">
        <MobileTop />
        <main id="main" tabIndex={-1} className="flex-1 outline-none">
          <ScreenBoundary key={location}>
            <Suspense fallback={<PageSpinner />}>
              <Screen key={current.key} />
            </Suspense>
          </ScreenBoundary>
        </main>
      </SidebarInset>
      <CommandPalette pages={visible} />
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
  const theme = useTheme()

  const badge = (key: string) => {
    if (key === "support" && support.data?.awaiting)
      return (
        <SidebarMenuBadge className="rounded-full bg-danger-fill px-1.5 text-white">
          {support.data.awaiting}
          <span className="sr-only"> {support.data.awaiting === 1 ? "ticket needs" : "tickets need"} your reply</span>
        </SidebarMenuBadge>
      )
    if (key === "monitoring" && alerts.data)
      return (
        <SidebarMenuBadge>
          <span className="size-2 rounded-full bg-danger-fill" />
          <span className="sr-only">(alerts firing)</span>
        </SidebarMenuBadge>
      )
    if (key === "system" && update.data)
      return (
        <SidebarMenuBadge>
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
        className="h-9 gap-2.5 text-[0.9375rem] text-sidebar-foreground no-underline hover:no-underline data-active:font-semibold"
        render={<a href={href(p.key)} onClick={() => setOpenMobile(false)} />}
      >
        <IconTile icon={p.icon} tint={p.tint} size="sm" />
        <span>{p.label}</span>
      </SidebarMenuButton>
      {badge(p.key)}
    </SidebarMenuItem>
  )

  return (
    <Sidebar className="material border-r-[0.5px] border-sidebar-border [&_[data-slot=sidebar-inner]]:bg-sidebar">
      <SidebarHeader className="gap-3 px-3 pt-4">
        <div className="flex items-center gap-2.5 px-1">
          <Logo className="size-8" />
          <div className="flex flex-col leading-tight">
            <span className="text-[0.9375rem] font-semibold">WPGenie</span>
            <small className="text-xs text-muted-foreground">Control panel</small>
          </div>
        </div>
        <button
          type="button"
          onClick={openPalette}
          className="flex h-9 w-full items-center gap-2 rounded-xl bg-sidebar-accent px-2.5 text-sm text-muted-foreground transition-colors hover:text-foreground"
        >
          <SearchIcon className="size-4" />
          <span className="flex-1 text-left">Search or jump to…</span>
          <Kbd>{isMac ? "⌘K" : "Ctrl K"}</Kbd>
        </button>
      </SidebarHeader>
      <SidebarContent className="px-1">
        {GROUPS.map((g) => {
          const items = pages.filter((p) => p.group === g && p.key !== "account")
          if (!items.length) return null
          return (
            <SidebarGroup key={g || "main"} className="py-1">
              {g && <SidebarGroupLabel className="text-xs font-semibold text-muted-foreground">{GROUP_LABEL(g, s)}</SidebarGroupLabel>}
              <SidebarGroupContent>
                <SidebarMenu>{items.map(item)}</SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          )
        })}
      </SidebarContent>
      <SidebarFooter className="px-3 pb-4">
        <div className="flex items-center gap-1">
          <a
            href={href("account")}
            onClick={() => setOpenMobile(false)}
            aria-current={current === "account" ? "page" : undefined}
            className={cn(
              "flex min-w-0 flex-1 items-center gap-2.5 rounded-xl p-1.5 text-foreground no-underline transition-colors hover:bg-sidebar-accent hover:no-underline",
              current === "account" && "bg-sidebar-accent"
            )}
          >
            <span aria-hidden className="brand-gradient flex size-8 shrink-0 items-center justify-center rounded-full text-sm font-semibold text-white">
              {s.me?.username.slice(0, 1).toUpperCase()}
            </span>
            <span className="flex min-w-0 flex-col leading-tight">
              <strong className="truncate text-sm font-semibold">{s.me?.username}</strong>
              <span className="text-xs text-muted-foreground">{s.me?.role}</span>
            </span>
          </a>
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
          <Tooltip>
            <TooltipTrigger render={<Button variant="ghost" size="icon-sm" onClick={() => s.signOut()} aria-label="Sign out" />}>
              <LogOutIcon />
            </TooltipTrigger>
            <TooltipContent>Sign out</TooltipContent>
          </Tooltip>
        </div>
      </SidebarFooter>
    </Sidebar>
  )
}

function MobileTop() {
  return (
    <header className="material sticky top-0 z-30 flex h-12 items-center justify-between border-b-[0.5px] border-border bg-background/75 px-2 md:hidden">
      <SidebarTrigger aria-label="Open navigation" />
      <div className="flex items-center gap-2 text-[0.9375rem] font-semibold">
        <Logo className="size-6" /> WPGenie
      </div>
      <Button variant="ghost" size="icon" aria-label="Search" onClick={openPalette}>
        <SearchIcon />
      </Button>
    </header>
  )
}
