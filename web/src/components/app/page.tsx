import type { ReactNode } from "react"
import type { LucideIcon } from "lucide-react"
import { CheckIcon, TriangleAlertIcon } from "lucide-react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle, CardAction } from "@/components/ui/card"
import { IconTile, type Tint } from "@/components/app/icon-tile"
import { cn } from "@/lib/utils"

// A page: its header (icon on a lit tile, Large Title, a line under it,
// actions on the right) and its content.
export function Page({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("mx-auto w-full max-w-[1200px] px-4 pt-6 pb-28 sm:px-9 sm:pt-9", className)}>{children}</div>
}

export function PageHeader({
  icon,
  tint,
  title,
  description,
  actions,
}: {
  icon: LucideIcon
  tint: Tint
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
}) {
  return (
    <header className="mb-6 flex flex-wrap items-center gap-x-4 gap-y-3">
      <IconTile icon={icon} tint={tint} size="xl" className="max-sm:hidden" />
      <div className="min-w-0 flex-1">
        <h1 className="text-[2.125rem] leading-tight font-bold tracking-[-0.022em]">{title}</h1>
        {description && <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </header>
  )
}

// Section is a titled card: the panel's unit of content.
export function Section({
  icon,
  tint,
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
            <CardTitle className="flex items-center gap-2.5 text-[1.0625rem] font-semibold tracking-[-0.017em]">
              {icon && <IconTile icon={icon} tint={tint} />}
              {title}
            </CardTitle>
          )}
          {description && <CardDescription className={cn("max-w-[78ch]", icon && "sm:pl-[38px]")}>{description}</CardDescription>}
          {action && <CardAction>{action}</CardAction>}
        </CardHeader>
      )}
      {children != null && <CardContent className={contentClassName}>{children}</CardContent>}
    </Card>
  )
}

// StatusHero is a page's verdict at a glance ("All systems normal").
export function StatusHero({ kind, title, sub }: { kind: "ok" | "bad"; title: ReactNode; sub?: ReactNode }) {
  return (
    <div
      role="status"
      className={cn(
        "mb-4 flex items-center gap-3.5 rounded-2xl p-4",
        kind === "ok" ? "bg-success-fill/12" : "bg-danger-fill/12"
      )}
    >
      <span
        aria-hidden
        className={cn(
          "flex size-10 shrink-0 items-center justify-center rounded-full text-white",
          kind === "ok" ? "bg-success-fill" : "bg-danger-fill"
        )}
      >
        {kind === "ok" ? <CheckIcon className="size-5" strokeWidth={3} /> : <TriangleAlertIcon className="size-5" strokeWidth={2.5} />}
      </span>
      <div className="flex flex-col">
        <strong className="text-[1.0625rem] font-semibold">{title}</strong>
        {sub && <span className="text-sm text-muted-foreground">{sub}</span>}
      </div>
    </div>
  )
}
