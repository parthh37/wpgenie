import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { CSPProvider } from "@base-ui/react/csp-provider"
import { QueryClientProvider } from "@tanstack/react-query"

import "./index.css"
import App from "./App"
import { TooltipProvider } from "@/components/ui/tooltip"
import { queryClient } from "@/lib/query"
import { SessionProvider } from "@/lib/session"

// The panel's CSP (default-src 'self') forbids <style> elements: Base UI's
// one (scrollbar hiding) is in index.css instead.
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <CSPProvider disableStyleElements>
      <QueryClientProvider client={queryClient}>
        <SessionProvider>
          <TooltipProvider delay={400}>
            <App />
          </TooltipProvider>
        </SessionProvider>
      </QueryClientProvider>
    </CSPProvider>
  </StrictMode>
)
