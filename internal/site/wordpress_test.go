package site

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// A 1x1 PNG.
var pngLogo, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

func dataURI(ctype string, b []byte) *string {
	s := "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(b)
	return &s
}

func TestParseLogo(t *testing.T) {
	if ct, b, err := parseLogo(*dataURI("image/png", pngLogo)); err != nil || ct != "image/png" || len(b) != len(pngLogo) {
		t.Fatalf("png: %q %d %v", ct, len(b), err)
	}
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"/>`)
	if ct, _, err := parseLogo(*dataURI("image/svg+xml", svg)); err != nil || ct != "image/svg+xml" {
		t.Fatalf("svg: %q %v", ct, err)
	}
	for name, uri := range map[string]string{
		"not a data URI":        "https://example.com/logo.png",
		"not base64":            "data:image/png,rawbytes",
		"type not allowed":      *dataURI("text/html", []byte("<p>hi</p>")),
		"bytes aren't the type": *dataURI("image/png", []byte("<html><script>alert(1)</script>")),
		"svg that isn't":        *dataURI("image/svg+xml", []byte("hello")),
		"too big":               *dataURI("image/png", append(slices.Clone(pngLogo), make([]byte, maxLogoBytes)...)),
		"bad base64":            "data:image/png;base64,!!!",
	} {
		if _, _, err := parseLogo(uri); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestBrandWrapperIsSafePHP(t *testing.T) {
	b := &Branding{Name: `Evil'); system('id'); //`, URL: "https://x.test/?a='b'", LogoType: "image/png", Logo: pngLogo}
	w := brandWrapperFor(b)
	if strings.Contains(w, "system(") || strings.Contains(w, "'b'") {
		t.Fatalf("brand values reached PHP unencoded:\n%s", w)
	}
	if !strings.Contains(w, "define( 'WPGENIE_BRAND_LOGO', '"+BrandLogoPath+"?v="+b.LogoVersion()+"' );") {
		t.Errorf("logo path missing:\n%s", w)
	}
	if !strings.Contains(w, managedMarker) {
		t.Error("wrapper must carry the managed marker")
	}
	if got, _ := base64.StdEncoding.DecodeString(between(w, "WPGENIE_BRAND_NAME', base64_decode( '", "'")); string(got) != b.Name {
		t.Errorf("name round trip: %q", got)
	}
}

func between(s, from, to string) string {
	_, after, _ := strings.Cut(s, from)
	v, _, _ := strings.Cut(after, to)
	return v
}

func TestSetBrandingWritesEverySite(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wrapper := filepath.Join(h.svc.Cfg.SiteRoot("s1"), brandWrapperPath)

	if _, err := h.svc.SetBranding(ctx, BrandingInput{Name: "Acme Hosting", URL: "ftp://acme.test"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-http link accepted: %v", err)
	}
	b, err := h.svc.SetBranding(ctx, BrandingInput{Name: " Acme Hosting ", URL: "https://acme.test", Logo: dataURI("image/png", pngLogo)})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "Acme Hosting" || !b.Enabled() {
		t.Fatalf("branding = %+v", b)
	}
	got, err := os.ReadFile(wrapper)
	if err != nil || !strings.Contains(string(got), "?v="+b.LogoVersion()) {
		t.Fatalf("wrapper: %v\n%s", err, got)
	}

	// Stored: a fresh service (a restart) reads the same brand.
	fresh := &Service{Cfg: h.svc.Cfg, Store: h.svc.Store, Log: h.svc.Log}
	if b2, err := fresh.Branding(ctx); err != nil || b2.Name != "Acme Hosting" || b2.LogoVersion() != b.LogoVersion() {
		t.Fatalf("after restart: %+v %v", b2, err)
	}

	// Logo absent from the input: kept.
	if b, err = h.svc.SetBranding(ctx, BrandingInput{Name: "Acme", URL: "https://acme.test"}); err != nil || len(b.Logo) == 0 {
		t.Fatalf("logo not kept: %v", err)
	}

	// The daemon serves it, cacheable for good at its version, sandboxed.
	w := httptest.NewRecorder()
	h.svc.ServeBrandLogo(w, httptest.NewRequest("GET", BrandLogoPath+"?v="+b.LogoVersion(), nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || w.Body.Len() != len(pngLogo) {
		t.Fatalf("logo: %d %v", w.Code, w.Header())
	}

	// Nothing left: no brand, no wrapper, no logo.
	empty := ""
	if _, err := h.svc.SetBranding(ctx, BrandingInput{Logo: &empty}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wrapper); !os.IsNotExist(err) {
		t.Errorf("wrapper still there without a brand: %v", err)
	}
	w = httptest.NewRecorder()
	h.svc.ServeBrandLogo(w, httptest.NewRequest("GET", BrandLogoPath, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("logo without one: %d", w.Code)
	}
}

func TestBrandingSkipsSitesBeingProvisioned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// The WordPress image only copies core into an empty docroot.
	if err := h.svc.Store.SetSiteStatus(ctx, "s1", store.StatusProvisioning); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetBranding(ctx, BrandingInput{Name: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(h.svc.Cfg.SiteRoot("s1")); len(entries) != 0 {
		t.Errorf("wrote into a docroot still being provisioned: %v", entries)
	}
}

func TestSetOptimize(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wrapper := filepath.Join(h.svc.Cfg.SiteRoot("s1"), optimizeWrapperPath)

	if _, err := h.svc.SetOptimize(ctx, "s1", OptimizeInput{Optimizations: []string{"emoji", "rm -rf"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown key: %v", err)
	}
	st, err := h.svc.SetOptimize(ctx, "s1", OptimizeInput{Optimizations: []string{OptDBCleanup, OptHead, OptEmoji, OptEmoji}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{OptEmoji, OptHead, OptDBCleanup}; !slices.Equal(st.Optimize, want) {
		t.Errorf("stored %v, want %v (catalogue order, no duplicates)", st.Optimize, want)
	}
	got, err := os.ReadFile(wrapper)
	if err != nil || !strings.Contains(string(got), "define( 'WPGENIE_OPTIMIZE', 'emoji,head' );") {
		t.Fatalf("wrapper (db_cleanup is the daemon's, not PHP's): %v\n%s", err, got)
	}

	// Only the nightly cleanup: nothing for PHP to do.
	if _, err := h.svc.SetOptimize(ctx, "s1", OptimizeInput{Optimizations: []string{OptDBCleanup}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wrapper); !os.IsNotExist(err) {
		t.Errorf("wrapper kept with no PHP tweaks: %v", err)
	}
}

func TestNewSitesGetDefaultOptimizations(t *testing.T) {
	st := newSite("a.test", "A")
	if !slices.Equal(st.Optimize, DefaultOptimizations()) || slices.Contains(st.Optimize, OptJQueryMigrate) {
		t.Errorf("new site optimizations %v", st.Optimize)
	}
}

// adminLoginExec answers the sign-in eval the way WordPress would.
func adminLoginExec(t *testing.T, siteURL string, cookieName string) func([]string, io.Reader, io.Writer) error {
	return func(args []string, _ io.Reader, out io.Writer) error {
		if !slices.Equal(args[:4], []string{"wp", "--skip-themes", "--skip-plugins", "eval"}) {
			return fmt.Errorf("unexpected command %v", args)
		}
		code := args[4]
		if strings.Contains(code, "$uid = 99;") {
			return fmt.Errorf("exit status 3: %s", noAdminMarker)
		}
		if !strings.Contains(code, "base64_decode( '"+base64.StdEncoding.EncodeToString([]byte("203.0.113.9"))+"' )") {
			t.Errorf("the requester's address isn't passed to the session: %s", code)
		}
		fmt.Fprintf(out, `{"user_id":"2","user":"boss","expires":1790000000,"site_url":%q,"admin_url":"https://a.test/blog/wp-admin/",`+
			`"domain":"","cookies":[{"name":%q,"value":"boss|1790000000|tok|mac","path":"/blog/wp-admin"},`+
			`{"name":"wordpress_logged_in_abc","value":"boss|1790000000|tok|mac2","path":"/blog/"}]}`, siteURL, cookieName)
		return nil
	}
}

func TestAdminLogin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rt.exec = adminLoginExec(t, "https://a.test/blog/", "wordpress_sec_abc")
	sess, err := h.svc.AdminLogin(ctx, "s1", 0, "203.0.113.9", "Firefox\r\nX-Evil: 1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Host != "a.test" || sess.AdminPath != "/blog/wp-admin/" || sess.User != "boss" || sess.UserID != 2 || len(sess.Cookies) != 2 {
		t.Fatalf("session = %+v", sess)
	}
	if !sess.Expires.Equal(time.Unix(1790000000, 0)) {
		t.Errorf("expires %v", sess.Expires)
	}

	// WordPress on a host that isn't one of the site's: the primary domain.
	h.rt.exec = adminLoginExec(t, "https://elsewhere.test/", "wordpress_sec_abc")
	if sess, err := h.svc.AdminLogin(ctx, "s1", 0, "203.0.113.9", ""); err != nil || sess.Host != "a.test" {
		t.Fatalf("foreign siteurl: %+v %v", sess, err)
	}

	// Not an administrator.
	if _, err := h.svc.AdminLogin(ctx, "s1", 99, "203.0.113.9", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("no such administrator: %v", err)
	}

	// A cookie name that would inject into Set-Cookie (a site can redefine
	// the constants in wp-config.php).
	h.rt.exec = adminLoginExec(t, "https://a.test/", "evil; Domain=.test")
	if _, err := h.svc.AdminLogin(ctx, "s1", 0, "203.0.113.9", ""); err == nil {
		t.Fatal("accepted an unsafe cookie name")
	}

	// Only an active site.
	h.svc.Store.SetSiteStatus(ctx, "s1", store.StatusSuspended)
	if _, err := h.svc.AdminLogin(ctx, "s1", 0, "203.0.113.9", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("suspended site: %v", err)
	}
}

// usersExec answers the panel's user listing: the first user is owner (1),
// then an administrator dev (7) and an editor eddie (9).
func usersExec(t *testing.T) func([]string, io.Reader, io.Writer) error {
	return func(args []string, _ io.Reader, out io.Writer) error {
		if len(args) != 5 || args[3] != "eval" || !strings.Contains(args[4], "get_users") {
			t.Errorf("unexpected %v", args)
			return fmt.Errorf("unexpected %v", args)
		}
		fmt.Fprint(out, `{"first":1,"users":[{"id":1,"login":"owner","email":"o@a.test","name":"Owner","role":"administrator"},`+
			`{"id":"7","login":"dev","email":"d@a.test","name":"Dev","role":"administrator"},`+
			`{"id":9,"login":"eddie","email":"e@a.test","name":"Ed","role":"editor"}]}`)
		return nil
	}
}

func TestUsers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rt.exec = usersExec(t)
	users, err := h.svc.Users(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	want := []WPUser{
		{ID: 1, Login: "owner", Email: "o@a.test", Name: "Owner", Role: RoleAdministrator, Owner: true},
		{ID: 7, Login: "dev", Email: "d@a.test", Name: "Dev", Role: RoleAdministrator},
		{ID: 9, Login: "eddie", Email: "e@a.test", Name: "Ed", Role: RoleEditor},
	}
	if !slices.Equal(users, want) {
		t.Fatalf("users = %+v", users)
	}
}

func TestResetAdminPassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rt.exec = usersExec(t)

	if _, err := h.svc.ResetAdminPassword(ctx, "s1", PasswordInput{UserID: 3}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("not an administrator or editor: %v", err)
	}
	if _, err := h.svc.ResetAdminPassword(ctx, "s1", PasswordInput{UserID: 7, Password: "short"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short password: %v", err)
	}
	*h.log = nil
	res, err := h.svc.ResetAdminPassword(ctx, "s1", PasswordInput{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if res.User != "dev" || len(res.Password) < 20 {
		t.Fatalf("reset = %+v", res)
	}
	want := []string{"wp user update 7 --prompt=user_pass --skip-email", "wp user session destroy 7 --all"}
	if !slices.Equal(*h.log, want) {
		t.Errorf("commands %v, want %v", *h.log, want)
	}
	for _, l := range *h.log {
		if strings.Contains(l, res.Password) {
			t.Error("the password went on the command line")
		}
	}
}

func TestCreateUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rt.exec = usersExec(t)
	ok := NewUserInput{Login: "eddie", Email: "e@a.test", Name: "Ed", Role: RoleEditor}

	for name, in := range map[string]NewUserInput{
		"short login":        {Login: "ab", Email: "e@a.test", Role: RoleEditor},
		"login is an option": {Login: "--yes", Email: "e@a.test", Role: RoleEditor},
		"login with space":   {Login: "a b c", Email: "e@a.test", Role: RoleEditor},
		"bad e-mail":         {Login: "edd", Email: "nope", Role: RoleEditor},
		"e-mail is option":   {Login: "edd", Email: "--x@a.test", Role: RoleEditor},
		"other role":         {Login: "edd", Email: "e@a.test", Role: "subscriber"},
		"short password":     {Login: "edd", Email: "e@a.test", Role: RoleEditor, Password: "short"},
		"control in name":    {Login: "edd", Email: "e@a.test", Role: RoleEditor, Name: "a\nb"},
	} {
		if _, err := h.svc.CreateUser(ctx, "s1", in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(*h.log) != 0 {
		t.Fatalf("ran %v for invalid input", *h.log)
	}

	res, err := h.svc.CreateUser(ctx, "s1", ok)
	if err != nil {
		t.Fatal(err)
	}
	if res.User.ID != 9 || res.User.Role != RoleEditor || len(res.Password) < 20 {
		t.Fatalf("created = %+v", res)
	}
	want := []string{"wp user create eddie e@a.test --role=editor --display_name=Ed --prompt=user_pass"}
	if !slices.Equal(*h.log, want) {
		t.Errorf("commands %v, want %v", *h.log, want)
	}
	for _, l := range *h.log {
		if strings.Contains(l, res.Password) {
			t.Error("the password went on the command line")
		}
	}

	h.svc.Store.SetSiteStatus(ctx, "s1", store.StatusSuspended)
	if _, err := h.svc.CreateUser(ctx, "s1", ok); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("suspended site: %v", err)
	}
}

func TestDeleteUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rt.exec = usersExec(t)

	// The first user, and someone who isn't an administrator or editor.
	for _, uid := range []int{1, 3} {
		if _, err := h.svc.DeleteUser(ctx, "s1", uid); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("deleting %d: %v", uid, err)
		}
	}
	if len(*h.log) != 0 {
		t.Fatalf("ran %v", *h.log)
	}

	u, err := h.svc.DeleteUser(ctx, "s1", 9)
	if err != nil || u.Login != "eddie" {
		t.Fatalf("deleted %+v %v", u, err)
	}
	if want := []string{"wp user delete 9 --reassign=1 --yes"}; !slices.Equal(*h.log, want) {
		t.Errorf("commands %v, want %v", *h.log, want)
	}

	// Never the last administrator, even when the first user isn't one.
	h.rt.exec = func(_ []string, _ io.Reader, out io.Writer) error {
		fmt.Fprint(out, `{"first":1,"users":[{"id":1,"login":"owner","role":"editor"},{"id":5,"login":"boss","role":"administrator"}]}`)
		return nil
	}
	if _, err := h.svc.DeleteUser(ctx, "s1", 5); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("last administrator: %v", err)
	}
}

func TestAnalyseFindings(t *testing.T) {
	now := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)
	st := &store.Site{ID: "s1", ShieldMode: "standard", WAF: true, AutoUpdate: AutoUpdateSecurity,
		PageCache: false, ObjectCache: true, Optimize: DefaultOptimizations(), Harden: DefaultHardening(), ImageFormats: []string{"webp"}}
	scan := &ScanReport{ScannedAt: now.Add(-time.Hour), Inventory: &Inventory{
		Core: Component{Type: "core", Slug: "wordpress", Version: "6.8.1"},
		Plugins: []Component{
			{Type: "plugin", Slug: "forms", Version: "1.0", UpdateVersion: "1.1", UpdateFixes: true,
				Vulns: []Vuln{{Title: "SQL injection", Severity: "high"}}},
			{Type: "plugin", Slug: "seo", Version: "2.0", UpdateVersion: "2.1"},
		}}}
	facts := &SiteFacts{UsersCanRegister: true, DefaultRole: "administrator", Admins: []string{"admin", "boss"}, BlogPublic: true,
		Permalinks: "/%postname%/", Revisions: 5000}
	fs := analyse(st, facts, scan, nil, now)

	byID := map[string]Finding{}
	for _, f := range fs {
		byID[f.ID] = f
	}
	for id, want := range map[string]struct{ sev, fix string }{
		"vuln:plugin:forms": {SevCritical, FixUpdateSecurity},
		"register-admin":    {SevCritical, FixDefaultRole},
		"page-cache-off":    {SevWarning, FixPageCache},
		"admin-user":        {SevWarning, ""},
		"updates":           {SevInfo, FixUpdateAll},
		"db-bloat":          {SevInfo, FixDBCleanup},
	} {
		f, ok := byID[id]
		if !ok || f.Severity != want.sev || f.Fix != want.fix {
			t.Errorf("%s = %+v (present %v), want severity %s fix %q", id, f, ok, want.sev, want.fix)
		}
	}
	for _, id := range []string{"object-cache-off", "optimize", "images-off", "shield-off", "waf-off", "no-scan", "noindex"} {
		if _, ok := byID[id]; ok {
			t.Errorf("unexpected finding %s", id)
		}
	}
	if !slices.IsSortedFunc(fs, func(a, b Finding) int { return severityRank(a.Severity) - severityRank(b.Severity) }) {
		t.Error("findings aren't worst first")
	}
	if n, g := score(fs); n != 100-2*20-2*8-2*2 || g != "D" {
		t.Errorf("score %d %s", n, g)
	}

	// A staging copy is meant to be hidden from search engines; a live
	// site isn't.
	facts.BlogPublic = false
	if !slices.ContainsFunc(analyse(st, facts, scan, nil, now), func(f Finding) bool { return f.ID == "noindex" }) {
		t.Error("live site hidden from search engines not reported")
	}
	st.ParentID = "live"
	if slices.ContainsFunc(analyse(st, facts, scan, nil, now), func(f Finding) bool { return f.ID == "noindex" }) {
		t.Error("staging site reported for being hidden from search engines")
	}

	// Nothing known but the settings.
	if fs := analyse(&store.Site{ShieldMode: "off"}, nil, nil, nil, now); len(fs) == 0 || fs[0].Severity != SevWarning {
		t.Errorf("settings-only findings: %+v", fs)
	}
}

func TestApplyFixRunsWPCLI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.ApplyFix(ctx, "s1", FixDefaultRole); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*h.log, "wp option update default_role subscriber") {
		t.Errorf("commands %v", *h.log)
	}
	if _, err := h.svc.ApplyFix(ctx, "s1", "rm -rf /"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown fix: %v", err)
	}
}
