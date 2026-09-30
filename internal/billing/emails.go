package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

// E-mail to clients about their billing. Messages go to the account's
// billing contact (else the account's e-mail); automation queues them with
// dedupe keys, so a run repeated (or a webhook replayed) sends nothing
// twice. Sending never fails what caused it: a message that can't be
// queued is logged.

func init() {
	inv := map[string]any{"Number": "INV-000123", "Total": "$118.00", "Balance": "$118.00", "AmountPaid": "$0.00",
		"DueDate": "Oct 30, 2026", "IssuedDate": "Oct 23, 2026", "Period": "Oct 30 to Nov 29, 2026",
		"URL": "https://panel.example.com/#/billing/invoices/12"}
	who := map[string]any{"Account": map[string]any{"Name": "Acme Ltd"}, "Contact": map[string]any{"Name": "Jo"}}
	sample := func(extra map[string]any) map[string]any {
		out := map[string]any{"Invoice": inv}
		for k, v := range who {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	invVars := []string{"Invoice.Number", "Invoice.Total", "Invoice.Balance", "Invoice.DueDate", "Invoice.Period",
		"Invoice.URL", "Account.Name", "Contact.Name"}
	for _, t := range []mailer.Template{
		{Name: "invoice.created", Description: "A new invoice is issued (renewals, plan changes, invoices staff send).",
			Subject: "Invoice {{.Invoice.Number}} for {{.Invoice.Total}}",
			Body: "Hello {{.Contact.Name}},\n\nInvoice {{.Invoice.Number}} for {{.Invoice.Total}} is ready" +
				"{{if .Invoice.Period}} ({{.Invoice.Period}}){{end}}. It is due on {{.Invoice.DueDate}}.\n\n" +
				"[[View and pay|{{.Invoice.URL}}]]\n\nThank you,\n{{.Brand.Name}}",
			Vars: invVars, Sample: sample(nil)},
		{Name: "invoice.reminder", Description: "A few days before an unpaid invoice is due.",
			Subject: "Reminder: invoice {{.Invoice.Number}} is due on {{.Invoice.DueDate}}",
			Body: "Hello {{.Contact.Name}},\n\nThis is a friendly reminder that invoice {{.Invoice.Number}} " +
				"({{.Invoice.Balance}}) is due on {{.Invoice.DueDate}}.\n\n[[Pay now|{{.Invoice.URL}}]]\n\n" +
				"If you have already paid, thank you: please ignore this message.\n\n{{.Brand.Name}}",
			Vars: invVars, Sample: sample(nil)},
		{Name: "invoice.overdue", Description: "After the due date of an unpaid invoice, on each overdue reminder day.",
			Subject: "Overdue: invoice {{.Invoice.Number}} ({{.Invoice.Balance}})",
			Body: "Hello {{.Contact.Name}},\n\nInvoice {{.Invoice.Number}} was due on {{.Invoice.DueDate}} and " +
				"{{.Invoice.Balance}} is still unpaid ({{.Days}} day(s) overdue).{{if .SuspendDate}} To keep your " +
				"services running, please pay it before {{.SuspendDate}}.{{end}}\n\n[[Pay now|{{.Invoice.URL}}]]\n\n{{.Brand.Name}}",
			Vars: append(invVars, "Days", "SuspendDate"), Sample: sample(map[string]any{"Days": 3, "SuspendDate": "Nov 4, 2026"})},
		{Name: "invoice.paid", Description: "The receipt, once an invoice is paid.",
			Subject: "Payment received: invoice {{.Invoice.Number}}",
			Body: "Hello {{.Contact.Name}},\n\nThank you: we received {{.Payment.Amount}} for invoice " +
				"{{.Invoice.Number}}{{if .Payment.Method}} ({{.Payment.Method}}){{end}}. The invoice is paid." +
				"{{if .BurstMinutes}} {{.BurstMinutes}} burst minutes were added to your account.{{end}}\n\n" +
				"[[View the invoice|{{.Invoice.URL}}]]\n\n{{.Brand.Name}}",
			Vars:   append(invVars, "Payment.Amount", "Payment.Method", "BurstMinutes"),
			Sample: sample(map[string]any{"Payment": map[string]any{"Amount": "$118.00", "Method": "Card"}})},
		{Name: "invoice.payment_failed", Description: "Charging the saved card for an invoice failed.",
			Subject: "We couldn't charge your card for invoice {{.Invoice.Number}}",
			Body: "Hello {{.Contact.Name}},\n\nWe tried to charge your saved card {{.Invoice.Balance}} for invoice " +
				"{{.Invoice.Number}}, but it didn't go through{{if .Reason}}: {{.Reason}}{{end}}.\n\n" +
				"Please pay it yourself, or update your card, before {{.Invoice.DueDate}}.\n\n" +
				"[[Pay now|{{.Invoice.URL}}]]\n\n{{.Brand.Name}}",
			Vars: append(invVars, "Reason"), Sample: sample(map[string]any{"Reason": "Your card was declined."})},
		{Name: "order.received", Description: "Someone ordered from the order form.",
			Subject: "We received your order",
			Body: "Hello {{.Contact.Name}},\n\nThank you for your order of {{.Order.Plan}} ({{.Order.Cycle}}). " +
				"Your account is ready as soon as invoice {{.Invoice.Number}} ({{.Invoice.Total}}) is paid.\n\n" +
				"[[Pay the invoice|{{.Invoice.URL}}]]\n\nYour username is {{.Username}}.\n\n{{.Brand.Name}}",
			Vars:   append(invVars, "Order.Plan", "Order.Cycle", "Username"),
			Sample: sample(map[string]any{"Order": map[string]any{"Plan": "Pro", "Cycle": "monthly"}, "Username": "jo"})},
		{Name: "account.welcome", Description: "An order is paid (or accepted) and its account is active.",
			Subject: "Welcome to {{.Brand.Name}}",
			Body: "Hello {{.Contact.Name}},\n\nYour {{.Plan}} account is active. Sign in to create your first " +
				"site.\n\n[[Sign in|{{.PanelURL}}/]]\n\n{{if .Username}}Your username is {{.Username}}.\n\n{{end}}{{.Brand.Name}}",
			Vars:   []string{"Account.Name", "Contact.Name", "Plan", "Username"},
			Sample: map[string]any{"Account": who["Account"], "Contact": who["Contact"], "Plan": "Pro", "Username": "jo"}},
		{Name: "account.suspended", Description: "The account is suspended for an unpaid invoice.",
			Subject: "Your services are suspended",
			Body: "Hello {{.Contact.Name}},\n\nInvoice {{.Invoice.Number}} ({{.Invoice.Balance}}) is overdue, so your " +
				"sites are suspended. Paying it brings them back right away.\n\n[[Pay now|{{.Invoice.URL}}]]\n\n{{.Brand.Name}}",
			Vars: invVars, Sample: sample(nil)},
		{Name: "account.unsuspended", Description: "The account is back after its overdue invoices are paid.",
			Subject: "Your services are back",
			Body: "Hello {{.Contact.Name}},\n\nThank you for your payment: your sites are running again.\n\n" +
				"[[Open the panel|{{.PanelURL}}/]]\n\n{{.Brand.Name}}",
			Vars: []string{"Account.Name", "Contact.Name"}, Sample: map[string]any{"Account": who["Account"], "Contact": who["Contact"]}},
		{Name: "account.cancelled", Description: "The account's service ended (cancelled, or terminated for non-payment).",
			Subject: "Your service has ended",
			Body: "Hello {{.Contact.Name}},\n\nYour account {{.Account.Name}} has been closed{{if .Reason}}: {{.Reason}}{{end}}." +
				"\n\nThank you for having been with us.\n\n{{.Brand.Name}}",
			Vars:   []string{"Account.Name", "Contact.Name", "Reason"},
			Sample: map[string]any{"Account": who["Account"], "Contact": who["Contact"], "Reason": "cancelled at your request"}},
		{Name: "credit.added", Description: "Credit is added to the account.",
			Subject: "{{.Credit.Amount}} of credit added to your account",
			Body: "Hello {{.Contact.Name}},\n\n{{.Credit.Amount}} of credit was added to your account" +
				"{{if .Credit.Description}} ({{.Credit.Description}}){{end}}. Your credit is now {{.Credit.Balance}}; " +
				"it pays your next invoices.\n\n{{.Brand.Name}}",
			Vars: []string{"Credit.Amount", "Credit.Balance", "Credit.Description", "Account.Name", "Contact.Name"},
			Sample: map[string]any{"Account": who["Account"], "Contact": who["Contact"],
				"Credit": map[string]any{"Amount": "$10.00", "Balance": "$25.00", "Description": "Goodwill"}}},
	} {
		t.Group = "Billing"
		mailer.Register(t)
	}
}

// shortDate is how e-mails write dates ("Oct 30, 2026").
func shortDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("Jan 2, 2006")
}

// periodText is a period for people: "Oct 30 to Nov 29, 2026" (the day
// before the end: the end is when the next period starts).
func periodText(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return ""
	}
	last := end.AddDate(0, 0, -1)
	if last.Before(start) {
		last = start
	}
	if start.Year() == last.Year() {
		return start.UTC().Format("Jan 2") + " to " + last.UTC().Format("Jan 2, 2006")
	}
	return start.UTC().Format("Jan 2, 2006") + " to " + last.UTC().Format("Jan 2, 2006")
}

// InvoiceURL is where a client sees (and pays) an invoice.
func (s *Service) InvoiceURL(id int64) string {
	return fmt.Sprintf("%s/#/billing/invoices/%d", s.PanelURL, id)
}

func (s *Service) invoiceMailData(cfg *InvoicingSettings, inv *store.Invoice) map[string]any {
	return map[string]any{"Invoice": map[string]any{
		"Number": displayNumber(inv), "Total": FormatMoney(inv.Total, cfg.Currency),
		"Balance": FormatMoney(max(inv.Balance(), 0), cfg.Currency), "AmountPaid": FormatMoney(inv.AmountPaid, cfg.Currency),
		"DueDate": shortDate(inv.DueAt), "IssuedDate": shortDate(inv.IssuedAt), "Period": periodText(inv.PeriodStart, inv.PeriodEnd),
		"URL": s.InvoiceURL(inv.ID), "Kind": inv.Kind}}
}

// recipient is who gets an account's billing e-mail.
func recipient(a *store.Account, p *store.BillingProfile) string {
	if p != nil && mailer.ValidAddress(p.Contact.Email) {
		return p.Contact.Email
	}
	if mailer.ValidAddress(a.Email) {
		return a.Email
	}
	return ""
}

func contactName(a *store.Account, p *store.BillingProfile) string {
	if p != nil {
		if n := strings.TrimSpace(p.Contact.FirstName); n != "" {
			return n
		}
		if n := strings.TrimSpace(p.Contact.Company); n != "" {
			return n
		}
	}
	return a.Name
}

// mail queues a billing message to an account (nothing without a mailer
// or an address). It reports whether a message was queued (false: sent
// before with this dedupe key, or not sendable).
func (s *Service) mail(ctx context.Context, accountID int64, template, dedupe string, data map[string]any) bool {
	if s.Mailer == nil {
		return false
	}
	ctx = context.WithoutCancel(ctx)
	a, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		s.Log.Warn("billing e-mail: account", "account", accountID, "err", err)
		return false
	}
	p, _ := s.Store.GetBillingProfile(ctx, accountID)
	to := recipient(a, p)
	if to == "" {
		s.Log.Info("billing e-mail not sent: the account has no e-mail address", "account", accountID, "template", template)
		return false
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Account"] = map[string]any{"Name": a.Name}
	data["Contact"] = map[string]any{"Name": contactName(a, p)}
	queued, err := s.Mailer.Queue(ctx, mailer.Message{To: []string{to}, Template: template, Data: data, AccountID: a.ID,
		DedupeKey: dedupe})
	if err != nil {
		s.Log.Warn("billing e-mail: queueing", "account", accountID, "template", template, "err", err)
	}
	return queued
}

// mailInvoice queues a message about an invoice.
func (s *Service) mailInvoice(ctx context.Context, cfg *InvoicingSettings, inv *store.Invoice, template, dedupe string, extra map[string]any) bool {
	data := s.invoiceMailData(cfg, inv)
	for k, v := range extra {
		data[k] = v
	}
	return s.mail(ctx, inv.AccountID, template, dedupe, data)
}
