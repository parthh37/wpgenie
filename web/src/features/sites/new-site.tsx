import { useSyncExternalStore } from "react"
import { useSession } from "@/lib/session"
import { CredentialsHost } from "./wizard/credentials"
import { NewSiteWizard } from "./wizard/new-site"

// The new-site wizard: openNewSite() from anywhere (the Sites page, the
// command palette). The shell renders NewSiteDialog, which also shows a new
// site's admin credentials once its creation job is done, on any page.

let open = false
const subs = new Set<() => void>()
const set = (v: boolean) => {
  open = v
  subs.forEach((f) => f())
}
export const openNewSite = () => set(true)
export const closeNewSite = () => set(false)
export const useNewSiteOpen = () => useSyncExternalStore((f) => (subs.add(f), () => void subs.delete(f)), () => open)

export function NewSiteDialog() {
  const isOpen = useNewSiteOpen()
  const s = useSession()
  return (
    <>
      {/* Admins and tenants create sites (the server checks the plan's limits). */}
      {s.canCreate && <NewSiteWizard open={isOpen} onClose={closeNewSite} />}
      <CredentialsHost />
    </>
  )
}
