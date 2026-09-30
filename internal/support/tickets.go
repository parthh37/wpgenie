package support

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

// Priorities, least urgent first.
var Priorities = []string{"low", "medium", "high", "urgent"}

const (
	maxSubject = 200
	maxBody    = 20000
)

// Actor is who is acting: staff (AccountID 0) or a user of a tenant
// account.
type Actor struct {
	UserID int64
	Name   string
	// AccountID is the tenant's account (0: the operator's staff).
	AccountID int64
	// Reseller: the tenant account is a reseller's.
	Reseller bool
	// ReadOnly: staff who may look but change nothing (viewers).
	ReadOnly bool
}

func (a Actor) staff() bool { return a.AccountID == 0 }

// Party is how an actor stands to a ticket.
type Party string

const (
	PartyNone     Party = ""
	PartyCustomer Party = "customer" // a user of the ticket's account
	PartyHandler  Party = "handler"  // the reseller of the ticket's account
	PartyStaff    Party = "staff"    // the operator's staff
)

// provider: staff or the handling reseller.
func (p Party) provider() bool { return p == PartyHandler || p == PartyStaff }

// partyOf says how a relates to t. A reseller stays a party to its
// customers' tickets after escalating them.
func (s *Service) partyOf(ctx context.Context, a Actor, t *store.Ticket) (Party, error) {
	switch {
	case a.staff():
		return PartyStaff, nil
	case t.AccountID == a.AccountID:
		return PartyCustomer, nil
	case a.Reseller:
		acct, err := s.Store.GetAccount(ctx, t.AccountID)
		if errors.Is(err, store.ErrNotFound) {
			return PartyNone, nil
		}
		if err != nil {
			return PartyNone, err
		}
		if acct.ParentID == a.AccountID {
			return PartyHandler, nil
		}
	}
	return PartyNone, nil
}

// ticket loads a ticket and a's standing; someone who isn't a party gets
// ErrNotFound, as if it didn't exist.
func (s *Service) ticket(ctx context.Context, a Actor, id int64) (*store.Ticket, Party, error) {
	t, err := s.Store.GetTicket(ctx, id)
	if err != nil {
		return nil, PartyNone, err
	}
	p, err := s.partyOf(ctx, a, t)
	if err != nil {
		return nil, PartyNone, err
	}
	if p == PartyNone {
		return nil, PartyNone, store.ErrNotFound
	}
	return t, p, nil
}

// ---- What the API returns ----

// TicketView is a ticket as a party sees it. Who handles it, the
// escalation and the assignee are the providers' business: customers
// don't get them.
type TicketView struct {
	ID           int64     `json:"id"`
	Mask         string    `json:"mask"`
	AccountID    int64     `json:"account_id"`
	AccountName  string    `json:"account_name"`
	OpenedBy     string    `json:"opened_by"`
	DepartmentID int64     `json:"department_id"`
	Department   string    `json:"department"`
	SiteID       string    `json:"site_id,omitempty"`
	SiteDomain   string    `json:"site_domain,omitempty"`
	Subject      string    `json:"subject"`
	Status       string    `json:"status"`
	Priority     string    `json:"priority"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastReplyAt  time.Time `json:"last_reply_at"`
	ClosedAt     time.Time `json:"closed_at,omitzero"`
	// Preview is the start of the last public message.
	Preview string `json:"preview"`
	// Awaiting: the ticket waits for the caller's reply.
	Awaiting bool `json:"awaiting"`
	// You: how the caller stands to the ticket.
	You Party `json:"you"`

	// Providers only.
	Handler string `json:"handler,omitempty"` // "staff" or "reseller"
	// Reseller is the account's reseller, who sees internal notes (but
	// not staff-only ones) whoever handles the ticket ("": none).
	Reseller        string    `json:"reseller,omitempty"`
	HandlerAccount  int64     `json:"handler_account_id,omitempty"`
	Escalated       bool      `json:"escalated,omitempty"`
	StaffOpened     bool      `json:"staff_opened,omitempty"`
	EscalatedAt     time.Time `json:"escalated_at,omitzero"`
	AssignedUserID  int64     `json:"assigned_user_id,omitempty"`
	AssignedTo      string    `json:"assigned_to,omitempty"`
	FirstResponseAt time.Time `json:"first_response_at,omitzero"`
}

// MessageView is a message as a party sees it.
type MessageView struct {
	ID int64 `json:"id"`
	// Side: customer, staff or system; providers also see handler (a
	// reseller's reply) where customers see staff.
	Side     string `json:"side"`
	Staff    bool   `json:"staff"`
	Internal bool   `json:"internal"`
	// StaffOnly: a note staff keep from the reseller too (staff only).
	StaffOnly bool             `json:"staff_only,omitempty"`
	Author    string           `json:"author"`
	Body      string           `json:"body"`
	At        time.Time        `json:"at"`
	Files     []AttachmentView `json:"attachments"`
}

// AttachmentView is an attachment's description (the file itself is at
// /api/v1/tickets/{ticket}/attachments/{id}).
type AttachmentView struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Type  string `json:"type"`
	Image bool   `json:"image"` // may be shown inline
}

// Thread is a ticket with its conversation.
type Thread struct {
	Ticket   *TicketView    `json:"ticket"`
	Messages []*MessageView `json:"messages"`
	// CanEscalate: the caller is the reseller handling it.
	CanEscalate bool `json:"can_escalate"`
	// Warning: the message was saved, but not everything with it.
	Warning string `json:"warning,omitempty"`
}

func awaiting(t *store.Ticket, p Party, a Actor) bool {
	waitingProvider := t.Status == store.TicketOpen || t.Status == store.TicketCustomerReply
	switch p {
	case PartyStaff:
		return waitingProvider && t.HandlerAccountID == 0
	case PartyHandler:
		return waitingProvider && t.HandlerAccountID == a.AccountID
	case PartyCustomer:
		return t.Status == store.TicketAnswered
	}
	return false
}

func view(t *store.Ticket, p Party, a Actor) *TicketView {
	v := &TicketView{ID: t.ID, Mask: t.Mask, AccountID: t.AccountID, AccountName: t.AccountName, OpenedBy: t.OpenedBy,
		DepartmentID: t.DepartmentID, Department: t.DepartmentName, SiteID: t.SiteID, Subject: t.Subject, Status: t.Status,
		Priority: t.Priority, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, LastReplyAt: t.LastReplyAt, ClosedAt: t.ClosedAt,
		Preview: t.Preview, Awaiting: awaiting(t, p, a), You: p}
	if p.provider() {
		v.Handler = "staff"
		if t.HandlerAccountID != 0 {
			v.Handler = "reseller"
		}
		v.HandlerAccount, v.EscalatedAt, v.FirstResponseAt = t.HandlerAccountID, t.EscalatedAt, t.FirstResponseAt
		v.Escalated, v.StaffOpened, v.Reseller = t.Escalated, t.StaffOpened, t.ResellerName
		v.AssignedUserID, v.AssignedTo = t.AssignedUserID, t.AssignedTo
	}
	return v
}

// previewTypes are the attachment types shown inline, as images (raster
// only: nothing that runs script).
var previewTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// Previewable reports whether an attachment of this type may be shown
// inline.
func Previewable(contentType string) bool { return previewTypes[contentType] }

// visible says whether a party sees a message: internal notes are the
// providers', staff-only notes the staff's alone.
func visible(m *store.TicketMessage, p Party) bool {
	switch {
	case m.StaffOnly:
		return p == PartyStaff
	case m.Internal:
		return p.provider()
	}
	return true
}

func messageView(m *store.TicketMessage, p Party) *MessageView {
	side := m.Side
	if p == PartyCustomer && side == store.SideHandler {
		side = store.SideStaff // a reseller's customers see their provider, whoever it is
	}
	v := &MessageView{ID: m.ID, Side: side, Staff: m.Side == store.SideHandler || m.Side == store.SideStaff,
		Internal: m.Internal, StaffOnly: m.StaffOnly, Author: m.Author, Body: m.Body, At: m.CreatedAt, Files: []AttachmentView{}}
	for _, a := range m.Attachments {
		v.Files = append(v.Files, AttachmentView{ID: a.ID, Name: a.Name, Size: a.Size, Type: a.ContentType, Image: Previewable(a.ContentType)})
	}
	return v
}

// ---- Reading ----

// ListInput filters a list of tickets.
type ListInput struct {
	Statuses     []string
	DepartmentID int64
	Priority     string
	// Assigned: "me", "none", or a user ID (staff only).
	Assigned  string
	AccountID int64
	Query     string
	// Awaiting keeps the tickets waiting for the caller's reply.
	Awaiting bool
	Before   int64
	Limit    int
}

// scope is the accounts whose tickets a tenant sees (nil for staff: all).
func (s *Service) scope(ctx context.Context, a Actor) ([]int64, error) {
	if a.staff() {
		return nil, nil
	}
	ids := []int64{a.AccountID}
	if a.Reseller {
		kids, err := s.Store.ListAccounts(ctx, store.AccountFilter{ParentID: a.AccountID})
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			ids = append(ids, k.ID)
		}
	}
	return ids, nil
}

func (s *Service) filter(ctx context.Context, a Actor, in ListInput) (store.TicketFilter, error) {
	f := store.TicketFilter{Statuses: in.Statuses, DepartmentID: in.DepartmentID, Priority: in.Priority,
		AccountID: in.AccountID, Query: in.Query, Before: in.Before, Limit: in.Limit}
	for _, st := range in.Statuses {
		if !slices.Contains(store.TicketStatuses, st) {
			return f, fmt.Errorf("%w: status %q", ErrInvalid, st)
		}
	}
	if in.Priority != "" && !slices.Contains(Priorities, in.Priority) {
		return f, fmt.Errorf("%w: priority %q", ErrInvalid, in.Priority)
	}
	if len(in.Query) > 100 {
		return f, fmt.Errorf("%w: search too long", ErrInvalid)
	}
	var err error
	if f.AccountIDs, err = s.scope(ctx, a); err != nil {
		return f, err
	}
	if in.Assigned != "" {
		if !a.staff() {
			return f, fmt.Errorf("%w: only staff filter by assignee", ErrInvalid)
		}
		switch in.Assigned {
		case "me":
			f.AssignedUserID = a.UserID
			if a.UserID == 0 {
				f.AssignedUserID = -1 // the installer token is nobody
			}
		case "none":
			f.AssignedUserID = -1
		default:
			n, err := strconv.ParseInt(in.Assigned, 10, 64)
			if err != nil || n <= 0 {
				return f, fmt.Errorf("%w: assigned is me, none or a user ID", ErrInvalid)
			}
			f.AssignedUserID = n
		}
	}
	if in.Awaiting {
		if a.staff() {
			zero := int64(0)
			f.AwaitingHandler = &zero
		} else {
			f.AwaitingCustomer = a.AccountID
			if a.Reseller {
				own := a.AccountID
				f.AwaitingHandler = &own
			}
		}
	}
	return f, nil
}

// List returns the tickets a may see that match in, last activity first.
func (s *Service) List(ctx context.Context, a Actor, in ListInput) ([]*TicketView, error) {
	f, err := s.filter(ctx, a, in)
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListTickets(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]*TicketView, 0, len(list))
	for _, t := range list {
		// The scope already holds only their own and (for a reseller)
		// their customers' accounts.
		p := PartyStaff
		if !a.staff() {
			p = PartyCustomer
			if t.AccountID != a.AccountID {
				p = PartyHandler
			}
		}
		out = append(out, view(t, p, a))
	}
	return out, nil
}

// Get returns a ticket and its conversation as a sees them.
func (s *Service) Get(ctx context.Context, a Actor, id int64) (*Thread, error) {
	t, p, err := s.ticket(ctx, a, id)
	if err != nil {
		return nil, err
	}
	msgs, err := s.Store.TicketMessages(ctx, id)
	if err != nil {
		return nil, err
	}
	th := &Thread{Ticket: view(t, p, a), Messages: []*MessageView{},
		CanEscalate: p == PartyHandler && t.HandlerAccountID == a.AccountID}
	if t.SiteID != "" {
		if st, err := s.Store.GetSite(ctx, t.SiteID); err == nil {
			th.Ticket.SiteDomain = st.PrimaryDomain
		}
	}
	for _, m := range msgs {
		if !visible(m, p) {
			continue
		}
		th.Messages = append(th.Messages, messageView(m, p))
	}
	return th, nil
}

// Summary is what the navigation shows: whether tickets can be opened,
// how many wait for the caller, and the attachment limits.
type Summary struct {
	Enabled  bool `json:"enabled"`
	Awaiting int  `json:"awaiting"`
	// Active: tickets in the caller's view that aren't closed.
	Active int `json:"active"`
	Total  int `json:"total"`
	Limits struct {
		MaxFiles   int      `json:"max_files"`
		MaxFileMB  int      `json:"max_file_mb"`
		Extensions []string `json:"extensions"`
	} `json:"limits"`
}

func (s *Service) Summary(ctx context.Context, a Actor) (*Summary, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	out := &Summary{Enabled: st.Enabled}
	out.Limits.MaxFiles, out.Limits.MaxFileMB, out.Limits.Extensions = st.MaxFiles, st.MaxFileMB, st.Extensions
	f, err := s.filter(ctx, a, ListInput{Awaiting: true})
	if err != nil {
		return nil, err
	}
	if out.Awaiting, err = s.Store.CountTickets(ctx, f); err != nil {
		return nil, err
	}
	all, err := s.filter(ctx, a, ListInput{})
	if err != nil {
		return nil, err
	}
	if out.Total, err = s.Store.CountTickets(ctx, all); err != nil {
		return nil, err
	}
	all.Statuses = slices.DeleteFunc(slices.Clone(store.TicketStatuses), func(x string) bool { return x == store.TicketClosed })
	out.Active, err = s.Store.CountTickets(ctx, all)
	return out, err
}

// Overview is the staff's view of the help desk.
type Overview struct {
	ByStatus     map[string]int `json:"by_status"`
	ByDepartment []DeptCount    `json:"by_department"`
	// Awaiting: tickets waiting for the operator's staff.
	Awaiting   int `json:"awaiting"`
	Unassigned int `json:"unassigned"`
	// The last 30 days: tickets opened and closed, and the average time
	// to the first reply (seconds; -1: no replies yet).
	Opened30d            int   `json:"opened_30d"`
	Closed30d            int   `json:"closed_30d"`
	AvgFirstResponseSecs int64 `json:"avg_first_response_seconds"`
}

// DeptCount is a department's tickets that aren't closed.
type DeptCount struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Active int    `json:"active"`
}

func (s *Service) Overview(ctx context.Context) (*Overview, error) {
	st, err := s.Store.SupportStats(ctx, s.now().Add(-30*24*time.Hour))
	if err != nil {
		return nil, err
	}
	out := &Overview{ByStatus: map[string]int{}, ByDepartment: []DeptCount{}, Opened30d: st.Opened, Closed30d: st.Closed,
		AvgFirstResponseSecs: -1}
	for _, status := range store.TicketStatuses {
		out.ByStatus[status] = st.ByStatus[status]
	}
	if st.Responded > 0 {
		out.AvgFirstResponseSecs = st.ResponseSecondSum / int64(st.Responded)
	}
	depts, err := s.Departments(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, d := range depts {
		out.ByDepartment = append(out.ByDepartment, DeptCount{ID: d.ID, Name: d.Name, Active: st.ByDepartment[d.ID]})
	}
	zero := int64(0)
	if out.Awaiting, err = s.Store.CountTickets(ctx, store.TicketFilter{AwaitingHandler: &zero}); err != nil {
		return nil, err
	}
	active := slices.DeleteFunc(slices.Clone(store.TicketStatuses), func(x string) bool { return x == store.TicketClosed })
	out.Unassigned, err = s.Store.CountTickets(ctx, store.TicketFilter{Statuses: active, AssignedUserID: -1})
	return out, err
}

// ---- Opening, replying, changing ----

// cleanText normalizes a message: line endings, no NULs or other control
// characters but tabs and line breaks, and no trailing blank space.
func cleanText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			return r
		}
		return -1
	}, s)
	return strings.TrimSpace(strings.ToValidUTF8(s, "�"))
}

func checkBody(body string, files int) (string, error) {
	body = cleanText(body)
	if body == "" && files == 0 {
		return "", fmt.Errorf("%w: write a message", ErrInvalid)
	}
	if utf8.RuneCountInString(body) > maxBody {
		return "", fmt.Errorf("%w: messages are up to %d characters", ErrInvalid, maxBody)
	}
	return body, nil
}

// OpenInput is a new ticket.
type OpenInput struct {
	// AccountID: staff open tickets on behalf of an account; a tenant's
	// ticket is always their own account's (0 or theirs).
	AccountID    int64  `json:"account_id"`
	DepartmentID int64  `json:"department_id"`
	SiteID       string `json:"site_id"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	Priority     string `json:"priority"`
}

// Open creates a ticket with its first message and attachments (staged
// files: Open takes them over, stored or discarded).
func (s *Service) Open(ctx context.Context, a Actor, in OpenInput, files []*Staged) (*Thread, error) {
	defer s.Discard(files)
	if err := s.CheckOpen(ctx, a); err != nil {
		return nil, err
	}
	st, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkFileCount(st, len(files)); err != nil {
		return nil, err
	}
	acctID := in.AccountID
	if !a.staff() {
		if acctID != 0 && acctID != a.AccountID {
			return nil, fmt.Errorf("%w: tickets are opened for your own account", ErrForbidden)
		}
		acctID = a.AccountID
	} else if acctID == 0 {
		return nil, fmt.Errorf("%w: choose the account the ticket is for", ErrInvalid)
	}
	acct, err := s.Store.GetAccount(ctx, acctID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: no account %d", ErrInvalid, acctID)
	} else if err != nil {
		return nil, err
	}
	if acct.Status == store.AccountTerminated {
		return nil, fmt.Errorf("%w: the account is terminated", ErrInvalid)
	}
	subject := strings.Join(strings.Fields(cleanText(in.Subject)), " ")
	if subject == "" || utf8.RuneCountInString(subject) > maxSubject {
		return nil, fmt.Errorf("%w: a subject (up to %d characters) says what it's about", ErrInvalid, maxSubject)
	}
	body, err := checkBody(in.Body, 0)
	if err != nil {
		return nil, err
	}
	prio := in.Priority
	if prio == "" {
		prio = "medium"
	}
	if !slices.Contains(Priorities, prio) {
		return nil, fmt.Errorf("%w: priority is low, medium, high or urgent", ErrInvalid)
	}
	dept, err := s.department(ctx, in.DepartmentID, a.staff())
	if err != nil {
		return nil, err
	}
	if in.SiteID != "" {
		if err := s.checkSite(ctx, acct, in.SiteID); err != nil {
			return nil, err
		}
	}
	now := s.now().UTC().Truncate(time.Second)
	t := &store.Ticket{AccountID: acct.ID, AccountName: acct.Name, UserID: a.UserID, OpenedBy: a.Name, DepartmentID: dept.ID,
		DepartmentName: dept.Name, SiteID: in.SiteID, Subject: subject, Status: store.TicketOpen, Priority: prio, CreatedAt: now}
	// A reseller's customers are the reseller's to answer (the store
	// derives who handles it), except what staff started: theirs.
	side := store.SideCustomer
	if a.staff() {
		// Staff writing first: the customer's turn.
		side, t.Status, t.FirstResponseAt, t.StaffOpened = store.SideStaff, store.TicketAnswered, now, true
	}
	m := &store.TicketMessage{UserID: a.UserID, Author: a.Name, Side: side, Body: body, CreatedAt: now}
	m.Attachments = attachmentRows(files, now)
	if err := s.Store.CreateTicket(ctx, t, m); err != nil {
		return nil, err
	}
	warning := s.keep(ctx, t.ID, files, m.Attachments)
	// As stored: who handles it.
	if t, err = s.Store.GetTicket(ctx, t.ID); err != nil {
		return nil, err
	}
	s.notifyOpened(ctx, t, dept, acct, m)
	th, err := s.Get(ctx, a, t.ID)
	if err != nil {
		return nil, err
	}
	th.Warning = warning
	return th, nil
}

// CheckOpen says whether a may open a ticket at all, before anything it
// uploads is read (Open checks again, with the rest).
func (s *Service) CheckOpen(ctx context.Context, a Actor) error {
	if a.ReadOnly {
		return fmt.Errorf("%w: your role can't open tickets", ErrForbidden)
	}
	if a.staff() {
		return nil
	}
	st, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	if !st.Enabled {
		return ErrDisabled
	}
	return nil
}

// CheckReply says whether a may write on a ticket, before anything it
// uploads is read (Reply checks again, with the rest).
func (s *Service) CheckReply(ctx context.Context, a Actor, id int64) error {
	if _, _, err := s.ticket(ctx, a, id); err != nil {
		return err
	}
	if a.ReadOnly {
		return fmt.Errorf("%w: your role can't reply", ErrForbidden)
	}
	return nil
}

func checkFileCount(st *Settings, n int) error {
	if n == 0 {
		return nil
	}
	if st.MaxFiles == 0 {
		return fmt.Errorf("%w: attachments are turned off", ErrInvalid)
	}
	if n > st.MaxFiles {
		return fmt.Errorf("%w: at most %d attachments per message", ErrTooLarge, st.MaxFiles)
	}
	return nil
}

// department returns the department a ticket goes to: id's, or the first
// visible one for 0. Customers can't choose a hidden one.
func (s *Service) department(ctx context.Context, id int64, staff bool) (*store.SupportDepartment, error) {
	if id == 0 {
		list, err := s.Departments(ctx, false)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("%w: no department takes tickets", ErrInvalid)
		}
		return list[0], nil
	}
	d, err := s.Store.GetSupportDepartment(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.Hidden && !staff) {
		return nil, fmt.Errorf("%w: no department %d", ErrInvalid, id)
	}
	return d, err
}

// checkSite: the site a ticket is about belongs to its account (or, for a
// reseller's own ticket, to one of its customers).
func (s *Service) checkSite(ctx context.Context, acct *store.Account, siteID string) error {
	bad := fmt.Errorf("%w: no site %q in this account", ErrInvalid, siteID)
	o, err := s.Store.SiteOwnerOf(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return bad
	} else if err != nil {
		return err
	}
	if o.AccountID == acct.ID {
		return nil
	}
	if acct.Kind == store.AccountReseller {
		owner, err := s.Store.GetAccount(ctx, o.AccountID)
		if err == nil && owner.ParentID == acct.ID {
			return nil
		}
	}
	return bad
}

// ReplyInput is a message on a ticket.
type ReplyInput struct {
	Body string `json:"body"`
	// Internal: a note between providers.
	Internal bool `json:"internal"`
	// StaffOnly (staff): a note the reseller of the ticket's account
	// doesn't see either. Implies Internal.
	StaffOnly bool `json:"staff_only"`
	// Status (providers): the ticket's status after this message ("":
	// answered for a reply, unchanged for a note). "closed" is Reply &
	// close.
	Status string `json:"status"`
}

// Reply adds a message (and attachments) to a ticket.
func (s *Service) Reply(ctx context.Context, a Actor, id int64, in ReplyInput, files []*Staged) (*Thread, error) {
	defer s.Discard(files)
	t, p, err := s.ticket(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if a.ReadOnly {
		return nil, fmt.Errorf("%w: your role can't reply", ErrForbidden)
	}
	st, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkFileCount(st, len(files)); err != nil {
		return nil, err
	}
	body, err := checkBody(in.Body, len(files))
	if err != nil {
		return nil, err
	}
	if in.StaffOnly {
		if p != PartyStaff {
			return nil, fmt.Errorf("%w: only the operator's staff write staff-only notes", ErrForbidden)
		}
		in.Internal = true
	}
	act := store.TicketActivity{Reply: !in.Internal}
	side := store.SideCustomer
	switch {
	case !p.provider():
		if in.Internal || in.Status != "" {
			return nil, fmt.Errorf("%w: customers write replies, not notes or status changes", ErrForbidden)
		}
		// A customer's reply reopens a closed ticket.
		act.Status = store.TicketCustomerReply
	default:
		side = store.SideStaff
		if p == PartyHandler {
			side = store.SideHandler
		}
		if in.Status != "" && !slices.Contains(store.TicketStatuses, in.Status) {
			return nil, fmt.Errorf("%w: status %q", ErrInvalid, in.Status)
		}
		act.Status, act.FirstResponse = in.Status, !in.Internal
		if act.Status == "" && !in.Internal {
			act.Status = store.TicketAnswered
		}
		// A note marking it answered starts the customer's turn, and the
		// auto-close clock, as a reply would.
		if act.Status == store.TicketAnswered {
			act.Reply = true
		}
	}
	now := s.now().UTC().Truncate(time.Second)
	m := &store.TicketMessage{TicketID: t.ID, UserID: a.UserID, Author: a.Name, Side: side, Internal: in.Internal, StaffOnly: in.StaffOnly, Body: body,
		CreatedAt: now, Attachments: attachmentRows(files, now)}
	if err := s.Store.AddTicketMessage(ctx, m, act); err != nil {
		return nil, err
	}
	warning := s.keep(ctx, t.ID, files, m.Attachments)
	if !in.Internal {
		if act.Status != "" {
			t.Status = act.Status
		}
		if side == store.SideCustomer {
			s.notifyCustomerReply(ctx, t, m)
		} else {
			s.notifyReply(ctx, t, m)
		}
	}
	th, err := s.Get(ctx, a, t.ID)
	if err != nil {
		return nil, err
	}
	th.Warning = warning
	return th, nil
}

// UpdateInput changes a ticket; nil fields stay. Customers may only close
// and reopen (status closed or open).
type UpdateInput struct {
	Status         *string `json:"status"`
	Priority       *string `json:"priority"`
	DepartmentID   *int64  `json:"department_id"`
	AssignedUserID *int64  `json:"assigned_user_id"`
}

// Update changes a ticket's status, priority, department or assignee,
// noting each change in the conversation (closing and reopening where
// the customer sees it, the rest for providers only).
func (s *Service) Update(ctx context.Context, a Actor, id int64, in UpdateInput) (*Thread, error) {
	t, p, err := s.ticket(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if a.ReadOnly {
		return nil, fmt.Errorf("%w: your role can't change tickets", ErrForbidden)
	}
	// Only what this change sets is written, and the status only if it's
	// still the one read (see store.TicketChanges).
	c := store.TicketChanges{FromStatus: t.Status, At: s.now().UTC().Truncate(time.Second)}
	var public, private []string
	if in.Status != nil && *in.Status != t.Status {
		next := *in.Status
		if !slices.Contains(store.TicketStatuses, next) {
			return nil, fmt.Errorf("%w: status %q", ErrInvalid, next)
		}
		if !p.provider() && next != store.TicketClosed && !(next == store.TicketOpen && t.Status == store.TicketClosed) {
			return nil, fmt.Errorf("%w: you can close your ticket, or reopen it once closed", ErrForbidden)
		}
		switch {
		case next == store.TicketClosed:
			public = append(public, a.Name+" closed the ticket.")
		case t.Status == store.TicketClosed:
			public = append(public, a.Name+" reopened the ticket.")
		default:
			private = append(private, fmt.Sprintf("%s set the status to %s.", a.Name, strings.ReplaceAll(next, "_", " ")))
		}
		c.Status = &next
	}
	if !p.provider() && (in.Priority != nil || in.DepartmentID != nil || in.AssignedUserID != nil) {
		return nil, fmt.Errorf("%w: only your provider changes the priority, department or assignee", ErrForbidden)
	}
	if in.Priority != nil && *in.Priority != t.Priority {
		if !slices.Contains(Priorities, *in.Priority) {
			return nil, fmt.Errorf("%w: priority is low, medium, high or urgent", ErrInvalid)
		}
		c.Priority = in.Priority
		private = append(private, fmt.Sprintf("%s set the priority to %s.", a.Name, *in.Priority))
	}
	if in.DepartmentID != nil && *in.DepartmentID != t.DepartmentID {
		if *in.DepartmentID == 0 {
			return nil, fmt.Errorf("%w: choose a department", ErrInvalid)
		}
		d, err := s.department(ctx, *in.DepartmentID, p == PartyStaff)
		if err != nil {
			return nil, err
		}
		c.DepartmentID = &d.ID
		private = append(private, fmt.Sprintf("%s moved the ticket to %s.", a.Name, d.Name))
	}
	if in.AssignedUserID != nil && *in.AssignedUserID != t.AssignedUserID {
		if p != PartyStaff {
			return nil, fmt.Errorf("%w: only the operator's staff are assigned tickets", ErrForbidden)
		}
		who := "nobody"
		if *in.AssignedUserID != 0 {
			u, err := s.Store.GetUser(ctx, *in.AssignedUserID)
			if err != nil || u.AccountID != 0 || auth.IsTenant(u.Role) || u.Disabled {
				return nil, fmt.Errorf("%w: assign the ticket to a member of staff", ErrInvalid)
			}
			who = u.Username
		}
		c.AssignedUserID = in.AssignedUserID
		private = append(private, fmt.Sprintf("%s assigned the ticket to %s.", a.Name, who))
	}
	if len(public)+len(private) == 0 {
		return s.Get(ctx, a, id)
	}
	// One note for what the customer may see, one for the rest.
	var notes []*store.TicketMessage
	if len(public) > 0 {
		notes = append(notes, &store.TicketMessage{Author: a.Name, UserID: a.UserID, Side: store.SideSystem, Body: strings.Join(public, " "), CreatedAt: c.At})
	}
	if len(private) > 0 {
		notes = append(notes, &store.TicketMessage{Author: a.Name, UserID: a.UserID, Side: store.SideSystem, Internal: true, Body: strings.Join(private, " "), CreatedAt: c.At})
	}
	c.Notes = notes
	if err := s.Store.UpdateTicket(ctx, id, c); errors.Is(err, store.ErrConflict) {
		return nil, fmt.Errorf("%w: the ticket changed meanwhile (a reply came in?); look again and retry", ErrConflict)
	} else if err != nil {
		return nil, err
	}
	// The provider closing it tells the customer; the customer closing it
	// needs no e-mail.
	if c.Status != nil && *c.Status == store.TicketClosed && p.provider() {
		t.Status, t.ClosedAt = *c.Status, c.At
		s.notifyClosed(ctx, t, false, 0)
	}
	return s.Get(ctx, a, id)
}

// Escalate hands a reseller's customer's ticket to the operator's staff.
// The reseller stays on it (it sees and may still answer it).
func (s *Service) Escalate(ctx context.Context, a Actor, id int64, reason string) (*Thread, error) {
	t, p, err := s.ticket(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if p != PartyHandler {
		return nil, fmt.Errorf("%w: only the reseller handling a ticket escalates it", ErrForbidden)
	}
	if t.HandlerAccountID != a.AccountID {
		return nil, fmt.Errorf("%w: this ticket is already with the operator's staff", ErrConflict)
	}
	reason = cleanText(reason)
	if utf8.RuneCountInString(reason) > 2000 {
		return nil, fmt.Errorf("%w: the reason is up to 2000 characters", ErrInvalid)
	}
	now := s.now().UTC().Truncate(time.Second)
	body := a.Name + " escalated the ticket to the operator's staff."
	if reason != "" {
		body += "\n\n" + reason
	}
	note := &store.TicketMessage{UserID: a.UserID, Author: a.Name, Side: store.SideSystem, Internal: true, Body: body, CreatedAt: now}
	if err := s.Store.EscalateTicket(ctx, id, now, note); errors.Is(err, store.ErrConflict) {
		return nil, fmt.Errorf("%w: this ticket is already with the operator's staff", ErrConflict)
	} else if err != nil {
		return nil, err
	}
	t.HandlerAccountID, t.Escalated, t.EscalatedAt = 0, true, now
	s.notifyEscalated(ctx, t, a.Name, reason)
	return s.Get(ctx, a, id)
}
