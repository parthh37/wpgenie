package mailer

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"mime/quotedprintable"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// smtpServer is a minimal SMTP server (EHLO, STARTTLS, AUTH PLAIN, MAIL,
// RCPT, DATA, QUIT) recording what it was given. fail > 0 refuses that
// many messages at DATA.
type smtpServer struct {
	addr string
	mu   sync.Mutex
	auth string
	tls  bool
	rcpt []string
	data []string
	fail int
}

func newSMTP(t *testing.T, cert *tls.Certificate) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &smtpServer{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cert)
		}
	}()
	return s
}

func (s *smtpServer) serve(c net.Conn, cert *tls.Certificate) {
	defer c.Close()
	r, w := bufio.NewReader(c), io.Writer(c)
	say := func(l string) { io.WriteString(w, l+"\r\n") }
	secure := false
	say("220 test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			say("250-test")
			if cert != nil && !secure {
				say("250-STARTTLS")
			}
			say("250 AUTH PLAIN")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*cert}})
			if tc.Handshake() != nil {
				return
			}
			c, secure = tc, true
			r, w = bufio.NewReader(tc), tc
		case strings.HasPrefix(up, "AUTH PLAIN "):
			b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(cmd[len("AUTH PLAIN "):]))
			parts := strings.Split(string(b), "\x00")
			s.mu.Lock()
			s.auth = parts[1] + ":" + parts[2]
			s.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			s.mu.Lock()
			s.rcpt = append(s.rcpt, cmd[len("RCPT TO:"):])
			s.mu.Unlock()
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			if s.fail > 0 {
				s.fail--
				s.mu.Unlock()
				say("451 try again later")
				continue
			}
			s.data, s.tls = append(s.data, b.String()), secure
			s.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func selfSigned(t *testing.T) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

type env struct {
	svc *Service
	st  *store.Store
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	e.svc = &Service{Store: st, Log: slog.New(slog.DiscardHandler), PanelURL: "https://panel.example.com",
		Now: func() time.Time { return e.now },
		Brand: func(context.Context) Brand {
			return Brand{Name: "Acme Hosting", URL: "https://acme.example", Footer: "1 Main St\nSpringfield"}
		}}
	return e
}

func (e *env) configure(t *testing.T, srv *smtpServer, mode, user, pass string, pool *x509.CertPool) {
	t.Helper()
	host, port, _ := net.SplitHostPort(srv.addr)
	p, _ := strconv.Atoi(port)
	e.svc.Roots = pool
	if _, err := e.svc.SetSettings(context.Background(), SettingsInput{Enabled: true, Host: host, Port: p, TLS: mode,
		Username: user, Password: &pass, FromName: "Acme Billing", FromAddress: "billing@acme.example",
		ReplyTo: "support@acme.example"}); err != nil {
		t.Fatal(err)
	}
}

// decodeParts returns the decoded text of a message's MIME parts.
func decodeParts(t *testing.T, data string) string {
	t.Helper()
	var out strings.Builder
	for _, chunk := range strings.Split(data, "Content-Transfer-Encoding: quoted-printable\r\n\r\n")[1:] {
		b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(chunk)))
		if err != nil {
			t.Fatal(err)
		}
		out.Write(b)
	}
	return out.String()
}

func init() {
	Register(Template{Name: "test.invoice", Group: "Test", Description: "for tests",
		Subject: "Invoice {{.Invoice.Number}} from {{.Brand.Name}}",
		Body: "Hello {{.Client.Name}},\n\nYour invoice {{.Invoice.Number}} is ready.\nTotal: {{.Invoice.Total}}\n\n" +
			"[[Pay invoice|{{.PanelURL}}/#/invoices/{{.Invoice.ID}}]]\n\nThanks!",
		Vars:   []string{"Client.Name", "Invoice.Number"},
		Sample: map[string]any{"Client": map[string]any{"Name": "Sam"}, "Invoice": map[string]any{"Number": "INV-1", "Total": "$10.00", "ID": 1}}})
}

func TestQueueRenderAndDeliver(t *testing.T) {
	e := newEnv(t)
	cert, pool := selfSigned(t)
	srv := newSMTP(t, cert)
	ctx := context.Background()

	// Queued before sending is configured: kept until it is.
	data := map[string]any{"Client": map[string]any{"Name": "Dana <script>"}, "Invoice": map[string]any{"Number": "INV-7", "Total": "$12.50", "ID": 7}}
	ok, err := e.svc.Queue(ctx, Message{To: []string{"dana@example.net"}, Template: "test.invoice", Data: data, AccountID: 3, DedupeKey: "inv:7"})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	// The same key queues nothing.
	if ok, err := e.svc.Queue(ctx, Message{To: []string{"dana@example.net"}, Template: "test.invoice", Data: data, DedupeKey: "inv:7"}); err != nil || ok {
		t.Fatalf("duplicate queued: %v %v", ok, err)
	}
	e.svc.DeliverDue(ctx)
	if len(srv.data) != 0 {
		t.Fatal("sent while disabled")
	}

	e.configure(t, srv, TLSStartTLS, "billing", "s3cret", pool)
	e.svc.DeliverDue(ctx)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.data) != 1 || srv.auth != "billing:s3cret" || !srv.tls {
		t.Fatalf("data %d auth %q tls %v", len(srv.data), srv.auth, srv.tls)
	}
	msg := srv.data[0]
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	for _, want := range []string{`From: "Acme Billing" <billing@acme.example>`, "To: dana@example.net",
		"Reply-To: support@acme.example", "Subject: Invoice INV-7 from Acme Hosting", "multipart/alternative"} {
		if !strings.Contains(head, want) {
			t.Errorf("header %q missing:\n%s", want, head)
		}
	}
	body := decodeParts(t, msg)
	for _, want := range []string{"Hello Dana <script>,", "Pay invoice: https://panel.example.com/#/invoices/7",
		"Hello Dana &lt;script&gt;,", `<a href="https://panel.example.com/#/invoices/7"`, "1 Main St<br>Springfield"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script>,<") {
		t.Error("HTML not escaped")
	}
	log, err := e.st.MailLog(ctx, store.MailFilter{AccountID: 3})
	if err != nil || len(log) != 1 || log[0].Status != store.MailSent || log[0].Attempts != 1 {
		t.Fatalf("log: %+v %v", log, err)
	}
}

func TestRetriesThenFails(t *testing.T) {
	e := newEnv(t)
	srv := newSMTP(t, nil)
	srv.fail = 100
	e.configure(t, srv, TLSNone, "", "", nil)
	ctx := context.Background()
	if _, err := e.svc.Queue(ctx, Message{To: []string{"a@example.net"}, Subject: "Hi", Text: "Hello"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= len(retryDelays); i++ {
		e.svc.DeliverDue(ctx)
		e.now = e.now.Add(7 * time.Hour)
	}
	log, _ := e.st.MailLog(ctx, store.MailFilter{})
	if len(log) != 1 || log[0].Status != store.MailFailed || log[0].Attempts != len(retryDelays)+1 ||
		!strings.Contains(log[0].LastError, "451") {
		t.Fatalf("%+v", log[0])
	}
	// Resending from the log tries again.
	srv.mu.Lock()
	srv.fail = 0
	srv.mu.Unlock()
	if err := e.svc.Resend(ctx, log[0].ID); err != nil {
		t.Fatal(err)
	}
	e.svc.DeliverDue(ctx)
	if m, _ := e.st.GetMail(ctx, log[0].ID); m.Status != store.MailSent {
		t.Fatalf("after resend: %+v", m)
	}
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pw := "x"
	for _, tc := range []struct {
		name string
		in   SettingsInput
		ok   bool
	}{
		{"starttls", SettingsInput{Enabled: true, Host: "smtp.example.com", TLS: TLSStartTLS, Username: "u", Password: &pw, FromAddress: "a@example.com"}, true},
		{"password without TLS", SettingsInput{Enabled: true, Host: "smtp.example.com", TLS: TLSNone, Username: "u", Password: &pw, FromAddress: "a@example.com"}, false},
		{"header injection in host", SettingsInput{Enabled: true, Host: "smtp.example.com\r\nX", FromAddress: "a@example.com"}, false},
		{"from with display name", SettingsInput{Enabled: true, Host: "smtp.example.com", FromAddress: "A <a@example.com>"}, false},
		{"bad bcc", SettingsInput{Enabled: true, Host: "smtp.example.com", FromAddress: "a@example.com", BCC: "nope"}, false},
		{"disabled and empty", SettingsInput{}, true},
	} {
		_, err := e.svc.SetSettings(ctx, tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The password is kept when not sent again, and never shown.
	if _, err := e.svc.SetSettings(ctx, SettingsInput{Enabled: true, Host: "smtp.example.com", Username: "u", Password: &pw, FromAddress: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.SetSettings(ctx, SettingsInput{Enabled: true, Host: "smtp2.example.com", Username: "u", FromAddress: "a@example.com"})
	if got.Password != "x" || got.Redacted().Password != "" || !got.Redacted().PasswordSet {
		t.Fatalf("%+v", got)
	}
	// Recipients with line breaks never reach a header.
	if _, err := e.svc.Queue(ctx, Message{To: []string{"a@example.com\r\nBcc: b@example.com"}, Subject: "s", Text: "t"}); err == nil {
		t.Fatal("recipient injection accepted")
	}
}

func TestTemplateOverrides(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.SetTemplate(ctx, "test.invoice", Override{Subject: "{{.Invoice.Number", Body: "x"}); err == nil {
		t.Fatal("broken template accepted")
	}
	v, err := e.svc.SetTemplate(ctx, "test.invoice", Override{Subject: "Bill {{.Invoice.Number}}", Body: "Hi {{.Client.Name}}"})
	if err != nil || !v.Customized || v.DefaultSubject == v.Subject {
		t.Fatal(v, err)
	}
	data := map[string]any{"Client": map[string]any{"Name": "Lee"}, "Invoice": map[string]any{"Number": "INV-9"}}
	r, err := e.svc.Render(ctx, "test.invoice", data)
	if err != nil || r.Subject != "Bill INV-9" || !strings.HasPrefix(r.Text, "Hi Lee\n\n--\nAcme Hosting") {
		t.Fatalf("%+v %v", r, err)
	}
	p, err := e.svc.Preview(ctx, "test.invoice", Override{})
	if err != nil || p.Subject != "Bill INV-1" {
		t.Fatalf("%+v %v", p, err)
	}
	// A stored override that no longer renders falls back to the default.
	e.st.SetSetting(ctx, overrideKey("test.invoice"), `{"subject":"{{.Invoice.Number.Nope}}","body":"b"}`)
	r, err = e.svc.Render(ctx, "test.invoice", data)
	if err != nil || r.Subject != "Invoice INV-9 from Acme Hosting" {
		t.Fatalf("fallback: %+v %v", r, err)
	}
	if v, err := e.svc.ResetTemplate(ctx, "test.invoice"); err != nil || v.Customized {
		t.Fatal(v, err)
	}
}

func TestLayout(t *testing.T) {
	r := layout(Brand{Name: "A&B"}, "S", "Line one\nline two https://x.example/a?b=1&c=2.\n\n\n[[Go|https://x.example/go]]\n\nbye")
	if !strings.Contains(r.HTML, `line two <a href="https://x.example/a?b=1&amp;c=2"`) || !strings.Contains(r.HTML, ">Go</a>") ||
		!strings.Contains(r.HTML, "A&amp;B") || !strings.Contains(r.Text, "Go: https://x.example/go") ||
		strings.Contains(r.Text, "[[") {
		t.Fatalf("%s\n%s", r.Text, r.HTML)
	}
}
