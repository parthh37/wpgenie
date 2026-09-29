package monitor

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// smtpServer is a minimal SMTP server: EHLO, STARTTLS (if offered),
// AUTH PLAIN, MAIL, RCPT, DATA, QUIT. It records what it was given.
type smtpServer struct {
	addr     string
	mu       sync.Mutex
	auth     string // "user:password"
	tls      bool   // the message arrived over TLS
	from, to []string
	data     string
}

func newSMTPServer(t *testing.T, cert tls.Certificate, implicit, offerSTARTTLS bool) *smtpServer {
	t.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	var ln net.Listener
	var err error
	if implicit {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", cfg)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
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
			go s.serve(c, cfg, implicit, offerSTARTTLS)
		}
	}()
	return s
}

func (s *smtpServer) serve(c net.Conn, cfg *tls.Config, secure, offer bool) {
	defer c.Close()
	r, w := bufio.NewReader(c), c
	say := func(l string) { io.WriteString(w, l+"\r\n") }
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
			if offer && !secure {
				say("250-test")
				say("250-STARTTLS")
				say("250 AUTH PLAIN")
			} else {
				say("250-test")
				say("250 AUTH PLAIN")
			}
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, cfg)
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
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.mu.Lock()
			s.from = append(s.from, cmd[len("MAIL FROM:"):])
			s.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			s.mu.Lock()
			s.to = append(s.to, cmd[len("RCPT TO:"):])
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
			s.data, s.tls = b.String(), secure
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

func testMessage() Message {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	return Message{Server: "host1", Time: now, Notices: []Notice{{Event: EventFiring, Alert: store.Alert{
		Key: "site_down:s1", Kind: KindSiteDown, SiteID: "s1", Target: "example.com", Severity: SevCritical,
		State: store.AlertFiring, Message: "example.com is down: HTTP 502\r\nBcc: evil@example.net", Since: now}}}}
}

func TestEmailDelivery(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.leaf(t, time.Now().Add(24*time.Hour), "127.0.0.1")
	for _, tc := range []struct {
		name            string
		implicit, offer bool
		mode            string
		fails           bool
	}{
		{"starttls", false, true, TLSStartTLS, false},
		{"implicit tls", true, false, TLSImplicit, false},
		// Required STARTTLS: a server that doesn't offer it gets nothing.
		{"no starttls offered", false, false, TLSStartTLS, true},
	} {
		srv := newSMTPServer(t, cert, tc.implicit, tc.offer)
		host, port, _ := net.SplitHostPort(srv.addr)
		p, _ := strconv.Atoi(port)
		e := Email{Enabled: true, Host: host, Port: p, TLS: tc.mode, Username: "alerts", Password: "s3cret",
			From: "alerts@example.com", To: []string{"ops@example.com", "dev@example.com"}}
		n := &Notifier{roots: ca.pool}
		err := n.sendEmail(context.Background(), e, testMessage())
		if tc.fails {
			srv.mu.Lock()
			if err == nil || srv.auth != "" || srv.data != "" {
				t.Errorf("%s: err %v, auth %q: credentials or mail sent", tc.name, err, srv.auth)
			}
			srv.mu.Unlock()
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		srv.mu.Lock()
		if srv.auth != "alerts:s3cret" || !srv.tls || len(srv.to) != 2 || !strings.Contains(srv.from[0], "alerts@example.com") {
			t.Errorf("%s: auth %q tls %v from %v to %v", tc.name, srv.auth, srv.tls, srv.from, srv.to)
		}
		head, body, _ := strings.Cut(srv.data, "\r\n\r\n")
		if !strings.Contains(head, "Subject: [WPGenie] CRITICAL: example.com is down: HTTP 502  Bcc: evil@example.net") ||
			strings.Contains(head, "\r\nBcc:") || !strings.Contains(body, "since 2026-09-30 12:00 UTC · site s1") {
			t.Errorf("%s: message:\n%s", tc.name, srv.data)
		}
		srv.mu.Unlock()
	}
}

func TestWebhookDeliveryAndSignature(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	fail := 1
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		got, bodies = append(got, r), append(bodies, b)
		if fail > 0 {
			fail--
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	wh := Webhook{ID: "w1", Name: "chat", Enabled: true, URL: srv.URL + "/hook", Secret: "0123456789abcdef-secret"}
	// Loopback is refused outside tests: alerts must not reach the
	// daemon's own APIs.
	if err := (&Notifier{roots: roots}).sendWebhook(context.Background(), wh, testMessage()); err == nil ||
		!strings.Contains(err.Error(), errForbiddenAddr.Error()) {
		t.Fatalf("loopback webhook: %v", err)
	}

	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryDelays = old }()
	s := &Service{Store: newStore(t), Log: quietLog(), Notifier: &Notifier{roots: roots, allowLoopback: true}}
	s.init()
	set := DefaultSettings()
	set.Webhooks = []Webhook{wh}
	if !s.dispatch(context.Background(), set, testMessage()) {
		t.Fatal("not dispatched")
	}
	if !s.wait(5 * time.Second) {
		t.Fatal("delivery hangs")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("deliveries: %d, want a failure and a retry", len(got))
	}
	r, body := got[1], bodies[1]
	ts := r.Header.Get(TimestampHeader)
	if ts != "1790769600" || r.Header.Get(SignatureHeader) != "sha256="+Sign(wh.Secret, ts, body) ||
		r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v", r.Header)
	}
	var p struct {
		Text, Content string
		Alerts        []Notice
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "example.com is down") || p.Content == "" || len(p.Alerts) != 1 || p.Alerts[0].Alert.SiteID != "s1" {
		t.Errorf("payload: %s", body)
	}
}

func TestMessageSubject(t *testing.T) {
	m := testMessage()
	a := m.Notices[0].Alert
	m.Notices = append(m.Notices, Notice{Event: EventResolved, Alert: a}, Notice{Event: EventReminder, Alert: a},
		Notice{Event: EventFiring, Alert: a})
	if got := m.Subject(); got != "[WPGenie] Alerts on host1: 2 firing, 1 still firing, 1 resolved" {
		t.Errorf("subject %q", got)
	}
	if strings.ContainsAny(oneLine("a\r\nb\x00c", 100), "\r\n\x00") || len(oneLine(strings.Repeat("x", 500), 10)) > 14 {
		t.Error("oneLine")
	}
}
