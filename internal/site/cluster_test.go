package site

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

func TestSpreadCounts(t *testing.T) {
	for _, c := range []struct {
		total int
		nodes []string
		want  []int
	}{
		{1, nil, []int{1}},
		{1, []string{"b"}, []int{1, 0}}, // one replica stays home
		{2, []string{"b"}, []int{1, 1}},
		{3, []string{"b"}, []int{2, 1}},
		{5, []string{"b", "c"}, []int{2, 2, 1}},
	} {
		if got := spreadCounts(c.total, c.nodes); !slices.Equal(got, c.want) {
			t.Errorf("spreadCounts(%d, %v) = %v, want %v", c.total, c.nodes, got, c.want)
		}
	}
	if n := localReplicas(&store.Site{Replicas: 3, SpreadNodes: []string{"b"}}); n != 2 {
		t.Errorf("localReplicas = %d", n)
	}
}

func TestGuestConfig(t *testing.T) {
	cfg, err := renderWPConfig(wpConfigData{SiteID: "sabc1234", DBName: "wp_sabc1234", DBUser: "u", DBPassword: "p",
		DBHost: "wpgenie-mariadb", RedisHost: "wpgenie-redis", TablePrefix: "wp_x_"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := guestConfig(string(cfg), "web-1")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"define( 'DB_HOST',     'wpg-link-web-1:3306' );", "define( 'WP_REDIS_HOST', 'wpg-link-web-1' );",
		"define( 'DB_PASSWORD', 'p' );"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(s, "wpgenie-mariadb") || strings.Contains(s, "wpgenie-redis") {
		t.Error("local hosts left in a guest's config")
	}
	// Salts are the home's: logins work on every replica.
	if strings.Count(s, "_KEY'") < 4 || !strings.Contains(string(cfg), s[strings.Index(s, "AUTH_KEY"):strings.Index(s, "AUTH_KEY")+60]) {
		t.Error("salts changed")
	}
	if _, err := guestConfig("<?php // hand-written", "web-1"); err == nil {
		t.Error("a config without WPGenie's lines was accepted")
	}
}

func TestTunnelTargets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.svc
	// s1 lives here and is spread to n2; this node runs a replica of n3's
	// site sx on 19500; n4 is a moved site's old server.
	if err := s.Store.SetSpreadNodes(ctx, "s1", []string{"n2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddGuestReplica(ctx, store.GuestReplica{Port: 19500, SiteID: "sx", HomeNode: "n3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowIngress(ctx, "s1", "n4", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowIngress(ctx, "sold0001", "n5", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowIngress(ctx, "sold0001", "n5", -time.Hour); err != nil { // ended
		t.Fatal(err)
	}
	// Valkey is found on the Docker network (never published on the host).
	s.ContainerIP = func(_ context.Context, name string) (string, error) {
		if name != s.Cfg.RedisHost {
			return "", errors.New("unexpected container " + name)
		}
		return "172.18.0.3", nil
	}
	control := cluster.Peer{Node: cluster.LocalNode, Control: true}
	for _, c := range []struct {
		peer   cluster.Peer
		target string
		want   string
	}{
		{cluster.Peer{Node: "n2"}, "mariadb", s.Cfg.MariaDBLoopback()},
		{cluster.Peer{Node: "n2"}, "valkey", "172.18.0.3:6379"},
		{cluster.Peer{Node: "n3"}, "mariadb", ""}, // not spread here
		{cluster.Peer{Node: "n3"}, "fpm:19500", "127.0.0.1:19500"},
		{cluster.Peer{Node: "n2"}, "fpm:19500", ""}, // not n2's replica
		{cluster.Peer{Node: "n3"}, "fpm:19000", ""}, // a local site's replica
		{cluster.Peer{Node: "n3"}, "fpm:x", ""},     // garbage
		{cluster.Peer{Node: "n4"}, "ingress", s.Cfg.IngressListen()},
		{cluster.Peer{Node: "n4"}, "http", "127.0.0.1:80"},
		{cluster.Peer{Node: "n5"}, "ingress", ""},
		{cluster.Peer{Node: "n2"}, "https", ""},
		{control, "https", "127.0.0.1:443"},
		{control, "mariadb", ""}, // the panel runs the database through the site API, not a tunnel
		{cluster.Peer{Node: "n2"}, "ssh", ""},
	} {
		addr, ok := s.TunnelTarget(c.peer, c.target)
		if addr != c.want || ok != (c.want != "") {
			t.Errorf("%s -> %s: %q %v, want %q", c.peer.Node, c.target, addr, ok, c.want)
		}
	}
}

func TestGuestNeedsGrant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.svc
	if err := s.guestAllowed(ctx, "n1", "sabc1234"); !errors.Is(err, errForbidden) {
		t.Fatalf("no grant: %v", err)
	}
	if err := s.SetSpreadGrant(ctx, "sabc1234", "n1"); err != nil {
		t.Fatal(err)
	}
	if err := s.guestAllowed(ctx, "n1", "sabc1234"); err != nil {
		t.Fatalf("granted: %v", err)
	}
	if err := s.guestAllowed(ctx, "n2", "sabc1234"); !errors.Is(err, errForbidden) {
		t.Fatalf("another home: %v", err)
	}
	// A grant for a site that lives here never lets a peer write over it.
	if err := s.Store.CreateSite(ctx, &store.Site{ID: "slocal01", Name: "x", PrimaryDomain: "x.test", PHPVersion: "8.3",
		FPMPort: 19010, DBName: "wp_slocal01", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSpreadGrant(ctx, "slocal01", "n1"); err != nil {
		t.Fatal(err)
	}
	if err := s.guestAllowed(ctx, "n1", "slocal01"); !errors.Is(err, errForbidden) {
		t.Fatalf("guest over a local site: %v", err)
	}
	if err := s.SetSpreadGrant(ctx, "../etc", "n1"); err == nil {
		t.Fatal("path-like site ID granted")
	}
}

func TestInstallFingerprint(t *testing.T) {
	root := t.TempDir()
	write := func(p, data string) {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.php", "<?php")
	write("wp-content/plugins/a/a.php", "<?php")
	write("wp-content/uploads/2026/x.jpg", "jpg")
	write("wp-content/cache/wpgenie/index.html", "page")
	fp := func(uploads bool) string {
		v, err := installFingerprint(root, uploads)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a, au := fp(false), fp(true)
	write("wp-content/cache/wpgenie/other/index.html", "page") // caches don't count
	write("wp-content/uploads/2026/y.jpg", "jpg")              // uploads only with uploads
	if fp(false) != a {
		t.Error("cache or uploads changed the code fingerprint")
	}
	if fp(true) == au {
		t.Error("a new upload didn't change the fingerprint with uploads")
	}
	write("wp-content/plugins/a/a.php", "<?php // updated")
	if fp(false) == a {
		t.Error("a plugin change went unnoticed")
	}
}

func TestSMTPCredsRoundTrip(t *testing.T) {
	b, err := smtpCreds("mail.example.com", "sabc1234@mail.example.com", "S3cretPass-word")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := parseSMTPCreds(b)
	if !ok || c.Host != "mail.example.com" || c.Address != "sabc1234@mail.example.com" || c.Password != "S3cretPass-word" {
		t.Fatalf("round trip: %+v %v", c, ok)
	}
	if _, ok := parseSMTPCreds([]byte("<?php")); ok {
		t.Fatal("empty creds parsed")
	}
}

// fakeEnd is one side of a move, recording what was asked of it.
type fakeEnd struct {
	id    string
	calls *[]string
	fail  string // the call that fails
}

func (f fakeEnd) rec(call string) error {
	*f.calls = append(*f.calls, f.id+" "+call)
	if call == f.fail {
		return errors.New(call + " failed")
	}
	return nil
}
func (f fakeEnd) name() string { return f.id }
func (f fakeEnd) export(context.Context, string) (*MigrationMeta, error) {
	return &MigrationMeta{Site: &store.Site{ID: "smove001", PrimaryDomain: "m.test"}}, f.rec("export")
}
func (f fakeEnd) exportFiles(_ context.Context, _ string, since int64, w io.Writer) error {
	w.Write([]byte("tar"))
	if since > 0 {
		return f.rec("files-changed")
	}
	return f.rec("files")
}
func (f fakeEnd) exportDB(_ context.Context, _ string, w io.Writer) error {
	w.Write([]byte("sql"))
	return f.rec("db")
}
func (f fakeEnd) maintenance(_ context.Context, _ string, on bool) error {
	if on {
		return f.rec("maintenance-on")
	}
	return f.rec("maintenance-off")
}
func (f fakeEnd) movedTo(context.Context, string, cluster.Endpoint) error { return f.rec("moved") }
func (f fakeEnd) importSite(context.Context, *MigrationMeta) error        { return f.rec("import") }
func (f fakeEnd) importFiles(_ context.Context, _ string, overlay bool, r io.Reader) error {
	io.Copy(io.Discard, r)
	if overlay {
		return f.rec("import-files-overlay")
	}
	return f.rec("import-files")
}
func (f fakeEnd) importDB(_ context.Context, _ string, r io.Reader) error {
	io.Copy(io.Discard, r)
	return f.rec("import-db")
}
func (f fakeEnd) finish(context.Context, *MigrationMeta, string) (*store.Site, error) {
	return &store.Site{ID: "smove001"}, f.rec("finish")
}
func (f fakeEnd) abort(context.Context, string) error { return f.rec("abort") }
func (f fakeEnd) manifest(_ context.Context, _ string, w io.Writer) error {
	w.Write([]byte("./index.php\n"))
	return f.rec("manifest")
}
func (f fakeEnd) prune(_ context.Context, _ string, r io.Reader) error {
	io.Copy(io.Discard, r)
	return f.rec("prune")
}
func (f fakeEnd) remove(context.Context, string) error     { return f.rec("remove") }
func (f fakeEnd) reactivate(context.Context, string) error { return f.rec("reactivate") }

type moveTask struct{ result any }

func (*moveTask) Progress(int, string) {}
func (t *moveTask) SetResult(v any)    { t.result = v }

func TestMigrateOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	calls := &[]string{}
	src, dst := fakeEnd{id: "n1", calls: calls}, fakeEnd{id: cluster.LocalNode, calls: calls}
	meta, _ := src.export(ctx, "smove001")
	*calls = nil
	if err := h.svc.migrate(ctx, &moveTask{}, "smove001", meta, src, dst, cluster.Endpoint{ID: "local"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"local import", "n1 files", "local import-files", "n1 db", "local import-db",
		"n1 maintenance-on", "n1 export", "n1 db", "local import-db", "n1 files-changed", "local import-files-overlay",
		"n1 manifest", "local prune", "local finish", "n1 moved"}
	if !slices.Equal(*calls, want) {
		t.Fatalf("calls:\n got %v\nwant %v", *calls, want)
	}
	if m, err := h.svc.LastMove(ctx, "smove001"); err != nil || m == nil || m.From != "n1" || m.To != cluster.LocalNode {
		t.Fatalf("move not recorded: %+v %v", m, err)
	}
}

func TestMigrateFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, c := range []struct {
		fail     string
		failOn   string // which end
		rollback []string
	}{
		// Before maintenance: the half-imported copy goes.
		{"import-files", "dst", []string{"local abort"}},
		// During the final copy: also out of maintenance.
		{"files-changed", "src", []string{"local abort", "n1 maintenance-off"}},
		{"prune", "dst", []string{"local abort", "n1 maintenance-off"}},
		// It went live on the target: that copy is removed.
		{"finish", "dst", []string{"local remove", "n1 maintenance-off"}},
		// The old copy couldn't retire: it serves again, the registry and
		// the new copy go back.
		{"moved", "src", []string{"n1 reactivate", "local remove"}},
	} {
		calls := &[]string{}
		src, dst := fakeEnd{id: "n1", calls: calls}, fakeEnd{id: cluster.LocalNode, calls: calls}
		if c.failOn == "src" {
			src.fail = c.fail
		} else {
			dst.fail = c.fail
		}
		meta := &MigrationMeta{Site: &store.Site{ID: "smove001"}}
		err := h.svc.migrate(ctx, &moveTask{}, "smove001", meta, src, dst, cluster.Endpoint{ID: "local"})
		if err == nil {
			t.Fatalf("%s: no error", c.fail)
		}
		got := (*calls)[len(*calls)-len(c.rollback):]
		if !slices.Equal(got, c.rollback) {
			t.Errorf("%s: ended with %v, want %v (all: %v)", c.fail, got, c.rollback, *calls)
		}
	}
}

func TestExportRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.svc
	s.Store.CreateSite(ctx, &store.Site{ID: "sstg0001", Name: "stg", PrimaryDomain: "stg.a.test", PHPVersion: "8.3",
		FPMPort: 19020, DBName: "wp_sstg0001", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512,
		CPUs: 1, Replicas: 1, ParentID: "s1"})
	if _, err := s.ExportMeta(ctx, "s1"); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("a site with a staging copy: %v", err)
	}
	if _, err := s.ExportMeta(ctx, "sstg0001"); err == nil {
		t.Fatal("a staging site exported")
	}
	s.Store.DeleteSite(ctx, "sstg0001")
	s.Store.SetSpreadNodes(ctx, "s1", []string{"n2"})
	if _, err := s.ExportMeta(ctx, "s1"); err == nil || !strings.Contains(err.Error(), "spread") {
		t.Fatalf("a spread site: %v", err)
	}
}

var _ = jobs.Spec{}

// Spreading needs offloaded uploads: otherwise every upload would mean
// copying the media library to every other server.
func TestSpreadNeedsOffload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.svc
	if err := s.SetPeers(ctx, cluster.Peers{Nodes: []cluster.Endpoint{{ID: "n2", Address: "10.0.0.2:7443"}}}); err != nil {
		t.Fatal(err)
	}
	s.ClusterClient = func() *cluster.Client { return &cluster.Client{} }
	_, err := s.SetSpread(ctx, "s1", SpreadInput{Nodes: []string{"n2"}})
	if err == nil || !strings.Contains(err.Error(), "offload") {
		t.Fatalf("spread without offload: %v", err)
	}
	if _, err := s.SetSpread(ctx, "s1", SpreadInput{Nodes: []string{"n9"}}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("unknown node: %v", err)
	}
}

// Only a site that moved here gets the loopback copy believing PROXY
// headers, and only while its old server may forward.
func TestIngressIsPerSite(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.svc
	s.Store.CreateSite(ctx, &store.Site{ID: "sother01", Name: "o", PrimaryDomain: "o.test", PHPVersion: "8.3",
		FPMPort: 19030, DBName: "wp_sother01", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1})
	if err := s.AllowIngress(ctx, "s1", "n4", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range h.proxy.last {
		if p.Forwarded != (p.ID == "s1") {
			t.Errorf("site %s forwarded=%v", p.ID, p.Forwarded)
		}
	}
}

// A guest has no record of the spread site: pushing its files must not
// need one (it did, and every push failed).
func TestGuestFilesWithoutARecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var ran []string
	h.rt.exec = func(args []string, stdin io.Reader, stdout io.Writer) error {
		ran = append(ran, strings.Join(args, " "))
		if stdin != nil {
			io.Copy(io.Discard, stdin)
		}
		return nil
	}
	if err := h.svc.guestFiles(ctx, "sguest01", true, strings.NewReader("tar")); err != nil {
		t.Fatalf("push to a guest: %v", err)
	}
	if len(ran) != 2 || !strings.Contains(ran[0], "tar -xf") || !strings.Contains(ran[1], "uploads cache") {
		t.Fatalf("ran %q", ran)
	}
}

// TestGeneratedScript runs what the home does with a guest's generated
// files in the PHP image's shell (busybox): new files land, the home's
// own files, symlinks, PHP, dotfiles and its page cache stay untouched.
func TestGeneratedScript(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1")
	}
	setup := `set -e
mkdir -p /s/wp-content/uploads/2026 /s/wp-content/cache/min
echo home > /s/wp-content/uploads/2026/a.jpg
echo secret > /secret; ln -s /secret /s/wp-content/cache/min/evil.css
mkdir -p /g/wp-content/uploads/2026/new /g/wp-content/cache/min /g/wp-content/cache/wpgenie
echo guest > /g/wp-content/uploads/2026/a.jpg
echo "b c" > "/g/wp-content/uploads/2026/new/b c.jpg"
echo css > /g/wp-content/cache/min/x.css
echo pwn > /g/wp-content/cache/min/evil.css
echo '<?php' > /g/wp-content/cache/min/y.php
echo pc > /g/wp-content/cache/wpgenie/page
echo dot > /g/wp-content/uploads/.htaccess
(cd /g && tar -cf - wp-content) | sh -c "$SCRIPT" sh /s
cat /s/wp-content/uploads/2026/a.jpg "/s/wp-content/uploads/2026/new/b c.jpg" /s/wp-content/cache/min/x.css /secret
ls /s/wp-content/cache/min; ls -A /s/wp-content/uploads /s/wp-content/cache /s`
	out, err := (&runtime.Docker{}).Run(context.Background(), nil, "run", "--rm", "-e", "SCRIPT="+generatedScript,
		"alpine:3", "sh", "-c", setup)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "home\nb c\ncss\nsecret\nevil.css\nx.css\n" +
		"/s:\nwp-content\n\n/s/wp-content/cache:\nmin\n\n/s/wp-content/uploads:\n2026"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
