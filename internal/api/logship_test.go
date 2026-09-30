package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/logship"
	"github.com/parthh37/wpgenie/internal/offload"
)

// logDocker has no Docker: every command fails (the shipper is "not
// running"; the API doesn't need it).
type logDocker struct{}

func (logDocker) Run(context.Context, io.Reader, ...string) ([]byte, error) {
	return nil, errors.New("docker: not here")
}

// logBucket is a bucket in memory.
type logBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	refuse  bool
}

func (b *logBucket) Put(_ context.Context, t offload.Target, name string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refuse {
		return errors.New("rclone rcat: 403 AccessDenied: Access Denied.")
	}
	b.objects[t.Prefix+name] = data
	return nil
}

func (b *logBucket) DeleteFile(_ context.Context, t offload.Target, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, t.Prefix+name)
	return nil
}

func (b *logBucket) List(_ context.Context, t offload.Target) (map[string]offload.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]offload.Object{}
	for k, v := range b.objects {
		if rel, ok := strings.CutPrefix(k, t.Prefix); ok {
			out[rel] = offload.Object{Size: int64(len(v))}
		}
	}
	return out, nil
}

func (b *logBucket) Download(_ context.Context, t offload.Target, paths []string, dir string) (offload.Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range paths {
		if v, ok := b.objects[t.Prefix+p]; ok {
			os.WriteFile(filepath.Join(dir, p), v, 0o600)
		}
	}
	return offload.Stats{}, nil
}

func (b *logBucket) Delete(_ context.Context, t offload.Target, paths []string) (offload.Stats, error) {
	return offload.Stats{}, nil
}

// withLogship gives a server log shipping (on fakes) and rebuilds its
// handler.
func withLogship(t *testing.T, srv **httptest.Server, api *Server) *logBucket {
	t.Helper()
	bucket := &logBucket{objects: map[string][]byte{}}
	ls := &logship.Service{Store: api.Store, Docker: logDocker{}, Rclone: bucket, Log: slog.New(slog.DiscardHandler),
		Cfg:    logship.Config{Dir: t.TempDir(), Image: "timberio/vector:0.58.0-alpine", AccessLog: "/var/log/wpgenie/access.log"},
		Server: func() string { return "panel" }, Panel: true}
	if err := ls.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.Logship = ls
	(*srv).Close()
	*srv = httptest.NewServer(api.Handler())
	t.Cleanup((*srv).Close)
	return bucket
}

const logSettingsBody = `{"enabled":true,"destination":{"provider":"minio","endpoint":"https://minio.example.com:9000",
	"region":"","bucket":"logs","prefix":"wpg/","access_key_id":"AKIAEXAMPLE","secret_key":"very-secret-key","path_style":true},
	"types":{"containers":false},"compression":"gzip","batch_max_mb":10,"batch_max_seconds":300,"spool_cap_mb":1024,
	"archive_retention_days":30,"local_access_logs":2,"container_log_mb":10}`

func TestLogshipRoutes(t *testing.T) {
	e := newAuthEnv(t)
	bucket := withLogship(t, &e.srv, e.api)
	admin := setupAdmin(t, e)
	var created struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/users", `{"username":"vic","role":"viewer"}`, true, &created)
	viewer := e.browser()
	if c := viewer.do("POST", "/api/v1/auth/login", `{"username":"vic","password":"`+created.Password+`"}`, true, nil); c != 200 {
		t.Fatalf("viewer login: %d", c)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(`{"request":{"host":"a.test"},"status":200}` + "\n"))
	zw.Close()
	key := "wpg/panel/access/2026/09/30/14-abc.log.gz"
	bucket.objects[key] = buf.Bytes()
	bucket.objects["wpg/private/notes.txt"] = []byte("not a log")

	for _, c := range []struct {
		who          *browser
		method, path string
		body         string
		want         int
	}{
		{viewer, "GET", "/api/v1/logs/status", "", 200},
		{viewer, "GET", "/api/v1/logs/settings", "", 403},
		{viewer, "PUT", "/api/v1/logs/settings", logSettingsBody, 403},
		{viewer, "POST", "/api/v1/logs/test", `{}`, 403},
		{viewer, "GET", "/api/v1/logs/archives?type=access", "", 403},
		{viewer, "GET", "/api/v1/logs/archives/object?key=" + url.QueryEscape(key), "", 403},
		{admin, "GET", "/api/v1/logs/archives?type=access", "", 400}, // no destination yet
		{admin, "PUT", "/api/v1/logs/settings", `{"enabled":true}`, 400},
		{admin, "PUT", "/api/v1/logs/settings", `{"nonsense":1}`, 400},
		{admin, "PUT", "/api/v1/logs/settings", logSettingsBody, 200},
		{admin, "GET", "/api/v1/logs/archives?type=access&date=2026-09-30", "", 200},
		{admin, "GET", "/api/v1/logs/archives?type=kernel", "", 400},
		{admin, "GET", "/api/v1/logs/archives?type=access&date=yesterday", "", 400},
		{admin, "GET", "/api/v1/logs/archives?type=access&server=../x", "", 400},
		{admin, "GET", "/api/v1/logs/archives/object?key=" + url.QueryEscape("wpg/private/notes.txt"), "", 400},
		{admin, "GET", "/api/v1/logs/archives/object?key=" + url.QueryEscape("wpg/panel/access/2026/09/30/../../x"), "", 400},
		{admin, "GET", "/api/v1/logs/archives/object?key=" + url.QueryEscape("wpg/panel/access/2026/09/30/15-gone.log.gz"), "", 404},
	} {
		if got := c.who.do(c.method, c.path, c.body, true, nil); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}

	// The secret never comes back.
	resp, err := admin.client.Get(e.srv.URL + "/api/v1/logs/settings")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "very-secret-key") || !strings.Contains(string(body), `"secret_key_set":true`) ||
		!strings.Contains(string(body), `"server":"panel"`) {
		t.Errorf("settings: %s", body)
	}

	var list struct {
		Objects []logship.Archive `json:"objects"`
		Servers []string          `json:"servers"`
	}
	admin.do("GET", "/api/v1/logs/archives?type=access&date=2026-09-30", "", true, &list)
	if len(list.Objects) != 1 || list.Objects[0].Key != key || len(list.Servers) != 1 {
		t.Errorf("archives: %+v", list)
	}
	// Viewed: decompressed text.
	resp, err = admin.client.Get(e.srv.URL + "/api/v1/logs/archives/object?key=" + url.QueryEscape(key))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"request":{"host":"a.test"},"status":200}`+"\n" ||
		resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Content-Disposition") != "" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("object: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	// Downloaded: the object as it is in the bucket.
	resp, err = admin.client.Get(e.srv.URL + "/api/v1/logs/archives/object?key=" + url.QueryEscape(key) + "&download=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, bucket.objects[key]) || resp.Header.Get("Content-Type") != "application/gzip" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="14-abc.log.gz"` {
		t.Errorf("download: %d %d bytes %v", resp.StatusCode, len(body), resp.Header)
	}

	// Test connection: the storage's words, plainly.
	var res logship.TestResult
	if c := admin.do("POST", "/api/v1/logs/test", `{"provider":"minio","endpoint":"https://minio.example.com:9000","bucket":"logs",
		"prefix":"wpg/","access_key_id":"AKIAEXAMPLE"}`, true, &res); c != 200 || !res.OK {
		t.Errorf("test with the stored secret: %d %+v", c, res)
	}
	bucket.refuse = true
	if c := admin.do("POST", "/api/v1/logs/test", `{"provider":"minio","endpoint":"https://minio.example.com:9000","bucket":"logs",
		"access_key_id":"AKIAEXAMPLE"}`, true, &res); c != 200 || res.OK || !strings.Contains(res.Error, "403 AccessDenied") {
		t.Errorf("refused: %d %+v", c, res)
	}
	if c := admin.do("POST", "/api/v1/logs/test", `{"provider":"minio","endpoint":"https://elsewhere.example.net","bucket":"logs",
		"access_key_id":"AKIAEXAMPLE"}`, true, nil); c != 400 {
		t.Errorf("stored secret for another endpoint: %d", c)
	}

	var st struct {
		Enabled bool   `json:"enabled"`
		Health  string `json:"health"`
		Types   []struct {
			Name string `json:"name"`
		} `json:"types"`
	}
	if c := viewer.do("GET", "/api/v1/logs/status", "", true, &st); c != 200 || !st.Enabled || len(st.Types) != len(logship.Types) {
		t.Errorf("status: %d %+v", c, st)
	}
}

// Log shipping is staff's: every route is closed to tenants.
func TestLogshipClosedToTenants(t *testing.T) {
	e := newTenancyEnv(t)
	withLogship(t, &e.srv, e.api)
	n := 0
	for _, rt := range e.api.routes {
		if !strings.HasPrefix(strings.SplitN(rt.Pattern, " ", 2)[1], "/api/v1/logs") {
			continue
		}
		n++
		if _, open := tenantRoutes[rt.Pattern]; open {
			t.Errorf("%s is open to tenants", rt.Pattern)
		}
		method, path, _ := strings.Cut(rt.Pattern, " ")
		for _, who := range []string{"alice", "rita", "session:carl"} {
			if got := e.as(who, method, path, "{}", nil); got != http.StatusForbidden {
				t.Errorf("%s: %s = %d", who, rt.Pattern, got)
			}
		}
	}
	if n != 6 {
		t.Errorf("%d log routes registered", n)
	}
}

// On a cluster, saved settings reach every server (the secret included);
// one that missed them gets them on the next sync.
func TestLogshipClusterSettings(t *testing.T) {
	ctx := context.Background()
	panel := newClusterServer(t, false)
	node := newClusterServer(t, true)
	for _, srv := range []*server{panel, node} {
		ls := &logship.Service{Store: srv.st, Docker: logDocker{}, Rclone: &logBucket{objects: map[string][]byte{}},
			Log: slog.New(slog.DiscardHandler), Cfg: logship.Config{Dir: t.TempDir(), Image: "vector"}}
		if err := ls.Load(ctx); err != nil {
			t.Fatal(err)
		}
		srv.api.Logship = ls
		srv.h = srv.api.Handler()
		if srv == node {
			node.agent.API = node.h
		}
	}
	code, err := node.agent.PairingCode()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(cluster.AddNodeInput{Name: "Web 2", Address: node.agent.Listen, PairingCode: code})
	if st := panel.do(t, "POST", "/api/v1/nodes", string(body), nil); st != 201 {
		t.Fatalf("add node: %d", st)
	}
	if st := panel.do(t, "PUT", "/api/v1/logs/settings", logSettingsBody, nil); st != 200 {
		t.Fatalf("put: %d", st)
	}
	got, _ := node.api.Logship.Settings(ctx)
	if !got.Enabled || got.Destination.SecretKey != "very-secret-key" || got.ArchiveRetentionDays != 30 {
		t.Fatalf("node settings: %+v", got)
	}
	want, _ := panel.api.Logship.Settings(ctx)
	if panel.api.Logship.NodeVersion("web-2") != want.Version() || got.Version() != want.Version() {
		t.Error("the node's version isn't recorded")
	}

	// A change the node missed (as if it had been unreachable): the next
	// sync sends it, and the one after sends nothing.
	want.ArchiveRetentionDays = 365
	if _, err := panel.api.Logship.SetSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := node.api.Logship.Settings(ctx); got.ArchiveRetentionDays != 30 {
		t.Fatal("changed without a push")
	}
	if err := panel.api.SyncNodeLogs(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := node.api.Logship.Settings(ctx); got.ArchiveRetentionDays != 365 {
		t.Errorf("not resent: %d", got.ArchiveRetentionDays)
	}
	node.api.Logship.SetSettings(ctx, func() logship.Settings { s, _ := node.api.Logship.Settings(ctx); s.BatchMaxSeconds = 999; return s }())
	panel.api.SyncNodeLogs(ctx)
	if got, _ := node.api.Logship.Settings(ctx); got.BatchMaxSeconds != 999 {
		t.Error("sent again though the node had the version")
	}
}
