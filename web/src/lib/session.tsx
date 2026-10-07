import { createContext, useContext, useEffect, useMemo, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { api, onAuthEvent } from "@/lib/api"
import { queryClient } from "@/lib/query"
import { navigate } from "@/lib/router"
import type { AuthState, User } from "@/lib/types"

// The signed-in user and what their role may do. Roles: admin, operator,
// viewer (staff); customer and reseller (tenants: users of an account).

export interface Session {
  state: AuthState | undefined
  loading: boolean
  me: User | null
  isAdmin: boolean
  isTenant: boolean
  isStaff: boolean
  isReseller: boolean
  // Not a viewer: may change things.
  canChange: boolean
  // Admins and tenants create (and delete) sites.
  canCreate: boolean
  // Staff of at least this role (viewer < operator < admin); never tenants.
  atLeast: (role: "viewer" | "operator" | "admin") => boolean
  // Panel requires 2FA and this user hasn't set it up: only Account opens.
  mustSetup2FA: boolean
  signedIn: (user: User) => void
  signOut: () => Promise<void>
}

const RANK: Record<string, number> = { viewer: 0, operator: 1, admin: 2 }
const Ctx = createContext<Session | null>(null)
const KEY = ["auth-state"]

export function SessionProvider({ children }: { children: ReactNode }) {
  const q = useQuery<AuthState>({
    queryKey: KEY,
    queryFn: () => api<AuthState>("GET", "/auth/state"),
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })

  useEffect(
    () =>
      onAuthEvent((e) => {
        if (e === "signed-out") {
          queryClient.setQueryData<AuthState>(KEY, (s) => (s ? { ...s, user: null } : s))
          queryClient.removeQueries({ predicate: (x) => x.queryKey[0] !== KEY[0] })
        } else if (e === "totp-required") navigate("/account")
      }),
    []
  )

  const value = useMemo<Session>(() => {
    const me = q.data?.user ?? null
    const role = me?.role
    const isTenant = role === "customer" || role === "reseller"
    return {
      state: q.data,
      loading: q.isLoading,
      me,
      isAdmin: role === "admin",
      isTenant,
      isStaff: !!me && !isTenant,
      isReseller: role === "reseller",
      canChange: !!me && role !== "viewer",
      canCreate: role === "admin" || isTenant,
      atLeast: (r) => !!me && !isTenant && (RANK[role ?? ""] ?? -1) >= RANK[r],
      mustSetup2FA: !!me && !!q.data?.require_2fa && !me.totp_enabled,
      signedIn: (user) => {
        queryClient.setQueryData<AuthState>(KEY, (s) => ({ ...(s ?? { setup: false, require_2fa: false }), setup: false, user }))
        // The account (tenants) comes with the state.
        queryClient.invalidateQueries({ queryKey: KEY })
      },
      signOut: async () => {
        try {
          await api("POST", "/auth/logout")
        } catch {
          /* signed out either way */
        }
        queryClient.clear()
        queryClient.setQueryData<AuthState>(KEY, (s) => (s ? { ...s, user: null } : s))
        navigate("/sites", { replace: true })
      },
    }
  }, [q.data, q.isLoading])

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useSession() {
  const s = useContext(Ctx)
  if (!s) throw new Error("useSession outside SessionProvider")
  return s
}

// useMe is the signed-in user (inside the authed app, never null).
export const useMe = () => useSession().me as User
