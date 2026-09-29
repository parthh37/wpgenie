package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Vuln is a known vulnerability affecting an installed version.
type Vuln struct {
	Title    string `json:"title"`
	Severity string `json:"severity,omitempty"` // critical, high, medium, low (CVSS 3)
	Score    string `json:"score,omitempty"`
	// FixedIn is the first version without the flaw, "" if unknown.
	FixedIn string `json:"fixed_in,omitempty"`
	Unfixed bool   `json:"unfixed,omitempty"` // no fixed version exists yet
	CVE     string `json:"cve,omitempty"`
	Link    string `json:"link,omitempty"`
}

// VulnDB looks up known vulnerabilities of WordPress components.
type VulnDB interface {
	// Lookup returns the vulnerabilities affecting version of a plugin,
	// theme or core ("plugin", "theme", "core"; slug is ignored for core).
	Lookup(ctx context.Context, kind, slug, version string) ([]Vuln, error)
}

// WPVulnerability queries the free WPVulnerability.net database (no API
// key, CVE-sourced). Answers are cached: the nightly scan of many sites
// asks about the same popular plugins over and over.
type WPVulnerability struct {
	BaseURL   string // default https://www.wpvulnerability.net
	UserAgent string
	Client    *http.Client
	TTL       time.Duration // cache lifetime, default 6h

	mu    sync.Mutex
	cache map[string]cachedVulns
}

type cachedVulns struct {
	at      time.Time
	entries []vulnEntry
}

// vulnEntry mirrors one element of data.vulnerability in the API response.
// impact is an object, or [] when there is no score, hence RawMessage.
type vulnEntry struct {
	Name     string `json:"name"`
	Operator *struct {
		MinVersion  *string `json:"min_version"`
		MinOperator *string `json:"min_operator"`
		MaxVersion  *string `json:"max_version"`
		MaxOperator *string `json:"max_operator"`
		Unfixed     string  `json:"unfixed"`
	} `json:"operator"`
	Source []struct {
		ID   string `json:"id"`
		Link string `json:"link"`
	} `json:"source"`
	Impact json.RawMessage `json:"impact"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

func (w *WPVulnerability) Lookup(ctx context.Context, kind, slug, version string) ([]Vuln, error) {
	var path string
	switch kind {
	case "plugin", "theme":
		if !slugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid %s slug %q", kind, slug)
		}
		path = "/" + kind + "/" + url.PathEscape(slug) + "/"
	case "core":
		// Core answers are already filtered to the version asked about.
		path = "/core/" + url.PathEscape(version) + "/"
	default:
		return nil, fmt.Errorf("unknown component kind %q", kind)
	}
	entries, err := w.fetch(ctx, path)
	if err != nil {
		return nil, err
	}
	var out []Vuln
	for _, e := range entries {
		if kind != "core" && !e.affects(version) {
			continue
		}
		out = append(out, e.vuln())
	}
	return out, nil
}

func (w *WPVulnerability) fetch(ctx context.Context, path string) ([]vulnEntry, error) {
	ttl := w.TTL
	if ttl == 0 {
		ttl = 6 * time.Hour
	}
	w.mu.Lock()
	if c, ok := w.cache[path]; ok && time.Since(c.at) < ttl {
		w.mu.Unlock()
		return c.entries, nil
	}
	w.mu.Unlock()

	base := w.BaseURL
	if base == "" {
		base = "https://www.wpvulnerability.net"
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", w.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vulnerability database: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vulnerability database: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Error   int     `json:"error"`
		Message *string `json:"message"`
		Data    *struct {
			Vulnerability []vulnEntry `json:"vulnerability"` // null for unknown slugs
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("vulnerability database: %w", err)
	}
	if body.Error != 0 {
		msg := "error"
		if body.Message != nil {
			msg = *body.Message
		}
		return nil, errors.New("vulnerability database: " + msg)
	}
	var entries []vulnEntry
	if body.Data != nil {
		entries = body.Data.Vulnerability
	}
	w.mu.Lock()
	if w.cache == nil {
		w.cache = map[string]cachedVulns{}
	}
	w.cache[path] = cachedVulns{at: time.Now(), entries: entries}
	w.mu.Unlock()
	return entries, nil
}

// affects applies the entry's version range with PHP version_compare
// semantics, as the database defines them. A missing bound is open.
func (e vulnEntry) affects(version string) bool {
	op := e.Operator
	if op == nil {
		return false
	}
	if op.MinVersion != nil && op.MinOperator != nil && !compareOp(version, *op.MinVersion, *op.MinOperator) {
		return false
	}
	if op.MaxVersion != nil && op.MaxOperator != nil && !compareOp(version, *op.MaxVersion, *op.MaxOperator) {
		return false
	}
	return true
}

func (e vulnEntry) vuln() Vuln {
	v := Vuln{Title: html.UnescapeString(e.Name)}
	if op := e.Operator; op != nil {
		v.Unfixed = op.Unfixed == "1"
		// "< 5.3.2" means 5.3.2 is the fix; "<= 5.3.2" only that some later
		// version is.
		if op.MaxVersion != nil && op.MaxOperator != nil && *op.MaxOperator == "lt" && !v.Unfixed {
			v.FixedIn = *op.MaxVersion
		}
	}
	for _, s := range e.Source {
		if strings.HasPrefix(s.ID, "CVE-") {
			v.CVE, v.Link = s.ID, s.Link
			break
		}
		if v.Link == "" {
			v.Link = s.Link
		}
	}
	var impact struct {
		CVSS3 *struct {
			Score    string `json:"score"`
			Severity string `json:"severity"`
		} `json:"cvss3"`
	}
	if json.Unmarshal(e.Impact, &impact) == nil && impact.CVSS3 != nil {
		v.Severity, v.Score = impact.CVSS3.Severity, impact.CVSS3.Score
	}
	return v
}

func compareOp(a, b, op string) bool {
	c := versionCompare(a, b)
	switch op {
	case "lt":
		return c < 0
	case "le":
		return c <= 0
	case "gt":
		return c > 0
	case "ge":
		return c >= 0
	case "eq":
		return c == 0
	case "ne":
		return c != 0
	}
	return false
}

// versionCompare implements PHP's version_compare(): "_", "-" and "+"
// separate parts like ".", letters and digits are split apart, numbers
// compare numerically, and pre-release words rank
// dev < alpha = a < beta = b < RC = rc < (number) < pl = p.
func versionCompare(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range max(len(pa), len(pb)) {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if c := comparePart(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func versionParts(v string) []string {
	var parts []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, cur.String())
			cur.Reset()
		}
	}
	var prevDigit, started bool
	for _, r := range strings.TrimSpace(v) {
		if strings.ContainsRune("._-+", r) {
			flush()
			started = false
			continue
		}
		d := unicode.IsDigit(r)
		if started && d != prevDigit {
			flush()
		}
		cur.WriteRune(r)
		prevDigit, started = d, true
	}
	flush()
	return parts
}

// partRank orders non-numeric parts; a missing part ranks between the
// pre-release words and numbers ("1.0" > "1.0rc1", "1.0" < "1.0.1").
func partRank(p string) int {
	switch strings.ToLower(p) {
	case "dev":
		return 0
	case "alpha", "a":
		return 1
	case "beta", "b":
		return 2
	case "rc":
		return 3
	case "":
		return 4
	case "pl", "p":
		return 6
	}
	if _, err := strconv.Atoi(p); err == nil {
		return 5
	}
	return -1 // any other word sorts below dev
}

func comparePart(x, y string) int {
	nx, ex := strconv.Atoi(x)
	ny, ey := strconv.Atoi(y)
	if ex == nil && ey == nil {
		switch {
		case nx < ny:
			return -1
		case nx > ny:
			return 1
		}
		return 0
	}
	rx, ry := partRank(x), partRank(y)
	switch {
	case rx < ry:
		return -1
	case rx > ry:
		return 1
	}
	return 0
}
