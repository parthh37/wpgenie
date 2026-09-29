package site

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVersionCompareMatchesPHP(t *testing.T) {
	// Expected values are PHP's version_compare().
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"5.3.2", "5.3.2", 0},
		{"5.3.1", "5.3.2", -1},
		{"5.10", "5.9", 1}, // numeric, not lexical
		{"1.0", "1.0.0", -1},
		{"1.0", "1.0.1", -1},
		{"1.0rc1", "1.0", -1},
		{"1.0-beta", "1.0-alpha", 1},
		{"1.0b2", "1.0RC1", -1},
		{"1.0-dev", "1.0alpha", -1},
		{"1.0pl1", "1.0", 1},
		{"3.6.0", "3.6", 1},
		{"2.0_1", "2.0.1", 0},
		{"6.8.3", "6.8.10", -1},
	} {
		if got := versionCompare(c.a, c.b); got != c.want {
			t.Errorf("versionCompare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := versionCompare(c.b, c.a); got != -c.want {
			t.Errorf("versionCompare(%q, %q) = %d, want %d (antisymmetry)", c.b, c.a, got, -c.want)
		}
	}
}

// Trimmed from real wpvulnerability.net responses, quirks included:
// "updated" as a string, "impact" as [] when unscored, HTML entities.
const cf7Response = `{"error":0,"message":null,"updated":"1776563733",
 "data":{"name":"Contact Form 7","plugin":"contact-form-7","vulnerability":[
  {"name":"Contact Form 7 [contact-form-7] &lt; 5.3.2",
   "operator":{"min_version":null,"min_operator":null,"max_version":"5.3.2","max_operator":"lt","unfixed":"0","closed":"0"},
   "source":[{"id":"CVE-2020-35489","name":"CVE-2020-35489","link":"https://www.cve.org/CVERecord?id=CVE-2020-35489"}],
   "impact":{"cvss":{"score":"10.0","severity":"c"},"cvss3":{"version":"3.1","score":"10.0","severity":"critical"}}},
  {"name":"Contact Form 7 &#8211; ranged [contact-form-7] &gt;= 3.6.0 - &lt;= 3.6.2",
   "operator":{"min_version":"3.6.0","min_operator":"ge","max_version":"3.6.2","max_operator":"le","unfixed":"0","closed":"0"},
   "source":[{"id":"WPVDB-1","name":"x","link":"https://example.test/1"}],
   "impact":[]},
  {"name":"Contact Form 7 unfixed",
   "operator":{"min_version":"5.9","min_operator":"ge","max_version":null,"max_operator":null,"unfixed":"1","closed":"0"},
   "source":[],"impact":[]}
 ]}}`

func TestWPVulnerabilityLookup(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/plugin/contact-form-7/":
			w.Write([]byte(cf7Response))
		case "/plugin/unknown-plugin/":
			w.Write([]byte(`{"error":0,"message":null,"data":{"name":null,"plugin":"unknown-plugin","vulnerability":null},"updated":1784485982}`))
		case "/core/6.2.0/":
			w.Write([]byte(`{"error":0,"data":{"core":"6.2.0","vulnerability":[{"name":"WordPress &lt; 6.2.1 - Shortcode XSS","source":[],"impact":[]}]}}`))
		default:
			w.Write([]byte(`{"error":1,"message":"Invalid slug format.","data":null}`))
		}
	}))
	defer srv.Close()
	db := &WPVulnerability{BaseURL: srv.URL, UserAgent: "test"}
	ctx := context.Background()

	v, err := db.Lookup(ctx, "plugin", "contact-form-7", "5.3.1")
	if err != nil || len(v) != 1 {
		t.Fatalf("5.3.1: %+v %v", v, err)
	}
	if v[0].Title != "Contact Form 7 [contact-form-7] < 5.3.2" || v[0].FixedIn != "5.3.2" ||
		v[0].Severity != "critical" || v[0].CVE != "CVE-2020-35489" {
		t.Errorf("parsed %+v", v[0])
	}
	if v, _ := db.Lookup(ctx, "plugin", "contact-form-7", "5.3.2"); len(v) != 0 {
		t.Errorf("5.3.2 is the fix, got %+v", v)
	}
	if v, _ := db.Lookup(ctx, "plugin", "contact-form-7", "3.6.1"); len(v) != 2 || v[1].FixedIn != "" || v[1].Severity != "" {
		t.Errorf("3.6.1 (in the <=-range, unscored): %+v", v)
	}
	if v, _ := db.Lookup(ctx, "plugin", "contact-form-7", "6.0"); len(v) != 1 || !v[0].Unfixed {
		t.Errorf("6.0 (open-ended, unfixed): %+v", v)
	}
	if hits.Load() != 1 {
		t.Errorf("%d requests for one slug: answers must be cached", hits.Load())
	}
	if v, err := db.Lookup(ctx, "plugin", "unknown-plugin", "1.0"); err != nil || len(v) != 0 {
		t.Errorf("unknown slug: %+v %v", v, err)
	}
	if v, err := db.Lookup(ctx, "core", "", "6.2.0"); err != nil || len(v) != 1 || !strings.HasPrefix(v[0].Title, "WordPress < 6.2.1") {
		t.Errorf("core: %+v %v", v, err)
	}
	if _, err := db.Lookup(ctx, "theme", "ok-theme", "1"); err == nil || !strings.Contains(err.Error(), "Invalid slug") {
		t.Errorf("API error not surfaced: %v", err)
	}
	if _, err := db.Lookup(ctx, "plugin", "../../admin", "1"); err == nil {
		t.Error("slug with path characters sent to the API")
	}
}
