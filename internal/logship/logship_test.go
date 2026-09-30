package logship

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

func argValue(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// The shipper's container: hardened, credentials only in the env file,
// recreated only when its configuration changes, stopped when shipping
// is turned off.
func TestShipperContainer(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { runtime.SetContainerLogLimit(0, 0) })
	s, d, _ := newService(t)
	var resyncs int
	s.Resync = func(context.Context) error { resyncs++; return nil }
	s.Store.CreateSite(ctx, &store.Site{ID: "s1", Name: "s1", PrimaryDomain: "a.test", PHPVersion: "8.3", FPMPort: 19001,
		DBName: "wp_s1", Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1})

	s.apply(ctx) // off: nothing runs
	if runs := d.find("run"); len(runs) != 0 {
		t.Fatalf("started while off: %v", runs)
	}

	// Caddy's access log is its own (0600); the WAF log next to it too.
	os.WriteFile(s.Cfg.AccessLog, []byte("{}\n"), 0o600)
	wafLog := filepath.Join(filepath.Dir(s.Cfg.AccessLog), "waf.log")
	os.WriteFile(wafLog, []byte("{}\n"), 0o600)
	os.Chmod(filepath.Dir(s.Cfg.AccessLog), 0o700)

	enable(t, s, nil)
	s.apply(ctx)
	runs := d.find("run")
	if len(runs) != 1 {
		t.Fatalf("runs: %v", d.calls)
	}
	run := runs[0]
	joined := strings.Join(run, " ")
	for _, want := range []string{"--cap-drop ALL", "--user 0:0", "--read-only", "--security-opt no-new-privileges",
		"--memory 384m", "--name " + Container, "--network bridge", "timberio/vector:0.58.0-alpine --config /etc/vector/vector.yaml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("run lacks %q: %s", want, joined)
		}
	}
	// No capability at all (DAC_READ_SEARCH would allow open_by_handle_at:
	// any file of the host), no environment (docker inspect shows it).
	for _, bad := range []string{"--cap-add", "--env-file", "-e", "--privileged"} {
		if slices.Contains(run, bad) {
			t.Errorf("run has %s: %s", bad, joined)
		}
	}
	if strings.Contains(joined, testDestination().SecretKey) || strings.Contains(joined, "AKIAEXAMPLE") {
		t.Errorf("credentials in argv: %s", joined)
	}
	// The access log (not the WAF log) is made readable to the log's
	// group, which the container gets (the test runs as a user: as root,
	// a root-owned log needs nothing).
	if os.Getuid() != 0 {
		fi, _ := os.Stat(s.Cfg.AccessLog)
		di, _ := os.Stat(filepath.Dir(s.Cfg.AccessLog))
		wi, _ := os.Stat(wafLog)
		gid := strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Gid))
		if fi.Mode().Perm() != 0o640 || di.Mode().Perm() != 0o750 || wi.Mode().Perm() != 0o600 ||
			!slices.Equal(argValue(run, "--group-add"), []string{gid}) {
			t.Errorf("access log %v, dir %v, waf log %v, groups %v (want %s)", fi.Mode(), di.Mode(), wi.Mode(),
				argValue(run, "--group-add"), gid)
		}
	}
	mounts := argValue(run, "-v")
	for _, m := range mounts {
		ro := strings.HasSuffix(m, ":ro")
		writable := strings.HasSuffix(m, ":"+ctrData) || strings.HasSuffix(m, ":"+ctrSpool)
		if ro == writable {
			t.Errorf("mount %s: read-only must be everything but the data and the spool", m)
		}
	}
	if !slices.ContainsFunc(mounts, func(m string) bool { return strings.HasSuffix(m, ":"+ctrCaddy+":ro") }) ||
		slices.ContainsFunc(mounts, func(m string) bool { return strings.Contains(m, "containers") || strings.Contains(m, ctrMail) }) {
		t.Errorf("mounts: %v", mounts)
	}
	creds, err := os.ReadFile(filepath.Join(s.Cfg.Dir, "secrets", "credentials"))
	if err != nil || string(creds) != "[wpgenie]\naws_access_key_id=AKIAEXAMPLE123\naws_secret_access_key="+testDestination().SecretKey+"\n" {
		t.Errorf("credentials file: %q %v", creds, err)
	}
	if !slices.Contains(mounts, filepath.Join(s.Cfg.Dir, "secrets")+":"+ctrSecrets+":ro") {
		t.Errorf("credentials not mounted: %v", mounts)
	}
	for _, f := range []string{"vector.yaml", "secrets/credentials", "tables/sites.csv"} {
		if fi, err := os.Stat(filepath.Join(s.Cfg.Dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if sites, _ := os.ReadFile(filepath.Join(s.Cfg.Dir, "tables", "sites.csv")); string(sites) != "host,site\na.test,s1\n" {
		t.Errorf("sites table: %q", sites)
	}
	// Local retention while shipping: capped container logs, 2 access logs.
	if mb, files := runtime.ContainerLogLimit(); mb != 10 || files != 2 {
		t.Errorf("container logs: %d MB × %d", mb, files)
	}
	if resyncs != 1 || s.AccessLogKeep() != 2 {
		t.Errorf("caddy resyncs %d, keep %d", resyncs, s.AccessLogKeep())
	}

	// Nothing changed: nothing restarts.
	d.reset()
	s.apply(ctx)
	if c := d.commands(); slices.Contains(c, "run") || slices.Contains(c, "rm") || slices.Contains(c, "kill") {
		t.Errorf("restarted without a change: %v", c)
	}
	// A new domain: the table changes, Vector reloads it (no restart).
	s.Store.AddDomain(ctx, "s1", "www.a.test", false)
	d.reset()
	s.apply(ctx)
	if c := d.commands(); slices.Contains(c, "run") || len(d.find("kill")) != 1 || argValue(d.find("kill")[0], "--signal")[0] != "HUP" {
		t.Errorf("table change: %v", d.calls)
	}
	// A setting that changes the configuration: recreated.
	set, _ := s.Settings(ctx)
	set.BatchMaxSeconds = 60
	s.SetSettings(ctx, set.Redacted())
	d.reset()
	s.apply(ctx)
	if c := d.commands(); !slices.Contains(c, "stop") || !slices.Contains(c, "rm") || len(d.find("run")) != 1 {
		t.Errorf("config change: %v", c)
	}
	// Off: stopped; local retention back to the defaults.
	set.Enabled = false
	s.SetSettings(ctx, set.Redacted())
	d.reset()
	s.apply(ctx)
	if d.state != "" || len(d.find("run")) != 0 {
		t.Errorf("still running when off: %v", d.calls)
	}
	if mb, _ := runtime.ContainerLogLimit(); mb != 0 || s.AccessLogKeep() != 0 || resyncs != 2 {
		t.Errorf("local retention while off: %d MB, keep %d, resyncs %d", mb, s.AccessLogKeep(), resyncs)
	}
}

// A shipper that keeps crashing is left to Docker's restart backoff (not
// recreated every minute) and reported.
func TestShipperCrashLoop(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { runtime.SetContainerLogLimit(0, 0) })
	s, d, _ := newService(t)
	enable(t, s, nil)
	s.apply(ctx)
	d.state, d.exit, d.restart = "restarting", 78, 5
	d.reset()
	s.apply(ctx)
	if c := d.commands(); slices.Contains(c, "run") || slices.Contains(c, "rm") {
		t.Errorf("recreated a crashing shipper: %v", c)
	}
	st, _ := s.Status(ctx)
	if st.Health != "error" || !strings.Contains(st.HealthMessage, "keeps stopping (exit code 78, restarted 5 times)") ||
		st.Shipper.Restarts != 5 {
		t.Errorf("status: %s %q", st.Health, st.HealthMessage)
	}
	// Its settings change: a new container.
	set, _ := s.Settings(ctx)
	set.BatchMaxSeconds = 90
	s.SetSettings(ctx, set.Redacted())
	d.reset()
	s.apply(ctx)
	if len(d.find("run")) != 1 {
		t.Errorf("not recreated for new settings: %v", d.commands())
	}
}

// Starting the shipper is bounded: a pull that hangs gives up (and the
// loop goes on).
func TestShipperStartTimeout(t *testing.T) {
	s, _, _ := newService(t)
	enable(t, s, nil)
	s.Docker = hangingDocker{s.Docker}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.apply(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("apply hung on docker run")
	}
}

// hangingDocker's run waits for its context (a registry that stalls).
type hangingDocker struct{ Docker }

func (h hangingDocker) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if args[0] == "run" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return h.Docker.Run(ctx, stdin, args...)
}

// Containers' own output is read by the daemon (Vector never gets
// Docker's containers directory, with every container's settings), only
// WPGenie's containers', through Docker's log rotation.
func TestContainerLogs(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { runtime.SetContainerLogLimit(0, 0) })
	s, d, _ := newService(t)
	d.root = t.TempDir()
	ours, theirs, later := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	logOf := func(id string) string { return filepath.Join(d.root, "containers", id, id+"-json.log") }
	line := func(msg string) string {
		return `{"log":"` + msg + `\n","stream":"stdout","time":"2026-09-30T10:00:00Z"}` + "\n"
	}
	appendTo := func(path, s string) {
		os.MkdirAll(filepath.Dir(path), 0o700)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		f.WriteString(s)
		f.Close()
	}
	appendTo(logOf(ours), line("before shipping"))
	appendTo(logOf(theirs), line("not ours"))
	d.ps = ours + " wpg-s1-19001\n" + theirs + " someone-elses-db\n"
	enable(t, s, func(set *Settings) { set.Types[TypeContainers] = true })
	s.apply(ctx)
	if !s.Available(TypeContainers) {
		t.Fatal("containers unavailable with json-file")
	}
	if slices.ContainsFunc(argValue(d.find("run")[0], "-v"), func(m string) bool { return strings.Contains(m, d.root) }) {
		t.Error("the shipper got Docker's directory")
	}
	mustf(t, s.tailContainers(ctx), "first pass") // starts at the end
	appendTo(logOf(ours), line("one"))
	appendTo(logOf(theirs), line("still not ours"))
	mustf(t, s.tailContainers(ctx), "second pass")
	// Docker rotates: the rest of the old file, then the new one.
	appendTo(logOf(ours), line("two"))
	os.Rename(logOf(ours), logOf(ours)+".1")
	appendTo(logOf(ours), line("three")+`{"log":"partial`)
	// A container that appears later is read from its start.
	appendTo(logOf(later), line("new container"))
	d.ps += later + " wpgenie-phpmyadmin\n"
	s.ctail.listedAt = time.Time{}
	mustf(t, s.tailContainers(ctx), "third pass")

	var got []string
	for _, l := range readSpool(t, s.spool.Dir, TypeContainers) {
		var e containerEntry
		json.Unmarshal([]byte(l), &e)
		got = append(got, e.Container+":"+e.Message)
	}
	want := []string{"wpg-s1-19001:one", "wpg-s1-19001:two", "wpg-s1-19001:three", "wpgenie-phpmyadmin:new container"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("shipped %q, want %q", got, want)
	}

	s2, d2, _ := newService(t)
	d2.driver = "journald"
	enable(t, s2, func(set *Settings) { set.Types[TypeContainers] = true })
	s2.apply(ctx)
	if s2.Available(TypeContainers) {
		t.Error("containers available without json-file logs")
	}
	if mb, _ := runtime.ContainerLogLimit(); mb != 0 {
		t.Error("json-file options set for another logging driver")
	}
}

func TestShipperStartFailure(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newService(t)
	d.runErr = errors.New("docker run: exit status 125")
	enable(t, s, nil)
	s.apply(ctx)
	st, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Health != "error" || !strings.Contains(st.HealthMessage, "couldn't start") || !strings.Contains(st.Shipper.Error, "pull access denied") {
		t.Errorf("status: %+v", st)
	}
}

const sampleMetrics = `# HELP vector_build_info Build information.
# TYPE vector_build_info gauge
vector_build_info{arch="x86_64",debug="false",host="abc",revision="x",rust_version="1.90",version="0.58.0"} 1 1790000000000
vector_component_received_events_total{component_id="in_access",component_kind="source",component_type="file",host="abc"} 1200
vector_component_received_event_bytes_total{component_id="in_access",component_kind="source",component_type="file",host="abc"} 480000
vector_component_received_events_total{component_id="in_spool",component_kind="source",component_type="file",host="abc"} 30
vector_component_sent_events_total{component_id="archive",component_kind="sink",component_type="aws_s3",host="abc",output="_default"} 1100
vector_component_sent_bytes_total{component_id="archive",component_kind="sink",component_type="aws_s3",endpoint="https://x",host="abc",protocol="https"} 52000
vector_component_errors_total{component_id="archive",component_kind="sink",component_type="aws_s3",error_type="request_failed",host="abc",stage="sending"} 2
vector_buffer_size_bytes{buffer_type="disk",component_id="archive",component_kind="sink",component_type="aws_s3",host="abc",stage="0"} 4096
vector_component_errors_total{component_id="t_access",component_kind="transform",error_type="x",host="abc",stage="processing",label="a \"quoted\" value"} 7
`

func TestParseVectorMetrics(t *testing.T) {
	m := parseVectorMetrics([]byte(sampleMetrics))
	if m.Version != "0.58.0" || m.Received[TypeAccess] != [2]float64{1200, 480000} || m.Sent != 1100 || m.SentBytes != 52000 ||
		m.Errors != 2 || m.BufferBytes != 4096 {
		t.Errorf("%+v", m)
	}
	smp := promText([]byte(sampleMetrics), map[string]bool{"vector_component_errors_total": true})
	if len(smp["vector_component_errors_total"]) != 2 || smp["vector_component_errors_total"][1].labels["label"] != `a "quoted" value` {
		t.Errorf("labels: %+v", smp)
	}
}

// The status: volumes per type, uploads and errors from Vector's metrics,
// health in plain words.
func TestStatus(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { runtime.SetContainerLogLimit(0, 0) })
	s, d, _ := newService(t)
	now := time.Now()
	st, err := s.Status(ctx)
	if err != nil || st.Health != "off" || len(st.Types) != len(Types) || len(st.Types[0].History) != historyDays {
		t.Fatalf("off: %+v %v", st, err)
	}

	enable(t, s, nil)
	s.apply(ctx)
	d.metrics = sampleMetrics
	s.pollMetrics(ctx, 0) // learns the counters
	d.metrics = strings.ReplaceAll(strings.ReplaceAll(sampleMetrics, "} 1100", "} 1500"), "} 1200", "} 1250")
	s.pollMetrics(ctx, 0)
	s.spool.Write(TypeSecurity, []byte(`{"verdict":"ban"}`))
	drainSpool(s.spool)
	s.recordVolumes(ctx)
	s.spool.Write(TypeSecurity, []byte(`{"verdict":"block"}`)) // queued: counted when written
	drainSpool(s.spool)

	st, err = s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Health != "ok" || st.Shipper.State != "running" || st.Shipper.Version != "0.58.0" || st.SentEvents != 1500 ||
		st.LastUpload.Before(now) || st.Server != "panel" || st.Destination.Bucket != "logs-bucket" {
		t.Errorf("status: %+v", st)
	}
	byType := map[string]TypeStatus{}
	for _, ts := range st.Types {
		byType[ts.Name] = ts
	}
	if a := byType[TypeAccess]; a.Today.Events != 50 || !a.Enabled || !a.Available {
		t.Errorf("access today: %+v", a.Today)
	}
	if sec := byType[TypeSecurity]; sec.Today.Events != 2 || sec.History[historyDays-1].Events != 2 {
		t.Errorf("security today: %+v", sec.Today)
	}
	if mail := byType[TypeMail]; mail.Available {
		t.Error("mail available without a mail server")
	}

	// Errors since the last upload: failing.
	d.metrics = strings.ReplaceAll(d.metrics, `stage="sending"} 2`, `stage="sending"} 9`)
	s.pollMetrics(ctx, 0)
	if st, _ := s.Status(ctx); st.Health != "error" || !strings.Contains(st.HealthMessage, "Test connection") {
		t.Errorf("failing uploads: %s %q", st.Health, st.HealthMessage)
	}
}

// Status requests at once read Vector's metrics once (an older reading
// landing after a newer one would look like a counter reset).
func TestPollMetricsOnce(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { runtime.SetContainerLogLimit(0, 0) })
	s, d, _ := newService(t)
	enable(t, s, nil)
	s.apply(ctx)
	d.metrics = sampleMetrics
	d.reset()
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); s.pollMetrics(ctx, time.Minute) }()
	}
	wg.Wait()
	if n := len(d.find("exec")); n != 1 {
		t.Errorf("%d metric reads, want 1", n)
	}
}

// Archives being read when the daemon stopped don't pile up.
func TestLoadClearsTmp(t *testing.T) {
	s, _, _ := newService(t)
	left := filepath.Join(s.Cfg.Dir, "tmp", "read-123", "14-a.log.gz")
	os.MkdirAll(filepath.Dir(left), 0o700)
	os.WriteFile(left, []byte("x"), 0o600)
	mustf(t, s.Load(context.Background()), "load")
	if _, err := os.Stat(filepath.Join(s.Cfg.Dir, "tmp")); !os.IsNotExist(err) {
		t.Errorf("tmp left: %v", err)
	}
}
