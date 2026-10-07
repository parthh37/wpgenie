import { QueryClient, useQuery } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { Node, Site } from "@/lib/types"

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The panel is a live view: refetch on focus, but don't hammer.
      staleTime: 5_000,
      retry: (n, err) => n < 1 && !(err && "status" in err && (err as { status: number }).status < 500),
      refetchOnWindowFocus: true,
    },
  },
})

// useApi is useQuery for a GET: the key is the path. Screens with forms
// filled from the data pass refetchOnWindowFocus: false, so coming back to
// the tab doesn't replace what's being typed.
export function useApi<T>(
  path: string | null,
  opts: { refetchInterval?: number | false; enabled?: boolean; refetchOnWindowFocus?: boolean; staleTime?: number } = {}
) {
  return useQuery<T>({
    queryKey: [path],
    queryFn: () => api<T>("GET", path!),
    enabled: path != null && opts.enabled !== false,
    refetchInterval: opts.refetchInterval,
    refetchOnWindowFocus: opts.refetchOnWindowFocus,
    staleTime: opts.staleTime,
  })
}

// matchesPrefix: "/sites" matches "/sites", "/sites/x/…" and "/sites?…",
// not "/sitesfoo"; a prefix ending in "/" or "?" matches as it is.
export function matchesPrefix(key: string, prefix: string) {
  if (prefix.endsWith("/") || prefix.endsWith("?")) return key.startsWith(prefix)
  return key === prefix || key.startsWith(prefix + "/") || key.startsWith(prefix + "?")
}

// invalidate refetches every query under a path ("/sites" refreshes the
// list and every site's details; "/account" leaves "/accounts" alone).
export const invalidate = (prefix: string) =>
  queryClient.invalidateQueries({
    predicate: (q) => typeof q.queryKey[0] === "string" && matchesPrefix(q.queryKey[0] as string, prefix),
  })

export const useSites = () => useApi<Site[]>("/sites")

export function useSite(id: string | undefined) {
  const q = useSites()
  return { ...q, site: id ? q.data?.find((s) => s.id === id) : undefined }
}

// Nodes of a cluster ([] on a single server, or for users who can't see them).
export function useNodes() {
  return useQuery<Node[]>({
    queryKey: ["/nodes"],
    queryFn: () => api<Node[]>("GET", "/nodes").catch(() => []),
    staleTime: 60_000,
  })
}

export function useClustered() {
  const { data } = useNodes()
  return (data?.length ?? 0) > 1
}
