import { useApi } from "@/lib/query"

// The host's Divi (Elegant Themes) license, as GET /settings/divi shows it.
// Administrators also get the username and the key's last characters;
// the key itself is never sent to the browser.
export interface DiviView {
  configured: boolean
  new_sites: boolean
  username?: string
  key_set: boolean
  key_hint?: string
}

export const DIVI = "/settings/divi"
// Divi's directory name in wp-content/themes (its slug).
export const DIVI_SLUG = "Divi"

export const useDivi = () => useApi<DiviView>(DIVI, { refetchOnWindowFocus: false, staleTime: 60_000 })
