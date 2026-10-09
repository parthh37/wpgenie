import { ExternalLinkIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import type { Site } from "@/lib/types"

// A section not moved to the new panel yet.
export function SectionFallback({ site, section }: { site: Site; section: string }) {
  return (
    <Empty className="rounded-2xl bg-card py-16">
      <EmptyHeader>
        <EmptyTitle>Still in the classic panel</EmptyTitle>
        <EmptyDescription>This section hasn't moved to the new panel yet.</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button render={<a href={`/classic/#/sites/${encodeURIComponent(site.id)}/${section}`} />} nativeButton={false}>
          <ExternalLinkIcon data-icon="inline-start" />
          Open in the classic panel
        </Button>
      </EmptyContent>
    </Empty>
  )
}
