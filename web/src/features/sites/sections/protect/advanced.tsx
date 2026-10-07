import { useId, useState, type FormEvent, type ReactNode } from "react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Switch } from "@/components/ui/switch"
import { notify, showError } from "@/components/app/toaster"
import { splitList } from "@/lib/format"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { CountryPicker } from "./country-picker"
import { Note, putShield } from "./shared"

// The detailed protection settings (the legacy renderSecurity): firewall,
// lists, country rule and limits, saved together. Remount it (key) when
// the settings change (advancedKey) to show the saved values.

interface Form {
  waf: boolean
  body_waf: string
  admin_allow: string
  trusted: string
  deny: string
  reputation: string
  country_mode: string
  countries: string[]
  country_action: string
  rate: string
  burst: string
  login: string
  bits: string
}

const formOf = (s: Site): Form => ({
  waf: s.waf,
  body_waf: s.body_waf || "off",
  admin_allow: (s.admin_allow || []).join(", "),
  trusted: (s.trusted_ips || []).join(", "),
  deny: (s.deny_ips || []).join(", "),
  reputation: s.reputation || "challenge",
  country_mode: s.country_mode || "off",
  countries: s.countries || [],
  country_action: s.country_action || "block",
  rate: String(s.rate_rps || 0),
  burst: String(s.rate_burst || 0),
  login: String(s.login_per_min || 0),
  bits: String(s.challenge_bits || 0),
})

// advancedKey changes whenever a saved advanced setting does: the form's key.
export const advancedKey = (s: Site) =>
  JSON.stringify([s.waf, s.body_waf, s.admin_allow, s.trusted_ips, s.deny_ips, s.reputation, s.country_mode, s.countries,
    s.country_action, s.rate_rps, s.rate_burst, s.login_per_min, s.challenge_bits])

export function AdvancedProtection({ site }: { site: Site }) {
  const s = useSession()
  const id = useId()
  const [f, setF] = useState(() => formOf(site))
  const [busy, setBusy] = useState(false)
  const set = <K extends keyof Form>(k: K, v: Form[K]) => setF((x) => ({ ...x, [k]: v }))
  const off = !s.canChangeSite(site, "manager") || busy

  const save = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    if (!e.currentTarget.reportValidity()) return
    setBusy(true)
    try {
      await putShield(site, {
        waf: f.waf,
        body_waf: f.body_waf,
        xmlrpc: site.xmlrpc,
        admin_allow: splitList(f.admin_allow),
        trusted_ips: splitList(f.trusted),
        deny_ips: splitList(f.deny),
        reputation: f.reputation,
        country_mode: f.country_mode,
        countries: f.countries,
        country_action: f.country_action,
        rate_rps: Number(f.rate || 0),
        rate_burst: Number(f.burst || 0),
        login_per_min: Number(f.login || 0),
        challenge_bits: Number(f.bits || 0),
      })
      notify(`Advanced security settings saved for ${site.primary_domain}`)
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={save} className="@container flex flex-col gap-6 pt-4">
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-end gap-x-8 gap-y-4">
          <Label className="h-9 font-normal">
            <Switch checked={f.waf} disabled={off} onCheckedChange={(v) => set("waf", v)} />
            Firewall (URLs and headers)
          </Label>
          <Field className="w-auto min-w-56">
            <FieldLabel htmlFor={`${id}-body`}>Request bodies (OWASP CRS)</FieldLabel>
            <NativeSelect id={`${id}-body`} value={f.body_waf} disabled={off} onChange={(e) => set("body_waf", e.target.value)}>
              <NativeSelectOption value="off">Off</NativeSelectOption>
              <NativeSelectOption value="detect">Log only</NativeSelectOption>
              <NativeSelectOption value="block">Block</NativeSelectOption>
            </NativeSelect>
          </Field>
        </div>
        {site.body_waf === "detect" && (
          <Note>
            Log only: matches appear under Recent blocks below as “detect”. Switch to Block once nothing legitimate shows up there.
          </Note>
        )}
      </div>

      <FieldGroup className="grid gap-4 @2xl:grid-cols-3">
        <ListField id={`${id}-admin`} label="Admin allowlist" hint="Only these IPs/CIDRs reach wp-admin; empty = anyone." placeholder="203.0.113.7, 198.51.100.0/24" value={f.admin_allow} disabled={off} onChange={(v) => set("admin_allow", v)} />
        <ListField id={`${id}-trusted`} label="Trusted IPs" hint="Bypass protection: office, uptime monitor." placeholder="192.0.2.10" value={f.trusted} disabled={off} onChange={(v) => set("trusted", v)} />
        <ListField id={`${id}-deny`} label="Blocked IPs" hint="Denied on this site." placeholder="198.51.100.0/24" value={f.deny} disabled={off} onChange={(v) => set("deny", v)} />
      </FieldGroup>

      <FieldGroup className="grid gap-4 @2xl:grid-cols-3">
        <Field>
          <FieldLabel htmlFor={`${id}-rep`}>IP blocklists</FieldLabel>
          <NativeSelect id={`${id}-rep`} className="w-full" value={f.reputation} disabled={off} onChange={(e) => set("reputation", e.target.value)}>
            <NativeSelectOption value="off">Ignore</NativeSelectOption>
            <NativeSelectOption value="challenge">Check listed visitors</NativeSelectOption>
            <NativeSelectOption value="block">Block listed visitors</NativeSelectOption>
          </NativeSelect>
        </Field>
        <Field>
          <FieldLabel htmlFor={`${id}-cmode`}>Countries</FieldLabel>
          <NativeSelect id={`${id}-cmode`} className="w-full" value={f.country_mode} disabled={off} onChange={(e) => set("country_mode", e.target.value)}>
            <NativeSelectOption value="off">No country rule</NativeSelectOption>
            <NativeSelectOption value="block">Rule applies to these countries</NativeSelectOption>
            <NativeSelectOption value="allow">Rule applies to all others</NativeSelectOption>
          </NativeSelect>
        </Field>
        <Field>
          <FieldLabel htmlFor={`${id}-caction`}>Rule</FieldLabel>
          <NativeSelect id={`${id}-caction`} className="w-full" value={f.country_action} disabled={off} onChange={(e) => set("country_action", e.target.value)}>
            <NativeSelectOption value="block">Block</NativeSelectOption>
            <NativeSelectOption value="challenge">Check</NativeSelectOption>
          </NativeSelect>
        </Field>
        <Field className="@2xl:col-span-3">
          <FieldLabel htmlFor={`${id}-countries`}>Countries in the rule</FieldLabel>
          <CountryPicker id={`${id}-countries`} value={f.countries} disabled={off} onChange={(v) => set("countries", v)} />
        </Field>
      </FieldGroup>

      <div className="flex flex-col gap-3">
        <FieldGroup className="grid gap-4 @md:grid-cols-2 @3xl:grid-cols-4">
          <NumberField id={`${id}-rate`} label="Requests / s" hint="0 = 10" step="any" value={f.rate} disabled={off} onChange={(v) => set("rate", v)} />
          <NumberField id={`${id}-burst`} label="Request bursts" hint="0 = 60" step="1" value={f.burst} disabled={off} onChange={(v) => set("burst", v)} />
          <NumberField id={`${id}-login`} label="Logins / min" hint="0 = 6" step="any" value={f.login} disabled={off} onChange={(v) => set("login", v)} />
          <NumberField id={`${id}-bits`} label="Check difficulty, bits" hint="0 = 16" step="1" max={22} value={f.bits} disabled={off} onChange={(v) => set("bits", v)} />
        </FieldGroup>
        <Note>
          Limits are per visitor address. The visitor check is a proof-of-work a browser solves in about a second; each extra bit doubles the
          work.
        </Note>
      </div>

      {s.canChangeSite(site, "manager") && (
        <div>
          <Button type="submit" variant="tinted" disabled={busy}>
            {busy ? "Saving…" : "Save advanced settings"}
          </Button>
        </div>
      )}
    </form>
  )
}

function ListField({
  id,
  label,
  hint,
  placeholder,
  value,
  disabled,
  onChange,
}: {
  id: string
  label: ReactNode
  hint: ReactNode
  placeholder: string
  value: string
  disabled?: boolean
  onChange: (v: string) => void
}) {
  return (
    <Field>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Input id={id} value={value} placeholder={placeholder} disabled={disabled} autoComplete="off" spellCheck={false} onChange={(e) => onChange(e.target.value)} />
      <FieldDescription>{hint}</FieldDescription>
    </Field>
  )
}

function NumberField({
  id,
  label,
  hint,
  step,
  max,
  value,
  disabled,
  onChange,
}: {
  id: string
  label: ReactNode
  hint: ReactNode
  step: string
  max?: number
  value: string
  disabled?: boolean
  onChange: (v: string) => void
}) {
  return (
    <Field>
      <FieldLabel htmlFor={id}>
        <span>
          {label} <span className="font-normal text-muted-foreground">({hint})</span>
        </span>
      </FieldLabel>
      <Input id={id} type="number" min={0} max={max} step={step} inputMode="decimal" value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)} />
    </Field>
  )
}
