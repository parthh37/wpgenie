import type { SectionProps } from "../sections"
import { CronCard } from "./tools/cron"
import { DebugCard } from "./tools/debug"
import { MaintenanceCard } from "./tools/maintenance"
import { SearchReplaceCard } from "./tools/search-replace"
import { SettingsCard } from "./tools/settings"
import { ThemesCard } from "./tools/themes"

// Tools: the jobs people otherwise install a plugin for. Maintenance mode,
// debug mode and its log, search & replace in the database, scheduled
// tasks, themes and a few WordPress settings (internal/api/tools.go).
export default function ToolsSection({ site }: SectionProps) {
  return (
    <div>
      <MaintenanceCard site={site} />
      <SettingsCard site={site} />
      <ThemesCard site={site} />
      <SearchReplaceCard site={site} />
      <CronCard site={site} />
      <DebugCard site={site} />
    </div>
  )
}
