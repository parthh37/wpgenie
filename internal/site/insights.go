package site

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Performance insights: PHP response times, cache hits and slow URLs come
// from Caddy's access log (internal/analytics); PHP errors from each site's
// own error log, logs/php-error.log next to wp-config.php, where PHP-FPM and
// cron write them (images/php/pool.conf). The daemon reads it every
// insightsEvery, groups errors by level, message and place, and truncates
// it once read past phpLogMax.

const (
	phpLogDir     = "logs"
	phpLogPath    = phpLogDir + "/php-error.log"
	insightsEvery = 30 * time.Second
	phpLogMax     = 8 << 20
	// phpLogChunk bounds one pass (a flood of errors is read over several).
	phpLogChunk = 4 << 20
)

// ensureLogDir creates the site's log directory, writable by PHP and
// readable by nobody else (Caddy isn't in group 82).
func ensureLogDir(siteDir string) error {
	root, err := os.OpenRoot(siteDir)
	if err != nil {
		return err
	}
	defer root.Close()
	fi, err := root.Lstat(phpLogDir)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return errors.New(phpLogDir + " exists and is not a directory")
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := root.Mkdir(phpLogDir, 0o750); err != nil {
		return err
	}
	return chownToSite(root, phpLogDir)
}

// Insights is a site's performance over a period.
type Insights struct {
	Perf   *store.PerfStats    `json:"perf"`
	Slow   []store.SlowRequest `json:"slow_requests"`
	Errors []store.PHPError    `json:"php_errors"`
	Live   *CPUReading         `json:"live"`
}

func (s *Service) Insights(ctx context.Context, id string, since time.Time) (*Insights, error) {
	if _, err := s.Store.GetSite(ctx, id); err != nil {
		return nil, err
	}
	perf, err := s.Store.SitePerf(ctx, id, since)
	if err != nil {
		return nil, err
	}
	slow, err := s.Store.SlowRequests(ctx, id, since, 50)
	if err != nil {
		return nil, err
	}
	// The log is read every 30 s: read it now, so an error just seen in the
	// browser is already listed.
	s.ingestPHPErrors(ctx, id)
	errs, err := s.Store.PHPErrors(ctx, id, since, 100)
	if err != nil {
		return nil, err
	}
	out := &Insights{Perf: perf, Slow: slow, Errors: errs}
	if r, ok := s.CPU(id); ok {
		out.Live = &r
	}
	return out, nil
}

// RunInsights reads every active site's PHP error log until ctx ends, and
// prunes old performance data hourly.
func (s *Service) RunInsights(ctx context.Context) {
	t := time.NewTicker(insightsEvery)
	defer t.Stop()
	var pruned time.Time
	for {
		if sites, err := s.Store.ListSites(ctx); err == nil {
			for _, st := range sites {
				if st.Status == store.StatusActive {
					s.ingestPHPErrors(ctx, st.ID)
				}
			}
		}
		if time.Since(pruned) > time.Hour {
			pruned = time.Now()
			if err := s.Store.PruneInsights(ctx, pruned); err != nil {
				s.Log.Warn("insights: pruning", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ingestPHPErrors reads what PHP logged since the last pass. One pass per
// site at a time (the loop and an Insights request may overlap).
func (s *Service) ingestPHPErrors(ctx context.Context, id string) {
	m, _ := s.phpLogLocks.LoadOrStore(id, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()
	if err := s.readPHPLog(ctx, id); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.Log.Warn("insights: reading the PHP error log", "site", id, "err", err)
	}
}

func (s *Service) readPHPLog(ctx context.Context, id string) error {
	dir := s.Cfg.SiteDir(id)
	if err := ensureLogDir(dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	// PHP (the site) owns the file and can swap it at any moment: a symlink
	// must not make the daemon read or truncate anything else, and a named
	// pipe must not block it (opening a FIFO for reading waits for a
	// writer). So: one open, never blocking, of what Lstat saw as a regular
	// file, checked again on the handle; then only that handle is used.
	lfi, err := root.Lstat(phpLogPath)
	if err != nil {
		return err
	}
	if !lfi.Mode().IsRegular() {
		return errors.New(phpLogPath + " is not a regular file")
	}
	state, err := s.Store.IngestState(ctx, "php_errors:"+id)
	if err != nil {
		return err
	}
	f, err := root.OpenFile(phpLogPath, os.O_RDWR|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(lfi, fi) {
		return errors.New(phpLogPath + " changed while being opened")
	}
	inode := uint64(0)
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		inode = uint64(st.Ino)
	}
	if inode != state.Inode || fi.Size() < state.Offset {
		state.Inode, state.Offset = inode, 0
	}
	if fi.Size() == state.Offset {
		return nil
	}
	if _, err := f.Seek(state.Offset, io.SeekStart); err != nil {
		return err
	}
	buf, err := io.ReadAll(io.LimitReader(f, phpLogChunk))
	if err != nil {
		return err
	}
	// Whole lines only: PHP may be writing the next one.
	end := bytes.LastIndexByte(buf, '\n') + 1
	if end == 0 && len(buf) < phpLogChunk {
		return nil
	}
	if end == 0 {
		end = len(buf) // one absurdly long line: take it as it is
	}
	state.Offset += int64(end)
	errs := parsePHPLog(buf[:end], s.Cfg.SiteRoot(id), dir, time.Now())
	if err := s.Store.RecordPHPErrors(ctx, id, errs, state); err != nil {
		return err
	}
	if s.PHPLogTee != nil {
		s.PHPLogTee(id, buf[:end])
	}
	if state.Offset >= phpLogMax && state.Offset >= fi.Size() {
		// Everything read: start the file over. PHP appends (O_APPEND), so
		// its next write lands at the new end. What it wrote since Stat is
		// lost: at most a few lines of a file this large.
		if err := f.Truncate(0); err != nil {
			return err
		}
	}
	return nil
}

var (
	// [29-Sep-2026 10:11:12 UTC] message
	phpLogHead = regexp.MustCompile(`^\[(\d{2}-[A-Za-z]{3}-\d{4} \d{2}:\d{2}:\d{2})(?: ([A-Za-z_/+-]+))?\] (.*)$`)
	phpLevel   = regexp.MustCompile(`^PHP ([A-Za-z ]+?):\s+(.*)$`)
	// "… in /path/file.php on line 12" or "… in /path/file.php:12"
	phpWhere = regexp.MustCompile(`^(.*?) in (/\S+?)(?: on line |:)(\d+)\s*$`)
	phpFrame = regexp.MustCompile(`^#\d+ (/\S+?)\(\d+\)`)
)

// parsePHPLog groups the entries of a PHP error log. An entry is a line
// starting with a timestamp plus the lines up to the next one (stack
// traces). Paths are shown relative to the docroot (or the site directory).
func parsePHPLog(data []byte, docroot, siteDir string, now time.Time) []store.PHPError {
	byFP := map[string]*store.PHPError{}
	var order []string
	var cur *store.PHPError
	var trace []string
	flush := func() {
		if cur == nil {
			return
		}
		if src := cur.Source; (src == "core" || src == "other") && len(trace) > 0 {
			// An error inside WordPress (or PHP's own) caused by a plugin:
			// the first plugin or theme in the stack trace is whose it is.
			for _, p := range trace {
				if s := codeSource(relPath(p, docroot, siteDir)); strings.HasPrefix(s, "plugin:") || strings.HasPrefix(s, "theme:") {
					cur.Source = s
					break
				}
			}
		}
		sum := sha256.Sum256([]byte(cur.Level + "\x00" + cur.Message + "\x00" + cur.File + "\x00" + strconv.Itoa(cur.Line)))
		cur.Fingerprint = hex.EncodeToString(sum[:8])
		if e := byFP[cur.Fingerprint]; e != nil {
			e.Count++
			e.FirstSeen = minTime(e.FirstSeen, cur.FirstSeen)
			e.LastSeen = maxTime(e.LastSeen, cur.LastSeen)
		} else {
			byFP[cur.Fingerprint] = cur
			order = append(order, cur.Fingerprint)
		}
		cur, trace = nil, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), phpLogChunk)
	for sc.Scan() {
		line := sc.Text()
		m := phpLogHead.FindStringSubmatch(line)
		if m == nil {
			if cur != nil {
				if f := phpFrame.FindStringSubmatch(strings.TrimSpace(line)); f != nil {
					trace = append(trace, f[1])
				}
			}
			continue
		}
		flush()
		at := now
		if t, err := time.Parse("02-Jan-2006 15:04:05", m[1]); err == nil {
			if loc, err := time.LoadLocation(m[2]); err == nil && m[2] != "" {
				t, _ = time.ParseInLocation("02-Jan-2006 15:04:05", m[1], loc)
			}
			at = t.UTC()
		}
		e := &store.PHPError{Level: "log", Message: m[3], Count: 1, FirstSeen: at, LastSeen: at, Source: "other"}
		if l := phpLevel.FindStringSubmatch(m[3]); l != nil {
			e.Level, e.Message = strings.ToLower(l[1]), l[2]
		}
		if w := phpWhere.FindStringSubmatch(e.Message); w != nil {
			e.Message, e.File = w[1], relPath(w[2], docroot, siteDir)
			e.Line, _ = strconv.Atoi(w[3])
			e.Source = codeSource(e.File)
		}
		e.Message = strings.ToValidUTF8(strings.TrimSpace(e.Message), "?")
		if len(e.Message) > 1000 {
			e.Message = e.Message[:1000] + "…"
		}
		cur = e
	}
	flush()
	out := make([]store.PHPError, 0, len(order))
	for _, fp := range order {
		out = append(out, *byFP[fp])
	}
	return out
}

func relPath(p, docroot, siteDir string) string {
	for _, base := range []string{docroot, siteDir, "/usr/src/wordpress"} {
		if rest, ok := strings.CutPrefix(p, strings.TrimSuffix(base, "/")+"/"); ok {
			return rest
		}
	}
	return p
}

// codeSource says whose code a file (relative to the docroot) is.
func codeSource(rel string) string {
	for _, kind := range []string{"plugins", "themes"} {
		if rest, ok := strings.CutPrefix(rel, "wp-content/"+kind+"/"); ok {
			slug, _, _ := strings.Cut(rest, "/")
			return strings.TrimSuffix(kind, "s") + ":" + strings.TrimSuffix(slug, ".php")
		}
	}
	switch {
	case strings.HasPrefix(rel, "wp-content/mu-plugins/"):
		return "mu-plugin"
	case strings.HasPrefix(rel, "wp-includes/"), strings.HasPrefix(rel, "wp-admin/"),
		!strings.Contains(rel, "/") && (strings.HasPrefix(rel, "wp-") || rel == "index.php" || rel == "xmlrpc.php"):
		return "core"
	}
	return "other"
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
