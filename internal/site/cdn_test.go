package site

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/store"
)

const cfToken = "abcdefghijklmnopqrstuvwxyz0123456789ABCD"

type fakeCDN struct {
	purges   []string // "zone:host,host"
	purgeErr error
	ssl      string
}

func (f *fakeCDN) FindZone(_ context.Context, token, host string) (cdn.Zone, error) {
	if token != cfToken {
		return cdn.Zone{}, fmt.Errorf("%w: Invalid access token", cdn.ErrAuth)
	}
	if !strings.HasSuffix(host, ".test") {
		return cdn.Zone{}, cdn.ErrNoZone
	}
	return cdn.Zone{ID: "z-test", Name: "a.test"}, nil
}

func (f *fakeCDN) PurgeHosts(_ context.Context, _, zone string, hosts []string) error {
	if f.purgeErr != nil {
		return f.purgeErr
	}
	f.purges = append(f.purges, zone+":"+strings.Join(hosts, ","))
	return nil
}

func (f *fakeCDN) SSLMode(context.Context, string, string) (string, error) {
	if f.ssl == "" {
		return "", fmt.Errorf("%w: missing Zone Settings: Read", cdn.ErrAuth)
	}
	return f.ssl, nil
}

type fakeResolver map[string][]string

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, a := range r[host] {
		out = append(out, netip.MustParseAddr(a))
	}
	if len(out) == 0 {
		return nil, errors.New("no such host")
	}
	return out, nil
}

func cdnHarness(t *testing.T) (*harness, *fakeCDN) {
	h := newHarness(t)
	f := &fakeCDN{ssl: "strict"}
	h.svc.CDN, h.svc.CDNRanges = f, cdn.NewRanges()
	h.svc.DNS = fakeResolver{"a.test": {"104.16.1.1", "2606:4700::6810:101"}}
	return h, f
}

func TestSetCDN(t *testing.T) {
	h, f := cdnHarness(t)
	ctx := context.Background()

	for name, in := range map[string]CDNInput{
		"no token":      {Provider: "cloudflare"},
		"wrong token":   {Provider: "cloudflare", APIToken: strings.Repeat("x", 40)},
		"header inject": {Provider: "cloudflare", APIToken: cfToken + "\r\nX: y"},
		"bad provider":  {Provider: "akamai", APIToken: cfToken},
	} {
		if _, err := h.svc.SetCDN(ctx, "s1", in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v, want ErrInvalidInput", name, err)
		}
	}
	f.purgeErr = fmt.Errorf("%w: missing Cache Purge", cdn.ErrAuth)
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken}); !errors.Is(err, ErrInvalidInput) ||
		!strings.Contains(err.Error(), "Cache Purge") {
		t.Errorf("a token that can't purge must be refused with a hint: %v", err)
	}
	if _, err := h.svc.Store.GetCDN(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused token must not be stored")
	}

	f.purgeErr = nil
	st, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken})
	if err != nil {
		t.Fatal(err)
	}
	if st.Provider != "cloudflare" || st.PurgedAt == nil || len(st.Warnings) != 0 || st.Domains[0].Proxied != "yes" ||
		st.Domains[0].SSLMode != "strict" {
		t.Errorf("status = %+v", st)
	}
	if len(f.purges) != 1 || f.purges[0] != "z-test:a.test" {
		t.Errorf("enabling must purge the site's own hostnames once: %v", f.purges)
	}
	// Re-saving without a token keeps the stored one.
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare"}); err != nil {
		t.Errorf("keeping the stored token: %v", err)
	}
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Store.GetCDN(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Error("turning the integration off must delete the token")
	}
}

func touchMarker(t *testing.T, h *harness, at time.Time) {
	t.Helper()
	p := filepath.Join(h.svc.Cfg.SiteRoot("s1"), pageCacheMarker)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// WordPress purges its page cache in PHP, which has no token: the daemon
// follows the marker PHP touches.
func TestCDNFollowsPageCachePurges(t *testing.T) {
	h, f := cdnHarness(t)
	ctx := context.Background()
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken}); err != nil {
		t.Fatal(err)
	}
	f.purges = nil
	forget := func() { h.svc.cdn.lastAttempt.Delete("s1") } // skip the cooldown

	touchMarker(t, h, time.Now().Add(-time.Hour)) // before the integration was enabled
	forget()
	h.svc.cdnPass(ctx)
	if len(f.purges) != 0 {
		t.Fatalf("an old purge must not purge the CDN again: %v", f.purges)
	}

	touchMarker(t, h, time.Now())
	h.svc.cdn.lastAttempt.Store("s1", time.Now()) // purged moments ago
	h.svc.cdnPass(ctx)
	if len(f.purges) != 0 {
		t.Fatalf("purges must be spaced out (Cloudflare Free allows 5/min per account): %v", f.purges)
	}
	forget()
	h.svc.cdnPass(ctx)
	if len(f.purges) != 1 {
		t.Fatalf("a WordPress purge must purge the CDN: %v", f.purges)
	}
	forget()
	h.svc.cdnPass(ctx)
	if len(f.purges) != 1 {
		t.Fatalf("one WordPress purge, one CDN purge: %v", f.purges)
	}

	// A failure is recorded and retried later, not forgotten.
	f.purgeErr = errors.New("cloudflare: service unavailable")
	touchMarker(t, h, time.Now().Add(time.Second))
	forget()
	h.svc.cdnPass(ctx)
	c, _ := h.svc.Store.GetCDN(ctx, "s1")
	if c.LastError == "" {
		t.Fatal("failure not recorded")
	}
	f.purgeErr = nil
	forget()
	h.svc.cdnPass(ctx)
	if c, _ := h.svc.Store.GetCDN(ctx, "s1"); len(f.purges) != 2 || c.LastError != "" {
		t.Errorf("retry after a failure: purges %v, last error %q", f.purges, c.LastError)
	}
}

func TestPanelPurgeIncludesCDN(t *testing.T) {
	h, f := cdnHarness(t)
	ctx := context.Background()
	if err := h.svc.Purge(ctx, "s1"); err != nil || len(f.purges) != 0 {
		t.Fatalf("no integration: %v, %v", err, f.purges)
	}
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken}); err != nil {
		t.Fatal(err)
	}
	f.purges = nil
	if err := h.svc.Purge(ctx, "s1"); err != nil || len(f.purges) != 1 {
		t.Fatalf("panel purge must purge the CDN: %v, %v", err, f.purges)
	}
	// The panel's own purge touches the marker; the loop must not purge again.
	h.svc.cdn.lastAttempt.Delete("s1")
	h.svc.cdnPass(ctx)
	if len(f.purges) != 1 {
		t.Errorf("the loop re-purged after a panel purge: %v", f.purges)
	}
	f.purgeErr = errors.New("cloudflare down")
	st, _ := h.svc.Store.GetSite(ctx, "s1")
	if err := h.svc.purgeLocal(ctx, st); err != nil {
		t.Errorf("local caches must purge regardless of the CDN: %v", err)
	}
}

func TestCDNStatusWarnings(t *testing.T) {
	h, f := cdnHarness(t)
	ctx := context.Background()

	// Proxied through Cloudflare, but no token: suggest purging.
	st, err := h.svc.CDNStatus(ctx, "s1")
	if err != nil || len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "API token") {
		t.Fatalf("status = %+v, %v", st, err)
	}

	f.ssl = "flexible"
	if _, err := h.svc.SetCDN(ctx, "s1", CDNInput{Provider: "cloudflare", APIToken: cfToken}); err != nil {
		t.Fatal(err)
	}
	h.svc.DNS = fakeResolver{"a.test": {"203.0.113.9"}}
	st, _ = h.svc.CDNStatus(ctx, "s1")
	joined := strings.Join(st.Warnings, "\n")
	if st.Domains[0].Proxied != "no" || !strings.Contains(joined, "endless redirect") || !strings.Contains(joined, "orange cloud") {
		t.Errorf("flexible SSL and an unproxied record must both be flagged:\n%s", joined)
	}

	f.ssl = "" // token lacks Zone Settings: Read
	h.svc.DNS = fakeResolver{"a.test": {"104.16.1.1", "203.0.113.9"}}
	st, _ = h.svc.CDNStatus(ctx, "s1")
	joined = strings.Join(st.Warnings, "\n")
	if st.Domains[0].Proxied != "partly" || !strings.Contains(joined, "Zone Settings: Read") {
		t.Errorf("status = %+v", st)
	}
}
