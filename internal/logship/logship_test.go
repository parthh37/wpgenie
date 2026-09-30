package logship

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	enable(t, s, nil)
	s.apply(ctx)
	runs := d.find("run")
	if len(runs) != 1 {
		t.Fatalf("runs: %v", d.calls)
	}
	run := runs[0]
	joined := strings.Join(run, " ")
	for _, want := range []string{"--cap-drop ALL", "--cap-add DAC_READ_SEARCH", "--read-only", "--security-opt no-new-privileges",
		"--memory 384m", "--name " + Container, "--network bridge", "timberio/vector:0.58.0-alpine --config /etc/vector/vector.yaml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("run lacks %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, testDestination().SecretKey) || strings.Contains(joined, "AKIAEXAMPLE") {
		t.Errorf("credentials in argv: %s", joined)
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
		slices.ContainsFunc(mounts, func(m string) bool { return strings.Contains(m, ctrContainers) || strings.Contains(m, ctrMail) }) {
		t.Errorf("mounts: %v", mounts)
	}
	env, err := os.ReadFile(argValue(run, "--env-file")[0])
	if err != nil || !strings.Contains(string(env), "AWS_SECRET_ACCESS_KEY="+testDestination().SecretKey+"\n") {
		t.Errorf("env file: %q %v", env, err)
	}
	for _, f := range []string{"vector.yaml", "vector.env", "tables/sites.csv"} {
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

// Containers' own output ships only with Docker's json-file logs, and only
// WPGenie's containers are in the table.
func TestShipperContainersType(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { runtime.SetContainerLogLimit(0, 0) })
	s, d, _ := newService(t)
	id := strings.Repeat("a", 64)
	d.ps = id + " wpg-s1-19001\n" + strings.Repeat("b", 64) + " someone-elses-db\n"
	enable(t, s, func(set *Settings) { set.Types[TypeContainers] = true })
	s.apply(ctx)
	if !s.Available(TypeContainers) {
		t.Fatal("containers unavailable with json-file")
	}
	mounts := argValue(d.find("run")[0], "-v")
	if !slices.Contains(mounts, "/var/lib/docker/containers:"+ctrContainers+":ro") {
		t.Errorf("mounts: %v", mounts)
	}
	if tbl, _ := os.ReadFile(filepath.Join(s.Cfg.Dir, "tables", "containers.csv")); string(tbl) != "id,name\n"+id+",wpg-s1-19001\n" {
		t.Errorf("containers table: %q", tbl)
	}

	s2, d2, _ := newService(t)
	d2.driver = "journald"
	enable(t, s2, func(set *Settings) { set.Types[TypeContainers] = true })
	s2.apply(ctx)
	if s2.Available(TypeContainers) || slices.ContainsFunc(argValue(d2.find("run")[0], "-v"),
		func(m string) bool { return strings.Contains(m, ctrContainers) }) {
		t.Error("containers shipped without json-file logs")
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
	s.pollMetrics(ctx) // learns the counters
	d.metrics = strings.ReplaceAll(strings.ReplaceAll(sampleMetrics, "} 1100", "} 1500"), "} 1200", "} 1250")
	s.pollMetrics(ctx)
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
	s.pollMetrics(ctx)
	if st, _ := s.Status(ctx); st.Health != "error" || !strings.Contains(st.HealthMessage, "Test connection") {
		t.Errorf("failing uploads: %s %q", st.Health, st.HealthMessage)
	}
}
