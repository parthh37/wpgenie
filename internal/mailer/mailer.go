// Package mailer sends the panel's e-mail to people: invoices, payment
// reminders, welcome messages, support ticket replies. (Alerts to staff
// are internal/monitor's.)
//
// Messages are rendered from templates when queued and stored in the
// outbox (store.MailMessage), which a background loop delivers with
// retries; the outbox is also the e-mail log staff and clients see.
// Features register their templates' defaults with Register; staff can
// override the subject and body of each from the panel.
package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

var (
	ErrInvalid  = errors.New("invalid input")
	ErrDisabled = errors.New("outgoing e-mail is not configured")
)

// SettingKey holds the SMTP settings (JSON) in the settings table.
const SettingKey = "mailer"

// TLS modes.
const (
	TLSStartTLS = "starttls" // submission, usually port 587
	TLSImplicit = "tls"      // SMTPS, usually port 465
	TLSNone     = "none"     // only without a password (a local relay)
)

// Settings is the SMTP server the panel sends through. The password is
// write-only: the API shows PasswordSet.
type Settings struct {
	Enabled     bool   `json:"enabled"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	TLS         string `json:"tls"`
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	FromName    string `json:"from_name"`
	FromAddress string `json:"from_address"`
	// ReplyTo is the default Reply-To ("": none); messages may set their own.
	ReplyTo string `json:"reply_to"`
	// BCC receives a copy of every message ("": none), for records.
	BCC string `json:"bcc"`
	// PasswordSet is output only.
	PasswordSet bool `json:"password_set"`
}

// Redacted is what the API returns.
func (s Settings) Redacted() Settings {
	s.PasswordSet = s.Password != ""
	s.Password = ""
	return s
}

// SettingsInput changes the settings; a nil Password keeps the stored one
// and "" clears it.
type SettingsInput struct {
	Enabled     bool    `json:"enabled"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	TLS         string  `json:"tls"`
	Username    string  `json:"username"`
	Password    *string `json:"password"`
	FromName    string  `json:"from_name"`
	FromAddress string  `json:"from_address"`
	ReplyTo     string  `json:"reply_to"`
	BCC         string  `json:"bcc"`
}

// Brand is who the e-mail comes from, for the layout.
type Brand struct {
	Name    string // company or panel name
	URL     string // their website ("": none)
	LogoURL string // absolute URL of the logo ("": none)
	Footer  string // address, registration numbers… (plain text)
}

// Message is an e-mail to queue: either a template and its data, or a
// subject and text written by the caller.
type Message struct {
	To []string
	// Template and Data: rendered with the (possibly overridden) template.
	Template string
	Data     map[string]any
	// Subject and Text: used when Template is "".
	Subject string
	Text    string
	// ReplyTo overrides the settings' default.
	ReplyTo string
	// AccountID files the message under a client account (0: none).
	AccountID int64
	// DedupeKey: a message with the same key is only ever queued once
	// ("": no deduplication).
	DedupeKey string
}

type Service struct {
	Store *store.Store
	Log   *slog.Logger
	// Brand returns the layout's branding (nil: "WPGenie").
	Brand func(ctx context.Context) Brand
	// PanelURL is the panel's public origin, available to templates as
	// {{.PanelURL}}.
	PanelURL string
	Now      func() time.Time

	// For tests: trust these roots.
	Roots *x509.CertPool

	wake    chan struct{}
	wakeMu  sync.Mutex
	sending sync.Mutex
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Settings returns the stored settings (secrets included).
func (s *Service) Settings(ctx context.Context) (*Settings, error) {
	v, err := s.Store.Setting(ctx, SettingKey)
	if err != nil {
		return nil, err
	}
	out := &Settings{Port: 587, TLS: TLSStartTLS}
	if v != "" {
		if err := json.Unmarshal([]byte(v), out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetSettings validates and stores new settings.
func (s *Service) SetSettings(ctx context.Context, in SettingsInput) (*Settings, error) {
	cur, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	next := Settings{Enabled: in.Enabled, Host: strings.TrimSpace(in.Host), Port: in.Port, TLS: in.TLS,
		Username: strings.TrimSpace(in.Username), Password: cur.Password, FromName: strings.TrimSpace(in.FromName),
		FromAddress: strings.TrimSpace(in.FromAddress), ReplyTo: strings.TrimSpace(in.ReplyTo), BCC: strings.TrimSpace(in.BCC)}
	if in.Password != nil {
		next.Password = *in.Password
	} else if next.Host == "" {
		next.Password = "" // no server: nothing to sign in to
	} else if cur.Password != "" && (next.Host != cur.Host || next.Port != cur.Port || next.Username != cur.Username) {
		// The stored password is only ever sent where it was entered for:
		// otherwise changing the host would hand it to another server.
		return nil, fmt.Errorf("%w: enter the password again for the new server", ErrInvalid)
	}
	if err := validate(&next); err != nil {
		return nil, err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetSetting(ctx, SettingKey, string(b)); err != nil {
		return nil, err
	}
	s.poke()
	return &next, nil
}

func validate(st *Settings) error {
	if st.TLS == "" {
		st.TLS = TLSStartTLS
	}
	if st.Port == 0 {
		st.Port = map[string]int{TLSStartTLS: 587, TLSImplicit: 465, TLSNone: 25}[st.TLS]
	}
	if !st.Enabled && st.Host == "" {
		return nil
	}
	if st.Host == "" || len(st.Host) > 253 || strings.ContainsAny(st.Host, " /\r\n") {
		return fmt.Errorf("%w: SMTP host", ErrInvalid)
	}
	if st.Port < 1 || st.Port > 65535 {
		return fmt.Errorf("%w: SMTP port", ErrInvalid)
	}
	switch st.TLS {
	case TLSStartTLS, TLSImplicit:
	case TLSNone:
		// The password would cross the network in clear.
		if st.Username != "" || st.Password != "" {
			return fmt.Errorf("%w: sign-in needs TLS (starttls or tls); none is only for a relay without a password", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: tls must be starttls, tls or none", ErrInvalid)
	}
	if strings.ContainsAny(st.Username+st.Password, "\r\n") {
		return fmt.Errorf("%w: credentials can't contain line breaks", ErrInvalid)
	}
	if _, err := parseAddress(st.FromAddress); err != nil {
		return fmt.Errorf("%w: from address: %v", ErrInvalid, err)
	}
	if len(st.FromName) > 100 || strings.ContainsAny(st.FromName, "\r\n") {
		return fmt.Errorf("%w: from name", ErrInvalid)
	}
	for _, a := range []string{st.ReplyTo, st.BCC} {
		if a == "" {
			continue
		}
		if _, err := parseAddress(a); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalid, a, err)
		}
	}
	return nil
}

// parseAddress accepts a bare address (no display name, no line breaks).
func parseAddress(a string) (string, error) {
	if a == "" {
		return "", errors.New("required")
	}
	if len(a) > 254 || strings.ContainsAny(a, "\r\n<>, ") {
		return "", errors.New("not an e-mail address")
	}
	p, err := mail.ParseAddress(a)
	if err != nil || p.Address != a || !strings.Contains(a, "@") {
		return "", errors.New("not an e-mail address")
	}
	return a, nil
}

// ValidAddress reports whether a is a bare e-mail address the mailer
// accepts as a recipient.
func ValidAddress(a string) bool {
	_, err := parseAddress(a)
	return err == nil
}

// Queue renders a message and puts it in the outbox. It returns false
// when a message with the same dedupe key was queued before. Messages are
// queued even while sending is off, and go once it is configured.
func (s *Service) Queue(ctx context.Context, m Message) (bool, error) {
	if len(m.To) == 0 {
		return false, fmt.Errorf("%w: no recipient", ErrInvalid)
	}
	for _, a := range m.To {
		if !ValidAddress(a) {
			return false, fmt.Errorf("%w: recipient %q", ErrInvalid, a)
		}
	}
	if m.ReplyTo != "" && !ValidAddress(m.ReplyTo) {
		return false, fmt.Errorf("%w: reply-to %q", ErrInvalid, m.ReplyTo)
	}
	if m.DedupeKey != "" {
		// Don't render (or look up anything for) a message already sent.
		if seen, err := s.Store.MailQueued(ctx, m.DedupeKey); err != nil || seen {
			return false, err
		}
	}
	r, err := s.render(ctx, m)
	if err != nil {
		return false, err
	}
	queued, err := s.Store.EnqueueMail(ctx, &store.MailMessage{DedupeKey: m.DedupeKey, AccountID: m.AccountID,
		Template: m.Template, To: m.To, ReplyTo: m.ReplyTo, Subject: r.Subject, Text: r.Text, HTML: r.HTML}, s.now())
	if err == nil && queued {
		s.poke()
	}
	return queued, err
}

// render fills the template (or wraps the caller's text) in the layout.
func (s *Service) render(ctx context.Context, m Message) (*Rendered, error) {
	brand := s.brand(ctx)
	if m.Template == "" {
		if m.Subject == "" || m.Text == "" {
			return nil, fmt.Errorf("%w: a message needs a template, or a subject and text", ErrInvalid)
		}
		return layout(brand, oneLine(m.Subject, 250), m.Text), nil
	}
	return s.Render(ctx, m.Template, m.Data)
}

func (s *Service) brand(ctx context.Context) Brand {
	b := Brand{Name: "WPGenie"}
	if s.Brand != nil {
		if got := s.Brand(ctx); got.Name != "" {
			b = got
		}
	}
	return b
}

// Resend queues a logged message again.
func (s *Service) Resend(ctx context.Context, id int64) error {
	if err := s.Store.RetryMail(ctx, id, s.now()); err != nil {
		return err
	}
	s.poke()
	return nil
}

func (s *Service) wakeCh() chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

func (s *Service) poke() {
	select {
	case s.wakeCh() <- struct{}{}:
	default:
	}
}

// Run delivers the outbox until ctx ends: on every Queue, and every
// 30 seconds for retries.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.DeliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wakeCh():
		}
	}
}

// retryDelays are the waits before each retry; after the last the
// message is failed (staff can resend it from the log).
var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// DeliverDue sends every due message once.
func (s *Service) DeliverDue(ctx context.Context) {
	s.sending.Lock()
	defer s.sending.Unlock()
	cfg, err := s.Settings(ctx)
	if err != nil {
		s.Log.Warn("mailer: settings", "err", err)
		return
	}
	// Messages that couldn't go for a week (sending off, or refused all
	// along) are dropped: a payment reminder weeks late is worse than none.
	if n, err := s.Store.ExpireMail(ctx, s.now().Add(-maxPending), "not sent within 7 days"); err != nil {
		s.Log.Warn("mailer: expiring old messages", "err", err)
	} else if n > 0 {
		s.Log.Warn("mailer: dropped messages that couldn't be sent for a week", "count", n)
	}
	if !cfg.Enabled || cfg.Host == "" {
		return // queued until sending is configured
	}
	for {
		due, err := s.Store.DueMail(ctx, s.now(), 20)
		if err != nil {
			s.Log.Warn("mailer: reading the outbox", "err", err)
			return
		}
		if len(due) == 0 {
			return
		}
		for _, m := range due {
			if ctx.Err() != nil {
				return
			}
			// A message whose outcome can't be recorded would be due
			// again at once: stop the round rather than resend it in a loop.
			if err := s.attempt(ctx, cfg, m); err != nil {
				s.Log.Warn("mailer: recording a delivery; stopping this round", "id", m.ID, "err", err)
				return
			}
		}
	}
}

// maxPending is how long a message may wait to be sent.
const maxPending = 7 * 24 * time.Hour

func (s *Service) attempt(ctx context.Context, cfg *Settings, m *store.MailMessage) error {
	sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	rejected, err := s.send(sctx, cfg, m)
	cancel()
	m.Attempts++
	if err == nil {
		m.Status, m.SentAt, m.LastError = store.MailSent, s.now(), ""
		if len(rejected) > 0 {
			m.LastError = oneLine("the server refused "+strings.Join(rejected, "; "), 500)
		}
	} else {
		m.LastError = oneLine(err.Error(), 500)
		if m.Attempts > len(retryDelays) {
			m.Status = store.MailFailed
			s.Log.Warn("mailer: giving up on a message", "id", m.ID, "to", m.To, "err", err)
		} else {
			m.NextAttemptAt = s.now().Add(retryDelays[m.Attempts-1])
		}
	}
	return s.Store.RecordMail(context.WithoutCancel(ctx), m)
}

// Test sends a message to one address now, bypassing the outbox, and
// returns the SMTP server's error, if any.
func (s *Service) Test(ctx context.Context, to string) error {
	if !ValidAddress(to) {
		return fmt.Errorf("%w: recipient", ErrInvalid)
	}
	cfg, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	if cfg.Host == "" {
		return ErrDisabled
	}
	brand := s.brand(ctx)
	r := layout(brand, "Test message from "+brand.Name,
		"This is a test message from "+brand.Name+".\n\nIf you can read it, the panel can send e-mail.")
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err = s.send(ctx, cfg, &store.MailMessage{To: []string{to}, Subject: r.Subject, Text: r.Text, HTML: r.HTML})
	return err
}

// send delivers a message. A recipient the server refuses is skipped and
// reported (one bad address mustn't stop the others); only a message that
// no recipient accepts fails.
func (s *Service) send(ctx context.Context, cfg *Settings, m *store.MailMessage) (rejected []string, _ error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12, RootCAs: s.Roots}
	d := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if cfg.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer c.Close()
	if cfg.TLS == TLSStartTLS {
		// Required, not opportunistic: without it the password (and the
		// invoices) would cross in clear.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return nil, errors.New("the SMTP server doesn't offer STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return nil, err
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return nil, err
		}
	}
	if err := c.Mail(cfg.FromAddress); err != nil {
		return nil, err
	}
	rcpts := append([]string{}, m.To...)
	if cfg.BCC != "" {
		rcpts = append(rcpts, cfg.BCC)
	}
	accepted := 0 // of the message's own recipients (the BCC copy doesn't count)
	for i, to := range rcpts {
		if err := c.Rcpt(to); err != nil {
			rejected = append(rejected, fmt.Sprintf("%s (%v)", to, err))
			continue
		}
		if i < len(m.To) {
			accepted++
		}
	}
	if accepted == 0 {
		return nil, fmt.Errorf("no recipient accepted: %s", strings.Join(rejected, "; "))
	}
	w, err := c.Data()
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(compose(cfg, m, s.now())); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return rejected, c.Quit()
}

// compose builds the MIME message: text and HTML alternatives, both
// quoted-printable (no line longer than SMTP allows).
func compose(cfg *Settings, m *store.MailMessage, now time.Time) []byte {
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	from := cfg.FromAddress
	if cfg.FromName != "" {
		from = (&mail.Address{Name: cfg.FromName, Address: cfg.FromAddress}).String()
	}
	h("From", from)
	h("To", strings.Join(m.To, ", "))
	if rt := m.ReplyTo; rt != "" {
		h("Reply-To", rt)
	} else if cfg.ReplyTo != "" {
		h("Reply-To", cfg.ReplyTo)
	}
	h("Subject", mime.QEncoding.Encode("utf-8", oneLine(m.Subject, 250)))
	h("Date", now.Format(time.RFC1123Z))
	domain := "wpgenie.invalid"
	if _, d, ok := strings.Cut(cfg.FromAddress, "@"); ok {
		domain = d
	}
	h("Message-ID", "<"+randomHex(12)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	part := func(ctype, body string) {
		b.WriteString("Content-Type: " + ctype + "; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		qp := quotedprintable.NewWriter(&b)
		qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
		qp.Close()
		b.WriteString("\r\n")
	}
	if m.HTML == "" {
		part("text/plain", m.Text)
		return b.Bytes()
	}
	boundary := "wpg-" + randomHex(12)
	h("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n--" + boundary + "\r\n")
	part("text/plain", m.Text)
	b.WriteString("--" + boundary + "\r\n")
	part("text/html", m.HTML)
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// oneLine keeps header-safe text: no line breaks or control characters,
// bounded.
func oneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}
