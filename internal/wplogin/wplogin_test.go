package wplogin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/phpmyadmin"
	"github.com/parthh37/wpgenie/internal/site"
)

type fakeMinter struct {
	calls  int
	ip, ua string
	domain string
}

func (f *fakeMinter) AdminLogin(_ context.Context, id string, userID int, ip, ua string) (*site.AdminSession, error) {
	f.calls++
	f.ip, f.ua = ip, ua
	return &site.AdminSession{UserID: 1, User: "boss", Host: "example.com", AdminPath: "/wp-admin/", Domain: f.domain,
		Cookies: []site.AuthCookie{
			{Name: "wordpress_sec_abc", Value: "boss|1|tok|mac", Path: "/wp-content/plugins"},
			{Name: "wordpress_sec_abc", Value: "boss|1|tok|mac", Path: "/wp-admin"},
			{Name: "wordpress_logged_in_abc", Value: "boss|1|tok|mac2", Path: "/"},
		}}, nil
}

func newService() (*Service, *fakeMinter, *time.Time) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := &fakeMinter{}
	return &Service{Sites: m, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}, m, &now
}

func visit(t *testing.T, s *Service, link, siteID string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", u.RequestURI(), nil)
	req.Host = u.Host
	req.Header.Set(phpmyadmin.SiteHeader, siteID)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestLinkSignsInOnce(t *testing.T) {
	s, m, _ := newService()
	l, err := s.Open(context.Background(), "s1", 0, "ann", "203.0.113.9", "Firefox")
	if err != nil {
		t.Fatal(err)
	}
	if m.ip != "203.0.113.9" || m.ua != "Firefox" {
		t.Errorf("session made for %q %q, want the requester's address and browser", m.ip, m.ua)
	}
	if !strings.HasPrefix(l.URL, "https://example.com"+Path+"?"+tokenParam+"=") || l.User != "boss" {
		t.Fatalf("link = %+v", l)
	}
	w := visit(t, s, l.URL, "s1")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/wp-admin/" {
		t.Fatalf("redeem: %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 3 {
		t.Fatalf("got %d cookies, want WordPress's 3", len(cookies))
	}
	for _, c := range cookies {
		if !c.Secure || !c.HttpOnly || c.Domain != "" || c.MaxAge != 0 || !c.Expires.IsZero() {
			t.Errorf("cookie %s: secure=%v httponly=%v domain=%q maxage=%d: want a secure, host-only session cookie",
				c.Name, c.Secure, c.HttpOnly, c.Domain, c.MaxAge)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("the sign-in response must not be cached or leak its URL")
	}
	// Single use.
	if w := visit(t, s, l.URL, "s1"); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Fatalf("second use: %d, want 403 and no cookies", w.Code)
	}
}

func TestLinkIsBoundToItsSiteHostAndTime(t *testing.T) {
	s, _, now := newService()
	ctx := context.Background()

	l, _ := s.Open(ctx, "s1", 0, "ann", "", "")
	// Presented on another site (same token, other X-WPGenie-Site): refused,
	// and the link still works where it belongs.
	if w := visit(t, s, l.URL, "s2"); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Fatalf("other site: %d", w.Code)
	}
	if w := visit(t, s, l.URL, "s1"); w.Code != http.StatusSeeOther {
		t.Fatalf("own site after a wrong one: %d", w.Code)
	}

	// Another host of the same site: the cookies would land on the wrong host.
	l, _ = s.Open(ctx, "s1", 0, "ann", "", "")
	if w := visit(t, s, strings.Replace(l.URL, "example.com", "www.example.com", 1), "s1"); w.Code != http.StatusForbidden {
		t.Fatalf("other host: %d", w.Code)
	}

	// Expired.
	l, _ = s.Open(ctx, "s1", 0, "ann", "", "")
	*now = now.Add(TokenTTL + time.Second)
	if w := visit(t, s, l.URL, "s1"); w.Code != http.StatusForbidden {
		t.Fatalf("expired: %d", w.Code)
	}

	// No token, a made-up one, no site header.
	if w := visit(t, s, "https://example.com"+Path, "s1"); w.Code != http.StatusForbidden {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := visit(t, s, "https://example.com"+Path+"?"+tokenParam+"=00", "s1"); w.Code != http.StatusForbidden {
		t.Fatalf("made-up token: %d", w.Code)
	}
	l, _ = s.Open(ctx, "s1", 0, "ann", "", "")
	if w := visit(t, s, l.URL, ""); w.Code != http.StatusNotFound {
		t.Fatalf("no site header: %d", w.Code)
	}
}

func TestSiteRemovedDropsLinks(t *testing.T) {
	s, _, _ := newService()
	l, _ := s.Open(context.Background(), "s1", 0, "ann", "", "")
	s.SiteRemoved(context.Background(), "s1")
	if w := visit(t, s, l.URL, "s1"); w.Code != http.StatusForbidden {
		t.Fatalf("after removal: %d", w.Code)
	}
}

func TestCookieDomain(t *testing.T) {
	for _, c := range []struct{ domain, host, want string }{
		{"", "example.com", ""},
		{".example.com", "example.com", "example.com"},
		{".example.com", "shop.example.com", "example.com"},
		{"example.com", "notexample.com", ""}, // not a parent of the host
		{".com", "example.com", ""},           // a whole TLD
		{"other.org", "example.com", ""},
	} {
		if got := cookieDomain(c.domain, c.host); got != c.want {
			t.Errorf("cookieDomain(%q, %q) = %q, want %q", c.domain, c.host, got, c.want)
		}
	}
}

func TestSiteCookieDomainIsUsed(t *testing.T) {
	s, m, _ := newService()
	m.domain = ".example.com"
	l, _ := s.Open(context.Background(), "s1", 0, "ann", "", "")
	w := visit(t, s, l.URL, "s1")
	for _, c := range w.Result().Cookies() {
		if c.Domain != "example.com" {
			t.Errorf("cookie %s domain %q, want the site's COOKIE_DOMAIN", c.Name, c.Domain)
		}
	}
}
