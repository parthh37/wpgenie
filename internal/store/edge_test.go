package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestSiteLockAndRedirects: the lock is part of the site's record, its hash
// never part of the site's JSON; redirects are a list of their own.
func TestSiteLockAndRedirects(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		for i, id := range []string{"s1", "s2"} {
			if err := s.CreateSite(ctx, &Site{ID: id, Name: id, PrimaryDomain: id + ".test", PHPVersion: "8.3",
				FPMPort: 19600 + i, DBName: "wp_" + id, Status: StatusActive, ShieldMode: "standard"}); err != nil {
				t.Fatal(err)
			}
		}
		st, err := s.GetSite(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		if st.Lock || st.LockUser != "" || st.LockHash != "" || st.LockAllow == nil || len(st.LockAllow) != 0 {
			t.Fatalf("a new site is open: %+v", st)
		}
		const hash = "$2a$10$abcdefghijklmnopqrstuuOJqQ0n4bF5uWlR5sT2m3aM7HxVb0V3m"
		if err := s.SetSiteLock(ctx, "s1", true, "team", hash, []string{"203.0.113.7/32", "2001:db8::/48"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSiteLock(ctx, "nope", true, "x", hash, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lock on a missing site: %v", err)
		}
		st, _ = s.GetSite(ctx, "s1")
		if !st.Lock || st.LockUser != "team" || st.LockHash != hash ||
			!reflect.DeepEqual(st.LockAllow, []string{"203.0.113.7/32", "2001:db8::/48"}) {
			t.Fatalf("locked site: %+v", st)
		}
		b, _ := json.Marshal(st)
		if strings.Contains(string(b), "$2a$") || !strings.Contains(string(b), `"site_lock":true`) ||
			!strings.Contains(string(b), `"site_lock_user":"team"`) {
			t.Fatalf("site JSON: %s", b)
		}
		// Listing reads the same columns; a site created with a lock (a
		// staging copy, a move) keeps it.
		list, err := s.ListSites(ctx)
		if err != nil || len(list) != 2 || !list[0].Lock || list[0].LockHash != hash || list[1].Lock {
			t.Fatalf("list: %v %+v", err, list)
		}
		if err := s.CreateSite(ctx, &Site{ID: "s3", Name: "s3", PrimaryDomain: "s3.test", PHPVersion: "8.3",
			FPMPort: 19610, DBName: "wp_s3", Status: StatusActive, ShieldMode: "standard",
			Lock: true, LockUser: "u", LockHash: hash, LockAllow: []string{"198.51.100.0/24"}}); err != nil {
			t.Fatal(err)
		}
		if st, _ := s.GetSite(ctx, "s3"); !st.Lock || st.LockUser != "u" || st.LockHash != hash || len(st.LockAllow) != 1 {
			t.Fatalf("created locked: %+v", st)
		}

		if rules, err := s.SiteRedirects(ctx, "s1"); err != nil || rules == nil || len(rules) != 0 {
			t.Fatalf("no redirects yet: %v %v", rules, err)
		}
		rules := []Redirect{{From: "/old", To: "/new", Code: 301}, {From: "/blog/*", To: "https://blog.example.com/", Code: 308, KeepQuery: true}}
		if err := s.SetSiteRedirects(ctx, "s1", rules); err != nil {
			t.Fatal(err)
		}
		if got, err := s.SiteRedirects(ctx, "s1"); err != nil || !reflect.DeepEqual(got, rules) {
			t.Fatalf("redirects: %v %+v", err, got)
		}
		all, err := s.AllSiteRedirects(ctx)
		if err != nil || len(all) != 1 || !reflect.DeepEqual(all["s1"], rules) {
			t.Fatalf("all: %v %+v", err, all)
		}
		if err := s.SetSiteRedirects(ctx, "s1", nil); err != nil {
			t.Fatal(err)
		}
		if all, _ := s.AllSiteRedirects(ctx); len(all) != 0 {
			t.Fatalf("cleared: %+v", all)
		}
		if _, err := s.SiteRedirects(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing site: %v", err)
		}
		if err := s.SetSiteRedirects(ctx, "nope", rules); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing site: %v", err)
		}
	})
}
