package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/store"
)

// TestSiteLockReachesProxy: the lock reaches Caddy as a bcrypt hash of the
// password, never the password, and the site's JSON has neither.
func TestSiteLockReachesProxy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetLock(ctx, "s1", LockInput{Enabled: true, Username: "team"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a first lock without a password: %v", err)
	}
	st, err := h.svc.SetLock(ctx, "s1", LockInput{Enabled: true, Username: " team ", Password: "correct horse",
		Allow: ptr([]string{"203.0.113.7", "2001:db8::/48"})})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Lock || st.LockUser != "team" || !reflect.DeepEqual(st.LockAllow, []string{"203.0.113.7/32", "2001:db8::/48"}) {
		t.Fatalf("stored %+v", st)
	}
	l := h.proxy.last[0].Lock
	if l == nil || l.User != "team" || bcrypt.CompareHashAndPassword([]byte(l.Hash), []byte("correct horse")) != nil ||
		len(l.Allow) != 2 || strings.Contains(l.Hash, "correct") {
		t.Fatalf("proxy lock %+v", l)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), l.Hash) || strings.Contains(string(b), "correct horse") || !strings.Contains(string(b), `"site_lock":true`) {
		t.Fatalf("site JSON: %s", b)
	}
	hash := l.Hash

	// Off keeps the username, password and networks; on again needs none.
	if st, err = h.svc.SetLock(ctx, "s1", LockInput{Enabled: false}); err != nil || st.Lock || st.LockUser != "team" {
		t.Fatalf("off: %+v %v", st, err)
	}
	if h.proxy.last[0].Lock != nil {
		t.Fatal("an unlocked site still asks for a password")
	}
	if st, err = h.svc.SetLock(ctx, "s1", LockInput{Enabled: true}); err != nil || !st.Lock || st.LockHash != hash || len(st.LockAllow) != 2 {
		t.Fatalf("on again: %+v %v", st, err)
	}
	// A new password replaces the hash; the networks can be cleared.
	if st, err = h.svc.SetLock(ctx, "s1", LockInput{Enabled: true, Username: "other", Password: "pässwörd!", Allow: ptr([]string{})}); err != nil ||
		st.LockHash == hash || st.LockUser != "other" || len(st.LockAllow) != 0 ||
		bcrypt.CompareHashAndPassword([]byte(h.proxy.last[0].Lock.Hash), []byte("pässwörd!")) != nil {
		t.Fatalf("changed: %+v %v", st, err)
	}

	for name, in := range map[string]LockInput{
		"short password":     {Enabled: true, Password: "seven77"},
		"long password":      {Enabled: true, Password: strings.Repeat("x", 73)},
		"control characters": {Enabled: true, Password: "pass\nword123"},
		"username space":     {Enabled: true, Username: "the team"},
		"username colon":     {Enabled: true, Username: "a:b"},
		"username brace":     {Enabled: true, Username: "{env.X}"},
		"username newline":   {Enabled: true, Username: "a\nb"},
		"username @ first":   {Enabled: true, Username: "@x"},
		"allow everyone":     {Enabled: true, Allow: ptr([]string{"0.0.0.0/0"})},
		"allow junk":         {Enabled: true, Allow: ptr([]string{"1.2.3.4 }"})},
	} {
		if _, err := h.svc.SetLock(ctx, "s1", in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if st, _ := h.svc.Store.GetSite(ctx, "s1"); st.LockUser != "other" {
		t.Error("a refused change was stored")
	}
	if _, err := h.svc.SetLock(ctx, "nope", LockInput{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing site: %v", err)
	}
}

// A staging copy can be locked from the start; without asking, it isn't.
func TestStagingLockFromTheStart(t *testing.T) {
	st := newSite("staging.a.test", "Staging: a")
	if err := applyLock(st, LockInput{Enabled: true, Username: "preview", Password: "letmesee1"}); err != nil {
		t.Fatal(err)
	}
	if !st.Lock || st.LockUser != "preview" || bcrypt.CompareHashAndPassword([]byte(st.LockHash), []byte("letmesee1")) != nil {
		t.Fatalf("%+v", st)
	}
	if st := newSite("staging.a.test", "x"); st.Lock || proxyLock(st) != nil {
		t.Fatal("new sites are open")
	}
	h := newHarness(t)
	if _, _, err := h.svc.StartStaging(context.Background(), "s1", StagingInput{Lock: &LockInput{Enabled: true, Username: "p", Password: "short"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a bad lock must refuse the copy before anything is made: %v", err)
	}
	if ids, _ := h.svc.Store.StagingOf(context.Background(), "s1"); len(ids) != 0 {
		t.Fatalf("staging made: %v", ids)
	}
}

// A lock arriving with a moved site is checked; a locked site never
// arrives open.
func TestImportedLock(t *testing.T) {
	good, _ := bcrypt.GenerateFromPassword([]byte("password1"), bcrypt.MinCost)
	for _, c := range []struct {
		name string
		st   store.Site
		ok   bool
		lock bool
	}{
		{"locked", store.Site{Lock: true, LockUser: "u", LockHash: string(good), LockAllow: []string{"203.0.113.0/24"}}, true, true},
		{"open with credentials", store.Site{LockUser: "u", LockHash: string(good)}, true, false},
		{"locked, hash lost", store.Site{Lock: true, LockUser: "u"}, false, false},
		{"locked, bad hash", store.Site{Lock: true, LockUser: "u", LockHash: "plain"}, false, false},
		{"locked, bad user", store.Site{Lock: true, LockUser: "a b", LockHash: string(good)}, false, false},
		{"locked, bad network", store.Site{Lock: true, LockUser: "u", LockHash: string(good), LockAllow: []string{"x }"}}, false, false},
		{"open, bad credentials dropped", store.Site{LockUser: "a b", LockHash: "plain"}, true, false},
	} {
		st := c.st
		err := checkImportedLock(&st)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if err == nil && (proxyLock(&st) != nil) != c.lock {
			t.Errorf("%s: lock %+v", c.name, proxyLock(&st))
		}
	}
}

func TestRedirectsReachProxy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	out, err := h.svc.SetRedirects(ctx, "s1", []store.Redirect{
		{From: "/old-page/", To: "/new-page"},
		{From: "https://a.test/Blog/*", To: "https://example.com/blog?src=old", Code: 308},
		{From: "/caf%C3%A9", To: "/café-new?x=é", Code: 302},
		{From: "/keep", To: "/kept", KeepQuery: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Redirect{
		{From: "/old-page", To: "/new-page", Code: 301},
		{From: "/Blog/*", To: "https://example.com/blog?src=old", Code: 308},
		{From: "/café", To: "/caf%C3%A9-new?x=%C3%A9", Code: 302},
		{From: "/keep", To: "/kept", Code: 301, KeepQuery: true},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("stored %+v", out)
	}
	if got, _ := h.svc.Redirects(ctx, "s1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %+v", got)
	}
	if got := h.proxy.last[0].PathRedirects; len(got) != 4 || got[1] != (proxy.PathRedirect{From: "/Blog/*",
		To: "https://example.com/blog?src=old", Code: 308}) {
		t.Fatalf("proxy %+v", got)
	}
	for _, c := range []struct {
		path, loc string
		rule      int
	}{
		{"/old-page", "/new-page", 0},
		{"/OLD-PAGE/", "/new-page", 0},
		{"https://a.test/blog/2020/post?utm=x", "https://example.com/blog?src=old", 1},
		{"blog", "https://example.com/blog?src=old", 1},
		{"/caf%C3%A9", "/caf%C3%A9-new?x=%C3%A9", 2},
		{"/keep?a=1&b=2", "/kept?a=1&b=2", 3},
		{"/keep", "/kept", 3},
		{"/other", "", -1},
	} {
		m, err := h.svc.TestRedirect(ctx, "s1", c.path)
		if err != nil || m.Rule != c.rule || m.Location != c.loc || (c.rule >= 0) != (m.Redirect != nil) {
			t.Errorf("test %q: %+v %v, want rule %d to %q", c.path, m, err, c.rule, c.loc)
		}
	}
	if out, err := h.svc.SetRedirects(ctx, "s1", nil); err != nil || len(out) != 0 || len(h.proxy.last[0].PathRedirects) != 0 {
		t.Fatalf("cleared: %v %v", out, err)
	}
}

// TestNormalizeRedirectsRefusesHostileInput: nothing that could reach the
// Caddyfile as more than a path or an address gets through, and the
// messages say what is wrong.
func TestNormalizeRedirectsRefusesHostileInput(t *testing.T) {
	hosts := []string{"a.test", "www.a.test"}
	for _, c := range []struct {
		from, to string
		code     int
		keep     bool
		msg      string
	}{
		{"", "/x", 0, false, "empty"},
		{"/x", "", 0, false, "empty"},
		{"old", "/x", 0, false, "starts with /"},
		{"/old page", "/x", 0, false, "spaces"},
		{"/old\n\trespond 200", "/x", 0, false, "spaces"},
		{"/old%0Arespond", "/x", 0, false, "can't contain"},
		{"/{path}", "/x", 0, false, "can't contain"},
		{"/a}", "/x", 0, false, "can't contain"},
		{`/a"b`, "/x", 0, false, "can't contain"},
		{"/a`b", "/x", 0, false, "can't contain"},
		{`/a\b`, "/x", 0, false, "can't contain"},
		{"/a[1]", "/x", 0, false, "can't contain"},
		{"/a%5B1%5D", "/x", 0, false, "can't contain"},
		{"/a%2", "/x", 0, false, "%-escape"},
		{"/a%25b", "/x", 0, false, "can't contain"},
		{"/a*b", "/x", 0, false, "only goes at the end"},
		{"/a/*/b", "/x", 0, false, "only goes at the end"},
		{"/*a", "/x", 0, false, "only goes at the end"},
		{"/a?b=1", "/x", 0, false, "path only"},
		{"/a#top", "/x", 0, false, "path only"},
		{"/a/../b", "/x", 0, false, "write the path as /b"},
		{"/a//b", "/x", 0, false, "write the path as /a/b"},
		{"/a/./b", "/x", 0, false, "write the path as /a/b"},
		{"/_wpgenie/login", "/x", 0, false, "WPGenie's"},
		{"/_WPGenie/*", "/x", 0, false, "WPGenie's"},
		{"/_shield/verify", "/x", 0, false, "WPGenie's"},
		{"/.well-known/acme-challenge/x", "/x", 0, false, "WPGenie's"},
		{"https://evil.test/old", "/x", 0, false, "path of this site"},
		{"https://user@a.test/old", "/x", 0, false, "path of this site"},
		{"/a b", "/x", 0, false, "spaces"},
		{"/a​b", "/x", 0, false, "can't contain"},
		{"/" + strings.Repeat("a", 600), "/x", 0, false, "longer"},
		{"/x", "x", 0, false, "start with /"},
		{"/x", "javascript:alert(1)", 0, false, "start with /"},
		{"/x", "http://example.com", 0, false, "https://"},
		{"/x", "//evil.test/", 0, false, "start with /"},
		{"/x", "/\\evil.test", 0, false, "can't contain"},
		{"/x", "https://example.com/ a", 0, false, "spaces"},
		{"/x", "https://example.com/{uri}", 0, false, "can't contain"},
		{"/x", "https://example.com/\nrespond 200", 0, false, "spaces"},
		{"/x", "https://example.com/#top", 0, false, "can't contain"},
		{"/x", "https://example.com/<script>", 0, false, "can't contain"},
		{"/x", `https://example.com/"`, 0, false, "can't contain"},
		{"/x", "https://example.com/`", 0, false, "can't contain"},
		{"/x", "https://example.com|x/", 0, false, "can't contain"},
		{"/x", "https://user:pw@example.com/", 0, false, "username or password"},
		{"/x", "https://exa_mple.com/", 0, false, "not a domain"},
		{"/x", "https://127.0.0.1/", 0, false, "not a domain"},
		{"/x", "https://example.com:0/", 0, false, "not a port"},
		{"/x", "https://example.com:99999/", 0, false, "not a port"},
		{"/x", "https://example.com/%zz", 0, false, "not a web address"},
		{"/x", "/a?b=%z", 0, false, "%-escape"},
		{"/x", "https://" + strings.Repeat("a", 2100) + ".com", 0, false, "longer"},
		{"/x", "/y", 200, false, "301, 302, 307 or 308"},
		{"/x", "/y", 304, false, "301, 302, 307 or 308"},
		{"/x", "/y?a=1", 301, true, "already has a query string"},
		{"/x", "/x/", 301, false, "round in circles"},
		{"/x/*", "/x/y", 301, false, "round in circles"},
		{"/*", "/anything", 301, false, "round in circles"},
		{"/*", "https://www.a.test/", 301, false, "round in circles"},
	} {
		_, err := NormalizeRedirects([]store.Redirect{{From: c.from, To: c.to, Code: c.code, KeepQuery: c.keep}}, hosts)
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q -> %q (%d): %v, want %q", c.from, c.to, c.code, err, c.msg)
		}
	}
	// Whatever is accepted is safe for the Caddyfile as it is.
	for _, c := range []struct{ from, to, wantFrom, wantTo string }{
		{"/", "https://example.com", "/", "https://example.com"},
		{"/*", "https://new.example.com/", "/*", "https://new.example.com/"},
		{"/Old-Page/", "/new", "/Old-Page", "/new"},
		{"/old/*", "/new/", "/old/*", "/new/"},
		{"https://www.a.test/old/", "/new", "/old", "/new"},
		{"/naïve", "/na%C3%AFve-2", "/naïve", "/na%C3%AFve-2"},
		{"/a'b(c)!$&+,;=:@~", "/x", "/a'b(c)!$&+,;=:@~", "/x"},
		{"/x", "https://EXAMPLE.com:8443/ab", "/x", "https://example.com:8443/ab"},
		{"/x", "/a[1]?q=[2]", "/x", "/a%5B1%5D?q=%5B2%5D"},
		{"/x", "/söme?q=ä", "/x", "/s%C3%B6me?q=%C3%A4"},
		{"/a", "https://www.a.test/b", "/a", "https://www.a.test/b"},
	} {
		out, err := NormalizeRedirects([]store.Redirect{{From: c.from, To: c.to}}, hosts)
		if err != nil || out[0].From != c.wantFrom || out[0].To != c.wantTo || out[0].Code != 301 {
			t.Errorf("%q -> %q: %+v %v", c.from, c.to, out, err)
			continue
		}
		if !proxy.ValidRedirectFrom(out[0].From) || !proxy.ValidRedirectTo(out[0].To) {
			t.Errorf("%q -> %q normalised to something the proxy refuses: %+v", c.from, c.to, out[0])
		}
	}
}

func TestRedirectListRules(t *testing.T) {
	hosts := []string{"a.test"}
	many := make([]store.Redirect, MaxRedirects+1)
	for i := range many {
		many[i] = store.Redirect{From: fmt.Sprintf("/p%d", i), To: "/x"}
	}
	if _, err := NormalizeRedirects(many, hosts); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("%d rules: %v", len(many), err)
	}
	if _, err := NormalizeRedirects(many[:MaxRedirects], hosts); err != nil {
		t.Fatalf("%d rules: %v", MaxRedirects, err)
	}
	for _, c := range []struct {
		rules []store.Redirect
		msg   string
	}{
		{[]store.Redirect{{From: "/a", To: "/b"}, {From: "/A/", To: "/c"}}, "rule 2 (/A/): rule 1 already redirects /A"},
		{[]store.Redirect{{From: "/a", To: "/b"}, {From: "/b", To: "/a"}}, "rules 1 (/a) and 2 (/b) send visitors round in circles"},
		{[]store.Redirect{{From: "/a", To: "/b"}, {From: "/b/*", To: "/c"}, {From: "/c", To: "https://a.test/a?x=1"}}, "round in circles"},
	} {
		if _, err := NormalizeRedirects(c.rules, hosts); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%+v: %v, want %q", c.rules, err, c.msg)
		}
	}
	// A chain that ends somewhere is fine; so are exact and prefix rules
	// on the same folder (the exact one wins).
	if _, err := NormalizeRedirects([]store.Redirect{{From: "/a", To: "/b"}, {From: "/b", To: "/c"},
		{From: "/old/*", To: "/new"}, {From: "/old", To: "/older-than-old"}}, hosts); err != nil {
		t.Fatal(err)
	}
}
