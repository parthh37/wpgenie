import { useRoute } from "@/lib/router"
import { useSession } from "@/lib/session"
import { TicketList } from "./list"
import { ManagePage } from "./manage"
import { NewTicketPage } from "./new-ticket"
import { TicketPage } from "./ticket"

// Support tickets: #/support (the list), #/support/<id> (a ticket),
// #/support/new[?site=<id>] (a new one) and #/support/manage (staff:
// canned replies, departments, settings).
export default function SupportPage() {
  const [, sub = ""] = useRoute()
  const s = useSession()
  if (sub === "new") return <NewTicketPage />
  if (sub === "manage" && !s.isTenant) return <ManagePage />
  if (/^\d+$/.test(sub)) return <TicketPage key={sub} id={Number(sub)} />
  return <TicketList />
}
