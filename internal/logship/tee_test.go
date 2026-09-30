package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/shield"
)

// drainSpool writes what's queued and flushes it (no Run loop in these
// tests).
func drainSpool(s *Spool) {
	s.drain()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		f.w.Flush()
	}
}

// The daemon's log goes to its handler as before and, once a spool that
// ships it is attached, to the spool too, with its attributes and groups.
func TestTee(t *testing.T) {
	var journal bytes.Buffer
	tee := NewTee(slog.NewJSONHandler(&journal, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log := slog.New(tee).With("svc", "x")
	log.Info("before any spool")

	sp, _ := newSpool(t)
	tee.Attach(sp)
	log.WithGroup("g").Info("hello", "k", 1)
	log.Debug("below the level") // the wrapped handler's level applies
	sp.Configure(map[string]bool{TypeSecurity: true}, 1<<30)
	log.Info("daemon type off")
	drainSpool(sp)

	if n := strings.Count(journal.String(), "\n"); n != 3 {
		t.Errorf("journal got %d records:\n%s", n, journal.String())
	}
	got := readSpool(t, sp.Dir, TypeDaemon)
	if len(got) != 1 {
		t.Fatalf("spool: %q", got)
	}
	var rec map[string]any
	json.Unmarshal([]byte(got[0]), &rec)
	if rec["msg"] != "hello" || rec["svc"] != "x" || rec["g"].(map[string]any)["k"] != 1.0 || rec["level"] != "INFO" {
		t.Errorf("record: %s", got[0])
	}
}

func TestTaps(t *testing.T) {
	s, _, _ := newService(t)
	enable(t, s, nil)
	idx := map[string]string{"a.test": "s1"}
	s.sites.Store(&idx)

	s.Security(shield.Event{Time: time.Unix(1790000000, 0).UTC(), Site: "s1", IP: "203.0.113.9", Verdict: "ban", Reason: "login flood"})
	s.WAF([]proxy.WAFEvent{{Time: time.Unix(1790000001, 0), Host: "a.test", IP: "198.51.100.7", Method: "POST", Path: "/x",
		Blocked: true, Rules: []proxy.WAFRule{{ID: 942100, Msg: "SQL Injection", Target: "ARGS:id"}}}})
	s.PHPErrors("s1", []byte("[29-Sep-2026 10:00:00 UTC] PHP Fatal error:  Boom in /x.php:3\nStack trace:\n#0 /y.php(1): f()\n"+
		"[29-Sep-2026 10:00:01 Europe/Paris] PHP Notice:  Hmm\n"))
	drainSpool(s.spool)

	sec := readSpool(t, s.spool.Dir, TypeSecurity)
	if len(sec) != 1 || !strings.Contains(sec[0], `"verdict":"ban"`) {
		t.Errorf("security: %q", sec)
	}
	waf := readSpool(t, s.spool.Dir, TypeWAF)
	if len(waf) != 1 || !strings.Contains(waf[0], `"site":"s1"`) || !strings.Contains(waf[0], `"id":942100`) {
		t.Errorf("waf: %q", waf)
	}
	php := readSpool(t, s.spool.Dir, TypePHPErrors)
	if len(php) != 2 {
		t.Fatalf("php: %q", php)
	}
	var e phpEntry
	json.Unmarshal([]byte(php[0]), &e)
	if e.Site != "s1" || e.Level != "fatal error" || !strings.Contains(e.Message, "#0 /y.php(1)") ||
		!e.Time.Equal(time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("php entry: %+v", e)
	}
	json.Unmarshal([]byte(php[1]), &e)
	if !e.Time.Equal(time.Date(2026, 9, 29, 8, 0, 1, 0, time.UTC)) || e.Level != "notice" {
		t.Errorf("php entry in Paris time: %+v", e)
	}

	// Off: the taps write nothing.
	set, _ := s.Settings(context.Background())
	set.Enabled = false
	s.SetSettings(context.Background(), set.Redacted())
	s.Security(shield.Event{Site: "s1", Verdict: "block"})
	drainSpool(s.spool)
	if got := readSpool(t, s.spool.Dir, TypeSecurity); len(got) != 1 {
		t.Errorf("written while off: %q", got)
	}
}
