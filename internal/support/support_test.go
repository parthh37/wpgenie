package support

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

// env: a reseller R with customer C, and a direct customer A.
type env struct {
	t    *testing.T
	st   *store.Store
	svc  *Service
	now  time.Time
	acct map[string]*store.Account
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Unix(1_800_000_000, 0).UTC(), acct: map[string]*store.Account{}}
	log := slog.New(slog.DiscardHandler)
	e.svc = &Service{Store: st, Log: log, Dir: t.TempDir(), Now: func() time.Time { return e.now },
		Mailer: &mailer.Service{Store: st, Log: log, PanelURL: "https://panel.test", Now: func() time.Time { return e.now }}}
	ctx := context.Background()
	if err := st.CreatePlan(ctx, &store.Plan{ID: "p", Name: "P", Overage: "notify"}); err != nil {
		t.Fatal(err)
	}
	mk := func(name, kind string, parent int64) {
		a, _, _, err := st.CreateAccount(ctx, &store.Account{Name: name, Kind: kind, PlanID: "p", ParentID: parent,
			Email: strings.ToLower(name) + "@example.test"}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		e.acct[name] = a
	}
	mk("A", store.AccountCustomer, 0)
	mk("R", store.AccountReseller, 0)
	mk("C", store.AccountCustomer, e.acct["R"].ID)
	if _, err := e.svc.SetSettings(ctx, Settings{Enabled: true, NotifyEmails: []string{"help@op.test"}, AutoCloseDays: 7,
		MaxFiles: 2, MaxFileMB: 1, Extensions: DefaultExtensions, ReplyTo: "support@op.test"}); err != nil {
		t.Fatal(err)
	}
	return e
}

var (
	staff = Actor{UserID: 1, Name: "sam"}
	alice = func(e *env) Actor { return Actor{UserID: 2, Name: "alice", AccountID: e.acct["A"].ID} }
	rita  = func(e *env) Actor { return Actor{UserID: 3, Name: "rita", AccountID: e.acct["R"].ID, Reseller: true} }
	carl  = func(e *env) Actor { return Actor{UserID: 4, Name: "carl", AccountID: e.acct["C"].ID} }
)

func (e *env) open(a Actor, subject string) *Thread {
	e.t.Helper()
	th, err := e.svc.Open(context.Background(), a, OpenInput{Subject: subject, Body: "Help with " + subject}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

// mails returns the outbox's messages by template, oldest first.
func (e *env) mails(template string) []*store.MailMessage {
	e.t.Helper()
	all, err := e.st.MailLog(context.Background(), store.MailFilter{Limit: 500})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []*store.MailMessage
	for _, m := range slices.Backward(all) {
		if template == "" || m.Template == template {
			out = append(out, m)
		}
	}
	return out
}

func TestStatusTransitions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	th := e.open(alice(e), "Site down")
	id := th.Ticket.ID
	if th.Ticket.Status != store.TicketOpen || th.Ticket.You != PartyCustomer || th.Ticket.Awaiting {
		t.Fatalf("opened: %+v", th.Ticket)
	}
	step := func(name string, th *Thread, err error, want string) *Thread {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if th.Ticket.Status != want {
			t.Fatalf("%s: status %s, want %s", name, th.Ticket.Status, want)
		}
		return th
	}
	th, err := e.svc.Reply(ctx, staff, id, ReplyInput{Body: "Looking", Internal: true}, nil)
	step("a note", th, err, store.TicketOpen)
	th, err = e.svc.Reply(ctx, staff, id, ReplyInput{Body: "Fixed"}, nil)
	th = step("staff reply", th, err, store.TicketAnswered)
	if th.Ticket.FirstResponseAt.IsZero() || th.Ticket.Awaiting {
		t.Errorf("first response: %+v", th.Ticket)
	}
	th, _ = e.svc.Get(ctx, alice(e), id)
	if !th.Ticket.Awaiting {
		t.Error("an answered ticket waits for the customer")
	}
	th, err = e.svc.Reply(ctx, alice(e), id, ReplyInput{Body: "Still broken"}, nil)
	step("customer reply", th, err, store.TicketCustomerReply)
	th, err = e.svc.Reply(ctx, staff, id, ReplyInput{Body: "On it", Status: store.TicketInProgress}, nil)
	step("reply keeping it in progress", th, err, store.TicketInProgress)
	th, err = e.svc.Reply(ctx, staff, id, ReplyInput{Body: "Done now", Status: store.TicketClosed}, nil)
	th = step("reply & close", th, err, store.TicketClosed)
	if th.Ticket.ClosedAt.IsZero() {
		t.Error("closed without closed_at")
	}
	th, err = e.svc.Reply(ctx, alice(e), id, ReplyInput{Body: "It broke again"}, nil)
	th = step("a reply reopens", th, err, store.TicketCustomerReply)
	if !th.Ticket.ClosedAt.IsZero() {
		t.Error("reopened with closed_at")
	}

	// Customers close and reopen; nothing else.
	closed, open := store.TicketClosed, store.TicketOpen
	th, err = e.svc.Update(ctx, alice(e), id, UpdateInput{Status: &closed})
	step("customer closes", th, err, store.TicketClosed)
	th, err = e.svc.Update(ctx, alice(e), id, UpdateInput{Status: &open})
	step("customer reopens", th, err, store.TicketOpen)
	for name, in := range map[string]UpdateInput{
		"status":     {Status: ptr(store.TicketOnHold)},
		"reopen":     {Status: &open}, // not closed
		"priority":   {Priority: ptr("urgent")},
		"department": {DepartmentID: ptr(int64(1))},
		"assignee":   {AssignedUserID: ptr(int64(1))},
	} {
		if in.Status != nil && *in.Status == open {
			continue // already open: no change, no error
		}
		if _, err := e.svc.Update(ctx, alice(e), id, in); !errors.Is(err, ErrForbidden) {
			t.Errorf("customer changed the %s: %v", name, err)
		}
	}
	if _, err := e.svc.Reply(ctx, alice(e), id, ReplyInput{Body: "x", Internal: true}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("customer wrote a note: %v", err)
	}
	if _, err := e.svc.Reply(ctx, alice(e), id, ReplyInput{Body: "x", Status: store.TicketClosed}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("customer set a status with a reply: %v", err)
	}
	// Staff: anything, and each change is noted.
	th, err = e.svc.Update(ctx, staff, id, UpdateInput{Status: ptr(store.TicketOnHold), Priority: ptr("urgent")})
	step("staff", th, err, store.TicketOnHold)
	if th.Ticket.Priority != "urgent" {
		t.Errorf("priority %s", th.Ticket.Priority)
	}
	last := th.Messages[len(th.Messages)-1]
	if last.Side != store.SideSystem || !last.Internal || !strings.Contains(last.Body, "priority to urgent") {
		t.Errorf("change note: %+v", last)
	}
	if _, err := e.svc.Update(ctx, staff, id, UpdateInput{Status: ptr("bogus")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bogus status: %v", err)
	}
	if _, err := e.svc.Update(ctx, staff, id, UpdateInput{AssignedUserID: ptr(int64(999))}); !errors.Is(err, ErrInvalid) {
		t.Errorf("assigned to nobody known: %v", err)
	}
	// Viewers look only.
	if _, err := e.svc.Reply(ctx, Actor{Name: "vic", ReadOnly: true}, id, ReplyInput{Body: "x"}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer replied: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestPartiesAndInternalNotes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	carls := e.open(carl(e), "Carl's problem").Ticket.ID
	ritas := e.open(rita(e), "Rita's own question").Ticket.ID
	alices := e.open(alice(e), "Alice's").Ticket.ID

	// Who's a party to what.
	for _, c := range []struct {
		who  Actor
		id   int64
		want Party
	}{
		{carl(e), carls, PartyCustomer}, {rita(e), carls, PartyHandler}, {staff, carls, PartyStaff},
		{rita(e), ritas, PartyCustomer}, {carl(e), ritas, PartyNone}, {alice(e), carls, PartyNone},
		{rita(e), alices, PartyNone},
	} {
		th, err := e.svc.Get(ctx, c.who, c.id)
		switch {
		case c.want == PartyNone && !errors.Is(err, store.ErrNotFound):
			t.Errorf("%s reached ticket %d: %v", c.who.Name, c.id, err)
		case c.want != PartyNone && (err != nil || th.Ticket.You != c.want):
			t.Errorf("%s on ticket %d: %+v %v", c.who.Name, c.id, th, err)
		}
	}
	// The reseller handles its customer's ticket: notes and replies.
	if _, err := e.svc.Reply(ctx, rita(e), carls, ReplyInput{Body: "note: carl's plugin is outdated", Internal: true}, nil); err != nil {
		t.Fatal(err)
	}
	th, err := e.svc.Reply(ctx, rita(e), carls, ReplyInput{Body: "Please update the plugin"}, nil)
	if err != nil || th.Ticket.Status != store.TicketAnswered || th.Messages[len(th.Messages)-1].Side != store.SideHandler {
		t.Fatalf("handler reply: %+v %v", th, err)
	}
	// Staff notes on the reseller's own ticket are not for the reseller.
	e.svc.Reply(ctx, staff, ritas, ReplyInput{Body: "note: reseller owes us", Internal: true}, nil)

	hidden := func(who Actor, id int64, secret string) {
		t.Helper()
		th, err := e.svc.Get(ctx, who, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range th.Messages {
			if m.Internal || strings.Contains(m.Body, secret) {
				t.Errorf("%s sees an internal note: %+v", who.Name, m)
			}
			if m.Side == store.SideHandler {
				t.Errorf("%s sees who handles the ticket: %+v", who.Name, m)
			}
		}
		if th.Ticket.Handler != "" || th.Ticket.HandlerAccount != 0 || th.Ticket.AssignedTo != "" {
			t.Errorf("%s sees provider fields: %+v", who.Name, th.Ticket)
		}
		list, _ := e.svc.List(ctx, who, ListInput{})
		for _, v := range list {
			if strings.Contains(v.Preview, secret) {
				t.Errorf("%s sees a note in a preview: %+v", who.Name, v)
			}
		}
	}
	hidden(carl(e), carls, "outdated")
	hidden(rita(e), ritas, "owes")
	// Providers see them.
	th, _ = e.svc.Get(ctx, staff, carls)
	if n := len(slices.DeleteFunc(slices.Clone(th.Messages), func(m *MessageView) bool { return !m.Internal })); n != 1 {
		t.Errorf("staff see %d notes", n)
	}
	// A note after a reply doesn't become the preview (not even for staff:
	// previews are always public).
	e.svc.Reply(ctx, staff, carls, ReplyInput{Body: "note: newest", Internal: true}, nil)
	list, _ := e.svc.List(ctx, staff, ListInput{})
	for _, v := range list {
		if v.ID == carls && v.Preview != "Please update the plugin" {
			t.Errorf("preview %q", v.Preview)
		}
	}

	// Lists: their own, and the reseller its customers'.
	ids := func(a Actor, in ListInput) []int64 {
		l, err := e.svc.List(ctx, a, in)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, v := range l {
			out = append(out, v.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(rita(e), ListInput{}); !slices.Equal(got, []int64{carls, ritas}) {
		t.Errorf("reseller's list %v", got)
	}
	if got := ids(carl(e), ListInput{}); !slices.Equal(got, []int64{carls}) {
		t.Errorf("customer's list %v", got)
	}
	if got := ids(staff, ListInput{}); len(got) != 3 {
		t.Errorf("staff list %v", got)
	}
	// Awaiting: staff wait on Alice's and Rita's (Carl's is Rita's to
	// answer, and answered); Carl waits on his; Rita on nothing.
	if got := ids(staff, ListInput{Awaiting: true}); !slices.Equal(got, []int64{ritas, alices}) && !slices.Equal(got, []int64{alices, ritas}) {
		t.Errorf("staff awaiting %v", got)
	}
	if got := ids(carl(e), ListInput{Awaiting: true}); !slices.Equal(got, []int64{carls}) {
		t.Errorf("carl awaiting %v", got)
	}
	e.svc.Reply(ctx, carl(e), carls, ReplyInput{Body: "Done, still broken"}, nil)
	if got := ids(rita(e), ListInput{Awaiting: true}); !slices.Equal(got, []int64{carls}) {
		t.Errorf("rita awaiting %v", got)
	}
	sum, err := e.svc.Summary(ctx, rita(e))
	if err != nil || sum.Awaiting != 1 || sum.Active != 2 || sum.Total != 2 || sum.Limits.MaxFiles != 2 {
		t.Errorf("summary %+v %v", sum, err)
	}
	if _, err := e.svc.List(ctx, carl(e), ListInput{Assigned: "me"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a customer filtered by assignee: %v", err)
	}

	// Escalation: the reseller only, once; the reseller stays on it.
	if _, err := e.svc.Escalate(ctx, carl(e), carls, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("customer escalated: %v", err)
	}
	if _, err := e.svc.Escalate(ctx, staff, carls, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("staff escalated: %v", err)
	}
	th, err = e.svc.Escalate(ctx, rita(e), carls, "Server-side issue")
	if err != nil || th.CanEscalate || th.Ticket.Handler != "staff" || th.Ticket.EscalatedAt.IsZero() {
		t.Fatalf("escalate: %+v %v", th, err)
	}
	if _, err := e.svc.Escalate(ctx, rita(e), carls, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("escalated twice: %v", err)
	}
	if got := ids(staff, ListInput{Awaiting: true}); !slices.Contains(got, carls) {
		t.Errorf("escalated ticket not in the staff's queue: %v", got)
	}
	hidden(carl(e), carls, "Server-side")
	if _, err := e.svc.Reply(ctx, rita(e), carls, ReplyInput{Body: "Staff are on it"}, nil); err != nil {
		t.Errorf("reseller reply after escalating: %v", err)
	}
	// Assignees are staff's business.
	if _, err := e.svc.Update(ctx, rita(e), carls, UpdateInput{AssignedUserID: ptr(int64(1))}); !errors.Is(err, ErrForbidden) {
		t.Errorf("reseller assigned: %v", err)
	}
}

func TestOpenRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, in := range map[string]OpenInput{
		"no subject":     {Body: "x"},
		"no body":        {Subject: "x"},
		"bad priority":   {Subject: "x", Body: "x", Priority: "asap"},
		"no department":  {Subject: "x", Body: "x", DepartmentID: 999},
		"long subject":   {Subject: strings.Repeat("x", 201), Body: "x"},
		"not their site": {Subject: "x", Body: "x", SiteID: "sc"},
	} {
		if _, err := e.svc.Open(ctx, alice(e), in, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.svc.Open(ctx, alice(e), OpenInput{AccountID: e.acct["C"].ID, Subject: "x", Body: "x"}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("customer opened for another account: %v", err)
	}
	if _, err := e.svc.Open(ctx, staff, OpenInput{Subject: "x", Body: "x"}, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("staff without an account: %v", err)
	}
	// Hidden departments are staff's.
	hid, _ := e.svc.CreateDepartment(ctx, DepartmentInput{Name: "Abuse", Hidden: true})
	if _, err := e.svc.Open(ctx, alice(e), OpenInput{Subject: "x", Body: "x", DepartmentID: hid.ID}, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("hidden department: %v", err)
	}
	// Sites: the account's, or a reseller's customers'.
	e.st.AssignSite(ctx, "sc", e.acct["C"].ID)
	if _, err := e.svc.Open(ctx, rita(e), OpenInput{Subject: "x", Body: "x", SiteID: "sc"}, nil); err != nil {
		t.Errorf("reseller about a customer's site: %v", err)
	}
	if th, err := e.svc.Open(ctx, carl(e), OpenInput{Subject: "  Carl's\n site  ", Body: "x\r\n", SiteID: "sc"}, nil); err != nil ||
		th.Ticket.Subject != "Carl's site" || th.Ticket.SiteID != "sc" || th.Ticket.Priority != "medium" || th.Ticket.Department != "General" {
		t.Errorf("customer's own site: %+v %v", th, err)
	}
	// Staff open tickets for accounts: the customer's turn.
	th, err := e.svc.Open(ctx, staff, OpenInput{AccountID: e.acct["A"].ID, Subject: "We noticed", Body: "Your site…", DepartmentID: hid.ID}, nil)
	if err != nil || th.Ticket.Status != store.TicketAnswered || th.Messages[0].Side != store.SideStaff || th.Ticket.AccountName != "A" {
		t.Errorf("staff-opened: %+v %v", th, err)
	}
	// Turned off: customers can't open tickets, staff still can.
	e.svc.SetSettings(ctx, Settings{Enabled: false, MaxFileMB: 1, Extensions: DefaultExtensions})
	if _, err := e.svc.Open(ctx, alice(e), OpenInput{Subject: "x", Body: "x"}, nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("opened while disabled: %v", err)
	}
	if _, err := e.svc.Open(ctx, staff, OpenInput{AccountID: e.acct["A"].ID, Subject: "x", Body: "x"}, nil); err != nil {
		t.Errorf("staff while disabled: %v", err)
	}
}

func TestAutoClose(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.open(alice(e), "Answered").Ticket.ID
	b := e.open(alice(e), "Waiting for staff").Ticket.ID
	c := e.open(alice(e), "Customer replied").Ticket.ID
	e.svc.Reply(ctx, staff, a, ReplyInput{Body: "Try this"}, nil)
	e.svc.Reply(ctx, staff, c, ReplyInput{Body: "Try that"}, nil)
	e.now = e.now.Add(3 * 24 * time.Hour)
	e.svc.Reply(ctx, alice(e), c, ReplyInput{Body: "Didn't work"}, nil)

	e.now = e.now.Add(3 * 24 * time.Hour) // 6 days after the answer
	if n, err := e.svc.AutoClose(ctx); n != 0 || err != nil {
		t.Fatalf("closed too early: %d %v", n, err)
	}
	e.now = e.now.Add(25 * time.Hour)
	if n, err := e.svc.AutoClose(ctx); n != 1 || err != nil {
		t.Fatalf("AutoClose = %d %v", n, err)
	}
	// Idempotent: nothing more, no second e-mail.
	if n, _ := e.svc.AutoClose(ctx); n != 0 {
		t.Errorf("second run closed %d", n)
	}
	for id, want := range map[int64]string{a: store.TicketClosed, b: store.TicketOpen, c: store.TicketCustomerReply} {
		th, _ := e.svc.Get(ctx, alice(e), id)
		if th.Ticket.Status != want {
			t.Errorf("ticket %d: %s, want %s", id, th.Ticket.Status, want)
		}
	}
	th, _ := e.svc.Get(ctx, alice(e), a)
	last := th.Messages[len(th.Messages)-1]
	if last.Side != store.SideSystem || last.Internal || !strings.Contains(last.Body, "7 days") {
		t.Errorf("auto-close note: %+v", last)
	}
	closed := e.mails("ticket.closed")
	if len(closed) != 1 || closed[0].To[0] != "a@example.test" || !strings.Contains(closed[0].Text, "7 days") {
		t.Fatalf("closed e-mails: %+v", closed)
	}
	// A reply reopens it; answered and silent again, it closes again.
	e.svc.Reply(ctx, alice(e), a, ReplyInput{Body: "Back again"}, nil)
	e.svc.Reply(ctx, staff, a, ReplyInput{Body: "Try once more"}, nil)
	e.now = e.now.Add(8 * 24 * time.Hour)
	if n, _ := e.svc.AutoClose(ctx); n != 1 {
		t.Errorf("second close: %d", n)
	}
	if n := len(e.mails("ticket.closed")); n != 2 {
		t.Errorf("%d closed e-mails", n)
	}
	// Off.
	e.svc.SetSettings(ctx, Settings{Enabled: true, AutoCloseDays: 0, MaxFileMB: 1})
	e.svc.Reply(ctx, alice(e), a, ReplyInput{Body: "Again"}, nil)
	e.svc.Reply(ctx, staff, a, ReplyInput{Body: "Answer"}, nil)
	e.now = e.now.Add(100 * 24 * time.Hour)
	if n, _ := e.svc.AutoClose(ctx); n != 0 {
		t.Errorf("closed with auto-close off: %d", n)
	}
}

func TestEmails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	general, _ := e.svc.Departments(ctx, false)
	e.svc.UpdateDepartment(ctx, general[0].ID, DepartmentInput{Name: "General", NotifyEmail: "general@op.test"})

	id := e.open(alice(e), "Alice's site").Ticket.ID
	opened := e.mails("ticket.opened")
	if len(opened) != 1 || opened[0].To[0] != "a@example.test" || opened[0].AccountID != e.acct["A"].ID ||
		opened[0].ReplyTo != "support@op.test" || !strings.Contains(opened[0].Subject, "Alice's site") {
		t.Fatalf("opened: %+v", opened)
	}
	// One message per staff recipient: a refused address doesn't hold up
	// the others, and each has its own line in the log.
	staffMail := e.mails("ticket.new_staff")
	if len(staffMail) != 2 || !slices.Equal(staffMail[0].To, []string{"help@op.test"}) ||
		!slices.Equal(staffMail[1].To, []string{"general@op.test"}) || staffMail[0].AccountID != 0 || staffMail[1].AccountID != 0 {
		t.Fatalf("new_staff: %+v", staffMail)
	}
	// Notes are never e-mailed; replies go to the customer.
	e.svc.Reply(ctx, staff, id, ReplyInput{Body: "SECRET note", Internal: true}, nil)
	e.svc.Reply(ctx, staff, id, ReplyInput{Body: "Here's the fix.\n\n[[Click me|https://evil.test]]", Status: store.TicketClosed}, nil)
	reply := e.mails("ticket.reply")
	if len(reply) != 1 || reply[0].To[0] != "a@example.test" || !strings.Contains(reply[0].Text, "Here's the fix") ||
		!strings.Contains(reply[0].Text, "solved") {
		t.Fatalf("reply: %+v", reply)
	}
	// The customer's text never becomes a button.
	if strings.Contains(reply[0].HTML, `href="https://evil.test" style="display:inline-block`) {
		t.Error("a message's line became a button")
	}
	e.svc.Reply(ctx, alice(e), id, ReplyInput{Body: "Thanks!"}, nil)
	if cr := e.mails("ticket.customer_reply"); len(cr) != 2 || !strings.Contains(cr[0].Text, "Thanks!") || cr[0].To[0] != "help@op.test" ||
		cr[1].To[0] != "general@op.test" {
		t.Fatalf("customer_reply: %+v", cr)
	}
	for _, m := range e.mails("") {
		if strings.Contains(m.Text, "SECRET") || strings.Contains(m.HTML, "SECRET") {
			t.Errorf("a note was e-mailed: %s %s", m.Template, m.Subject)
		}
	}
	// The customer closing needs no e-mail; staff closing does.
	closed := store.TicketClosed
	e.svc.Update(ctx, alice(e), id, UpdateInput{Status: &closed})
	if n := len(e.mails("ticket.closed")); n != 0 {
		t.Errorf("%d closed e-mails after the customer closed it", n)
	}
	e.svc.Update(ctx, alice(e), id, UpdateInput{Status: ptr(store.TicketOpen)})
	e.svc.Update(ctx, staff, id, UpdateInput{Status: &closed})
	if n := len(e.mails("ticket.closed")); n != 1 {
		t.Errorf("%d closed e-mails after staff closed it", n)
	}

	// A reseller's customer: the reseller hears about it, not the operator;
	// once escalated, the operator.
	before := len(e.mails("ticket.new_staff"))
	cid := e.open(carl(e), "Carl's").Ticket.ID
	ns := e.mails("ticket.new_staff")
	if len(ns) != before+1 || ns[len(ns)-1].To[0] != "r@example.test" || ns[len(ns)-1].AccountID != e.acct["R"].ID {
		t.Fatalf("handler not told: %+v", ns[len(ns)-1])
	}
	e.svc.Reply(ctx, carl(e), cid, ReplyInput{Body: "More detail"}, nil)
	if cr := e.mails("ticket.customer_reply"); cr[len(cr)-1].To[0] != "r@example.test" {
		t.Errorf("customer reply went to %v", cr[len(cr)-1].To)
	}
	e.svc.Escalate(ctx, rita(e), cid, "Needs root")
	ns = e.mails("ticket.new_staff")
	for i, want := range []string{"help@op.test", "general@op.test"} {
		m := ns[len(ns)-2+i]
		if m.To[0] != want || len(m.To) != 1 || !strings.Contains(m.Subject, "Escalated") || !strings.Contains(m.Text, "Needs root") {
			t.Errorf("escalation e-mail to %s: %s %v", want, m.Subject, m.To)
		}
	}
	// No address, no e-mail (and no error).
	a := e.acct["A"]
	a.Email = ""
	e.st.UpdateAccount(ctx, a)
	n := len(e.mails(""))
	e.svc.Reply(ctx, staff, id, ReplyInput{Body: "x"}, nil)
	if len(e.mails("")) != n {
		t.Error("e-mailed an account without an address")
	}
	// Every template renders its sample.
	for _, tpl := range []string{"ticket.opened", "ticket.new_staff", "ticket.reply", "ticket.customer_reply", "ticket.closed"} {
		if _, err := e.svc.Mailer.Preview(ctx, tpl, mailer.Override{}); err != nil {
			t.Errorf("%s: %v", tpl, err)
		}
	}
}

var pngHead = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func TestAttachments(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, _ := e.svc.Settings(ctx)
	stage := func(name string, body []byte) (*Staged, error) { return e.svc.Stage(st, name, bytes.NewReader(body)) }
	for _, c := range []struct {
		name string
		body []byte
		want error
	}{
		{"page.html", []byte("<html>"), ErrInvalid},                      // not allowed
		{"x.png", []byte("<html><script>alert(1)</script>"), ErrInvalid}, // not a PNG
		{"notes.txt", []byte("<!DOCTYPE html><html>"), ErrInvalid},       // a page named .txt
		{"empty.txt", nil, ErrInvalid},
		{"noext", []byte("hello"), ErrInvalid},
		{"big.txt", bytes.Repeat([]byte("a"), 1<<20+1), ErrTooLarge},
	} {
		if _, err := stage(c.name, c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if left, _ := os.ReadDir(e.svc.stagingDir()); len(left) != 0 {
		t.Errorf("refused uploads left files: %v", left)
	}
	png, err := stage(`C:\Users\me\"shot".png`, pngHead)
	if err != nil || png.Name != "shot.png" || png.ContentType != "image/png" || png.Size != int64(len(pngHead)) {
		t.Fatalf("png: %+v %v", png, err)
	}
	log, err := stage("../../etc/error.log", []byte("PHP Fatal error: x"))
	if err != nil || log.Name != "error.log" || log.ContentType != "text/plain" {
		t.Fatalf("log: %+v %v", log, err)
	}
	third, _ := stage("c.txt", []byte("c"))
	if _, err := e.svc.Open(ctx, alice(e), OpenInput{Subject: "x", Body: "x"}, []*Staged{png, log, third}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("3 files with a limit of 2: %v", err)
	}
	if _, err := os.Stat(third.path); !os.IsNotExist(err) {
		t.Error("a refused message's files stayed staged")
	}
	png, _ = stage("shot.png", pngHead)
	th, err := e.svc.Open(ctx, alice(e), OpenInput{Subject: "Screenshot", Body: "See attached"}, []*Staged{png})
	if err != nil || len(th.Messages[0].Files) != 1 || !th.Messages[0].Files[0].Image || th.Warning != "" {
		t.Fatalf("open with a file: %+v %v", th, err)
	}
	id, attID := th.Ticket.ID, th.Messages[0].Files[0].ID
	att, f, err := e.svc.Attachment(ctx, alice(e), id, attID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, pngHead) || att.Name != "shot.png" || filepath.Dir(f.Name()) != filepath.Join(e.svc.Dir, "1") {
		t.Errorf("attachment %+v at %s", att, f.Name())
	}
	// A note's attachment is the providers'.
	secret, _ := stage("secret.txt", []byte("internal"))
	th, err = e.svc.Reply(ctx, staff, id, ReplyInput{Internal: true}, []*Staged{secret})
	if err != nil {
		t.Fatal(err)
	}
	noteAtt := th.Messages[len(th.Messages)-1].Files[0].ID
	if _, _, err := e.svc.Attachment(ctx, alice(e), id, noteAtt); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("customer got a note's attachment: %v", err)
	}
	if _, f, err := e.svc.Attachment(ctx, staff, id, noteAtt); err != nil {
		t.Errorf("staff: %v", err)
	} else {
		f.Close()
	}
	// Other people's tickets: not there.
	if _, _, err := e.svc.Attachment(ctx, carl(e), id, attID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another customer: %v", err)
	}
	other := e.open(carl(e), "x").Ticket.ID
	if _, _, err := e.svc.Attachment(ctx, carl(e), other, attID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("through another ticket: %v", err)
	}
	// Attachments off.
	e.svc.SetSettings(ctx, Settings{Enabled: true, MaxFiles: 0, MaxFileMB: 1, Extensions: DefaultExtensions})
	st, _ = e.svc.Settings(ctx)
	f2, _ := e.svc.Stage(st, "a.txt", strings.NewReader("a"))
	if _, err := e.svc.Reply(ctx, alice(e), id, ReplyInput{Body: "x"}, []*Staged{f2}); !errors.Is(err, ErrInvalid) {
		t.Errorf("attachments while off: %v", err)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"a.png": "a.png", "dir/b.pdf": "b.pdf", `C:\x\c.txt`: "c.txt", "\x00\x1bev\x07il.zip": "evil.zip", "..": "attachment",
		"": "attachment", "  spaced.txt ": "spaced.txt", strings.Repeat("é", 200) + ".pdf": strings.Repeat("é", 116) + ".pdf",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSettingsAndDepartments(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, in := range map[string]Settings{
		"bad address":    {NotifyEmails: []string{"not an address"}, MaxFileMB: 1},
		"bad reply-to":   {ReplyTo: "x", MaxFileMB: 1},
		"too many days":  {AutoCloseDays: 400, MaxFileMB: 1},
		"too many files": {MaxFiles: 11, MaxFileMB: 1},
		"huge files":     {MaxFileMB: 100},
		"html allowed":   {MaxFileMB: 1, Extensions: []string{"pdf", "html"}},
		"bad extension":  {MaxFileMB: 1, Extensions: []string{"p d f"}},
	} {
		if _, err := e.svc.SetSettings(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, err := e.svc.SetSettings(ctx, Settings{Enabled: true, NotifyEmails: []string{" a@op.test", "a@op.test", ""}, MaxFileMB: 2,
		Extensions: []string{".PDF", "pdf", "docx"}})
	if err != nil || !slices.Equal(got.NotifyEmails, []string{"a@op.test"}) || !slices.Equal(got.Extensions, []string{"pdf", "docx"}) {
		t.Errorf("normalized: %+v %v", got, err)
	}
	// Defaults before anything is saved.
	e.st.SetSetting(ctx, SettingKey, "")
	if st, _ := e.svc.Settings(ctx); !st.Enabled || st.AutoCloseDays != 7 || st.MaxFiles != 5 || st.MaxFileMB != 5 {
		t.Errorf("defaults: %+v", st)
	}

	// The last visible department stays.
	list, _ := e.svc.Departments(ctx, true)
	general := list[0].ID
	if _, err := e.svc.UpdateDepartment(ctx, general, DepartmentInput{Name: "General", Hidden: true}); !errors.Is(err, ErrConflict) {
		t.Errorf("hid the last department: %v", err)
	}
	if err := e.svc.DeleteDepartment(ctx, general); !errors.Is(err, ErrConflict) {
		t.Errorf("deleted the last department: %v", err)
	}
	sales, err := e.svc.CreateDepartment(ctx, DepartmentInput{Name: " Sales ", NotifyEmail: "sales@op.test", Sort: 1})
	if err != nil || sales.Name != "Sales" {
		t.Fatalf("create: %+v %v", sales, err)
	}
	if _, err := e.svc.UpdateDepartment(ctx, general, DepartmentInput{Name: "General", Hidden: true}); err != nil {
		t.Errorf("hide with another visible: %v", err)
	}
	if list, _ := e.svc.Departments(ctx, false); len(list) != 1 || list[0].ID != sales.ID {
		t.Errorf("visible: %+v", list)
	}
	for name, in := range map[string]DepartmentInput{"no name": {}, "bad e-mail": {Name: "x", NotifyEmail: "x"}, "sort": {Name: "x", Sort: 5000}} {
		if _, err := e.svc.CreateDepartment(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Canned replies.
	if _, err := e.svc.CreateCanned(ctx, CannedInput{Title: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("canned without a body: %v", err)
	}
	c, err := e.svc.CreateCanned(ctx, CannedInput{Title: "Thanks", Body: "Glad to help"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateCanned(ctx, c.ID+1, CannedInput{Title: "x", Body: "y"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update a missing canned reply: %v", err)
	}
}

// The handler follows the account: moved to another reseller, the new
// one handles its tickets (and hears about them); detached, staff do.
func TestHandlerFollowsTheAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r2, _, _, err := e.st.CreateAccount(ctx, &store.Account{Name: "R2", Kind: store.AccountReseller, PlanID: "p",
		Email: "r2@example.test"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ruth := Actor{UserID: 9, Name: "ruth", AccountID: r2.ID, Reseller: true}
	id := e.open(carl(e), "Carl's").Ticket.ID
	c := e.acct["C"]
	c.ParentID = r2.ID
	if err := e.st.UpdateAccount(ctx, c); err != nil {
		t.Fatal(err)
	}
	e.svc.Reply(ctx, carl(e), id, ReplyInput{Body: "Hello new reseller"}, nil)
	cr := e.mails("ticket.customer_reply")
	if len(cr) != 1 || cr[0].To[0] != "r2@example.test" || cr[0].AccountID != r2.ID {
		t.Fatalf("customer reply went to %+v", cr)
	}
	if _, err := e.svc.Get(ctx, rita(e), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old reseller still reaches it: %v", err)
	}
	th, err := e.svc.Get(ctx, ruth, id)
	if err != nil || th.Ticket.You != PartyHandler || !th.CanEscalate || !th.Ticket.Awaiting {
		t.Fatalf("the new reseller: %+v %v", th, err)
	}
	if sum, _ := e.svc.Summary(ctx, rita(e)); sum.Awaiting != 0 {
		t.Errorf("the old reseller is awaited on %d", sum.Awaiting)
	}
	if sum, _ := e.svc.Summary(ctx, ruth); sum.Awaiting != 1 {
		t.Errorf("the new reseller is awaited on %d", sum.Awaiting)
	}
	if sum, _ := e.svc.Summary(ctx, staff); sum.Awaiting != 0 {
		t.Errorf("staff awaited on a reseller's ticket: %d", sum.Awaiting)
	}
	// Detached: the staff's.
	c.ParentID = 0
	e.st.UpdateAccount(ctx, c)
	e.svc.Reply(ctx, carl(e), id, ReplyInput{Body: "Anyone?"}, nil)
	if cr := e.mails("ticket.customer_reply"); len(cr) != 2 || cr[1].To[0] != "help@op.test" || cr[1].AccountID != 0 {
		t.Fatalf("after detaching: %+v", cr[len(cr)-1])
	}
	if sum, _ := e.svc.Summary(ctx, staff); sum.Awaiting != 1 {
		t.Errorf("staff not awaited on a detached customer's ticket: %d", sum.Awaiting)
	}
	if th, _ := e.svc.Get(ctx, staff, id); th.Ticket.Handler != "staff" {
		t.Errorf("handler %q", th.Ticket.Handler)
	}
	if _, err := e.svc.Escalate(ctx, ruth, id, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a former reseller escalated: %v", err)
	}
	// Escalated tickets stay with staff when the customer moves.
	c.ParentID = r2.ID
	e.st.UpdateAccount(ctx, c)
	if _, err := e.svc.Escalate(ctx, ruth, id, "yours"); err != nil {
		t.Fatal(err)
	}
	c.ParentID = e.acct["R"].ID
	e.st.UpdateAccount(ctx, c)
	if th, _ := e.svc.Get(ctx, rita(e), id); th.Ticket.Handler != "staff" || th.CanEscalate {
		t.Errorf("escalated ticket after a move: %+v", th.Ticket)
	}
}

// What staff open for a reseller's customer stays with staff.
func TestStaffOpenedTicketsStayWithStaff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	th, err := e.svc.Open(ctx, staff, OpenInput{AccountID: e.acct["C"].ID, Subject: "About your site", Body: "We noticed…"}, nil)
	if err != nil || th.Ticket.Handler != "staff" {
		t.Fatalf("open: %+v %v", th, err)
	}
	id := th.Ticket.ID
	if th, _ := e.svc.Get(ctx, rita(e), id); th.CanEscalate || th.Ticket.Awaiting || !th.Ticket.StaffOpened || th.Ticket.Escalated {
		t.Errorf("the reseller handles a staff ticket: %+v", th)
	}
	if _, err := e.svc.Escalate(ctx, rita(e), id, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("escalated a staff ticket: %v", err)
	}
	e.svc.Reply(ctx, carl(e), id, ReplyInput{Body: "Thanks, what should I do?"}, nil)
	cr := e.mails("ticket.customer_reply")
	if len(cr) != 1 || cr[0].To[0] != "help@op.test" {
		t.Fatalf("customer reply went to %+v", cr)
	}
	if n := len(e.mails("ticket.new_staff")); n != 0 {
		t.Errorf("%d new-ticket e-mails for a ticket staff opened", n)
	}
	if sum, _ := e.svc.Summary(ctx, staff); sum.Awaiting != 1 {
		t.Errorf("staff awaited on %d", sum.Awaiting)
	}
	if sum, _ := e.svc.Summary(ctx, rita(e)); sum.Awaiting != 0 {
		t.Errorf("the reseller awaited on %d", sum.Awaiting)
	}
}

// A status change decided on a status that has since changed is refused
// (409), and writes nothing.
func TestUpdateRefusesStaleStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open(alice(e), "x").Ticket.ID
	e.svc.Reply(ctx, staff, id, ReplyInput{Body: "Answer"}, nil)
	// Staff read the ticket answered; the customer replies meanwhile, and
	// the close is decided on what was read.
	t0, _, err := e.svc.ticket(ctx, staff, id)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Reply(ctx, alice(e), id, ReplyInput{Body: "Still broken"}, nil)
	closed := store.TicketClosed
	err = e.st.UpdateTicket(ctx, id, store.TicketChanges{Status: &closed, FromStatus: t0.Status, At: e.now})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale close: %v", err)
	}
	if th, _ := e.svc.Get(ctx, staff, id); th.Ticket.Status != store.TicketCustomerReply {
		t.Errorf("status %s", th.Ticket.Status)
	}
	// Changing the priority alone keeps the reply's status.
	th, err := e.svc.Update(ctx, staff, id, UpdateInput{Priority: ptr("high")})
	if err != nil || th.Ticket.Status != store.TicketCustomerReply || th.Ticket.Priority != "high" {
		t.Errorf("priority change: %+v %v", th, err)
	}
	// Closing twice sends one e-mail.
	e.svc.Update(ctx, staff, id, UpdateInput{Status: &closed})
	e.svc.Update(ctx, staff, id, UpdateInput{Status: &closed})
	if n := len(e.mails("ticket.closed")); n != 1 {
		t.Errorf("%d closed e-mails", n)
	}
}
