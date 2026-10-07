import { useState } from "react"
import { PlusIcon, Trash2Icon, ZapIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { InputGroup, InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"
import { fmtNum } from "@/lib/format"
import { currencySymbol, fromMinor, money, toMinor } from "@/lib/money"
import { SettingsCard, WithInvoicing, saveInvoicing, type BurstPack, type InvoicingSettings } from "./common"

// Burst minute packs: what clients billed by invoice can buy when their
// plan's minutes run out. A pack's ID follows its minutes (b500).

export function BurstPacks() {
  // After a save the editor starts again from what the server kept.
  const [version, setVersion] = useState(0)
  return <WithInvoicing>{(s) => <PacksForm key={version} s={s} onSaved={() => setVersion((v) => v + 1)} />}</WithInvoicing>
}

interface Row {
  key: number
  id: string
  minutes: string
  price: string
}

let rowKey = 0

function PacksForm({ s, onSaved }: { s: InvoicingSettings; onSaved: () => void }) {
  const [rows, setRows] = useState<Row[]>(() =>
    (s.burst_packs || []).map((p: BurstPack) => ({ key: ++rowKey, id: p.id || "", minutes: p.minutes ? String(p.minutes) : "", price: fromMinor(p.price) }))
  )
  // The row just added takes the focus.
  const [added, setAdded] = useState<number | null>(null)
  const change = (key: number, k: "minutes" | "price", v: string) => setRows(rows.map((r) => (r.key === key ? { ...r, [k]: v } : r)))

  return (
    <SettingsCard
      icon={ZapIcon}
      tint="yellow"
      title="Burst minute packs"
      intro="Clients billed by invoice can buy extra burst minutes when their plan's run out. Bought minutes never expire. No packs: nothing is for sale."
      ok="Save packs"
      onSave={async (form) => {
        const seen = new Set<string>()
        const packs: BurstPack[] = []
        const focus = (key: number, k: string) => form.querySelector<HTMLInputElement>(`[data-row="${key}"] [data-k="${k}"]`)?.focus()
        for (const row of rows) {
          const m = Math.trunc(Number(row.minutes))
          const v = toMinor(row.price)
          if (!m && v == null) continue
          if (!(m > 0)) {
            focus(row.key, "minutes")
            throw new Error("Each pack needs a number of minutes.")
          }
          if (v == null || Number.isNaN(v) || v <= 0) {
            focus(row.key, "price")
            throw new Error(`Give the ${fmtNum(m)}-minute pack a price.`)
          }
          // Keep a pack's ID while its minutes don't change; else b<minutes>.
          let id = row.id && row.id.replace(/-\d+$/, "") === `b${m}` ? row.id : `b${m}`
          for (let n = 2; seen.has(id); n++) id = `b${m}-${n}`
          seen.add(id)
          packs.push({ id, minutes: m, price: v })
        }
        await saveInvoicing({ burst_packs: packs.sort((a, b) => a.minutes - b.minutes) })
        onSaved()
      }}
    >
      <div className="flex flex-col gap-2">
        {rows.length > 0 && (
          <div aria-hidden className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2.25rem] gap-2 px-1 text-xs font-semibold tracking-wide text-muted-foreground uppercase sm:grid-cols-[11rem_11rem_minmax(0,1fr)_2.25rem]">
            <span>Minutes</span>
            <span>Price</span>
            <span className="text-right max-sm:hidden">Works out at</span>
            <span />
          </div>
        )}
        <div role="list" className="flex flex-col gap-2">
          {rows.map((r, i) => {
            const m = Number(r.minutes)
            const v = toMinor(r.price)
            const per = m > 0 && v != null && v > 0 ? `${money(Math.round((v / m) * 60))} an hour` : ""
            return (
              <div
                key={r.key}
                role="listitem"
                data-row={r.key}
                className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2.25rem] items-center gap-2 sm:grid-cols-[11rem_11rem_minmax(0,1fr)_2.25rem]"
              >
                <InputGroup>
                  <InputGroupInput
                    type="number"
                    min={1}
                    step={1}
                    data-k="minutes"
                    aria-label={`Pack ${i + 1}: minutes`}
                    placeholder="500"
                    autoFocus={r.key === added}
                    value={r.minutes}
                    onChange={(e) => change(r.key, "minutes", e.target.value)}
                  />
                  <InputGroupAddon align="inline-end">
                    <InputGroupText>minutes</InputGroupText>
                  </InputGroupAddon>
                </InputGroup>
                <InputGroup>
                  <InputGroupAddon>
                    <InputGroupText aria-hidden>{currencySymbol()}</InputGroupText>
                  </InputGroupAddon>
                  <InputGroupInput
                    inputMode="decimal"
                    data-k="price"
                    aria-label={`Pack ${i + 1}: price`}
                    placeholder={fromMinor(0)}
                    value={r.price}
                    onChange={(e) => change(r.key, "price", e.target.value)}
                  />
                </InputGroup>
                <span className="text-right text-sm whitespace-nowrap text-muted-foreground tabular-nums max-sm:order-last max-sm:col-span-2 max-sm:text-left">{per}</span>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={`Remove pack ${i + 1}`}
                  onClick={() => setRows(rows.filter((x) => x.key !== r.key))}
                >
                  <Trash2Icon />
                </Button>
              </div>
            )
          })}
        </div>
        <div>
          <Button
            type="button"
            variant="tinted"
            onClick={() => {
              const key = ++rowKey
              setRows([...rows, { key, id: "", minutes: "", price: "" }])
              setAdded(key)
            }}
          >
            <PlusIcon data-icon="inline-start" />
            Add a pack
          </Button>
        </div>
      </div>
    </SettingsCard>
  )
}
