package support

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

// E-mail about tickets. Customers get the provider's replies at their
// account's address; new tickets and customers' replies go to whoever
// handles the ticket: the reseller (its account's address) or the
// operator's staff (the settings' recipients and the department's). Only
// public messages are ever e-mailed, and a message's text never becomes
// a button (see quote).

func init() {
	ticket := map[string]any{"Mask": "WPG-482913", "Subject": "My site shows an error after the update",
		"Department": "General", "Priority": "high", "Status": "answered", "Account": "Acme Ltd", "Site": "acme.example",
		"URL": "https://panel.example.com/#/support/42"}
	vars := []string{"Ticket.Mask", "Ticket.Subject", "Ticket.Department", "Ticket.Priority", "Ticket.Status",
		"Ticket.Account", "Ticket.Site", "Ticket.URL"}
	for _, t := range []mailer.Template{
		{Name: "ticket.opened", Description: "To the customer, when a ticket is opened (by them or by staff for them).",
			Subject: `[{{.Ticket.Mask}}] {{.Ticket.Subject}}`,
			Body: `Hello {{.Name}},

{{if .ByStaff}}We opened a support ticket for you: "{{.Ticket.Subject}}".

{{.Message}}{{else}}Thanks for getting in touch. We received your request "{{.Ticket.Subject}}" and will get back to you as soon as we can.{{end}}

Your ticket number is {{.Ticket.Mask}}.

[[View your ticket|{{.Ticket.URL}}]]`,
			Vars:   append([]string{"Name", "ByStaff", "Message"}, vars...),
			Sample: map[string]any{"Ticket": ticket, "Name": "Acme Ltd", "Message": "Hello, we noticed your site…"}},
		{Name: "ticket.new_staff", Description: "To staff (or the reseller handling it), when a ticket is opened or escalated.",
			Subject: `{{if .Escalated}}Escalated{{else}}New ticket{{end}} [{{.Ticket.Mask}}] {{.Ticket.Subject}}`,
			Body: `{{if .Escalated}}{{.Author}} escalated a ticket from {{.Ticket.Account}} to you.{{else}}{{.Ticket.Account}} opened a ticket in {{.Ticket.Department}} (priority {{.Ticket.Priority}}{{if .Ticket.Site}}, about {{.Ticket.Site}}{{end}}).{{end}}

{{.Message}}

[[Open the ticket|{{.Ticket.URL}}]]`,
			Vars:   append([]string{"Escalated", "Author", "Message"}, vars...),
			Sample: map[string]any{"Ticket": ticket, "Author": "alice", "Message": "Since this morning my site shows…"}},
		{Name: "ticket.reply", Description: "To the customer, when staff (or their reseller) reply.",
			Subject: `Re: [{{.Ticket.Mask}}] {{.Ticket.Subject}}`,
			Body: `{{.Author}} replied to your ticket "{{.Ticket.Subject}}":

{{.Message}}

{{if .Closed}}We've marked this ticket as solved. If you still need help, reply to it and it opens again.

{{end}}[[View and reply|{{.Ticket.URL}}]]`,
			Vars:   append([]string{"Author", "Message", "Closed"}, vars...),
			Sample: map[string]any{"Ticket": ticket, "Author": "Sam from support", "Message": "We found the cause…"}},
		{Name: "ticket.customer_reply", Description: "To staff (or the reseller handling it), when the customer replies.",
			Subject: `Reply [{{.Ticket.Mask}}] {{.Ticket.Subject}}`,
			Body: `{{.Author}} ({{.Ticket.Account}}) replied:

{{.Message}}

[[Open the ticket|{{.Ticket.URL}}]]`,
			Vars:   append([]string{"Author", "Message"}, vars...),
			Sample: map[string]any{"Ticket": ticket, "Author": "alice", "Message": "Thanks, it works again!"}},
		{Name: "ticket.closed", Description: "To the customer, when staff close a ticket or it closes after days without a reply.",
			Subject: `Closed [{{.Ticket.Mask}}] {{.Ticket.Subject}}`,
			Body: `Hello {{.Name}},

{{if .AutoClosed}}We haven't heard back from you for {{.Days}} days, so we've closed your ticket "{{.Ticket.Subject}}".{{else}}Your ticket "{{.Ticket.Subject}}" is closed.{{end}}

If you still need help, reply to it and it opens again.

[[View your ticket|{{.Ticket.URL}}]]`,
			Vars:   append([]string{"Name", "AutoClosed", "Days"}, vars...),
			Sample: map[string]any{"Ticket": ticket, "Name": "Acme Ltd", "AutoClosed": true, "Days": 7}},
	} {
		t.Group = "Support"
		mailer.Register(t)
	}
}

// quote makes a message safe to put in a template: a line that looks like
// a button ([[Label|URL]]) stays text.
func quote(body string) string {
	return strings.ReplaceAll(body, "[[", "[ [")
}

// ticketData is what every ticket template gets as .Ticket.
func (s *Service) ticketData(ctx context.Context, t *store.Ticket) map[string]any {
	site := ""
	if t.SiteID != "" {
		site = t.SiteID
		if st, err := s.Store.GetSite(ctx, t.SiteID); err == nil {
			site = st.PrimaryDomain
		}
	}
	return map[string]any{"Mask": t.Mask, "Subject": t.Subject, "Department": t.DepartmentName, "Priority": t.Priority,
		"Status": strings.ReplaceAll(t.Status, "_", " "), "Account": t.AccountName, "Site": site,
		"URL": strings.TrimSuffix(s.Mailer.PanelURL, "/") + "/#/support/" + strconv.FormatInt(t.ID, 10)}
}

// send queues a message for each recipient (one address a server
// refuses doesn't hold up the others, and each has its own line in the
// e-mail log); failures are logged, never the caller's: the ticket is
// saved whether or not its e-mail goes.
func (s *Service) send(ctx context.Context, st *Settings, to []string, acctID int64, tpl, key string, data map[string]any) {
	for _, addr := range to {
		k := key
		if len(to) > 1 {
			k += ":" + addr
		}
		_, err := s.Mailer.Queue(context.WithoutCancel(ctx), mailer.Message{To: []string{addr}, Template: tpl, Data: data,
			AccountID: acctID, ReplyTo: st.ReplyTo, DedupeKey: k})
		if err != nil {
			s.Log.Warn("support: queueing an e-mail", "template", tpl, "to", addr, "err", err)
		}
	}
}

// customer is the address of the ticket's account ("" when it has none).
func (s *Service) customer(ctx context.Context, t *store.Ticket) (*store.Account, []string) {
	acct, err := s.Store.GetAccount(ctx, t.AccountID)
	if err != nil {
		return nil, nil
	}
	if !mailer.ValidAddress(acct.Email) {
		return acct, nil
	}
	return acct, []string{acct.Email}
}

// providers are who handle the ticket now (t.HandlerAccountID is
// derived when read: the account's current reseller, unless escalated or
// opened by staff): the reseller's address (filed under its account), or
// the staff recipients and the department's (filed under no account: a
// customer's e-mail log never lists them).
func (s *Service) providers(ctx context.Context, st *Settings, t *store.Ticket) ([]string, int64) {
	if t.HandlerAccountID != 0 {
		r, err := s.Store.GetAccount(ctx, t.HandlerAccountID)
		if err != nil || !mailer.ValidAddress(r.Email) {
			return nil, 0
		}
		return []string{r.Email}, r.ID
	}
	to := slices.Clone(st.NotifyEmails)
	if d, err := s.Store.GetSupportDepartment(ctx, t.DepartmentID); err == nil && d.NotifyEmail != "" && !slices.Contains(to, d.NotifyEmail) {
		to = append(to, d.NotifyEmail)
	}
	return to, 0
}

func (s *Service) mailSettings(ctx context.Context) *Settings {
	if s.Mailer == nil {
		return nil
	}
	st, err := s.Settings(ctx)
	if err != nil {
		s.Log.Warn("support: settings", "err", err)
		return nil
	}
	return st
}

func (s *Service) notifyOpened(ctx context.Context, t *store.Ticket, dept *store.SupportDepartment, acct *store.Account, m *store.TicketMessage) {
	st := s.mailSettings(ctx)
	if st == nil {
		return
	}
	data := s.ticketData(ctx, t)
	data["Department"] = dept.Name
	if acct.Email != "" && mailer.ValidAddress(acct.Email) {
		s.send(ctx, st, []string{acct.Email}, acct.ID, "ticket.opened", fmt.Sprintf("ticket.opened:%d", t.ID),
			map[string]any{"Ticket": data, "Name": acct.Name, "ByStaff": m.Side == store.SideStaff, "Message": quote(m.Body)})
	}
	if m.Side == store.SideStaff {
		return // staff wrote it: nobody to tell on their side
	}
	to, filed := s.providers(ctx, st, t)
	s.send(ctx, st, to, filed, "ticket.new_staff", fmt.Sprintf("ticket.new_staff:%d", t.ID),
		map[string]any{"Ticket": data, "Author": m.Author, "Message": quote(m.Body)})
}

func (s *Service) notifyReply(ctx context.Context, t *store.Ticket, m *store.TicketMessage) {
	st := s.mailSettings(ctx)
	if st == nil {
		return
	}
	acct, to := s.customer(ctx, t)
	if acct == nil {
		return
	}
	s.send(ctx, st, to, acct.ID, "ticket.reply", fmt.Sprintf("ticket.reply:%d", m.ID), map[string]any{
		"Ticket": s.ticketData(ctx, t), "Author": m.Author, "Message": quote(m.Body), "Closed": t.Status == store.TicketClosed})
}

func (s *Service) notifyCustomerReply(ctx context.Context, t *store.Ticket, m *store.TicketMessage) {
	st := s.mailSettings(ctx)
	if st == nil {
		return
	}
	to, filed := s.providers(ctx, st, t)
	s.send(ctx, st, to, filed, "ticket.customer_reply", fmt.Sprintf("ticket.customer_reply:%d", m.ID),
		map[string]any{"Ticket": s.ticketData(ctx, t), "Author": m.Author, "Message": quote(m.Body)})
}

func (s *Service) notifyEscalated(ctx context.Context, t *store.Ticket, by, reason string) {
	st := s.mailSettings(ctx)
	if st == nil {
		return
	}
	if reason == "" {
		reason = "No reason given: see the conversation."
	}
	to, filed := s.providers(ctx, st, t)
	s.send(ctx, st, to, filed, "ticket.new_staff", fmt.Sprintf("ticket.escalated:%d:%d", t.ID, t.EscalatedAt.Unix()),
		map[string]any{"Ticket": s.ticketData(ctx, t), "Escalated": true, "Author": by, "Message": quote(reason)})
}

// notifyClosed tells the customer their ticket was closed (by the
// provider, or automatically after days days).
func (s *Service) notifyClosed(ctx context.Context, t *store.Ticket, auto bool, days int) {
	st := s.mailSettings(ctx)
	if st == nil {
		return
	}
	acct, to := s.customer(ctx, t)
	if acct == nil {
		return
	}
	s.send(ctx, st, to, acct.ID, "ticket.closed", fmt.Sprintf("ticket.closed:%d:%d", t.ID, t.ClosedAt.Unix()),
		map[string]any{"Ticket": s.ticketData(ctx, t), "Name": acct.Name, "AutoClosed": auto, "Days": days})
}
