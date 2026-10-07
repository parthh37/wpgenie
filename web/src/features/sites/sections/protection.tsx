import { useState } from "react"
import { BotIcon, ChevronDownIcon, ScanFaceIcon, ShieldAlertIcon, ShieldCheckIcon, SlidersHorizontalIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Banner, ChoiceCard } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import { useSession } from "@/lib/session"
import { useQuery } from "@tanstack/react-query"
import { useAttack } from "../data"
import type { SectionProps } from "../sections"
import { AdvancedProtection, advancedKey } from "./protect/advanced"
import { RecentBlocks } from "./protect/recent-blocks"
import { ScanSection } from "./protect/scan"
import { CHECK_NAMES, LEVEL_NAMES, levelOf, Note, putShield, type ShieldChange } from "./protect/shared"
import type { ProtectionLevel } from "./protect/types"

// Protection: a level and a visitor check in plain words (simple.js), the
// detailed rules under Advanced (app.js renderSecurity), what was blocked
// lately, and the vulnerability scan.

const recommended = (
  <Badge variant="secondary" className="ml-1.5 bg-primary/12 align-middle text-link">
    Recommended
  </Badge>
)

export default function ProtectionSection({ site }: SectionProps) {
  const s = useSession()
  const active = site.status === "active"
  const [busy, setBusy] = useState(false)
  // The server's levels never change while it runs: fetched once.
  const levels = useQuery<ProtectionLevel[]>({
    queryKey: ["/security/levels"],
    queryFn: () => api<ProtectionLevel[]>("GET", "/security/levels"),
    staleTime: Infinity,
  })
  const level = levels.data ? levelOf(site, levels.data) : undefined
  const off = !active || busy || !s.canChange

  const put = async (change: ShieldChange, msg: string) => {
    setBusy(true)
    try {
      await putShield(site, change)
      notify(msg)
    } catch (e) {
      showError(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="@container flex flex-col">
      <AttackBanner site={site} />
      {site.shield_mode === "off" && (
        <Banner tone="warn" icon={ShieldAlertIcon} title="Protection is off">
          Nothing is checked or blocked on this site. Pick a visitor check below to turn it on.
        </Banner>
      )}

      <Section icon={ShieldCheckIcon} tint="green" title="Protection level">
        {levels.isPending ? (
          <div className="grid gap-2.5 @2xl:grid-cols-3">
            <Skeleton className="h-24 rounded-2xl" />
            <Skeleton className="h-24 rounded-2xl" />
            <Skeleton className="h-24 rounded-2xl" />
          </div>
        ) : levels.isError ? (
          <Note tone="bad">{`The protection levels didn't load: ${levels.error.message}`}</Note>
        ) : (
          <>
            <div role="radiogroup" aria-label="Protection level" className="grid gap-2.5 @2xl:grid-cols-3">
              <ChoiceCard name={`level-${site.id}`} value="basic" title="Basic" checked={level === "basic"} disabled={off}
                onChange={(v) => put({ level: v }, `${LEVEL_NAMES[v]} protection on ${site.primary_domain}`)}>
                Blocks known attacks and never gets in a visitor's way. For shops and apps with unusual traffic.
              </ChoiceCard>
              <ChoiceCard name={`level-${site.id}`} value="recommended" title={<>Recommended{recommended}</>} checked={level === "recommended"} disabled={off}
                onChange={(v) => put({ level: v }, `${LEVEL_NAMES[v]} protection on ${site.primary_domain}`)}>
                Firewall, login protection, and a check for suspicious visitors.
              </ChoiceCard>
              <ChoiceCard name={`level-${site.id}`} value="strict" title="Strict" checked={level === "strict"} disabled={off}
                onChange={(v) => put({ level: v }, `${LEVEL_NAMES[v]} protection on ${site.primary_domain}`)}>
                For sites that are attacked often: tighter limits, and known bad addresses are blocked.
              </ChoiceCard>
            </div>
            {level === "custom" && <Note className="mt-3">This site has custom settings (see Advanced). Choosing a level replaces them.</Note>}
          </>
        )}
      </Section>

      <Section
        icon={ScanFaceIcon}
        tint="indigo"
        title="Visitor check"
        description="A quick check that runs in the visitor's browser instead of a CAPTCHA: no puzzles or pictures to click. People get through in about a second; bots don't."
      >
        <div role="radiogroup" aria-label="Visitor check" className="grid gap-2.5 @2xl:grid-cols-3">
          <ChoiceCard name={`check-${site.id}`} value="auto" title={<>Automatic{recommended}</>} checked={site.shield_mode === "auto"} disabled={off}
            onChange={(v) => put({ mode: v }, `Visitor check on ${site.primary_domain}: ${CHECK_NAMES[v]}`)}>
            Suspicious visitors only, and everyone while an attack is detected.
          </ChoiceCard>
          <ChoiceCard name={`check-${site.id}`} value="under_attack" title="Everyone" checked={site.shield_mode === "under_attack"} disabled={off}
            onChange={(v) => put({ mode: v }, `Visitor check on ${site.primary_domain}: ${CHECK_NAMES[v]}`)}>
            Every visitor, until you switch it back. For an attack in progress.
          </ChoiceCard>
          <ChoiceCard name={`check-${site.id}`} value="standard" title="Suspicious visitors only" checked={site.shield_mode === "standard"} disabled={off}
            onChange={(v) => put({ mode: v }, `Visitor check on ${site.primary_domain}: ${CHECK_NAMES[v]}`)}>
            Never switches on for everyone by itself.
          </ChoiceCard>
        </div>
      </Section>

      <Section icon={BotIcon} tint="purple" title="Bots and apps">
        <div className="flex flex-col gap-4">
          <Label className="items-start font-normal">
            <Switch
              className="mt-0.5"
              checked={site.block_ai_bots}
              disabled={off}
              onCheckedChange={(v) => put({ block_ai_bots: v }, v ? "AI crawlers blocked" : "AI crawlers allowed")}
            />
            <span className="flex flex-col gap-0.5">
              Block AI crawlers
              <span className="text-sm text-muted-foreground">Bots that copy your content to train AI.</span>
            </span>
          </Label>
          <Label className="items-start font-normal">
            <Switch
              className="mt-0.5"
              checked={site.xmlrpc}
              disabled={off}
              onCheckedChange={(v) => put({ xmlrpc: v }, v ? "Jetpack and the WordPress app can connect" : "XML-RPC blocked")}
            />
            <span className="flex flex-col gap-0.5">
              Allow Jetpack and the WordPress app
              <span className="text-sm text-muted-foreground">XML-RPC.</span>
            </span>
          </Label>
        </div>
      </Section>

      <Collapsible className="mb-4 rounded-2xl bg-card px-5 py-4 card-shadow">
        <CollapsibleTrigger
          render={<Button variant="ghost" className="group -mx-2 h-auto w-[calc(100%+1rem)] justify-start gap-2.5 px-2 py-1.5 text-[0.9375rem] font-semibold" />}
        >
          <SlidersHorizontalIcon className="size-4 text-muted-foreground" />
          Advanced: firewall rules, lists and limits
          <ChevronDownIcon className="ml-auto size-4 text-muted-foreground transition-transform group-data-[panel-open]:rotate-180" />
        </CollapsibleTrigger>
        <CollapsibleContent>
          {/* Remounted with the saved settings whenever they change. */}
          <AdvancedProtection key={advancedKey(site)} site={site} />
        </CollapsibleContent>
      </Collapsible>

      <RecentBlocks site={site} />
      <ScanSection site={site} />
    </div>
  )
}

// AttackBanner: an automatically detected attack, and "It's over" to end
// the every-visitor check now (simple.js loadAttack).
function AttackBanner({ site }: SectionProps) {
  const s = useSession()
  const { data: attack, refetch } = useAttack(site)
  const [busy, setBusy] = useState(false)
  if (!attack?.active || !attack.attack) return null
  const over = async () => {
    setBusy(true)
    try {
      await api("DELETE", `/sites/${site.id}/attack`)
      notify("Visitors are no longer all checked")
    } catch (e) {
      showError(e)
    }
    await refetch()
    setBusy(false)
  }
  return (
    <Banner
      tone="bad"
      icon={ShieldAlertIcon}
      title="Under attack"
      actions={
        s.canChange && (
          <Button variant="tinted" size="sm" disabled={busy} onClick={over}>
            It's over
          </Button>
        )
      }
    >
      Since {fmtTime(attack.attack.since)}: {attack.attack.reason}. Every visitor is checked automatically, and your site stays online. This
      ends by itself once the attack stops.
    </Banner>
  )
}
