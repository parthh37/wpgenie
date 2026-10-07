// The API's billing shapes (internal/billing, internal/store), as the
// staff screens and the client area read them. Amounts are integer minor
// units; dates are RFC 3339 strings.

export interface TaxLine {
  name: string
  rate: number // hundredths of a percent
  amount: number
}

export interface InvoiceItem {
  id?: number
  kind?: string // plan, setup, custom, discount, credit, …
  description: string
  quantity?: number
  unit_price?: number
  amount?: number
  taxable?: boolean
}

export interface BillingAddress {
  name?: string
  company?: string
  lines?: string[]
  country?: string
  tax_id?: string
  email?: string
}

export interface Payment {
  id: number
  invoice_id?: number
  invoice_number?: string
  account_id?: number
  account_name?: string
  gateway?: string
  method?: string
  reference?: string
  amount: number
  fee?: number
  refunded?: number
  credited?: number
  note?: string
  at: string
}

// An invoice as the list (InvoiceView) or one invoice (InvoiceDetail) has it.
export interface Invoice {
  id: number
  number?: string
  account_id?: number
  account_name?: string
  kind?: string
  status?: string
  overdue?: boolean
  currency?: string
  issued_at?: string | null
  due_at?: string | null
  paid_at?: string | null
  period_start?: string | null
  period_end?: string | null
  subtotal?: number
  discount?: number
  tax_lines?: TaxLine[]
  tax?: number
  total?: number
  credit_applied?: number
  amount_paid?: number
  amount_refunded?: number
  balance?: number
  notes?: string
  pay_methods?: string[]
  items?: InvoiceItem[]
  billing_address?: BillingAddress
  payments?: Payment[]
  // Anything else the API adds.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  [k: string]: any
}

export interface Contact {
  first_name?: string
  last_name?: string
  company?: string
  email?: string
  phone?: string
  address1?: string
  address2?: string
  city?: string
  state?: string
  postcode?: string
  country?: string
  tax_id?: string
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  [k: string]: any
}

export interface BillingProfile {
  mode: string // none, invoice, stripe_subscription, whmcs
  cycle: string
  price_override: number | null
  next_due_at: string | null
  anchor_day?: number
  auto_pay: boolean
  card: { brand?: string; last4: string; exp_month: number; exp_year: number } | null
  credit: number
  tax_exempt: boolean
  tax_exempt_by_tax_id?: boolean
  cancel_at: string | null
  cancel_reason?: string
  contact: Contact
  balance_due: number
  overdue: boolean
  recurring_amount: number | null
  upcoming?: { period_start: string; period_end: string; amount: number } | null
}

export interface CreditEntry {
  id: number
  amount: number
  balance: number
  description: string
  invoice_id?: number
  at: string
  by?: string
}

export interface Plan {
  id: string
  name: string
  description?: string
  public?: boolean
  max_sites?: number
  disk_mb?: number
  bandwidth_gb?: number
  prices?: Record<string, { price: number; setup_fee?: number }>
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  [k: string]: any
}

// A price quote (a plan change, an order): its lines and totals.
export interface Quote {
  items?: InvoiceItem[]
  credit?: number
  charge?: number
  subtotal?: number
  discount?: number
  tax_lines?: TaxLine[]
  tax?: number
  total?: number
  effective?: string
  new_next_due_at?: string
}

export interface PlanChangeResult {
  invoice?: Invoice
  applied?: boolean
}

export interface Order {
  id: number
  account_id: number
  account_name?: string
  account_status?: string
  email?: string
  plan_id: string
  plan_name?: string
  cycle: string
  promo_code?: string
  total: number
  invoice_id?: number
  invoice_number?: string
  invoice_status?: string
  ip?: string
  status: string
  created_at: string
}

export interface Overview {
  currency?: { code?: string; symbol?: string; decimals?: number }
  mrr?: number
  income_this_month?: number
  income_last_month?: number
  outstanding?: number
  overdue_total?: number
  overdue_count?: number
  unpaid_count?: number
  billed_accounts?: number
  pending_orders?: number
  credit_total?: number
  income_by_month?: Array<{ month: string; amount: number }>
  recent_payments?: Payment[]
  overdue_invoices?: Invoice[]
  upcoming_renewals?: Array<{
    account_id: number
    account_name?: string
    plan_id?: string
    plan_name?: string
    cycle?: string
    next_due_at?: string
    due_at?: string
    amount?: number
    recurring_amount?: number
  }>
}

// A message sent to an account (GET /accounts/{id}/emails).
export interface SentEmail {
  id: number
  template?: string
  subject: string
  sent_at?: string | null
  created_at?: string
  status?: string
}
