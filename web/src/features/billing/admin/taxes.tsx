import { useState } from "react"
import { LandmarkIcon, PlusIcon, TriangleAlertIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, Banner, BTable, EmptyState, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { fmtRate, listOf, toRate } from "@/lib/money"
import { useApi } from "@/lib/query"
import { navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { billingPath } from "../route"
import {
  GroupLabel, Labeled, LiveHint, PercentField, Segmented, SelectField, SubHeading, TextField, ToggleField, countryList, countryName, fieldError, grid,
  rateText, type InvoicingSettings,
} from "./common"

// Tax rules: who pays which tax, by where they are; presets write the
// usual rules (the operator checks the list first).

export interface TaxRule {
  id?: number
  name: string
  country: string
  state: string
  rate: number | null
  level: number
  compound: boolean
}

const rule = (name: string, country: string, state: string, rate: number | null, level = 1, compound = false): TaxRule => ({
  name,
  country,
  state: state || "",
  rate,
  level,
  compound,
})

// Standard VAT rates of the EU countries (hundredths of a percent), 2025.
const EU_VAT: Array<[string, number]> = [
  ["AT", 2000], ["BE", 2100], ["BG", 2000], ["HR", 2500], ["CY", 1900], ["CZ", 2100], ["DK", 2500], ["EE", 2400],
  ["FI", 2550], ["FR", 2000], ["DE", 1900], ["GR", 2400], ["HU", 2700], ["IE", 2300], ["IT", 2200], ["LV", 2100], ["LT", 2100],
  ["LU", 1700], ["MT", 1800], ["NL", 2100], ["PL", 2300], ["PT", 2300], ["RO", 2100], ["SK", 2300], ["SI", 2200], ["ES", 2100], ["SE", 2500],
]

interface TaxPreset {
  id: string
  label: string
  needState?: string
  help?: string
  rules: (state: string) => TaxRule[]
}

const TAX_PRESETS: TaxPreset[] = [
  {
    id: "in",
    label: "India GST 18% (CGST 9% + SGST 9%)",
    needState: "Your state",
    help: "Clients in your state pay CGST and SGST; clients in the rest of India pay IGST. Write the state as your clients' addresses do.",
    rules: (st) => [rule("CGST", "IN", st, 900, 1), rule("SGST", "IN", st, 900, 2), rule("IGST", "IN", "", 1800, 1)],
  },
  {
    id: "eu",
    label: "EU VAT (each country's standard rate)",
    help:
      "Charges the VAT of the client's country (the One-Stop Shop scheme). For businesses with a VAT number, turn on " +
      '"No tax with a tax ID" in Settings → Taxes (reverse charge). Rates as of 2025: check them before you rely on them.',
    rules: () => EU_VAT.map(([c, r]) => rule("VAT", c, "", r, 1)),
  },
  {
    id: "ca",
    label: "Canada GST/HST (+ PST/QST)",
    help:
      "GST everywhere; HST instead in ON, NB, NS, NL and PE; provincial sales tax on top in BC, MB, SK and QC. QST (9.975%) " +
      "is rounded to 9.98%. Provinces as two-letter codes.",
    rules: () => [
      rule("GST", "CA", "", 500, 1), rule("HST", "CA", "ON", 1300, 1), rule("HST", "CA", "NB", 1500, 1), rule("HST", "CA", "NS", 1400, 1),
      rule("HST", "CA", "NL", 1500, 1), rule("HST", "CA", "PE", 1500, 1), rule("PST", "CA", "BC", 700, 2), rule("RST", "CA", "MB", 700, 2),
      rule("PST", "CA", "SK", 600, 2), rule("QST", "CA", "QC", 998, 2),
    ],
  },
  { id: "uk", label: "UK VAT 20%", rules: () => [rule("VAT", "GB", "", 2000, 1)] },
  { id: "au", label: "Australia GST 10%", rules: () => [rule("GST", "AU", "", 1000, 1)] },
]

const ruleWhere = (r: TaxRule) => (r.country ? countryName(r.country) + (r.state ? `, ${r.state}` : "") : "Everywhere")
const sameRule = (a: TaxRule, b: TaxRule) => a.name === b.name && a.country === b.country && (a.state || "") === (b.state || "") && a.level === b.level

type DialogReq = { kind: "rule"; r: TaxRule | null } | { kind: "preset"; preset: TaxPreset }
type Dialog = DialogReq & { key: number }

export function Taxes() {
  const s = useSession()
  const rulesQ = useApi<unknown>("/billing/tax-rules")
  // Only to warn that taxes are off; the rules work without it.
  const settings = useApi<InvoicingSettings>("/billing/invoicing")
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [open, setOpen] = useState(false)

  if (!s.atLeast("admin")) return <LoadError error={new Error("Tax rules are for administrators.")} />
  if (rulesQ.error) return <LoadError error={rulesQ.error} retry={() => rulesQ.refetch()} />
  if (!rulesQ.data) return <Skeleton className="h-64 rounded-2xl" />

  const rules = listOf<TaxRule>(rulesQ.data, "rules")
  const reload = () => rulesQ.refetch()
  const show = (d: DialogReq) => {
    setDialog({ ...d, key: Date.now() })
    setOpen(true)
  }
  const sorted = rules.slice().sort((a, b) => ruleWhere(a).localeCompare(ruleWhere(b)) || a.level - b.level)
  const taxOff = settings.data?.tax && !settings.data.tax.enabled

  return (
    <>
      {taxOff && (
        <Banner
          tone="warn"
          icon={TriangleAlertIcon}
          title="Taxes are off"
          actions={<Button onClick={() => navigate(billingPath("settings", "taxes"))}>Turn on taxes</Button>}
        >
          These rules aren't applied until taxes are turned on.
        </Banner>
      )}
      <Section
        icon={LandmarkIcon}
        tint="brown"
        title="Tax rules"
        description={
          "Each client pays the rules for where they are: a rule for their state beats one for their country, " +
          "which beats one for everywhere. Level 2 is a second tax on top (like SGST after CGST, or PST after GST); " +
          "a compound level 2 is also charged on the level 1 tax."
        }
        action={
          <Button onClick={() => show({ kind: "rule", r: null })}>
            <PlusIcon data-icon="inline-start" />
            New rule
          </Button>
        }
        contentClassName="flex flex-col gap-4"
      >
        <div className="flex flex-col gap-2.5">
          <SubHeading>Start from a preset</SubHeading>
          <div className="flex flex-wrap gap-2">
            {TAX_PRESETS.map((p) => (
              <Button key={p.id} variant="tinted" size="sm" onClick={() => show({ kind: "preset", preset: p })}>
                {p.label}
              </Button>
            ))}
          </div>
        </div>
        <BTable
          caption="Tax rules"
          cols={["Name", "Where", { label: "Rate", num: true }, "Level", ""]}
          rows={sorted.map((r) => ({
            key: r.id,
            cells: [
              <strong className="font-semibold">{r.name}</strong>,
              ruleWhere(r),
              fmtRate(r.rate ?? 0),
              r.level === 2 ? `2${r.compound ? ", compound" : ""}` : "1",
              <div className="flex justify-end gap-1.5">
                <Button variant="tinted" size="sm" onClick={() => show({ kind: "rule", r })}>
                  Edit
                </Button>
                <ActionButton
                  variant="destructive"
                  size="sm"
                  run={async () => {
                    if (!(await ask(`Delete the ${r.name} rule for ${ruleWhere(r)}? Invoices already issued keep their tax.`))) return
                    await api("DELETE", `/billing/tax-rules/${r.id}`)
                    reload()
                  }}
                >
                  Delete
                </ActionButton>
              </div>,
            ],
          }))}
          empty={
            <EmptyState icon={LandmarkIcon} tint="brown" title="No tax rules" className="bg-transparent py-6 shadow-none">
              Without rules no tax is charged. Start from a preset above, or add a rule for your country.
            </EmptyState>
          }
        />
      </Section>
      {dialog?.kind === "rule" && <TaxRuleDialog key={dialog.key} open={open} onOpenChange={setOpen} r={dialog.r} onSaved={reload} />}
      {dialog?.kind === "preset" && (
        <PresetDialog key={dialog.key} open={open} onOpenChange={setOpen} preset={dialog.preset} existing={rules} onSaved={reload} />
      )}
    </>
  )
}

// The rules a preset adds, with the ones already there marked.
function PresetDialog({
  open,
  onOpenChange,
  preset,
  existing,
  onSaved,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  preset: TaxPreset
  existing: TaxRule[]
  onSaved: () => void
}) {
  const [state, setState] = useState("")
  const st = state.trim()
  const rules = preset.rules(st)
  // Which rules take the state typed in (a marker tells them apart).
  const takesState = preset.rules("\u0001").map((r) => r.state === "\u0001")
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      wide
      title={preset.label}
      intro={preset.help}
      ok="Add these rules"
      onSubmit={async () => {
        const add = preset.rules(st).filter((r) => !existing.some((x) => sameRule(x, r)))
        for (const r of add) await api("POST", "/billing/tax-rules", r)
        notify(`${add.length} tax rule${add.length === 1 ? "" : "s"} added`)
        onSaved()
      }}
    >
      {preset.needState && (
        <Labeled label={preset.needState}>
          <TextField name="state" required placeholder="e.g. Maharashtra" value={state} onChange={(e) => setState(e.target.value)} />
        </Labeled>
      )}
      <BTable
        cols={["Name", "Where", { label: "Rate", num: true }, "Level", ""]}
        rows={rules.map((r, i) => ({
          cells: [
            r.name,
            takesState[i] && !st ? `${countryName(r.country)}, your state` : ruleWhere(r),
            fmtRate(r.rate ?? 0),
            String(r.level),
            existing.some((x) => sameRule(x, r)) ? <span className="text-xs text-muted-foreground">already there</span> : "",
          ],
        }))}
      />
    </FormDialog>
  )
}

function TaxRuleDialog({ open, onOpenChange, r: given, onSaved }: { open: boolean; onOpenChange: (o: boolean) => void; r: TaxRule | null; onSaved: () => void }) {
  const r = given || rule("", "", "", null, 1, false)
  const [name, setName] = useState(r.name)
  const [rate, setRate] = useState(rateText(r.rate))
  const [country, setCountry] = useState(r.country || "")
  const [state, setState] = useState(r.state || "")
  const [level, setLevel] = useState<"1" | "2">(r.level === 2 ? "2" : "1")
  const [compound, setCompound] = useState(!!r.compound)

  const rv = toRate(rate)
  const hint =
    `${Number.isNaN(rv) ? "…" : fmtRate(rv)} ${name.trim() || "tax"} for clients ` +
    (country ? `in ${state.trim() ? state.trim() + ", " : ""}${countryName(country)}` : "anywhere without a more specific rule") +
    (level === "2" ? ", on top of the level 1 tax" : "") +
    "."

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title={r.id ? `Edit ${r.name}` : "New tax rule"}
      onSubmit={async () => {
        const v = toRate(rate)
        if (Number.isNaN(v) || v < 0 || v > 10000) throw fieldError("rate", "Enter a rate between 0 and 100%.")
        const lvl = Number(level)
        const body = { name: name.trim(), country, state: country ? state.trim() : "", rate: v, level: lvl, compound: lvl === 2 && compound }
        if (r.id) await api("PUT", `/billing/tax-rules/${r.id}`, body)
        else await api("POST", "/billing/tax-rules", body)
        notify("Tax rule saved")
        onSaved()
      }}
    >
      <div className={grid}>
        <Labeled label="Name" help="Printed on invoices.">
          <TextField name="name" required maxLength={40} placeholder="GST, VAT…" value={name} onChange={(e) => setName(e.target.value)} />
        </Labeled>
        <Labeled label="Rate">
          <PercentField name="rate" required value={rate} onValue={setRate} />
        </Labeled>
      </div>
      <div className={grid}>
        <Labeled label="Country">
          <SelectField name="country" value={country} onChange={(e) => setCountry(e.target.value)}>
            <NativeSelectOption value="">Everywhere</NativeSelectOption>
            {countryList().map(([code, n]) => (
              <NativeSelectOption key={code} value={code}>
                {n}
              </NativeSelectOption>
            ))}
          </SelectField>
        </Labeled>
        <Labeled label="State / province" help="Empty: the whole country.">
          <TextField name="state" placeholder="every state" disabled={!country} value={state} onChange={(e) => setState(e.target.value)} />
        </Labeled>
      </div>
      <div className="flex flex-col gap-3">
        <GroupLabel>Level</GroupLabel>
        <Segmented
          name="level"
          label="Level"
          value={level}
          onChange={setLevel}
          options={[
            ["1", "1 · the main tax"],
            ["2", "2 · a second tax"],
          ]}
        />
      </div>
      {level === "2" && (
        <ToggleField name="compound" label="Compound: charged on the price plus the level 1 tax" checked={compound} onChange={setCompound} />
      )}
      <LiveHint>{hint}</LiveHint>
    </FormDialog>
  )
}
