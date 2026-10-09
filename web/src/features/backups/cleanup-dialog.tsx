import { useState } from "react"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { FormDialog } from "@/components/app/blocks"

// Deleting backups in bulk: a site's (everywhere, or in one destination)
// or a whole destination's. Some kinds only, if wanted; typing a name to
// confirm, since nothing brings them back.

export interface CleanupBody {
  repo_id?: string
  kinds: string[] // empty: every kind
}

const KINDS: Array<[string, string, string]> = [
  ["scheduled", "Scheduled", "taken by the schedule"],
  ["manual", "Manual", "taken with Back up now"],
  ["safety", "Safety", "taken before restores, pushes and search & replace"],
]

export function CleanupDialog({
  open,
  onOpenChange,
  title,
  intro,
  confirm,
  destinations,
  onSubmit,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  title: string
  intro: string
  confirm: string // what to type to confirm
  destinations?: Array<{ id: string; name: string }> // offered as a choice (a site's cleanup)
  onSubmit: (body: CleanupBody) => Promise<unknown>
}) {
  const [kinds, setKinds] = useState<string[]>(KINDS.map(([k]) => k))
  const [repo, setRepo] = useState("")
  const [typed, setTyped] = useState("")
  const toggle = (k: string, on: boolean) => setKinds((ks) => (on ? [...ks, k] : ks.filter((x) => x !== k)))

  return (
    <FormDialog
      open={open}
      onOpenChange={(o) => {
        if (o) {
          setKinds(KINDS.map(([k]) => k))
          setRepo("")
          setTyped("")
        }
        onOpenChange(o)
      }}
      danger
      title={title}
      intro={intro}
      ok="Delete backups"
      okDisabled={!kinds.length || typed.trim() !== confirm}
      onSubmit={() => onSubmit({ repo_id: repo || undefined, kinds: kinds.length === KINDS.length ? [] : kinds })}
    >
      <div className="flex flex-col gap-4">
        {destinations && destinations.length > 1 && (
          <Field>
            <FieldLabel htmlFor="cleanup-repo">From</FieldLabel>
            <NativeSelect id="cleanup-repo" className="w-full" value={repo} onChange={(e) => setRepo(e.target.value)}>
              <NativeSelectOption value="">Every destination</NativeSelectOption>
              {destinations.map((d) => (
                <NativeSelectOption key={d.id} value={d.id}>
                  {d.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </Field>
        )}
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-2 text-sm font-medium">Which backups</legend>
          {KINDS.map(([k, label, sub]) => (
            <label key={k} className="flex cursor-pointer items-start gap-2.5 text-sm">
              <Checkbox className="mt-0.5" checked={kinds.includes(k)} onCheckedChange={(c) => toggle(k, !!c)} />
              <span>
                <span className="font-medium">{label}</span> <span className="text-muted-foreground">{sub}</span>
              </span>
            </label>
          ))}
        </fieldset>
        <Field>
          <FieldLabel htmlFor="cleanup-confirm">
            Type <code className="rounded bg-muted px-1 py-0.5 text-xs">{confirm}</code> to confirm
          </FieldLabel>
          <Input id="cleanup-confirm" autoComplete="off" spellCheck={false} value={typed} onChange={(e) => setTyped(e.target.value)} />
          <FieldDescription>Deleted backups can't be restored. Their space is given back right away.</FieldDescription>
        </Field>
      </div>
    </FormDialog>
  )
}
