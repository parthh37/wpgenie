package logship

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Containers' own output: Docker's json-file logs of WPGenie's containers
// (wpg-*, wpgenie-*), read by the daemon and spooled. Vector could tail
// them itself, but only with Docker's containers directory mounted, and
// that directory also holds every container's configuration (its
// environment: database passwords, keys). Offsets are kept in memory: a
// container is read from its end the first time the daemon sees it after
// starting (like the shipper's read_from: end), from its start when it
// appears later (a new container).

const (
	// containerRead bounds what one pass reads of one container's log.
	containerRead = 4 << 20
	// containerList: how often the containers are listed again.
	containerList = time.Minute
)

var containerNameRe = regexp.MustCompile(`^(wpg-|wpgenie-)[A-Za-z0-9_.-]{1,120}$`)

// containerLog is where the daemon is in a container's log.
type containerLog struct {
	name  string
	inode uint64
	off   int64
}

// containerTail is the tailer's state (the Run loop's only).
type containerTail struct {
	logs     map[string]*containerLog // container ID ->
	names    map[string]string        // container ID -> name
	listedAt time.Time
	started  bool // the first pass is over: later containers are new
}

// dockerLine is a line of Docker's json-file log.
type dockerLine struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
	Time   string `json:"time"`
}

// containerEntry is a line of a container's output as shipped.
type containerEntry struct {
	Time      string `json:"time"`
	Container string `json:"container"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

// tailContainers reads what WPGenie's containers printed since the last
// pass into the spool.
func (s *Service) tailContainers(ctx context.Context) error {
	if !s.spool.Accepts(TypeContainers) || !s.Available(TypeContainers) {
		s.ctail = containerTail{}
		return nil
	}
	s.mu.Lock()
	root := s.st.dockerRoot
	s.mu.Unlock()
	t := &s.ctail
	if t.logs == nil {
		t.logs = map[string]*containerLog{}
	}
	if t.names == nil || s.now().Sub(t.listedAt) >= containerList {
		out, err := s.Docker.Run(ctx, nil, "ps", "-a", "--no-trunc", "--format", "{{.ID}} {{.Names}}")
		if err != nil {
			return fmt.Errorf("listing containers: %w", err)
		}
		t.names, t.listedAt = map[string]string{}, s.now()
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			id, name, ok := strings.Cut(strings.TrimSpace(line), " ")
			if ok && len(id) == 64 && strings.Trim(id, "0123456789abcdef") == "" && containerNameRe.MatchString(name) {
				t.names[id] = name
			}
		}
		for id := range t.logs {
			if _, ok := t.names[id]; !ok {
				delete(t.logs, id) // removed
			}
		}
	}
	var errs []error
	for id, name := range t.names {
		path := filepath.Join(root, "containers", id, id+"-json.log")
		lines, err := t.read(id, name, path, !t.started)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		if len(lines) > 0 {
			if err := s.spool.WriteSync(TypeContainers, lines); err != nil {
				return err
			}
		}
	}
	t.started = true
	return errors.Join(errs...)
}

// read returns the container's new whole lines as entries. A log Docker
// rotated since (renamed to .1) is read to its end first.
func (t *containerTail) read(id, name, path string, fromEnd bool) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	ino := inodeOf(fi)
	st := t.logs[id]
	if st == nil {
		st = &containerLog{name: name, inode: ino}
		if fromEnd {
			st.off = fi.Size()
		}
		t.logs[id] = st
	}
	var out [][]byte
	if st.inode != ino {
		// Rotated: the rest of the old file, if it's still there.
		if old, err := os.Open(path + ".1"); err == nil {
			if ofi, err := old.Stat(); err == nil && inodeOf(ofi) == st.inode {
				lines, _ := readLines(old, st.off, name)
				out = append(out, lines...)
			}
			old.Close()
		}
		st.inode, st.off = ino, 0
	}
	if fi.Size() < st.off {
		st.off = 0 // truncated
	}
	lines, n := readLines(f, st.off, name)
	st.off += n
	return append(out, lines...), nil
}

// readLines reads whole lines from off (at most containerRead bytes) and
// returns them as entries, and how far it read.
func readLines(f *os.File, off int64, name string) ([][]byte, int64) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, 0
	}
	r := bufio.NewReaderSize(io.LimitReader(f, containerRead), 64<<10)
	var out [][]byte
	var n int64
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return out, n // a partial last line is read again next time
		}
		n += int64(len(line))
		var d dockerLine
		if json.Unmarshal(bytes.TrimSpace(line), &d) != nil {
			continue
		}
		b, _ := json.Marshal(containerEntry{Time: d.Time, Container: name, Stream: d.Stream,
			Message: strings.TrimRight(d.Log, "\r\n")})
		out = append(out, b)
	}
}

func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
