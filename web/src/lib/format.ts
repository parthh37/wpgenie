// Formatting shared by every screen (the legacy panel's fmt* helpers).

export function fmtBytes(n: number | null | undefined): string {
  if (n == null) return "–"
  const u = ["B", "KB", "MB", "GB", "TB"]
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return (i ? n.toFixed(1) : String(n)) + " " + u[i]
}

const nf = new Intl.NumberFormat()
export const fmtNum = (n: number | null | undefined) => (n == null ? "–" : nf.format(n))

export const fmtTime = (t: string | number | Date | null | undefined) =>
  t ? new Date(t).toLocaleString([], { dateStyle: "short", timeStyle: "short" }) : "–"

export const fmtDate = (t: string | number | Date | null | undefined) =>
  t ? new Date(t).toLocaleDateString([], { dateStyle: "medium" }) : "–"

export const fmtMem = (mb: number) => (mb >= 1024 ? `${mb / 1024} GB` : `${mb} MB`)

export const fmtPct = (v: number | null | undefined) =>
  v == null ? "–" : v >= 99.95 ? "100%" : v >= 99 ? `${v.toFixed(1)}%` : `${Math.round(v)}%`

// "3 minutes ago", "in 2 days".
const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" })
export function fmtAgo(t: string | number | Date | null | undefined): string {
  if (!t) return "–"
  const s = (new Date(t).getTime() - Date.now()) / 1000
  const abs = Math.abs(s)
  if (abs < 45) return "just now"
  if (abs < 3600) return rtf.format(Math.round(s / 60), "minute")
  if (abs < 86400) return rtf.format(Math.round(s / 3600), "hour")
  if (abs < 86400 * 30) return rtf.format(Math.round(s / 86400), "day")
  if (abs < 86400 * 365) return rtf.format(Math.round(s / (86400 * 30)), "month")
  return rtf.format(Math.round(s / (86400 * 365)), "year")
}

export const plural = (n: number, one: string, many = one + "s") => `${fmtNum(n)} ${n === 1 ? one : many}`

export const splitList = (v: string) =>
  v
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean)

// "running_update" -> "running update"
export const humanize = (s: string) => s.replace(/_/g, " ")

export const sum = (xs: number[]) => xs.reduce((a, b) => a + b, 0)
