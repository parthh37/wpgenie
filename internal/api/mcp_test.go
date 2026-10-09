package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

// noRedirects is a client that shows redirects instead of following them.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *tenancyEnv) postForm(path string, form url.Values) (int, map[string]any) {
	e.t.Helper()
	resp, err := http.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *tenancyEnv) register(redirect string) string {
	e.t.Helper()
	resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+redirect+`"]}`))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("register: %d %v", resp.StatusCode, out)
	}
	return out["client_id"].(string)
}

func pkce() (verifier, challenge string) {
	verifier = auth.RandomToken(48)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// connect runs the whole flow for a user: authorize, approve in their
// session, exchange. It returns the access and refresh tokens.
func (e *tenancyEnv) connect(user, clientID, redirect string) (string, string) {
	e.t.Helper()
	verifier, challenge := pkce()
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"xyz"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"wpgenie"},
		"resource": {"https://panel.test/mcp"}}
	resp, err := noRedirects.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "/#/connect?") {
		e.t.Fatalf("authorize: %d %q", resp.StatusCode, loc)
	}
	params, _ := url.ParseQuery(strings.TrimPrefix(loc, "/#/connect?"))
	var info map[string]any
	if c := e.as("session:"+user, "GET", "/api/v1/oauth/authorize?"+params.Encode(), "", &info); c != 200 {
		e.t.Fatalf("consent info: %d %v", c, info)
	}
	if info["client_name"] != "Claude" || info["redirect_host"] != mustHost(redirect) || info["user"] != user {
		e.t.Fatalf("consent info: %v", info)
	}
	body := map[string]any{"approve": true}
	for k := range params {
		body[k] = params.Get(k)
	}
	b, _ := json.Marshal(body)
	var out map[string]string
	if c := e.as("session:"+user, "POST", "/api/v1/oauth/authorize", string(b), &out); c != 200 {
		e.t.Fatalf("approve: %d %v", c, out)
	}
	back, _ := url.Parse(out["redirect"])
	if !strings.HasPrefix(out["redirect"], redirect) || back.Query().Get("state") != "xyz" ||
		back.Query().Get("iss") != "https://panel.test" {
		e.t.Fatalf("redirect back: %q", out["redirect"])
	}
	code := back.Query().Get("code")
	c, tok := e.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {clientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
	if c != 200 || tok["token_type"] != "Bearer" {
		e.t.Fatalf("token: %d %v", c, tok)
	}
	// A code works once.
	if c, _ := e.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {clientID}, "code_verifier": {verifier}}); c != 400 {
		e.t.Errorf("code reused: %d", c)
	}
	return tok["access_token"].(string), tok["refresh_token"].(string)
}

func mustHost(raw string) string {
	u, _ := url.Parse(raw)
	return u.Host
}

// mcp sends one JSON-RPC request to /mcp with a bearer token.
func (e *tenancyEnv) mcp(bearer, method string, params any) (int, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// call runs a tool: its text and whether it is an error.
func (e *tenancyEnv) call(bearer, tool string, args map[string]any) (string, bool) {
	e.t.Helper()
	c, out := e.mcp(bearer, "tools/call", map[string]any{"name": tool, "arguments": args})
	if c != 200 {
		e.t.Fatalf("tools/call %s: %d %v", tool, c, out)
	}
	if out["error"] != nil {
		return out["error"].(map[string]any)["message"].(string), true
	}
	res := out["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
}

func toolNames(e *tenancyEnv, bearer string) []string {
	e.t.Helper()
	_, out := e.mcp(bearer, "tools/list", nil)
	var names []string
	for _, t := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	return names
}

func TestOAuthConnectsAnAssistantAsItsUser(t *testing.T) {
	e := newTenancyEnv(t)
	// Discovery.
	var prm, asm map[string]any
	for path, out := range map[string]*map[string]any{"/.well-known/oauth-protected-resource/mcp": &prm,
		"/.well-known/oauth-authorization-server": &asm} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
	}
	if prm["resource"] != "https://panel.test/mcp" || asm["token_endpoint"] != "https://panel.test/oauth/token" {
		t.Fatalf("metadata: %v %v", prm, asm)
	}
	// Unauthenticated, the MCP endpoint says where to look.
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"),
		`resource_metadata="https://panel.test/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("unauthenticated /mcp: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	const redirect = "https://claude.ai/api/mcp/auth_callback"
	client := e.register(redirect)
	access, refresh := e.connect("alice", client, redirect)

	c, init := e.mcp(access, "initialize", map[string]any{"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	if c != 200 || init["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", c, init)
	}
	text, isErr := e.call(access, "list_sites", nil)
	if isErr || !strings.Contains(text, `"sa"`) || strings.Contains(text, `"sb"`) || strings.Contains(text, `"sx"`) {
		t.Fatalf("alice's assistant lists: %s", text)
	}
	// Another account's site doesn't exist for it.
	if text, isErr := e.call(access, "get_site", map[string]any{"site_id": "sb"}); !isErr || !strings.Contains(text, "404") {
		t.Errorf("reached bob's site: %s", text)
	}
	// The grant shows up among alice's tokens, named after the assistant.
	var mine []store.APIToken
	e.as("session:alice", "GET", "/api/v1/account/tokens", "", &mine)
	if i := slices.IndexFunc(mine, func(t store.APIToken) bool { return t.ClientID == client }); i < 0 || mine[i].Name != "Claude" {
		t.Errorf("tokens: %+v", mine)
	}

	// Refreshing rotates both tokens; the old ones stop working.
	c, tok := e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {client}})
	if c != 200 {
		t.Fatalf("refresh: %d %v", c, tok)
	}
	if c, _ := e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {client}}); c != 400 {
		t.Errorf("refresh token reused: %d", c)
	}
	if c, _ := e.mcp(access, "tools/list", nil); c != 401 {
		t.Errorf("old access token still works: %d", c)
	}
	access = tok["access_token"].(string)
	if c, _ := e.mcp(access, "tools/list", nil); c != 200 {
		t.Errorf("new access token: %d", c)
	}
	// Access tokens expire within the hour.
	e.now = e.now.Add(oauthAccessTTL + time.Second)
	if c, _ := e.mcp(access, "tools/list", nil); c != 401 {
		t.Errorf("expired access token: %d", c)
	}
	e.now = e.now.Add(-oauthAccessTTL - time.Second)

	// Revocation disconnects it.
	if c, _ := e.postForm("/oauth/revoke", url.Values{"token": {access}, "client_id": {client}}); c != 200 {
		t.Fatalf("revoke: %d", c)
	}
	if c, _ := e.mcp(access, "tools/list", nil); c != 401 {
		t.Errorf("revoked token works: %d", c)
	}
}

func TestOAuthRefusesBadRequests(t *testing.T) {
	e := newTenancyEnv(t)
	const redirect = "http://127.0.0.1:33418/callback" // Claude Code on the user's machine
	client := e.register(redirect)

	// Return addresses: https, or http to this machine only.
	for _, bad := range []string{"http://evil.example/cb", "javascript:alert(1)", "https://x.test/cb#frag", "ftp://x/cb"} {
		resp, _ := http.Post(e.srv.URL+"/oauth/register", "application/json",
			strings.NewReader(`{"redirect_uris":["`+bad+`"]}`))
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("registered %s: %d", bad, resp.StatusCode)
		}
	}
	// A return address the client didn't register: told on the page, never
	// redirected there.
	_, challenge := pkce()
	q := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {"https://evil.example/cb"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	resp, _ := noRedirects.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	resp.Body.Close()
	if resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Errorf("unregistered redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Without PKCE: an error sent back to the (registered) client.
	q.Set("redirect_uri", redirect)
	q.Del("code_challenge")
	resp, _ = noRedirects.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != 302 || !strings.Contains(loc, "error=invalid_request") {
		t.Errorf("no PKCE: %d %q", resp.StatusCode, loc)
	}
	// A different resource than this server's MCP endpoint.
	q.Set("code_challenge", challenge)
	q.Set("resource", "https://other.example/mcp")
	resp, _ = noRedirects.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=invalid_target") {
		t.Errorf("foreign resource: %q", loc)
	}

	// Approving takes a signed-in session, not an API token.
	body := `{"approve":true,"response_type":"code","client_id":"` + client + `","redirect_uri":"` + redirect +
		`","code_challenge":"` + challenge + `","code_challenge_method":"S256"}`
	if c := e.as("alice", "POST", "/api/v1/oauth/authorize", body, nil); c != 403 {
		t.Errorf("approved with an API token: %d", c)
	}
	// Declining sends the client access_denied.
	var out map[string]string
	if c := e.as("session:alice", "POST", "/api/v1/oauth/authorize",
		strings.Replace(body, `"approve":true`, `"approve":false`, 1), &out); c != 200 ||
		!strings.Contains(out["redirect"], "error=access_denied") || strings.Contains(out["redirect"], "code=") {
		t.Errorf("decline: %d %v", c, out)
	}

	// The wrong verifier, or another client, can't use a code.
	verifier, challenge := pkce()
	body = strings.Replace(body, `"approve":false`, `"approve":true`, 1)
	body = strings.Replace(body, `"code_challenge":"`+challengeOf(body)+`"`, `"code_challenge":"`+challenge+`"`, 1)
	e.as("session:alice", "POST", "/api/v1/oauth/authorize", body, &out)
	back, _ := url.Parse(out["redirect"])
	code := back.Query().Get("code")
	other := e.register(redirect)
	if c, _ := e.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {other}, "code_verifier": {verifier}}); c != 400 {
		t.Errorf("another client used the code: %d", c)
	}
	e.as("session:alice", "POST", "/api/v1/oauth/authorize", body, &out)
	back, _ = url.Parse(out["redirect"])
	if c, _ := e.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"},
		"code": {back.Query().Get("code")}, "client_id": {client}, "code_verifier": {pkceVerifierWrong()}}); c != 400 {
		t.Errorf("wrong verifier accepted: %d", c)
	}

	// A confidential client must present its secret.
	r2, _ := http.Post(e.srv.URL+"/oauth/register", "application/json", strings.NewReader(
		`{"redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"client_secret_post"}`))
	var reg map[string]any
	json.NewDecoder(r2.Body).Decode(&reg)
	r2.Body.Close()
	if reg["client_secret"] == nil {
		t.Fatalf("no secret issued: %v", reg)
	}
	if c, _ := e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"},
		"client_id": {reg["client_id"].(string)}}); c != 401 {
		t.Errorf("confidential client without its secret: %d", c)
	}
}

func challengeOf(body string) string {
	var m map[string]any
	json.Unmarshal([]byte(body), &m)
	return m["code_challenge"].(string)
}

func pkceVerifierWrong() string { v, _ := pkce(); return v }

func TestOAuthHonoursTwoFactorRequirement(t *testing.T) {
	e := newTenancyEnv(t)
	const redirect = "https://chatgpt.com/connector_platform_oauth_redirect"
	client := e.register(redirect)
	_, refresh := e.connect("alice", client, redirect)
	if err := e.st.SetSetting(context.Background(), settingRequire2FA, "1"); err != nil {
		t.Fatal(err)
	}
	// Without two-factor authentication, the assistant can't renew access.
	if c, _ := e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {client}}); c != 400 {
		t.Errorf("refreshed without 2FA: %d", c)
	}
}

func TestMCPRefusesCookiesAndForeignOrigins(t *testing.T) {
	e := newTenancyEnv(t)
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.sessionFor["alice"]})
	req.Header.Set(csrfHeader, csrfValue)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("session cookie accepted on /mcp: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+e.tokens["alice"])
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign origin: %d", resp.StatusCode)
	}
}

func TestMCPToolsFollowRoles(t *testing.T) {
	e := newTenancyEnv(t)
	ctx := context.Background()
	viewer, err := e.st.CreateUser(ctx, "vera", "x", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	e.user["vera"] = viewer
	e.tokens["vera"] = e.token("vera")

	staff := toolNames(e, "tok")
	if len(staff) != len(mcpTools) {
		t.Errorf("the installer token gets %d of %d tools", len(staff), len(mcpTools))
	}
	// A viewer only looks.
	for _, n := range toolNames(e, e.tokens["vera"]) {
		i := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == n })
		if !mcpTools[i].ReadOnly {
			t.Errorf("viewer offered %s", n)
		}
	}
	if text, isErr := e.call(e.tokens["vera"], "purge_cache", map[string]any{"site_id": "sx"}); !isErr {
		t.Errorf("viewer purged: %s", text)
	}
	// Tenants get the tools their routes allow, none of the staff's.
	alice := toolNames(e, e.tokens["alice"])
	for _, n := range alice {
		i := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == n })
		if _, ok := tenantRoutes[mcpTools[i].Pattern]; !ok {
			t.Errorf("tenant offered staff-only %s", n)
		}
	}
	if !slices.Contains(alice, "list_sites") || !slices.Contains(alice, "get_job") {
		t.Errorf("tenant tools: %v", alice)
	}
}

func TestMCPToolArguments(t *testing.T) {
	e := newTenancyEnv(t)
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_site", map[string]any{}, "site_id is required"},
		{"get_site", map[string]any{"site_id": "../users"}, "not a valid ID"},
		{"get_site", map[string]any{"site_id": "sa%2F..%2Fusers"}, "not a valid ID"},
		{"get_site", map[string]any{"site_id": 7}, "must be a string"},
		{"get_site", map[string]any{"site_id": "sx", "x": 1}, `unknown argument "x"`},
		{"set_auto_updates", map[string]any{"site_id": "sx", "policy": "sometimes"}, "must be one of"},
		{"site_traffic", map[string]any{"site_id": "sx", "hours": 1.5}, "whole number"},
		{"delete_site", map[string]any{"site_id": "sx"}, "unknown tool"},
	} {
		text, isErr := e.call("tok", c.tool, c.args)
		if !isErr || !strings.Contains(text, c.want) {
			t.Errorf("%s %v: %q (error %v), want %q", c.tool, c.args, text, isErr, c.want)
		}
	}
	// The installer token reads a staff-only site.
	if text, isErr := e.call("tok", "get_site", map[string]any{"site_id": "sx"}); isErr || !strings.Contains(text, `"sx"`) {
		t.Errorf("get_site: %s", text)
	}
}

func TestMCPToolResultsKeepSecretsInThePanel(t *testing.T) {
	rec := &mcpRecorder{header: http.Header{}, status: 200}
	rec.Write([]byte(`{"job":{"id":3,"status":"succeeded"},"secret":{"password":"hunter2"}}`))
	res := toolResult(&mcpTools[0], rec)
	text := res["content"].([]map[string]any)[0]["text"].(string)
	if strings.Contains(text, "hunter2") || !strings.Contains(text, "panel") {
		t.Errorf("secret leaked: %s", text)
	}
	rec = &mcpRecorder{header: http.Header{}, status: 202}
	rec.Write([]byte(`{"job_id":9}`))
	text = toolResult(&mcpTools[0], rec)["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "get_job") {
		t.Errorf("job hint missing: %s", text)
	}
}

func TestMCPToolsAreRegisteredRoutes(t *testing.T) {
	e := newTenancyEnv(t)
	names := map[string]bool{}
	for _, tool := range mcpTools {
		if names[tool.Name] {
			t.Errorf("tool %s listed twice", tool.Name)
		}
		names[tool.Name] = true
		if e.api.routeRole(tool.Pattern) == "" {
			t.Errorf("tool %s: no route %s", tool.Name, tool.Pattern)
		}
		if strings.HasPrefix(tool.Pattern, "DELETE ") {
			t.Errorf("tool %s deletes", tool.Name)
		}
		if tool.ReadOnly != strings.HasPrefix(tool.Pattern, "GET ") {
			t.Errorf("tool %s: read-only %v but %s", tool.Name, tool.ReadOnly, tool.Pattern)
		}
		// Every path value is filled by a parameter.
		_, path, _ := strings.Cut(tool.Pattern, " ")
		for _, p := range tool.Params {
			if where, name, _ := strings.Cut(p.In, ":"); where == "path" {
				path = strings.Replace(path, "{"+name+"}", "x", 1)
			}
		}
		if strings.Contains(path, "{") {
			t.Errorf("tool %s leaves %s unfilled", tool.Name, path)
		}
	}
}
