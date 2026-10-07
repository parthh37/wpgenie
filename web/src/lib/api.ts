// The panel's API client. The session lives in an HttpOnly cookie the page
// can't read; the custom header is what the server checks on every change:
// other sites can't set it, so they can't act with your session.

export class ApiError extends Error {
  status: number
  data: Record<string, unknown>
  constructor(message: string, status: number, data: Record<string, unknown>) {
    super(message)
    this.status = status
    this.data = data
  }
  get code(): string | undefined {
    return typeof this.data.code === "string" ? this.data.code : undefined
  }
}

// Listeners hear about a session that ended (401) or a change that needs
// two-factor authentication first (code totp_required).
type AuthEvent = "signed-out" | "totp-required"
const listeners = new Set<(e: AuthEvent) => void>()
export function onAuthEvent(fn: (e: AuthEvent) => void) {
  listeners.add(fn)
  return () => void listeners.delete(fn)
}
const emit = (e: AuthEvent) => listeners.forEach((fn) => fn(e))

// signedOut: for requests made without api() (raw uploads) that got a 401.
export const signedOut = () => emit("signed-out")

export type Method = "GET" | "POST" | "PUT" | "PATCH" | "DELETE"

export async function api<T = unknown>(method: Method, path: string, body?: unknown): Promise<T> {
  const res = await fetch("/api/v1" + path, {
    method,
    credentials: "same-origin",
    headers: {
      "X-Requested-With": "wpgenie",
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (res.status === 401 && !path.startsWith("/auth/")) {
    emit("signed-out")
    throw new ApiError("Signed out: please sign in again", 401, {})
  }
  if (res.status === 204) return null as T
  const data = (await res.json().catch(() => ({}))) as Record<string, unknown>
  if (!res.ok) {
    const err = new ApiError(typeof data?.error === "string" ? data.error : res.statusText, res.status, data ?? {})
    if (err.code === "totp_required") emit("totp-required")
    throw err
  }
  return data as T
}

export const get = <T,>(path: string) => api<T>("GET", path)

// upload sends a multipart form (files), reporting progress; same headers,
// same errors as api().
export function upload<T = unknown>(
  path: string,
  form: FormData,
  onProgress?: (fraction: number) => void,
  method: Method = "POST"
): Promise<T> {
  return new Promise((resolve, reject) => {
    const x = new XMLHttpRequest()
    x.open(method, "/api/v1" + path)
    x.withCredentials = true
    x.setRequestHeader("X-Requested-With", "wpgenie")
    if (onProgress) x.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total)
    x.onload = () => {
      let data: Record<string, unknown> = {}
      try {
        data = JSON.parse(x.responseText || "{}")
      } catch {
        /* not JSON */
      }
      if (x.status === 401) emit("signed-out")
      if (x.status >= 200 && x.status < 300) resolve((x.status === 204 ? null : data) as T)
      else reject(new ApiError(typeof data.error === "string" ? data.error : x.statusText, x.status, data))
    }
    x.onerror = () => reject(new Error("The connection dropped while sending: nothing was saved, try again"))
    x.send(form)
  })
}

export const errorMessage = (e: unknown) => (e instanceof Error ? e.message : String(e))
