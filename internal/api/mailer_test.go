package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

func TestAccountEmailsAreScoped(t *testing.T) {
	e := newTenancyEnv(t)
	e.api.Mailer = &mailer.Service{Store: e.st, Log: slog.New(slog.DiscardHandler), Now: e.api.Now}
	ctx := context.Background()
	send := func(acct string) int64 {
		m := &store.MailMessage{AccountID: e.acct[acct].ID, To: []string{"x@example.com"}, Subject: "Hi " + acct,
			Text: "t", HTML: "<html><head></head><body><p style=\"color:red\">Hi</p></body></html>"}
		if _, err := e.st.EnqueueMail(ctx, m, e.now); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	ma, mb := send("A"), send("B")

	var list []store.MailMessage
	if code := e.as("alice", "GET", fmt.Sprintf("/api/v1/accounts/%d/emails", e.acct["A"].ID), "", &list); code != 200 ||
		len(list) != 1 || list[0].Subject != "Hi A" || list[0].HTML != "" {
		t.Fatalf("%d %+v", code, list)
	}
	// Another account's log, or its message through one's own account: 404.
	for _, p := range []string{fmt.Sprintf("/api/v1/accounts/%d/emails", e.acct["B"].ID),
		fmt.Sprintf("/api/v1/accounts/%d/emails/%d", e.acct["A"].ID, mb),
		fmt.Sprintf("/api/v1/accounts/%d/emails/%d/html", e.acct["A"].ID, mb)} {
		if code := e.as("alice", "GET", p, "", nil); code != http.StatusNotFound {
			t.Errorf("%s: %d", p, code)
		}
	}
	// Staff-only routes stay staff-only.
	for _, p := range []string{"/api/v1/email/log", "/api/v1/settings/email", "/api/v1/email/templates"} {
		if code := e.as("alice", "GET", p, "", nil); code != http.StatusForbidden {
			t.Errorf("%s: %d", p, code)
		}
	}

	// The HTML part comes sandboxed, framable by the panel only.
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/accounts/%d/emails/%d/html", e.srv.URL, e.acct["A"].ID, ma), nil)
	req.Header.Set("Authorization", "Bearer "+e.tokens["alice"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "frame-ancestors 'self'") ||
		!strings.Contains(csp, "default-src 'none'") || resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" ||
		!strings.Contains(string(b), `<head><base target="_blank">`) {
		t.Fatalf("%d %v\n%s", resp.StatusCode, resp.Header, b)
	}
}
