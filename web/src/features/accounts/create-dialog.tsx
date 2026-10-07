import { useState } from "react"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { FormDialog } from "@/components/app/blocks"
import { showSecret } from "@/components/app/secret"
import { notify } from "@/components/app/toaster"
import type { Plan } from "@/features/plans/types"
import { api } from "@/lib/api"
import { invalidate } from "@/lib/query"
import { navigate } from "@/lib/router"
import { useSession } from "@/lib/session"
import { accountPath, type Account, type AccountUser } from "./types"

// Creating an account with, optionally, its first user (the password is
// generated and shown once). Administrators pick the kind and reseller; a
// reseller's new accounts are always their customers, on a plan they may
// hand out.
export function CreateAccountDialog({
  open,
  onOpenChange,
  accounts,
  plans,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  accounts: Account[]
  plans: Plan[]
}) {
  const s = useSession()
  const [kind, setKind] = useState("customer")
  const offered = plans.filter((p) => !s.isTenant || p.resellable)
  const resellers = accounts.filter((a) => a.kind === "reseller")

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Create an account"
      ok="Create account"
      onSubmit={async (data) => {
        const body: Record<string, unknown> = {
          name: String(data.get("name") || "").trim(),
          plan_id: String(data.get("plan_id") || ""),
          email: String(data.get("email") || "").trim(),
        }
        if (!s.isTenant) {
          body.kind = String(data.get("kind") || "customer")
          body.parent_id = body.kind === "customer" ? Number(data.get("parent_id") || 0) : 0
        }
        const username = String(data.get("username") || "").trim()
        if (username) body.user = { username }
        const r = await api<{ account: Account; user?: AccountUser; password?: string; existed?: boolean }>("POST", "/accounts", body)
        await invalidate("/accounts")
        if (r.password && r.user) showSecret(`Account ${r.account.name}`, [`User:     ${r.user.username}`, `Password: ${r.password}`])
        else notify(`Account ${r.account.name} created`)
        navigate(accountPath(r.account.id))
      }}
    >
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field className="sm:col-span-2">
          <FieldLabel htmlFor="acct-name">Name</FieldLabel>
          <Input id="acct-name" name="name" required maxLength={100} placeholder="Acme Ltd" autoComplete="off" />
        </Field>
        {!s.isTenant && (
          <Field>
            <FieldLabel htmlFor="acct-kind">Kind</FieldLabel>
            <NativeSelect id="acct-kind" name="kind" value={kind} onChange={(e) => setKind(e.target.value)} className="w-full">
              <NativeSelectOption value="customer">Customer</NativeSelectOption>
              <NativeSelectOption value="reseller">Reseller</NativeSelectOption>
            </NativeSelect>
          </Field>
        )}
        <Field className={s.isTenant ? "sm:col-span-2" : undefined}>
          <FieldLabel htmlFor="acct-plan">Plan</FieldLabel>
          <NativeSelect id="acct-plan" name="plan_id" required className="w-full">
            {offered.map((p) => (
              <NativeSelectOption key={p.id} value={p.id}>
                {p.name} ({p.id})
              </NativeSelectOption>
            ))}
          </NativeSelect>
          {!offered.length && <FieldDescription>There are no plans you can assign yet.</FieldDescription>}
        </Field>
        {!s.isTenant && (
          <Field className="sm:col-span-2">
            <FieldLabel htmlFor="acct-parent">Reseller</FieldLabel>
            <NativeSelect id="acct-parent" name="parent_id" defaultValue="0" disabled={kind !== "customer"} className="w-full">
              <NativeSelectOption value="0">None</NativeSelectOption>
              {resellers.map((a) => (
                <NativeSelectOption key={a.id} value={String(a.id)}>
                  {a.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
            <FieldDescription>Customers only.</FieldDescription>
          </Field>
        )}
        <Field>
          <FieldLabel htmlFor="acct-email">E-mail</FieldLabel>
          <Input id="acct-email" name="email" type="email" autoComplete="off" />
          <FieldDescription>Optional.</FieldDescription>
        </Field>
        <Field>
          <FieldLabel htmlFor="acct-user">First user</FieldLabel>
          <Input id="acct-user" name="username" placeholder="username" autoComplete="off" autoCapitalize="none" spellCheck={false} />
          <FieldDescription>Optional; password generated.</FieldDescription>
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}
