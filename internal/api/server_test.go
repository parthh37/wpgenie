package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
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
	// Building the handler also catches ServeMux pattern conflicts (they panic).
	return (&Server{Token: "tok", Sites: svc, Store: st, Shield: sh, Log: slog.Default()}).Handler()
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
