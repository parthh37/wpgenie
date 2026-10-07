import { Component, type ReactNode } from "react"
import { RefreshCwIcon, TriangleAlertIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { IconTile } from "@/components/app/icon-tile"

// ScreenBoundary keeps one broken screen from blanking the panel: the rest
// (navigation, search, the other pages) keeps working, and the screen can
// be tried again. Keyed by the address, so going elsewhere resets it.
export class ScreenBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null }

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  componentDidCatch(error: Error) {
    console.error("Screen failed:", error)
  }

  render() {
    if (!this.state.error) return this.props.children
    return (
      <Empty role="alert" className="mx-auto mt-16 max-w-lg">
        <EmptyHeader>
          <EmptyMedia>
            <IconTile icon={TriangleAlertIcon} tint="red" size="xl" />
          </EmptyMedia>
          <EmptyTitle className="text-lg">This page hit a problem</EmptyTitle>
          <EmptyDescription>
            Nothing was changed. Try again; if it keeps happening, reload the panel. ({this.state.error.message})
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent className="flex-row justify-center">
          <Button variant="tinted" onClick={() => this.setState({ error: null })}>
            <RefreshCwIcon data-icon="inline-start" />
            Try again
          </Button>
          <Button variant="ghost" onClick={() => location.reload()}>
            Reload
          </Button>
        </EmptyContent>
      </Empty>
    )
  }
}
