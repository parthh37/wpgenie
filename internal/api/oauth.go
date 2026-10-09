package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

// OAuth 2.1 for AI assistants (Claude, ChatGPT, ...) connecting to the MCP
// server (mcp.go): what lets someone paste the panel's MCP address into
// their assistant and approve it once, instead of handling API tokens.
//
//   - Discovery: protected resource metadata (RFC 9728) points at this
//     panel as the authorization server (RFC 8414 metadata).
//   - Clients register themselves (RFC 7591): open to anyone, rate limited
//     and pruned, and worth nothing on its own.
//   - /oauth/authorize sends the browser to the panel's consent page
//     (#/connect). The person signs in there as usual (password, two-factor)
//     and approves; the page asks the API for the code with the session.
//     The session cookie is SameSite=Strict, so it never rides along with
//     the cross-site navigation from the assistant: approving always takes
//     a same-origin request from the panel itself.
//   - The code (PKCE S256 required, single use, two minutes) becomes an API
//     token of that user with a refresh token. It acts as the user, with the
//     user's role and account, like every API token: tenants reach only their
//     sites, a viewer can only look.

const (
	oauthCodeTTL    = 2 * time.Minute
	oauthAccessTTL  = time.Hour
	oauthRefreshTTL = 60 * 24 * time.Hour
	oauthScope      = "wpgenie"
	// Registration is open: a ceiling on clients, and clients nobody used
	// for oauthClientIdle (and that hold no tokens) are forgotten.
	maxOAuthClients = 1000
	oauthClientIdle = 30 * 24 * time.Hour
	// A client that never got a token is forgotten sooner: connecting
	// takes minutes (a code lives two).
	oauthClientUnused = time.Hour
	oauthRegisterRate = 20 // per address per guardWindow
	refreshPrefix     = "wpgr_"
	oauthClientPrefix = "wpgc_"
)

var errTooManyTokens = fmt.Errorf("%w: you have %d API tokens, the most there can be; revoke one under Account first",
	errConflict, maxTokensPerUser)

// oauthCode is an authorization code waiting to be exchanged.
type oauthCode struct {
	clientID    string
	redirectURI string
	challenge   string
	userID      int64
	expires     time.Time
}

// oauthState holds codes (in memory: they live two minutes; a restart
// only means approving again) and the registration limiter.
type oauthState struct {
	mu       sync.Mutex
	codes    map[string]*oauthCode // by hash
	register loginGuard
}

func (o *oauthState) putCode(hash string, c *oauthCode, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.codes == nil {
		o.codes = map[string]*oauthCode{}
	}
	for k, v := range o.codes {
		if now.After(v.expires) {
			delete(o.codes, k)
		}
	}
	o.codes[hash] = c
}

// takeCode returns a code once: a second exchange finds nothing.
func (o *oauthState) takeCode(hash string, now time.Time) *oauthCode {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.codes[hash]
	delete(o.codes, hash)
	if c == nil || now.After(c.expires) {
		return nil
	}
	return c
}

func (s *Server) oauthRoutes(mux *http.ServeMux, r func(pattern, role string, h handlerFunc)) {
	if s.Node {
		return // people connect assistants to the panel
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServerMetadata)
	mux.HandleFunc("GET /oauth/authorize", s.oauthAuthorize)
	mux.HandleFunc("POST /oauth/token", s.oauthToken)
	mux.HandleFunc("POST /oauth/register", s.oauthRegister)
	mux.HandleFunc("POST /oauth/revoke", s.oauthRevoke)
	// The consent page's calls (signed in).
	r("GET /api/v1/oauth/authorize", viewer, s.oauthRequestInfo)
	r("POST /api/v1/oauth/authorize", viewer, s.oauthApprove)
	r("GET /api/v1/mcp", viewer, s.mcpInfo)
}

func init() {
	registerTenantRoutes(map[string]tenantRule{
		"GET /api/v1/oauth/authorize":  anyTenant,
		"POST /api/v1/oauth/authorize": anyTenant,
		"GET /api/v1/mcp":              anyTenant,
	})
}

// publicBase is the panel's origin as assistants reach it: its public
// address, or (through an SSH tunnel, no panel domain) the address this
// request came to, so Claude Code on the same machine can connect too.
func (s *Server) publicBase(r *http.Request) string {
	if strings.HasPrefix(s.PanelURL, "https://") {
		return strings.TrimSuffix(s.PanelURL, "/")
	}
	return "http://" + r.Host
}

func (s *Server) mcpURL(r *http.Request) string { return s.publicBase(r) + "/mcp" }

func (s *Server) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{oauthScope},
		"resource_name":            "WPGenie",
	})
}

func (s *Server) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                     base,
		"authorization_endpoint":                     base + "/oauth/authorize",
		"token_endpoint":                             base + "/oauth/token",
		"registration_endpoint":                      base + "/oauth/register",
		"revocation_endpoint":                        base + "/oauth/revoke",
		"response_types_supported":                   []string{"code"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":           []string{"S256"},
		"token_endpoint_auth_methods_supported":      []string{"none", "client_secret_post", "client_secret_basic"},
		"revocation_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                           []string{oauthScope},
	})
}

// ---- Registration ----

// validRedirectURI accepts https addresses, and plain http only to this
// machine (assistants running on the user's computer: Claude Code, Claude
// Desktop, local tools), as OAuth for native apps does (RFC 8252).
func validRedirectURI(raw string) bool {
	if len(raw) > 500 || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil || u.Host == "" || u.Opaque != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

func oauthJSONError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (s *Server) oauthRegister(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	ctx := r.Context()
	now := s.now()
	key := limitKey{"oauth-register:" + ipKey(clientIP(r)).key, oauthRegisterRate}
	if wait, _ := s.oauth.register.attempt(now, key); wait > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		oauthJSONError(w, http.StatusTooManyRequests, "invalid_request", "too many registrations from this address; try again later")
		return
	}
	var in struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	// Unknown metadata (scope, logo_uri, software_id, ...) is ignored, as
	// RFC 7591 requires: not decode(), which refuses unknown fields.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		oauthJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "the request is not valid JSON")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthJSONError(w, http.StatusBadRequest, "invalid_redirect_uri", "give 1 to 10 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirectURI(u) {
			oauthJSONError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect URIs must be https (or http to localhost), without a fragment")
			return
		}
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "grant type "+g+" is not supported")
			return
		}
	}
	for _, t := range in.ResponseTypes {
		if t != "code" {
			oauthJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "response type "+t+" is not supported")
			return
		}
	}
	method := in.TokenEndpointAuthMethod
	switch method {
	case "":
		method = "none"
	case "none", "client_secret_post", "client_secret_basic":
	default:
		oauthJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method "+method+" is not supported")
		return
	}
	name := cleanClientName(in.ClientName)
	if _, err := s.Store.PruneOAuthClients(ctx, now.Add(-oauthClientIdle), now.Add(-oauthClientUnused)); err != nil {
		s.Log.Warn("oauth: pruning clients", "err", err)
	}
	// Full: the oldest client that never connected makes room, so a flood
	// of registrations can't lock real assistants out.
	if n, err := s.Store.CountOAuthClients(ctx); err != nil ||
		(n >= maxOAuthClients && s.Store.EvictUnusedOAuthClient(ctx) != nil) {
		oauthJSONError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many registered clients")
		return
	}
	c := &store.OAuthClient{ID: oauthClientPrefix + auth.RandomToken(16), Name: name, RedirectURIs: in.RedirectURIs,
		CreatedAt: now}
	var secret string
	if method != "none" {
		secret = auth.RandomToken(32)
		c.SecretHash = auth.HashToken(secret)
	}
	if err := s.Store.CreateOAuthClient(ctx, c); err != nil {
		s.Log.Error("oauth: registering a client", "err", err)
		oauthJSONError(w, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	out := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        now.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      oauthScope,
	}
	if secret != "" {
		out["client_secret"] = secret
		out["client_secret_expires_at"] = 0
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

// cleanClientName is what the consent page shows for a client: one line,
// at most 64 characters. It is what the client says it is, so the page
// also shows where approval sends the browser.
func cleanClientName(n string) string {
	// Invisible format characters (zero-width spaces, bidi overrides) could
	// make a name read as something else.
	n = strings.Join(strings.FieldsFunc(n, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}), " ")
	if n == "" {
		return "AI assistant"
	}
	if utf8.RuneCountInString(n) > 64 {
		n = string([]rune(n)[:64])
	}
	return n
}

// ---- Authorization ----

// authRequest is an authorization request's parameters.
type authRequest struct {
	ResponseType        string `json:"response_type"`
	ClientID            string `json:"client_id"`
	RedirectURI         string `json:"redirect_uri"`
	State               string `json:"state"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	Scope               string `json:"scope"`
	Resource            string `json:"resource"`
}

func authRequestFrom(q url.Values) authRequest {
	return authRequest{ResponseType: q.Get("response_type"), ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"),
		State: q.Get("state"), CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"),
		Scope: q.Get("scope"), Resource: q.Get("resource")}
}

// errNoRedirect: the request can't be answered at its redirect URI (an
// unknown client, or an address it didn't register): the person is told
// instead, never sent somewhere unverified.
var errNoRedirect = errors.New("this connection request is not valid")

// checkClient resolves the client and redirect URI of a request. With an
// error, nothing may be sent to the redirect URI.
func (s *Server) checkClient(ctx context.Context, in *authRequest) (*store.OAuthClient, error) {
	if in.ClientID == "" || len(in.ClientID) > 100 {
		return nil, fmt.Errorf("%w: it names no app", errNoRedirect)
	}
	c, err := s.Store.OAuthClient(ctx, in.ClientID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: the app isn't registered here (start connecting again from the app)", errNoRedirect)
	}
	if err != nil {
		return nil, err
	}
	if in.RedirectURI == "" && len(c.RedirectURIs) == 1 {
		in.RedirectURI = c.RedirectURIs[0]
	}
	if !slices.Contains(c.RedirectURIs, in.RedirectURI) {
		return nil, fmt.Errorf("%w: the app's return address doesn't match the one it registered", errNoRedirect)
	}
	return c, nil
}

// checkRequest validates the rest of a request: an OAuth error code (to
// send to the redirect URI) and its description, or "".
func (s *Server) checkRequest(r *http.Request, in *authRequest) (string, string) {
	switch {
	case in.ResponseType != "code":
		return "unsupported_response_type", "only the authorization code flow is supported"
	case in.CodeChallengeMethod != "S256" || len(in.CodeChallenge) < 43 || len(in.CodeChallenge) > 128:
		return "invalid_request", "PKCE with S256 is required"
	case len(in.State) > 1000:
		return "invalid_request", "state is too long"
	case in.Resource != "" && strings.TrimSuffix(in.Resource, "/") != s.mcpURL(r) &&
		strings.TrimSuffix(in.Resource, "/") != s.publicBase(r):
		return "invalid_target", "this server only grants access to " + s.mcpURL(r)
	}
	return "", ""
}

// redirectWith is the client's redirect URI with params added.
func redirectWith(raw string, params map[string]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// oauthAuthorize is where an assistant sends the browser: on to the
// panel's consent page, which signs the person in if needed.
func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	in := authRequestFrom(r.URL.Query())
	if _, err := s.checkClient(r.Context(), &in); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "WPGenie: %s.\n", strings.TrimPrefix(err.Error(), "store: "))
		return
	}
	// A bad request is shown here, not redirected: registration is open,
	// so redirecting errors would make the panel's address a springboard
	// to any site (RFC 9700 4.11.2).
	if _, desc := s.checkRequest(r, &in); desc != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "WPGenie: this connection request is not valid: %s.\n", desc)
		return
	}
	// The consent page reads the request from its address (a fragment:
	// never sent to a server, kept out of logs).
	q := url.Values{}
	for k, v := range map[string]string{"client_id": in.ClientID, "redirect_uri": in.RedirectURI, "state": in.State,
		"code_challenge": in.CodeChallenge, "code_challenge_method": in.CodeChallengeMethod, "scope": in.Scope,
		"resource": in.Resource, "response_type": in.ResponseType} {
		if v != "" {
			q.Set(k, v)
		}
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/#/connect?"+q.Encode(), http.StatusFound)
}

// oauthRequestInfo tells the consent page who is asking.
func (s *Server) oauthRequestInfo(w http.ResponseWriter, r *http.Request) error {
	in := authRequestFrom(r.URL.Query())
	c, err := s.checkClient(r.Context(), &in)
	if err != nil {
		return fmt.Errorf("%w: %w", errBadRequest, err)
	}
	if code, desc := s.checkRequest(r, &in); code != "" {
		return fmt.Errorf("%w: %s", errBadRequest, desc)
	}
	u, _ := url.Parse(in.RedirectURI)
	p := principalFrom(r.Context())
	return writeJSON(w, http.StatusOK, map[string]any{
		"client_name":   c.Name,
		"redirect_host": u.Host,
		"user":          p.Name,
		"role":          p.Role,
		"mcp_url":       s.mcpURL(r),
	})
}

// oauthApprove answers the consent page: with approve, a code for the
// signed-in user; either way, where to send the browser back to.
func (s *Server) oauthApprove(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		authRequest
		Approve bool `json:"approve"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	req := in.authRequest
	if _, err := s.checkClient(r.Context(), &req); err != nil {
		return fmt.Errorf("%w: %w", errBadRequest, err)
	}
	back := map[string]string{"state": req.State, "iss": s.publicBase(r)}
	if code, desc := s.checkRequest(r, &req); code != "" {
		back["error"], back["error_description"] = code, desc
		return writeJSON(w, http.StatusOK, map[string]string{"redirect": redirectWith(req.RedirectURI, back)})
	}
	if !in.Approve {
		back["error"], back["error_description"] = "access_denied", "the request was declined"
		return writeJSON(w, http.StatusOK, map[string]string{"redirect": redirectWith(req.RedirectURI, back)})
	}
	p := principalFrom(r.Context())
	// Like API tokens: from a signed-in person, never from another token.
	if p.SessionID == "" || p.UserID == 0 {
		return fmt.Errorf("%w: assistants are connected from a signed-in session", errForbidden)
	}
	if err := s.roomForOAuthToken(r.Context(), p.UserID, req.ClientID); err != nil {
		return err
	}
	code := auth.RandomToken(32)
	s.oauth.putCode(auth.HashToken(code), &oauthCode{clientID: req.ClientID, redirectURI: req.RedirectURI,
		challenge: req.CodeChallenge, userID: p.UserID, expires: s.now().Add(oauthCodeTTL)}, s.now())
	back["code"] = code
	return writeJSON(w, http.StatusOK, map[string]string{"redirect": redirectWith(req.RedirectURI, back)})
}

// roomForOAuthToken: a user's tokens are bounded; a client connecting
// again replaces its own.
func (s *Server) roomForOAuthToken(ctx context.Context, userID int64, clientID string) error {
	have, err := s.Store.APITokens(ctx, userID)
	if err != nil {
		return err
	}
	n := 0
	for _, t := range have {
		if t.ClientID != clientID {
			n++
		}
	}
	if n >= maxTokensPerUser {
		return errTooManyTokens
	}
	return nil
}

// ---- Tokens ----

// tokenClient authenticates the client at the token and revocation
// endpoints: public clients by their ID, confidential ones also by their
// secret (form or HTTP Basic).
func (s *Server) tokenClient(r *http.Request) (*store.OAuthClient, bool) {
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		u, _ = url.QueryUnescape(u)
		p, _ = url.QueryUnescape(p)
		if id != "" && id != u {
			return nil, false
		}
		id, secret = u, p
	}
	if id == "" || len(id) > 100 {
		return nil, false
	}
	c, err := s.Store.OAuthClient(r.Context(), id)
	if err != nil {
		return nil, false
	}
	if c.SecretHash != "" &&
		subtle.ConstantTimeCompare([]byte(auth.HashToken(secret)), []byte(c.SecretHash)) != 1 {
		return nil, false
	}
	return c, true
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthJSONError(w, http.StatusBadRequest, "invalid_request", "send the parameters form-encoded")
		return
	}
	c, ok := s.tokenClient(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="wpgenie"`)
		oauthJSONError(w, http.StatusUnauthorized, "invalid_client", "unknown client or wrong client secret")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, c)
	case "refresh_token":
		s.refreshToken(w, r, c)
	default:
		oauthJSONError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

// oauthUser is the user a token is for, if they may still have one:
// enabled, in a live account, and with two-factor authentication when the
// panel requires it (as for API tokens).
func (s *Server) oauthUser(ctx context.Context, id int64) (*store.User, bool) {
	u, err := s.Store.GetUser(ctx, id)
	if err != nil || u.Disabled {
		return nil, false
	}
	if _, err := s.principalFor(ctx, u); err != nil {
		return nil, false
	}
	if !u.TOTPEnabled && s.require2FA(ctx) {
		return nil, false
	}
	return u, true
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, c *store.OAuthClient) {
	ctx := r.Context()
	now := s.now()
	code := s.oauth.takeCode(auth.HashToken(r.PostForm.Get("code")), now)
	if code == nil || code.clientID != c.ID {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, used or expired")
		return
	}
	if ru := r.PostForm.Get("redirect_uri"); ru != "" && ru != code.redirectURI {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri doesn't match the authorization request")
		return
	}
	if !pkceMatches(r.PostForm.Get("code_verifier"), code.challenge) {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match the code challenge")
		return
	}
	u, ok := s.oauthUser(ctx, code.userID)
	if !ok {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "the user can't have API access")
		return
	}
	if err := s.roomForOAuthToken(ctx, u.ID, c.ID); err != nil {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	access, refresh := apiTokenPrefix+auth.RandomToken(32), refreshPrefix+auth.RandomToken(32)
	t := &store.APIToken{UserID: u.ID, Name: c.Name, ClientID: c.ID, Hint: access[:len(apiTokenPrefix)+6] + "…",
		ExpiresAt: now.Add(oauthAccessTTL), RefreshExpiresAt: now.Add(oauthRefreshTTL)}
	if _, err := s.Store.CreateOAuthToken(ctx, t, auth.HashToken(access), auth.HashToken(refresh)); err != nil {
		s.Log.Error("oauth: issuing a token", "err", err)
		oauthJSONError(w, http.StatusInternalServerError, "server_error", "issuing the token failed")
		return
	}
	s.Store.TouchOAuthClient(ctx, c.ID, now)
	s.audit(ctx, store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: "oauth_connected", Target: c.ID,
		Status: http.StatusOK, Detail: "AI assistant connected: " + c.Name})
	writeTokens(w, access, refresh)
}

func (s *Server) refreshToken(w http.ResponseWriter, r *http.Request, c *store.OAuthClient) {
	ctx := r.Context()
	now := s.now()
	old := auth.HashToken(r.PostForm.Get("refresh_token"))
	t, err := s.Store.APITokenByRefresh(ctx, old)
	if err != nil || t.ClientID != c.ID {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown or was revoked")
		return
	}
	if !now.Before(t.RefreshExpiresAt) {
		s.Store.DeleteAPIToken(ctx, t.ID, 0)
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "the refresh token expired; connect again")
		return
	}
	if _, ok := s.oauthUser(ctx, t.UserID); !ok {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "the user can't have API access")
		return
	}
	access, refresh := apiTokenPrefix+auth.RandomToken(32), refreshPrefix+auth.RandomToken(32)
	err = s.Store.RotateOAuthToken(ctx, t.ID, old, auth.HashToken(access), auth.HashToken(refresh),
		now.Add(oauthAccessTTL), now.Add(oauthRefreshTTL))
	if errors.Is(err, store.ErrNotFound) {
		oauthJSONError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was already used")
		return
	}
	if err != nil {
		s.Log.Error("oauth: refreshing a token", "err", err)
		oauthJSONError(w, http.StatusInternalServerError, "server_error", "refreshing the token failed")
		return
	}
	s.Store.TouchOAuthClient(ctx, c.ID, now)
	writeTokens(w, access, refresh)
}

func writeTokens(w http.ResponseWriter, access, refresh string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(oauthAccessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         oauthScope,
	})
}

// oauthRevoke (RFC 7009): a client disconnecting itself. Unknown tokens
// are not an error.
func (s *Server) oauthRevoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthJSONError(w, http.StatusBadRequest, "invalid_request", "send the parameters form-encoded")
		return
	}
	c, ok := s.tokenClient(r)
	if !ok {
		oauthJSONError(w, http.StatusUnauthorized, "invalid_client", "unknown client or wrong client secret")
		return
	}
	if tok := r.PostForm.Get("token"); tok != "" {
		if err := s.Store.DeleteAPITokenByHash(r.Context(), auth.HashToken(tok), c.ID); err != nil {
			s.Log.Error("oauth: revoking a token", "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}
