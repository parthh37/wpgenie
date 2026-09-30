package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Support tickets (internal/support): departments, tickets, their
// messages and attachments' records (the files are on disk), and canned
// replies. Who may see and do what is internal/support's; this file only
// stores it.
//
// A ticket belongs to the account that opened it (account_id). Who
// handles it is derived when it's read, never stored: the account's
// current reseller (see handlerExpr), or the operator's staff when the
// account has none (any more), the ticket was escalated or staff opened
// it. A customer moved to another reseller takes its tickets along; one
// detached, or whose reseller is gone, is the staff's. Messages
// are the customer's, the provider's (a handler's or staff's) or the
// panel's own (system); internal ones are notes between providers.
// Previews in lists only ever come from public messages.
const supportSchema = `CREATE TABLE support_departments (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT NOT NULL,
		description  TEXT NOT NULL DEFAULT '',
		notify_email TEXT NOT NULL DEFAULT '',
		hidden       INTEGER NOT NULL DEFAULT 0,
		sort         INTEGER NOT NULL DEFAULT 0,
		created_at   INTEGER NOT NULL
	);
	CREATE TABLE tickets (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		mask               TEXT NOT NULL UNIQUE,
		account_id         INTEGER NOT NULL,
		user_id            INTEGER NOT NULL DEFAULT 0,
		opened_by          TEXT NOT NULL DEFAULT '',
		escalated          INTEGER NOT NULL DEFAULT 0,
		escalated_at       INTEGER NOT NULL DEFAULT 0,
		staff_opened       INTEGER NOT NULL DEFAULT 0,
		department_id      INTEGER NOT NULL,
		site_id            TEXT NOT NULL DEFAULT '',
		subject            TEXT NOT NULL,
		status             TEXT NOT NULL,
		priority           TEXT NOT NULL,
		assigned_user_id   INTEGER NOT NULL DEFAULT 0,
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL,
		last_reply_at      INTEGER NOT NULL,
		first_response_at  INTEGER NOT NULL DEFAULT 0,
		closed_at          INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX tickets_by_account ON tickets (account_id, updated_at);
	CREATE INDEX tickets_by_status ON tickets (status, last_reply_at);
	CREATE INDEX tickets_by_update ON tickets (updated_at, id);
	CREATE INDEX tickets_by_department ON tickets (department_id, status);
	CREATE INDEX tickets_by_assignee ON tickets (assigned_user_id, status);
	CREATE TABLE ticket_messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id  INTEGER NOT NULL,
		user_id    INTEGER NOT NULL DEFAULT 0,
		author     TEXT NOT NULL,
		side       TEXT NOT NULL,
		internal   INTEGER NOT NULL DEFAULT 0,
		body       TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX ticket_messages_by_ticket ON ticket_messages (ticket_id, id);
	CREATE TABLE ticket_attachments (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id    INTEGER NOT NULL,
		message_id   INTEGER NOT NULL,
		name         TEXT NOT NULL,
		file         TEXT NOT NULL,
		content_type TEXT NOT NULL,
		size         INTEGER NOT NULL,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX ticket_attachments_by_ticket ON ticket_attachments (ticket_id, message_id);
	CREATE TABLE support_canned (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		title      TEXT NOT NULL,
		body       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`

// Ticket statuses.
const (
	TicketOpen          = "open"           // new, nobody answered yet
	TicketAnswered      = "answered"       // the provider answered: the customer's turn
	TicketCustomerReply = "customer_reply" // the customer answered: the provider's turn
	TicketInProgress    = "in_progress"
	TicketOnHold        = "on_hold"
	TicketClosed        = "closed"
)

// TicketStatuses lists them in the order the panel shows them.
var TicketStatuses = []string{TicketOpen, TicketCustomerReply, TicketInProgress, TicketOnHold, TicketAnswered, TicketClosed}

// Who wrote a ticket message.
const (
	SideCustomer = "customer" // a user of the ticket's account
	SideHandler  = "handler"  // the account's reseller
	SideStaff    = "staff"    // the operator's staff
	SideSystem   = "system"   // the panel (closed automatically, escalated…)
)

// SupportDepartment is where tickets go (Sales, Technical…).
type SupportDepartment struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// NotifyEmail also receives new tickets and replies of this
	// department ("": only the settings' recipients).
	NotifyEmail string `json:"notify_email,omitempty"`
	// Hidden departments aren't offered to customers (staff can still
	// move tickets there).
	Hidden bool `json:"hidden"`
	Sort   int  `json:"sort"`
	// Active counts the department's tickets that aren't closed.
	Active    int       `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// Ticket is a support request and its state. AccountName, DepartmentName,
// AssignedTo and Preview are read from other tables.
type Ticket struct {
	ID          int64
	Mask        string
	AccountID   int64
	AccountName string
	UserID      int64
	OpenedBy    string
	// HandlerAccountID is who handles it now (derived: see handlerExpr):
	// the account's reseller, or 0 for the operator's staff.
	HandlerAccountID int64
	// Escalated: its reseller handed it to staff; StaffOpened: staff
	// opened it. Either keeps it with staff whatever the account's reseller.
	Escalated       bool
	StaffOpened     bool
	EscalatedAt     time.Time
	DepartmentID    int64
	DepartmentName  string
	SiteID          string
	Subject         string
	Status          string
	Priority        string
	AssignedUserID  int64
	AssignedTo      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastReplyAt     time.Time
	FirstResponseAt time.Time
	ClosedAt        time.Time
	// Preview is the start of the last public message.
	Preview string
}

// TicketMessage is a message on a ticket (Attachments filled by the
// reads that say so).
type TicketMessage struct {
	ID          int64
	TicketID    int64
	UserID      int64
	Author      string
	Side        string
	Internal    bool
	Body        string
	CreatedAt   time.Time
	Attachments []*TicketAttachment
}

// TicketAttachment is a file attached to a message. File is its name on
// disk (random, never the uploader's); Name is the uploader's, for display.
type TicketAttachment struct {
	ID          int64
	TicketID    int64
	MessageID   int64
	Name        string
	File        string
	ContentType string
	Size        int64
	CreatedAt   time.Time
}

// CannedReply is a saved answer staff insert into replies.
type CannedReply struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ---- Departments ----

const departmentCols = `d.id, d.name, d.description, d.notify_email, d.hidden, d.sort, d.created_at,
	(SELECT COUNT(*) FROM tickets t WHERE t.department_id = d.id AND t.status <> 'closed')`

func scanDepartment(row interface{ Scan(...any) error }) (*SupportDepartment, error) {
	var d SupportDepartment
	var created int64
	err := row.Scan(&d.ID, &d.Name, &d.Description, &d.NotifyEmail, &d.Hidden, &d.Sort, &created, &d.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	return &d, nil
}

// SupportDepartments lists every department, in their order.
func (s *Store) SupportDepartments(ctx context.Context) ([]*SupportDepartment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+departmentCols+` FROM support_departments d ORDER BY d.sort, d.name, d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SupportDepartment{}
	for rows.Next() {
		d, err := scanDepartment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetSupportDepartment(ctx context.Context, id int64) (*SupportDepartment, error) {
	return scanDepartment(s.db.QueryRowContext(ctx, `SELECT `+departmentCols+` FROM support_departments d WHERE d.id = ?`, id))
}

func (s *Store) CreateSupportDepartment(ctx context.Context, d *SupportDepartment) error {
	now := time.Now()
	err := s.db.QueryRowContext(ctx, `INSERT INTO support_departments (name, description, notify_email, hidden, sort, created_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, d.Name, d.Description, d.NotifyEmail, d.Hidden, d.Sort, now.Unix()).Scan(&d.ID)
	if err == nil {
		d.CreatedAt = now.UTC().Truncate(time.Second)
	}
	return err
}

func (s *Store) UpdateSupportDepartment(ctx context.Context, d *SupportDepartment) error {
	return s.exec1(ctx, `UPDATE support_departments SET name = ?, description = ?, notify_email = ?, hidden = ?, sort = ?
		WHERE id = ?`, d.Name, d.Description, d.NotifyEmail, d.Hidden, d.Sort, d.ID)
}

// DeleteSupportDepartment removes a department no ticket is in (ErrInUse
// otherwise: hide it instead).
func (s *Store) DeleteSupportDepartment(ctx context.Context, id int64) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE department_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrInUse
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM support_departments WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---- Tickets ----

// handlerExpr is the account handling a ticket: its account's reseller
// (pa, joined by ticketFrom) while that reseller exists and isn't
// terminated, unless the ticket was escalated or opened by staff; else 0,
// the operator's staff.
const handlerExpr = `(CASE WHEN t.escalated = 1 OR t.staff_opened = 1 OR pa.id IS NULL OR pa.status = 'terminated'
	THEN 0 ELSE pa.id END)`

const ticketCols = `t.id, t.mask, t.account_id, COALESCE(a.name, ''), t.user_id, t.opened_by, ` + handlerExpr + `,
	t.escalated, t.staff_opened, t.escalated_at, t.department_id, COALESCE(d.name, ''), t.site_id, t.subject, t.status, t.priority, t.assigned_user_id,
	COALESCE(u.username, ''), t.created_at, t.updated_at, t.last_reply_at, t.first_response_at, t.closed_at,
	COALESCE((SELECT SUBSTR(m.body, 1, 240) FROM ticket_messages m WHERE m.ticket_id = t.id AND m.internal = 0
		AND m.side <> 'system' ORDER BY m.id DESC LIMIT 1), '')`

const ticketJoins = ` LEFT JOIN accounts a ON a.id = t.account_id LEFT JOIN accounts pa ON pa.id = a.parent_id`

const ticketFrom = ` FROM tickets t` + ticketJoins + `
	LEFT JOIN support_departments d ON d.id = t.department_id LEFT JOIN users u ON u.id = t.assigned_user_id`

func scanTicket(row interface{ Scan(...any) error }) (*Ticket, error) {
	var t Ticket
	var escalated, created, updated, lastReply, firstResponse, closed int64
	err := row.Scan(&t.ID, &t.Mask, &t.AccountID, &t.AccountName, &t.UserID, &t.OpenedBy, &t.HandlerAccountID, &t.Escalated,
		&t.StaffOpened, &escalated,
		&t.DepartmentID, &t.DepartmentName, &t.SiteID, &t.Subject, &t.Status, &t.Priority, &t.AssignedUserID, &t.AssignedTo,
		&created, &updated, &lastReply, &firstResponse, &closed, &t.Preview)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.EscalatedAt, t.FirstResponseAt, t.ClosedAt = unixTime(escalated), unixTime(firstResponse), unixTime(closed)
	t.CreatedAt, t.UpdatedAt, t.LastReplyAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC(), time.Unix(lastReply, 0).UTC()
	return &t, nil
}

// ticketMask is a ticket's public reference: WPG- and six digits.
func ticketMask() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("WPG-%06d", n.Int64()+100000), nil
}

// CreateTicket inserts a ticket with its first message (and that
// message's attachments), and gives it a unique mask. t and first get
// their IDs; t its mask.
func (s *Store) CreateTicket(ctx context.Context, t *Ticket, first *TicketMessage) error {
	for attempt := 0; ; attempt++ {
		mask, err := ticketMask()
		if err != nil {
			return err
		}
		err = s.db.inTx(ctx, func(tx *Tx) error {
			at := t.CreatedAt.Unix()
			if err := tx.QueryRowContext(ctx, `INSERT INTO tickets (mask, account_id, user_id, opened_by, staff_opened,
				department_id, site_id, subject, status, priority, assigned_user_id, created_at, updated_at, last_reply_at,
				first_response_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, mask, t.AccountID, t.UserID,
				t.OpenedBy, t.StaffOpened, t.DepartmentID, t.SiteID, t.Subject, t.Status, t.Priority, t.AssignedUserID,
				at, at, at, unixOrZero(t.FirstResponseAt)).Scan(&t.ID); err != nil {
				return err
			}
			first.TicketID = t.ID
			return insertMessage(ctx, tx, first)
		})
		// Another ticket drew the same mask: draw again.
		if isUnique(err) && attempt < 5 {
			continue
		}
		if err != nil {
			return err
		}
		t.Mask = mask
		t.UpdatedAt, t.LastReplyAt = t.CreatedAt, t.CreatedAt
		return nil
	}
}

func insertMessage(ctx context.Context, tx *Tx, m *TicketMessage) error {
	if err := tx.QueryRowContext(ctx, `INSERT INTO ticket_messages (ticket_id, user_id, author, side, internal, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`, m.TicketID, m.UserID, m.Author, m.Side, m.Internal, m.Body,
		m.CreatedAt.Unix()).Scan(&m.ID); err != nil {
		return err
	}
	for _, a := range m.Attachments {
		a.TicketID, a.MessageID, a.CreatedAt = m.TicketID, m.ID, m.CreatedAt
		if err := tx.QueryRowContext(ctx, `INSERT INTO ticket_attachments (ticket_id, message_id, name, file, content_type,
			size, created_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`, a.TicketID, a.MessageID, a.Name, a.File,
			a.ContentType, a.Size, a.CreatedAt.Unix()).Scan(&a.ID); err != nil {
			return err
		}
	}
	return nil
}

// TicketActivity is what a new message changes on its ticket.
type TicketActivity struct {
	// Status is the ticket's new status ("": unchanged); closing sets
	// closed_at, any other status clears it.
	Status string
	// Reply: a public reply, which moves last_reply_at.
	Reply bool
	// FirstResponse: the provider's reply; the first one is recorded.
	FirstResponse bool
}

// AddTicketMessage adds a message (and its attachments) to a ticket and
// updates the ticket, in one transaction.
func (s *Store) AddTicketMessage(ctx context.Context, m *TicketMessage, act TicketActivity) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		at := m.CreatedAt.Unix()
		// The ticket's row lock orders concurrent replies.
		if err := touchTicket(ctx, tx, m.TicketID, at); err != nil {
			return err
		}
		if err := insertMessage(ctx, tx, m); err != nil {
			return err
		}
		if act.Status != "" {
			var closed int64
			if act.Status == TicketClosed {
				closed = at
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = ?, closed_at = ? WHERE id = ?`,
				act.Status, closed, m.TicketID); err != nil {
				return err
			}
		}
		if act.Reply {
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET last_reply_at = ? WHERE id = ?`, at, m.TicketID); err != nil {
				return err
			}
		}
		if act.FirstResponse {
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET first_response_at = ? WHERE id = ? AND first_response_at = 0`,
				at, m.TicketID); err != nil {
				return err
			}
		}
		return nil
	})
}

func touchTicket(ctx context.Context, tx *Tx, id, at int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE tickets SET updated_at = ? WHERE id = ?`, at, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetTicket(ctx context.Context, id int64) (*Ticket, error) {
	return scanTicket(s.db.QueryRowContext(ctx, `SELECT `+ticketCols+ticketFrom+` WHERE t.id = ?`, id))
}

// TicketChanges are the fields a change sets (nil: unchanged), with the
// messages saying so. Only what changed is written: a change never undoes
// what happened to the ticket since it was read.
type TicketChanges struct {
	// Status moves the ticket from FromStatus (the status the change was
	// decided on) to Status; the ticket having moved on meanwhile is
	// ErrConflict, and nothing is written. Closing sets closed_at; any
	// other status clears it.
	Status         *string
	FromStatus     string
	Priority       *string
	DepartmentID   *int64
	AssignedUserID *int64
	At             time.Time
	Notes          []*TicketMessage
}

// UpdateTicket applies a change to a ticket, in one transaction.
func (s *Store) UpdateTicket(ctx context.Context, id int64, c TicketChanges) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		at := c.At.Unix()
		if err := touchTicket(ctx, tx, id, at); err != nil {
			return err
		}
		if c.Status != nil {
			var closed int64
			if *c.Status == TicketClosed {
				closed = at
			}
			res, err := tx.ExecContext(ctx, `UPDATE tickets SET status = ?, closed_at = ? WHERE id = ? AND status = ?`,
				*c.Status, closed, id, c.FromStatus)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrConflict
			}
		}
		if c.Priority != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET priority = ? WHERE id = ?`, *c.Priority, id); err != nil {
				return err
			}
		}
		if c.DepartmentID != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET department_id = ? WHERE id = ?`, *c.DepartmentID, id); err != nil {
				return err
			}
		}
		if c.AssignedUserID != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET assigned_user_id = ? WHERE id = ?`, *c.AssignedUserID, id); err != nil {
				return err
			}
		}
		for _, m := range c.Notes {
			m.TicketID = id
			if err := insertMessage(ctx, tx, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// EscalateTicket hands a ticket to the operator's staff for good, with a
// note (ErrConflict: it already is theirs, escalated or opened by staff).
func (s *Store) EscalateTicket(ctx context.Context, id int64, at time.Time, note *TicketMessage) error {
	return s.db.inTx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE tickets SET escalated = 1, escalated_at = ?, updated_at = ?
			WHERE id = ? AND escalated = 0 AND staff_opened = 0`, at.Unix(), at.Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrConflict
		}
		note.TicketID = id
		return insertMessage(ctx, tx, note)
	})
}

// AutoCloseTicket closes a ticket that is still answered with no reply
// since cutoff, adding note; false: it changed in the meantime (someone
// replied, or another run closed it).
func (s *Store) AutoCloseTicket(ctx context.Context, id int64, cutoff time.Time, note *TicketMessage) (bool, error) {
	closed := false
	err := s.db.inTx(ctx, func(tx *Tx) error {
		closed = false
		at := note.CreatedAt.Unix()
		res, err := tx.ExecContext(ctx, `UPDATE tickets SET status = ?, closed_at = ?, updated_at = ?
			WHERE id = ? AND status = ? AND last_reply_at <= ?`, TicketClosed, at, at, id, TicketAnswered, cutoff.Unix())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		note.TicketID = id
		if err := insertMessage(ctx, tx, note); err != nil {
			return err
		}
		closed = true
		return nil
	})
	return closed, err
}

// TicketFilter selects tickets; zero values don't filter.
type TicketFilter struct {
	// AccountIDs: only these accounts' tickets (nil: every account's;
	// empty: none).
	AccountIDs   []int64
	AccountID    int64
	Statuses     []string
	DepartmentID int64
	Priority     string
	// AssignedUserID: -1 unassigned tickets, > 0 that user's.
	AssignedUserID int64
	// Awaiting keeps tickets waiting for a reply from a handler
	// (AwaitingHandler: open or customer_reply, handled by that account,
	// 0 the operator) or from a customer account (AwaitingCustomer:
	// answered). With both, either.
	AwaitingHandler  *int64
	AwaitingCustomer int64
	// LastReplyBefore: no public reply since.
	LastReplyBefore time.Time
	// Query matches the subject, the mask or the account's name.
	Query string
	// Before pages: tickets after this one in the order (last activity
	// first).
	Before int64
	Limit  int
}

// where builds the filter's WHERE clause from fixed fragments.
func (f TicketFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.AccountIDs != nil {
		if len(f.AccountIDs) == 0 {
			return " WHERE 1 = 0", nil
		}
		conds = append(conds, `t.account_id IN (`+placeholders(len(f.AccountIDs))+`)`)
		for _, id := range f.AccountIDs {
			args = append(args, id)
		}
	}
	if f.AccountID != 0 {
		conds, args = append(conds, `t.account_id = ?`), append(args, f.AccountID)
	}
	if len(f.Statuses) > 0 {
		conds = append(conds, `t.status IN (`+placeholders(len(f.Statuses))+`)`)
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.DepartmentID != 0 {
		conds, args = append(conds, `t.department_id = ?`), append(args, f.DepartmentID)
	}
	if f.Priority != "" {
		conds, args = append(conds, `t.priority = ?`), append(args, f.Priority)
	}
	switch {
	case f.AssignedUserID < 0:
		conds = append(conds, `t.assigned_user_id = 0`)
	case f.AssignedUserID > 0:
		conds, args = append(conds, `t.assigned_user_id = ?`), append(args, f.AssignedUserID)
	}
	var awaiting []string
	if f.AwaitingHandler != nil {
		awaiting = append(awaiting, `(`+handlerExpr+` = ? AND t.status IN (?, ?))`)
		args = append(args, *f.AwaitingHandler, TicketOpen, TicketCustomerReply)
	}
	if f.AwaitingCustomer != 0 {
		awaiting = append(awaiting, `(t.account_id = ? AND t.status = ?)`)
		args = append(args, f.AwaitingCustomer, TicketAnswered)
	}
	if len(awaiting) > 0 {
		conds = append(conds, `(`+strings.Join(awaiting, ` OR `)+`)`)
	}
	if !f.LastReplyBefore.IsZero() {
		conds, args = append(conds, `t.last_reply_at <= ?`), append(args, f.LastReplyBefore.Unix())
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		conds = append(conds, `(t.subject LIKE ? ESCAPE '\' OR t.mask LIKE ? ESCAPE '\' OR a.name LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if f.Before != 0 {
		conds = append(conds, `(t.updated_at < (SELECT b.updated_at FROM tickets b WHERE b.id = ?)
			OR (t.updated_at = (SELECT b.updated_at FROM tickets b WHERE b.id = ?) AND t.id < ?))`)
		args = append(args, f.Before, f.Before, f.Before)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListTickets lists tickets, last activity first.
func (s *Store) ListTickets(ctx context.Context, f TicketFilter) ([]*Ticket, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, `SELECT `+ticketCols+ticketFrom+where+` ORDER BY t.updated_at DESC, t.id DESC LIMIT ?`,
		append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountTickets counts the tickets a filter selects (Before and Limit
// ignored).
func (s *Store) CountTickets(ctx context.Context, f TicketFilter) (int, error) {
	f.Before = 0
	where, args := f.where()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets t`+ticketJoins+where,
		args...).Scan(&n)
	return n, err
}

// TicketMessages returns a ticket's messages, oldest first, with their
// attachments.
func (s *Store) TicketMessages(ctx context.Context, ticketID int64) ([]*TicketMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, ticket_id, user_id, author, side, internal, body, created_at
		FROM ticket_messages WHERE ticket_id = ? ORDER BY id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TicketMessage{}
	byID := map[int64]*TicketMessage{}
	for rows.Next() {
		var m TicketMessage
		var created int64
		if err := rows.Scan(&m.ID, &m.TicketID, &m.UserID, &m.Author, &m.Side, &m.Internal, &m.Body, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(created, 0).UTC()
		m.Attachments = []*TicketAttachment{}
		out = append(out, &m)
		byID[m.ID] = &m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	atts, err := s.TicketAttachments(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		if m, ok := byID[a.MessageID]; ok {
			m.Attachments = append(m.Attachments, a)
		}
	}
	return out, nil
}

const attachmentCols = `id, ticket_id, message_id, name, file, content_type, size, created_at`

func scanAttachment(row interface{ Scan(...any) error }) (*TicketAttachment, error) {
	var a TicketAttachment
	var created int64
	err := row.Scan(&a.ID, &a.TicketID, &a.MessageID, &a.Name, &a.File, &a.ContentType, &a.Size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	return &a, nil
}

// TicketAttachments lists a ticket's attachments, in the order they came.
func (s *Store) TicketAttachments(ctx context.Context, ticketID int64) ([]*TicketAttachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+attachmentCols+` FROM ticket_attachments WHERE ticket_id = ? ORDER BY id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TicketAttachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetTicketAttachment returns an attachment of a ticket (ErrNotFound if
// it is another ticket's).
func (s *Store) GetTicketAttachment(ctx context.Context, ticketID, id int64) (*TicketAttachment, error) {
	return scanAttachment(s.db.QueryRowContext(ctx, `SELECT `+attachmentCols+` FROM ticket_attachments
		WHERE id = ? AND ticket_id = ?`, id, ticketID))
}

// GetTicketMessage returns a message of a ticket.
func (s *Store) GetTicketMessage(ctx context.Context, ticketID, id int64) (*TicketMessage, error) {
	var m TicketMessage
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, ticket_id, user_id, author, side, internal, body, created_at
		FROM ticket_messages WHERE id = ? AND ticket_id = ?`, id, ticketID).Scan(&m.ID, &m.TicketID, &m.UserID, &m.Author,
		&m.Side, &m.Internal, &m.Body, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = time.Unix(created, 0).UTC()
	return &m, nil
}

// DeleteTicketAttachments forgets attachments (whose files couldn't be
// stored).
func (s *Store) DeleteTicketAttachments(ctx context.Context, ids ...int64) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM ticket_attachments WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// SupportStats are the numbers of the staff's support overview.
type SupportStats struct {
	// ByStatus counts every ticket by status.
	ByStatus map[string]int
	// ByDepartment counts tickets that aren't closed by department ID.
	ByDepartment map[int64]int
	// Since the period's start: tickets opened and closed, and the
	// first responses to tickets opened (their count and total seconds).
	Opened, Closed    int
	Responded         int
	ResponseSecondSum int64
}

// SupportStats counts tickets, and those of the period from since.
func (s *Store) SupportStats(ctx context.Context, since time.Time) (*SupportStats, error) {
	st := &SupportStats{ByStatus: map[string]int{}, ByDepartment: map[int64]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT status, department_id, COUNT(*) FROM tickets GROUP BY status, department_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var dept int64
		var n int
		if err := rows.Scan(&status, &dept, &n); err != nil {
			return nil, err
		}
		st.ByStatus[status] += n
		if status != TicketClosed {
			st.ByDepartment[dept] += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	from := since.Unix()
	err = s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM tickets WHERE created_at >= ?),
		(SELECT COUNT(*) FROM tickets WHERE closed_at >= ?),
		(SELECT COUNT(*) FROM tickets WHERE created_at >= ? AND first_response_at > 0),
		(SELECT COALESCE(SUM(first_response_at - created_at), 0) FROM tickets WHERE created_at >= ? AND first_response_at > 0)`,
		from, from, from, from).Scan(&st.Opened, &st.Closed, &st.Responded, &st.ResponseSecondSum)
	return st, err
}

// ---- Canned replies ----

// CannedReplies lists the saved replies by title.
func (s *Store) CannedReplies(ctx context.Context) ([]*CannedReply, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, body, created_at, updated_at FROM support_canned ORDER BY title, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CannedReply{}
	for rows.Next() {
		var c CannedReply
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.Title, &c.Body, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *Store) CreateCannedReply(ctx context.Context, c *CannedReply) error {
	now := time.Now().UTC().Truncate(time.Second)
	err := s.db.QueryRowContext(ctx, `INSERT INTO support_canned (title, body, created_at, updated_at) VALUES (?, ?, ?, ?)
		RETURNING id`, c.Title, c.Body, now.Unix(), now.Unix()).Scan(&c.ID)
	if err == nil {
		c.CreatedAt, c.UpdatedAt = now, now
	}
	return err
}

func (s *Store) UpdateCannedReply(ctx context.Context, c *CannedReply) error {
	c.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	return s.exec1(ctx, `UPDATE support_canned SET title = ?, body = ?, updated_at = ? WHERE id = ?`,
		c.Title, c.Body, c.UpdatedAt.Unix(), c.ID)
}

func (s *Store) DeleteCannedReply(ctx context.Context, id int64) error {
	return s.exec1(ctx, `DELETE FROM support_canned WHERE id = ?`, id)
}
