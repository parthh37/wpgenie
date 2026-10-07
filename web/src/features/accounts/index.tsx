import { useRoute } from "@/lib/router"
import { AccountDetail } from "./detail"
import { AccountsList } from "./list"

// Accounts: the list at #/accounts, an account at #/accounts/<id>.
export default function AccountsPage() {
  const [, id] = useRoute()
  return id ? <AccountDetail key={id} id={id} /> : <AccountsList />
}
