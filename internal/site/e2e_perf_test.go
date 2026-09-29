package site

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// fcgiGet sends one GET to PHP-FPM, as Caddy's php_fastcgi does, and
// returns the response headers and body.
func fcgiGet(addr string, params map[string]string) (textproto.MIMEHeader, string, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(60 * time.Second))
	record := func(typ byte, body []byte) {
		h := []byte{1, typ, 0, 1, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(h[4:], uint16(len(body)))
		c.Write(append(h, body...))
	}
	record(1, []byte{0, 1, 0, 0, 0, 0, 0, 0}) // BEGIN_REQUEST, responder
	var p bytes.Buffer
	for k, v := range params {
		for _, n := range []int{len(k), len(v)} {
			if n < 128 {
				p.WriteByte(byte(n))
			} else {
				binary.Write(&p, binary.BigEndian, uint32(n)|1<<31)
			}
		}
		p.WriteString(k + v)
	}
	record(4, p.Bytes())
	record(4, nil)
	record(5, nil) // empty stdin
	var out, stderr bytes.Buffer
	r := bufio.NewReader(c)
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, "", err
		}
		body := make([]byte, int(binary.BigEndian.Uint16(h[4:]))+int(h[6]))
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, "", err
		}
		body = body[:binary.BigEndian.Uint16(h[4:])]
		switch h[1] {
		case 6:
			out.Write(body)
		case 7:
			stderr.Write(body)
		case 3:
			tp := textproto.NewReader(bufio.NewReader(&out))
			hdr, err := tp.ReadMIMEHeader()
			if err != nil {
				return nil, "", fmt.Errorf("%w (stderr: %s)", err, stderr.String())
			}
			rest, _ := io.ReadAll(tp.R)
			return hdr, string(rest), nil
		}
	}
}

// TestPerformanceEndToEnd runs Phase 3 against real WordPress and PHP-FPM:
// the page cache (compressed copies, device copies, purges from WP-CLI and
// the admin bar), image conversion (new uploads by cron, existing files by
// a job, copies deleted with their image), PHP errors in the site's own
// log, pull-zone CDN links, and PHP-FPM load sampling. Needs the
// wpgenie/php:8.3 image (see newE2E).
func TestPerformanceEndToEnd(t *testing.T) {
	e := newE2E(t)
	ctx, svc := e.ctx, e.svc
	st, jobID, err := svc.StartCreate(ctx, CreateInput{Domain: "perf.test", AdminEmail: "a@perf.test"})
	e.wait(jobID, err)
	id, docroot := st.ID, svc.Cfg.SiteRoot(st.ID)
	st = mustSite(t, e.st, id)
	fpm := runtime.ContainerName(id, st.Upstreams[0]) + ":9000"
	const iphone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Mobile/15E148 Safari/604.1"
	const mac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) Safari/605.1.15"
	get := func(uri, ua string) (textproto.MIMEHeader, string) {
		t.Helper()
		path, query, _ := strings.Cut(uri, "?")
		hdr, body, err := fcgiGet(fpm, map[string]string{
			"GATEWAY_INTERFACE": "CGI/1.1", "SERVER_PROTOCOL": "HTTP/1.1", "REQUEST_METHOD": "GET",
			"SCRIPT_FILENAME": docroot + "/index.php", "SCRIPT_NAME": "/index.php", "DOCUMENT_ROOT": docroot,
			"REQUEST_URI": uri, "DOCUMENT_URI": path, "QUERY_STRING": query, "HTTP_HOST": "perf.test", "SERVER_NAME": "perf.test",
			"SERVER_PORT": "443", "HTTPS": "on", "REMOTE_ADDR": "203.0.113.9", "HTTP_USER_AGENT": ua,
		})
		if err != nil {
			t.Fatalf("GET %s: %v", uri, err)
		}
		return hdr, body
	}
	exists := func(rel string) bool {
		var out bytes.Buffer
		err := e.docker.Exec(ctx, id, nil, &out, "sh", "-c", `test -e "$1" && echo yes || true`, "sh", docroot+"/"+rel)
		return err == nil && strings.TrimSpace(out.String()) == "yes"
	}
	const cache = "wp-content/cache/wpgenie/"
	// A page rendered in the same second as a purge isn't stored (it may
	// predate the change): let the clock move on after purging.
	settle := func() { time.Sleep(1100 * time.Millisecond) }

	// Page cache: stored with compressed copies Caddy serves as they are.
	if hdr, body := get("/", mac); hdr.Get("X-Wpgenie-Cache") != "MISS" || !strings.Contains(body, "</html>") {
		t.Fatalf("first render: %v\n%.300s", hdr, body)
	}
	for _, f := range []string{"index.html", "index.html.gz", "index.html.br"} {
		if !exists(cache + f) {
			t.Fatalf("%s not stored", f)
		}
	}
	if out := e.sh(id, `php -r '$h = file_get_contents("`+cache+`index.html"); echo (gzdecode(file_get_contents("`+cache+
		`index.html.gz")) === $h && brotli_uncompress(file_get_contents("`+cache+`index.html.br")) === $h) ? "same" : "differ";'`); out != "same" {
		t.Fatalf("compressed copies %s from the page", out)
	}

	// WP-CLI runs with --skip-plugins, but mu-plugins still load: content
	// changed from the command line purges the cache.
	e.wp(id, "post", "create", "--post_title=From the CLI", "--post_status=publish")
	if exists(cache+"index.html") || !exists("wp-content/cache/wpgenie.purged") {
		t.Fatal("a post published with WP-CLI didn't purge the page cache")
	}

	// A theme that asks wp_is_mobile() gets separate copies per device.
	settle()
	e.sh(id, `mkdir -p wp-content/mu-plugins && printf '%s' '<?php add_action("wp_footer", function () { echo wp_is_mobile() ? "<!-- phone -->" : "<!-- computer -->"; });' > wp-content/mu-plugins/e2e-device.php`)
	get("/", iphone)
	get("/", mac)
	if exists(cache+"index.html") || !exists(cache+"index-mobile.html") || !exists(cache+"index-desktop.html") {
		t.Fatal("device-dependent page not stored per device")
	}
	if m := e.sh(id, "cat "+cache+"index-mobile.html"); !strings.Contains(m, "<!-- phone -->") {
		t.Fatal("the phone copy holds the computer's page")
	}

	// The admin bar's purge button, as an editor.
	href := e.wp(id, "eval", `wp_set_current_user(1); require_once ABSPATH . WPINC . "/class-wp-admin-bar.php";
		$b = new WP_Admin_Bar(); do_action("admin_bar_menu", $b); $n = $b->get_node("wpgenie-purge"); echo $n ? $n->href : "none";`)
	if !strings.Contains(href, "action=wpgenie_purge") || !strings.Contains(href, "_wpnonce=") {
		t.Fatalf("admin bar node: %q", href)
	}
	e.wp(id, "eval", `wp_set_current_user(1); $_REQUEST["_wpnonce"] = wp_create_nonce("wpgenie_purge"); do_action("admin_post_wpgenie_purge");`)
	if exists(cache + "index-mobile.html") {
		t.Fatal("the admin bar's purge left cached pages")
	}
	e.sh(id, "rm wp-content/mu-plugins/e2e-device.php")

	// PHP errors land in the site's own log and are grouped.
	e.sh(id, `printf '%s' '<?php if ( isset( $_GET["oops"] ) ) { trigger_error( "e2e oops", E_USER_WARNING ); }' > wp-content/mu-plugins/e2e-oops.php`)
	get("/?oops=1", mac)
	get("/?oops=2", mac)
	svc.ingestPHPErrors(ctx, id)
	errs, _ := e.st.PHPErrors(ctx, id, time.Now().Add(-time.Hour), 10)
	if !slices.ContainsFunc(errs, func(x store.PHPError) bool {
		return x.Message == "e2e oops" && x.Level == "warning" && x.Source == "mu-plugin" && x.Count == 2
	}) {
		t.Fatalf("PHP errors: %+v", errs)
	}

	// Images: existing files by a job, new uploads by cron.
	e.sh(id, `php -r '$i = imagecreatetruecolor(1200, 800); for ($n = 0; $n < 4000; $n++) imagefilledellipse($i, mt_rand(0, 1199), mt_rand(0, 799), 30, 30, imagecolorallocate($i, mt_rand(0, 255), mt_rand(0, 255), mt_rand(0, 255))); imagejpeg($i, "/tmp/photo.jpg", 90);'`)
	e.sh(id, "mkdir -p wp-content/uploads/2026/09 && cp /tmp/photo.jpg wp-content/uploads/2026/09/sftp.jpg")
	_, jobID, err = svc.SetImages(ctx, id, ImagesInput{Formats: []string{"avif", "webp"}})
	j := e.wait(jobID, err)
	if !strings.Contains(j.Result, `"converted":2`) || !exists("wp-content/uploads/2026/09/sftp.jpg.avif") || !exists("wp-content/uploads/2026/09/sftp.jpg.webp") {
		t.Fatalf("conversion job: %s", j.Result)
	}
	attachment := e.wp(id, "media", "import", "/tmp/photo.jpg", "--porcelain")
	file := e.wp(id, "post", "meta", "get", attachment, "_wp_attached_file")
	spec, err := svc.specFor(ctx, mustSite(t, e.st, id))
	if err != nil {
		t.Fatal(err)
	}
	cronOut, err := e.docker.RunCron(ctx, spec)
	if err != nil {
		t.Fatalf("cron: %v\n%s", err, cronOut)
	}
	sizes := e.sh(id, "ls wp-content/uploads/"+file[:strings.LastIndex(file, "/")])
	if !strings.Contains(sizes, strings.TrimPrefix(file[strings.LastIndex(file, "/"):], "/")+".webp") || !strings.Contains(sizes, "-300x200.jpg.avif") {
		t.Fatalf("new upload not converted by cron:\n%s\ncron: %s\nevents: %s", sizes, cronOut,
			e.wp(id, "cron", "event", "list", "--fields=hook,next_run_relative,args"))
	}
	e.wp(id, "post", "delete", attachment, "--force")
	if left := e.sh(id, "ls wp-content/uploads/"+file[:strings.LastIndex(file, "/")]); strings.Contains(left, "photo") {
		t.Fatalf("copies outlived their image:\n%s", left)
	}

	// A pull-zone CDN: the site's static files are linked there, and the
	// page cache (which held the old links) is purged.
	settle()
	get("/", mac)
	if _, err := svc.SetCDN(ctx, id, CDNInput{Provider: "generic", AssetHost: "cdn.perf.test"}); err != nil {
		t.Fatal(err)
	}
	if exists(cache + "index.html") {
		t.Fatal("pages with the old links survived")
	}
	// Block themes inline their small stylesheets: a post with an image
	// has a static link to rewrite for sure.
	post := e.wp(id, "post", "create", "--post_status=publish", "--post_title=Picture", "--porcelain",
		`--post_content=<img src="https://perf.test/wp-content/uploads/2026/09/sftp.jpg" alt=""> <img src="/wp-content/uploads/2026/09/sftp.jpg" alt="">`)
	_, body := get("/?p="+post, mac)
	if strings.Count(body, `src="https://cdn.perf.test/wp-content/uploads/2026/09/sftp.jpg"`) != 2 ||
		regexp.MustCompile(`(src|href)="(https://perf\.test)?/wp-(content|includes)/`).MatchString(body) {
		t.Fatalf("links not rewritten:\n%s", regexp.MustCompile(`(?:src|href)="[^"]*"`).FindAllString(body, -1))
	}

	// PHP-FPM load is sampled for the replica.
	load, err := e.docker.FPMLoad(ctx)
	if _, ok := load[runtime.ContainerName(id, st.Upstreams[0])]; err != nil || !ok {
		t.Fatalf("FPM load %v %v", load, err)
	}
}
