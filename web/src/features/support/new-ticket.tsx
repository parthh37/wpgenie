import { useEffect, useRef, useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { LifeBuoyIcon, SendIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { ChoiceCard, EmptyState, LoadError } from "@/components/app/blocks"
import { Page, PageHeader } from "@/components/app/page"
import { notify, showError } from "@/components/app/toaster"
import { api } from "@/lib/api"
import { useSites } from "@/lib/query"
import { href, navigate, useHashQuery } from "@/lib/router"
import { useSession } from "@/lib/session"
import type { Site } from "@/lib/types"
import { AttachBar, DropOverlay, UploadProgress, sendForm, useAttachments } from "./attach"
import { BackLink, NO_LIMITS, PRIORITIES, afterChange, useSummary, type Department, type Limits, type Thread } from "./shared"

// #/support/new: a customer's new ticket, or staff's on a customer's
// behalf. #/support/new?site=<id> (from a site's page) is about that site.

interface AccountLite {
  id: number
  name: string
}

export function NewTicketPage() {
  const s = useSession()
  const staff = s.isStaff
  const fromSite = useHashQuery().get("site") || ""
  const summary = useSummary()
  const depts = useQuery({ queryKey: ["/support/departments"], queryFn: () => api<Department[]>("GET", "/support/departments") })
  const sites = useSites()
  const accounts = useQuery({
    queryKey: ["/accounts"],
    enabled: staff,
    queryFn: () => api<AccountLite[]>("GET", "/accounts").catch(() => [] as AccountLite[]),
  })
  const sum = summary.data

  const header = (
    <PageHeader
      icon={LifeBuoyIcon}
      tint="pink"
      title={staff ? "Open a ticket for a customer" : "New ticket"}
      description={
        staff ? "The customer gets an e-mail with your message and can reply from their panel." : "Tell us what's going on. You'll get an e-mail when we reply."
      }
    />
  )

  if (!staff && sum && !sum.enabled) {
    return (
      <Page>
        <BackLink />
        <EmptyState icon={LifeBuoyIcon} tint="pink" title="Tickets are turned off">
          New tickets can't be opened at the moment. Please contact your provider another way.
        </EmptyState>
      </Page>
    )
  }
  if (depts.isError) {
    return (
      <Page>
        <BackLink />
        {header}
        <LoadError error={depts.error} retry={() => depts.refetch()} />
      </Page>
    )
  }
  const ready = !summary.isPending && depts.data && !sites.isPending && (!staff || accounts.data)
  return (
    <Page className="max-w-[900px]">
      <BackLink />
      {header}
      {ready ? (
        <NewTicketForm
          staff={staff}
          depts={depts.data!}
          sites={sites.data ?? []}
          accounts={accounts.data ?? []}
          limits={sum?.limits ?? NO_LIMITS}
          fromSite={fromSite}
        />
      ) : (
        <Skeleton role="status" aria-label="Loading" className="h-[36rem] rounded-2xl" />
      )}
    </Page>
  )
}

function NewTicketForm({
  staff,
  depts,
  sites,
  accounts,
  limits,
  fromSite,
}: {
  staff: boolean
  depts: Department[]
  sites: Site[]
  accounts: AccountLite[]
  limits: Limits
  fromSite: string
}) {
  const choices = staff ? depts : depts.filter((d) => !d.hidden)
  // Staff opening a ticket from a site's page: it's that site's customer.
  const [account, setAccount] = useState(() => {
    const owner = staff && fromSite ? sites.find((x) => x.id === fromSite)?.account_id : undefined
    return owner ? String(owner) : ""
  })
  const siteChoices = (acct: string) => sites.filter((x) => !x.parent_id && (!staff || (acct && x.account_id === Number(acct))))
  const mine = siteChoices(account)
  // A ticket opened from a site's page is about that site.
  const [site, setSite] = useState(() => (siteChoices(account).some((x) => x.id === fromSite) ? fromSite : ""))
  const [dept, setDept] = useState(choices[0] ? String(choices[0].id) : "")
  const [priority, setPriority] = useState("medium")
  const [subject, setSubject] = useState("")
  const [body, setBody] = useState("")
  const [problem, setProblem] = useState("")
  const [progress, setProgress] = useState<number | null>(null)
  const [busy, setBusy] = useState(false)
  const att = useAttachments(limits)
  const accountRef = useRef<HTMLSelectElement>(null)
  const subjectRef = useRef<HTMLInputElement>(null)
  const bodyRef = useRef<HTMLTextAreaElement>(null)

  useEffect(() => {
    ;(staff ? accountRef : subjectRef).current?.focus()
  }, [staff])

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (busy) return
    const say = (msg: string, el?: HTMLElement | null) => {
      setProblem(msg)
      el?.focus()
    }
    setProblem("")
    if (staff && !account) return say("Choose the customer this ticket is for.", accountRef.current)
    if (!subject.trim()) return say("Add a short subject: what is it about?", subjectRef.current)
    if (!body.trim()) return say("Describe what you need help with.", bodyRef.current)
    setBusy(true)
    setProgress(att.files.length ? 0 : null)
    try {
      const th = await sendForm<Thread>(
        "/tickets",
        { account_id: staff ? Number(account) : 0, department_id: Number(dept || 0), site_id: site, subject, body, priority },
        att.files,
        (v) => setProgress(v)
      )
      notify(staff ? `Ticket ${th.ticket.mask} opened; the customer gets an e-mail` : `Ticket ${th.ticket.mask} sent. We'll e-mail you when we reply.`)
      if (th.warning) showError(new Error(th.warning))
      afterChange()
      navigate(`/support/${th.ticket.id}`)
    } catch (err) {
      say(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
      setProgress(null)
    }
  }

  return (
    <form noValidate onSubmit={submit} {...att.target} className="relative rounded-2xl bg-card p-5 card-shadow sm:p-6">
      <FieldGroup className="gap-6">
        {staff && (
          <Field>
            <FieldLabel htmlFor="tk-account">Customer</FieldLabel>
            <NativeSelect
              id="tk-account"
              ref={accountRef}
              required
              value={account}
              className="w-full"
              onChange={(e) => {
                const next = e.target.value
                setAccount(next)
                setProblem("")
                if (!siteChoices(next).some((x) => x.id === site)) setSite("")
              }}
            >
              <NativeSelectOption value="">Choose a customer…</NativeSelectOption>
              {accounts.map((a) => (
                <NativeSelectOption key={a.id} value={String(a.id)}>
                  {`${a.name} (#${a.id})`}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </Field>
        )}

        {choices.length > 0 && (
          <FieldSet>
            <FieldLegend variant="label">What is it about?</FieldLegend>
            <div role="radiogroup" aria-label="Department" className="grid gap-2.5 sm:grid-cols-2">
              {choices.map((d) => (
                <ChoiceCard key={d.id} name="department_id" value={String(d.id)} checked={dept === String(d.id)} onChange={setDept} title={d.name + (d.hidden ? " (hidden)" : "")}>
                  {d.description || undefined}
                </ChoiceCard>
              ))}
            </div>
          </FieldSet>
        )}

        <div className="grid gap-6 sm:grid-cols-2">
          <Field>
            <FieldLabel htmlFor="tk-subject">Subject</FieldLabel>
            <Input
              id="tk-subject"
              ref={subjectRef}
              required
              maxLength={200}
              autoComplete="off"
              placeholder="e.g. My contact form stopped sending e-mail"
              value={subject}
              onChange={(e) => {
                setSubject(e.target.value)
                setProblem("")
              }}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="tk-site">
              Related site <span className="font-normal text-muted-foreground">(optional)</span>
            </FieldLabel>
            <NativeSelect id="tk-site" value={site} onChange={(e) => setSite(e.target.value)} className="w-full">
              <NativeSelectOption value="">{mine.length ? "Not about one site" : "No sites to choose from"}</NativeSelectOption>
              {mine.map((x) => (
                <NativeSelectOption key={x.id} value={x.id}>
                  {x.primary_domain}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </Field>
        </div>

        <FieldSet>
          <FieldLegend variant="label">How urgent is it?</FieldLegend>
          <div role="radiogroup" aria-label="How urgent is it?" className="grid gap-2.5 sm:grid-cols-2 lg:grid-cols-4">
            {PRIORITIES.map(([k, l, hint]) => (
              <ChoiceCard key={k} name="priority" value={k} checked={priority === k} onChange={setPriority} title={l}>
                {hint}
              </ChoiceCard>
            ))}
          </div>
        </FieldSet>

        <Field>
          <FieldLabel htmlFor="tk-body">Message</FieldLabel>
          <Textarea
            id="tk-body"
            ref={bodyRef}
            required
            rows={7}
            maxLength={20000}
            className="min-h-36 resize-y"
            placeholder={staff ? "What should the customer know?" : "What happened? Include the page address, what you did and what you expected. Screenshots help."}
            value={body}
            onChange={(e) => {
              setBody(e.target.value)
              setProblem("")
            }}
          />
        </Field>

        <AttachBar att={att} disabled={busy} />
        {problem && (
          <p role="alert" className="text-sm font-medium text-danger">
            {problem}
          </p>
        )}
        <UploadProgress value={progress} />

        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="tinted" nativeButton={false} render={<a href={href("/support")} />}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy}>
            <SendIcon data-icon="inline-start" />
            {staff ? "Open ticket" : "Send to support"}
          </Button>
        </div>
      </FieldGroup>
      <DropOverlay show={att.dragging} />
    </form>
  )
}
