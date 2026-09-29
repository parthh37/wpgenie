package monitor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Notifications. Every evaluation pass sends at most one message per
// channel, listing everything that changed: when Caddy stops, every site
// goes down at once, and one message saying so beats fifty. Deliveries run
// in the background with retries, so a slow mail server never delays the
// next evaluation.

// Notice is one alert in a notification.
type Notice struct {
	Event string      `json:"event"` // firing | resolved | reminder | test
	Alert store.Alert `json:"alert"`
}

const (
	EventFiring   = "firing"
	EventResolved = "resolved"
	EventReminder = "reminder"
	EventTest     = "test"
)

// Message is what the channels send.
type Message struct {
	Server  string
	Time    time.Time
	Notices []Notice
}

// Headers of signed webhook requests. The signature is
// "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body)); receivers
// should also refuse timestamps more than a few minutes old (replays).
const (
	SignatureHeader = "X-WPGenie-Signature"
	TimestampHeader = "X-WPGenie-Timestamp"
)

// retryDelays are the waits before each retry of a failed delivery.
var retryDelays = []time.Duration{15 * time.Second, time.Minute, 4 * time.Minute}

// maxInFlight bounds concurrent deliveries (retries included); beyond it
// new messages are dropped and logged rather than piling up goroutines.
const maxInFlight = 16

// Subject summarises a message in one line.
func (m Message) Subject() string {
	if len(m.Notices) == 1 {
		n := m.Notices[0]
		return fmt.Sprintf("[WPGenie] %s: %s", eventLabel(n), oneLine(n.Alert.Message, 150))
	}
	counts := map[string]int{}
	for _, n := range m.Notices {
		counts[n.Event]++
	}
	var parts []string
	for _, e := range []string{EventFiring, EventReminder, EventResolved, EventTest} {
		if c := counts[e]; c > 0 {
			label := map[string]string{EventFiring: "firing", EventReminder: "still firing", EventResolved: "resolved", EventTest: "test"}[e]
			parts = append(parts, fmt.Sprintf("%d %s", c, label))
		}
	}
	return "[WPGenie] Alerts on " + oneLine(m.Server, 100) + ": " + strings.Join(parts, ", ")
}

// Text is the plain-text body (also the chat message of webhooks).
func (m Message) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "WPGenie on %s, %s\n\n", oneLine(m.Server, 100), m.Time.UTC().Format("2006-01-02 15:04 MST"))
	for _, n := range m.Notices {
		a := n.Alert
		fmt.Fprintf(&b, "%s: %s\n", eventLabel(n), oneLine(a.Message, 500))
		switch n.Event {
		case EventFiring, EventReminder:
			fmt.Fprintf(&b, "  since %s", a.Since.UTC().Format("2006-01-02 15:04 MST"))
		case EventResolved:
			fmt.Fprintf(&b, "  resolved %s", a.UpdatedAt.UTC().Format("2006-01-02 15:04 MST"))
		}
		if a.SiteID != "" {
			fmt.Fprintf(&b, " · site %s", a.SiteID)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func eventLabel(n Notice) string {
	switch n.Event {
	case EventResolved:
		return "RESOLVED"
	case EventTest:
		return "TEST"
	case EventReminder:
		return "STILL " + strings.ToUpper(n.Alert.Severity)
	}
	return strings.ToUpper(n.Alert.Severity)
}

// oneLine keeps header-safe text: no line breaks or control characters
// (a domain or error message must never add a mail header), bounded.
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

// channel is one configured destination.
type channel struct {
	name string
	send func(ctx context.Context, m Message) error
}

// Notifier sends messages to e-mail and webhooks.
type Notifier struct {
	UserAgent string
	// For tests: trust these roots, and allow loopback destinations.
	roots         *x509.CertPool
	allowLoopback bool

	once   sync.Once
	client *http.Client
}

func (n *Notifier) channels(set Settings) []channel {
	var out []channel
	if e := set.Email; e.Enabled && e.Host != "" {
		out = append(out, channel{name: "email", send: func(ctx context.Context, m Message) error { return n.sendEmail(ctx, e, m) }})
	}
	for _, w := range set.Webhooks {
		if !w.Enabled || w.URL == "" {
			continue
		}
		name := "webhook " + w.ID
		if w.Name != "" {
			name = "webhook " + w.Name
		}
		out = append(out, channel{name: name, send: func(ctx context.Context, m Message) error { return n.sendWebhook(ctx, w, m) }})
	}
	return out
}

// errForbiddenAddr: webhooks may not reach this server itself or
// link-local addresses (cloud metadata endpoints), whatever the URL's name
// resolves to: an administrator's typo or a rebinding DNS name must not
// turn alerts into requests against the daemon's own APIs.
var errForbiddenAddr = errors.New("destination address not allowed for webhooks")

func (n *Notifier) dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errForbiddenAddr
	}
	if ip.IsLoopback() && n.allowLoopback {
		return nil
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return errForbiddenAddr
	}
	return nil
}

func (n *Notifier) httpClient() *http.Client {
	n.once.Do(func() {
		d := &net.Dialer{Timeout: 10 * time.Second, Control: n.dialControl}
		n.client = &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				DialContext:       d.DialContext,
				TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: n.roots},
				ForceAttemptHTTP2: true,
				// No proxy from the environment: the destination check above
				// would then only see the proxy.
				Proxy: nil,
			},
			// A redirect is either a misconfiguration or an attempt to move
			// the (signed) alert elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
	return n.client
}

// webhookPayload is Slack/Mattermost-compatible (text), Discord-compatible
// (content) and structured for anything else.
type webhookPayload struct {
	Text    string    `json:"text"`
	Content string    `json:"content"`
	Server  string    `json:"server"`
	SentAt  time.Time `json:"sent_at"`
	Alerts  []Notice  `json:"alerts"`
}

func (n *Notifier) sendWebhook(ctx context.Context, w Webhook, m Message) error {
	text := m.Subject() + "\n" + m.Text()
	// Discord refuses content over 2000 characters.
	content := text
	if len(content) > 1900 {
		content = strings.ToValidUTF8(content[:1900], "") + "…"
	}
	body, err := json.Marshal(webhookPayload{Text: text, Content: content, Server: m.Server, SentAt: m.Time.UTC(), Alerts: m.Notices})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(m.Time.Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", n.UserAgent)
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, "sha256="+Sign(w.Secret, ts, body))
	resp, err := n.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}

// Sign is the webhook signature: hex HMAC-SHA256 over timestamp "." body.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (n *Notifier) sendEmail(ctx context.Context, e Email, m Message) error {
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	tlsCfg := &tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12, RootCAs: n.roots}
	d := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if e.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	deadline := time.Now().Add(time.Minute)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if e.TLS == TLSStartTLS {
		// Required, not opportunistic: a server (or someone in the path)
		// that doesn't offer it gets nothing, the password least of all.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the SMTP server doesn't offer STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if e.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.Username, e.Password, e.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(e.From); err != nil {
		return err
	}
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(emailBody(e, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// emailBody renders the message. Addresses were validated when saved;
// everything else in the headers goes through oneLine.
func emailBody(e Email, m Message) []byte {
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", e.From)
	h("To", strings.Join(e.To, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject()))
	h("Date", m.Time.Format(time.RFC1123Z))
	h("Message-ID", "<"+randomHex(12)+"@wpgenie.invalid>")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "8bit")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	for _, line := range strings.Split(m.Text(), "\n") {
		b.WriteString(line + "\r\n")
	}
	return b.Bytes()
}

// deliver sends m with retries; failures are logged, never returned.
func (s *Service) deliver(ctx context.Context, ch channel, m Message) {
	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := ch.send(actx, m)
		cancel()
		if err == nil {
			return
		}
		if attempt >= len(retryDelays) {
			s.Log.Error("monitor: notification not delivered", "channel", ch.name, "attempts", attempt+1, "err", err)
			return
		}
		s.Log.Warn("monitor: notification failed, retrying", "channel", ch.name, "in", retryDelays[attempt], "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelays[attempt]):
		}
	}
}

// dispatch hands m to every configured channel in the background and
// reports whether any channel took it.
func (s *Service) dispatch(ctx context.Context, set Settings, m Message) bool {
	sent := false
	for _, ch := range s.Notifier.channels(set) {
		select {
		case s.inFlight <- struct{}{}:
			sent = true
			s.wg.Add(1)
			go func() {
				defer func() { <-s.inFlight; s.wg.Done() }()
				s.deliver(ctx, ch, m)
			}()
		default:
			s.Log.Error("monitor: too many notifications in flight; dropping one", "channel", ch.name)
		}
	}
	return sent
}

// ChannelResult is the outcome of a test notification.
type ChannelResult struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// TestChannels sends a test message to every configured channel once, now,
// and reports each outcome (no retries: the point is to see the error).
func (s *Service) TestChannels(ctx context.Context) ([]ChannelResult, error) {
	s.init()
	set, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	m := Message{Server: s.Server, Time: now, Notices: []Notice{{Event: EventTest, Alert: store.Alert{
		Key: "test", Kind: "test", Target: s.Server, Severity: "info", State: store.AlertResolved, Since: now, UpdatedAt: now,
		Message: "Test notification from WPGenie: alerts will arrive here"}}}}
	chans := s.Notifier.channels(set)
	out := make([]ChannelResult, len(chans))
	var wg sync.WaitGroup
	for i, ch := range chans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			out[i] = ChannelResult{Channel: ch.name, OK: true}
			if err := ch.send(cctx, m); err != nil {
				out[i] = ChannelResult{Channel: ch.name, Error: err.Error()}
			}
		}()
	}
	wg.Wait()
	return out, nil
}
