package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The React panel is the one at /; the classic one moved to /classic/ and
// /next/ (where the React one was previewed) sends people to /.
func TestPanelAddresses(t *testing.T) {
	e := newTenancyEnv(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		res, err := noRedirect.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}

	res, body := get("/")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `<div id="root">`) || !strings.Contains(body, "/next/assets/") {
		t.Fatalf("/ = %d, want the React panel: %.200s", res.StatusCode, body)
	}
	if res.Header.Get("Cache-Control") != "no-cache" || !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Errorf("/ headers: %v", res.Header)
	}

	res, body = get("/classic/")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `src="/app.js"`) {
		t.Fatalf("/classic/ = %d, want the classic panel: %.200s", res.StatusCode, body)
	}

	res, _ = get("/next/")
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/" {
		t.Errorf("/next/ = %d → %q, want a redirect to /", res.StatusCode, res.Header.Get("Location"))
	}

	// The built scripts and the classic panel's files are still served.
	if res, _ = get("/app.js"); res.StatusCode != http.StatusOK {
		t.Errorf("/app.js = %d", res.StatusCode)
	}
	if res, _ = get("/next/theme.js"); res.StatusCode != http.StatusOK {
		t.Errorf("/next/theme.js = %d", res.StatusCode)
	}
}
