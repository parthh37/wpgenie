package site

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// hookedEnd is a move's source that changes the site between the two
// passes (a post published, a file uploaded, as visitors and editors
// would), and whose last step leaves its containers alone: both "servers"
// of this test share one Docker, where removing the source's replicas by
// site ID would take the target's with them.
type hookedEnd struct {
	localEnd
	afterFirstDB func()
	dbCopies     int
	moved        bool
}

func (h *hookedEnd) exportDB(ctx context.Context, id string, w io.Writer) error {
	if err := h.localEnd.exportDB(ctx, id, w); err != nil {
		return err
	}
	h.dbCopies++
	if h.dbCopies == 1 && h.afterFirstDB != nil {
		h.afterFirstDB()
	}
	return nil
}

func (h *hookedEnd) movedTo(context.Context, string, cluster.Endpoint) error {
	h.moved = true
	return nil
}

// TestMigrationEndToEnd moves a real WordPress site between two servers:
// everything, including what changed during the first copy, arrives, and
// the target serves it while the source shows its maintenance page.
func TestMigrationEndToEnd(t *testing.T) {
	a := newE2ENamed(t, "mig-a", 29000)
	b := newE2ENamed(t, "mig-b", 29600)
	ctx := a.ctx

	st, jobID, err := a.svc.StartCreate(jobs.WithOwner(ctx, "user:1"), CreateInput{Domain: "move.test", AdminEmail: "a@move.test"})
	a.wait(jobID, err)
	id := st.ID
	a.wp(id, "post", "create", "--post_title=Before", "--post_status=publish")
	a.sh(id, "mkdir -p wp-content/uploads/2026 && echo one > wp-content/uploads/2026/one.jpg")
	a.wp(id, "option", "update", "blogdescription", "moved with care")

	src := &hookedEnd{localEnd: localEnd{a.svc}}
	src.afterFirstDB = func() {
		a.wp(id, "post", "create", "--post_title=During", "--post_status=publish")
		a.sh(id, "echo two > wp-content/uploads/2026/two.jpg")
	}
	meta, err := a.svc.ExportMeta(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	task := &moveTask{}
	if err := a.svc.migrate(ctx, task, id, meta, src, localEnd{b.svc}, cluster.Endpoint{ID: "b", Address: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if !src.moved || src.dbCopies != 2 {
		t.Fatalf("moved %v after %d database copies", src.moved, src.dbCopies)
	}

	// The target serves the site, with the same ID, prefix and content.
	got, err := b.st.GetSite(ctx, id)
	if err != nil || got.Status != store.StatusActive || got.PrimaryDomain != "move.test" {
		t.Fatalf("on the target: %+v %v", got, err)
	}
	titles := b.wp(id, "post", "list", "--post_type=post", "--field=post_title")
	for _, want := range []string{"Before", "During"} {
		if !strings.Contains(titles, want) {
			t.Errorf("post %q missing on the target (%q)", want, titles)
		}
	}
	if d := b.wp(id, "option", "get", "blogdescription"); d != "moved with care" {
		t.Errorf("option: %q", d)
	}
	if files := b.sh(id, "cat wp-content/uploads/2026/one.jpg wp-content/uploads/2026/two.jpg"); files != "one\ntwo" {
		t.Errorf("uploads on the target: %q", files)
	}
	pa, _ := a.svc.tablePrefix(id)
	pb, _ := b.svc.tablePrefix(id)
	if pa != pb {
		t.Errorf("table prefix %q became %q", pa, pb)
	}
	// Its own database account on the target, not the source's password.
	if cfgA, cfgB := a.sh(id, "cat ../wp-config.php | grep DB_PASSWORD"), b.sh(id, "cat ../wp-config.php | grep DB_PASSWORD"); cfgA == cfgB {
		t.Error("the target reuses the source's database password")
	}
	// The maintenance page stayed on the source and never reached the target.
	a.sh(id, "test -f .maintenance")
	if out := b.sh(id, "test -f .maintenance && echo yes || echo no"); out != "no" {
		t.Error("the target copied the maintenance page")
	}
}

// TestCacheIsolationEndToEnd: each WordPress caches through its own Valkey
// user, and PHP on one site can neither read nor overwrite another's
// cached options (the way to make yourself administrator elsewhere).
func TestCacheIsolationEndToEnd(t *testing.T) {
	e := newE2E(t)
	ctx := e.ctx
	mk := func(domain string) string {
		st, jobID, err := e.svc.StartCreate(jobs.WithOwner(ctx, "user:1"), CreateInput{Domain: domain, AdminEmail: "a@" + domain})
		e.wait(jobID, err)
		if _, err := e.svc.SetCache(ctx, st.ID, CacheSettings{ObjectCache: true, PageCache: true}); err != nil {
			t.Fatal(err)
		}
		return st.ID
	}
	a, b := mk("cache-a.test"), mk("cache-b.test")
	// B's options land in the cache under B's user.
	e.wp(b, "option", "get", "blogname")
	if got := e.wp(a, "eval", `wp_cache_set("probe", "a-value"); echo wp_cache_get("probe");`); got != "a-value" {
		t.Fatalf("A's own cache: %q", got)
	}
	status := e.wp(a, "eval", `global $wp_object_cache; echo $wp_object_cache->redis_status() ? "connected" : "down";`)
	if status != "connected" {
		t.Fatalf("A isn't connected to the cache with its user: %q", status)
	}
	// Straight at the server, past the drop-in's API: refused.
	probe := `global $wp_object_cache; $r = $wp_object_cache->redis_instance();
		try { var_export($r->get("` + b + `:options:alloptions")); } catch (Throwable $e) { echo "refused"; }
		echo "|";
		try { var_export($r->set("` + b + `:options:alloptions", "pwned")); } catch (Throwable $e) { echo "refused"; }`
	if got := e.wp(a, "eval", probe); got != "refused|refused" {
		t.Fatalf("site A reaching site B's cache: %q", got)
	}
}
