package proxy

import (
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var perfSite = Site{ID: "sperf001", Name: "Perf", Domains: []string{"perf.test"},
	Root: "/var/lib/wpgenie/sites/sperf001/public", Upstreams: []string{"127.0.0.1:19000"},
	ShieldEnabled: true, PageCache: true, EdgeHTML: true, Images: []string{"webp", "avif"}}

func TestRenderPerformance(t *testing.T) {
	cdnSite := perfSite
	cdnSite.ID, cdnSite.Domains, cdnSite.AssetCDN, cdnSite.EdgeHTML = "scdn0001", []string{"cdn-origin.test"}, true, false
	out, err := testCaddy().Render([]Site{perfSite, cdnSite})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	perf := s[strings.Index(s, "perf.test {"):strings.Index(s, "cdn-origin.test {")]
	other := s[strings.Index(s, "cdn-origin.test {"):]
	for _, want := range []string{
		"file /wp-content/cache/wpgenie{path}index-mobile.html",
		"file /wp-content/cache/wpgenie{path}index-desktop.html",
		`{header.User-Agent}.matches("` + MobileUA + `")`,
		"precompressed br gzip",
		"header @wpg_img Vary Accept",
	} {
		if !strings.Contains(perf, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The best format is tried first: once rewritten to x.jpg.avif, the
	// WebP rule no longer matches.
	if a, w := strings.Index(perf, "rewrite @wpg_img_avif"), strings.Index(perf, "rewrite @wpg_img_webp"); a < 0 || w < a {
		t.Error("AVIF must be preferred over WebP")
	}
	if strings.Index(perf, "rewrite @wpg_img_avif") > strings.Index(perf, "php_fastcgi") {
		t.Error("formats must be picked before PHP's try_files")
	}
	// Only device-independent pages may be kept by a CDN, which can't tell
	// phones from computers.
	cached := perf[strings.Index(perf, "route @wpg_cached {"):strings.Index(perf, "route @wpg_cached_mobile {")]
	rest := perf[strings.Index(perf, "route @wpg_cached_mobile {"):strings.Index(perf, "php_fastcgi")]
	if !strings.Contains(cached, "s-maxage=3600") || strings.Contains(rest, "s-maxage") {
		t.Errorf("s-maxage belongs on device-independent cache hits only:\n%s%s", cached, rest)
	}
	if strings.Contains(other, "s-maxage") {
		t.Error("s-maxage without edge caching")
	}
	// Behind a pull-zone CDN: CORS for fonts, no negotiation (the CDN would
	// cache one format for every browser).
	if !strings.Contains(other, "header @wpg_fonts Access-Control-Allow-Origin *") || strings.Contains(other, "wpg_img") {
		t.Errorf("asset CDN site:\n%s", other)
	}
	if strings.Contains(perf, "Access-Control-Allow-Origin") {
		t.Error("CORS without an asset CDN")
	}
	if _, err := testCaddy().Render([]Site{{ID: "x", Domains: []string{"a.test"}, Upstreams: []string{"127.0.0.1:1"},
		Images: []string{"jxl"}}}); err == nil {
		t.Error("unknown image format rendered")
	}
	adapt(t, out)
}

// The PHP side decides what to store with the same rules Caddy serves by:
// a drift would serve a page to requests that couldn't have produced it.
func TestCacheRulesMatchPHP(t *testing.T) {
	php, err := os.ReadFile("../../images/php/page-cache.php")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`WPGENIE_CACHE_MOBILE_UA = '#(.+)#';`).FindSubmatch(php)
	if m == nil || string(m[1]) != MobileUA {
		t.Errorf("page-cache.php mobile rule %q, Caddy's %q", m, MobileUA)
	}
	m = regexp.MustCompile(`WPGENIE_CACHE_BYPASS_COOKIES = '/\^\((.+)\)/';`).FindSubmatch(php)
	if m == nil || !slices.Equal(strings.Split(string(m[1]), "|"), CacheBypassCookies) {
		t.Errorf("page-cache.php bypass cookies %q, Caddy's %q", m, CacheBypassCookies)
	}
}

// TestPerformanceInRealCaddy serves the page cache's device copies and
// compressed files, and converted images, from a real Caddy. Needs Docker:
// WPGENIE_TEST_DOCKER=1.
func TestPerformanceInRealCaddy(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to run against real Caddy via Docker")
	}
	const cache = "wp-content/cache/wpgenie"
	files := map[string]string{
		cache + "/index.html":                 "home",
		cache + "/index.html.br":              "home-br",
		cache + "/shop/index-mobile.html":     "shop-mobile",
		cache + "/shop/index-desktop.html":    "shop-desktop",
		"wp-content/uploads/a.jpg":            "jpg",
		"wp-content/uploads/a.jpg.avif":       "avif",
		"wp-content/uploads/a.jpg.webp":       "webp",
		"wp-content/uploads/b.png":            "png",
		"wp-content/uploads/b.png.webp":       "png-webp",
		"wp-content/uploads/gone.jpg.avif":    "orphan", // original deleted
		"wp-content/themes/t/font.woff2":      "font",
		"wp-content/uploads/not-an-image.pdf": "pdf",
	}
	site := perfSite
	site.ShieldEnabled = false // the stand-in shield refuses everything
	// The test client's connections come from the Docker gateway: trusting
	// every address makes CF-Connecting-IP count, as from Cloudflare's edge.
	base := startSiteCaddy(t, netip.MustParsePrefix("0.0.0.0/0"), site, files, "/wp-content/uploads/a.jpg")

	get := func(path string, hdr ...string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Host = "a.test"
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultTransport.RoundTrip(req) // no transparent gzip
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := make([]byte, 64)
		n, _ := resp.Body.Read(b)
		return resp, string(b[:n])
	}
	const iphone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Mobile/15E148 Safari/604.1"
	const mac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) Safari/605.1.15"
	for _, c := range []struct {
		name, path string
		hdr        []string
		want       string
		wantHeader [2]string
	}{
		{"device-independent page", "/", []string{"User-Agent", iphone}, "home", [2]string{"Cache-Control", "public, max-age=0, s-maxage=3600"}},
		{"precompressed brotli", "/", []string{"Accept-Encoding", "br, gzip"}, "home-br", [2]string{"Content-Encoding", "br"}},
		{"mobile copy", "/shop/", []string{"User-Agent", iphone}, "shop-mobile", [2]string{"Cache-Control", ""}},
		{"desktop copy", "/shop/", []string{"User-Agent", mac}, "shop-desktop", [2]string{"Cache-Control", ""}},
		{"client hint wins over the UA", "/shop/", []string{"User-Agent", iphone, "Sec-CH-UA-Mobile", "?0"}, "shop-desktop", [2]string{}},
		{"AVIF first", "/wp-content/uploads/a.jpg", []string{"Accept", "image/avif,image/webp,*/*"}, "avif", [2]string{"Content-Type", "image/avif"}},
		{"WebP", "/wp-content/uploads/a.jpg", []string{"Accept", "image/webp,*/*"}, "webp", [2]string{"Vary", "Accept"}},
		{"PNG to WebP", "/wp-content/uploads/b.png", []string{"Accept", "image/avif,image/webp"}, "png-webp", [2]string{}},
		{"original for old browsers", "/wp-content/uploads/a.jpg", []string{"Accept", "*/*"}, "jpg", [2]string{"Content-Type", "image/jpeg"}},
		{"original through the CDN edge", "/wp-content/uploads/a.jpg", []string{"Accept", "image/avif", "CF-Connecting-IP", "198.51.100.7"}, "jpg", [2]string{}},
	} {
		resp, body := get(c.path, c.hdr...)
		if resp.StatusCode != 200 || body != c.want {
			t.Errorf("%s: %d %q, want %q", c.name, resp.StatusCode, body, c.want)
		}
		if k := c.wantHeader[0]; k != "" && resp.Header.Get(k) != c.wantHeader[1] {
			t.Errorf("%s: %s %q, want %q", c.name, k, resp.Header.Get(k), c.wantHeader[1])
		}
	}
	if resp, _ := get("/wp-content/uploads/gone.jpg", "Accept", "image/avif"); resp.StatusCode == 200 {
		t.Error("a converted copy was served for a deleted original")
	}
	if resp, _ := get("/wp-content/themes/t/font.woff2"); resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS without an asset CDN")
	}
}
