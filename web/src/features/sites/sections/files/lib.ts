import { ApiError, signedOut } from "@/lib/api"
import type { Site } from "@/lib/types"

// The file manager's API (/sites/{id}/files…). Paths go in the query
// string, so the audit log names them.

export interface Entry {
  name: string
  type: "dir" | "file" | "link" | "other" | string
  size: number
  mode: string // "0644"
  modified: string
  // The site owns it: it may be renamed, deleted, re-moded (and, a file with
  // the owner's write bit, edited).
  owned: boolean
  target?: string // a link's target
  version?: string
}

export interface Listing {
  path: string
  // The site may add entries here.
  writable: boolean
  entries: Entry[]
  truncated?: boolean
}

export interface TextFile {
  path: string
  content: string
  version: string
  size: number
  mode: string
  writable: boolean
}

export interface CopyResult {
  files: number
  folders: number
  skipped: number
}

export const TEXT_FILE =
  /(^|\/)(\.htaccess|\.user\.ini|\.env[^/]*|robots\.txt|readme|license|changelog)$|\.(php\d?|phtml|inc|js|mjs|cjs|jsx|ts|tsx|json|map|css|scss|sass|less|html?|xml|svg|txt|md|markdown|ini|conf|cfg|log|csv|tsv|ya?ml|twig|po|pot|sql|sh|lock|vue|htm|tpl|mustache|hbs)$/i
export const IMAGE_FILE = /\.(png|jpe?g|gif|webp|avif|ico|bmp)$/i
export const ZIP_FILE = /\.zip$/i

type Params = Record<string, string | number | null | undefined | false>

export const qs = (params: Params) =>
  new URLSearchParams(
    Object.entries(params)
      .filter(([, v]) => v != null && v !== false)
      .map(([k, v]) => [k, String(v)])
  ).toString()

// filesPath is the API path (for api()), without /api/v1.
export const filesPath = (site: Site, sub: string, params: Params) => `/sites/${encodeURIComponent(site.id)}/files${sub}?` + qs(params)
// filesURL is the full URL (downloads, previews, raw uploads).
export const filesURL = (site: Site, sub: string, params: Params) => "/api/v1" + filesPath(site, sub, params)
// listPath is a folder listing's path: its query key too.
export const listPath = (site: Site, dir: string) => filesPath(site, "", { path: dir })
// The prefix every listing of a site starts with (to refresh them).
export const listPrefix = (site: Site) => `/sites/${encodeURIComponent(site.id)}/files?`

export const joinPath = (dir: string, name: string) => (dir === "/" ? "" : dir) + "/" + name
export const parentDir = (p: string) => p.replace(/\/[^/]*$/, "") || "/"
export const baseName = (p: string) => p.slice(p.lastIndexOf("/") + 1)

// resolvePath: what someone typed for a new name, relative to dir unless
// it starts with "/".
export const resolvePath = (dir: string, typed: string) => (typed.startsWith("/") ? typed : joinPath(dir, typed))

// The folder shown per site, kept while moving around the panel.
export const FILE_DIR = new Map<string, string>()

// putFile sends a file's bytes (PUT, raw body) with upload progress, which
// fetch() can't report (and upload() in @/lib/api sends a multipart form,
// which this endpoint doesn't take).
export function putFile<T = Entry>(site: Site, path: string, body: Blob, params: Params = {}, onProgress?: (fraction: number) => void): Promise<T> {
  return new Promise((resolve, reject) => {
    const x = new XMLHttpRequest()
    x.open("PUT", filesURL(site, "/content", { path, ...params }))
    x.withCredentials = true
    x.setRequestHeader("X-Requested-With", "wpgenie")
    x.setRequestHeader("Content-Type", "application/octet-stream")
    if (onProgress) x.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total)
    x.onload = () => {
      let data: Record<string, unknown> = {}
      try {
        data = JSON.parse(x.responseText || "{}")
      } catch {
        /* not JSON */
      }
      if (x.status >= 200 && x.status < 300) return resolve(data as T)
      // Signed out: the session asks the server again, and shows the sign-in.
      if (x.status === 401) signedOut()
      reject(new ApiError(typeof data.error === "string" ? data.error : x.statusText || `HTTP ${x.status}`, x.status, data))
    }
    x.onerror = () => reject(new Error(`Uploading ${baseName(path)} failed: the connection dropped`))
    x.send(body)
  })
}

// download starts a browser download (a folder comes as a zip archive).
export function download(site: Site, path: string) {
  const a = document.createElement("a")
  a.href = filesURL(site, "/download", { path })
  a.download = ""
  document.body.append(a)
  a.click()
  a.remove()
}

// preview opens a raster image in a new tab (the server only serves those
// inline).
export function preview(site: Site, path: string) {
  window.open(filesURL(site, "/download", { path, inline: 1 }), "_blank", "noopener")
}
