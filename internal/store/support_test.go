package store

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

// Support tickets' store cases, part of the suite (both backends).

func TestStoreSupport(t *testing.T) { forEachBackend(t, testSupport) }

func testSupport(t *testing.T, s *Store) {
	ctx := context.Background()
	if depts, err := s.SupportDepartments(ctx); err != nil || len(depts) != 0 {
		t.Fatalf("SupportDepartments = %+v, %v", depts, err)
	}
	gen := &SupportDepartment{Name: "General", Description: "Anything"}
	if err := s.CreateSupportDepartment(ctx, gen); err != nil {
		t.Fatal(err)
	}
	general := gen.ID
	billing := &SupportDepartment{Name: "Billing", Description: "Invoices", NotifyEmail: "billing@op.test", Sort: 5}
	if err := s.CreateSupportDepartment(ctx, billing); err != nil || billing.ID == 0 {
		t.Fatal(err)
	}
	billing.Hidden, billing.Sort = true, -1
	if err := s.UpdateSupportDepartment(ctx, billing); err != nil {
		t.Fatal(err)
	}
	if d, err := s.GetSupportDepartment(ctx, billing.ID); err != nil || !d.Hidden || d.NotifyEmail != "billing@op.test" {
		t.Errorf("GetSupportDepartment = %+v, %v", d, err)
	}
	if depts, _ := s.SupportDepartments(ctx); len(depts) != 2 || depts[0].ID != billing.ID {
		t.Errorf("departments by sort: %+v", depts)
	}
	if err := s.UpdateSupportDepartment(ctx, &SupportDepartment{ID: 999, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing department: %v", err)
	}

	if err := s.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Overage: "notify"}); err != nil {
		t.Fatal(err)
	}
	a, _, _, err := s.CreateAccount(ctx, &Account{Name: "Acme", Kind: AccountCustomer, PlanID: "p"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, _ := s.CreateAccount(ctx, &Account{Name: "Bravo", Kind: AccountCustomer, PlanID: "p"}, "", nil)
	staff, err := s.CreateUser(ctx, "sam", "x", "operator")
	if err != nil {
		t.Fatal(err)
	}

	at := time.Unix(1_800_000_000, 0).UTC()
	tk := &Ticket{AccountID: a.ID, UserID: 7, OpenedBy: "alice", DepartmentID: general, SiteID: "sa", Subject: "Site down",
		Status: TicketOpen, Priority: "high", CreatedAt: at}
	first := &TicketMessage{UserID: 7, Author: "alice", Side: SideCustomer, Body: "It shows a 500 error", CreatedAt: at,
		Attachments: []*TicketAttachment{{Name: "shot.png", File: "0123456789abcdef0123456789abcdef", ContentType: "image/png", Size: 42}}}
	if err := s.CreateTicket(ctx, tk, first); err != nil {
		t.Fatal(err)
	}
	if tk.ID == 0 || !regexp.MustCompile(`^WPG-\d{6}$`).MatchString(tk.Mask) || first.ID == 0 || first.Attachments[0].ID == 0 {
		t.Fatalf("CreateTicket: %+v %+v", tk, first)
	}
	other := &Ticket{AccountID: b.ID, OpenedBy: "bob", DepartmentID: billing.ID, Subject: "Invoice question",
		Status: TicketOpen, Priority: "low", CreatedAt: at.Add(time.Minute)}
	if err := s.CreateTicket(ctx, other, &TicketMessage{Author: "bob", Side: SideCustomer, Body: "Hi", CreatedAt: other.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	if other.Mask == tk.Mask {
		t.Fatal("two tickets share a mask")
	}

	got, err := s.GetTicket(ctx, tk.ID)
	if err != nil || got.AccountName != "Acme" || got.DepartmentName != "General" || got.Subject != "Site down" ||
		got.Preview != "It shows a 500 error" || !got.LastReplyAt.Equal(at) || !got.FirstResponseAt.IsZero() {
		t.Fatalf("GetTicket = %+v, %v", got, err)
	}
	if _, err := s.GetTicket(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTicket(missing) = %v", err)
	}

	// An internal note: no preview, no last reply, status unchanged.
	note := &TicketMessage{TicketID: tk.ID, UserID: staff.ID, Author: "sam", Side: SideStaff, Internal: true,
		Body: "secret: the customer's plugin", CreatedAt: at.Add(2 * time.Minute)}
	if err := s.AddTicketMessage(ctx, note, TicketActivity{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTicket(ctx, tk.ID)
	if got.Preview != "It shows a 500 error" || got.Status != TicketOpen || !got.LastReplyAt.Equal(at) ||
		!got.UpdatedAt.Equal(at.Add(2*time.Minute)) {
		t.Errorf("after a note: %+v", got)
	}
	// A reply: first response, answered.
	reply := &TicketMessage{TicketID: tk.ID, UserID: staff.ID, Author: "sam", Side: SideStaff, Body: "Fixed it",
		CreatedAt: at.Add(3 * time.Minute)}
	if err := s.AddTicketMessage(ctx, reply, TicketActivity{Status: TicketAnswered, Reply: true, FirstResponse: true}); err != nil {
		t.Fatal(err)
	}
	later := &TicketMessage{TicketID: tk.ID, Author: "sam", Side: SideStaff, Body: "And closed", CreatedAt: at.Add(4 * time.Minute)}
	if err := s.AddTicketMessage(ctx, later, TicketActivity{Status: TicketClosed, Reply: true, FirstResponse: true}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTicket(ctx, tk.ID)
	if got.Status != TicketClosed || !got.ClosedAt.Equal(at.Add(4*time.Minute)) || !got.FirstResponseAt.Equal(at.Add(3*time.Minute)) ||
		got.Preview != "And closed" || !got.LastReplyAt.Equal(at.Add(4*time.Minute)) {
		t.Errorf("after replies: %+v", got)
	}
	if err := s.AddTicketMessage(ctx, &TicketMessage{TicketID: 999, Author: "x", Side: SideStaff, Body: "x", CreatedAt: at},
		TicketActivity{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("message on a missing ticket: %v", err)
	}

	msgs, err := s.TicketMessages(ctx, tk.ID)
	if err != nil || len(msgs) != 4 || len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Name != "shot.png" ||
		!msgs[1].Internal || len(msgs[1].Attachments) != 0 {
		t.Fatalf("TicketMessages = %+v, %v", msgs, err)
	}
	att, err := s.GetTicketAttachment(ctx, tk.ID, first.Attachments[0].ID)
	if err != nil || att.Size != 42 || att.MessageID != first.ID || att.ContentType != "image/png" {
		t.Errorf("GetTicketAttachment = %+v, %v", att, err)
	}
	if _, err := s.GetTicketAttachment(ctx, other.ID, first.Attachments[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("attachment through another ticket: %v", err)
	}
	if m, err := s.GetTicketMessage(ctx, tk.ID, note.ID); err != nil || !m.Internal || m.Body != note.Body {
		t.Errorf("GetTicketMessage = %+v, %v", m, err)
	}
	if _, err := s.GetTicketMessage(ctx, other.ID, note.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("message through another ticket: %v", err)
	}

	// Changes, with notes.
	inProgress, urgent := TicketInProgress, "urgent"
	if err := s.UpdateTicket(ctx, tk.ID, TicketChanges{Status: &inProgress, FromStatus: TicketClosed, Priority: &urgent,
		DepartmentID: &billing.ID, AssignedUserID: &staff.ID, At: at.Add(5 * time.Minute), Notes: []*TicketMessage{
			{Author: "sam", Side: SideSystem, Body: "sam reopened the ticket.", CreatedAt: at.Add(5 * time.Minute)},
			{Author: "sam", Side: SideSystem, Internal: true, Body: "sam set the priority to urgent.", CreatedAt: at.Add(5 * time.Minute)},
		}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTicket(ctx, tk.ID)
	if got.Status != TicketInProgress || !got.ClosedAt.IsZero() || got.Priority != "urgent" || got.AssignedTo != "sam" ||
		got.DepartmentName != "Billing" || got.Preview != "And closed" {
		t.Errorf("after UpdateTicket: %+v", got)
	}
	if msgs, _ = s.TicketMessages(ctx, tk.ID); len(msgs) != 6 {
		t.Errorf("notes: %d messages", len(msgs))
	}
	closedStatus := TicketClosed
	if err := s.UpdateTicket(ctx, 999, TicketChanges{Status: &closedStatus, At: at}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing ticket: %v", err)
	}

	// Escalation: once.
	esc := &TicketMessage{Author: "rita", Side: SideSystem, Internal: true, Body: "escalated", CreatedAt: at.Add(6 * time.Minute)}
	if err := s.EscalateTicket(ctx, other.ID, at.Add(6*time.Minute), esc); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTicket(ctx, other.ID); got.HandlerAccountID != 0 || !got.EscalatedAt.Equal(at.Add(6*time.Minute)) {
		t.Errorf("escalated: %+v", got)
	}
	if err := s.EscalateTicket(ctx, other.ID, at, &TicketMessage{Author: "x", Side: SideSystem, Body: "x", CreatedAt: at}); !errors.Is(err, ErrConflict) {
		t.Errorf("escalated twice: %v", err)
	}

	// Lists and filters.
	list := func(f TicketFilter) []int64 {
		t.Helper()
		l, err := s.ListTickets(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, x := range l {
			ids = append(ids, x.ID)
		}
		return ids
	}
	eq := func(name string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", name, got, want)
				return
			}
		}
	}
	eq("all, last activity first", list(TicketFilter{}), other.ID, tk.ID)
	eq("account scope", list(TicketFilter{AccountIDs: []int64{a.ID}}), tk.ID)
	eq("empty scope", list(TicketFilter{AccountIDs: []int64{}}))
	eq("status", list(TicketFilter{Statuses: []string{TicketInProgress, TicketOnHold}}), tk.ID)
	eq("priority", list(TicketFilter{Priority: "low"}), other.ID)
	eq("department", list(TicketFilter{DepartmentID: billing.ID}), other.ID, tk.ID)
	eq("assigned", list(TicketFilter{AssignedUserID: staff.ID}), tk.ID)
	eq("unassigned", list(TicketFilter{AssignedUserID: -1}), other.ID)
	eq("query subject", list(TicketFilter{Query: "invoice"}), other.ID)
	eq("query account", list(TicketFilter{Query: "acm"}), tk.ID)
	eq("query mask", list(TicketFilter{Query: tk.Mask}), tk.ID)
	eq("query wildcard is literal", list(TicketFilter{Query: "%"}))
	eq("paging", list(TicketFilter{Before: other.ID}), tk.ID)
	eq("limit", list(TicketFilter{Limit: 1}), other.ID)
	zero := int64(0)
	eq("awaiting staff", list(TicketFilter{AwaitingHandler: &zero}), other.ID)
	eq("awaiting customer", list(TicketFilter{AwaitingCustomer: a.ID}))
	if n, err := s.CountTickets(ctx, TicketFilter{AccountIDs: []int64{a.ID, b.ID}, Query: "site"}); err != nil || n != 1 {
		t.Errorf("CountTickets = %d, %v", n, err)
	}

	// Auto-close: only answered tickets without a reply since the cutoff.
	s.AddTicketMessage(ctx, &TicketMessage{TicketID: tk.ID, Author: "sam", Side: SideStaff, Body: "Try now",
		CreatedAt: at.Add(10 * time.Minute)}, TicketActivity{Status: TicketAnswered, Reply: true})
	eq("awaiting customer", list(TicketFilter{AwaitingCustomer: a.ID}), tk.ID)
	eq("answered before", list(TicketFilter{Statuses: []string{TicketAnswered}, LastReplyBefore: at.Add(10 * time.Minute)}), tk.ID)
	eq("answered before", list(TicketFilter{Statuses: []string{TicketAnswered}, LastReplyBefore: at.Add(9 * time.Minute)}))
	closeNote := func() *TicketMessage {
		return &TicketMessage{Author: "WPGenie", Side: SideSystem, Body: "closed", CreatedAt: at.Add(time.Hour)}
	}
	if ok, err := s.AutoCloseTicket(ctx, tk.ID, at.Add(9*time.Minute), closeNote()); ok || err != nil {
		t.Errorf("closed a ticket replied to after the cutoff: %v %v", ok, err)
	}
	if ok, err := s.AutoCloseTicket(ctx, tk.ID, at.Add(10*time.Minute), closeNote()); !ok || err != nil {
		t.Errorf("AutoCloseTicket = %v %v", ok, err)
	}
	if ok, _ := s.AutoCloseTicket(ctx, tk.ID, at.Add(10*time.Minute), closeNote()); ok {
		t.Error("closed twice")
	}
	if got, _ := s.GetTicket(ctx, tk.ID); got.Status != TicketClosed || !got.ClosedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("auto-closed: %+v", got)
	}

	st, err := s.SupportStats(ctx, at)
	if err != nil || st.ByStatus[TicketClosed] != 1 || st.ByStatus[TicketOpen] != 1 || st.ByDepartment[billing.ID] != 1 ||
		st.Opened != 2 || st.Closed != 1 || st.Responded != 1 || st.ResponseSecondSum != 180 {
		t.Errorf("SupportStats = %+v, %v", st, err)
	}

	// Attachments that couldn't be kept are forgotten.
	if err := s.DeleteTicketAttachments(ctx, first.Attachments[0].ID); err != nil {
		t.Fatal(err)
	}
	if atts, _ := s.TicketAttachments(ctx, tk.ID); len(atts) != 0 {
		t.Errorf("attachments left: %+v", atts)
	}

	// A department with tickets can't go.
	if err := s.DeleteSupportDepartment(ctx, billing.ID); !errors.Is(err, ErrInUse) {
		t.Errorf("delete a department in use: %v", err)
	}
	empty := &SupportDepartment{Name: "Sales"}
	s.CreateSupportDepartment(ctx, empty)
	if err := s.DeleteSupportDepartment(ctx, empty.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSupportDepartment(ctx, empty.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}

	// Canned replies.
	c := &CannedReply{Title: "Thanks", Body: "Glad it works!"}
	if err := s.CreateCannedReply(ctx, c); err != nil || c.ID == 0 {
		t.Fatal(err)
	}
	c.Body = "Glad it works again!"
	if err := s.UpdateCannedReply(ctx, c); err != nil {
		t.Fatal(err)
	}
	s.CreateCannedReply(ctx, &CannedReply{Title: "Apology", Body: "Sorry"})
	cs, err := s.CannedReplies(ctx)
	if err != nil || len(cs) != 2 || cs[0].Title != "Apology" || cs[1].Body != "Glad it works again!" {
		t.Errorf("CannedReplies = %+v, %v", cs, err)
	}
	if err := s.DeleteCannedReply(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCannedReply(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

// Who handles a ticket is derived when it's read: the account's current
// reseller, unless the ticket was escalated or opened by staff, or the
// reseller is gone. Nothing goes stale when accounts move.
func TestStoreTicketHandlers(t *testing.T) { forEachBackend(t, testTicketHandlers) }

func testTicketHandlers(t *testing.T, s *Store) {
	ctx := context.Background()
	if err := s.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Overage: "notify"}); err != nil {
		t.Fatal(err)
	}
	mk := func(name, kind string, parent int64) *Account {
		a, _, _, err := s.CreateAccount(ctx, &Account{Name: name, Kind: kind, PlanID: "p", ParentID: parent}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	r1, r2 := mk("R1", AccountReseller, 0), mk("R2", AccountReseller, 0)
	c := mk("C", AccountCustomer, r1.ID)
	dept := &SupportDepartment{Name: "General"}
	s.CreateSupportDepartment(ctx, dept)
	at := time.Unix(1_800_000_000, 0).UTC()
	open := func(staffOpened bool) *Ticket {
		tk := &Ticket{AccountID: c.ID, OpenedBy: "carl", DepartmentID: dept.ID, Subject: "x", Status: TicketOpen,
			Priority: "medium", StaffOpened: staffOpened, CreatedAt: at}
		if err := s.CreateTicket(ctx, tk, &TicketMessage{Author: "carl", Side: SideCustomer, Body: "x", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	plain, byStaff, escalated := open(false), open(true), open(false)
	if err := s.EscalateTicket(ctx, escalated.ID, at, &TicketMessage{Author: "r", Side: SideSystem, Internal: true, Body: "x", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.EscalateTicket(ctx, byStaff.ID, at, &TicketMessage{Author: "r", Side: SideSystem, Body: "x", CreatedAt: at}); !errors.Is(err, ErrConflict) {
		t.Errorf("escalated a staff-opened ticket: %v", err)
	}
	handlers := func() (int64, int64, int64) {
		t.Helper()
		var out []int64
		for _, tk := range []*Ticket{plain, byStaff, escalated} {
			got, err := s.GetTicket(ctx, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, got.HandlerAccountID)
		}
		return out[0], out[1], out[2]
	}
	awaiting := func(handler int64) int {
		t.Helper()
		n, err := s.CountTickets(ctx, TicketFilter{AwaitingHandler: &handler})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func(name string, want int64, staffAwaits, r1Awaits, r2Awaits int) {
		t.Helper()
		p, b, e := handlers()
		if p != want || b != 0 || e != 0 {
			t.Errorf("%s: handlers %d %d %d, want %d 0 0", name, p, b, e, want)
		}
		if a0, a1, a2 := awaiting(0), awaiting(r1.ID), awaiting(r2.ID); a0 != staffAwaits || a1 != r1Awaits || a2 != r2Awaits {
			t.Errorf("%s: awaiting staff %d, R1 %d, R2 %d; want %d %d %d", name, a0, a1, a2, staffAwaits, r1Awaits, r2Awaits)
		}
	}
	if got, _ := s.GetTicket(ctx, byStaff.ID); !got.StaffOpened || got.Escalated {
		t.Errorf("flags: %+v", got)
	}
	check("under R1", r1.ID, 2, 1, 0)
	c.ParentID = r2.ID
	s.UpdateAccount(ctx, c)
	check("moved to R2", r2.ID, 2, 0, 1)
	s.SetAccountStatus(ctx, r2.ID, AccountTerminated, "terminated", at)
	check("reseller terminated", 0, 3, 0, 0)
	c.ParentID = r1.ID
	s.UpdateAccount(ctx, c)
	check("back to R1", r1.ID, 2, 1, 0)
	c.ParentID = 0
	s.UpdateAccount(ctx, c)
	check("detached", 0, 3, 0, 0)
	// A ticket list shows the same.
	list, _ := s.ListTickets(ctx, TicketFilter{})
	for _, tk := range list {
		if tk.HandlerAccountID != 0 {
			t.Errorf("listed with handler %d", tk.HandlerAccountID)
		}
	}
}

// A change writes only what it changes, and a status change only applies
// to the status it was decided on.
func TestStoreTicketChanges(t *testing.T) { forEachBackend(t, testTicketChanges) }

func testTicketChanges(t *testing.T, s *Store) {
	ctx := context.Background()
	s.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Overage: "notify"})
	a, _, _, _ := s.CreateAccount(ctx, &Account{Name: "A", Kind: AccountCustomer, PlanID: "p"}, "", nil)
	dept := &SupportDepartment{Name: "General"}
	s.CreateSupportDepartment(ctx, dept)
	at := time.Unix(1_800_000_000, 0).UTC()
	tk := &Ticket{AccountID: a.ID, OpenedBy: "a", DepartmentID: dept.ID, Subject: "x", Status: TicketAnswered, Priority: "low", CreatedAt: at}
	if err := s.CreateTicket(ctx, tk, &TicketMessage{Author: "a", Side: SideCustomer, Body: "x", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	// Staff read "answered"; the customer replies meanwhile.
	s.AddTicketMessage(ctx, &TicketMessage{TicketID: tk.ID, Author: "a", Side: SideCustomer, Body: "again", CreatedAt: at.Add(time.Minute)},
		TicketActivity{Status: TicketCustomerReply, Reply: true})
	// A priority change leaves the status the reply set.
	high := "high"
	if err := s.UpdateTicket(ctx, tk.ID, TicketChanges{Priority: &high, At: at.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTicket(ctx, tk.ID); got.Status != TicketCustomerReply || got.Priority != "high" {
		t.Errorf("after a priority change: %+v", got)
	}
	// Closing from the stale "answered": refused, nothing written.
	closed, urgent := TicketClosed, "urgent"
	err := s.UpdateTicket(ctx, tk.ID, TicketChanges{Status: &closed, FromStatus: TicketAnswered, Priority: &urgent, At: at.Add(3 * time.Minute),
		Notes: []*TicketMessage{{Author: "s", Side: SideSystem, Body: "closed", CreatedAt: at.Add(3 * time.Minute)}}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale status change: %v", err)
	}
	got, _ := s.GetTicket(ctx, tk.ID)
	if got.Status != TicketCustomerReply || got.Priority != "high" || !got.ClosedAt.IsZero() || !got.UpdatedAt.Equal(at.Add(2*time.Minute)) {
		t.Errorf("a refused change wrote: %+v", got)
	}
	if msgs, _ := s.TicketMessages(ctx, tk.ID); len(msgs) != 2 {
		t.Errorf("a refused change's note was kept: %d messages", len(msgs))
	}
	// From the right status it applies; closing twice from open is refused.
	if err := s.UpdateTicket(ctx, tk.ID, TicketChanges{Status: &closed, FromStatus: TicketCustomerReply, At: at.Add(4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTicket(ctx, tk.ID, TicketChanges{Status: &closed, FromStatus: TicketCustomerReply, At: at.Add(5 * time.Minute)}); !errors.Is(err, ErrConflict) {
		t.Errorf("closed twice: %v", err)
	}
	if got, _ := s.GetTicket(ctx, tk.ID); !got.ClosedAt.Equal(at.Add(4 * time.Minute)) {
		t.Errorf("closed_at %v", got.ClosedAt)
	}
}

// The list and count queries' filters have indexes.
func TestStoreTicketIndexes(t *testing.T) { forEachBackend(t, testTicketIndexes) }

func testTicketIndexes(t *testing.T, s *Store) {
	// Each backend's catalog, straight to the driver.
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`
	if s.Postgres() {
		q = `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1`
	}
	for _, name := range []string{"tickets_by_account", "tickets_by_status", "tickets_by_update", "tickets_by_department",
		"tickets_by_assignee", "ticket_messages_by_ticket", "ticket_attachments_by_ticket"} {
		var n int
		if err := s.db.sql.QueryRowContext(context.Background(), q, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d, %v", name, n, err)
		}
	}
}
