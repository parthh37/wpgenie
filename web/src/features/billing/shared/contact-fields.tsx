// CONTRACT (owner: billing-core agent). A billing contact's fields
// (uncontrolled, for a <form>; names as the API's), shared by staff and the
// client area. readContact turns the form back into the API's shape.
import { useState } from "react"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { cn } from "@/lib/utils"
import { STATE_COUNTRIES, countryList, guessCountry } from "@/lib/countries"
import { LabeledField } from "./dialog-form"
import type { Contact } from "./types"

export type { Contact } from "./types"

export const CONTACT_KEYS = ["first_name", "last_name", "company", "email", "phone", "address1", "address2", "city", "state", "postcode", "country", "tax_id"] as const

export function ContactFields({ contact, className }: { contact?: Contact; className?: string }) {
  const c = contact || {}
  const [country, setCountry] = useState<string>(() => c.country || guessCountry())
  // Countries whose taxes depend on the state need one.
  const needsState = STATE_COUNTRIES.includes(country)

  const text = (name: keyof Contact & string, label: string, autoComplete: string, attrs: React.ComponentProps<"input"> = {}, help?: string) => (
    <LabeledField label={label} help={help}>
      {(a) => <Input {...a} name={name} autoComplete={autoComplete} defaultValue={c[name] || ""} {...attrs} />}
    </LabeledField>
  )

  return (
    <div className={cn("grid gap-4 sm:grid-cols-2", className)}>
      {text("first_name", "First name", "given-name", { required: true })}
      {text("last_name", "Last name", "family-name", { required: true })}
      {text("company", "Company (optional)", "organization")}
      {text("email", "E-mail for invoices", "email", { type: "email", required: true })}
      {text("phone", "Phone (optional)", "tel", { type: "tel" })}
      {text("address1", "Address", "address-line1", { required: true })}
      {text("address2", "Address, line 2 (optional)", "address-line2")}
      {text("city", "City", "address-level2", { required: true })}
      {text("state", needsState ? "State / province" : "State / province (optional)", "address-level1", { required: needsState })}
      {text("postcode", "Postcode", "postal-code")}
      <LabeledField label="Country">
        {(a) => (
          <NativeSelect {...a} name="country" autoComplete="country" required className="w-full" defaultValue={country} onChange={(e) => setCountry(e.target.value)}>
            <NativeSelectOption value="">Choose…</NativeSelectOption>
            {countryList().map(([code, name]) => (
              <NativeSelectOption key={code} value={code}>
                {name}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </LabeledField>
      {text("tax_id", "Tax ID (optional)", "off", {}, "VAT, GST or a similar number, if you're a registered business.")}
    </div>
  )
}

// readContact is the contact a form with ContactFields holds, trimmed.
export function readContact(form: HTMLFormElement): Contact {
  return Object.fromEntries(
    CONTACT_KEYS.map((k) => {
      const el = form.elements.namedItem(k) as HTMLInputElement | HTMLSelectElement | null
      return [k, ((el && el.value) || "").trim()]
    })
  )
}
