package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/files"
)

// withFiles gives the tenancy panel a file manager over temporary
// docroots, one per site, each with an index.php.
func withFiles(t *testing.T, e *tenancyEnv) string {
	t.Helper()
	base := t.TempDir()
	for _, id := range []string{"sa", "sb", "sc", "sr", "sx"} {
		if err := os.MkdirAll(filepath.Join(base, id, "public"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(base, id, "public", "index.php"), []byte("<?php // "+id), 0o644)
	}
	e.api.Files = &files.Service{Root: func(id string) string { return filepath.Join(base, id, "public") },
		Store: e.st, UID: os.Getuid(), GID: os.Getgid()}
	return base
}

func TestFileManagerNeedsThePlanFeature(t *testing.T) {
	e := newTenancyEnv(t)
	withFiles(t, e)
	var out map[string]any
	// Basic (alice) doesn't include the file manager; Full (bob) does.
	if c := e.as("alice", "GET", "/api/v1/sites/sa/files?path=/", "", &out); c != 403 || !strings.Contains(out["error"].(string), "files") {
		t.Errorf("without the feature: %d %v", c, out)
	}
	if c := e.as("alice", "PUT", "/api/v1/sites/sa/files/content?path=/x.php", "<?php", nil); c != 403 {
		t.Errorf("upload without the feature: %d", c)
	}
	if c := e.as("bob", "GET", "/api/v1/sites/sb/files?path=/", "", &out); c != 200 {
		t.Errorf("with the feature: %d %v", c, out)
	}
	// Someone else's site doesn't exist, whatever the plan.
	if c := e.as("bob", "GET", "/api/v1/sites/sa/files?path=/", "", nil); c != 404 {
		t.Errorf("another account's site: %d", c)
	}
	// Staff aren't bound by plans.
	if c := e.as("tok", "GET", "/api/v1/sites/sa/files?path=/", "", nil); c != 200 {
		t.Errorf("staff: %d", c)
	}
}

func TestFileManagerEndToEnd(t *testing.T) {
	e := newTenancyEnv(t)
	base := withFiles(t, e)
	doc := filepath.Join(base, "sb", "public")
	as := func(method, path, body string, out any) int {
		return e.as("bob", method, "/api/v1/sites/sb/files"+path, body, out)
	}

	if c := as("POST", "/folder?path=/wp-content", "", nil); c != 204 {
		t.Fatalf("mkdir: %d", c)
	}
	if c := as("PUT", "/content?path=/wp-content/hello.txt", "hi there", nil); c != 200 {
		t.Fatalf("upload: %d", c)
	}
	var conflict map[string]string
	if c := as("PUT", "/content?path=/wp-content/hello.txt", "again", &conflict); c != 409 {
		t.Errorf("upload over a file without overwrite: %d %v", c, conflict)
	}
	var txt files.Text
	if c := as("GET", "/content?path=/wp-content/hello.txt", "", &txt); c != 200 || txt.Content != "hi there" {
		t.Fatalf("read: %d %+v", c, txt)
	}
	if c := as("PUT", "/content?path=/wp-content/hello.txt&version="+txt.Version, "edited", nil); c != 200 {
		t.Errorf("save: %d", c)
	}
	if c := as("PUT", "/content?path=/wp-content/hello.txt&version="+txt.Version, "stale", nil); c != 409 {
		t.Errorf("stale save: %d", c)
	}
	if b, _ := os.ReadFile(filepath.Join(doc, "wp-content/hello.txt")); string(b) != "edited" {
		t.Errorf("on disk: %q", b)
	}
	if c := as("POST", "/move?path=/wp-content/hello.txt&to=/hello.txt", "", nil); c != 204 {
		t.Errorf("move: %d", c)
	}
	if c := as("PUT", "/mode?path=/hello.txt&mode=600", "", nil); c != 204 {
		t.Errorf("chmod: %d", c)
	}
	if c := as("PUT", "/mode?path=/hello.txt&mode=rw", "", nil); c != 400 {
		t.Errorf("chmod with nonsense: %d", c)
	}
	if c := as("GET", "/content?path=/../../sa/public/index.php", "", &txt); c == 200 {
		t.Errorf("read another site's file: %q", txt.Content)
	}
	var l files.Listing
	if c := as("GET", "?path=/", "", &l); c != 200 || len(l.Entries) != 3 {
		t.Errorf("list: %d %+v", c, l)
	}
	if c := as("DELETE", "?path=/wp-content", "", nil); c != 204 {
		t.Errorf("delete: %d", c)
	}
	if _, err := os.Stat(filepath.Join(doc, "wp-content")); err == nil {
		t.Error("not deleted")
	}

	// Every change is in the audit log, with the paths it touched.
	entries, err := e.st.Audit(t.Context(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range entries {
		actions = append(actions, a.Action)
	}
	if all := strings.Join(actions, "\n"); !strings.Contains(all, "POST /sites/sb/files/move?path=/wp-content/hello.txt&to=/hello.txt") ||
		!strings.Contains(all, "DELETE /sites/sb/files?path=/wp-content") {
		t.Errorf("audit log:\n%s", all)
	}
}

// Files are served from the panel's origin: nothing a site holds may run
// there as a page.
func TestDownloadsCantRunInThePanel(t *testing.T) {
	e := newTenancyEnv(t)
	base := withFiles(t, e)
	doc := filepath.Join(base, "sb", "public")
	os.WriteFile(filepath.Join(doc, "evil.html"), []byte("<script>alert(1)</script>"), 0o644)
	os.WriteFile(filepath.Join(doc, "evil.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), 0o644)
	os.WriteFile(filepath.Join(doc, "logo.png"), []byte("\x89PNG\r\n\x1a\n"), 0o644)
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/sites/sb/files/download?path="+path, nil)
		req.Header.Set("Authorization", "Bearer "+e.tokens["bob"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	for _, p := range []string{"/evil.html", "/evil.html&inline=1", "/evil.svg&inline=1", "/logo.png"} {
		resp := get(p)
		h := resp.Header
		if resp.StatusCode != 200 || h.Get("Content-Type") != "application/octet-stream" ||
			!strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: %d %v", p, resp.StatusCode, h)
		}
	}
	// Raster images may be previewed, as images.
	resp := get("/logo.png&inline=1")
	if resp.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("preview: %v", resp.Header)
	}
	// A folder comes as a zip archive.
	resp = get("/")
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Content-Disposition"), "sb.zip") ||
		!strings.HasPrefix(string(b), "PK") {
		t.Errorf("folder: %v %.20q", resp.Header, b)
	}
}
