import { useEffect, useId, useRef, useState } from "react"
import { Checkbox } from "@/components/ui/checkbox"
import { Label } from "@/components/ui/label"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { MoneyInput } from "@/components/app/blocks"
import { CYCLES, fromMinor, money, toMinor } from "@/lib/money"
import type { PlanPrice } from "./types"

// A plan's prices per billing cycle (the legacy billing.js
// renderPriceEditor / readPriceEditor / planPriceText). The fields are
// uncontrolled, for a <form>: read them back with readPriceEditor(form).
// Render only once useBillingConfig() has loaded (amounts are in its
// currency).

export type PlanPrices = Record<string, PlanPrice>

interface Priced {
  prices?: Record<string, PlanPrice> | null
}

type Cycle = (typeof CYCLES)[number]

export function PriceEditor({ plan }: { plan?: Priced | null }) {
  const prices = plan?.prices || {}
  return (
    <Table>
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          <TableHead scope="col">Billing period</TableHead>
          <TableHead scope="col">Price</TableHead>
          <TableHead scope="col">Setup fee</TableHead>
          <TableHead scope="col" className="text-right">
            Works out at
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {CYCLES.map((c) => (
          <PriceRow key={c.id} cycle={c} price={prices[c.id]} />
        ))}
      </TableBody>
    </Table>
  )
}

function PriceRow({ cycle: c, price: pr }: { cycle: Cycle; price?: PlanPrice }) {
  const id = useId()
  const [offer, setOffer] = useState(!!pr)
  const [text, setText] = useState(pr ? fromMinor(pr.price) : "")
  const priceRef = useRef<HTMLInputElement>(null)
  const focusNext = useRef(false)

  // Ticking a period puts the cursor in its price.
  useEffect(() => {
    if (offer && focusNext.current) priceRef.current?.focus()
    focusNext.current = false
  }, [offer])

  const v = toMinor(text)
  const per = offer && v && c.months > 1 ? `${money(Math.round(v / c.months))} / month` : ""

  return (
    <TableRow className="hover:bg-transparent">
      <TableCell>
        <Label htmlFor={`${id}-offer`} className="font-normal">
          <Checkbox
            id={`${id}-offer`}
            name={`offer_${c.id}`}
            checked={offer}
            aria-label={`Offer ${c.label.toLowerCase()}`}
            onCheckedChange={(x) => {
              focusNext.current = !!x
              setOffer(!!x)
            }}
          />
          {c.label}
        </Label>
      </TableCell>
      <TableCell className="min-w-32">
        <MoneyInput
          ref={priceRef}
          name={`price_${c.id}`}
          minor={pr ? pr.price : null}
          disabled={!offer}
          aria-label={`${c.label} price`}
          onChange={(e) => setText(e.target.value)}
        />
      </TableCell>
      <TableCell className="min-w-32">
        <MoneyInput name={`setup_${c.id}`} minor={pr ? pr.setup_fee : null} disabled={!offer} aria-label={`${c.label} setup fee`} />
      </TableCell>
      <TableCell className="text-right text-sm text-muted-foreground tabular-nums">{per}</TableCell>
    </TableRow>
  )
}

// readPriceEditor is the prices the form offers ({} when none: free). It
// throws, in words, when a ticked period's price isn't an amount.
export function readPriceEditor(form: HTMLFormElement): PlanPrices {
  const field = (name: string) => form.elements.namedItem(name) as HTMLInputElement | null
  const out: PlanPrices = {}
  for (const c of CYCLES) {
    const offer = field(`offer_${c.id}`)
    if (!offer || !offer.checked) continue
    const price = toMinor(field(`price_${c.id}`)?.value)
    const setup = toMinor(field(`setup_${c.id}`)?.value)
    if (price == null || Number.isNaN(price) || price < 0) throw new Error(`Enter the ${c.label.toLowerCase()} price, or untick it.`)
    if (setup != null && (Number.isNaN(setup) || setup < 0)) throw new Error(`The ${c.label.toLowerCase()} setup fee isn't an amount.`)
    out[c.id] = { price, setup_fee: setup || 0 }
  }
  return out
}

// planCycles are the billing periods a plan is sold for, shortest first.
export const planCycles = (p: Priced) => CYCLES.filter((c) => p.prices?.[c.id])

// planPriceText is a plan's price for the Plans table: its shortest
// period's ("$12.00 / month"), or "free".
export function planPriceText(plan: Priced): string {
  const cs = planCycles(plan)
  if (!cs.length) return "free"
  const first = cs[0]
  return `${money(plan.prices![first.id].price)} / ${first.per}`
}
