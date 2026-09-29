package iprep

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Feed is one public blocklist.
type Feed struct {
	Name  string // file name and the reason shown in the security log
	Title string
	URL   string
	Parse func(io.Reader) ([]netip.Prefix, error)
	// MinEntries rejects a download that is implausibly short (an error
	// page, a truncated file): keeping yesterday's list is better than
	// silently protecting nobody.
	MinEntries int
}

// DefaultFeeds are free lists that need no account. Both are aimed at
// exactly the clients a WordPress host wants to keep out: networks run by
// criminals, and addresses seen attacking servers in the last two days.
var DefaultFeeds = []Feed{
	{
		Name: "spamhaus-drop", Title: "Spamhaus DROP (hijacked and criminal networks)",
		URL:   "https://www.spamhaus.org/drop/drop_v4.json",
		Parse: parseSpamhausJSON, MinEntries: 200,
	},
	{
		Name: "spamhaus-drop-v6", Title: "Spamhaus DROPv6",
		URL:   "https://www.spamhaus.org/drop/drop_v6.json",
		Parse: parseSpamhausJSON, MinEntries: 10,
	},
	{
		// Reported by thousands of fail2ban installations: SSH, mail, web
		// brute force and scanning in the last 48 hours.
		Name: "blocklist-de", Title: "blocklist.de (attacks reported in the last 48 hours)",
		URL:   "https://lists.blocklist.de/lists/all.txt",
		Parse: parsePlain, MinEntries: 1000,
	},
}

// FeedStatus is what the panel shows about a list.
type FeedStatus struct {
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Entries   int       `json:"entries"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// Lists holds the blocklists: loaded from Dir at start (so a restart
// without network keeps protecting), refreshed from the feeds periodically.
type Lists struct {
	Dir    string
	Feeds  []Feed
	Client *http.Client
	Log    *slog.Logger

	sets atomic.Pointer[[]namedSet] // hot path: lock-free

	mu     sync.Mutex
	status map[string]*FeedStatus
}

type namedSet struct {
	name string
	set  *Set
}

func (l *Lists) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	return &http.Client{Timeout: time.Minute}
}

func (l *Lists) logger() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}

// Listed returns the first list that has a.
func (l *Lists) Listed(a netip.Addr) (string, bool) {
	sets := l.sets.Load()
	if sets == nil {
		return "", false
	}
	for _, s := range *sets {
		if s.set.Contains(a) {
			return s.name, true
		}
	}
	return "", false
}

// Status reports every feed, in order.
func (l *Lists) Status() []FeedStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]FeedStatus, 0, len(l.Feeds))
	for _, f := range l.Feeds {
		st := FeedStatus{Name: f.Name, Title: f.Title}
		if s := l.status[f.Name]; s != nil {
			st = *s
		}
		out = append(out, st)
	}
	return out
}

func (l *Lists) path(f Feed) string { return filepath.Join(l.Dir, f.Name+".txt") }

// Load reads the lists saved by the last refresh.
func (l *Lists) Load() {
	for _, f := range l.Feeds {
		file, err := os.Open(l.path(f))
		if err != nil {
			continue
		}
		prefixes, err := parsePlain(file)
		file.Close()
		info, _ := os.Stat(l.path(f))
		if err != nil || len(prefixes) == 0 {
			continue
		}
		l.install(f, prefixes, info.ModTime(), "")
	}
}

// Refresh downloads every feed. A feed that fails keeps its previous list.
func (l *Lists) Refresh(ctx context.Context) error {
	var errs []error
	for _, f := range l.Feeds {
		prefixes, err := l.fetch(ctx, f)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Name, err))
			l.setError(f, err)
			continue
		}
		if err := l.save(f, prefixes); err != nil {
			l.logger().Warn("ip reputation: saving list", "list", f.Name, "err", err)
		}
		l.install(f, prefixes, time.Now(), "")
	}
	return errors.Join(errs...)
}

// Run refreshes the lists every interval (blocklist.de changes by the hour;
// Spamhaus asks for no more than one download per hour). After a failure
// it retries within 15 minutes: a server that booted without network
// shouldn't wait hours for its first lists.
func (l *Lists) Run(ctx context.Context, every time.Duration) {
	for {
		wait := every
		if err := l.Refresh(ctx); err != nil && ctx.Err() == nil {
			l.logger().Warn("ip reputation: refreshing blocklists", "err", err)
			wait = min(every, 15*time.Minute)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (l *Lists) fetch(ctx context.Context, f Feed) ([]netip.Prefix, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "WPGenie (+https://github.com/parthh37/wpgenie)")
	resp, err := l.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	prefixes, err := f.Parse(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if len(prefixes) < f.MinEntries {
		return nil, fmt.Errorf("only %d entries (expected at least %d): keeping the previous list", len(prefixes), f.MinEntries)
	}
	return prefixes, nil
}

func (l *Lists) save(f Feed, prefixes []netip.Prefix) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, p := range prefixes {
		b.WriteString(p.String())
		b.WriteByte('\n')
	}
	tmp := l.path(f) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path(f))
}

func (l *Lists) install(f Feed, prefixes []netip.Prefix, at time.Time, errMsg string) {
	set := NewSet(prefixes)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status == nil {
		l.status = map[string]*FeedStatus{}
	}
	l.status[f.Name] = &FeedStatus{Name: f.Name, Title: f.Title, Entries: len(prefixes), UpdatedAt: at.UTC(), Error: errMsg}
	var sets []namedSet
	if old := l.sets.Load(); old != nil {
		sets = append(sets, *old...)
	}
	replaced := false
	for i := range sets {
		if sets[i].name == f.Name {
			sets[i].set, replaced = set, true
		}
	}
	if !replaced {
		sets = append(sets, namedSet{f.Name, set})
	}
	l.sets.Store(&sets)
}

func (l *Lists) setError(f Feed, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status == nil {
		l.status = map[string]*FeedStatus{}
	}
	st := l.status[f.Name]
	if st == nil {
		st = &FeedStatus{Name: f.Name, Title: f.Title}
		l.status[f.Name] = st
	}
	st.Error = err.Error()
}

// special are networks no public list may cover: blocking them would cut
// off loopback, the Docker network, a LAN or a tunnel. They are dropped
// from every list, whatever it says.
var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/3"), // multicast and reserved
	netip.MustParsePrefix("::/8"),        // loopback, unspecified, mapped
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// plausible accepts only prefixes a blocklist can mean: not special, and
// not so wide that one bad line would cut off a large part of the internet
// (the widest real DROP entries are /10 on IPv4).
func plausible(p netip.Prefix) bool {
	if !p.IsValid() {
		return false
	}
	if p.Addr().Is4() && p.Bits() < 8 || p.Addr().Is6() && p.Bits() < 19 {
		return false
	}
	for _, s := range special {
		if s.Overlaps(p) {
			return false
		}
	}
	return true
}

// parsePrefix reads an address or a CIDR prefix.
func parsePrefix(v string) (netip.Prefix, bool) {
	v = strings.TrimSpace(v)
	if a, err := netip.ParseAddr(v); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	p, err := netip.ParsePrefix(v)
	if err != nil {
		return netip.Prefix{}, false
	}
	return p.Masked(), true
}

// parsePlain reads one address or prefix per line; "#" and ";" start
// comments. Lines that aren't addresses are skipped.
func parsePlain(r io.Reader) ([]netip.Prefix, error) {
	var out []netip.Prefix
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		if p, ok := parsePrefix(line); ok && plausible(p) {
			out = append(out, p)
		}
	}
	return out, sc.Err()
}

// parseSpamhausJSON reads Spamhaus's JSON-lines DROP format:
// {"cidr":"1.10.16.0/20","sblid":"SBL256894","rir":"apnic"}, ending with a
// {"type":"metadata",...} line.
func parseSpamhausJSON(r io.Reader) ([]netip.Prefix, error) {
	var out []netip.Prefix
	dec := json.NewDecoder(r)
	for {
		var e struct {
			CIDR string `json:"cidr"`
		}
		if err := dec.Decode(&e); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		if p, ok := parsePrefix(e.CIDR); ok && plausible(p) {
			out = append(out, p)
		}
	}
}
