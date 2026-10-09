import { SparklesIcon, TriangleAlertIcon } from "lucide-react"
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { ActionButton, Banner, BTable, CopyField, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import type { Token } from "./tokens"

// AI assistants: Claude, ChatGPT and other MCP clients managing sites for
// you. The assistant asks for access, you approve it once here (#/connect);
// it then acts as you, with your role, and only on what you can reach.

interface MCPInfo {
  url: string
  public: boolean
  tools: { name: string; title: string; description: string; read_only: boolean }[]
}

export function Assistants({ blocked }: { blocked: boolean }) {
  const info = useApi<MCPInfo>(blocked ? null : "/mcp")
  const tokens = useApi<Token[]>(blocked ? null : "/account/tokens")
  const connected = tokens.data?.filter((t) => t.client_id) ?? []

  return (
    <Section
      icon={SparklesIcon}
      tint="purple"
      title="AI assistants"
      description="Let Claude, ChatGPT or another assistant look after your sites: check traffic and updates, back up, update WordPress, clear the cache, create sites. It acts as you, with your role, and can't delete anything or see passwords."
    >
      {blocked ? (
        <p className="text-sm text-muted-foreground">Turn on two-factor authentication first: this panel requires it before an assistant can connect.</p>
      ) : (
        <div className="flex flex-col gap-5">
          {info.isLoading && <Skeleton className="h-24 rounded-xl" />}
          {info.error && <LoadError error={info.error} retry={() => info.refetch()} />}
          {info.data && (
            <>
              {!info.data.public && (
                <Banner tone="warn" icon={TriangleAlertIcon} title="Claude.ai and ChatGPT can't reach this panel yet.">
                  They connect over the internet, so the panel needs its own HTTPS address (install with PANEL_DOMAIN).
                  Claude Code on the computer you reach the panel from works as it is.
                </Banner>
              )}
              <div className="grid gap-1.5">
                <span className="text-sm font-medium">Connector address</span>
                <CopyField text={info.data.url} className="rounded-xl bg-muted px-3 font-mono text-sm" />
              </div>
              <Steps url={info.data.url} />
            </>
          )}

          <div className="grid gap-2">
            <h3 className="text-sm font-semibold">Connected assistants</h3>
            {tokens.isLoading && <Skeleton className="h-16 rounded-xl" />}
            {tokens.error && <LoadError error={tokens.error} retry={() => tokens.refetch()} />}
            {tokens.data && (
              <BTable
                caption="Connected assistants"
                empty={<p className="py-2 text-sm text-muted-foreground">None yet. Follow the steps above; you'll be asked to approve it here.</p>}
                cols={["Assistant", "Connected", "Last used", { label: <span className="sr-only">Actions</span>, className: "w-0" }]}
                rows={connected.map((t) => ({
                  key: t.id,
                  cells: [
                    <span className="font-medium">{t.name}</span>,
                    fmtTime(t.created_at),
                    t.last_used_at ? `${fmtTime(t.last_used_at)} from ${t.last_used_ip}` : "not yet",
                    <ActionButton
                      size="sm"
                      variant="destructive"
                      run={async () => {
                        if (!(await ask(`Disconnect ${t.name}? It loses access at once; connect it again to give it back.`))) return
                        await api("DELETE", `/account/tokens/${t.id}`)
                        await invalidate("/account/tokens")
                      }}
                    >
                      Disconnect
                    </ActionButton>,
                  ],
                }))}
              />
            )}
          </div>

          {info.data && info.data.tools.length > 0 && (
            <details className="text-sm">
              <summary className="cursor-pointer font-medium">What an assistant can do for you ({info.data.tools.length})</summary>
              <ul className="mt-2 grid list-none gap-1.5 p-0 sm:grid-cols-2">
                {info.data.tools.map((t) => (
                  <li key={t.name} className="rounded-xl bg-muted/60 px-3 py-2">
                    <span className="font-medium">{t.title}</span>
                    {!t.read_only && <span className="ml-1.5 text-xs text-warning">makes changes</span>}
                    <span className="block text-xs text-muted-foreground">{t.description}</span>
                  </li>
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
    </Section>
  )
}

function Steps({ url }: { url: string }) {
  return (
    <Tabs defaultValue="claude">
      <TabsList>
        <TabsTrigger value="claude">Claude</TabsTrigger>
        <TabsTrigger value="chatgpt">ChatGPT</TabsTrigger>
        <TabsTrigger value="code">Claude Code</TabsTrigger>
        <TabsTrigger value="other">Other</TabsTrigger>
      </TabsList>
      <TabsContent value="claude">
        <ol className="list-decimal space-y-1 pl-5 text-sm">
          <li>In Claude (claude.ai or the desktop app), open Settings → Connectors.</li>
          <li>Choose Add custom connector, name it WPGenie and paste the address above.</li>
          <li>Choose Connect. This panel opens: check it's you and approve.</li>
        </ol>
      </TabsContent>
      <TabsContent value="chatgpt">
        <ol className="list-decimal space-y-1 pl-5 text-sm">
          <li>In ChatGPT, open Settings → Apps &amp; Connectors → Advanced settings and turn on developer mode.</li>
          <li>Create a connector: name it WPGenie, paste the address above, and choose OAuth as authentication.</li>
          <li>ChatGPT opens this panel: check it's you and approve.</li>
        </ol>
      </TabsContent>
      <TabsContent value="code">
        <div className="grid gap-2 text-sm">
          <p>Run this, then type /mcp in Claude Code and choose WPGenie to sign in:</p>
          <CopyField text={`claude mcp add --transport http wpgenie ${url}`} className="rounded-xl bg-muted px-3 font-mono text-xs" />
        </div>
      </TabsContent>
      <TabsContent value="other">
        <p className="text-sm">
          Any assistant that supports remote MCP servers over HTTP can use the address above. If it can't sign in with OAuth, create an
          API token below and send it as the header <code className="font-mono text-xs">Authorization: Bearer &lt;token&gt;</code>.
        </p>
      </TabsContent>
    </Tabs>
  )
}
