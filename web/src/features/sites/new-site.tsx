import { useSyncExternalStore } from "react"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"

// The new-site wizard: openNewSite() from anywhere (the Sites page, the
// command palette).

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
  return (
    <Dialog open={isOpen} onOpenChange={set}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New site</DialogTitle>
          <DialogDescription>The new-site wizard is still in the classic panel.</DialogDescription>
        </DialogHeader>
        <Button render={<a href="/#/sites" />} nativeButton={false}>
          Open the classic panel
        </Button>
      </DialogContent>
    </Dialog>
  )
}
