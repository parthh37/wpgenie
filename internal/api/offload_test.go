package api

import (
	"context"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/store"
)

// Uploads offload: viewers read, operators change; the secret key is
// write-only.
func TestOffloadRoutes(t *testing.T) {
	e := newAuthEnv(t)
	admin := setupAdmin(t, e)
	var created struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/users", `{"username":"vic","role":"viewer"}`, true, &created)
	viewer := e.browser()
	if c := viewer.do("POST", "/api/v1/auth/login", `{"username":"vic","password":"`+created.Password+`"}`, true, nil); c != 200 {
		t.Fatalf("viewer login: %d", c)
	}
	ctx := context.Background()
	e.store.CreateSite(ctx, &store.Site{ID: "slive", Name: "live", PrimaryDomain: "live.test", PHPVersion: "8.3",
		FPMPort: 19000, DBName: "wp_slive", Status: store.StatusActive, MemoryMB: 512, CPUs: 1, Replicas: 1})
	const secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	e.store.SetOffload(ctx, &store.Offload{SiteID: "slive", Endpoint: "https://s3.example.com", Bucket: "media",
		Prefix: "slive/uploads/", AccessKeyID: "AKIAEXAMPLE123", SecretKey: secret, PublicURL: "https://media.example.com"}, true)

	var status map[string]any
	if c := viewer.do("GET", "/api/v1/sites/slive/offload", "", false, &status); c != 200 || status["enabled"] != true ||
		status["secret_set"] != true {
		t.Fatalf("status: %d %v", c, status)
	}
	for k, v := range status {
		if s, ok := v.(string); ok && (strings.Contains(s, secret) || strings.Contains(s, "AKIAEXAMPLE123")) {
			t.Errorf("%s leaks a key: %q", k, s)
		}
	}
	for _, c := range []struct {
		who          *browser
		method, path string
		body         string
		want         int
	}{
		{viewer, "PUT", "/api/v1/sites/slive/offload", `{"enabled":false}`, 403},
		{viewer, "POST", "/api/v1/sites/slive/offload/sync", "", 403},
		{viewer, "POST", "/api/v1/sites/slive/offload/download", "", 403},
		{admin, "GET", "/api/v1/sites/nope/offload", "", 404},
		{admin, "PUT", "/api/v1/sites/slive/offload", `{"enabled":true,"secret":"x"}`, 400}, // unknown field
		// No storage client in this server: refused before anything else.
		{admin, "PUT", "/api/v1/sites/slive/offload", `{"enabled":true,"endpoint":"https://s3.example.com"}`, 400},
		{admin, "POST", "/api/v1/sites/slive/offload/sync", "", 400},
		{admin, "POST", "/api/v1/sites/slive/offload/download", "", 400},
	} {
		if got := c.who.do(c.method, c.path, c.body, true, nil); got != c.want {
			t.Errorf("%s %s %s: %d, want %d", c.method, c.path, c.body, got, c.want)
		}
	}
	if o, err := e.store.GetOffload(ctx, "slive"); err != nil || o.SecretKey != secret {
		t.Errorf("refused changes touched the settings: %v", err)
	}
}
