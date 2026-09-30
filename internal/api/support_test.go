package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/support"
)

// supportEnv is the tenancy panel with the help desk, and a ticket of
// each of alice (A), bob (B), rita (R, the reseller's own) and carl (C, R's
// customer).
type supportEnv struct {
	*tenancyEnv
	ticket map[string]int64 // user -> their ticket
}

func newSupportEnv(t *testing.T) *supportEnv {
	t.Helper()
	e := &supportEnv{tenancyEnv: newTenancyEnv(t), ticket: map[string]int64{}}
	log := slog.New(slog.DiscardHandler)
	e.api.Support = &support.Service{Store: e.st, Log: log, Dir: t.TempDir(),
		Mailer: &mailer.Service{Store: e.st, Log: log, PanelURL: "https://panel.test"}}
	ctx := context.Background()
	for name, a := range e.acct {
		a.Email = strings.ToLower(name) + "@example.test"
		if err := e.st.UpdateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{"alice", "bob", "rita", "carl"} {
		var th support.Thread
		if c := e.as(u, "POST", "/api/v1/tickets", `{"subject":"`+u+`'s ticket","body":"Help `+u+`"}`, &th); c != 201 {
			t.Fatalf("%s opens a ticket: %d", u, c)
		}
		e.ticket[u] = th.Ticket.ID
	}
	return e
}

func (e *supportEnv) path(user, suffix string) string {
	return "/api/v1/tickets/" + strconv.FormatInt(e.ticket[user], 10) + suffix
}

// upload posts a multipart form: data (JSON) and files (name -> content).
func (e *supportEnv) upload(who, path, data string, files ...[2]string) (int, []byte) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("data", data)
	for _, f := range files {
		w, _ := mw.CreateFormFile("files", f[0])
		w.Write([]byte(f[1]))
	}
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if who == "tok" {
		req.Header.Set("Authorization", "Bearer tok")
	} else {
		req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// Every route under a ticket answers 404 to tenants who aren't party to
// it, whatever its method: another customer's, the reseller's own ticket
// for its customer, someone else's customer's for a reseller.
func TestTicketRoutesAreScoped(t *testing.T) {
	e := newSupportEnv(t)
	n := 0
	for _, rt := range e.api.routes {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		if !strings.HasPrefix(path, "/api/v1/tickets/{id}") {
			continue
		}
		n++
		for _, c := range []struct{ who, owner string }{
			{"alice", "bob"}, {"bob", "carl"}, {"carl", "rita"}, {"carl", "alice"}, {"rita", "alice"}, {"rita", "bob"},
			{"session:carl", "rita"},
		} {
			p := strings.NewReplacer("{id}", strconv.FormatInt(e.ticket[c.owner], 10), "{att}", "1").Replace(path)
			want := http.StatusNotFound
			if tenantRoutes[rt.Pattern].reseller && !strings.Contains(c.who, "rita") {
				want = http.StatusForbidden // resellers only, before anything else
			}
			if got := e.as(c.who, method, p, "{}", nil); got != want {
				t.Errorf("%s %s (%s's ticket) as %s = %d, want %d", method, p, c.owner, c.who, got, want)
			}
		}
	}
	if n != 5 {
		t.Errorf("%d ticket routes checked", n)
	}
	// Parties reach them.
	for _, c := range []struct{ who, owner string }{{"alice", "alice"}, {"rita", "carl"}, {"rita", "rita"}, {"tok", "carl"}} {
		if got := e.as(c.who, "GET", e.path(c.owner, ""), "", nil); got != 200 {
			t.Errorf("%s reading %s's ticket: %d", c.who, c.owner, got)
		}
	}
	// Staff-only support routes are refused to tenants.
	for _, p := range []string{"/api/v1/support/overview", "/api/v1/support/agents", "/api/v1/support/canned", "/api/v1/support/settings"} {
		if got := e.as("rita", "GET", p, "", nil); got != 403 {
			t.Errorf("rita GET %s = %d", p, got)
		}
	}
	if got := e.as("alice", "POST", "/api/v1/support/departments", `{"name":"x"}`, nil); got != 403 {
		t.Errorf("tenant created a department: %d", got)
	}
}

func TestTicketListsAndNotes(t *testing.T) {
	e := newSupportEnv(t)
	list := func(who, query string) map[int64]support.TicketView {
		t.Helper()
		var l []support.TicketView
		if c := e.as(who, "GET", "/api/v1/tickets"+query, "", &l); c != 200 {
			t.Fatalf("%s lists: %d", who, c)
		}
		out := map[int64]support.TicketView{}
		for _, v := range l {
			out[v.ID] = v
		}
		return out
	}
	if l := list("alice", ""); len(l) != 1 || l[e.ticket["alice"]].You != support.PartyCustomer {
		t.Errorf("alice's list: %+v", l)
	}
	if l := list("rita", ""); len(l) != 2 || l[e.ticket["carl"]].You != support.PartyHandler || l[e.ticket["rita"]].You != support.PartyCustomer {
		t.Errorf("rita's list: %+v", l)
	}
	if l := list("carl", ""); len(l) != 1 {
		t.Errorf("carl's list: %+v", l)
	}
	if l := list("tok", ""); len(l) != 4 {
		t.Errorf("staff list: %d", len(l))
	}
	// Filtering on someone else's account finds nothing.
	if l := list("alice", fmt.Sprintf("?account=%d", e.acct["B"].ID)); len(l) != 0 {
		t.Errorf("alice filtered to B: %+v", l)
	}
	for _, q := range []string{"status=bogus", "assigned=12abc", "limit=0", "before=-1"} {
		if c := e.as("tok", "GET", "/api/v1/tickets?"+q, "", nil); c != 400 {
			t.Errorf("bad filter %s: %d", q, c)
		}
	}
	if c := e.as("alice", "GET", "/api/v1/tickets?assigned=me", "", nil); c != 400 {
		t.Errorf("a customer filtered by assignee: %d", c)
	}

	// Notes: the handler's on carl's ticket, staff's on rita's own.
	if c := e.as("rita", "POST", e.path("carl", "/replies"), `{"body":"NOTE-R","internal":true}`, nil); c != 201 {
		t.Fatalf("handler note: %d", c)
	}
	if c := e.as("tok", "POST", e.path("rita", "/replies"), `{"body":"NOTE-S","internal":true}`, nil); c != 201 {
		t.Fatalf("staff note: %d", c)
	}
	if c := e.as("carl", "POST", e.path("carl", "/replies"), `{"body":"x","internal":true}`, nil); c != 403 {
		t.Errorf("customer note: %d", c)
	}
	for _, c := range []struct{ who, owner, secret string }{{"carl", "carl", "NOTE-R"}, {"rita", "rita", "NOTE-S"}, {"session:carl", "carl", "NOTE-R"}} {
		raw := e.raw(c.who, "GET", e.path(c.owner, ""))
		if strings.Contains(raw, c.secret) || strings.Contains(raw, `"internal":true`) || strings.Contains(raw, "handler") {
			t.Errorf("%s sees provider-only content: %s", c.who, raw)
		}
		if raw := e.raw(c.who, "GET", "/api/v1/tickets"); strings.Contains(raw, c.secret) {
			t.Errorf("%s sees a note in the list: %s", c.who, raw)
		}
	}
	if raw := e.raw("rita", "GET", e.path("carl", "")); !strings.Contains(raw, "NOTE-R") {
		t.Error("the handler doesn't see its note")
	}

	// Transitions over HTTP: a customer closes and reopens, nothing more.
	if c := e.as("alice", "PUT", e.path("alice", ""), `{"status":"on_hold"}`, nil); c != 403 {
		t.Errorf("customer set on hold: %d", c)
	}
	var th support.Thread
	if c := e.as("alice", "PUT", e.path("alice", ""), `{"status":"closed"}`, &th); c != 200 || th.Ticket.Status != "closed" {
		t.Errorf("customer closes: %d %+v", c, th.Ticket)
	}
	if c := e.as("tok", "POST", e.path("alice", "/replies"), `{"body":"Reopening for you","status":"in_progress"}`, &th); c != 201 ||
		th.Ticket.Status != "in_progress" {
		t.Errorf("staff reply with status: %d %+v", c, th.Ticket)
	}

	// Escalation: resellers only.
	if c := e.as("alice", "POST", e.path("alice", "/escalate"), `{}`, nil); c != 403 {
		t.Errorf("customer escalate: %d", c)
	}
	if c := e.as("rita", "POST", e.path("rita", "/escalate"), `{}`, nil); c != 403 {
		t.Errorf("reseller escalated its own ticket: %d", c)
	}
	if c := e.as("rita", "POST", e.path("carl", "/escalate"), `{"reason":"root needed"}`, &th); c != 200 || th.Ticket.Handler != "staff" {
		t.Errorf("escalate: %d %+v", c, th.Ticket)
	}
	if c := e.as("rita", "POST", e.path("carl", "/escalate"), `{}`, nil); c != 409 {
		t.Errorf("escalated twice: %d", c)
	}

	// Summary (the navigation's badge).
	var sum support.Summary
	if c := e.as("tok", "GET", "/api/v1/support/summary", "", &sum); c != 200 || sum.Awaiting != 3 { // bob's, rita's, carl's
		t.Errorf("staff summary: %d %+v", c, sum)
	}
	var ov support.Overview
	if c := e.as("tok", "GET", "/api/v1/support/overview", "", &ov); c != 200 || ov.ByStatus["open"] != 3 || ov.Opened30d != 4 {
		t.Errorf("overview: %d %+v", c, ov)
	}
}

func (e *supportEnv) raw(who, method, path string) string {
	e.t.Helper()
	var v json.RawMessage
	if c := e.as(who, method, path, "", &v); c != 200 {
		e.t.Fatalf("%s %s as %s: %d", method, path, who, c)
	}
	return string(v)
}

func TestTicketAttachmentsOverHTTP(t *testing.T) {
	e := newSupportEnv(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"
	c, body := e.upload("alice", "/api/v1/tickets", `{"subject":"With a screenshot","body":"see","site_id":"sa"}`,
		[2]string{"screen shot.png", png}, [2]string{"php.log", "Fatal error"})
	var th support.Thread
	if c != 201 || json.Unmarshal(body, &th) != nil || len(th.Messages[0].Files) != 2 || th.Ticket.SiteDomain != "sa.test" {
		t.Fatalf("upload: %d %s", c, body)
	}
	img := th.Messages[0].Files[0]
	get := func(who, q string) *http.Response {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/tickets/%d/attachments/%d%s", e.srv.URL, th.Ticket.ID, img.ID, q), nil)
		req.Header.Set("Authorization", "Bearer "+e.tokens[who])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	resp := get("alice", "")
	h := resp.Header
	if resp.StatusCode != 200 || h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Content-Security-Policy") != "default-src 'none'; sandbox" || h.Get("Content-Disposition") != `attachment; filename="screen shot.png"` ||
		h.Get("Cache-Control") != "private, no-store" {
		t.Errorf("download headers: %d %v", resp.StatusCode, h)
	}
	resp = get("alice", "?inline=1")
	if resp.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") ||
		resp.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Errorf("inline headers: %v", resp.Header)
	}
	if resp := get("bob", ""); resp.StatusCode != 404 {
		t.Errorf("bob downloaded alice's attachment: %d", resp.StatusCode)
	}
	// The log is never inline.
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/tickets/%d/attachments/%d?inline=1", e.srv.URL, th.Ticket.ID, th.Messages[0].Files[1].ID), nil)
	req.Header.Set("Authorization", "Bearer "+e.tokens["alice"])
	if r, _ := http.DefaultClient.Do(req); r.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("a log inline: %v", r.Header)
	}

	// Limits: count, size, type; nothing is kept from a refused form.
	e.api.Support.SetSettings(context.Background(), support.Settings{Enabled: true, MaxFiles: 1, MaxFileMB: 1, Extensions: support.DefaultExtensions})
	reply := e.path("alice", "/replies")
	for _, c := range []struct {
		name  string
		files [][2]string
		want  int
	}{
		{"two files", [][2]string{{"a.txt", "a"}, {"b.txt", "b"}}, 413},
		{"too big", [][2]string{{"big.txt", strings.Repeat("a", 1<<20+10)}}, 413},
		{"html", [][2]string{{"x.html", "<html>"}}, 400},
		{"fake png", [][2]string{{"x.png", "<script>alert(1)</script>"}}, 400},
	} {
		if got, body := e.upload("alice", reply, `{"body":"x"}`, c.files...); got != c.want {
			t.Errorf("%s: %d %s", c.name, got, body)
		}
	}
	if c, _ := e.upload("alice", reply, `{"body":"x","bogus":1}`); c != 400 {
		t.Errorf("unknown field: %d", c)
	}
	if c, _ := e.upload("alice", "/api/v1/tickets", `{"subject":"x","body":"x","site_id":"sb"}`); c != 400 {
		t.Errorf("someone else's site: %d", c)
	}
	var thread support.Thread
	e.as("alice", "GET", e.path("alice", ""), "", &thread)
	if len(thread.Messages) != 1 {
		t.Errorf("refused replies were saved: %d messages", len(thread.Messages))
	}
}

func TestTicketsWhileSuspended(t *testing.T) {
	e := newSupportEnv(t)
	ctx := context.Background()
	e.st.SetAccountStatus(ctx, e.acct["A"].ID, store.AccountSuspended, billing.ReasonBilling, e.now)
	if c := e.as("alice", "POST", "/api/v1/tickets", `{"subject":"Why suspended?","body":"?"}`, nil); c != 201 {
		t.Errorf("open while suspended: %d", c)
	}
	if c := e.as("alice", "POST", e.path("alice", "/replies"), `{"body":"hello?"}`, nil); c != 201 {
		t.Errorf("reply while suspended: %d", c)
	}
	if c := e.as("alice", "PUT", e.path("alice", ""), `{"status":"closed"}`, nil); c != 200 {
		t.Errorf("close while suspended: %d", c)
	}
	// Other changes stay frozen.
	if c := e.as("alice", "POST", "/api/v1/sites/sa/cache/purge", "", nil); c != 403 {
		t.Errorf("purge while suspended: %d", c)
	}
	// Turned off: tenants can't open tickets; staff can for them.
	e.api.Support.SetSettings(ctx, support.Settings{Enabled: false, MaxFileMB: 1})
	if c := e.as("bob", "POST", "/api/v1/tickets", `{"subject":"x","body":"x"}`, nil); c != 403 {
		t.Errorf("opened while support is off: %d", c)
	}
	if c := e.as("tok", "POST", "/api/v1/tickets", fmt.Sprintf(`{"account_id":%d,"subject":"x","body":"x"}`, e.acct["B"].ID), nil); c != 201 {
		t.Errorf("staff for bob: %d", c)
	}
}

func TestSupportAdminRoutes(t *testing.T) {
	e := newSupportEnv(t)
	var d store.SupportDepartment
	if c := e.as("tok", "POST", "/api/v1/support/departments", `{"name":"Abuse","hidden":true,"notify_email":"abuse@op.test"}`, &d); c != 201 {
		t.Fatalf("create department: %d", c)
	}
	var tenantView []map[string]any
	e.as("alice", "GET", "/api/v1/support/departments", "", &tenantView)
	if len(tenantView) != 1 || tenantView[0]["notify_email"] != nil || tenantView[0]["name"] != "General" {
		t.Errorf("tenant departments: %+v", tenantView)
	}
	var staffView []store.SupportDepartment
	e.as("tok", "GET", "/api/v1/support/departments", "", &staffView)
	if len(staffView) != 2 {
		t.Errorf("staff departments: %+v", staffView)
	}
	general := staffView[0].ID
	if staffView[0].Name != "General" {
		general = staffView[1].ID
	}
	if c := e.as("tok", "DELETE", fmt.Sprintf("/api/v1/support/departments/%d", general), "", nil); c != 409 {
		t.Errorf("deleted a department with tickets: %d", c)
	}
	if c := e.as("tok", "DELETE", fmt.Sprintf("/api/v1/support/departments/%d", d.ID), "", nil); c != 204 {
		t.Errorf("delete: %d", c)
	}
	var cr store.CannedReply
	if c := e.as("tok", "POST", "/api/v1/support/canned", `{"title":"Thanks","body":"Glad to help"}`, &cr); c != 201 || cr.ID == 0 {
		t.Errorf("canned: %d", c)
	}
	if c := e.as("tok", "PUT", fmt.Sprintf("/api/v1/support/canned/%d", cr.ID), `{"title":"Thanks!","body":"Glad"}`, nil); c != 200 {
		t.Errorf("canned update: %d", c)
	}
	if c := e.as("tok", "DELETE", fmt.Sprintf("/api/v1/support/canned/%d", cr.ID), "", nil); c != 204 {
		t.Errorf("canned delete: %d", c)
	}
	var st support.Settings
	if c := e.as("tok", "PUT", "/api/v1/support/settings", `{"enabled":true,"notify_emails":["x@op.test"],"auto_close_days":3,"max_files":2,"max_file_mb":2,"extensions":["pdf"],"reply_to":""}`, &st); c != 200 || st.AutoCloseDays != 3 {
		t.Errorf("settings: %d %+v", c, st)
	}
	if c := e.as("tok", "PUT", "/api/v1/support/settings", `{"max_file_mb":2,"extensions":["svg"]}`, nil); c != 400 {
		t.Errorf("svg allowed: %d", c)
	}
	var agents []map[string]any
	if c := e.as("tok", "GET", "/api/v1/support/agents", "", &agents); c != 200 {
		t.Errorf("agents: %d", c)
	}
	for _, a := range agents {
		if a["username"] == "alice" || a["username"] == "rita" {
			t.Errorf("a tenant user among agents: %v", a)
		}
	}
	// E-mails were queued for the tickets of the set-up.
	msgs, _ := e.st.MailLog(context.Background(), store.MailFilter{Limit: 100})
	opened := 0
	for _, m := range msgs {
		if m.Template == "ticket.opened" {
			opened++
		}
	}
	if opened != 4 {
		t.Errorf("%d ticket.opened e-mails", opened)
	}
}
