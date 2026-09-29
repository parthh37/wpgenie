package runtime

import (
	"slices"
	"strings"
	"testing"
)

var testSpec = SiteSpec{
	ID: "s1", Image: "wpgenie/php:8.3", ImageID: "sha256:abc", Dir: "/srv/s1", Docroot: "/srv/s1/public",
	Domain: "example.com", Network: "wpgenie", MemoryMB: 1024, CPUs: 2, MaxChildren: 8,
}

func TestRunArgsHardening(t *testing.T) {
	args := runArgs(testSpec, 19000)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--cap-drop ALL", "--security-opt no-new-privileges", "--read-only",
		"--user 82:82", "-p 127.0.0.1:19000:9000", "--pids-limit 256",
		"-v /srv/s1:/srv/s1", "-w /srv/s1/public", "--tmpfs /var/www/html:ro",
		"--name wpg-s1-19000", "--label wpgenie.site=s1", "--label wpgenie.port=19000",
		"--label wpgenie.spec=" + testSpec.Hash(), "--memory 1024m", "--cpus 2", "-e WPG_MAX_CHILDREN=8",
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

func TestSpecHash(t *testing.T) {
	base := testSpec.Hash()
	if base != testSpec.Hash() {
		t.Fatal("hash not deterministic")
	}
	for name, mut := range map[string]func(*SiteSpec){
		"image rebuild": func(s *SiteSpec) { s.ImageID = "sha256:def" },
		"memory":        func(s *SiteSpec) { s.MemoryMB = 2048 },
		"cpus":          func(s *SiteSpec) { s.CPUs = 1.5 },
		"workers":       func(s *SiteSpec) { s.MaxChildren = 9 },
	} {
		s := testSpec
		mut(&s)
		if s.Hash() == base {
			t.Errorf("%s change must change the spec hash", name)
		}
	}
	s := testSpec
	s.Domain = "other.example"
	if s.Hash() != base {
		t.Error("domain is not a container property; it must not force a recreate")
	}
}

func TestFPMMaxChildren(t *testing.T) {
	prev := 0
	for mem := 128; mem <= 65536; mem += 64 {
		n := FPMMaxChildren(mem)
		if n < MinMaxChildren || n > MaxMaxChildren {
			t.Fatalf("FPMMaxChildren(%d) = %d, outside [%d, %d]", mem, n, MinMaxChildren, MaxMaxChildren)
		}
		if n < prev {
			t.Fatalf("FPMMaxChildren(%d) = %d < %d: more memory must never mean fewer workers", mem, n, prev)
		}
		prev = n
	}
	// The sizes the doc comment promises; the smallest site keeps 2 workers.
	for mem, want := range map[int]int{256: 2, 512: 5, 1024: 13, 2048: 29, 8192: MaxMaxChildren} {
		if got := FPMMaxChildren(mem); got != want {
			t.Errorf("FPMMaxChildren(%d) = %d, want %d", mem, got, want)
		}
	}
}

func TestSocketStates(t *testing.T) {
	procNet := []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000    82        0 1 1 0000000000000000 100 0 0 10 0
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:2328 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000    82        0 2 1 0000000000000000 100 0 0 10 0
`)
	if l, n := socketStates(procNet, 9000); !l || n != 0 {
		t.Errorf("socketStates = %v, %d; want the tcp6 listener on :9000 (0x2328), no connections", l, n)
	}
	if l, _ := socketStates(procNet, 9001); l {
		t.Error("false positive for :9001")
	}
	busy := []byte("   0: 0100007F:2328 0100007F:C000 01 00000000:00000000\n" +
		"   1: 0100007F:2328 0100007F:C001 08 00000000:00000000\n" + // client gone, PHP still running
		"   2: 0100007F:2328 0100007F:C002 06 00000000:00000000\n" + // TIME_WAIT: finished
		"   3: 0100007F:1F90 0100007F:C003 01 00000000:00000000\n") //   other port
	if l, n := socketStates(busy, 9000); l || n != 2 {
		t.Errorf("socketStates = %v, %d; want not listening, 2 active", l, n)
	}
	// Every worker busy: three connections wait in the backlog of the
	// (tcp6) listener, the rx_queue of its LISTEN row.
	saturated := append(busy, "   4: 00000000000000000000000000000000:2328 00000000000000000000000000000000:0000 0A 00000000:00000003\n"+
		"   5: 0100007F:2328 0100007F:C004 01 00000000:00000000\n"+
		"   6: 0100007F:2328 0100007F:C005 01 00000000:00000000\n"+
		"   7: 0100007F:2328 0100007F:C006 01 00000000:00000000\n"...)
	if l, n, q := socketLoad(saturated, 9000); !l || n != 5 || q != 3 {
		t.Errorf("socketLoad = %v, %d, %d; want listening, 5 requests, 3 queued", l, n, q)
	}
}

func TestParseReplicas(t *testing.T) {
	out := []byte("wpg-s1-19003|19003|abc|running\nwpg-s1|||exited\n")
	got := parseReplicas(out)
	want := []Replica{
		{Name: "wpg-s1-19003", Port: 19003, SpecHash: "abc", Running: true},
		{Name: "wpg-s1"}, // pre-replica container: no labels, must never match a spec
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseReplicas = %+v, want %+v", got, want)
	}
	if parseReplicas([]byte("\n")) != nil {
		t.Error("empty output must yield no replicas")
	}
}

func TestCronArgsJailed(t *testing.T) {
	args := cronArgs("wpg-s1-19000", testSpec)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-e PHP_INI_SCAN_DIR=:/usr/local/etc/php/jail.d", "-e HTTP_HOST=example.com",
		"wpg-s1-19000 sh -c", "/usr/local/etc/php/jail.d/zz-jail.ini", "/srv/s1/public/wp-cron.php",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	// Paths are passed as positional args, never spliced into the script.
	script := args[slices.Index(args, "-c")+1]
	if strings.Contains(script, "/srv/s1") {
		t.Errorf("site path interpolated into shell script: %s", script)
	}
}

func TestValkeyPrefixIsNotAGlob(t *testing.T) {
	v := &Valkey{Docker: Docker{Bin: "/nonexistent"}, Container: "wpgenie-redis"}
	for _, bad := range []string{"", "*", "s1", "s1:*", "s?:", "s[1]:", "S1:", "a b:"} {
		if err := v.FlushPrefix(t.Context(), bad); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Errorf("FlushPrefix(%q) = %v, want refusal", bad, err)
		}
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

func TestParseCPUStats(t *testing.T) {
	out := []byte("wpg-s1-19000|87.25%\nwpgenie-mariadb|3.10%\nwpg-s1-19001|--\nwpg-s2-19002|150.00%\n\n")
	got := parseCPUStats(out)
	want := map[string]float64{"wpg-s1-19000": 87.25, "wpg-s2-19002": 150}
	if len(got) != len(want) {
		t.Fatalf("parseCPUStats = %v, want %v (infrastructure and unsampled containers skipped)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}
