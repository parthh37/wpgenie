package cdn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestBuiltInRanges(t *testing.T) {
	r := NewRanges()
	for _, ip := range []string{"104.16.1.1", "172.64.0.9", "2606:4700::1", "::ffff:162.158.3.4"} {
		if !r.Contains(netip.MustParseAddr(ip)) {
			t.Errorf("%s is a Cloudflare edge address", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "127.0.0.1", "10.0.0.1", "2001:db8::1"} {
		if r.Contains(netip.MustParseAddr(ip)) {
			t.Errorf("%s is not Cloudflare", ip)
		}
	}
}

// Whoever holds a trusted range can claim to be any visitor, so a bad list
// must be rejected rather than trusted.
func TestParseRangesRejectsDangerousLists(t *testing.T) {
	good := append([]string(nil), cloudflareRanges...)
	for name, list := range map[string][]string{
		"everything v4": append(good, "0.0.0.0/0"),
		"everything v6": append(good, "::/0"),
		"wide":          append(good, "104.0.0.0/7"),
		"private":       append(good, "10.0.0.0/8"),
		"loopback":      append(good, "127.0.0.0/8"),
		"garbage":       append(good, "104.16.0.0/13 }"),
		"too short":     good[:2],
	} {
		if _, err := ParseRanges(list); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseRanges(good); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"success":true,"result":{"ipv4_cidrs":["173.245.48.0/20","103.21.244.0/22","104.16.0.0/13"],
			"ipv6_cidrs":["2400:cb00::/32","2606:4700::/32"]}}`)
	}))
	defer srv.Close()
	p, err := FetchRanges(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 5 || !(&Ranges{}).setAndContains(p, "2606:4700::5") {
		t.Fatalf("ranges = %v", p)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"success":true,"result":{"ipv4_cidrs":["0.0.0.0/0","1.0.0.0/8","2.0.0.0/8"],"ipv6_cidrs":["::/0"]}}`)
	}))
	defer bad.Close()
	if _, err := FetchRanges(context.Background(), bad.Client(), bad.URL); err == nil {
		t.Fatal("a response trusting the whole internet must be rejected")
	}
}

func (r *Ranges) setAndContains(p []netip.Prefix, ip string) bool {
	r.Set(p)
	return r.Contains(netip.MustParseAddr(ip))
}

const testToken = "abcdefghijklmnopqrstuvwxyz0123456789ABCD"

// fakeAPI answers like Cloudflare's v4 API for one zone, example.co.uk.
func fakeAPI(t *testing.T, purged *[]string) *Cloudflare {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.co.uk" {
				io.WriteString(w, `{"success":true,"result":[{"id":"z1","name":"example.co.uk","plan":{"name":"Free Website"}}]}`)
				return
			}
			io.WriteString(w, `{"success":true,"result":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/zones/z1/purge_cache":
			var body struct {
				Hosts []string `json:"hosts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			*purged = append(*purged, body.Hosts...)
			io.WriteString(w, `{"success":true,"result":{"id":"z1"}}`)
		case r.URL.Path == "/zones/z1/settings/ssl":
			io.WriteString(w, `{"success":true,"result":{"id":"ssl","value":"flexible"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"success":false,"errors":[{"code":7003,"message":"Could not route"}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Cloudflare{BaseURL: srv.URL, Client: srv.Client()}
}

func TestCloudflareClient(t *testing.T) {
	var purged []string
	cf := fakeAPI(t, &purged)
	ctx := context.Background()

	z, err := cf.FindZone(ctx, testToken, "www.shop.example.co.uk")
	if err != nil || z.ID != "z1" || z.Plan != "Free Website" {
		t.Fatalf("FindZone = %+v, %v", z, err)
	}
	if _, err := cf.FindZone(ctx, testToken, "other.test"); !errors.Is(err, ErrNoZone) {
		t.Errorf("unknown domain: %v, want ErrNoZone", err)
	}
	if _, err := cf.FindZone(ctx, strings.Repeat("x", 40), "example.co.uk"); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong token: %v, want ErrAuth", err)
	}
	if _, err := cf.FindZone(ctx, "bad token\r\nX-Evil: 1", "example.co.uk"); !errors.Is(err, ErrAuth) {
		t.Errorf("a malformed token must never reach a header: %v", err)
	}
	if err := cf.PurgeHosts(ctx, testToken, "z1", []string{"example.co.uk", "www.example.co.uk"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(purged, ",") != "example.co.uk,www.example.co.uk" {
		t.Errorf("purged %v", purged)
	}
	if mode, err := cf.SSLMode(ctx, testToken, "z1"); err != nil || mode != "flexible" {
		t.Errorf("SSLMode = %q, %v", mode, err)
	}
	if err := cf.PurgeHosts(ctx, testToken, "nope", []string{"a.test"}); err == nil || errors.Is(err, ErrAuth) {
		t.Errorf("unknown zone: %v", err)
	}
}
