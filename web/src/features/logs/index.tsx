import { ScrollTextIcon } from "lucide-react"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/app/blocks"
import { Page, PageHeader } from "@/components/app/page"
import { useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import { ArchiveCard } from "./archive"
import { SETTINGS_KEY, type LogSettings, type LogStatus } from "./data"
import { DestinationCard, RetentionCard } from "./settings"
import { LogTypes, LogsHero } from "./status"

// Logs: shipping every log to the customer's own S3-compatible storage.
// Staff see the status; administrators change the settings and read the
// archive. The secret key is write-only: the API never returns it.

export default function LogsPage() {
  const s = useSession()
  // The status is refreshed every minute while the page is open.
  const status = useApi<LogStatus>("/logs/status", { refetchInterval: 60_000 })
  const settings = useApi<LogSettings>(s.isAdmin ? SETTINGS_KEY[0] : null)
  const st = status.data
  const set = s.isAdmin ? (settings.data ?? null) : null
  const loading = status.isLoading || (s.isAdmin && settings.isLoading)
  const error = status.error || (s.isAdmin ? settings.error : null)

  return (
    <Page>
      <PageHeader icon={ScrollTextIcon} tint="brown" title="Logs" description="Every log kept in your own storage, not on this server's disk." />

      {loading && (
        <div className="flex flex-col gap-4">
          <Skeleton className="h-40 rounded-2xl" />
          <Skeleton className="h-72 rounded-2xl" />
        </div>
      )}
      {error && !loading && (
        <LoadError
          error={error}
          retry={() => {
            status.refetch()
            if (s.isAdmin) settings.refetch()
          }}
        />
      )}

      {st && !loading && !error && (
        <>
          <LogsHero
            st={st}
            set={set}
            refreshing={status.isFetching || settings.isFetching}
            onRefresh={() => {
              status.refetch()
              if (s.isAdmin) settings.refetch()
            }}
          />
          <LogTypes st={st} set={set} />
          {set && (
            <>
              {/* Fresh fields once the saved destination changes. */}
              <DestinationCard key={JSON.stringify(set.destination)} set={set} />
              <RetentionCard
                key={[set.archive_retention_days, set.local_access_logs, set.container_log_mb, set.compression, set.batch_max_seconds, set.batch_max_mb, set.spool_cap_mb].join("/")}
                set={set}
              />
              <ArchiveCard st={st} set={set} />
            </>
          )}
        </>
      )}
    </Page>
  )
}
