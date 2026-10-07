import type { ReactNode } from "react"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"

// SimpleTable: headers and rows of cells, the legacy panel's table(). For
// sortable or selectable tables, compose shadcn's Table directly.
export function SimpleTable({
  headers,
  rows,
  empty = "Nothing here yet.",
  className,
  rowKey,
}: {
  headers: ReactNode[]
  rows: ReactNode[][]
  empty?: ReactNode
  className?: string
  rowKey?: (i: number) => string | number
}) {
  if (!rows.length) return <p className="py-2 text-sm text-muted-foreground">{empty}</p>
  return (
    <Table className={className}>
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          {headers.map((h, i) => (
            <TableHead key={i}>{h}</TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((r, i) => (
          <TableRow key={rowKey ? rowKey(i) : i}>
            {r.map((c, j) => (
              <TableCell key={j} className={cn("align-top whitespace-normal [overflow-wrap:anywhere]", j === 0 && "font-medium")}>
                {c}
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

// KeyValues: a two-column list of facts (the legacy key/value tables).
export function KeyValues({ items, className }: { items: Array<[ReactNode, ReactNode] | null | false>; className?: string }) {
  return (
    <dl className={cn("grid grid-cols-[minmax(8rem,12rem)_1fr] gap-x-4 text-sm", className)}>
      {items.filter(Boolean).map((it, i) => {
        const [k, v] = it as [ReactNode, ReactNode]
        return (
          <div key={i} className="contents">
            <dt className="border-b border-border/60 py-2 text-muted-foreground last-of-type:border-0">{k}</dt>
            <dd className="border-b border-border/60 py-2 [overflow-wrap:anywhere] last-of-type:border-0">{v}</dd>
          </div>
        )
      })}
    </dl>
  )
}
