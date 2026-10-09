import { lazy, Suspense, useState } from "react"
import { AppShell, PageSpinner } from "@/app/shell"
import { ConfirmHost } from "@/components/app/confirm"
import { SecretHost } from "@/components/app/secret"
import { Toaster } from "@/components/app/toaster"
import { Button } from "@/components/ui/button"
import { SignIn, Setup, SSO } from "@/features/auth/sign-in"
import { useRoute } from "@/lib/router"
import { useSession } from "@/lib/session"

// An AI assistant asking for access (#/connect, from /oauth/authorize): a
// screen of its own, after signing in.
const Connect = lazy(() => import("@/features/connect"))

// A one-time sign-in link: #sso=<token> (the fragment never reaches a
// server or a Referer). Taken out of the address at once.
function takeSSOToken() {
  const m = location.hash.match(/^#sso=([A-Za-z0-9_-]{20,100})$/)
  if (!m) return null
  history.replaceState(null, "", location.pathname)
  return m[1]
}

export default function App() {
  const s = useSession()
  const [sso, setSSO] = useState(takeSSOToken)
  const route = useRoute()

  let screen
  if (sso) screen = <SSO token={sso} onDone={() => setSSO(null)} />
  else if (s.loading) screen = <PageSpinner />
  else if (!s.state) screen = <LoadFailed />
  else if (s.state.setup) screen = <Setup />
  else if (!s.me) screen = <SignIn />
  else if (route[0] === "connect")
    screen = (
      <Suspense fallback={<PageSpinner />}>
        <Connect />
      </Suspense>
    )
  else screen = <AppShell />

  return (
    <>
      {screen}
      {(!s.me || route[0] === "connect") && <Toaster />}
      <ConfirmHost />
      <SecretHost />
    </>
  )
}

function LoadFailed() {
  return (
    <div className="flex min-h-svh flex-col items-center justify-center gap-4 p-6 text-center">
      <p className="text-muted-foreground">The panel couldn't reach the server.</p>
      <Button onClick={() => location.reload()}>Try again</Button>
    </div>
  )
}
