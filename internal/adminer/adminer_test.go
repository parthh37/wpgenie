package adminer

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/parthh37/wpgenie/internal/dbprov"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
)

type fakeAccounts struct {
	mu      sync.Mutex
	dropped []string
}

func (f *fakeAccounts) CreateTempUser(context.Context, string, string, string) error { return nil }
func (f *fakeAccounts) DropTempUser(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, u)
	return nil
}
func (f *fakeAccounts) DropTempUsers(context.Context) error { return nil }
func (f *fakeAccounts) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dropped...)
}

// harness: an Adminer stand-in that echoes what it received.
func harness(t *testing.T) (*Service, *fakeAccounts, *time.Time, chan *http.Request) {
	t.Helper()
	seen := make(chan *http.Request, 10)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		io.WriteString(w, "adminer")
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	acc := &fakeAccounts{}
	s := &Service{Cfg: Config{DBHost: "wpgenie-mariadb"}, Accounts: acc, Log: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return now }, proxy: httputil.NewSingleHostReverseProxy(u), secret: "s3cret",
		pending: map[string]*session{}, sessions: map[string]*session{}}
	return s, acc, &now, seen
}

func (s *Service) addPending(token, site string) {
	s.pending[hash(token)] = &session{siteID: site, db: "wp_" + site, user: "wpga_" + site + "x1", pass: "pw-" + site,
		created: s.now(), touched: s.now()}
}

func get(s *Service, site, target string, cookies ...*http.Cookie) *http.Response {
	r := httptest.NewRequest("GET", target, nil)
	if site != "" {
		r.Header.Set(SiteHeader, site)
	}
	// A client trying to supply the credentials headers itself, including
	// spellings PHP maps to the same variable.
	r.Header.Set("X-WPGenie-DB-User", "root")
	r.Header["X_WPGenie-DB-Host"] = []string{"evil:6379"}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Result()
}

func TestTokenIsSingleUseAndBoundToItsSite(t *testing.T) {
	s, acc, _, seen := harness(t)
	s.addPending("tok1", "s1")
	if res := get(s, "s2", Path+"?wpgenie_token=tok1"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("token redeemed on another site's domain: %d", res.StatusCode)
	}
	s.addPending("tok1", "s1")
	res := get(s, "s1", Path+"?wpgenie_token=tok1")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != Path {
		t.Fatalf("redeem: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	cookie := res.Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != Path {
		t.Fatalf("cookie %+v", cookie)
	}
	if res := get(s, "s1", Path+"?wpgenie_token=tok1"); res.StatusCode != http.StatusForbidden {
		t.Fatal("a token worked twice")
	}

	res = get(s, "s1", Path+"?select=wp_posts", cookie, &http.Cookie{Name: "adminer_sid", Value: "a"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("session request: %d", res.StatusCode)
	}
	r := <-seen
	if r.Header.Get("X-WPGenie-DB-User") != "wpga_s1x1" || r.Header.Get("X-WPGenie-DB-Pass") != "pw-s1" ||
		r.Header.Get("X-WPGenie-Adminer") != "s3cret" || r.Header.Get("X-WPGenie-DB-Name") != "wp_s1" {
		t.Errorf("headers to Adminer: %v", r.Header)
	}
	if _, ok := r.Header["X_WPGenie-DB-Host"]; ok {
		t.Error("an underscore spelling of a credentials header reached Adminer")
	}
	if c := r.Header.Get("Cookie"); strings.Contains(c, cookieName) || !strings.Contains(c, "adminer_sid=a") {
		t.Errorf("cookies to Adminer: %q", c)
	}
	if r.URL.Path != Path {
		t.Errorf("path %q: Adminer builds its links from it", r.URL.Path)
	}
	if res := get(s, "s2", Path, cookie); res.StatusCode != http.StatusForbidden {
		t.Error("a session cookie worked on another site")
	}
	// The token tried on s2 was spent and its account dropped; drops run in
	// the background, so wait for it.
	for i := 0; len(acc.list()) < 1 && i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if len(acc.list()) != 1 {
		t.Errorf("dropped %v", acc.list())
	}
}

func TestSessionsExpire(t *testing.T) {
	s, acc, now, _ := harness(t)
	s.addPending("tok", "s1")
	cookie := get(s, "s1", Path+"?wpgenie_token=tok").Cookies()[0]
	*now = now.Add(10 * time.Minute)
	if res := get(s, "s1", Path, cookie); res.StatusCode != http.StatusOK {
		t.Fatal("session ended while in use")
	}
	*now = now.Add(sessionIdle + time.Second)
	if res := get(s, "s1", Path, cookie); res.StatusCode != http.StatusForbidden {
		t.Fatal("idle session still works")
	}
	s.addPending("late", "s1")
	*now = now.Add(tokenTTL + time.Second)
	if res := get(s, "s1", Path+"?wpgenie_token=late"); res.StatusCode != http.StatusForbidden {
		t.Fatal("expired token redeemed")
	}
	time.Sleep(50 * time.Millisecond) // drops run in the background
	if len(acc.list()) != 2 {
		t.Errorf("accounts dropped: %v", acc.list())
	}
}

func TestNoSessionNoProxy(t *testing.T) {
	s, _, _, seen := harness(t)
	for _, res := range []*http.Response{get(s, "s1", Path), get(s, "", Path), get(s, "s1", Path, &http.Cookie{Name: cookieName, Value: "guess"})} {
		if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusNotFound {
			t.Errorf("status %d", res.StatusCode)
		}
	}
	select {
	case <-seen:
		t.Fatal("a request without a session reached Adminer")
	default:
	}
}

func TestWithoutCookie(t *testing.T) {
	if got := withoutCookie("a=1; wpgenie_adminer=x; adminer_sid=2", "wpgenie_adminer"); got != "a=1; adminer_sid=2" {
		t.Fatal(got)
	}
}

// TestAdminerEndToEnd opens a real Adminer on a real MariaDB: token, cookie,
// temporary account, the site's tables; and the account dropped at the end.
func TestAdminerEndToEnd(t *testing.T) {
	net := os.Getenv("E2E_NET")
	if os.Getenv("WPGENIE_TEST_E2E") != "1" || net == "" {
		t.Skip("set WPGENIE_TEST_E2E=1 and E2E_NET (run in a container on that network)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	docker := &runtime.Docker{}
	const dbName, dbPass = "wpgt-adm-db", "adm-root-pw"
	docker.Run(ctx, nil, "rm", "-f", dbName, container)
	if _, err := docker.Run(ctx, nil, "run", "-d", "--name", dbName, "--network", net,
		"-e", "MARIADB_ROOT_PASSWORD="+dbPass, "mariadb:11.4"); err != nil {
		t.Fatal(err)
	}
	defer docker.Run(context.Background(), nil, "rm", "-f", dbName, container)
	db, err := dbprov.Open("root:" + dbPass + "@tcp(" + dbName + ":3306)/")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; db.CreateSiteDB(ctx, "wp_sadm", "u_sadm", "pw") != nil; i++ {
		if i > 60 {
			t.Fatal("MariaDB didn't start")
		}
		time.Sleep(time.Second)
	}
	raw, _ := sql.Open("mysql", "root:"+dbPass+"@tcp("+dbName+":3306)/")
	defer raw.Close()
	raw.ExecContext(ctx, "CREATE TABLE wp_sadm.wp_secret_marker (id int)")
	st := storetest.Open(t)
	st.CreateSite(ctx, &store.Site{ID: "sadm", Name: "x", PrimaryDomain: "adm.test", PHPVersion: "8.3", FPMPort: 19000,
		DBName: "wp_sadm", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1})
	s := &Service{Store: st, Docker: docker, Accounts: db, Log: slog.New(slog.DiscardHandler),
		Cfg: Config{Image: "wpgenie/adminer:test", ImageDir: "../../images/adminer", Port: 18090, Network: net,
			DBHost: dbName, Upstream: container + ":8080"}}

	link, _, err := s.Open(ctx, "sadm", "tester")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	if u.Host != "adm.test" || u.Path != Path {
		t.Fatalf("link %s: must be on the site's own domain", link)
	}
	res := get(s, "sadm", u.RequestURI())
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("redeem %d", res.StatusCode)
	}
	cookie := res.Cookies()[0]
	jar := []*http.Cookie{cookie}
	// Adminer logs in with the temporary account and redirects; follow like a browser.
	target := Path
	var body string
	for range 5 {
		r := get(s, "sadm", target, jar...)
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		jar = append(jar, r.Cookies()...)
		loc := r.Header.Get("Location")
		if loc == "" {
			break
		}
		target = Path + strings.TrimPrefix(loc, Path)
	}
	if !strings.Contains(body, "wp_secret_marker") {
		t.Fatalf("the site's tables aren't shown:\n%.2000s", body)
	}
	var n int
	raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'wpga\\_%'").Scan(&n)
	if n != 1 {
		t.Fatalf("%d temporary accounts, want 1", n)
	}
	s.SiteRemoved(ctx, "sadm")
	raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'wpga\\_%'").Scan(&n)
	if n != 0 {
		t.Fatal("the temporary account outlived its session")
	}
}
