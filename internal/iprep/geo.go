package iprep

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Countries maps addresses to countries with DB-IP's free "IP to Country
// Lite" database (CC BY 4.0: the panel credits it). It is only downloaded
// once a site uses country rules, then refreshed monthly with the
// database's own release cycle.
type Countries struct {
	Dir    string
	URL    string // format with the "2006-01" month; default DB-IP's
	Client *http.Client
	Log    *slog.Logger
	// Needed reports whether any site uses country rules.
	Needed func() bool

	db atomic.Pointer[geoDB]

	mu     sync.Mutex
	status GeoStatus
}

// GeoStatus is what the panel shows about the database.
type GeoStatus struct {
	Loaded      bool      `json:"loaded"`
	Month       string    `json:"month,omitempty"` // release, e.g. 2026-09
	Ranges      int       `json:"ranges"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	Error       string    `json:"error,omitempty"`
	Attribution string    `json:"attribution"`
}

const attribution = "IP geolocation by DB-IP (https://db-ip.com), CC BY 4.0"

type geoDB struct {
	month string
	v4    []geo4
	v6    []geo6
}

type geo4 struct {
	lo, hi uint32
	cc     [2]byte
}

type geo6 struct {
	lo, hi [16]byte
	cc     [2]byte
}

const defaultGeoURL = "https://download.db-ip.com/free/dbip-country-lite-%s.csv.gz"

func (c *Countries) file() string { return filepath.Join(c.Dir, "dbip-country-lite.csv.gz") }

func (c *Countries) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Country returns the ISO code for a ("" when the database has no country
// for it). ok is false while no database is loaded.
func (c *Countries) Country(a netip.Addr) (string, bool) {
	db := c.db.Load()
	if db == nil {
		return "", false
	}
	return db.lookup(a), true
}

func (db *geoDB) lookup(a netip.Addr) string {
	a = a.Unmap()
	var cc [2]byte
	if a.Is4() {
		x := u32(a)
		i, found := slices.BinarySearchFunc(db.v4, x, func(r geo4, x uint32) int { return cmp.Compare(r.lo, x) })
		switch {
		case found:
			cc = db.v4[i].cc
		case i > 0 && x <= db.v4[i-1].hi:
			cc = db.v4[i-1].cc
		}
	} else {
		x := a.As16()
		i, found := slices.BinarySearchFunc(db.v6, x, func(r geo6, x [16]byte) int { return bytes.Compare(r.lo[:], x[:]) })
		switch {
		case found:
			cc = db.v6[i].cc
		case i > 0 && bytes.Compare(x[:], db.v6[i-1].hi[:]) <= 0:
			cc = db.v6[i-1].cc
		}
	}
	if cc == [2]byte{} || cc == [2]byte{'Z', 'Z'} { // ZZ: reserved / unknown
		return ""
	}
	return string(cc[:])
}

// Status reports the database state.
func (c *Countries) Status() GeoStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	st.Attribution = attribution
	return st
}

// Load reads the database saved by the last download, if any.
func (c *Countries) Load() error {
	f, err := os.Open(c.file())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	month, _ := os.ReadFile(c.file() + ".month")
	db, err := parseGeoGzip(f)
	if err != nil {
		return err
	}
	db.month = strings.TrimSpace(string(month))
	info, _ := f.Stat()
	c.install(db, info.ModTime())
	return nil
}

func (c *Countries) install(db *geoDB, at time.Time) {
	c.db.Store(db)
	c.mu.Lock()
	c.status = GeoStatus{Loaded: true, Month: db.month, Ranges: len(db.v4) + len(db.v6), UpdatedAt: at.UTC()}
	c.mu.Unlock()
}

// Update downloads this month's release (or last month's, early in a month
// before the new one is out) unless it is already loaded.
func (c *Countries) Update(ctx context.Context, now time.Time) error {
	if db := c.db.Load(); db != nil && db.month == now.Format("2006-01") {
		return nil
	}
	var errs []error
	for _, m := range []time.Time{now, now.AddDate(0, -1, 0)} {
		month := m.Format("2006-01")
		if db := c.db.Load(); db != nil && db.month >= month {
			return nil // what we have is at least as new
		}
		err := c.download(ctx, month)
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", month, err))
	}
	err := errors.Join(errs...)
	c.mu.Lock()
	c.status.Error = err.Error()
	c.mu.Unlock()
	return err
}

func (c *Countries) download(ctx context.Context, month string) error {
	u := c.URL
	if u == "" {
		u = defaultGeoURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(u, month), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "WPGenie (+https://github.com/parthh37/wpgenie)")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	tmp := c.file() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, err = io.Copy(f, io.LimitReader(resp.Body, 256<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// Parse before replacing the saved copy: a broken download must not
	// replace a working database.
	rf, err := os.Open(tmp)
	if err != nil {
		return err
	}
	db, err := parseGeoGzip(rf)
	rf.Close()
	if err != nil {
		return err
	}
	if len(db.v4) < 50000 {
		return fmt.Errorf("only %d IPv4 ranges: not a complete database", len(db.v4))
	}
	db.month = month
	if err := os.Rename(tmp, c.file()); err != nil {
		return err
	}
	if err := os.WriteFile(c.file()+".month", []byte(month+"\n"), 0o600); err != nil {
		return err
	}
	c.install(db, time.Now())
	c.logger().Info("country database updated", "release", month, "ranges", len(db.v4)+len(db.v6))
	return nil
}

// Run keeps the database current while any site needs it. A site that
// turns country rules on is picked up within a minute.
func (c *Countries) Run(ctx context.Context) {
	var lastTry time.Time
	for {
		now := time.Now()
		needed := c.Needed == nil || c.Needed()
		stale := c.db.Load() == nil || c.db.Load().month != now.Format("2006-01")
		// Retry a failed or pending download every 6 hours; DB-IP publishes
		// early in the month.
		if needed && stale && now.Sub(lastTry) > 6*time.Hour {
			lastTry = now
			if err := c.Update(ctx, now); err != nil && ctx.Err() == nil {
				c.logger().Warn("country database: download failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// parseGeoGzip reads DB-IP's CSV ("first,last,CC" per line, IPv4 then
// IPv6), merging adjacent ranges of the same country.
func parseGeoGzip(r io.Reader) (*geoDB, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return parseGeoCSV(zr)
}

func parseGeoCSV(r io.Reader) (*geoDB, error) {
	db := &geoDB{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		first, rest, ok1 := strings.Cut(sc.Text(), ",")
		last, code, ok2 := strings.Cut(rest, ",")
		code = strings.Trim(strings.TrimSpace(code), `"`)
		lo, err1 := netip.ParseAddr(strings.Trim(first, `"`))
		hi, err2 := netip.ParseAddr(strings.Trim(last, `"`))
		if !ok1 || !ok2 || err1 != nil || err2 != nil || len(code) != 2 || lo.Is4() != hi.Is4() || hi.Less(lo) {
			return nil, fmt.Errorf("country database line %d: unexpected format", n)
		}
		cc := [2]byte{code[0], code[1]}
		if lo.Is4() {
			g := geo4{u32(lo), u32(hi), cc}
			if k := len(db.v4); k > 0 && db.v4[k-1].cc == cc && db.v4[k-1].hi+1 == g.lo {
				db.v4[k-1].hi = g.hi
			} else {
				db.v4 = append(db.v4, g)
			}
		} else {
			g := geo6{lo.As16(), hi.As16(), cc}
			if k := len(db.v6); k > 0 && db.v6[k-1].cc == cc && next6(db.v6[k-1].hi) == g.lo {
				db.v6[k-1].hi = g.hi
			} else {
				db.v6 = append(db.v6, g)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// The file is sorted; make sure, since lookups depend on it.
	if !slices.IsSortedFunc(db.v4, func(a, b geo4) int { return cmp.Compare(a.lo, b.lo) }) ||
		!slices.IsSortedFunc(db.v6, func(a, b geo6) int { return bytes.Compare(a.lo[:], b.lo[:]) }) {
		return nil, errors.New("country database is not sorted")
	}
	db.v4, db.v6 = slices.Clip(db.v4), slices.Clip(db.v6)
	return db, nil
}

func next6(a [16]byte) [16]byte {
	for i := 15; i >= 0; i-- {
		a[i]++
		if a[i] != 0 {
			break
		}
	}
	return a
}
