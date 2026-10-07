import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

// EmailFrameDialog shows a page (a sent e-mail) in a sandboxed frame: no
// scripts, no same-origin access, links open in a new tab, no referrer
// (the legacy openFrame). The URL is the panel's own, so the CSP's
// frame-src ('self') allows it.
export function EmailFrameDialog({
  open,
  onOpenChange,
  title,
  url,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  url: string
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100svh-2rem)] gap-4 sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle className="pr-8 text-lg leading-snug font-semibold">{title}</DialogTitle>
        </DialogHeader>
        <iframe
          src={url}
          title={title}
          sandbox="allow-popups allow-popups-to-escape-sandbox"
          referrerPolicy="no-referrer"
          // E-mails are designed on white, whatever the panel's theme.
          className="h-[min(70svh,640px)] w-full rounded-xl bg-white ring-1 ring-border"
        />
        <DialogFooter>
          <Button type="button" variant="tinted" onClick={() => onOpenChange(false)}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
