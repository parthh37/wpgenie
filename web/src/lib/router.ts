import { useSyncExternalStore } from "react"

// A hash router with the legacy panel's addresses: #/<page>,
// #/sites/<id>/<section>, #/support/<ticket>, #/billing/...?<query>, so
// links, bookmarks and the server's emails keep working. The query stays
// inside the hash (#/support/new?site=abc), as the legacy panel wrote it.

const subs = new Set<() => void>()
const emit = () => subs.forEach((f) => f())
addEventListener("hashchange", emit)
addEventListener("popstate", emit)

const current = () => location.hash.replace(/^#\/?/, "/")

export function navigate(to: string, opts: { replace?: boolean } = {}) {
  const hash = "#/" + to.replace(/^#?\/?/, "")
  if (location.hash === hash) return
  history[opts.replace ? "replaceState" : "pushState"](null, "", hash)
  emit()
}

// useLocation is the path after "#", with its query ("/support/new?site=x").
export const useLocation = () => useSyncExternalStore((f) => (subs.add(f), () => void subs.delete(f)), current)

const decode = (x: string) => {
  try {
    return decodeURIComponent(x)
  } catch {
    return x
  }
}

// useRoute is the path split into decoded parts: "/sites/abc/files" ->
// ["sites", "abc", "files"].
export function useRoute(): string[] {
  return useLocation().split("?")[0].split("/").filter(Boolean).map(decode)
}

// useHashQuery is the query of the hash ("#/support/new?site=abc").
export function useHashQuery() {
  return new URLSearchParams(useLocation().split("?")[1] || "")
}

export const sitePath = (id: string, section = "overview") =>
  `/sites/${encodeURIComponent(id)}` + (section === "overview" ? "" : "/" + section)

export const href = (path: string) => "#/" + path.replace(/^\//, "")
