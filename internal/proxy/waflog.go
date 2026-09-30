package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// WAFAuditEntry is one line of Coraza's JSON audit log, as configured in
// waf/wpgenie.conf (parts A, H, Z: no request headers or bodies).
type WAFAuditEntry struct {
	Transaction struct {
		UnixNano int64  `json:"unix_timestamp"`
		ClientIP string `json:"client_ip"` // Caddy's client IP: the visitor, behind Cloudflare too
		ServerID string `json:"server_id"` // the Host header
		Request  struct {
			Method  string              `json:"method"`
			URI     string              `json:"uri"`
			Headers map[string][]string `json:"headers"`
		} `json:"request"`
		Interrupted bool `json:"is_interrupted"`
	} `json:"transaction"`
	Messages []struct {
		ErrorMessage string `json:"error_message"`
	} `json:"messages"`
}

// WAFRule is a rule that matched.
type WAFRule struct {
	ID     int    `json:"id"`
	Msg    string `json:"msg"`
	Target string `json:"target,omitempty"` // e.g. ARGS:id, where it matched
}

// WAFEvent is an audit entry digested for the security log.
type WAFEvent struct {
	Time    time.Time
	Host    string
	IP      string
	Method  string
	Path    string // without the query string
	Blocked bool   // false in detect mode
	Rules   []WAFRule
}

var (
	ruleIDRe  = regexp.MustCompile(`\[id "(\d+)"\]`)
	ruleMsgRe = regexp.MustCompile(`\[msg "([^"]*)"\]`)
	targetRe  = regexp.MustCompile(`found within ([A-Z_]+(?::[^\s:"\]]{1,80})?)`)
)

// scoringRules only total up other rules' findings.
var scoringRules = map[int]bool{949110: true, 949111: true, 980170: true, 980130: true}

// Event digests the entry. Matched data is left out on purpose: it is a
// piece of what the visitor sent, which may be personal.
func (e WAFAuditEntry) Event() WAFEvent {
	t := e.Transaction
	path, _, _ := strings.Cut(t.Request.URI, "?")
	host := strings.ToLower(t.ServerID)
	if h, _, ok := strings.Cut(host, ":"); ok && !strings.Contains(h, "]") {
		host = h
	}
	ev := WAFEvent{Time: time.Unix(0, t.UnixNano), Host: host, IP: t.ClientIP, Method: t.Request.Method,
		Path: path, Blocked: t.Interrupted}
	for _, m := range e.Messages {
		id := 0
		if s := ruleIDRe.FindStringSubmatch(m.ErrorMessage); s != nil {
			id, _ = strconv.Atoi(s[1])
		}
		if id == 0 || scoringRules[id] {
			continue
		}
		r := WAFRule{ID: id}
		if s := ruleMsgRe.FindStringSubmatch(m.ErrorMessage); s != nil {
			r.Msg = s[1]
		}
		if s := targetRe.FindStringSubmatch(m.ErrorMessage); s != nil {
			r.Target = s[1]
		}
		ev.Rules = append(ev.Rules, r)
	}
	return ev
}

// Reason is a one-line summary for the security log.
func (ev WAFEvent) Reason() string {
	if len(ev.Rules) == 0 {
		return "waf"
	}
	r := ev.Rules[0]
	s := "waf " + strconv.Itoa(r.ID) + ": " + r.Msg
	if r.Target != "" {
		s += " (" + r.Target + ")"
	}
	if n := len(ev.Rules) - 1; n > 0 {
		s += " +" + strconv.Itoa(n) + " more"
	}
	return s
}

// WAFLog follows the audit log Caddy's Coraza writes and hands new entries
// to Handle. It starts at the end of the file (the security log is about
// now) and truncates the file once it has read past MaxSize: Coraza opens
// it with O_APPEND, so its next write lands at the new end.
type WAFLog struct {
	Path    string
	MaxSize int64 // default 16 MB
	Handle  func([]WAFEvent)
	// Tee, if set, gets every entry read too, whatever its host (log
	// shipping: the file is truncated once read, so this is the only
	// chance to keep it).
	Tee func([]WAFEvent)
	Log *slog.Logger

	offset int64
	inode  uint64
	primed bool
}

// Run polls the log every couple of seconds until ctx is done.
func (w *WAFLog) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if err := w.poll(); err != nil && !errors.Is(err, os.ErrNotExist) && w.Log != nil {
			w.Log.Warn("reading the WAF audit log", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *WAFLog) poll() error {
	f, err := os.Open(w.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	ino := inode(info)
	switch {
	case !w.primed:
		w.primed, w.inode, w.offset = true, ino, info.Size()
		return nil
	case ino != w.inode || info.Size() < w.offset:
		w.inode, w.offset = ino, 0 // recreated or truncated
	}
	if info.Size() == w.offset {
		return nil
	}
	if _, err := f.Seek(w.offset, io.SeekStart); err != nil {
		return err
	}
	var events []WAFEvent
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next time
		}
		w.offset += int64(len(line))
		var e WAFAuditEntry
		if json.Unmarshal(bytes.TrimSpace(line), &e) == nil {
			events = append(events, e.Event())
		}
	}
	if len(events) > 0 && w.Tee != nil {
		w.Tee(events)
	}
	if len(events) > 0 && w.Handle != nil {
		w.Handle(events)
	}
	max := w.MaxSize
	if max == 0 {
		max = 16 << 20
	}
	if w.offset >= max {
		if err := os.Truncate(w.Path, 0); err != nil {
			return err
		}
		w.offset = 0
	}
	return nil
}

func inode(fi os.FileInfo) uint64 {
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino)
	}
	return 0
}
