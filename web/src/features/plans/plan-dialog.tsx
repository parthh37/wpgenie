import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { FormDialog, MoneyInput } from "@/components/app/blocks"
import { notify } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { splitList } from "@/lib/format"
import { toMinor } from "@/lib/money"
import { invalidate } from "@/lib/query"
import { PriceEditor, readPriceEditor } from "./price-editor"
import { FEATURES, type Plan } from "./types"

// Creating or editing a plan (the legacy #plan-form): its limits per
// account and per site, features, backup destinations, what happens past
// the bandwidth, whether resellers may hand it out and, when the server
// sells plans, its prices and place on the order page. Editing a plan and
// changing its ID saves a copy under the new ID, as the legacy form did.

// New plans start from these (the legacy form's values).
const DEFAULTS = {
  max_sites: 1,
  disk_mb: 10240,
  bandwidth_gb: 100,
  max_replicas: 1,
  max_memory_mb: 512,
  max_cpus: 1,
  max_domains: 5,
  burst_minutes: 600,
}

export function PlanDialog({
  open,
  onOpenChange,
  plan,
  pricing,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  // The plan edited; null for a new one.
  plan: Plan | null
  // The server's plans have prices (older servers reject the fields).
  pricing: boolean
}) {
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      wide
      title={plan ? `Edit ${plan.name}` : "Create a plan"}
      intro="Limits are per account, and per site where it says so. 0 means unlimited (the server's own limits still apply)."
      ok="Save plan"
      onSubmit={async (data, form) => {
        const num = (k: string) => Number(data.get(k) || 0)
        const checked = (k: string) => !!(form.elements.namedItem(k) as HTMLInputElement | null)?.checked
        const body: Record<string, unknown> = {
          id: String(data.get("id") || "").trim(),
          name: String(data.get("name") || "").trim(),
          max_sites: num("max_sites"),
          disk_mb: num("disk_mb"),
          bandwidth_gb: num("bandwidth_gb"),
          max_replicas: num("max_replicas"),
          max_memory_mb: num("max_memory_mb"),
          max_cpus: num("max_cpus"),
          max_domains: num("max_domains"),
          burst_minutes: num("burst_minutes"),
          overage: String(data.get("overage") || "notify"),
          resellable: checked("resellable"),
          features: FEATURES.filter((f) => checked(`feature_${f}`)),
          backup_repos: splitList(String(data.get("backup_repos") || "")),
        }
        if (pricing) {
          const overage = toMinor(String(data.get("overage_gb_price") || ""))
          if (overage != null && (Number.isNaN(overage) || overage < 0)) throw new Error("The price per extra GB isn't an amount.")
          Object.assign(body, {
            description: String(data.get("description") || "").trim(),
            public: checked("public"),
            account_kind: String(data.get("account_kind") || "customer"),
            sort: Number(data.get("sort") || 0),
            overage_gb_price: overage || 0,
            prices: readPriceEditor(form),
          })
        }
        if (plan && plan.id === body.id) await api("PUT", `/plans/${encodeURIComponent(plan.id)}`, body)
        else await api("POST", "/plans", body)
        await invalidate("/plans")
        invalidate("/accounts")
        notify(`Plan ${body.name} saved`)
      }}
    >
      <PlanFields plan={plan} pricing={pricing} />
    </FormDialog>
  )
}

function NumberField({ name, label, value, step, hint }: { name: string; label: string; value: number; step?: string; hint?: string }) {
  return (
    <Field>
      <FieldLabel htmlFor={`plan-${name}`}>{label}</FieldLabel>
      <Input id={`plan-${name}`} name={name} type="number" min={0} step={step} defaultValue={value} />
      {hint && <FieldDescription>{hint}</FieldDescription>}
    </Field>
  )
}

function PlanFields({ plan, pricing }: { plan: Plan | null; pricing: boolean }) {
  const p = plan
  const v = (k: keyof typeof DEFAULTS) => (p ? Number(p[k] ?? 0) : DEFAULTS[k])
  const features = new Set(p?.features ?? [])
  return (
    <div className="flex flex-col gap-6">
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="plan-id">ID</FieldLabel>
          <Input
            id="plan-id"
            name="id"
            required
            pattern="[a-z0-9][a-z0-9\-]{0,31}"
            title="Lowercase letters, digits and -, up to 32"
            defaultValue={p?.id ?? ""}
            placeholder="starter"
            autoComplete="off"
          />
          <FieldDescription>{p ? "Lowercase. A different ID saves a copy as a new plan." : "Lowercase, e.g. starter."}</FieldDescription>
        </Field>
        <Field>
          <FieldLabel htmlFor="plan-name">Name</FieldLabel>
          <Input id="plan-name" name="name" required defaultValue={p?.name ?? ""} placeholder="Starter" />
        </Field>
      </FieldGroup>

      <FieldSet>
        <FieldLegend>Per account</FieldLegend>
        <FieldDescription>Sites count staging copies; disk is files and databases; bandwidth is per calendar month (UTC).</FieldDescription>
        <FieldGroup className="grid gap-4 sm:grid-cols-3">
          <NumberField name="max_sites" label="Sites" value={v("max_sites")} />
          <NumberField name="disk_mb" label="Disk (MB)" value={v("disk_mb")} />
          <NumberField name="bandwidth_gb" label="Bandwidth / month (GB)" value={v("bandwidth_gb")} />
          <NumberField name="burst_minutes" label="Burst minutes / month" value={v("burst_minutes")} hint="With the burst feature." />
          <Field>
            <FieldLabel htmlFor="plan-overage">Past the bandwidth</FieldLabel>
            <NativeSelect id="plan-overage" name="overage" defaultValue={p?.overage || "notify"} className="w-full">
              <NativeSelectOption value="notify">Notify</NativeSelectOption>
              <NativeSelectOption value="suspend">Suspend the account</NativeSelectOption>
            </NativeSelect>
          </Field>
          <Field>
            <FieldLabel htmlFor="plan-backup_repos">Backup destinations</FieldLabel>
            <Input id="plan-backup_repos" name="backup_repos" defaultValue={(p?.backup_repos ?? []).join(", ")} placeholder="local" />
            <FieldDescription>IDs, comma-separated.</FieldDescription>
          </Field>
        </FieldGroup>
      </FieldSet>

      <FieldSet>
        <FieldLegend>Per site</FieldLegend>
        <FieldGroup className="grid gap-4 sm:grid-cols-4">
          <NumberField name="max_replicas" label="Replicas per site" value={v("max_replicas")} />
          <NumberField name="max_memory_mb" label="Memory per replica (MB)" value={v("max_memory_mb")} />
          <NumberField name="max_cpus" label="CPUs per replica" value={v("max_cpus")} step="0.25" />
          <NumberField name="max_domains" label="Domains per site" value={v("max_domains")} />
        </FieldGroup>
      </FieldSet>

      <FieldSet>
        <FieldLegend>Features</FieldLegend>
        <div className="flex flex-wrap gap-x-5 gap-y-3">
          {FEATURES.map((f) => (
            <Label key={f} className="font-normal">
              <Checkbox name={`feature_${f}`} defaultChecked={features.has(f)} />
              {f}
            </Label>
          ))}
        </div>
        <Label className="font-normal">
          <Checkbox name="resellable" defaultChecked={!!p?.resellable} />
          Resellers may assign it
        </Label>
      </FieldSet>

      {pricing && (
        <FieldSet>
          <FieldLegend>Selling it</FieldLegend>
          <FieldDescription>Tick the billing periods you offer and their prices. A plan without prices is free and can't be ordered.</FieldDescription>
          <PriceEditor plan={p} />
          <FieldGroup className="grid gap-4 sm:grid-cols-2">
            <Field>
              <FieldLabel htmlFor="plan-description">Description</FieldLabel>
              <Input id="plan-description" name="description" maxLength={200} placeholder="For growing stores" defaultValue={p?.description ?? ""} />
              <FieldDescription>On the order page.</FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="plan-account_kind">An order creates</FieldLabel>
              <NativeSelect id="plan-account_kind" name="account_kind" defaultValue={p?.account_kind || "customer"} className="w-full">
                <NativeSelectOption value="customer">A customer account</NativeSelectOption>
                <NativeSelectOption value="reseller">A reseller account</NativeSelectOption>
              </NativeSelect>
            </Field>
            <Field>
              <FieldLabel htmlFor="plan-sort">Position on the order page</FieldLabel>
              <Input id="plan-sort" name="sort" type="number" defaultValue={p?.sort ?? 0} />
              <FieldDescription>Lower first.</FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="plan-overage_gb_price">Extra bandwidth</FieldLabel>
              <MoneyInput id="plan-overage_gb_price" name="overage_gb_price" minor={p?.overage_gb_price ?? 0} />
              <FieldDescription>Price per GB past the plan; 0: none.</FieldDescription>
            </Field>
          </FieldGroup>
          <Label className="font-normal">
            <Checkbox name="public" defaultChecked={!!p?.public} />
            Show it on the order page
          </Label>
        </FieldSet>
      )}
    </div>
  )
}
