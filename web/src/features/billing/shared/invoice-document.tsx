// CONTRACT (owner: billing-core agent). An invoice as a document (staff
// invoice screen and the client area), its payments, and a link to it.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type Invoice = { id: number; [k: string]: any }

export function InvoiceDocument(_props: { invoice: Invoice }) {
  return null
}
export function PaymentsTable(_props: { invoice: Invoice }) {
  return null
}
export function InvoiceLink({ invoice, children }: { invoice: Invoice; children?: React.ReactNode }) {
  return <a href={`#/billing/invoices/${invoice.id}`}>{children ?? (invoice.number || `Proforma #${invoice.id}`)}</a>
}
