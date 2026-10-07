package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The panel's CSP is default-src 'self': no inline scripts or <style>
// elements, no style attributes, nothing from other hosts. The React panel
// (static/next, built from web/) is checked here so a dependency that
// injects styles, or an inline script in index.html, fails the build
// rather than the browser.

var (
	scriptTag = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	styleTag  = regexp.MustCompile(`(?i)<style\b`)
	styleAttr = regexp.MustCompile(`(?i)\sstyle\s*=`)
	remoteRef = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*["']?(?:https?:)?//`)
	// What libraries do to inject CSS: a <style> element made at run time,
	// or rules inserted into a sheet.
	styleInjection = regexp.MustCompile(`createElement\(\s*["'\x60]style["'\x60]\s*\)|\.insertRule\(|\badoptedStyleSheets\b`)
	// React DOM's own: React 19 hoists <style href precedence> elements,
	// which only Base UI renders, and CSPProvider disableStyleElements
	// (web/src/main.tsx) turns those off.
	reactHoistedStyle = regexp.MustCompile(`\(\w+\.ownerDocument\|\|\w+\)\.createElement\(\x60style\x60\)$`)
)

func TestNextPanelIsCSPSafe(t *testing.T) {
	html, err := fs.ReadFile(Static, "static/next/index.html")
	if err != nil {
		t.Fatalf("static/next/index.html: %v (run `make ui`)", err)
	}
	page := string(html)
	for _, m := range scriptTag.FindAllStringSubmatch(page, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Errorf("inline script in index.html: %.80s", m[0])
		}
	}
	if styleTag.MatchString(page) {
		t.Error("<style> element in index.html")
	}
	if styleAttr.MatchString(page) {
		t.Error("style attribute in index.html")
	}
	if remoteRef.MatchString(page) {
		t.Error("index.html loads something from another host")
	}

	assets, err := fs.Glob(Static, "static/next/assets/*.js")
	if err != nil || len(assets) == 0 {
		t.Fatalf("no built scripts in static/next/assets (%v)", err)
	}
	for _, name := range assets {
		js, err := fs.ReadFile(Static, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range styleInjection.FindAllIndex(js, -1) {
			if reactHoistedStyle.Match(js[max(0, loc[0]-40):loc[1]]) {
				continue
			}
			from, to := max(0, loc[0]-60), min(len(js), loc[1]+60)
			t.Errorf("%s injects styles at run time (blocked by the CSP): …%s…", name, js[from:to])
		}
	}
}
