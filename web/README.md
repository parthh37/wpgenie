# WPGenie panel (React)

The control panel, rebuilt on [shadcn/ui](https://ui.shadcn.com) (Base UI
primitives, style "maia") with Tailwind v4, themed to Apple's Human Interface
Guidelines: the system font, grouped backgrounds, systemIndigo as the one
tint, Liquid Glass surfaces over a soft wash of colour, Settings-style icon
tiles, vital gauges and Dynamic Island toasts.

It replaces the vanilla-JS dashboard in `internal/web/static` screen by screen.
Until it has parity it is served at **`/next/`**; the classic panel stays at
`/`. Both use the same addresses (`#/sites/<id>/<section>`, `#/billing/...`),
so links move across unchanged.

```sh
npm ci
npm run dev      # Vite on :5173, proxying /api to WPGENIE_API (default 127.0.0.1:8080)
npm run build    # typecheck, then build into ../internal/web/static/next (embedded by Go)
npm run lint
```

The build output is committed, so `go build` never needs Node. CI rebuilds
it and fails if the committed copy is stale.

## Rules

- **CSP is `default-src 'self'`.** No inline `<script>` or `<style>` elements,
  no `style="..."` in markup, no web fonts or `data:` URIs, no third-party
  hosts. React's `style` prop is fine (it's CSSOM). `<CSPProvider
  disableStyleElements>` stops Base UI from rendering its one `<style>`; its
  rule is in `index.css`. Don't add libraries that inject styles (sonner,
  input-otp, Radix's react-remove-scroll); check before adding any.
- **Light and dark parity.** Use the semantic tokens (`bg-card`,
  `text-muted-foreground`, `text-success`/`text-danger`/`text-warning`,
  `bg-success-fill/15`, `text-link`), never raw colours.
  `prefers-reduced-motion` is honoured globally.
- **Glass.** Every card surface is `bg-card card-shadow` (frosted, with a lit
  edge); menus and dialogs add `material`. Don't put an opaque background on
  the page or the shell, or the glass has nothing to show. Reduce
  Transparency (`prefers-reduced-transparency`) swaps in solid surfaces.
- **Words.** The panel says "Protection" (on/off), never "Shield". Customers
  are non-technical: plain words, sentences that say what happens.
- **The server enforces everything.** Hide what a role can't do, but never
  rely on it.

## Layout

```
src/
  app/            shell (sidebar, routing to pages), command palette, job tray, page registry
  components/ui/  shadcn components (generated: `npx shadcn@latest add <name>`; then
                  change `from "cn"` to `from "@/lib/utils"`)
  components/app/ the panel's own building blocks (see below)
  features/<page>/index.tsx   one module per page, lazily loaded
  features/sites/sections/<key>.tsx   one module per site section, lazily loaded
  lib/            api client, query hooks, session, router, jobs, formatting, money
```

## Building blocks

| Need | Use |
| --- | --- |
| Call the API | `api(method, path, body)` from `@/lib/api` (throws `ApiError` with `.status`, `.data`, `.code`); `upload(path, formData, onProgress)` for files |
| Read data | `useApi<T>(path)` from `@/lib/query` (TanStack Query, key = path); `invalidate("/sites")` after a change refetches every key starting with it |
| Sites | `useSites()`, `useSite(id)`, `useClustered()`, `useNodes()` |
| Who's signed in | `useSession()`: `me`, `isAdmin`, `isTenant`, `isStaff`, `isReseller`, `canChange` (not a viewer), `canCreate`, `atLeast("operator")` |
| Addresses | `navigate("/sites/x/files")`, `useRoute()` (decoded parts), `useHashQuery()`, `href(path)` for `<a href>`, `sitePath(id, section)` |
| Long operations (202 `{job_id}`) | `startJob(method, path, body, onDone)` or `followJob(id, onDone)` from `@/lib/jobs`; the tray shows progress |
| Messages | `notify("Saved")`, `showError(err)` from `@/components/app/toaster` |
| Confirm / prompt | `await ask("Delete x? Explanation.")` (title from the question, verb on the button, red when destructive); `await askText(msg, {title, label, match, type})` |
| Shown once | `showSecret(title, lines)` from `@/components/app/secret` |
| Page | `<Page><PageHeader icon={…} tint="…" title description actions/>…</Page>` from `@/components/app/page` |
| Card with a title | `<Section icon tint title description action>` (same file) |
| Verdict | `<StatusHero kind="ok" title sub/>` |
| Icon tile | `<IconTile icon={ShieldIcon} tint="green" size="md"/>` |
| Status words | `<StatusText status="failed"/>`, `<StatusPill status="active"/>`, `<Severity level/>`, `toneOf(status)` |
| Tables | `<SimpleTable headers rows/>`, `<KeyValues items/>` from `@/components/app/data-table`; shadcn `Table` for anything richer |
| Charts | `<LineChart series label readout/>`, `hourly()`, `hoursSince()` from `@/components/app/chart`; `<VitalBars values/>` (labelled gauges for a card), `<VitalMeter values/>` (compact, for a row) from `@/components/app/vitals` |
| Formatting | `fmtBytes`, `fmtNum`, `fmtTime`, `fmtDate`, `fmtAgo`, `fmtMem`, `fmtPct`, `plural`, `splitList`, `humanize` from `@/lib/format` |
| Number cards, notices, sub-tabs, filters | `Kpi`, `Banner`, `SubNav`, `Chips` from `@/components/app/blocks` |
| Empty / failed / busy | `EmptyState`, `LoadError` (404 = "not on this server yet"), `ActionButton` (disabled while running, errors to toasts) |
| Forms in dialogs | `FormDialog` (errors inside, next to the field the API names), `MoneyInput`, `PercentInput`, `ChoiceCard`, `CopyField`, `BTable` |
| Money | `money(minor)`, `toMinor`, `fromMinor`, `fmtRate`, `cycleOf`, `methodName`, `invoiceState`, `dueLine`… from `@/lib/money`, after `useBillingConfig()` has loaded |

Forms: shadcn `Field`, `FieldLabel`, `FieldDescription`, `FieldGroup`,
`Input`, `NativeSelect` (prefer it over `Select` for plain choices),
`Switch`, `Checkbox`, `RadioGroup`, `Textarea`. Buttons: `default` (filled
tint), `tinted` (gray fill, tint text: the usual secondary), `ghost`,
`outline`, `destructive` (tinted red), `destructive-solid`, `link`. Base UI
composes with `render={<a href=… />}` (plus `nativeButton={false}` on
`Button`) where Radix used `asChild`.
