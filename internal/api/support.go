package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/support"
)

// Support tickets (internal/support). Tenants reach a ticket only when it
// is their account's or, for a reseller, one of their customers'
// (registerScope below); what each party may do on it is the support
// service's. Opening, replying and closing work while an account is
// suspended: asking why is what support is for.

func init() {
	registerErrorStatus(support.ErrInvalid, http.StatusBadRequest)
	registerErrorStatus(support.ErrForbidden, http.StatusForbidden)
	registerErrorStatus(support.ErrDisabled, http.StatusForbidden)
	registerErrorStatus(support.ErrConflict, http.StatusConflict)
	registerErrorStatus(support.ErrTooLarge, http.StatusRequestEntityTooLarge)
	suspendedToo := tenantRule{whileSuspended: true}
	registerTenantRoutes(map[string]tenantRule{
		"GET /api/v1/tickets":                        anyTenant,
		"POST /api/v1/tickets":                       suspendedToo,
		"GET /api/v1/tickets/{id}":                   anyTenant,
		"PUT /api/v1/tickets/{id}":                   suspendedToo, // customers: close and reopen
		"POST /api/v1/tickets/{id}/replies":          suspendedToo,
		"POST /api/v1/tickets/{id}/escalate":         {reseller: true, whileSuspended: true},
		"GET /api/v1/tickets/{id}/attachments/{att}": anyTenant,
		"GET /api/v1/support/departments":            anyTenant, // the visible ones
		"GET /api/v1/support/summary":                anyTenant,
	})
	registerScope("/api/v1/tickets/{id}", func(s *Server, ctx context.Context, p *Principal, id string) bool {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 {
			return false
		}
		t, err := s.Store.GetTicket(ctx, n)
		if err != nil {
			return false
		}
		_, ok := s.accountInScope(ctx, p, t.AccountID)
		return ok
	})
}

func (s *Server) supportRoutes(mux *http.ServeMux, r func(string, string, handlerFunc)) {
	r("GET /api/v1/tickets", viewer, s.listTickets)
	r("POST /api/v1/tickets", operator, s.openTicket)
	r("GET /api/v1/tickets/{id}", viewer, s.getTicket)
	r("PUT /api/v1/tickets/{id}", operator, s.updateTicket)
	r("POST /api/v1/tickets/{id}/replies", operator, s.replyTicket)
	r("POST /api/v1/tickets/{id}/escalate", operator, s.escalateTicket)
	r("GET /api/v1/tickets/{id}/attachments/{att}", viewer, s.ticketAttachment)
	r("GET /api/v1/support/summary", viewer, s.supportSummary)
	r("GET /api/v1/support/overview", viewer, s.supportOverview)
	r("GET /api/v1/support/agents", viewer, s.supportAgents)
	r("GET /api/v1/support/departments", viewer, s.supportDepartments)
	r("POST /api/v1/support/departments", admin, s.createSupportDepartment)
	r("PUT /api/v1/support/departments/{id}", admin, s.updateSupportDepartment)
	r("DELETE /api/v1/support/departments/{id}", admin, s.deleteSupportDepartment)
	r("GET /api/v1/support/canned", operator, s.cannedReplies)
	r("POST /api/v1/support/canned", operator, s.createCannedReply)
	r("PUT /api/v1/support/canned/{id}", operator, s.updateCannedReply)
	r("DELETE /api/v1/support/canned/{id}", operator, s.deleteCannedReply)
	r("GET /api/v1/support/settings", admin, s.supportSettings)
	r("PUT /api/v1/support/settings", admin, s.setSupportSettings)
}

var errNoSupport = fmt.Errorf("%w: support tickets aren't available on this server", errBadRequest)

func (s *Server) support() (*support.Service, error) {
	if s.Support == nil {
		return nil, errNoSupport
	}
	return s.Support, nil
}

// supportActor is who a request acts as, for the support service.
func supportActor(r *http.Request) support.Actor {
	p := principalFrom(r.Context())
	a := support.Actor{UserID: p.UserID, Name: p.Name}
	if t := tenantOf(r); t != nil {
		a.AccountID, a.Reseller = t.Account.ID, p.Role == auth.RoleReseller
	} else {
		a.ReadOnly = auth.Level(p.Role) < auth.Level(operator)
	}
	return a
}

func pathID(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		return 0, store.ErrNotFound
	}
	return n, nil
}

// listTickets: ?status= (comma-separated), department=, priority=,
// assigned= (me, none or a user ID; staff), account=, q=, awaiting=1 (the
// tickets waiting for the caller), before= (a ticket ID: the page after
// it), limit=.
func (s *Server) listTickets(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	q := r.URL.Query()
	in := support.ListInput{Priority: q.Get("priority"), Assigned: q.Get("assigned"), Query: q.Get("q"),
		Awaiting: q.Get("awaiting") == "1"}
	if v := q.Get("status"); v != "" {
		in.Statuses = strings.Split(v, ",")
	}
	for _, f := range []struct {
		name string
		into *int64
	}{{"department", &in.DepartmentID}, {"account", &in.AccountID}, {"before", &in.Before}} {
		if v := q.Get(f.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return fmt.Errorf("%w: %s", errBadRequest, f.name)
			}
			*f.into = n
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return fmt.Errorf("%w: limit is 1 to 200", errBadRequest)
		}
		in.Limit = n
	}
	list, err := sv.List(r.Context(), supportActor(r), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

// readTicketForm decodes a ticket or a reply: JSON, or multipart with the
// JSON in a "data" part and files in "files" parts (up to the settings'
// count and size; checked and staged as they stream in). On error nothing
// staged is left behind.
func (s *Server) readTicketForm(w http.ResponseWriter, r *http.Request, sv *support.Service, into any) ([]*support.Staged, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		return nil, decode(w, r, into)
	}
	st, err := sv.Settings(r.Context())
	if err != nil {
		return nil, err
	}
	// Room for every file at its largest, and the rest of the form.
	r.Body = http.MaxBytesReader(w, r.Body, int64(st.MaxFiles)*int64(st.MaxFileMB)<<20+1<<20)
	// A slow line uploading 25 MB outlasts the server's write timeout.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(15 * time.Minute))
	rc.SetWriteDeadline(time.Now().Add(15 * time.Minute))
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, errors.Join(errBadRequest, err)
	}
	var files []*support.Staged
	fail := func(err error) ([]*support.Staged, error) {
		sv.Discard(files)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, fmt.Errorf("%w: the upload is larger than %d files of %d MB", support.ErrTooLarge, st.MaxFiles, st.MaxFileMB)
		}
		return nil, err
	}
	gotData := false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(errors.Join(errBadRequest, err))
		}
		switch {
		case part.FormName() == "files" && part.FileName() != "":
			if st.MaxFiles == 0 {
				return fail(fmt.Errorf("%w: attachments are turned off", support.ErrInvalid))
			}
			if len(files) == st.MaxFiles {
				return fail(fmt.Errorf("%w: at most %d attachments per message", support.ErrTooLarge, st.MaxFiles))
			}
			f, err := sv.Stage(st, part.FileName(), part)
			if err != nil {
				return fail(err)
			}
			files = append(files, f)
		case part.FormName() == "data" && !gotData:
			gotData = true
			dec := json.NewDecoder(io.LimitReader(part, 1<<20))
			dec.DisallowUnknownFields()
			if err := dec.Decode(into); err != nil {
				return fail(errors.Join(errBadRequest, err))
			}
		default:
			return fail(fmt.Errorf("%w: unexpected form field %q", errBadRequest, part.FormName()))
		}
	}
	if !gotData {
		return fail(fmt.Errorf("%w: the form has no data part", errBadRequest))
	}
	return files, nil
}

// openTicket: a tenant's for their own account; staff on behalf of one
// (account_id).
func (s *Server) openTicket(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	// Refused before any upload is read (support turned off, a viewer).
	if err := sv.CheckOpen(r.Context(), supportActor(r)); err != nil {
		return err
	}
	var in support.OpenInput
	files, err := s.readTicketForm(w, r, sv, &in)
	if err != nil {
		return err
	}
	if err := s.checkTicketSite(r, in.SiteID); err != nil {
		sv.Discard(files)
		return err
	}
	th, err := sv.Open(r.Context(), supportActor(r), in, files)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, th)
}

// checkTicketSite: a tenant names only a site they can see (the support
// service then checks that it's the ticket's account's).
func (s *Server) checkTicketSite(r *http.Request, siteID string) error {
	if siteID == "" || tenantOf(r) == nil {
		return nil
	}
	if _, ok := s.siteOwnerInScope(r.Context(), principalFrom(r.Context()), siteID); !ok {
		return fmt.Errorf("%w: no site %q in your account", support.ErrInvalid, siteID)
	}
	return nil
}

func (s *Server) getTicket(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	th, err := sv.Get(r.Context(), supportActor(r), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, th)
}

func (s *Server) replyTicket(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := sv.CheckReply(r.Context(), supportActor(r), id); err != nil {
		return err
	}
	var in support.ReplyInput
	files, err := s.readTicketForm(w, r, sv, &in)
	if err != nil {
		return err
	}
	th, err := sv.Reply(r.Context(), supportActor(r), id, in, files)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, th)
}

func (s *Server) updateTicket(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in support.UpdateInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	th, err := sv.Update(r.Context(), supportActor(r), id, in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, th)
}

func (s *Server) escalateTicket(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	th, err := sv.Escalate(r.Context(), supportActor(r), id, in.Reason)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, th)
}

// ticketAttachment sends an attachment. Like the file manager's downloads
// (see downloadFile), anyone's upload served from the panel's origin is
// an attachment of an opaque type, never sniffed, sandboxed; only raster
// images are shown inline (?inline=1), as images.
func (s *Server) ticketAttachment(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	attID, err := pathID(r, "att")
	if err != nil {
		return err
	}
	att, f, err := sv.Attachment(r.Context(), supportActor(r), id, attID)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hdr.Set("Cache-Control", "private, no-store")
	disposition, ctype := "attachment", "application/octet-stream"
	if support.Previewable(att.ContentType) && r.URL.Query().Get("inline") == "1" {
		disposition, ctype = "inline", att.ContentType
	}
	hdr.Set("Content-Type", ctype)
	hdr.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": att.Name}))
	http.ServeContent(w, r, "", att.CreatedAt, f)
	return nil
}

func (s *Server) supportSummary(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	sum, err := sv.Summary(r.Context(), supportActor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, sum)
}

func (s *Server) supportOverview(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	o, err := sv.Overview(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, o)
}

// supportAgents are the staff a ticket can be assigned to.
func (s *Server) supportAgents(w http.ResponseWriter, r *http.Request) error {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		return err
	}
	type agent struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	out := []agent{}
	for _, u := range users {
		if u.AccountID == 0 && !auth.IsTenant(u.Role) && !u.Disabled {
			out = append(out, agent{u.ID, u.Username, u.Role})
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// supportDepartments: every department for staff; the visible ones, and
// only what a customer needs of them, for tenants.
func (s *Server) supportDepartments(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	tenant := tenantOf(r) != nil
	list, err := sv.Departments(r.Context(), !tenant)
	if err != nil {
		return err
	}
	if !tenant {
		return writeJSON(w, http.StatusOK, list)
	}
	type dept struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := make([]dept, 0, len(list))
	for _, d := range list {
		out = append(out, dept{d.ID, d.Name, d.Description})
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSupportDepartment(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	var in support.DepartmentInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	d, err := sv.CreateDepartment(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, d)
}

func (s *Server) updateSupportDepartment(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in support.DepartmentInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	d, err := sv.UpdateDepartment(r.Context(), id, in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *Server) deleteSupportDepartment(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := sv.DeleteDepartment(r.Context(), id); errors.Is(err, store.ErrInUse) {
		return fmt.Errorf("%w: the department has tickets; hide it instead", store.ErrInUse)
	} else if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) cannedReplies(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.CannedReplies(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) createCannedReply(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	var in support.CannedInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	c, err := sv.CreateCanned(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateCannedReply(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in support.CannedInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	c, err := sv.UpdateCanned(r.Context(), id, in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCannedReply(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.Store.DeleteCannedReply(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) supportSettings(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	st, err := sv.Settings(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) setSupportSettings(w http.ResponseWriter, r *http.Request) error {
	sv, err := s.support()
	if err != nil {
		return err
	}
	var in support.Settings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := sv.SetSettings(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// ticketFilesOf is called before an account is deleted: it returns what
// removes the attachments of the account's tickets from disk once the
// deletion succeeded (the tickets' rows go with the account, in
// store.DeleteAccount). Best effort: failures are logged, and the support
// service's hourly sweep removes what's left.
func (s *Server) ticketFilesOf(ctx context.Context, accountID int64) func() {
	if s.Support == nil {
		return func() {}
	}
	ids, err := s.Store.AccountTicketIDs(ctx, accountID)
	if err != nil {
		s.Log.Warn("support: listing a deleted account's tickets", "account", accountID, "err", err)
		return func() {}
	}
	return func() { s.Support.RemoveTicketFiles(ids) }
}
