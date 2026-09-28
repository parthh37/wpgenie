package runtime

import (
	"slices"
	"strings"
	"testing"
)

func TestRunArgsHardening(t *testing.T) {
	args := runArgs(SiteSpec{
		ID: "s1", Image: "wpgenie/php:8.3", Dir: "/srv/s1", Docroot: "/srv/s1/public",
		HostPort: 19000, Network: "wpgenie",
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--cap-drop ALL", "--security-opt no-new-privileges", "--read-only",
		"--user 82:82", "-p 127.0.0.1:19000:9000", "--pids-limit 256",
		"-v /srv/s1:/srv/s1", "-w /srv/s1/public", "--tmpfs /var/www/html:ro",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	if slices.Contains(args, "--privileged") {
		t.Error("site containers must never be privileged")
	}
	if strings.Contains(joined, "-p 0.0.0.0") || strings.Contains(joined, "-p 19000") {
		t.Error("PHP-FPM must only be published on loopback")
	}
}

func TestErrorLinesDropsEchoedSecrets(t *testing.T) {
	out := "2/3 [--admin_password=<password>]: hunter2\nwp core install --admin_password='hunter2'\nError: Error establishing a database connection.\n"
	got := errorLines([]byte(out))
	if strings.Contains(got, "hunter2") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, "database connection") {
		t.Fatalf("lost the actual error: %q", got)
	}
}
