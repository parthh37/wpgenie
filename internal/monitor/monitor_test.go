package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// evalEnv runs the evaluator against fake probes, certificates and
// filesystems, and collects what the webhook receives.
type evalEnv struct {
	t     *testing.T
	ctx   context.Context
	s     *Service
	st    *store.Store
	now   time.Time
	probe map[string]site.Health // by primary domain
	certs map[string]CertResult  // by domain
	used  float64                // data filesystem use, percent
	certN map[string]int         // handshakes per domain

	mu   sync.Mutex
	got  []Notice
	sent int
}

func newEvalEnv(t *testing.T) *evalEnv {
	t.Helper()
	e := &evalEnv{t: t, ctx: context.Background(), st: newStore(t), now: time.Now().Truncate(time.Second),
		probe: map[string]site.Health{}, certs: map[string]CertResult{}, used: 50, certN: map[string]int{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &p); err != nil {
			t.Error(err)
		}
		e.mu.Lock()
		e.got = append(e.got, p.Alerts...)
		e.sent++
		e.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	var pmu sync.Mutex
	e.s = &Service{Store: e.st, Log: quietLog(), Version: "v1", Server: "host1",
		Now: func() time.Time { return e.now },
		Probe: func(_ context.Context, st *store.Site) site.Health {
			pmu.Lock()
			defer pmu.Unlock()
			if h, ok := e.probe[st.PrimaryDomain]; ok {
				return h
			}
			return site.Health{Checked: true, OK: true}
		},
		CertCheck: func(_ context.Context, ct CertTarget) CertResult {
			pmu.Lock()
			defer pmu.Unlock()
			e.certN[ct.Domain]++
			if r, ok := e.certs[ct.Domain]; ok {
				return r
			}
			return CertResult{Served: true, NotAfter: e.now.Add(80 * 24 * time.Hour), Issuer: "Test CA"}
		},
		Hosts: func(context.Context) ([]Host, error) {
			const g = 1 << 30
			free := uint64(100*g) - uint64(e.used*float64(g))
			return []Host{{MemTotal: 4 * g, MemAvailable: g, Disks: []Disk{{Path: "/data", Size: 100 * g, Free: free, Avail: free}}}}, nil
		},
		Notifier: &Notifier{roots: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, allowLoopback: true},
	}
	set := DefaultSettings()
	set.Webhooks = []Webhook{{Name: "test", Enabled: true, URL: srv.URL}}
	if _, _, err := e.s.SetSettings(e.ctx, set); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *evalEnv) addSite(id, domain, parent string) {
	e.t.Helper()
	if err := e.st.CreateSite(e.ctx, &store.Site{ID: id, Name: id, PrimaryDomain: domain, PHPVersion: "8.3",
		FPMPort: 19000 + len(id) + int(domain[0]), DBName: "wp_" + id, Status: store.StatusActive, MemoryMB: 512, CPUs: 1,
		Replicas: 1, ParentID: parent}); err != nil {
		e.t.Fatal(err)
	}
}

// pass evaluates once, waits for deliveries and returns the notices sent.
func (e *evalEnv) pass() []Notice {
	e.t.Helper()
	if err := e.s.Evaluate(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	if !e.s.wait(5 * time.Second) {
		e.t.Fatal("deliveries hang")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	got := e.got
	e.got = nil
	return got
}

func (e *evalEnv) firing() map[string]store.Alert {
	e.t.Helper()
	o, err := e.s.Alerts(e.ctx, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]store.Alert{}
	for _, a := range o.Active {
		out[a.Key] = a
	}
	return out
}

func events(ns []Notice) map[string]string {
	out := map[string]string{}
	for _, n := range ns {
		out[n.Alert.Key] = n.Event + "/" + n.Alert.Severity
	}
	return out
}

func TestSiteDown(t *testing.T) {
	e := newEvalEnv(t)
	e.addSite("s1", "one.test", "")
	e.addSite("s2", "new.test", "")
	e.addSite("s3", "staging.one.test", "s1")
	// new.test has no certificate yet: never reachable, never paged.
	e.probe["new.test"] = site.Health{Detail: "unreachable: tls: internal error"}
	e.probe["staging.one.test"] = site.Health{Checked: true, Detail: "HTTP 500"}

	if got := e.pass(); len(got) != 0 {
		t.Fatalf("first pass notified: %+v", got)
	}
	e.probe["one.test"] = site.Health{Checked: true, Detail: "/: HTTP 502"}
	for i := range 2 {
		e.now = e.now.Add(time.Minute)
		if got := e.pass(); len(got) != 0 {
			t.Fatalf("failure %d notified: %+v", i+1, got)
		}
	}
	e.now = e.now.Add(time.Minute)
	got := e.pass()
	if ev := events(got); len(got) != 1 || ev["site_down:s1"] != "firing/critical" {
		t.Fatalf("third failure: %+v", got)
	}
	if a := e.firing()["site_down:s1"]; a.Message != "one.test is down: /: HTTP 502 (3 failed checks in a row)" || a.SiteID != "s1" {
		t.Errorf("alert: %+v", a)
	}
	// Still down: no repeat until renotify_hours have passed.
	e.now = e.now.Add(time.Minute)
	if got := e.pass(); len(got) != 0 {
		t.Fatalf("repeat: %+v", got)
	}
	e.now = e.now.Add(4 * time.Hour)
	if ev := events(e.pass()); ev["site_down:s1"] != "reminder/critical" {
		t.Fatalf("reminder: %v", ev)
	}
	e.now = e.now.Add(time.Minute)
	if got := e.pass(); len(got) != 0 {
		t.Fatalf("reminder repeated: %+v", got)
	}
	// Back up.
	delete(e.probe, "one.test")
	e.now = e.now.Add(time.Minute)
	if ev := events(e.pass()); ev["site_down:s1"] != "resolved/" {
		t.Fatalf("resolve: %v", ev)
	}
	// A restore is running: the site may be down on purpose.
	jid, _ := e.st.CreateJob(e.ctx, "s1", "restore", "t")
	e.st.StartJob(e.ctx, jid)
	e.probe["one.test"] = site.Health{Checked: true, Detail: "/: HTTP 503"}
	for range 4 {
		e.now = e.now.Add(time.Minute)
		if got := e.pass(); len(got) != 0 {
			t.Fatalf("down during a restore: %+v", got)
		}
	}
	e.st.FinishJob(e.ctx, jid, store.JobSucceeded, "", "")
	// Caddy stops: unreachable now counts, since the site worked before.
	e.probe["one.test"] = site.Health{Detail: "unreachable: connection refused"}
	for range 3 {
		e.now = e.now.Add(time.Minute)
		got = e.pass()
	}
	if ev := events(got); ev["site_down:s1"] != "firing/critical" || len(ev) != 1 {
		t.Fatalf("unreachable after healthy: %v", ev)
	}
	// The site is deleted while down: resolved and forgotten.
	if err := e.st.DeleteSite(e.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Minute)
	got = e.pass()
	if ev := events(got); ev["site_down:s1"] != "resolved/" {
		t.Fatalf("deleted site: %v", ev)
	}
	rows, _ := e.st.Alerts(e.ctx)
	for _, r := range rows {
		if r.SiteID == "s1" || r.SiteID == "s3" || r.Key == "site_down:s2" {
			t.Errorf("left over: %+v", r)
		}
	}
	hist, _ := e.st.AlertHistory(e.ctx, 100)
	// Fired, resolved, fired, resolved (deleted); reminders aren't history.
	if len(hist) != 4 || hist[0].State != store.AlertResolved || hist[3].State != store.AlertFiring {
		t.Errorf("history: %+v", hist)
	}
}

func TestCertificateAndDiskAlerts(t *testing.T) {
	e := newEvalEnv(t)
	e.addSite("s1", "one.test", "")
	if err := e.st.AddDomain(e.ctx, "s1", "www.one.test", true); err != nil {
		t.Fatal(err)
	}
	e.certs["www.one.test"] = CertResult{Served: true, NotAfter: e.now.Add(10 * 24 * time.Hour)}
	e.used = 90
	got := e.pass()
	if ev := events(got); len(ev) != 2 || ev["certificate:www.one.test"] != "firing/warning" || ev["disk:/data"] != "firing/warning" {
		t.Fatalf("first pass: %v", ev)
	}
	e.mu.Lock()
	if e.sent != 1 {
		t.Errorf("%d messages for one pass, want 1", e.sent)
	}
	e.mu.Unlock()
	// Escalation is notified at once.
	e.used = 96
	e.now = e.now.Add(time.Minute)
	if ev := events(e.pass()); len(ev) != 1 || ev["disk:/data"] != "firing/critical" {
		t.Fatalf("escalation: %v", ev)
	}
	// Certificates aren't re-checked every minute; a bad one every ten.
	if e.certN["one.test"] != 1 || e.certN["www.one.test"] != 1 {
		t.Errorf("handshakes: %v", e.certN)
	}
	e.certs["www.one.test"] = CertResult{Served: true, NotAfter: e.now.Add(90 * 24 * time.Hour)}
	e.now = e.now.Add(10 * time.Minute)
	e.used = 40
	got = e.pass()
	if ev := events(got); len(ev) != 2 || ev["certificate:www.one.test"] != "resolved/" || ev["disk:/data"] != "resolved/" {
		t.Fatalf("resolved: %v", ev)
	}
	if e.certN["one.test"] != 1 || e.certN["www.one.test"] != 2 {
		t.Errorf("handshakes: %v", e.certN)
	}
	e.now = e.now.Add(time.Hour)
	e.pass()
	if e.certN["one.test"] != 2 {
		t.Errorf("hourly handshake: %v", e.certN)
	}

	// Metrics show what the evaluator saw.
	body, err := e.s.Collect(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`wpgenie_site_up{site="s1"} 1`,
		`wpgenie_cert_valid{domain="www.one.test",site="s1"} 1`,
		`wpgenie_filesystem_size_bytes{path="/data"} 107374182400`,
		`wpgenie_memory_available_bytes 1073741824`,
		`wpgenie_alerts_firing{severity="critical"} 0`,
	} {
		if !containsLine(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestBackupAlert(t *testing.T) {
	e := newEvalEnv(t)
	e.addSite("s1", "one.test", "")
	if err := e.st.CreateRepo(e.ctx, &store.BackupRepo{ID: "local", Name: "local", Kind: "local", Location: "/b", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetBackupPolicy(e.ctx, &store.BackupPolicy{SiteID: "s1", RepoID: "local", IntervalHours: 24}); err != nil {
		t.Fatal(err)
	}
	e.st.BackupAttempted(e.ctx, "s1", e.now, "")
	if got := e.pass(); len(got) != 0 {
		t.Fatalf("fresh backup: %+v", got)
	}
	e.now = e.now.Add(49 * time.Hour)
	e.st.BackupAttempted(e.ctx, "s1", e.now, "repository locked")
	if a := e.firing()["backup:s1"]; a.Key != "" {
		t.Fatal("fired before evaluation")
	}
	got := e.pass()
	if ev := events(got); ev["backup:s1"] != "firing/warning" {
		t.Fatalf("overdue: %v", ev)
	}
	if a := e.firing()["backup:s1"]; a.Message == "" || a.Target != "one.test" {
		t.Errorf("alert: %+v", a)
	}
}
