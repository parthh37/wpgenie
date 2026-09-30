package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a settable time for the spool.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newSpool(t *testing.T) (*Spool, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 30, 14, 10, 0, 0, time.UTC)}
	s := NewSpool(t.TempDir(), quietLog())
	s.Now = c.now
	s.Configure(map[string]bool{TypeSecurity: true, TypeDaemon: true, TypeAudit: true}, 1<<30)
	mustf(t, s.Open(), "open")
	return s, c
}

func listFiles(t *testing.T, dir, typ string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(dir, typ))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Records are written to <type>/<hour>-<n>.jsonl.part and handed over
// (renamed) when the hour changes, the file is big, or a minute old.
func TestSpoolRotation(t *testing.T) {
	s, c := newSpool(t)
	s.Write(TypeSecurity, []byte(`{"n":1}`+"\n"))
	s.Write(TypeDaemon, []byte(`{"msg":"x"}`))
	s.Write(TypeWAF, []byte(`{"n":2}`)) // not configured: ignored
	for len(s.ch) > 0 {
		r := <-s.ch
		s.mu.Lock()
		s.writeLocked(r.typ, r.line)
		s.mu.Unlock()
	}
	if f := listFiles(t, s.Dir, TypeSecurity); len(f) != 1 || f[0] != "2026093014-1.jsonl.part" {
		t.Fatalf("security files: %v", f)
	}
	if f := listFiles(t, s.Dir, TypeWAF); len(f) != 0 {
		t.Errorf("an unconfigured type was written: %v", f)
	}
	s.tick() // flushed, not handed over yet
	if f := listFiles(t, s.Dir, TypeSecurity); !strings.HasSuffix(f[0], ".part") {
		t.Fatalf("handed over too early: %v", f)
	}
	c.add(spoolFileAge)
	s.tick()
	if f := listFiles(t, s.Dir, TypeSecurity); len(f) != 1 || f[0] != "2026093014-1.jsonl" {
		t.Fatalf("after a minute: %v", f)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, TypeSecurity, "2026093014-1.jsonl"))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"wpgenie_spool":"`) || lines[1] != `{"n":1}` {
		t.Fatalf("file: %q", b)
	}

	// Two files never start alike (Vector's fingerprint is the first line).
	s.mu.Lock()
	s.writeLocked(TypeSecurity, []byte(`{"n":1}`))
	s.mu.Unlock()
	c.add(spoolFileAge)
	s.tick()
	b2, _ := os.ReadFile(filepath.Join(s.Dir, TypeSecurity, "2026093014-3.jsonl"))
	if strings.SplitN(string(b2), "\n", 2)[0] == lines[0] {
		t.Error("two spool files share their first line")
	}

	// A new hour starts a new file; so does a full one.
	s.mu.Lock()
	s.writeLocked(TypeAudit, []byte(`{"a":1}`))
	c.add(time.Hour)
	s.writeLocked(TypeAudit, []byte(`{"a":2}`))
	s.files[TypeAudit].size = spoolFileMax
	s.writeLocked(TypeAudit, []byte(`{"a":3}`))
	s.mu.Unlock()
	f := listFiles(t, s.Dir, TypeAudit)
	if len(f) != 3 || !strings.HasPrefix(f[0], "2026093014-") || !strings.HasPrefix(f[1], "2026093015-") ||
		!strings.HasSuffix(f[2], ".part") {
		t.Errorf("audit files: %v", f)
	}
	// An empty file (only its first line) is removed, not handed over.
	s.mu.Lock()
	empty, err := s.openLocked(TypeAccountEvents, c.now())
	mustf(t, err, "open")
	s.files[TypeAccountEvents] = empty
	s.closeLocked(TypeAccountEvents)
	s.mu.Unlock()
	if f := listFiles(t, s.Dir, TypeAccountEvents); len(f) != 0 {
		t.Errorf("an empty file was handed over: %v", f)
	}

	counts := s.Take()
	if counts[TypeSecurity].Events != 2 || counts[TypeAudit].Events != 3 || counts[TypeSecurity].Bytes != 14 {
		t.Errorf("counts: %+v", counts)
	}
	if again := s.Take(); len(again) != 0 {
		t.Errorf("Take didn't reset: %+v", again)
	}
}

// Writers never wait: once the queue is full, records are dropped and
// counted.
func TestSpoolNeverBlocks(t *testing.T) {
	s, _ := newSpool(t)
	done := make(chan struct{})
	go func() {
		for i := range spoolQueue + 100 {
			s.Write(TypeSecurity, []byte(`{"i":`+strconv.Itoa(i)+`}`))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked with nobody reading the queue")
	}
	if c := s.Take()[TypeSecurity]; c.Dropped != 100 {
		t.Errorf("dropped %d, want 100", c.Dropped)
	}
	// Not one JSON line: kept as text.
	s2, _ := newSpool(t)
	s2.Write(TypeDaemon, []byte("two\nlines"))
	r := <-s2.ch
	var m map[string]string
	if json.Unmarshal(r.line, &m) != nil || m["message"] != "two\nlines" {
		t.Errorf("reformatted: %s", r.line)
	}
}

// Past the cap the oldest handed-over files go, their events counted as
// dropped; files being written stay.
func TestSpoolCap(t *testing.T) {
	s, c := newSpool(t)
	line := []byte(`{"pad":"` + strings.Repeat("x", 1000) + `"}`)
	for f := range 5 {
		s.mu.Lock()
		for range 100 {
			s.writeLocked(TypeSecurity, line)
		}
		s.closeLocked(TypeSecurity)
		s.mu.Unlock()
		path := filepath.Join(s.Dir, TypeSecurity, "2026093014-"+strconv.Itoa(f+1)+".jsonl")
		os.Chtimes(path, c.now(), c.now().Add(time.Duration(f)*time.Minute))
	}
	s.mu.Lock()
	s.writeLocked(TypeAudit, line) // being written
	s.mu.Unlock()
	s.Take()
	s.capBytes.Store(250 << 10) // room for two files
	s.enforceCap()
	f := listFiles(t, s.Dir, TypeSecurity)
	if len(f) != 2 || f[0] != "2026093014-4.jsonl" || f[1] != "2026093014-5.jsonl" {
		t.Errorf("kept %v, want the two newest", f)
	}
	if got := listFiles(t, s.Dir, TypeAudit); len(got) != 1 {
		t.Errorf("the file being written went: %v", got)
	}
	if d := s.Take()[TypeSecurity].Dropped; d != 300 {
		t.Errorf("dropped %d events, want 300", d)
	}
	if b, n := s.Usage(); n != 3 || b > 250<<10 || b == 0 {
		t.Errorf("usage %d bytes, %d files", b, n)
	}
}

// Files a crash left open are handed over (cut to whole lines), and new
// files are numbered after every existing one.
func TestSpoolRecover(t *testing.T) {
	s, _ := newSpool(t)
	dir := filepath.Join(s.Dir, TypeSecurity)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "2026093013-7.jsonl.part"), []byte("{\"wpgenie_spool\":\"a\"}\n{\"n\":1}\n{\"n\":"), 0o600)
	os.WriteFile(filepath.Join(dir, "2026093013-8.jsonl.part"), []byte("{\"wpgenie_spool\":\"b\"}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "2026093013-9.jsonl"), []byte("{\"wpgenie_spool\":\"c\"}\n{\"n\":2}\n"), 0o600)
	s.recover()
	if f := listFiles(t, s.Dir, TypeSecurity); len(f) != 2 || f[0] != "2026093013-7.jsonl" || f[1] != "2026093013-9.jsonl" {
		t.Fatalf("after recovery: %v", f)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "2026093013-7.jsonl"))
	if string(b) != "{\"wpgenie_spool\":\"a\"}\n{\"n\":1}\n" {
		t.Errorf("recovered file: %q", b)
	}
	s.mu.Lock()
	s.writeLocked(TypeSecurity, []byte(`{}`))
	s.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "2026093014-10.jsonl.part")); err != nil {
		t.Errorf("numbering after recovery: %v", listFiles(t, s.Dir, TypeSecurity))
	}
}

// Run writes what's queued and hands every open file over when it stops.
func TestSpoolRun(t *testing.T) {
	s, _ := newSpool(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	s.WriteJSON(TypeSecurity, map[string]string{"verdict": "block"})
	if err := s.WriteSync(TypeAudit, [][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}` + "\n")}); err != nil {
		t.Fatal(err)
	}
	// WriteSync's lines are on disk when it returns.
	if got := readSpool(t, s.Dir, TypeAudit); len(got) != 2 || got[1] != `{"id":2}` {
		t.Errorf("audit spool: %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	for _, typ := range []string{TypeSecurity, TypeAudit} {
		for _, f := range listFiles(t, s.Dir, typ) {
			if strings.HasSuffix(f, ".part") {
				t.Errorf("%s left open at shutdown", f)
			}
		}
	}
	if got := readSpool(t, s.Dir, TypeSecurity); len(got) != 1 || got[0] != `{"verdict":"block"}` {
		t.Errorf("security spool: %q", got)
	}
	if err := s.WriteSync("kernel", nil); err == nil {
		t.Error("WriteSync took an unknown type")
	}
}

// A file that can't be written (a full disk) is given up: removed, its
// events counted as dropped, the next record starting a new file; and the
// warnings about it are rate-limited (they may themselves be spooled).
func TestSpoolWriteFailure(t *testing.T) {
	s, c := newSpool(t)
	var warnings atomic.Int32
	s.Log = slog.New(countHandler{&warnings})
	s.mu.Lock()
	s.writeLocked(TypeSecurity, []byte(`{"n":1}`))
	s.writeLocked(TypeSecurity, []byte(`{"n":2}`))
	s.files[TypeSecurity].f.Close() // the next flush fails
	s.mu.Unlock()
	s.tick()
	if f := listFiles(t, s.Dir, TypeSecurity); len(f) != 0 {
		t.Errorf("a broken file was left: %v", f)
	}
	if d := s.Take()[TypeSecurity].Dropped; d != 2 {
		t.Errorf("dropped %d, want 2", d)
	}
	s.mu.Lock()
	err := s.writeLocked(TypeSecurity, []byte(`{"n":3}`))
	s.files[TypeSecurity].f.Close()
	s.mu.Unlock()
	s.tick()
	if err != nil || warnings.Load() != 1 {
		t.Errorf("write after a failure: %v; %d warnings, want 1 (rate-limited)", err, warnings.Load())
	}
	c.add(warnEvery)
	s.mu.Lock()
	s.writeLocked(TypeSecurity, []byte(`{"n":4}`))
	s.files[TypeSecurity].f.Close()
	s.mu.Unlock()
	s.tick()
	if warnings.Load() != 2 {
		t.Errorf("%d warnings after a minute, want 2", warnings.Load())
	}
}

type countHandler struct{ n *atomic.Int32 }

func (h countHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h countHandler) Handle(context.Context, slog.Record) error { h.n.Add(1); return nil }
func (h countHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h countHandler) WithGroup(string) slog.Handler             { return h }

// Records are one line each, never longer than the shipper reads.
func TestFitLine(t *testing.T) {
	if got := string(fitLine([]byte(`{"a":1}` + "\n\n"))); got != `{"a":1}` {
		t.Errorf("trimmed: %q", got)
	}
	huge := append([]byte(`{"x":"`), bytes.Repeat([]byte{0x01}, maxLine)...)
	for _, in := range [][]byte{huge, []byte("two\nlines")} {
		out := fitLine(in)
		var m map[string]string
		if len(out) > maxShippedLine || bytes.IndexByte(out, '\n') >= 0 || json.Unmarshal(out, &m) != nil || m["note"] == "" {
			t.Errorf("fitLine(%.20q…) = %d bytes: %.80s", in, len(out), out)
		}
	}
}
