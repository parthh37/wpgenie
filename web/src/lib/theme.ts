import { useSyncExternalStore } from "react"

// The theme: chosen in the panel (kept in localStorage, shared with the
// legacy panel), else the system's. public/theme.js applies it before the
// first paint; this keeps React in step.

type Theme = "light" | "dark"
const subs = new Set<() => void>()
const read = (): Theme => (document.documentElement.classList.contains("dark") ? "dark" : "light")

new MutationObserver(() => subs.forEach((f) => f())).observe(document.documentElement, { attributes: true, attributeFilter: ["class"] })

export const useTheme = () => useSyncExternalStore((f) => (subs.add(f), () => void subs.delete(f)), read)

export function toggleTheme() {
  const next: Theme = read() === "light" ? "dark" : "light"
  document.documentElement.classList.toggle("dark", next === "dark")
  document.documentElement.dataset.theme = next
  try {
    localStorage.setItem("wpgenie_theme", next)
  } catch {
    /* this page only */
  }
}
