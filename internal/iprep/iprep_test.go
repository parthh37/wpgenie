package iprep

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustPrefixes(t *testing.T, vs ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, v := range vs {
		p, ok := parsePrefix(v)
		if !ok {
			t.Fatalf("bad prefix %q", v)
		}
		out = append(out, p)
	}
	return out
}

func TestSetContains(t *testing.T) {
	s := NewSet(mustPrefixes(t, "203.0.113.0/24", "203.0.113.128/25", "198.51.100.7", "198.51.100.8/31",
		"2001:db8:1::/48", "2001:db8:1:5::/64", "2001:db8:ff::1", "192.0.2.255"))
	for addr, want := range map[string]bool{
		"203.0.113.0": true, "203.0.113.255": true, "203.0.114.0": false, "203.0.112.255": false,
		"198.51.100.6": false, "198.51.100.7": true, "198.51.100.8": true, "198.51.100.9": true, "198.51.100.10": false,
		"192.0.2.255": true, "192.0.2.254": false,
		"::ffff:203.0.113.9": true, // mapped addresses are looked up as IPv4
		"2001:db8:1::1":      true, "2001:db8:1:ffff:ffff:ffff:ffff:ffff": true, "2001:db8:2::": false,
		"2001:db8:ff::1": true, "2001:db8:ff::2": false, "2001:db8::": false,
	} {
		if got := s.Contains(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", addr, got, want)
		}
	}
	// Adjacent and overlapping ranges merge: the /25 inside the /24,
	// 198.51.100.7 + .8/31, the /64 inside the /48.
	if n := s.Len(); n != 5 {
		t.Errorf("Len() = %d, want 5 merged ranges", n)
	}
	var nilSet *Set
	if nilSet.Contains(netip.MustParseAddr("1.2.3.4")) {
		t.Error("nil set contains an address")
	}
	full := NewSet(mustPrefixes(t, "1.0.0.0/8", "255.255.255.255/32"))
	if !full.Contains(netip.MustParseAddr("255.255.255.255")) || !full.Contains(netip.MustParseAddr("1.255.255.255")) {
		t.Error("range bounds at the top of the address space")
	}
}

func TestParsersDropSpecialAndHugeRanges(t *testing.T) {
	in := strings.Join([]string{
		"# comment", "1.2.3.4", "5.6.7.0/24 ; SBL123", "10.1.2.3", "127.0.0.1", "192.168.1.0/24",
		"100.64.1.1", "0.0.0.0/0", "8.0.0.0/7", "2a01:4f8::/32", "fe80::1", "::1", "2000::/3", "junk", "",
	}, "\n")
	got, err := parsePlain(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	if want := "1.2.3.4/32 5.6.7.0/24 2a01:4f8::/32"; strings.Join(s, " ") != want {
		t.Errorf("parsed %v, want %s", s, want)
	}

	js := `{"cidr":"1.10.16.0/20","sblid":"SBL256894","rir":"apnic"}
{"cidr":"10.0.0.0/8","sblid":"x","rir":"x"}
{"type":"metadata","timestamp":1790603042,"size":103299,"records":2}
`
	got, err = parseSpamhausJSON(strings.NewReader(js))
	if err != nil || len(got) != 1 || got[0].String() != "1.10.16.0/20" {
		t.Errorf("spamhaus: %v %v", got, err)
	}
}

func TestListsRefreshKeepsGoodListOnBadDownload(t *testing.T) {
	body := "1.2.3.4\n5.6.7.8\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	dir := t.TempDir()
	feed := Feed{Name: "test", Title: "Test", URL: srv.URL, Parse: parsePlain, MinEntries: 2}
	l := &Lists{Dir: dir, Feeds: []Feed{feed}}
	if err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if name, ok := l.Listed(netip.MustParseAddr("5.6.7.8")); !ok || name != "test" {
		t.Fatalf("Listed = %q, %v", name, ok)
	}
	body = "<html>error</html>" // an error page served with 200
	if err := l.Refresh(context.Background()); err == nil {
		t.Fatal("implausible list accepted")
	}
	if _, ok := l.Listed(netip.MustParseAddr("5.6.7.8")); !ok {
		t.Error("a failed refresh dropped the previous list")
	}
	st := l.Status()
	if len(st) != 1 || st[0].Entries != 2 || st[0].Error == "" {
		t.Errorf("status %+v", st)
	}
	// A restart loads the saved copy without network.
	l2 := &Lists{Dir: dir, Feeds: []Feed{feed}}
	l2.Load()
	if _, ok := l2.Listed(netip.MustParseAddr("1.2.3.4")); !ok {
		t.Error("saved list not loaded")
	}
}

const geoCSV = `0.0.0.0,0.255.255.255,ZZ
1.0.0.0,1.0.0.255,AU
1.0.1.0,1.0.3.255,CN
1.0.4.0,1.0.7.255,AU
1.0.8.0,1.0.15.255,CN
1.0.16.0,1.0.16.255,CN
2001:200::,2001:200:ffff:ffff:ffff:ffff:ffff:ffff,JP
2001:201::,2001:201:ffff:ffff:ffff:ffff:ffff:ffff,JP
2a00::,2a00:ff:ffff:ffff:ffff:ffff:ffff:ffff,DE
`

func TestGeoLookup(t *testing.T) {
	db, err := parseGeoCSV(strings.NewReader(geoCSV))
	if err != nil {
		t.Fatal(err)
	}
	// Adjacent ranges of one country merge (CN 1.0.8.0 - 1.0.16.255, JP).
	if len(db.v4) != 5 || len(db.v6) != 2 {
		t.Errorf("merged into %d/%d ranges, want 5/2", len(db.v4), len(db.v6))
	}
	for addr, want := range map[string]string{
		"1.0.0.1": "AU", "1.0.2.3": "CN", "1.0.16.200": "CN", "1.0.17.0": "", "0.1.2.3": "",
		"2001:200::1": "JP", "2001:201:ab::": "JP", "2001:202::": "", "2a00:1::": "DE", "::ffff:1.0.5.5": "AU",
	} {
		if got := db.lookup(netip.MustParseAddr(addr)); got != want {
			t.Errorf("lookup(%s) = %q, want %q", addr, got, want)
		}
	}
	if _, err := parseGeoCSV(strings.NewReader("1.0.0.0,AU\n")); err == nil {
		t.Error("malformed line accepted")
	}
}

func TestCountriesDownloadFallsBackToLastMonth(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	// Enough IPv4 ranges to pass the completeness check.
	for i := 0; i < 60000; i++ {
		a := netip.AddrFrom4([4]byte{byte(1 + i>>16), byte(i >> 8), byte(i), 0})
		b := netip.AddrFrom4([4]byte{byte(1 + i>>16), byte(i >> 8), byte(i), 255})
		cc := [...]string{"DE", "FR"}[i%2]
		fmt.Fprintf(zw, "%s,%s,%s\n", a, b, cc)
	}
	zw.Close()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if r.URL.Path != "/2026-08.csv.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(gz.Bytes())
	}))
	defer srv.Close()
	dir := t.TempDir()
	c := &Countries{Dir: dir, URL: srv.URL + "/%s.csv.gz"}
	if _, ok := c.Country(netip.MustParseAddr("1.0.0.1")); ok {
		t.Fatal("country lookups must report no database before one is loaded")
	}
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	if err := c.Update(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if cc, ok := c.Country(netip.MustParseAddr("1.0.1.9")); !ok || cc != "FR" {
		t.Errorf("Country = %q, %v", cc, ok)
	}
	if st := c.Status(); !st.Loaded || st.Month != "2026-08" || st.Attribution == "" {
		t.Errorf("status %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "dbip-country-lite.csv.gz")); err != nil {
		t.Error(err)
	}
	c2 := &Countries{Dir: dir}
	if err := c2.Load(); err != nil {
		t.Fatal(err)
	}
	if cc, _ := c2.Country(netip.MustParseAddr("1.0.1.9")); cc != "FR" {
		t.Errorf("reloaded database says %q", cc)
	}
	if st := c2.Status(); st.Month != "2026-08" {
		t.Errorf("reloaded month %q", st.Month)
	}
	if strings.Join(asked, " ") != "/2026-09.csv.gz /2026-08.csv.gz" {
		t.Errorf("asked for %v", asked)
	}
}
