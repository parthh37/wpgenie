package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/parthh37/wpgenie/internal/auth"
)

// The MCP server: AI assistants (Claude, ChatGPT, Claude Code, ...) manage
// sites through tools, over MCP's Streamable HTTP transport at /mcp.
//
// Every tool is one of the panel's own API routes, called in-process as
// the assistant's user, through the same wrapper as any request: the
// route's role, tenants' ownership and plan checks, two-factor
// requirement, cluster forwarding and the audit log all apply unchanged.
// A tool can never do more than its user could in the panel; tools/list
// only offers what that user may call.
//
// The catalogue is deliberately "read, plus everyday actions": nothing
// that deletes or overwrites (sites, backups restored over a live site,
// files, database search-replace), and no credentials (wp-admin sign-in
// links, passwords, a new site's admin password): what an assistant
// receives ends up in a chat transcript kept by its provider.
//
// Clients authenticate with a bearer token: an OAuth token (oauth.go) or
// a user's API token. Session cookies are never accepted here.

// mcpVersions are the protocol revisions this server speaks, newest first.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const (
	mcpMaxRequest = 1 << 20
	// mcpMaxResult bounds what one tool call returns to the assistant.
	mcpMaxResult = 100 << 10
)

// mcpParam is one argument of a tool and where it goes in the API call:
// "path:<name>" fills {name}, "query:<name>" and "body:<name>" add it.
type mcpParam struct {
	Name        string
	Type        string // string, integer, boolean, strings (array of strings)
	Description string
	Required    bool
	Enum        []string
	In          string
}

type mcpTool struct {
	Name        string
	Title       string
	Description string
	Pattern     string // the API route: "GET /api/v1/sites/{id}"
	Params      []mcpParam
	// ReadOnly: changes nothing. Idempotent: calling it twice is the same
	// as once.
	ReadOnly, Idempotent bool
}

func siteParam() mcpParam {
	return mcpParam{Name: "site_id", Type: "string", Required: true, In: "path:id",
		Description: "The site's ID (from list_sites)."}
}

func hoursParam(what string) mcpParam {
	return mcpParam{Name: "hours", Type: "integer", In: "query:hours",
		Description: "How many hours back " + what + " (default 24, at most 2160)."}
}

var mcpTools = []mcpTool{
	// ---- Looking ----
	{Name: "list_sites", Title: "List sites", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites",
		Description: "Lists the WordPress sites you can manage: ID, name, domains, status, PHP version, size, cache and protection settings. Start here to find a site's ID."},
	{Name: "get_site", Title: "Site details", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}",
		Params:      []mcpParam{siteParam()},
		Description: "Everything about one site: domains, status, PHP version and limits, resources and replicas, cache, protection, burst and auto-update settings."},
	{Name: "site_traffic", Title: "Traffic", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/stats",
		Params:      []mcpParam{siteParam(), hoursParam("to count")},
		Description: "Visitors, page views, bandwidth and requests blocked by protection, hour by hour."},
	{Name: "site_performance", Title: "Performance", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/insights",
		Params:      []mcpParam{siteParam(), hoursParam("to look")},
		Description: "Response time percentiles, page cache hit rate, the slowest URLs and PHP errors grouped by plugin or theme."},
	{Name: "site_health_report", Title: "Health report", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/analysis",
		Params:      []mcpParam{siteParam()},
		Description: "The site analyser's scored report (A–F) on security, performance and upkeep, with what to fix."},
	{Name: "list_updates", Title: "Available updates", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/updates",
		Params:      []mcpParam{siteParam()},
		Description: "Outdated WordPress core, plugins and themes, and which updates fix known vulnerabilities."},
	{Name: "update_history", Title: "Update history", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/updates/history",
		Params:      []mcpParam{siteParam()},
		Description: "Past and running WordPress update runs: what was updated, what failed and what was rolled back. Use it to follow an update started with update_wordpress."},
	{Name: "security_scan_results", Title: "Security scan results", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/scan",
		Params:      []mcpParam{siteParam()},
		Description: "The last security scan: known vulnerabilities, modified core or plugin files, PHP files in uploads."},
	{Name: "plugin_report", Title: "Plugin report", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/plugins",
		Params:      []mcpParam{siteParam()},
		Description: "The plugin analyser's last report: closed or abandoned plugins, modified or nulled copies, load time per plugin."},
	{Name: "list_backups", Title: "Backups", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/backups",
		Params:      []mcpParam{siteParam()},
		Description: "The site's backups and its backup schedule and retention."},
	{Name: "list_themes", Title: "Themes", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/tools/themes",
		Params:      []mcpParam{siteParam()},
		Description: "Installed themes: which one is active, versions and available updates."},
	{Name: "site_activity", Title: "Activity", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/events",
		Params:      []mcpParam{siteParam()},
		Description: "Recent events on the site: deployments, scaling, updates, backups, settings changed."},
	{Name: "maintenance_mode_status", Title: "Maintenance mode", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/tools/maintenance",
		Params:      []mcpParam{siteParam()},
		Description: "Whether maintenance mode is on and the message visitors see."},
	{Name: "read_error_log", Title: "PHP error log", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/tools/debug/log",
		Params:      []mcpParam{siteParam()},
		Description: "The end of the site's PHP error log (debug mode writes more to it)."},
	{Name: "burst_status", Title: "Burst status", ReadOnly: true, Idempotent: true,
		Pattern:     "GET /api/v1/sites/{id}/burst",
		Params:      []mcpParam{siteParam()},
		Description: "The site's burst mode (extra instances under load) and burst minutes used and left."},
	{Name: "check_dns", Title: "Check DNS", ReadOnly: true, Idempotent: true,
		Pattern: "GET /api/v1/dns-check",
		Params: []mcpParam{{Name: "domain", Type: "string", Required: true, In: "query:domain",
			Description: "The domain to check, e.g. example.com."}},
		Description: "Checks whether a domain's DNS points at this server, before creating a site or adding a domain."},
	{Name: "list_jobs", Title: "Jobs", ReadOnly: true, Idempotent: true,
		Pattern: "GET /api/v1/jobs",
		Params: []mcpParam{
			{Name: "site_id", Type: "string", In: "query:site", Description: "Only this site's jobs."},
			{Name: "active", Type: "boolean", In: "query:active", Description: "Only jobs still queued or running."},
		},
		Description: "Background jobs (site creation, backups, staging copies, installs) with their progress."},
	{Name: "get_job", Title: "Job progress", ReadOnly: true, Idempotent: true,
		Pattern: "GET /api/v1/jobs/{id}",
		Params: []mcpParam{{Name: "job_id", Type: "integer", Required: true, In: "path:id",
			Description: "The job's ID, as returned by the tool that started it."}},
		Description: "A job's status, progress and result. Tools that start long operations return a job_id: call this until the status is succeeded or failed."},

	// ---- Everyday actions ----
	{Name: "create_site", Title: "Create a site",
		Pattern: "POST /api/v1/sites",
		Params: []mcpParam{
			{Name: "domain", Type: "string", Required: true, In: "body:domain",
				Description: "The site's domain, e.g. example.com. Point its DNS at the server first (check_dns) so HTTPS works."},
			{Name: "admin_email", Type: "string", Required: true, In: "body:admin_email",
				Description: "The WordPress administrator's e-mail address."},
			{Name: "name", Type: "string", In: "body:name", Description: "The site's title (default: the domain)."},
			{Name: "admin_user", Type: "string", In: "body:admin_user",
				Description: "The administrator's username (default: a generated one, never 'admin')."},
			{Name: "divi", Type: "boolean", In: "body:divi",
				Description: "Install and activate the Divi theme (when the host has a Divi license; default: the host's setting for new sites)."},
		},
		Description: "Creates a new WordPress site. Returns a job_id: follow it with get_job. The administrator's password is not returned here: it is shown once in the WPGenie panel to whoever started the job (or reset it there)."},
	{Name: "purge_cache", Title: "Clear the cache", Idempotent: true,
		Pattern:     "POST /api/v1/sites/{id}/cache/purge",
		Params:      []mcpParam{siteParam()},
		Description: "Clears the site's page cache (and the CDN's copy), so visitors get fresh pages."},
	{Name: "start_backup", Title: "Back up now",
		Pattern:     "POST /api/v1/sites/{id}/backups",
		Params:      []mcpParam{siteParam()},
		Description: "Backs up the site's files and database now. Returns a job_id: follow it with get_job."},
	{Name: "update_wordpress", Title: "Update WordPress",
		Pattern: "POST /api/v1/sites/{id}/updates",
		Params: []mcpParam{siteParam(),
			{Name: "all", Type: "boolean", In: "body:all", Description: "Update everything that's outdated."},
			{Name: "core", Type: "boolean", In: "body:core", Description: "Update WordPress itself."},
			{Name: "plugins", Type: "strings", In: "body:plugins", Description: "Plugins to update, by slug."},
			{Name: "themes", Type: "strings", In: "body:themes", Description: "Themes to update, by slug."},
		},
		Description: "Updates WordPress core, plugins or themes safely: a snapshot first, then a health check, and an automatic rollback if the site breaks. Returns a run_id; follow it with update_history."},
	{Name: "run_security_scan", Title: "Scan for security issues",
		Pattern:     "POST /api/v1/sites/{id}/scan",
		Params:      []mcpParam{siteParam()},
		Description: "Scans the site for known vulnerabilities and modified or suspicious files; read the outcome with security_scan_results."},
	{Name: "analyse_plugins", Title: "Analyse plugins",
		Pattern:     "POST /api/v1/sites/{id}/plugins",
		Params:      []mcpParam{siteParam()},
		Description: "Re-runs the plugin analyser (closed, abandoned, modified or nulled plugins; load time per plugin); read it with plugin_report."},
	{Name: "create_staging", Title: "Create a staging copy",
		Pattern: "POST /api/v1/sites/{id}/staging",
		Params: []mcpParam{siteParam(),
			{Name: "domain", Type: "string", In: "body:domain",
				Description: "The staging copy's domain (default: staging.<the site's domain>)."}},
		Description: "Copies a live site into a new staging site to try changes safely. Returns a job_id: follow it with get_job."},
	{Name: "set_maintenance_mode", Title: "Maintenance mode", Idempotent: true,
		Pattern: "PUT /api/v1/sites/{id}/tools/maintenance",
		Params: []mcpParam{siteParam(),
			{Name: "on", Type: "boolean", Required: true, In: "body:on",
				Description: "true shows visitors a maintenance page (signed-in administrators still see the site); false turns it off."},
			{Name: "message", Type: "string", In: "body:message", Description: "The message visitors see."}},
		Description: "Turns maintenance mode on or off."},
	{Name: "set_debug_mode", Title: "Debug mode", Idempotent: true,
		Pattern: "PUT /api/v1/sites/{id}/tools/debug",
		Params: []mcpParam{siteParam(),
			{Name: "on", Type: "boolean", Required: true, In: "body:on",
				Description: "true logs PHP notices and warnings to the error log (never shown to visitors)."}},
		Description: "Turns WordPress debug logging on or off; read the log with read_error_log."},
	{Name: "set_auto_updates", Title: "Automatic updates", Idempotent: true,
		Pattern: "PUT /api/v1/sites/{id}/auto-update",
		Params: []mcpParam{siteParam(),
			{Name: "policy", Type: "string", Required: true, In: "body:policy", Enum: []string{"off", "security", "all"},
				Description: "off; security (nightly, only updates fixing known vulnerabilities); all (nightly, everything)."}},
		Description: "Sets which WordPress updates are applied automatically every night (with snapshot and rollback)."},
	{Name: "set_burst", Title: "Burst", Idempotent: true,
		Pattern: "PUT /api/v1/sites/{id}/burst",
		Params: []mcpParam{siteParam(),
			{Name: "mode", Type: "string", Required: true, In: "body:mode", Enum: []string{"off", "auto", "on"},
				Description: "off; auto (extra instances when traffic needs them); on (extra instances now, e.g. for a launch)."},
			{Name: "hours", Type: "integer", In: "body:hours",
				Description: "With on: go back to auto after this many hours (0: until turned off)."}},
		Description: "Sets burst: extra instances of the site under load, billed in burst minutes."},
	{Name: "install_theme", Title: "Install a theme",
		Pattern: "POST /api/v1/sites/{id}/tools/themes",
		Params: []mcpParam{siteParam(),
			{Name: "slug", Type: "string", Required: true, In: "body:slug",
				Description: "The theme's WordPress.org name, as in wordpress.org/themes/<slug>."}},
		Description: "Installs a theme from WordPress.org (not activated). Returns a job_id: follow it with get_job."},
	{Name: "install_divi", Title: "Install Divi", Idempotent: true,
		Pattern:     "POST /api/v1/sites/{id}/divi",
		Params:      []mcpParam{siteParam()},
		Description: "Installs and activates the Divi theme with the host's license (or just activates it if it's installed). Returns a job_id: follow it with get_job."},
	{Name: "activate_theme", Title: "Switch theme", Idempotent: true,
		Pattern: "POST /api/v1/sites/{id}/tools/themes/{theme}/activate",
		Params: []mcpParam{siteParam(),
			{Name: "theme", Type: "string", Required: true, In: "path:theme",
				Description: "The installed theme's slug (from list_themes)."}},
		Description: "Switches the site to an installed theme and clears the cache."},
}

const mcpInstructions = `WPGenie hosts WordPress sites. Use list_sites to find a site's ID, then the site tools.
Long operations (creating a site, backups, staging copies, theme or Divi installs) return a job_id: follow them with get_job. WordPress updates return a run_id: follow them with update_history.
Updates are safe: a snapshot is taken first and the site is rolled back automatically if it breaks.
Passwords and sign-in links are never returned here; they are in the WPGenie panel.
Deleting sites, restoring backups and editing files are done in the panel, not here.`

// mcpSegment is what a path value may be: a site, theme or job ID.
var mcpSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

func (t *mcpTool) inputSchema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.Params {
		var sch map[string]any
		switch p.Type {
		case "strings":
			sch = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		case "integer":
			sch = map[string]any{"type": "integer", "minimum": 0}
		default:
			sch = map[string]any{"type": p.Type}
		}
		sch["description"] = p.Description
		if len(p.Enum) > 0 {
			sch["enum"] = p.Enum
		}
		props[p.Name] = sch
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func (t *mcpTool) describe() map[string]any {
	return map[string]any{
		"name":        t.Name,
		"title":       t.Title,
		"description": t.Description,
		"inputSchema": t.inputSchema(),
		"annotations": map[string]any{
			"title":           t.Title,
			"readOnlyHint":    t.ReadOnly,
			"destructiveHint": false,
			"idempotentHint":  t.Idempotent,
			"openWorldHint":   false,
		},
	}
}

// mcpRequest builds the API call a tool's arguments make.
func (t *mcpTool) request(args map[string]any) (method, target string, body []byte, err error) {
	method, path, _ := strings.Cut(t.Pattern, " ")
	known := map[string]bool{}
	q := url.Values{}
	bodyArgs := map[string]any{}
	for _, p := range t.Params {
		known[p.Name] = true
		v, ok := args[p.Name]
		if !ok || v == nil {
			if p.Required {
				return "", "", nil, fmt.Errorf("%s is required", p.Name)
			}
			continue
		}
		v, err := p.check(v)
		if err != nil {
			return "", "", nil, err
		}
		where, name, _ := strings.Cut(p.In, ":")
		switch where {
		case "path":
			s := fmt.Sprint(v)
			if !mcpSegment.MatchString(s) {
				return "", "", nil, fmt.Errorf("%s is not a valid ID", p.Name)
			}
			path = strings.Replace(path, "{"+name+"}", url.PathEscape(s), 1)
		case "query":
			switch v := v.(type) {
			case bool:
				if v {
					q.Set(name, "1")
				}
			default:
				q.Set(name, fmt.Sprint(v))
			}
		case "body":
			bodyArgs[name] = v
		}
	}
	for k := range args {
		if !known[k] {
			return "", "", nil, fmt.Errorf("unknown argument %q", k)
		}
	}
	if strings.Contains(path, "{") {
		return "", "", nil, errors.New("missing an ID")
	}
	target = path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	if method != http.MethodGet {
		if body, err = json.Marshal(bodyArgs); err != nil {
			return "", "", nil, err
		}
	}
	return method, target, body, nil
}

// check converts a JSON argument to the parameter's type.
func (p *mcpParam) check(v any) (any, error) {
	bad := fmt.Errorf("%s must be %s", p.Name, map[string]string{"string": "a string", "integer": "a whole number",
		"boolean": "true or false", "strings": "a list of strings"}[p.Type])
	switch p.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, bad
		}
		if len(p.Enum) > 0 && !slices.Contains(p.Enum, s) {
			return nil, fmt.Errorf("%s must be one of %s", p.Name, strings.Join(p.Enum, ", "))
		}
		return s, nil
	case "integer":
		switch n := v.(type) {
		case float64:
			if n != float64(int64(n)) || n < 0 || n > 1e12 {
				return nil, bad
			}
			return int64(n), nil
		case string: // some clients send numbers as strings
			i, err := strconv.ParseInt(n, 10, 64)
			if err != nil || i < 0 {
				return nil, bad
			}
			return i, nil
		}
		return nil, bad
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, bad
		}
		return b, nil
	case "strings":
		list, ok := v.([]any)
		if !ok || len(list) > 200 {
			return nil, bad
		}
		out := make([]string, len(list))
		for i, x := range list {
			if out[i], ok = x.(string); !ok {
				return nil, bad
			}
		}
		return out, nil
	}
	return nil, bad
}

// routeRole is the staff role a registered route needs ("": no such
// route here, e.g. a feature this server doesn't run).
func (s *Server) routeRole(pattern string) string {
	for _, r := range s.routes {
		if r.Pattern == pattern {
			return r.Role
		}
	}
	return ""
}

// mayCall says whether a principal may call a route at all (the call
// itself still checks everything, e.g. which sites a tenant owns).
func (s *Server) mayCall(p *Principal, pattern string) bool {
	role := s.routeRole(pattern)
	if role == "" {
		return false
	}
	if auth.IsTenant(p.Role) {
		rule, ok := tenantRoutes[pattern]
		return ok && (!rule.reseller || p.Role == auth.RoleReseller)
	}
	return auth.Level(p.Role) >= auth.Level(role)
}

// toolsFor are the tools a principal may use.
func (s *Server) toolsFor(p *Principal) []*mcpTool {
	var out []*mcpTool
	for i := range mcpTools {
		if s.mayCall(p, mcpTools[i].Pattern) {
			out = append(out, &mcpTools[i])
		}
	}
	return out
}

// ---- Transport ----

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = result
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// mcpUnauthorized points the client at the OAuth metadata (RFC 9728).
func (s *Server) mcpUnauthorized(w http.ResponseWriter, r *http.Request, invalid bool) {
	h := fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="%s"`,
		s.publicBase(r), oauthScope)
	if invalid {
		h += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", h)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func (s *Server) mcpRoutes(mux *http.ServeMux) {
	if s.Node {
		return
	}
	mux.HandleFunc("POST /mcp", s.mcpHandler)
	mux.HandleFunc("GET /mcp", mcpNoStream)
	mux.HandleFunc("DELETE /mcp", mcpNoStream)
}

// mcpNoStream: this server answers each POST with JSON and keeps no
// sessions, so it offers no server-to-client stream.
func mcpNoStream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
}

func (s *Server) mcpHandler(w http.ResponseWriter, r *http.Request) {
	// A browser page on another origin must not drive the server (DNS
	// rebinding); assistants call from their servers, without Origin.
	if o := r.Header.Get("Origin"); o != "" && o != s.publicBase(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		s.mcpUnauthorized(w, r, false)
		return
	}
	p, err := s.authenticate(r)
	if err != nil {
		s.mcpUnauthorized(w, r, true)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxRequest))
	if err != nil {
		writeRPC(w, nil, nil, &rpcError{rpcInvalidRequest, "request too large"})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		writeRPC(w, nil, nil, &rpcError{rpcInvalidRequest, "batches are not supported"})
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		writeRPC(w, nil, nil, &rpcError{rpcParseError, "invalid JSON"})
		return
	}
	if msg.JSONRPC != "2.0" {
		writeRPC(w, msg.ID, nil, &rpcError{rpcInvalidRequest, "jsonrpc must be 2.0"})
		return
	}
	if msg.ID == nil || msg.Method == "" {
		// A notification (initialized, cancelled) or a response: nothing to
		// answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch msg.Method {
	case "initialize":
		var in struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(msg.Params, &in)
		v := mcpVersions[0]
		if slices.Contains(mcpVersions, in.ProtocolVersion) {
			v = in.ProtocolVersion
		}
		writeRPC(w, msg.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "wpgenie", "title": "WPGenie", "version": s.Version},
			"instructions":    mcpInstructions,
		}, nil)
	case "ping":
		writeRPC(w, msg.ID, map[string]any{}, nil)
	case "tools/list":
		tools := s.toolsFor(p)
		list := make([]map[string]any, len(tools))
		for i, t := range tools {
			list[i] = t.describe()
		}
		writeRPC(w, msg.ID, map[string]any{"tools": list}, nil)
	case "tools/call":
		var in struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &in); err != nil {
			writeRPC(w, msg.ID, nil, &rpcError{rpcInvalidParams, "invalid params"})
			return
		}
		i := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == in.Name })
		if i < 0 || !s.mayCall(p, mcpTools[i].Pattern) {
			writeRPC(w, msg.ID, nil, &rpcError{rpcInvalidParams, "unknown tool " + strconv.Quote(in.Name)})
			return
		}
		writeRPC(w, msg.ID, s.callTool(r, &mcpTools[i], in.Arguments), nil)
	default:
		writeRPC(w, msg.ID, nil, &rpcError{rpcMethodNotFound, "method not found: " + msg.Method})
	}
}

// mcpCallKey marks an in-process request made by a tool (its route
// pattern): only callTool sets it, so an assistant's token works there
// and nowhere else (see route).
type mcpCallKey struct{}

func mcpToolPattern(ctx context.Context) string {
	p, _ := ctx.Value(mcpCallKey{}).(string)
	return p
}

// toolText is a tool call's result.
func toolText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// callTool runs a tool: its API route, in-process, as the caller.
func (s *Server) callTool(orig *http.Request, t *mcpTool, args map[string]any) map[string]any {
	method, target, body, err := t.request(args)
	if err != nil {
		return toolText(err.Error(), true)
	}
	ctx := context.WithValue(context.WithoutCancel(orig.Context()), mcpCallKey{}, t.Pattern)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return toolText(err.Error(), true)
	}
	req.Host = orig.Host
	req.RemoteAddr = orig.RemoteAddr
	req.RequestURI = target
	req.Header.Set("Authorization", orig.Header.Get("Authorization"))
	if xff := orig.Header.Get("X-Forwarded-For"); xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	req.Header.Set("User-Agent", strings.TrimSpace(orig.UserAgent()+" (MCP)"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
	}
	rec := &mcpRecorder{header: http.Header{}, status: http.StatusOK}
	s.mux.ServeHTTP(rec, req)
	return toolResult(t, rec)
}

// toolResult turns an API response into what the assistant reads.
func toolResult(t *mcpTool, rec *mcpRecorder) map[string]any {
	raw := rec.body.Bytes()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		v = nil
	}
	if rec.status >= 400 {
		msg := http.StatusText(rec.status)
		if m, ok := v.(map[string]any); ok {
			if e, ok := m["error"].(string); ok && e != "" {
				msg = e
			}
		}
		return toolText(fmt.Sprintf("Error (%d): %s", rec.status, msg), true)
	}
	if m, ok := v.(map[string]any); ok {
		// Secrets stay in the panel (see the package comment): a new
		// site's admin password is the job's secret.
		if _, has := m["secret"]; has {
			m = maps.Clone(m)
			delete(m, "secret")
			m["note"] = "Credentials from this job are shown in the WPGenie panel, not here."
			v = m
		}
		if id, ok := m["job_id"]; ok && rec.status == http.StatusAccepted {
			m = maps.Clone(m)
			m["next"] = fmt.Sprintf("Started job %v: call get_job with job_id %v to follow it.", id, id)
			v = m
		}
	}
	var text []byte
	if v != nil {
		text, _ = json.Marshal(v)
	} else if len(raw) > 0 {
		text = raw
	} else {
		text = []byte("Done.")
	}
	if len(text) > mcpMaxResult {
		text = append(text[:mcpMaxResult:mcpMaxResult], []byte("\n… (cut: the result is too long)")...)
	}
	return toolText(string(text), false)
}

// mcpRecorder collects an in-process API response (bounded).
type mcpRecorder struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (r *mcpRecorder) Header() http.Header { return r.header }

func (r *mcpRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
}

func (r *mcpRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	if room := 4*mcpMaxResult - r.body.Len(); room > 0 {
		r.body.Write(b[:min(len(b), room)])
	}
	return len(b), nil
}

// mcpInfo is the panel's "Connect an AI assistant" card: the address to
// paste, and the tools this user's assistant will get.
func (s *Server) mcpInfo(w http.ResponseWriter, r *http.Request) error {
	tools := s.toolsFor(principalFrom(r.Context()))
	list := make([]map[string]any, len(tools))
	for i, t := range tools {
		list[i] = map[string]any{"name": t.Name, "title": t.Title, "description": t.Description, "read_only": t.ReadOnly}
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"url":    s.mcpURL(r),
		"public": strings.HasPrefix(s.publicBase(r), "https://"),
		"tools":  list,
	})
}
