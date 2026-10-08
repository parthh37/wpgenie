package site

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Debug mode: WP_DEBUG on for a while, its messages logged where PHP's
// errors already go (logs/php-error.log next to wp-config.php, outside the
// docroot: see insights.go, which keeps grouping them) and never shown to
// visitors. WordPress reads WP_DEBUG before any plugin loads, so the
// switch is a block in WPGenie's part of wp-config.php (root-owned, not
// writable by PHP). It ends itself: the block only applies while time() is
// before its end, and the maintenance loop takes it out afterwards.

const (
	// DebugFor is how long debug mode stays on.
	DebugFor = 24 * time.Hour
	// debugTailLines and debugTailBytes bound what the panel shows of the log.
	debugTailLines = 200
	debugTailBytes = 128 << 10

	debugBlockStart = "// --- WPGenie debug mode"
	debugBlockEnd   = "// --- end WPGenie debug mode ---\n"
	wpgenieEnd      = "// --- end WPGenie ---\n"
)

var (
	debugBlockRe = regexp.MustCompile(`(?s)// --- WPGenie debug mode[^\n]*\n.*?// --- end WPGenie debug mode ---\n`)
	debugUntilRe = regexp.MustCompile(`if \( time\(\) < (\d+) \) \{`)
)

// debugBlock is what goes into wp-config.php while debug mode is on.
func debugBlock(until time.Time) string {
	return fmt.Sprintf(`%s: on until %s (WPGenie turns it off) ---
if ( time() < %d ) {
	define( 'WP_DEBUG', true );
	define( 'WP_DEBUG_LOG', __DIR__ . '/%s' );
	define( 'WP_DEBUG_DISPLAY', false );
	@ini_set( 'display_errors', '0' );
}
%s`, debugBlockStart, until.UTC().Format("2006-01-02 15:04 UTC"), until.Unix(), phpLogPath, debugBlockEnd)
}

// withDebugBlock returns wp-config.php with debug mode on until until, or
// off (a zero until). The block goes at the end of WPGenie's section, before
// anything the site's owner added below it.
func withDebugBlock(cfg []byte, until time.Time) ([]byte, error) {
	out := debugBlockRe.ReplaceAll(cfg, nil)
	if until.IsZero() {
		return out, nil
	}
	i := bytes.Index(out, []byte(wpgenieEnd))
	if i < 0 {
		return nil, fmt.Errorf("%w: wp-config.php has no WPGenie section (%q)", ErrConflict, strings.TrimSpace(wpgenieEnd))
	}
	return slices.Concat(out[:i], []byte(debugBlock(until)), out[i:]), nil
}

// debugUntil is when the debug block in wp-config.php ends (zero: none).
func debugUntil(cfg []byte) time.Time {
	block := debugBlockRe.Find(cfg)
	if block == nil {
		return time.Time{}
	}
	m := debugUntilRe.FindSubmatch(block)
	if m == nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// Debug is a site's debug mode.
type Debug struct {
	On    bool      `json:"on"`
	Until time.Time `json:"until,omitzero"`
}

// DebugInput turns debug mode on (for DebugFor) or off.
type DebugInput struct {
	On bool `json:"on"`
}

// DebugLog is the end of the site's PHP error log.
type DebugLog struct {
	Lines []string `json:"lines"`
	// Size is the whole file's; Truncated: there is more before Lines.
	Size      int64 `json:"size"`
	Truncated bool  `json:"truncated"`
}

func (s *Service) wpConfigPath(id string) string {
	return filepath.Join(s.Cfg.SiteDir(id), "wp-config.php")
}

// wpConfigLock serialises WPGenie's edits of a site's wp-config.php.
func (s *Service) wpConfigLock(id string) *sync.Mutex {
	m, _ := s.wpConfigLocks.LoadOrStore(id, new(sync.Mutex))
	return m.(*sync.Mutex)
}

// DebugMode reads a site's debug mode from its wp-config.php.
func (s *Service) DebugMode(ctx context.Context, id string) (*Debug, error) {
	if _, err := s.Store.GetSite(ctx, id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.wpConfigPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return &Debug{}, nil
	} else if err != nil {
		return nil, err
	}
	return debugState(b, time.Now()), nil
}

func debugState(cfg []byte, now time.Time) *Debug {
	until := debugUntil(cfg)
	if until.IsZero() || !now.Before(until) {
		return &Debug{}
	}
	return &Debug{On: true, Until: until}
}

// SetDebugMode turns debug mode on for DebugFor (again from now, if it
// was on) or off.
func (s *Service) SetDebugMode(ctx context.Context, id string, in DebugInput) (*Debug, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := activeSite(st); err != nil {
		return nil, err
	}
	var until time.Time
	if in.On {
		until = time.Now().Add(DebugFor).Truncate(time.Minute)
	}
	was, err := s.setDebugBlock(id, until)
	if err != nil {
		return nil, err
	}
	switch {
	case in.On:
		s.event(id, "debug", "Debug mode on until "+until.Format("2006-01-02 15:04 UTC")+
			": WordPress logs notices and warnings to the PHP error log (never shown to visitors)")
	case was.On:
		s.event(id, "debug", "Debug mode off")
	}
	if !in.On {
		return &Debug{}, nil
	}
	return &Debug{On: true, Until: until}, nil
}

// setDebugBlock writes wp-config.php with the debug block for until (zero:
// none), returning the state before.
func (s *Service) setDebugBlock(id string, until time.Time) (*Debug, error) {
	mu := s.wpConfigLock(id)
	mu.Lock()
	defer mu.Unlock()
	path := s.wpConfigPath(id)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	was := debugState(b, time.Now())
	out, err := withDebugBlock(b, until)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(out, b) {
		return was, nil
	}
	return was, replaceWPConfig(path, out)
}

// replaceWPConfig writes a site's wp-config.php atomically, keeping it
// root-owned and readable (not writable) by PHP.
func replaceWPConfig(path string, data []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, fi.Mode().Perm()); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(tmp, 0, wwwData); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, path)
}

// expireDebug takes out a debug block whose time is up (PHP already
// ignores it) and says so in the site's activity log.
func (s *Service) expireDebug(id string, now time.Time) {
	b, err := os.ReadFile(s.wpConfigPath(id))
	if err != nil {
		return
	}
	until := debugUntil(b)
	if until.IsZero() || now.Before(until) {
		return
	}
	mu := s.wpConfigLock(id)
	if !mu.TryLock() {
		return // being changed right now; next time
	}
	defer mu.Unlock()
	if b, err = os.ReadFile(s.wpConfigPath(id)); err != nil {
		return
	}
	if until = debugUntil(b); until.IsZero() || now.Before(until) {
		return
	}
	out, err := withDebugBlock(b, time.Time{})
	if err == nil {
		err = replaceWPConfig(s.wpConfigPath(id), out)
	}
	if err != nil {
		s.Log.Warn("turning debug mode off", "site", id, "err", err)
		return
	}
	s.event(id, "debug", "Debug mode turned itself off after 24 hours")
}

// DebugLogTail returns the last lines of the site's PHP error log, where
// debug mode writes (bounded: at most debugTailBytes are read, from the end).
func (s *Service) DebugLogTail(ctx context.Context, id string) (*DebugLog, error) {
	if _, err := s.Store.GetSite(ctx, id); err != nil {
		return nil, err
	}
	empty := &DebugLog{Lines: []string{}}
	root, err := os.OpenRoot(s.Cfg.SiteDir(id))
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	} else if err != nil {
		return nil, err
	}
	defer root.Close()
	f, fi, err := openRegular(root, phpLogPath)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	start := max(fi.Size()-debugTailBytes, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, debugTailBytes))
	if err != nil {
		return nil, err
	}
	lines, cut := tailLines(buf, start > 0, debugTailLines)
	return &DebugLog{Lines: lines, Size: fi.Size(), Truncated: cut}, nil
}

// tailLines returns the last n lines of buf (a partial first line dropped
// when buf starts mid-file), and whether anything came before them.
func tailLines(buf []byte, midFile bool, n int) ([]string, bool) {
	cut := midFile
	if midFile {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}
	text := strings.TrimRight(strings.ToValidUTF8(string(buf), "?"), "\n")
	if text == "" {
		return []string{}, cut
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines, cut = lines[len(lines)-n:], true
	}
	for i, l := range lines {
		l = strings.TrimSuffix(l, "\r")
		if len(l) > 2000 {
			l = l[:2000] + "…"
		}
		lines[i] = l
	}
	return lines, cut
}

// ClearDebugLog empties the site's PHP error log. What it holds is read
// into the panel's error insights first, so nothing is lost from there.
func (s *Service) ClearDebugLog(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if st.Status == store.StatusActive {
		s.ingestPHPErrors(ctx, id)
	}
	root, err := os.OpenRoot(s.Cfg.SiteDir(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer root.Close()
	f, _, err := openRegularRW(root, phpLogPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(0); err != nil {
		return err
	}
	s.event(id, "debug", "PHP error log cleared")
	return nil
}

// openRegularRW is openRegular for writing.
func openRegularRW(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	lfi, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := root.OpenFile(name, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(lfi, fi) {
		f.Close()
		return nil, nil, fmt.Errorf("%s changed while being opened", name)
	}
	return f, fi, nil
}
