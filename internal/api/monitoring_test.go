package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/monitor"
)

func TestMonitoringRoutes(t *testing.T) {
	e := newAuthEnv(t)
	e.api.Monitor = &monitor.Service{Store: e.store, Log: slog.New(slog.DiscardHandler), Version: "v0"}
	e.srv.Close()
	e.srv = httptest.NewServer(e.api.Handler())
	t.Cleanup(e.srv.Close)

	admin := setupAdmin(t, e)
	var created struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/users", `{"username":"vic","role":"viewer"}`, true, &created)
	viewer := e.browser()
	if c := viewer.do("POST", "/api/v1/auth/login", `{"username":"vic","password":"`+created.Password+`"}`, true, nil); c != 200 {
		t.Fatalf("viewer login: %d", c)
	}

	settings := `{"disk_warn_percent":80,"disk_critical_percent":90,"cert_warn_days":21,"cert_critical_days":5,
		"down_after":3,"renotify_hours":6,"email":{"enabled":false},
		"webhooks":[{"name":"chat","enabled":true,"url":"https://chat.example.com/hooks/very-secret"}]}`
	for _, c := range []struct {
		who          *browser
		method, path string
		body         string
		want         int
	}{
		{viewer, "GET", "/api/v1/monitoring/alerts", "", 200},
		{viewer, "GET", "/api/v1/monitoring/alerts?limit=0", "", 400},
		{viewer, "GET", "/api/v1/monitoring/settings", "", 403},
		{viewer, "PUT", "/api/v1/monitoring/settings", settings, 403},
		{viewer, "POST", "/api/v1/monitoring/test", "", 403},
		{viewer, "POST", "/api/v1/monitoring/metrics-token", "", 403},
		{admin, "PUT", "/api/v1/monitoring/settings", `{"disk_warn_percent":99,"disk_critical_percent":90}`, 400},
		{admin, "PUT", "/api/v1/monitoring/settings", `{"nonsense":1}`, 400},
		{admin, "POST", "/api/v1/monitoring/test", "", 200},
	} {
		if got := c.who.do(c.method, c.path, c.body, true, nil); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}

	var put struct {
		Webhooks []struct {
			ID      string `json:"id"`
			URLHint string `json:"url_hint"`
		} `json:"webhooks"`
		GeneratedSecrets map[string]string `json:"generated_secrets"`
	}
	if c := admin.do("PUT", "/api/v1/monitoring/settings", settings, true, &put); c != 200 || len(put.Webhooks) != 1 ||
		len(put.GeneratedSecrets[put.Webhooks[0].ID]) != 64 {
		t.Fatalf("put settings: %d %+v", c, put)
	}
	resp, err := admin.client.Get(e.srv.URL + "/api/v1/monitoring/settings")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "very-secret") || strings.Contains(string(body), put.GeneratedSecrets[put.Webhooks[0].ID]) ||
		!strings.Contains(string(body), `"url_hint":"https://chat.example.com/…"`) {
		t.Errorf("settings leak secrets: %s", body)
	}

	// The scrape token: shown once, and the only key to /metrics.
	var tok struct{ Token string }
	if c := admin.do("POST", "/api/v1/monitoring/metrics-token", "", true, &tok); c != 200 || tok.Token == "" {
		t.Fatalf("token: %d", c)
	}
	scrape := func(auth string) int {
		req, _ := http.NewRequest("GET", e.srv.URL+"/metrics", nil)
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if scrape("Bearer "+tok.Token) != 200 || scrape("Bearer tok") != 401 {
		t.Error("/metrics must take the scrape token and only it")
	}
	// A session cookie (even an admin's) isn't a scrape token either.
	if resp, _ := admin.client.Get(e.srv.URL + "/metrics"); resp.StatusCode != 401 {
		t.Errorf("/metrics with a session: %d", resp.StatusCode)
	}

	var view struct {
		MetricsToken monitor.TokenInfo `json:"metrics_token"`
	}
	admin.do("GET", "/api/v1/monitoring/settings", "", false, &view)
	if !view.MetricsToken.Set {
		t.Error("token not reported as set")
	}
	var o monitor.Overview
	viewer.do("GET", "/api/v1/monitoring/alerts", "", false, &o)
	if o.Active == nil || o.History == nil {
		t.Errorf("alerts: %+v", o)
	}
	b, _ := json.Marshal(o)
	if !strings.Contains(string(b), `"active":[]`) {
		t.Errorf("alerts JSON: %s", b)
	}
}
