import type { ReactNode } from "react"
import type { LucideIcon } from "lucide-react"
import { CircleCheckIcon, TriangleAlertIcon } from "lucide-react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle, CardAction } from "@/components/ui/card"
import type { Tint } from "@/components/app/icon-tile"
import { cn } from "@/lib/utils"

// A page: its header (title, a line under it, actions on the right, as in
// Cloudflare's dashboard) and its content.
export function Page({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("mx-auto w-full max-w-[1200px] px-4 pt-6 pb-24 sm:px-8 sm:pt-8", className)}>{children}</div>
}

// icon and tint are accepted for the callers' sake; the top bar and the
// sidebar already say where you are, so the header shows words only.
export function PageHeader({
  title,
  description,
  actions,
}: {
  icon?: LucideIcon
  tint?: Tint
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
}) {
  return (
    <header className="mb-6 flex flex-wrap items-end gap-x-4 gap-y-3">
      <div className="min-w-0 flex-[1_1_16rem]">
        <h1 className="text-2xl leading-tight font-semibold tracking-[-0.01em]">{title}</h1>
        {description && <p className="mt-1 max-w-[80ch] text-base text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </header>
  )
}

// Section is a titled card: the panel's unit of content.
export function Section({
  icon: Icon,
  title,
  description,
  action,
  children,
  className,
  contentClassName,
}: {
  icon?: LucideIcon
  tint?: Tint
  title?: ReactNode
  description?: ReactNode
  action?: ReactNode
  children?: ReactNode
  className?: string
  contentClassName?: string
}) {
  return (
    <Card className={cn("mb-4", className)}>
      {(title || description || action) && (
        <CardHeader>
          {title && (
            <CardTitle className="flex items-center gap-2">
              {Icon && <Icon aria-hidden className="size-4 shrink-0 text-muted-foreground" />}
              {title}
            </CardTitle>
          )}
          {description && <CardDescription className="max-w-[78ch]">{description}</CardDescription>}
          {action && <CardAction>{action}</CardAction>}
        </CardHeader>
      )}
      {children != null && <CardContent className={contentClassName}>{children}</CardContent>}
    </Card>
  )
}

// StatusHero is a page's verdict at a glance ("All systems normal").
export function StatusHero({ kind, title, sub }: { kind: "ok" | "bad"; title: ReactNode; sub?: ReactNode }) {
  const Icon = kind === "ok" ? CircleCheckIcon : TriangleAlertIcon
  return (
    <div
      role="status"
      className={cn(
        "mb-4 flex items-start gap-3 rounded-lg bg-card px-4 py-3.5 card-shadow",
        "border-l-[3px]",
        kind === "ok" ? "border-l-success-fill" : "border-l-danger-fill"
      )}
    >
      <Icon aria-hidden className={cn("mt-0.5 size-5 shrink-0", kind === "ok" ? "text-success" : "text-danger")} />
      <div className="flex min-w-0 flex-col gap-0.5">
        <strong className="text-lg leading-snug font-semibold">{title}</strong>
        {sub && <span className="text-sm text-muted-foreground">{sub}</span>}
      </div>
    </div>
  )
}
