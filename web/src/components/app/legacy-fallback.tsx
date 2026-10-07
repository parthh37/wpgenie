import { ExternalLinkIcon, HammerIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Page } from "@/components/app/page"

// A page not moved to the new panel yet: opens the same address in the
// classic one.
export function LegacyFallback({ path, title }: { path: string; title: string }) {
  return (
    <Page>
      <Empty className="mt-16">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <HammerIcon />
          </EmptyMedia>
          <EmptyTitle>{title} is still in the classic panel</EmptyTitle>
          <EmptyDescription>This page hasn't moved to the new panel yet. It opens in the classic one, signed in as you.</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button render={<a href={"/#/" + path} />} nativeButton={false}>
            <ExternalLinkIcon data-icon="inline-start" />
            Open in the classic panel
          </Button>
        </EmptyContent>
      </Empty>
    </Page>
  )
}
