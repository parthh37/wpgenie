import { useEffect, useRef, useState } from "react"
import { ZapIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ChoiceCard } from "@/components/app/blocks"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { BuyBurstButton } from "@/features/billing/shared/burst"
import { api } from "@/lib/api"
import { fmtNum, fmtTime, plural } from "@/lib/format"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { Meter, Note, Warning } from "./shared"
import { ScalingAdvanced } from "./scaling"

// Burst: the site gets extra copies of itself while traffic needs them,
// paid for in burst minutes. Off, Automatic or On now: the plain choice
// for owners; the sizes and targets underneath are under Advanced.

export const BURST_NAMES: Record<string, string> = { off: "off", auto: "automatic", on: "on" }

// GET /sites/{id}/burst (site.BurstStatus and the account's balance).
interface BurstView {
  mode: string
  until?: string
  paused: boolean
  base: number
  replicas: number
  max: number
  bursting: boolean
  minutes: number
  account: {
    allowed: boolean
    included: number
    unlimited: boolean
    used: number
    credit: number
    remaining: number
  } | null
}

const copies = (n: number) => plural(n, "copy", "copies")

export function BurstCard({ site }: { site: Site }) {
  const s = useSession()
  const active = site.status === "active"
  const mode = site.burst_mode || "off"
  // The status (and the account's minutes) only for a live site; when it
  // doesn't load, the rest of the section still works.
  const { data: b, isLoading } = useApi<BurstView>(active ? `/sites/${site.id}/burst` : null)
  const [picked, setPicked] = useState(mode)
  const [hours, setHours] = useState("3")
  const [busy, setBusy] = useState(false)
  const hoursRef = useRef<HTMLSelectElement>(null)

  // The site's mode changed (saved here or elsewhere): show it.
  const [seen, setSeen] = useState(mode)
  if (seen !== mode) {
    setSeen(mode)
    setPicked(mode)
  }

  // "On now" is started with the button, once they say for how long.
  const [focusHours, setFocusHours] = useState(0)
  useEffect(() => {
    if (focusHours) hoursRef.current?.focus()
  }, [focusHours])

  const notAllowed = !!b?.account && !b.account.allowed && s.isTenant
  const locked = !active || busy || !s.canChange || notAllowed

  const save = async (body: { mode: string; hours?: number }, msg: string) => {
    setBusy(true)
    try {
      await api("PUT", `/sites/${site.id}/burst`, body)
      notify(msg)
    } catch (e) {
      showError(e)
      setPicked(mode)
    }
    await invalidate("/sites")
    setBusy(false)
  }

  const pick = (v: string) => {
    setPicked(v)
    if (v === "on") {
      setFocusHours((n) => n + 1)
      return
    }
    save({ mode: v }, v === "off" ? `Burst off for ${site.primary_domain}` : `Burst is automatic for ${site.primary_domain}`)
  }

  const start = () => {
    const n = Number(hours)
    save({ mode: "on", hours: n }, n ? `Burst on for ${plural(n, "hour")}, then automatic` : "Burst on until you turn it off")
  }

  return (
    <Section
      icon={ZapIcon}
      tint="orange"
      title="Burst"
      description="When lots of visitors arrive at once, your site gets extra copies of itself so it stays fast, then goes back to normal. How many it gets depends on the traffic and on how busy the server is. You use burst minutes only while extra copies are running."
    >
      <div className="flex flex-col gap-4">
        <div role="radiogroup" aria-label="Burst" className="grid gap-2 sm:grid-cols-3">
          <ChoiceCard name={`burst-${site.id}`} value="off" title="Off" checked={picked === "off"} disabled={locked} onChange={pick}>
            Always the normal size. Uses no minutes.
          </ChoiceCard>
          <ChoiceCard
            name={`burst-${site.id}`}
            value="auto"
            title={
              <span className="inline-flex flex-wrap items-center gap-1.5">
                Automatic <Badge variant="secondary">Recommended</Badge>
              </span>
            }
            checked={picked === "auto"}
            disabled={locked}
            onChange={pick}
          >
            Extra copies only while traffic needs them.
          </ChoiceCard>
          <ChoiceCard name={`burst-${site.id}`} value="on" title="On now" checked={picked === "on"} disabled={locked} onChange={pick}>
            Extra copies right away: for a launch, a sale or a campaign.
          </ChoiceCard>
        </div>

        {picked === "on" && (
          <div className="flex flex-wrap items-end gap-3">
            <Field className="w-auto">
              <FieldLabel htmlFor={`burst-hours-${site.id}`}>Keep it on</FieldLabel>
              <NativeSelect
                id={`burst-hours-${site.id}`}
                ref={hoursRef}
                value={hours}
                disabled={locked}
                onChange={(e) => setHours(e.target.value)}
              >
                <NativeSelectOption value="1">for 1 hour</NativeSelectOption>
                <NativeSelectOption value="3">for 3 hours</NativeSelectOption>
                <NativeSelectOption value="24">for 24 hours</NativeSelectOption>
                <NativeSelectOption value="0">until I turn it off</NativeSelectOption>
              </NativeSelect>
            </Field>
            {s.canChange && (
              <Button type="button" disabled={locked} onClick={start}>
                {mode === "on" ? "Change" : "Start burst"}
              </Button>
            )}
          </div>
        )}

        {active && isLoading && <Skeleton className="h-16 w-full max-w-md" />}
        {b && <BurstStatus b={b} tenant={s.isTenant} />}

        <ScalingAdvanced site={site} />
      </div>
    </Section>
  )
}

function BurstStatus({ b, tenant }: { b: BurstView; tenant: boolean }) {
  let now
  if (b.paused) {
    now = (
      <Warning>
        Burst is paused: there are no burst minutes left. Your site stays at its normal size and carries on by itself when minutes are added or the
        month starts again.
      </Warning>
    )
  } else if (b.bursting) {
    now = (
      <p className="text-sm text-success">
        Bursting now: {copies(b.replicas)} running (normally {b.base}).
      </p>
    )
  } else if (b.mode === "off") {
    now = <Note>Normal size: {copies(b.base)}.</Note>
  } else if (b.mode === "on") {
    now = <Note>Waiting for room on the server for extra copies: it is busy right now. No minutes are used meanwhile.</Note>
  } else {
    now = (
      <Note>
        Normal size now ({copies(b.base)}); up to {copies(b.max)} when traffic needs them.
      </Note>
    )
  }
  const a = b.account
  return (
    <div role="status" className="flex flex-col gap-1.5 rounded-xl bg-muted/60 p-3.5">
      {now}
      {/* Out of minutes: tenants billed here can buy more. */}
      {b.paused && tenant && (
        <div>
          <BuyBurstButton />
        </div>
      )}
      {b.mode === "on" && b.until && <Note>On until {fmtTime(b.until)}, then automatic.</Note>}
      <Note>This site used {plural(b.minutes, "burst minute")} this month.</Note>
      {a &&
        (!a.allowed && tenant ? (
          <Warning>Burst isn't included in your plan. Ask your provider to add it.</Warning>
        ) : a.unlimited ? (
          <Note>Your plan includes unlimited burst minutes.</Note>
        ) : (
          <BurstMeter a={a} />
        ))}
    </div>
  )
}

// BurstMeter shows what an account has left this month.
function BurstMeter({ a }: { a: NonNullable<BurstView["account"]> }) {
  const monthly = Math.max(0, a.included - a.used)
  let text = `${fmtNum(monthly)} of ${fmtNum(a.included)} monthly minutes left`
  if (a.credit) text += ` · ${plural(a.credit, "extra minute")}`
  return (
    <div className="mt-1 grid grid-cols-[auto_minmax(4rem,14rem)_auto] items-center gap-x-3 gap-y-1 text-sm max-sm:grid-cols-[auto_1fr]">
      <span>Burst minutes</span>
      <Meter label="Monthly burst minutes left" value={monthly} max={a.included} low={a.included * 0.2} high={a.included * 0.5} />
      <span className="text-muted-foreground max-sm:col-span-2">{text}</span>
    </div>
  )
}
