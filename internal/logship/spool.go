package logship

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The spool holds the logs the daemon itself produces or rescues, one
// directory per type: <dir>/<type>/<YYYYMMDDHH>-<n>.jsonl, JSON lines.
// A file is written as <name>.part and renamed once complete (an hour
// passed, it's big enough, or it's a minute old), so Vector only ever sees
// whole files; it ships and deletes them. While the destination can't be
// reached files pile up, and past the cap the oldest are deleted (the
// events in them counted as dropped).
//
// Writers on the request path (the shield's events) never wait: records
// go through a buffered channel, and when it's full they're dropped and
// counted. Exporters, which can wait, write synchronously (WriteSync) and
// move their cursors only once their lines are on disk.

const (
	// spoolQueue is how many records may wait for the writer.
	spoolQueue = 8192
	// spoolFileMax and spoolFileAge bound a file before it's handed over.
	spoolFileMax = 8 << 20
	spoolFileAge = time.Minute
	// maxLine bounds one record (a runaway log line is cut).
	maxLine = 256 << 10
	// capEvery is how often the cap is enforced (and usage measured).
	capEvery = 10 * time.Second
)

// Spool is the daemon's side of log shipping. The zero value isn't usable:
// see NewSpool.
type Spool struct {
	Dir string
	Log *slog.Logger
	Now func() time.Time

	// accept: the types written (nil: none); capBytes: the cap.
	accept   atomic.Pointer[map[string]bool]
	capBytes atomic.Int64
	ch       chan record
	counts   map[string]*counters // every type's, created up front: read-only map

	mu    sync.Mutex // the open files, seq
	files map[string]*spoolFile
	seq   int64

	usageBytes, usageFiles atomic.Int64
}

type record struct {
	typ  string
	line []byte
}

// counters are a type's since they were last taken (see Take).
type counters struct {
	events, bytes, dropped atomic.Int64
}

// Counts is what a type amounted to since the last Take.
type Counts struct {
	Events, Bytes, Dropped int64
}

type spoolFile struct {
	part, final string
	f           *os.File
	w           *bufio.Writer
	size        int64
	events      int
	hour        string
	opened      time.Time
}

// NewSpool makes a spool in dir (created when it runs).
func NewSpool(dir string, log *slog.Logger) *Spool {
	s := &Spool{Dir: dir, Log: log, ch: make(chan record, spoolQueue), counts: map[string]*counters{},
		files: map[string]*spoolFile{}}
	for _, t := range Types {
		s.counts[t.Name] = &counters{}
	}
	return s
}

func (s *Spool) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Configure sets which types are written (nil: none) and the cap.
func (s *Spool) Configure(types map[string]bool, capBytes int64) {
	if types == nil {
		s.accept.Store(nil)
	} else {
		m := map[string]bool{}
		for k, v := range types {
			if v {
				m[k] = true
			}
		}
		s.accept.Store(&m)
	}
	s.capBytes.Store(capBytes)
}

// Accepts reports whether records of typ are written now.
func (s *Spool) Accepts(typ string) bool {
	m := s.accept.Load()
	return m != nil && (*m)[typ]
}

// Write queues one record (a JSON object on one line) without ever
// waiting: when the writer is behind, the record is dropped and counted.
// line is copied.
func (s *Spool) Write(typ string, line []byte) {
	if !s.Accepts(typ) {
		return
	}
	c := s.counts[typ]
	line = bytes.TrimRight(line, "\n")
	if len(line) == 0 {
		return
	}
	if len(line) > maxLine || bytes.IndexByte(line, '\n') >= 0 {
		// Not one JSON line (or a huge one): keep what it said, as text.
		msg := string(line[:min(len(line), maxLine)])
		line, _ = json.Marshal(map[string]string{"message": msg, "note": "cut or reformatted by WPGenie"})
	} else {
		line = bytes.Clone(line)
	}
	select {
	case s.ch <- record{typ, line}:
	default:
		c.dropped.Add(1)
	}
}

// WriteJSON queues v as one record (see Write).
func (s *Spool) WriteJSON(typ string, v any) {
	if !s.Accepts(typ) {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.Write(typ, b)
}

// WriteSync writes lines to typ's current file and flushes them: once it
// returns, they are on disk (they ship with the file).
func (s *Spool) WriteSync(typ string, lines [][]byte) error {
	if TypeByName(typ) == nil {
		return fmt.Errorf("unknown log type %q", typ)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range lines {
		if err := s.writeLocked(typ, bytes.TrimRight(l, "\n")); err != nil {
			return err
		}
	}
	if f := s.files[typ]; f != nil {
		return f.w.Flush()
	}
	return nil
}

// Take returns every type's counts since the last Take and resets them.
func (s *Spool) Take() map[string]Counts {
	out := map[string]Counts{}
	for name, c := range s.counts {
		if v := (Counts{c.events.Swap(0), c.bytes.Swap(0), c.dropped.Swap(0)}); v != (Counts{}) {
			out[name] = v
		}
	}
	return out
}

// Usage is the spool's size on disk and its number of files, as last
// measured.
func (s *Spool) Usage() (bytes, files int64) { return s.usageBytes.Load(), s.usageFiles.Load() }

// Open creates the spool's directory and recovers the files a crash left
// open (before anything is written: see recover).
func (s *Spool) Open() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	s.recover()
	s.enforceCap()
	return nil
}

// Run writes queued records until ctx ends, then closes the open files
// (so they ship).
func (s *Spool) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastCap := s.now()
	for {
		select {
		case <-ctx.Done():
			s.drain()
			s.mu.Lock()
			for typ := range s.files {
				s.closeLocked(typ)
			}
			s.mu.Unlock()
			return
		case r := <-s.ch:
			s.mu.Lock()
			if err := s.writeLocked(r.typ, r.line); err != nil {
				s.counts[r.typ].dropped.Add(1)
				s.Log.Warn("logship: writing the spool", "type", r.typ, "err", err)
			}
			s.mu.Unlock()
		case <-tick.C:
			s.tick()
			if s.now().Sub(lastCap) >= capEvery {
				lastCap = s.now()
				s.enforceCap()
			}
		}
	}
}

// drain writes what's queued (shutting down).
func (s *Spool) drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		select {
		case r := <-s.ch:
			s.writeLocked(r.typ, r.line)
		default:
			return
		}
	}
}

// tick flushes the open files and hands over those that are due.
func (s *Spool) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for typ, f := range s.files {
		if f.hour != hourOf(now) || (f.events > 0 && now.Sub(f.opened) >= spoolFileAge) {
			s.closeLocked(typ)
			continue
		}
		if err := f.w.Flush(); err != nil {
			s.Log.Warn("logship: writing the spool", "type", typ, "err", err)
		}
	}
}

func hourOf(t time.Time) string { return t.UTC().Format("2006010215") }

// writeLocked appends one line to typ's current file (opened, or rotated
// first, as needed). Caller holds mu.
func (s *Spool) writeLocked(typ string, line []byte) error {
	f := s.files[typ]
	now := s.now()
	if f != nil && (f.hour != hourOf(now) || f.size >= spoolFileMax) {
		s.closeLocked(typ)
		f = nil
	}
	if f == nil {
		var err error
		if f, err = s.openLocked(typ, now); err != nil {
			return err
		}
		s.files[typ] = f
	}
	n, err := f.w.Write(line)
	if err == nil {
		err = f.w.WriteByte('\n')
		n++
	}
	f.size += int64(n)
	if err != nil {
		return err
	}
	f.events++
	c := s.counts[typ]
	c.events.Add(1)
	c.bytes.Add(int64(len(line)))
	return nil
}

func (s *Spool) openLocked(typ string, now time.Time) (*spoolFile, error) {
	dir := filepath.Join(s.Dir, typ)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var final, part string
	var fh *os.File
	for range 1000 {
		s.seq++
		final = filepath.Join(dir, hourOf(now)+"-"+strconv.FormatInt(s.seq, 10)+".jsonl")
		part = final + ".part"
		var err error
		if fh, err = os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			break
		} else if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	if fh == nil {
		return nil, errors.New("no free spool file name")
	}
	f := &spoolFile{part: part, final: final, f: fh, w: bufio.NewWriterSize(fh, 64<<10), hour: hourOf(now), opened: now}
	// The first line makes the file unique to Vector's fingerprint (a
	// checksum of the first line: two files starting alike would be taken
	// for one). The shipper drops it.
	b := make([]byte, 12)
	rand.Read(b)
	n, _ := fmt.Fprintf(f.w, `{"wpgenie_spool":"%s"}`+"\n", hex.EncodeToString(b))
	f.size = int64(n)
	return f, nil
}

// closeLocked hands a file over to the shipper (or removes it when it
// holds nothing but its first line).
func (s *Spool) closeLocked(typ string) {
	f := s.files[typ]
	if f == nil {
		return
	}
	delete(s.files, typ)
	err := errors.Join(f.w.Flush(), f.f.Close())
	if f.events == 0 {
		os.Remove(f.part)
		return
	}
	if err == nil {
		err = os.Rename(f.part, f.final)
	}
	if err != nil {
		s.Log.Warn("logship: closing a spool file", "file", f.part, "err", err)
	}
}

var spoolNameRe = regexp.MustCompile(`^(\d{10})-(\d+)\.jsonl(\.part)?$`)

// recover hands over the files a crash left open (their last line cut to
// a whole one), and numbers new files after every existing one.
func (s *Spool) recover() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range Types {
		entries, err := os.ReadDir(filepath.Join(s.Dir, t.Name))
		if err != nil {
			continue
		}
		for _, e := range entries {
			m := spoolNameRe.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			if n, _ := strconv.ParseInt(m[2], 10, 64); n > s.seq {
				s.seq = n
			}
			if m[3] == "" {
				continue
			}
			part := filepath.Join(s.Dir, t.Name, e.Name())
			if err := recoverPart(part); err != nil {
				s.Log.Warn("logship: recovering a spool file", "file", part, "err", err)
			}
		}
	}
}

func recoverPart(part string) error {
	b, err := os.ReadFile(part)
	if err != nil {
		return err
	}
	b = b[:bytes.LastIndexByte(b, '\n')+1]
	if bytes.Count(b, []byte{'\n'}) < 2 { // nothing after the first line
		return os.Remove(part)
	}
	if err := os.WriteFile(part, b, 0o600); err != nil {
		return err
	}
	return os.Rename(part, strings.TrimSuffix(part, ".part"))
}

type spoolEntry struct {
	path, typ string
	size      int64
	mod       time.Time
	done      bool // handed over (not being written)
}

// enforceCap measures the spool and deletes the oldest handed-over files
// while it's above the cap, counting their events as dropped.
func (s *Spool) enforceCap() {
	var all []spoolEntry
	var total int64
	for _, t := range Types {
		entries, err := os.ReadDir(filepath.Join(s.Dir, t.Name))
		if err != nil {
			continue
		}
		for _, e := range entries {
			m := spoolNameRe.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue // shipped and deleted meanwhile
			}
			total += info.Size()
			all = append(all, spoolEntry{filepath.Join(s.Dir, t.Name, e.Name()), t.Name, info.Size(), info.ModTime(), m[3] == ""})
		}
	}
	limit := s.capBytes.Load()
	files := int64(len(all))
	if limit > 0 && total > limit {
		slices.SortFunc(all, func(a, b spoolEntry) int { return a.mod.Compare(b.mod) })
		for _, e := range all {
			if total <= limit {
				break
			}
			if !e.done {
				continue
			}
			lines := countLines(e.path)
			if err := os.Remove(e.path); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					s.Log.Warn("logship: dropping a spool file", "file", e.path, "err", err)
				}
				continue
			}
			total -= e.size
			files--
			if lines > 1 {
				s.counts[e.typ].dropped.Add(int64(lines - 1))
			}
			s.Log.Warn("logship: the spool is full (is the destination reachable?): dropped the oldest logs",
				"type", e.typ, "events", max(lines-1, 0))
		}
	}
	s.usageBytes.Store(max(total, 0))
	s.usageFiles.Store(files)
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	buf := make([]byte, 64<<10)
	for {
		k, err := f.Read(buf)
		n += bytes.Count(buf[:k], []byte{'\n'})
		if err == io.EOF || err != nil {
			return n
		}
	}
}
