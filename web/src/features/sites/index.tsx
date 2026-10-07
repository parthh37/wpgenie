import { useRoute } from "@/lib/router"
import { SitesList } from "./list"
import { SiteWorkspace } from "./workspace"

// #/sites: the list; #/sites/<id>[/<section>]: one site's workspace. (The
// new-site wizard is mounted by the shell, so its credentials show wherever
// you are when the site is ready.)
export default function SitesPage() {
  const [, id, section] = useRoute()
  return id ? <SiteWorkspace id={id} section={section} /> : <SitesList />
}
