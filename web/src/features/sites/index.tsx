import { useRoute } from "@/lib/router"
import { SitesList } from "./list"
import { SiteWorkspace } from "./workspace"
import { NewSiteDialog } from "./new-site"

// #/sites: the list; #/sites/<id>[/<section>]: one site's workspace.
export default function SitesPage() {
  const [, id, section] = useRoute()
  return (
    <>
      {id ? <SiteWorkspace id={id} section={section} /> : <SitesList />}
      <NewSiteDialog />
    </>
  )
}
