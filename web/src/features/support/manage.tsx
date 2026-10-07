import { useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { ChevronRightIcon, FolderTreeIcon, MessageSquareTextIcon, PlusIcon, SettingsIcon, SlidersHorizontalIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { ActionButton, BTable, FormDialog, LoadError } from "@/components/app/blocks"
import { ask } from "@/components/app/confirm"
import { Page, PageHeader, Section } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { invalidate } from "@/lib/query"
import { useSession } from "@/lib/session"
import { BackLink, type Canned, type Department, type Settings } from "./shared"

// #/support/manage (staff): canned replies (operators), departments and
// the help desk's settings (administrators).

export function ManagePage() {
  const s = useSession()
  const operator = s.atLeast("operator")
  const admin = s.isAdmin
  return (
    <Page>
      <BackLink />
      <PageHeader
        icon={SlidersHorizontalIcon}
        tint="pink"
        title="Manage support"
        description="Saved answers for your team, where tickets go, and how the help desk behaves."
      />
      {operator && <CannedSection />}
      {admin && <DepartmentsSection />}
      {admin && <SettingsSection />}
      {!operator && <p className="text-sm text-muted-foreground">Read-only: your role can look, not change support's set-up.</p>}
    </Page>
  )
}

// ---- Canned replies ----

function CannedSection() {
  const q = useQuery({ queryKey: ["/support/canned"], queryFn: () => api<Canned[]>("GET", "/support/canned") })
  const [editing, setEditing] = useState<Canned | "new" | null>(null)
  const current = editing === "new" ? null : editing
  return (
    <Section
      icon={MessageSquareTextIcon}
      tint="pink"
      title="Canned replies"
      description='Answers you give often. In a ticket, pick one from "Insert a canned reply" and edit it before sending.'
      action={
        <Button size="sm" onClick={() => setEditing("new")}>
          <PlusIcon data-icon="inline-start" />
          New canned reply
        </Button>
      }
    >
      {q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="shadow-none" />
      ) : !q.data ? (
        <Skeleton className="h-24 rounded-xl" />
      ) : q.data.length ? (
        <ul className="divide-y divide-border/60">
          {q.data.map((c) => (
            <li key={c.id} className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
              <div className="min-w-0">
                <strong className="font-semibold">{c.title}</strong>
                <p className="mt-0.5 text-sm whitespace-pre-wrap text-muted-foreground">{c.body.length > 160 ? c.body.slice(0, 160) + "…" : c.body}</p>
              </div>
              <div className="flex shrink-0 gap-1.5">
                <Button size="sm" variant="tinted" onClick={() => setEditing(c)}>
                  Edit
                </Button>
                <ActionButton
                  size="sm"
                  variant="destructive"
                  run={async () => {
                    if (!(await ask(`Delete the canned reply "${c.title}"?`))) return
                    await api("DELETE", `/support/canned/${c.id}`)
                    invalidate("/support/canned")
                  }}
                >
                  Delete
                </ActionButton>
              </div>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-muted-foreground">None yet.</p>
      )}

      <FormDialog
        open={editing != null}
        onOpenChange={(o) => !o && setEditing(null)}
        title={current ? "Edit canned reply" : "New canned reply"}
        ok={current ? "Save" : "Add"}
        onSubmit={async (data) => {
          const body = { title: String(data.get("title") ?? ""), body: String(data.get("body") ?? "") }
          await api(current ? "PUT" : "POST", current ? `/support/canned/${current.id}` : "/support/canned", body)
          invalidate("/support/canned")
        }}
      >
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor="canned-title">Title</FieldLabel>
            <Input id="canned-title" name="title" required maxLength={100} placeholder="e.g. Cleared the cache" defaultValue={current?.title ?? ""} autoFocus />
          </Field>
          <Field>
            <FieldLabel htmlFor="canned-body">Text</FieldLabel>
            <Textarea id="canned-body" name="body" required rows={8} className="min-h-36 resize-y" placeholder="Hi, thanks for reaching out…" defaultValue={current?.body ?? ""} />
          </Field>
        </FieldGroup>
      </FormDialog>
    </Section>
  )
}

// ---- Departments ----

function DepartmentsSection() {
  const q = useQuery({ queryKey: ["/support/departments"], queryFn: () => api<Department[]>("GET", "/support/departments") })
  const [editing, setEditing] = useState<Department | "new" | null>(null)
  const current = editing === "new" ? null : editing
  const [hidden, setHidden] = useState(false)
  const open = (d: Department | "new") => {
    setHidden(d !== "new" && !!d.hidden)
    setEditing(d)
  }
  return (
    <Section
      icon={FolderTreeIcon}
      tint="pink"
      title="Departments"
      description="Customers choose one when they open a ticket. New tickets of a department also go to its address."
      action={
        <Button size="sm" onClick={() => open("new")}>
          <PlusIcon data-icon="inline-start" />
          New department
        </Button>
      }
    >
      {q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="shadow-none" />
      ) : !q.data ? (
        <Skeleton className="h-24 rounded-xl" />
      ) : (
        <BTable
          caption="Departments"
          cols={["Name", "Also notify", { label: "Open tickets", num: true }, "Visible", ""]}
          rows={q.data.map((d) => ({
            key: d.id,
            cells: [
              <div>
                <strong className="font-semibold">{d.name}</strong>
                {d.description && <div className="text-sm text-muted-foreground">{d.description}</div>}
              </div>,
              d.notify_email || "–",
              String(d.active ?? 0),
              d.hidden ? "Hidden" : "Yes",
              <div className="flex justify-end gap-1.5">
                <Button size="sm" variant="tinted" onClick={() => open(d)}>
                  Edit
                </Button>
                <ActionButton
                  size="sm"
                  variant="destructive"
                  run={async () => {
                    if (!(await ask(`Delete the department ${d.name}? Departments with tickets can only be hidden.`))) return
                    await api("DELETE", `/support/departments/${d.id}`)
                    invalidate("/support/departments")
                  }}
                >
                  Delete
                </ActionButton>
              </div>,
            ],
          }))}
        />
      )}

      <FormDialog
        open={editing != null}
        onOpenChange={(o) => !o && setEditing(null)}
        title={current ? `Edit ${current.name}` : "New department"}
        ok={current ? "Save" : "Add"}
        onSubmit={async (data) => {
          const body = {
            name: String(data.get("name") ?? ""),
            description: String(data.get("description") ?? ""),
            notify_email: String(data.get("notify_email") ?? ""),
            sort: Number(data.get("sort") || 0),
            hidden,
          }
          await api(current ? "PUT" : "POST", current ? `/support/departments/${current.id}` : "/support/departments", body)
          invalidate("/support/departments")
        }}
      >
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor="dept-name">Name</FieldLabel>
            <Input id="dept-name" name="name" required maxLength={60} placeholder="e.g. Billing" defaultValue={current?.name ?? ""} autoFocus />
          </Field>
          <Field>
            <FieldLabel htmlFor="dept-description">
              Description <span className="font-normal text-muted-foreground">(customers see it when they choose)</span>
            </FieldLabel>
            <Input id="dept-description" name="description" maxLength={300} defaultValue={current?.description ?? ""} />
          </Field>
          <Field>
            <FieldLabel htmlFor="dept-notify">
              Also notify <span className="font-normal text-muted-foreground">(an e-mail address, optional)</span>
            </FieldLabel>
            <Input id="dept-notify" name="notify_email" type="email" defaultValue={current?.notify_email ?? ""} />
          </Field>
          <Field>
            <FieldLabel htmlFor="dept-sort">
              Order <span className="font-normal text-muted-foreground">(lower comes first)</span>
            </FieldLabel>
            <Input id="dept-sort" name="sort" type="number" defaultValue={String(current?.sort ?? 0)} className="w-32" />
          </Field>
          <Label className="font-normal">
            <Checkbox checked={hidden} onCheckedChange={(c) => setHidden(!!c)} />
            Hidden from customers (staff can still move tickets here)
          </Label>
        </FieldGroup>
      </FormDialog>
    </Section>
  )
}

// ---- Settings ----

function SettingsSection() {
  const q = useQuery({ queryKey: ["/support/settings"], queryFn: () => api<Settings>("GET", "/support/settings") })
  return (
    <Section
      icon={SettingsIcon}
      tint="gray"
      title="Settings"
      description="Customers get e-mail at their account's address; replies to that e-mail aren't read yet, so they answer from the panel. Tickets of a reseller's customers go to the reseller until they escalate them."
    >
      {q.isError ? (
        <LoadError error={q.error} retry={() => q.refetch()} className="shadow-none" />
      ) : !q.data ? (
        <Skeleton className="h-40 rounded-xl" />
      ) : (
        <SettingsForm settings={q.data} />
      )}
    </Section>
  )
}

const list = (v: string) =>
  v
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean)

function SettingsForm({ settings }: { settings: Settings }) {
  const [enabled, setEnabled] = useState(settings.enabled)
  const [busy, setBusy] = useState(false)
  const [advanced, setAdvanced] = useState(false)

  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const f = new FormData(e.currentTarget)
    const str = (k: string) => String(f.get(k) ?? "")
    setBusy(true)
    try {
      await api("PUT", "/support/settings", {
        enabled,
        notify_emails: list(str("notify_emails")),
        auto_close_days: Number(str("auto_close_days") || 0),
        max_files: Number(str("max_files") || 0),
        max_file_mb: Number(str("max_file_mb") || 1),
        extensions: list(str("extensions")),
        reply_to: str("reply_to").trim(),
      })
      notify("Support settings saved")
      invalidate("/support/settings")
      invalidate("/support/summary")
    } catch (err) {
      // The fields stay as typed; the error says what to fix.
      showError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    // A field in the closed Advanced part that the browser refuses opens it.
    <form onSubmit={save} onInvalidCapture={() => setAdvanced(true)}>
      <FieldGroup className="gap-5">
        <Field orientation="horizontal">
          <Switch id="sup-enabled" checked={enabled} onCheckedChange={setEnabled} />
          <FieldLabel htmlFor="sup-enabled" className="font-normal">
            Customers can open new tickets
          </FieldLabel>
        </Field>
        <div className="grid gap-5 md:grid-cols-2">
          <Field>
            <FieldLabel htmlFor="sup-notify">
              Send new tickets and replies to <span className="font-normal text-muted-foreground">(comma-separated)</span>
            </FieldLabel>
            <Input id="sup-notify" name="notify_emails" defaultValue={(settings.notify_emails ?? []).join(", ")} placeholder="support@yourcompany.com" />
          </Field>
          <Field>
            <FieldLabel htmlFor="sup-close">
              Close answered tickets after <span className="font-normal text-muted-foreground">(days without a reply; 0 = never)</span>
            </FieldLabel>
            <Input id="sup-close" name="auto_close_days" type="number" min={0} max={365} defaultValue={String(settings.auto_close_days)} />
          </Field>
        </div>
        <Collapsible open={advanced} onOpenChange={setAdvanced}>
          <CollapsibleTrigger className="group flex items-center gap-1.5 text-sm font-medium text-link">
            <ChevronRightIcon className="size-4 transition-transform group-data-[panel-open]:rotate-90" aria-hidden />
            Advanced: attachments and e-mail
          </CollapsibleTrigger>
          {/* Kept in the form while closed: its fields are saved too. */}
          <CollapsibleContent keepMounted className="data-[closed]:hidden">
            <div className="mt-4 grid gap-5 md:grid-cols-2">
              <Field>
                <FieldLabel htmlFor="sup-files">
                  Files per message <span className="font-normal text-muted-foreground">(0 = no attachments)</span>
                </FieldLabel>
                <Input id="sup-files" name="max_files" type="number" min={0} max={10} defaultValue={String(settings.max_files)} />
              </Field>
              <Field>
                <FieldLabel htmlFor="sup-mb">Largest file (MB)</FieldLabel>
                <Input id="sup-mb" name="max_file_mb" type="number" min={1} max={25} defaultValue={String(settings.max_file_mb)} />
              </Field>
              <Field>
                <FieldLabel htmlFor="sup-ext">Allowed file types</FieldLabel>
                <Input id="sup-ext" name="extensions" defaultValue={(settings.extensions ?? []).join(", ")} />
              </Field>
              <Field>
                <FieldLabel htmlFor="sup-replyto">
                  Reply-To address <span className="font-normal text-muted-foreground">(optional)</span>
                </FieldLabel>
                <Input id="sup-replyto" name="reply_to" type="email" defaultValue={settings.reply_to || ""} />
              </Field>
            </div>
          </CollapsibleContent>
        </Collapsible>
        <div>
          <Button type="submit" disabled={busy}>
            Save settings
          </Button>
        </div>
      </FieldGroup>
    </form>
  )
}
