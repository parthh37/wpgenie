import { useEffect, useMemo, useState, useSyncExternalStore } from "react"
import {
  ChevronRightIcon, GlobeIcon, LogOutIcon, MoonIcon, PlusIcon, ShieldIcon, SunIcon, UserIcon, ZapIcon, type LucideIcon,
} from "lucide-react"
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList, CommandShortcut } from "@/components/ui/command"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { notify, showError } from "@/components/app/toaster"
import type { PageDef } from "@/app/pages"
import { SECTIONS } from "@/features/sites/sections"
import { openNewSite } from "@/features/sites/new-site"
import { api } from "@/lib/api"
import { invalidate, useSites } from "@/lib/query"
import { navigate, sitePath } from "@/lib/router"
import { useSession } from "@/lib/session"
import { toggleTheme, useTheme } from "@/lib/theme"

// The command palette (⌘K / Ctrl+K, or /): sites, their sections, quick
// actions and pages, found by typing.

let open = false
const subs = new Set<() => void>()
const setOpen = (v: boolean) => {
  open = v
  subs.forEach((f) => f())
}
export const openPalette = () => setOpen(true)

interface Item {
  id: string
  kind: "Site" | "Section" | "Action" | "Page"
  label: string
  icon: LucideIcon
  hint?: string
  // Only found by typing (a site's sections, per-site actions).
  deep?: boolean
  run: () => void | Promise<void>
}

// matchScore ranks an item for a query: every word of the query must be in
// the label, as text (better at the start of a word) or as letters in order
// ("nwc" finds northwind-coffee). -1: no match.
export function matchScore(label: string, query: string) {
  const hay = label.toLowerCase()
  let score = 0
  for (const word of query.toLowerCase().split(/\s+/).filter(Boolean)) {
    const i = hay.indexOf(word)
    if (i >= 0) {
      score += 100 - Math.min(i, 50) + (i === 0 || /[\s.›·-]/.test(hay[i - 1]) ? 50 : 0)
      continue
    }
    let j = -1
    for (const ch of word) {
      j = hay.indexOf(ch, j + 1)
      if (j < 0) return -1
    }
    score += 10
  }
  return score
}

export function CommandPalette({ pages }: { pages: PageDef[] }) {
  const isOpen = useSyncExternalStore((f) => (subs.add(f), () => void subs.delete(f)), () => open)
  const s = useSession()
  const { data: sites = [] } = useSites()
  const theme = useTheme()
  const [query, setQuery] = useState("")

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing = (e.target as HTMLElement | null)?.closest?.("input, textarea, select, [contenteditable]")
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault()
        setOpen(!open)
        return
      }
      if (typing || e.metaKey || e.ctrlKey || e.altKey || document.querySelector("[role=dialog],[role=alertdialog]")) return
      if (e.key === "/") {
        e.preventDefault()
        setOpen(true)
      }
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [])

  useEffect(() => {
    if (isOpen) setQuery("")
  }, [isOpen])

  const items = useMemo<Item[]>(() => {
    const out: Item[] = []
    for (const site of sites) {
      out.push({ id: "site:" + site.id, kind: "Site", label: site.primary_domain, icon: GlobeIcon, hint: site.status === "active" ? "" : site.status, run: () => navigate(sitePath(site.id)) })
      for (const sec of SECTIONS.slice(1)) {
        out.push({ id: `sec:${site.id}:${sec.key}`, kind: "Section", label: `${site.primary_domain} › ${sec.label}`, icon: ChevronRightIcon, deep: true, run: () => navigate(sitePath(site.id, sec.key)) })
      }
      if (!s.canChange || site.status !== "active") continue
      const attack = site.shield_mode === "under_attack"
      out.push({
        id: "attack:" + site.id, kind: "Action", icon: ShieldIcon, deep: true,
        label: `${attack ? "Turn off" : "Turn on"} Under attack mode · ${site.primary_domain}`,
        run: async () => {
          await api("PUT", `/sites/${site.id}/shield`, { mode: attack ? "standard" : "under_attack", block_ai_bots: site.block_ai_bots })
          notify(attack ? `${site.primary_domain}: back to standard protection` : `${site.primary_domain}: every visitor is challenged now`)
          await invalidate("/sites")
        },
      })
      if (site.page_cache) {
        out.push({
          id: "purge:" + site.id, kind: "Action", icon: ZapIcon, deep: true, label: `Purge cache · ${site.primary_domain}`,
          run: async () => {
            await api("POST", `/sites/${site.id}/cache/purge`)
            notify(`Cache purged on ${site.primary_domain}`)
          },
        })
      }
    }
    for (const p of pages) {
      if (p.key === "account") continue
      out.push({ id: "page:" + p.key, kind: "Page", label: p.label, icon: p.icon, run: () => navigate("/" + p.key) })
    }
    out.push({ id: "page:account", kind: "Page", label: "Your account", icon: UserIcon, run: () => navigate("/account") })
    if (s.canCreate) out.push({ id: "new-site", kind: "Action", label: "Create a new site", icon: PlusIcon, run: () => { navigate("/sites"); openNewSite() } })
    out.push({ id: "theme", kind: "Action", label: `Switch to ${theme === "light" ? "dark" : "light"} theme`, icon: theme === "light" ? MoonIcon : SunIcon, run: toggleTheme })
    out.push({ id: "signout", kind: "Action", label: "Sign out", icon: LogOutIcon, run: () => s.signOut() })
    return out
  }, [sites, pages, s, theme])

  const q = query.trim()
  const shown = q
    ? items
        .map((it) => ({ it, score: matchScore(it.label, q) - (it.deep ? 5 : 0) }))
        .filter((x) => x.score >= 0)
        .sort((a, b) => b.score - a.score)
        .slice(0, 40)
        .map((x) => x.it)
    : items.filter((it) => !it.deep)

  const run = async (it: Item) => {
    setOpen(false)
    try {
      await it.run()
    } catch (e) {
      showError(e)
    }
  }

  return (
    <Dialog open={isOpen} onOpenChange={setOpen}>
      <DialogHeader className="sr-only">
        <DialogTitle>Search or jump to</DialogTitle>
        <DialogDescription>Sites, their sections, actions and pages</DialogDescription>
      </DialogHeader>
      <DialogContent showCloseButton={false} className="top-[18%] translate-y-0 overflow-hidden rounded-3xl! p-0 sm:max-w-xl">
        {/* Ranked here (matchScore), not by cmdk. */}
        <Command shouldFilter={false} className="rounded-3xl">
          <CommandInput placeholder="Search sites, sections and actions…" value={query} onValueChange={setQuery} />
          <CommandList className="max-h-[min(420px,60vh)]">
            <CommandEmpty>Nothing matches “{q}”.</CommandEmpty>
            <CommandGroup>
              {shown.map((it) => (
                <CommandItem key={it.id} value={it.id} onSelect={() => run(it)} className="gap-2.5">
                  <it.icon />
                  <span className="min-w-0 flex-1 truncate">{it.label}</span>
                  {it.hint && <span className="rounded-full bg-muted px-2 text-xs text-muted-foreground">{it.hint}</span>}
                  <CommandShortcut>{it.kind}</CommandShortcut>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </DialogContent>
    </Dialog>
  )
}
