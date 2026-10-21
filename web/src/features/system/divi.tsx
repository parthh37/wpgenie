import { useState, type FormEvent } from "react"
import { BadgeCheckIcon, LayoutTemplateIcon, Trash2Icon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { SwitchRow } from "@/features/sites/sections/speed/shared"
import { api } from "@/lib/api"
import { DIVI, useDivi, type DiviView } from "@/lib/divi"
import { queryClient } from "@/lib/query"

// The Divi license (administrators): the Elegant Themes username and API
// key new sites get Divi with. The key is write-only: the panel only ever
// learns whether one is saved and how it ends.

export function DiviCard() {
  const q = useDivi()
  return (
    <Section
      icon={LayoutTemplateIcon}
      tint="purple"
      title="Divi"
      description="Install the Divi theme from Elegant Themes on new sites, with your license. Every site with Divi gets updates and premade layouts without anyone seeing the key: it isn't saved in the site's database or shown in Divi's settings."
    >
      {q.isLoading && <Skeleton className="h-40 rounded-lg" />}
      {q.error && !q.data && <LoadError error={q.error} retry={() => q.refetch()} />}
      {/* Fresh fields whenever the saved license changes. */}
      {q.data && <DiviForm key={JSON.stringify(q.data)} d={q.data} />}
    </Section>
  )
}

const save = (body: Record<string, unknown>) => api<DiviView>("PUT", DIVI, body).then((v) => (queryClient.setQueryData([DIVI], v), v))

async function check(quiet = false) {
  try {
    const r = await api<{ message: string }>("POST", `${DIVI}/check`)
    notify(r.message)
    return true
  } catch (e) {
    showError(quiet ? new Error(`Saved, but Elegant Themes didn't accept it: ${e instanceof Error ? e.message : e}`) : e)
    return false
  }
}

function DiviForm({ d }: { d: DiviView }) {
  const [username, setUsername] = useState(d.username ?? "")
  const [key, setKey] = useState("")
  const [busy, setBusy] = useState(false)
  const ending = d.key_hint?.replace("…", "")

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      const body: Record<string, string> = { username: username.trim() }
      // Empty: keep the saved key.
      if (key.trim()) body.api_key = key.trim()
      const v = await save(body)
      setKey("")
      if (!v.configured) return
      notify(d.configured ? "Divi license saved: sites with Divi use it now" : "Divi license saved")
      // A new key: see straight away whether Elegant Themes takes it.
      if (body.api_key) await check(true)
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  async function remove() {
    if (
      !(await ask(
        "Remove the Divi license? Sites keep the Divi theme, but lose updates and premade layouts, and new sites no longer get it. Revoke the API key in your Elegant Themes account too if you no longer use it.",
        { ok: "Remove license" }
      ))
    )
      return
    await save({ username: "", api_key: "" })
    notify("Divi license removed")
  }

  async function setNewSites(on: boolean) {
    try {
      await save({ new_sites: on })
      notify(on ? "New sites get Divi" : "New sites no longer get Divi")
    } catch (e) {
      showError(e)
    }
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-5">
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="divi-user">Elegant Themes username</FieldLabel>
          <Input
            id="divi-user"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
            maxLength={100}
            pattern="[A-Za-z0-9@._+\-]+"
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        <Field>
          <FieldLabel htmlFor="divi-key">API key</FieldLabel>
          <Input
            id="divi-key"
            type="password"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            required={!d.key_set}
            maxLength={128}
            pattern="[A-Za-z0-9]+"
            autoComplete="new-password"
            spellCheck={false}
            placeholder={d.key_set ? (ending ? `Saved, ends in ${ending}` : "Saved") : undefined}
          />
          <FieldDescription>{d.key_set ? "Leave empty to keep the saved key. It's never shown again." : "It's never shown again once saved."}</FieldDescription>
        </Field>
      </FieldGroup>

      <p className="max-w-[80ch] text-sm text-muted-foreground">
        Create an API key just for this panel in your Elegant Themes account (Account → Username &amp; API Key), so you can revoke it on its own.
        The key stays out of sight, but someone who can add their own code to a site (a plugin, for example) could still read it.
      </p>

      {d.configured && (
        <SwitchRow checked={d.new_sites} onChange={setNewSites} title="Install Divi on new sites">
          New sites get Divi installed and switched on. It can be skipped for one site when it's created.
        </SwitchRow>
      )}

      <div className="flex flex-wrap justify-end gap-2">
        {d.configured && (
          <>
            <ActionButton variant="destructive" run={remove}>
              <Trash2Icon data-icon="inline-start" />
              Remove license
            </ActionButton>
            <ActionButton run={() => check()}>
              <BadgeCheckIcon data-icon="inline-start" />
              Check license
            </ActionButton>
          </>
        )}
        <Button type="submit" disabled={busy}>
          Save license
        </Button>
      </div>
    </form>
  )
}
