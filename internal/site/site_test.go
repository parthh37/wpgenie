package site

import (
	"strings"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	ok := map[string]string{
		"Example.COM":                 "example.com",
		" https://blog.example.com/ ": "blog.example.com",
		"shop.example.co.uk.":         "shop.example.co.uk",
		"xn--bcher-kva.example":       "xn--bcher-kva.example",
	}
	for in, want := range ok {
		got, err := NormalizeDomain(in)
		if err != nil || got != want {
			t.Errorf("NormalizeDomain(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "localhost", "-a.com", "a..com", "exa mple.com", "example.com/path", "example.com {", "*.example.com", "1.2.3.4"} {
		if _, err := NormalizeDomain(bad); err == nil {
			t.Errorf("NormalizeDomain(%q) accepted", bad)
		}
	}
}

func TestWPConfigSafeForPHP(t *testing.T) {
	out, err := renderWPConfig(wpConfigData{SiteID: "s1", DBName: "wp_s1", DBUser: "u_s1", DBPassword: "pw", DBHost: "db", RedisHost: "r", TablePrefix: "wp_ab12_"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, name := range saltNames {
		if n := strings.Count(s, "define( '"+name+"',"); n != 1 {
			t.Errorf("%s defined %d times, want 1", name, n)
		}
	}
	if !strings.Contains(s, "DISALLOW_FILE_EDIT', true") {
		t.Error("missing hardening constant")
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "define( '") && strings.Contains(line, "_SALT") {
			val := line[strings.Index(line, ", '")+3 : strings.LastIndex(line, "'")]
			if strings.ContainsAny(val, `'\`) {
				t.Errorf("salt contains a PHP-breaking character: %s", line)
			}
		}
	}
}
