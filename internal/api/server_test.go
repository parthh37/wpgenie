package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/updater"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	svc := &site.Service{Cfg: cfg, Store: st, Log: slog.Default()}
	sh := shield.New(shield.Options{Secret: []byte("k"), Sites: svc.ShieldLookup})
	ml := &mail.Service{Cfg: mail.Config{DataDir: t.TempDir()}, Store: st, Log: slog.Default()}
	if err := ml.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	upd := &updater.Updater{Current: "v0.1.0", Repo: "o/r", StateDir: t.TempDir(), APIBase: "http://127.0.0.1:1"}
	// Building the handler also catches ServeMux pattern conflicts (they panic).
	return (&Server{Token: "tok", Version: "v0.1.0", Sites: svc, Store: st, Shield: sh, Updater: upd, Mail: ml, Log: slog.Default()}).Handler()
}

func TestRoutesAndAuth(t *testing.T) {
	h := newTestServer(t)
	cases := []struct {
		method, path, token, body string
		want                      int
	}{
		{"GET", "/healthz", "", "", 200},
		{"GET", "/", "", "", 200},
		{"GET", "/api/v1/sites", "", "", 401},
		{"GET", "/api/v1/sites", "wrong", "", 401},
		{"GET", "/api/v1/sites", "tok", "", 200},
		{"GET", "/api/v1/sites/nope", "tok", "", 404},
		{"POST", "/api/v1/sites", "tok", `{"domain":"not a domain","admin_email":"a@b.co"}`, 400},
		{"POST", "/api/v1/sites", "tok", `{"domain":"a.com","evil":1}`, 400},
		{"GET", "/api/v1/sites/x/stats?hours=abc", "tok", "", 400},
		{"PUT", "/api/v1/sites/x/resources", "", `{"memory_mb":1024,"cpus":1,"replicas":2}`, 401},
		{"PUT", "/api/v1/sites/x/resources", "tok", `{"memory_mb":64,"cpus":1,"replicas":1}`, 400},
		{"PUT", "/api/v1/sites/x/resources", "tok", `{"memory_mb":1024,"cpus":1,"replicas":0}`, 400},
		{"PUT", "/api/v1/sites/x/resources", "tok", `{"memory_mb":1024,"cpus":1,"replicas":2}`, 404},
		{"PUT", "/api/v1/sites/x/cache", "tok", `{"page_cache":true,"object_cache":true}`, 404},
		{"PUT", "/api/v1/sites/x/cache", "tok", `{"page_cache":"yes"}`, 400},
		{"POST", "/api/v1/sites/x/cache/purge", "", "", 401},
		{"POST", "/api/v1/sites/x/cache/purge", "tok", "", 404},
		{"PUT", "/api/v1/sites/x/autoscale", "tok", `{"enabled":true,"min_replicas":1,"max_replicas":4,"target_cpu":70}`, 404},
		{"PUT", "/api/v1/sites/x/autoscale", "tok", `{"enabled":true,"min_replicas":3,"max_replicas":2,"target_cpu":70}`, 400},
		{"GET", "/api/v1/sites/x/events", "tok", "", 404},
		{"GET", "/api/v1/sites/x/events?limit=-1", "tok", "", 400},
		{"GET", "/api/v1/sites/x/metrics", "tok", "", 404},
		{"PUT", "/api/v1/sites/x/shield", "tok", `{"mode":"standard","block_ai_bots":true,"admin_allow":["not-an-ip"]}`, 404},
		{"GET", "/api/v1/sites/x/updates", "tok", "", 404},
		{"POST", "/api/v1/sites/x/updates", "tok", `{"all":true}`, 404},
		{"POST", "/api/v1/sites/x/updates", "tok", `{}`, 400},
		{"GET", "/api/v1/sites/x/updates/history", "tok", "", 404},
		{"PUT", "/api/v1/sites/x/auto-update", "tok", `{"policy":"sometimes"}`, 400},
		{"POST", "/api/v1/sites/x/scan", "tok", "", 404},
		{"GET", "/api/v1/sites/x/scan", "tok", "", 404},
		{"GET", "/api/v1/mail", "tok", "", 200},
		{"PUT", "/api/v1/mail", "tok", `{"enabled":true,"hostname":"not a host"}`, 400},
		{"POST", "/api/v1/mail/mailboxes", "tok", `{"address":"a@example.com"}`, 409}, // mail is off
		{"POST", "/api/v1/mail/domains", "", `{"domain":"example.com"}`, 401},
		{"GET", "/api/v1/mail/domains/nope.test", "tok", "", 404},
		{"PUT", "/api/v1/sites/x/smtp", "tok", `{"enabled":true}`, 404},
		{"GET", "/api/v1/system", "", "", 401},
		{"GET", "/api/v1/system", "tok", "", 200},
		{"GET", "/api/v1/system/version", "tok", "", 200},
		{"POST", "/api/v1/system/update", "", "", 401},
		{"GET", "/api/v1/security/bans", "", "", 401},
		{"GET", "/api/v1/security/bans", "tok", "", 200},
		{"POST", "/api/v1/security/bans", "tok", `{"addr":"nonsense"}`, 400},
		{"POST", "/api/v1/security/bans", "tok", `{"addr":"203.0.113.9","hours":2}`, 201},
		{"DELETE", "/api/v1/security/bans?addr=203.0.113.9", "tok", "", 204},
		{"DELETE", "/api/v1/security/bans?addr=203.0.113.9", "tok", "", 404},
		{"GET", "/api/v1/security/events?limit=5", "tok", "", 200},
		{"GET", "/_shield/check", "", "", 200}, // unknown site fails open
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d (%s)", c.method, c.path, rec.Code, c.want, rec.Body)
		}
	}
}
