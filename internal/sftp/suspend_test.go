package sftp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/store"
)

// recDocker records docker commands; the SFTP container "runs".
type recDocker struct{ cmds []string }

func (d *recDocker) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	d.cmds = append(d.cmds, strings.Join(args, " "))
	return nil, nil
}

func (d *recDocker) EnsureBuilt(context.Context, string, string) (string, error) {
	return "sha256:img", nil
}

// Logins of a suspended site are left out of the server's accounts (and
// their sessions ended) until the site is back; the records stay.
func TestSuspendedSitesLoginsAreLeftOut(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for i, id := range []string{"sabc0001", "sabc0002"} {
		if err := st.CreateSite(ctx, &store.Site{ID: id, Name: id, PrimaryDomain: id + ".test", PHPVersion: "8.3",
			FPMPort: 19000 + i, DBName: "wp_" + id, Status: store.StatusActive, ShieldMode: "standard"}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSFTPUser(ctx, &store.SFTPUser{Username: id, SiteID: id, Password: "$6$s$h"}); err != nil {
			t.Fatal(err)
		}
	}
	d := &recDocker{}
	s := &Service{Store: st, Docker: d, Log: slog.New(slog.DiscardHandler),
		Cfg: Config{DataDir: t.TempDir(), SitesDir: "/var/lib/wpgenie/sites", Image: "wpgenie/sftp"}}
	passwd := func() string {
		b, _ := os.ReadFile(filepath.Join(s.configDir(), "passwd"))
		return string(b)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if p := passwd(); !strings.Contains(p, "sabc0001:") || !strings.Contains(p, "sabc0002:") {
		t.Fatalf("passwd:\n%s", p)
	}
	st.SetSiteStatus(ctx, "sabc0002", store.StatusSuspended)
	d.cmds = nil
	s.SiteRemoved(ctx, "sabc0002") // what suspending a site calls
	if p := passwd(); !strings.Contains(p, "sabc0001:") || strings.Contains(p, "sabc0002") {
		t.Fatalf("suspended login still rendered:\n%s", p)
	}
	if users, _ := st.SFTPUsers(ctx, "sabc0002"); len(users) != 1 {
		t.Fatal("the login's record went")
	}
	st.SetSiteStatus(ctx, "sabc0002", store.StatusActive)
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if p := passwd(); !strings.Contains(p, "sabc0002:") {
		t.Fatalf("login not back:\n%s", p)
	}
}
