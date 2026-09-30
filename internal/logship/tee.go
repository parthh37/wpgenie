package logship

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/shield"
)

// ---- The daemon's own log ----

// Tee is a slog.Handler that passes every record to the handler it wraps
// (journald, through stderr) and, once a spool is attached and the daemon
// type ships, also writes it to the spool as the same JSON line. The
// logger exists before the spool, hence Attach.
type Tee struct {
	inner  slog.Handler
	side   slog.Handler
	target *teeTarget
}

// teeTarget is shared by a Tee and every handler derived from it (With…).
type teeTarget struct {
	spool atomic.Pointer[Spool]
}

// Write takes one record from the side JSON handler (it writes each in a
// single call) and queues it without waiting.
func (t *teeTarget) Write(p []byte) (int, error) {
	if s := t.spool.Load(); s != nil {
		s.Write(TypeDaemon, p)
	}
	return len(p), nil
}

// NewTee wraps inner.
func NewTee(inner slog.Handler) *Tee {
	t := &teeTarget{}
	return &Tee{inner: inner, side: slog.NewJSONHandler(t, &slog.HandlerOptions{Level: slog.LevelDebug}), target: t}
}

// Attach starts copying records to s (nil stops).
func (h *Tee) Attach(s *Spool) { h.target.spool.Store(s) }

func (h *Tee) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h *Tee) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	if s := h.target.spool.Load(); s != nil && s.Accepts(TypeDaemon) {
		h.side.Handle(ctx, r)
	}
	return err
}

func (h *Tee) WithAttrs(as []slog.Attr) slog.Handler {
	return &Tee{inner: h.inner.WithAttrs(as), side: h.side.WithAttrs(as), target: h.target}
}

func (h *Tee) WithGroup(name string) slog.Handler {
	return &Tee{inner: h.inner.WithGroup(name), side: h.side.WithGroup(name), target: h.target}
}

// ---- Taps in other packages' readers ----

// Security takes an event of the shield's security log
// (shield.TeeEvents). It's called on the request path: it only queues.
func (s *Service) Security(e shield.Event) { s.spool.WriteJSON(TypeSecurity, e) }

// wafEntry is a WAF match as shipped: what the security log shows, with
// every rule (the matched data is left out, as there: it's a piece of what
// the visitor sent).
type wafEntry struct {
	Time    time.Time       `json:"time"`
	Site    string          `json:"site,omitempty"`
	Host    string          `json:"host"`
	IP      string          `json:"ip"`
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Blocked bool            `json:"blocked"`
	Rules   []proxy.WAFRule `json:"rules"`
}

// WAF takes the entries proxy.WAFLog read (its Tee).
func (s *Service) WAF(events []proxy.WAFEvent) {
	if !s.spool.Accepts(TypeWAF) {
		return
	}
	for _, e := range events {
		rules := e.Rules
		if rules == nil {
			rules = []proxy.WAFRule{}
		}
		s.spool.WriteJSON(TypeWAF, wafEntry{Time: e.Time.UTC(), Site: s.siteOf(e.Host), Host: e.Host, IP: e.IP,
			Method: e.Method, Path: e.Path, Blocked: e.Blocked, Rules: rules})
	}
}

// phpEntry is one entry of a site's PHP error log (its stack trace
// included).
type phpEntry struct {
	Time    time.Time `json:"time"`
	Site    string    `json:"site"`
	Level   string    `json:"level,omitempty"`
	Message string    `json:"message"`
}

var (
	// [29-Sep-2026 10:11:12 UTC] PHP Warning:  message
	phpHead  = regexp.MustCompile(`^\[(\d{2}-[A-Za-z]{3}-\d{4} \d{2}:\d{2}:\d{2})(?: ([A-Za-z_/+-]+))?\] (.*)$`)
	phpLevel = regexp.MustCompile(`^PHP ([A-Za-z ]+?):\s+`)
)

// maxPHPEntry bounds one entry (a stack trace of a deep recursion).
const maxPHPEntry = 64 << 10

// PHPErrors takes the whole lines the site service read from a site's PHP
// error log (its PHPLogTee): one record per entry.
func (s *Service) PHPErrors(siteID string, lines []byte) {
	if !s.spool.Accepts(TypePHPErrors) {
		return
	}
	for _, e := range parsePHPEntries(lines, s.now()) {
		e.Site = siteID
		s.spool.WriteJSON(TypePHPErrors, e)
	}
}

func parsePHPEntries(data []byte, now time.Time) []phpEntry {
	var out []phpEntry
	var cur *phpEntry
	var msg strings.Builder
	flush := func() {
		if cur != nil {
			cur.Message = strings.ToValidUTF8(strings.TrimRight(msg.String(), "\n"), "?")
			out = append(out, *cur)
		}
		cur = nil
		msg.Reset()
	}
	for line := range bytes.Lines(data) {
		text := strings.TrimRight(string(line), "\r\n")
		m := phpHead.FindStringSubmatch(text)
		if m == nil {
			if cur == nil {
				cur = &phpEntry{Time: now.UTC()} // lines before the first entry
			}
			if msg.Len()+len(text) < maxPHPEntry {
				msg.WriteString(text + "\n")
			}
			continue
		}
		flush()
		at := now.UTC()
		if t, err := time.Parse("02-Jan-2006 15:04:05", m[1]); err == nil {
			if loc, err := time.LoadLocation(m[2]); err == nil && m[2] != "" {
				t, _ = time.ParseInLocation("02-Jan-2006 15:04:05", m[1], loc)
			}
			at = t.UTC()
		}
		cur = &phpEntry{Time: at}
		body := m[3]
		if l := phpLevel.FindStringSubmatch(body); l != nil {
			cur.Level = strings.ToLower(l[1])
		}
		msg.WriteString(body[:min(len(body), maxPHPEntry)] + "\n")
	}
	flush()
	return out
}
