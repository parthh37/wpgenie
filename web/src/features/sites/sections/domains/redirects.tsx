import { useId, useState, type FormEvent } from "react"
import { ArrowRightLeftIcon, PencilIcon, PlusIcon, Trash2Icon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ActionButton, BTable, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { invalidate, useApi } from "@/lib/query"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import type { RedirectMatch, SiteRedirect, SiteRedirects } from "../data/types"

// Redirects: visitors asking for an old path of the site go somewhere
// else (a new page, another site). The list is edited here and sent back
// whole; the server checks every rule and decides the order Caddy tries
// them in (exact paths first, then folders, longest first), and its Test
// says which rule answers a path, as Caddy will.

// Each status: its name in the list, in the dialog, and what it does.
const CODES: Array<[SiteRedirect["code"], string, string, string]> = [
  [301, "Permanent (301)", "Permanent (301)", "The page moved for good. Search engines move its ranking to the new address."],
  [302, "Temporary (302)", "Temporary (302)", "The page is elsewhere for now; search engines keep the old address."],
  [307, "Temporary (307)", "Temporary, keeps forms working (307)", "Like 302, and a form sent to the old address is sent again to the new one."],
  [308, "Permanent (308)", "Permanent, keeps forms working (308)", "Like 301, and a form sent to the old address is sent again to the new one."],
]
const codeName = (c: number) => CODES.find(([v]) => v === c)?.[1] ?? String(c)

const EMPTY: SiteRedirect = { from: "", to: "", code: 301, keep_query: false }

export function Redirects({ site }: { site: Site }) {
  const s = useSession()
  const path = `/sites/${site.id}/redirects`
  const { data, error, isPending, refetch } = useApi<SiteRedirects>(path)
  // The rule being edited: its index (-1: a new one).
  const [editing, setEditing] = useState<{ index: number; rule: SiteRedirect } | null>(null)
  const canChange = s.canChangeSite(site, "manager")
  const rules = data?.rules ?? []
  const max = data?.max ?? 100

  const save = async (next: SiteRedirect[]) => {
    await api("PUT", path, { rules: next })
    await invalidate(path)
  }
  const remove = async (i: number) => {
    const r = rules[i]
    if (!(await ask(`Delete the redirect from ${r.from}? Visitors asking for it get the site's own page again.`))) return
    await save(rules.filter((_, j) => j !== i))
    notify(`Redirect from ${r.from} deleted`)
  }

  let body
  if (error) body = <LoadError error={error} retry={() => refetch()} className="shadow-none" />
  else if (isPending) body = <Skeleton className="h-24 rounded-xl" />
  else
    body = (
      <div className="flex flex-col gap-5">
        <BTable
          caption={`Redirects of ${site.primary_domain}`}
          cols={[
            { label: "From", className: "w-[30%]" },
            { label: "To", className: "w-[34%]" },
            "Kind",
            ...(canChange ? [{ label: <span className="sr-only">Actions</span> }] : []),
          ]}
          rows={rules.map((r, i) => ({
            key: r.from,
            cells: [
              <code className="[overflow-wrap:anywhere]">{r.from}</code>,
              <span className="flex flex-wrap items-center gap-1.5">
                <code className="[overflow-wrap:anywhere]">{r.to}</code>
                {r.keep_query && <Badge variant="secondary">keeps ?…</Badge>}
              </span>,
              <span className="text-muted-foreground">{codeName(r.code)}</span>,
              ...(canChange
                ? [
                    <div className="flex justify-end gap-1.5">
                      <Button size="sm" variant="ghost" onClick={() => setEditing({ index: i, rule: r })}>
                        <PencilIcon data-icon="inline-start" />
                        Edit
                      </Button>
                      <ActionButton run={() => remove(i)} size="sm" variant="destructive">
                        <Trash2Icon data-icon="inline-start" />
                        Delete
                      </ActionButton>
                    </div>,
                  ]
                : []),
            ],
          }))}
          empty={<p className="py-2 text-sm text-muted-foreground">No redirects: every address is answered by the site itself.</p>}
        />
        <TestRedirect site={site} rules={rules} />
      </div>
    )

  return (
    <Section
      icon={ArrowRightLeftIcon}
      tint="purple"
      title="Redirects"
      description={
        rules.length
          ? `${rules.length} of ${max}. When several match, the most specific wins: an exact path before a folder, a deeper folder before its parent.`
          : "Send visitors from old addresses of the site to new pages, or to another site."
      }
      action={
        canChange &&
        !error &&
        !isPending && (
          <Button onClick={() => setEditing({ index: -1, rule: EMPTY })} disabled={rules.length >= max}>
            <PlusIcon data-icon="inline-start" />
            Add redirect
          </Button>
        )
      }
    >
      {body}
      {editing && (
        <RedirectDialog
          site={site}
          rule={editing.rule}
          isNew={editing.index < 0}
          onClose={() => setEditing(null)}
          onSave={async (r) => {
            const next = editing.index < 0 ? [...rules, r] : rules.map((x, j) => (j === editing.index ? r : x))
            await save(next)
            notify(editing.index < 0 ? `Redirect from ${r.from} added` : `Redirect from ${r.from} saved`)
          }}
        />
      )}
    </Section>
  )
}

function RedirectDialog({
  site,
  rule,
  isNew,
  onClose,
  onSave,
}: {
  site: Site
  rule: SiteRedirect
  isNew: boolean
  onClose: () => void
  onSave: (r: SiteRedirect) => Promise<void>
}) {
  const id = useId()
  const [r, setR] = useState(rule)
  const set = <K extends keyof SiteRedirect>(k: K, v: SiteRedirect[K]) => setR((x) => ({ ...x, [k]: v }))
  return (
    <FormDialog
      open
      onOpenChange={(o) => !o && onClose()}
      wide
      title={isNew ? "Add a redirect" : `Redirect from ${rule.from}`}
      intro={`Visitors asking for this path of ${site.primary_domain} are sent to the new address.`}
      ok={isNew ? "Add redirect" : "Save"}
      onSubmit={() => onSave({ ...r, from: r.from.trim(), to: r.to.trim() })}
    >
      <Field>
        <FieldLabel htmlFor={`${id}-from`}>From</FieldLabel>
        <Input
          id={`${id}-from`}
          name="from"
          required
          maxLength={512}
          autoComplete="off"
          spellCheck={false}
          className="font-mono"
          placeholder="/old-page"
          value={r.from}
          onChange={(e) => set("from", e.target.value)}
        />
        <FieldDescription>
          A path of this site, like <code>/old-page</code> (with or without the last /). End it in <code>/*</code> for a whole folder:{" "}
          <code>/blog/*</code> covers /blog and everything in it.
        </FieldDescription>
      </Field>
      <Field>
        <FieldLabel htmlFor={`${id}-to`}>To</FieldLabel>
        <Input
          id={`${id}-to`}
          name="to"
          required
          maxLength={2048}
          autoComplete="off"
          spellCheck={false}
          className="font-mono"
          placeholder="/new-page or https://example.com/page"
          value={r.to}
          onChange={(e) => set("to", e.target.value)}
        />
        <FieldDescription>A page of this site (starting with /) or a full address starting with https://.</FieldDescription>
      </Field>
      <Field>
        <FieldLabel htmlFor={`${id}-code`}>Kind</FieldLabel>
        <NativeSelect id={`${id}-code`} name="code" className="w-full" value={String(r.code)} onChange={(e) => set("code", Number(e.target.value) as SiteRedirect["code"])}>
          {CODES.map(([v, , label]) => (
            <NativeSelectOption key={v} value={String(v)}>
              {label}
            </NativeSelectOption>
          ))}
        </NativeSelect>
        <FieldDescription>{CODES.find(([v]) => v === r.code)?.[3]}</FieldDescription>
      </Field>
      <Label className="w-fit font-normal">
        <Checkbox checked={r.keep_query} onCheckedChange={(c) => set("keep_query", !!c)} />
        Pass on what follows ? in the visitor's address (like ?utm_source=…)
      </Label>
    </FormDialog>
  )
}

// TestRedirect asks the server which rule answers a path.
function TestRedirect({ site, rules }: { site: Site; rules: SiteRedirect[] }) {
  const id = useId()
  const [path, setPath] = useState("")
  const [result, setResult] = useState<RedirectMatch | null>(null)
  const [busy, setBusy] = useState(false)

  const test = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    setBusy(true)
    try {
      setResult(await api<RedirectMatch>("GET", `/sites/${site.id}/redirects/test?path=${encodeURIComponent(path.trim() || "/")}`))
    } catch (err) {
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  if (!rules.length) return null
  return (
    <form onSubmit={test} className="flex flex-col gap-2 rounded-2xl bg-muted/50 p-3.5">
      <div className="flex flex-wrap items-end gap-3">
        <Field className="w-auto min-w-64 flex-1">
          <FieldLabel htmlFor={`${id}-test`}>Try an address</FieldLabel>
          <Input
            id={`${id}-test`}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            placeholder={`/old-page or https://${site.primary_domain}/old-page?x=1`}
            value={path}
            onChange={(e) => {
              setPath(e.target.value)
              setResult(null)
            }}
          />
        </Field>
        <Button type="submit" variant="tinted" disabled={busy}>
          {busy ? "Testing…" : "Test"}
        </Button>
      </div>
      {result && (
        <p role="status" className="text-sm [overflow-wrap:anywhere]">
          {result.redirect ? (
            <>
              Rule {result.rule + 1} answers <code>{result.path}</code>: visitors go to <code>{result.location}</code> ({codeName(result.redirect.code).toLowerCase()}).
            </>
          ) : (
            <>
              No redirect matches <code>{result.path}</code>: the site answers it itself.
            </>
          )}
        </p>
      )}
    </form>
  )
}
