import { XIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { countryList, countryName } from "./countries"

// CountryPicker: the countries a rule names, as removable chips, and a
// list to add one by its name (the legacy panel had codes typed in).
export function CountryPicker({
  id,
  value,
  onChange,
  disabled,
}: {
  id?: string
  value: string[]
  onChange: (codes: string[]) => void
  disabled?: boolean
}) {
  const chosen = new Set(value)
  return (
    <div className="flex flex-col gap-2">
      <NativeSelect
        id={id}
        value=""
        disabled={disabled}
        className="w-full"
        onChange={(e) => {
          const c = e.target.value
          if (c && !chosen.has(c)) onChange([...value, c])
        }}
      >
        <NativeSelectOption value="">{value.length ? "Add another country…" : "Add a country…"}</NativeSelectOption>
        {countryList()
          .filter(([c]) => !chosen.has(c))
          .map(([c, name]) => (
            <NativeSelectOption key={c} value={c}>
              {name}
            </NativeSelectOption>
          ))}
      </NativeSelect>
      {value.length > 0 && (
        <ul aria-label="Countries chosen" className="flex flex-wrap gap-1.5">
          {value.map((c) => (
            <li key={c}>
              <Badge variant="secondary" className="h-7 gap-1 pr-1 pl-2.5 text-sm">
                {countryName(c)} <span className="text-xs text-muted-foreground">{c}</span>
                {!disabled && (
                  <button
                    type="button"
                    aria-label={`Remove ${countryName(c)}`}
                    onClick={() => onChange(value.filter((x) => x !== c))}
                    className="ml-0.5 flex size-5 items-center justify-center rounded-full text-muted-foreground hover:bg-foreground/10 hover:text-foreground"
                  >
                    <XIcon className="size-3" />
                  </button>
                )}
              </Badge>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
