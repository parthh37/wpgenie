package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

// Outgoing e-mail to people (internal/mailer): the SMTP server, the
// templates staff can edit, and the e-mail log. Clients see the messages
// sent to their account.

func (s *Server) mailerRoutes(r func(string, string, handlerFunc)) {
	r("GET /api/v1/settings/email", admin, s.emailSettings)
	r("PUT /api/v1/settings/email", admin, s.setEmailSettings)
	r("POST /api/v1/settings/email/test", admin, s.testEmail)
	r("GET /api/v1/email/templates", admin, s.emailTemplates)
	r("PUT /api/v1/email/templates/{name}", admin, s.setEmailTemplate)
	r("DELETE /api/v1/email/templates/{name}", admin, s.resetEmailTemplate)
	r("POST /api/v1/email/templates/{name}/preview", admin, s.previewEmailTemplate)
	r("GET /api/v1/email/log", operator, s.emailLog)
	r("GET /api/v1/email/log/{id}", operator, s.emailMessage)
	r("GET /api/v1/email/log/{id}/html", operator, s.emailMessageHTML)
	r("POST /api/v1/email/log/{id}/resend", admin, s.resendEmail)
	r("GET /api/v1/email/previews/{token}", admin, s.emailPreviewHTML)
	r("GET /api/v1/accounts/{id}/emails", viewer, s.accountEmails)
	r("GET /api/v1/accounts/{id}/emails/{mail}", viewer, s.accountEmail)
	r("GET /api/v1/accounts/{id}/emails/{mail}/html", viewer, s.accountEmailHTML)
}

func init() {
	registerErrorStatus(mailer.ErrInvalid, http.StatusBadRequest)
	registerErrorStatus(mailer.ErrDisabled, http.StatusConflict)
	registerTenantRoutes(map[string]tenantRule{
		"GET /api/v1/accounts/{id}/emails":             anyTenant,
		"GET /api/v1/accounts/{id}/emails/{mail}":      anyTenant,
		"GET /api/v1/accounts/{id}/emails/{mail}/html": anyTenant,
	})
}

func (s *Server) emailSettings(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	cfg, err := s.Mailer.Settings(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, s.emailSettingsView(cfg))
}

func (s *Server) setEmailSettings(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	var in mailer.SettingsInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	cfg, err := s.Mailer.SetSettings(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, s.emailSettingsView(cfg))
}

// emailSettingsView is the settings without secrets, and this server's
// mail hostname (its certificate's name) for the "this server" preset.
func (s *Server) emailSettingsView(cfg *mailer.Settings) any {
	host := ""
	if s.Mail != nil {
		host = s.Mail.SMTPHost()
	}
	return struct {
		mailer.Settings
		LocalMailHost string `json:"local_mail_host"`
	}{cfg.Redacted(), host}
}

func (s *Server) testEmail(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	var in struct {
		To string `json:"to"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.Mailer.Test(r.Context(), in.To); err != nil {
		if errors.Is(err, mailer.ErrInvalid) || errors.Is(err, mailer.ErrDisabled) {
			return err
		}
		// The SMTP server's answer is what staff need to see.
		return writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
	return writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) emailTemplates(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	list, err := s.Mailer.Templates(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) setEmailTemplate(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	var in mailer.Override
	if err := decode(w, r, &in); err != nil {
		return err
	}
	v, err := s.Mailer.SetTemplate(r.Context(), r.PathValue("name"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) resetEmailTemplate(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	v, err := s.Mailer.ResetTemplate(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *Server) previewEmailTemplate(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	var in mailer.Override
	if err := decode(w, r, &in); err != nil {
		return err
	}
	out, err := s.Mailer.Preview(r.Context(), r.PathValue("name"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"subject": out.Subject, "text": out.Text, "html": out.HTML,
		"html_url": "/api/v1/email/previews/" + emailPreviews.put(out.HTML, s.now())})
}

// emailPreviews holds rendered previews for a few minutes, so the editor
// can show them in a frame (see writeEmailHTML).
var emailPreviews = &previewCache{m: map[string]preview{}}

type preview struct {
	html string
	at   time.Time
}

type previewCache struct {
	mu sync.Mutex
	m  map[string]preview
}

const previewTTL = 10 * time.Minute

func (c *previewCache) put(html string, now time.Time) string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		if now.Sub(v.at) > previewTTL || len(c.m) > 100 {
			delete(c.m, k)
		}
	}
	c.m[tok] = preview{html, now}
	return tok
}

func (c *previewCache) get(tok string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[tok]
	if !ok || now.Sub(p.at) > previewTTL {
		return "", false
	}
	return p.html, true
}

func (s *Server) emailPreviewHTML(w http.ResponseWriter, r *http.Request) error {
	h, ok := emailPreviews.get(r.PathValue("token"), s.now())
	if !ok {
		return store.ErrNotFound
	}
	writeEmailHTML(w, h)
	return nil
}

// writeEmailHTML serves an e-mail's HTML part to be shown in a frame of
// the panel: its own policy (the panel's forbids the inline styles e-mail
// needs, and a srcdoc frame would inherit it), sandboxed so nothing in it
// runs or reaches the panel's origin, framable by the panel only.
func writeEmailHTML(w http.ResponseWriter, body string) {
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src https: data:; "+
		"frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox")
	hdr.Set("X-Frame-Options", "SAMEORIGIN")
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Cache-Control", "private, no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	// Links open outside the frame.
	if i := strings.Index(body, "<head>"); i >= 0 {
		body = body[:i+6] + `<base target="_blank">` + body[i+6:]
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

func (s *Server) emailMessageHTML(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.ErrNotFound
	}
	m, err := s.Store.GetMail(r.Context(), id)
	if err != nil {
		return err
	}
	writeEmailHTML(w, htmlOrText(m))
	return nil
}

func (s *Server) accountEmailHTML(w http.ResponseWriter, r *http.Request) error {
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(r.PathValue("mail"), 10, 64)
	if err != nil {
		return store.ErrNotFound
	}
	m, err := s.Store.GetMail(r.Context(), id)
	if err != nil || m.AccountID != a.ID {
		return store.ErrNotFound
	}
	writeEmailHTML(w, htmlOrText(m))
	return nil
}

// htmlOrText is a message's HTML part, or its text in a <pre>.
func htmlOrText(m *store.MailMessage) string {
	if m.HTML != "" {
		return m.HTML
	}
	return "<!doctype html><html><head><meta charset=\"utf-8\"></head><body><pre style=\"white-space:pre-wrap;font:14px/1.5 monospace\">" +
		html.EscapeString(m.Text) + "</pre></body></html>"
}

// mailSummary is a log entry without its bodies.
func mailSummary(list []*store.MailMessage) []*store.MailMessage {
	for _, m := range list {
		m.Text, m.HTML = "", ""
	}
	return list
}

func mailFilter(r *http.Request) store.MailFilter {
	q := r.URL.Query()
	f := store.MailFilter{Status: q.Get("status")}
	f.AccountID, _ = strconv.ParseInt(q.Get("account"), 10, 64)
	f.BeforeID, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	return f
}

func (s *Server) emailLog(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.MailLog(r.Context(), mailFilter(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, mailSummary(list))
}

func (s *Server) emailMessage(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.ErrNotFound
	}
	m, err := s.Store.GetMail(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, m)
}

func (s *Server) resendEmail(w http.ResponseWriter, r *http.Request) error {
	if s.Mailer == nil {
		return mailer.ErrDisabled
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.ErrNotFound
	}
	if err := s.Mailer.Resend(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// accountEmails is the e-mail sent to an account (ownership of {id} is
// checked centrally for tenants).
func (s *Server) accountEmails(w http.ResponseWriter, r *http.Request) error {
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	f := mailFilter(r)
	f.AccountID, f.Status = a.ID, ""
	list, err := s.Store.MailLog(r.Context(), f)
	if err != nil {
		return err
	}
	for _, m := range list {
		forTenant(r, m)
	}
	return writeJSON(w, http.StatusOK, mailSummary(list))
}

// forTenant hides the SMTP server's answers (they can name internal relay
// hosts) from tenants: they see whether a message went, not why not.
func forTenant(r *http.Request, m *store.MailMessage) {
	if tenantOf(r) != nil {
		m.LastError = ""
	}
}

func (s *Server) accountEmail(w http.ResponseWriter, r *http.Request) error {
	a, err := s.accountFromPath(r)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(r.PathValue("mail"), 10, 64)
	if err != nil {
		return store.ErrNotFound
	}
	m, err := s.Store.GetMail(r.Context(), id)
	if err != nil || m.AccountID != a.ID {
		return store.ErrNotFound
	}
	forTenant(r, m)
	return writeJSON(w, http.StatusOK, m)
}
