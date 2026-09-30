package api

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/store"
)

// The printable invoice: a page of its own (not the dashboard), styled by
// /invoice.css (print-first: A4/Letter, no navigation) with semantic
// classes, no inline style or script (the panel's CSP). The Print button
// is wired by /invoice-print.js; without it, the browser's Print works
// the same.

var invoicePage = template.Must(template.New("invoice").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/invoice.css">
<script src="/invoice-print.js" defer></script>
</head>
<body>
<main class="invoice status-{{.Status}}">
<div class="toolbar"><button type="button" class="print-btn" data-print>Print / Save as PDF</button></div>
<header class="invoice-head">
<div class="company">
{{if .LogoURL}}<img class="logo" src="{{.LogoURL}}" alt="{{.Company.Name}}">{{end}}
<h1>{{.Company.Name}}</h1>
{{range .CompanyLines}}<div>{{.}}</div>{{end}}
{{if .Company.Email}}<div class="email">{{.Company.Email}}</div>{{end}}
{{if .Company.Phone}}<div class="phone">{{.Company.Phone}}</div>{{end}}
{{if .Company.Website}}<div class="website">{{.Company.Website}}</div>{{end}}
{{if .Company.TaxID}}<div class="tax-id">Tax ID: {{.Company.TaxID}}</div>{{end}}
</div>
<div class="meta">
<h2>{{.Heading}}</h2>
<dl>
<dt>Number</dt><dd class="number">{{.Number}}</dd>
{{if .Issued}}<dt>Issued</dt><dd>{{.Issued}}</dd>{{end}}
{{if .Due}}<dt>Due</dt><dd>{{.Due}}</dd>{{end}}
{{if .PaidOn}}<dt>Paid</dt><dd>{{.PaidOn}}</dd>{{end}}
{{if .Period}}<dt>Period</dt><dd>{{.Period}}</dd>{{end}}
<dt>Status</dt><dd class="status">{{.StatusLabel}}</dd>
</dl>
</div>
</header>
{{if .Stamp}}<div class="stamp {{.Stamp}}" aria-hidden="true">{{.StampLabel}}</div>{{end}}
<section class="bill-to">
<h3>Bill to</h3>
<div class="name">{{.BillTo.Name}}</div>
{{if .BillTo.Company}}<div class="company-name">{{.BillTo.Company}}</div>{{end}}
{{range .BillTo.Lines}}<div>{{.}}</div>{{end}}
{{if .BillTo.Country}}<div class="country">{{.BillTo.Country}}</div>{{end}}
{{if .BillTo.Email}}<div class="email">{{.BillTo.Email}}</div>{{end}}
{{if .BillTo.TaxID}}<div class="tax-id">Tax ID: {{.BillTo.TaxID}}</div>{{end}}
</section>
<table class="items">
<thead><tr><th scope="col" class="desc">Description</th><th scope="col" class="qty">Qty</th><th scope="col" class="price">Unit price</th><th scope="col" class="amount">Amount</th></tr></thead>
<tbody>
{{range .Items}}<tr class="item-{{.Kind}}"><td class="desc">{{.Description}}</td><td class="qty">{{.Quantity}}</td><td class="price">{{.UnitPrice}}</td><td class="amount">{{.Amount}}</td></tr>
{{end}}</tbody>
</table>
<table class="totals">
<tbody>
<tr class="subtotal"><th scope="row">Subtotal</th><td>{{.Subtotal}}</td></tr>
{{if .Discount}}<tr class="discount"><th scope="row">{{or .DiscountLabel "Discount"}}</th><td>-{{.Discount}}</td></tr>{{end}}
{{range .TaxLines}}<tr class="tax"><th scope="row">{{.Name}} ({{.Rate}}){{if $.Inclusive}}, included{{end}}</th><td>{{.Amount}}</td></tr>
{{end}}<tr class="total"><th scope="row">Total</th><td>{{.Total}}</td></tr>
{{if .CreditApplied}}<tr class="credit"><th scope="row">Credit applied</th><td>-{{.CreditApplied}}</td></tr>{{end}}
{{if .AmountPaid}}<tr class="paid"><th scope="row">Paid</th><td>-{{.AmountPaid}}</td></tr>{{end}}
{{if .AmountRefunded}}<tr class="refunded"><th scope="row">Refunded</th><td>{{.AmountRefunded}}</td></tr>{{end}}
{{if .Balance}}<tr class="balance"><th scope="row">Balance due</th><td>{{.Balance}}</td></tr>{{end}}
</tbody>
</table>
{{if .Payments}}<section class="payments">
<h3>Payments</h3>
<table>
<thead><tr><th scope="col">Date</th><th scope="col">Method</th><th scope="col">Reference</th><th scope="col" class="amount">Amount</th></tr></thead>
<tbody>
{{range .Payments}}<tr><td>{{.Date}}</td><td>{{.Method}}</td><td>{{.Reference}}</td><td class="amount">{{.Amount}}{{if .Refunded}} ({{.Refunded}} refunded){{end}}</td></tr>
{{end}}</tbody>
</table>
</section>{{end}}
{{if .Notes}}<section class="notes"><h3>Notes</h3>{{range .Notes}}<p>{{.}}</p>{{end}}</section>{{end}}
{{if .Instructions}}<section class="notes instructions"><h3>How to pay</h3>{{range .Instructions}}<p>{{.}}</p>{{end}}</section>{{end}}
<footer class="footer">{{range .Footer}}<p>{{.}}</p>{{end}}{{if .TermsURL}}<p class="terms">Terms: {{.TermsURL}}</p>{{end}}</footer>
</main>
</body>
</html>
`))

type printItem struct {
	Kind, Description, UnitPrice, Amount string
	Quantity                             int64
}

type printTax struct{ Name, Rate, Amount string }

type printPayment struct{ Date, Method, Reference, Amount, Refunded string }

type printData struct {
	Title, Heading, Number, Status, StatusLabel string
	Stamp, StampLabel                           string
	Company                                     billing.Company
	CompanyLines                                []string
	LogoURL                                     string
	Issued, Due, PaidOn, Period                 string
	BillTo                                      store.BillingAddress
	Items                                       []printItem
	Subtotal, Discount, Total                   string
	DiscountLabel                               string
	TaxLines                                    []printTax
	Inclusive                                   bool
	CreditApplied, AmountPaid, AmountRefunded   string
	Balance                                     string
	Payments                                    []printPayment
	Notes, Instructions, Footer                 []string
	TermsURL                                    string
}

// lines splits text into its non-empty lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func printDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("Jan 2, 2006")
}

// rateText writes a rate in hundredths of a percent ("18%", "7.25%").
func rateText(r int64) string {
	s := fmt.Sprintf("%d.%02d", r/100, r%100)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".") + "%"
}

func (s *Server) printInvoice(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	cfg, err := s.Billing.Invoicing(ctx)
	if err != nil {
		return err
	}
	inv, err := s.Billing.RawInvoice(ctx, id)
	if err != nil {
		return err
	}
	view, err := s.Billing.Invoice(ctx, id)
	if err != nil {
		return err
	}
	c := billing.Currency{Code: inv.Currency, Symbol: cfg.Currency.Symbol, Decimals: cfg.Currency.Decimals}
	if !strings.EqualFold(inv.Currency, cfg.Currency.Code) {
		c.Symbol = inv.Currency + " " // an invoice from before a currency change
	}
	money := func(v int64) string { return billing.FormatMoney(v, c) }
	optMoney := func(v int64) string {
		if v == 0 {
			return ""
		}
		return money(v)
	}
	d := printData{Heading: "Invoice", Number: view.Number, Status: inv.Status, Company: cfg.Company,
		CompanyLines: lines(cfg.Company.Address), LogoURL: s.logoURL(ctx), Issued: printDate(inv.IssuedAt),
		Due: printDate(inv.DueAt), PaidOn: printDate(inv.PaidAt), BillTo: inv.BillingAddress, Subtotal: money(inv.Subtotal),
		Discount: optMoney(inv.Discount), Total: money(inv.Total), Inclusive: cfg.Tax.Inclusive,
		CreditApplied: optMoney(inv.CreditApplied), AmountPaid: optMoney(inv.AmountPaid),
		AmountRefunded: optMoney(inv.AmountRefunded), Balance: optMoney(view.Balance), Notes: lines(inv.Notes),
		Footer: lines(cfg.Invoice.Footer), TermsURL: cfg.Invoice.TermsURL}
	if d.Company.Name == "" {
		d.Company.Name = s.brandName(ctx)
	}
	if !inv.PeriodStart.IsZero() && !inv.PeriodEnd.IsZero() {
		d.Period = printDate(inv.PeriodStart) + " – " + printDate(inv.PeriodEnd.AddDate(0, 0, -1))
	}
	if inv.Number == "" && inv.Status != store.InvoiceDraft {
		d.Heading = "Proforma invoice"
	}
	d.StatusLabel = map[string]string{store.InvoiceDraft: "Draft", store.InvoiceUnpaid: "Unpaid", store.InvoicePaid: "Paid",
		store.InvoiceCancelled: "Cancelled", store.InvoiceRefunded: "Refunded",
		store.InvoicePartiallyRefunded: "Partially refunded"}[inv.Status]
	switch {
	case inv.Status == store.InvoicePaid || inv.Status == store.InvoicePartiallyRefunded:
		d.Stamp, d.StampLabel = "paid", "Paid"
	case view.Overdue:
		d.Stamp, d.StampLabel, d.StatusLabel = "overdue", "Overdue", "Overdue"
	case inv.Status == store.InvoiceCancelled:
		d.Stamp, d.StampLabel = "cancelled", "Cancelled"
	case inv.Status == store.InvoiceRefunded:
		d.Stamp, d.StampLabel = "refunded", "Refunded"
	}
	d.Title = d.Heading + " " + view.Number
	if d.Company.Name != "" {
		d.Title += " — " + d.Company.Name
	}
	// A promotion is an item with a negative amount; it shows once, as the
	// discount line under the subtotal, named after the promotion.
	for _, it := range inv.Items {
		if it.Kind == billing.ItemDiscount {
			d.DiscountLabel = it.Description
			continue
		}
		d.Items = append(d.Items, printItem{Kind: it.Kind, Description: it.Description, Quantity: it.Quantity,
			UnitPrice: money(it.UnitPrice), Amount: money(it.Amount)})
	}
	for _, t := range inv.TaxLines {
		d.TaxLines = append(d.TaxLines, printTax{Name: t.Name, Rate: rateText(t.Rate), Amount: money(t.Amount)})
	}
	for _, p := range inv.Payments {
		pp := printPayment{Date: printDate(p.At), Method: p.Gateway, Reference: p.Reference, Amount: money(p.Amount)}
		if p.Refunded > 0 {
			pp.Refunded = money(p.Refunded)
		}
		d.Payments = append(d.Payments, pp)
	}
	if inv.Status == store.InvoiceUnpaid && cfg.Methods.Manual.Enabled {
		d.Instructions = lines(cfg.Methods.Manual.Instructions)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	return invoicePage.Execute(w, d)
}
