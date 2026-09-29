package proxy

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Uploads offload: a site's uploads are copied to object storage, and local
// copies may be removed after a while. A request for an upload that isn't
// on disk is then proxied to the storage's public URL: GET and HEAD only,
// never anything under a ".php" or a dot path, never outside
// /wp-content/uploads, with no cookies or credentials forwarded, and with
// the storage's own errors turned into a plain 404.

// offloadTarget is a parsed public base URL.
type offloadTarget struct {
	Origin string // scheme://host:port, what reverse_proxy dials
	Host   string // Host header and TLS server name (no default port)
	Path   string // "" or /a/b: what /wp-content/uploads is replaced with
	TLS    bool
	// ServerName is the TLS server name (SNI, certificate check).
	ServerName string
}

var (
	offloadHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	// Path segments of unreserved characters only: the path is written into
	// the Caddyfile, and a %-escape would be decoded twice.
	offloadPathRe = regexp.MustCompile(`^(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*$`)
)

// parseOffload refuses anything but a plain http(s) origin plus a safe path.
// The site layer only produces https; http is for tests against MinIO.
func parseOffload(raw string) (offloadTarget, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		!offloadHostRe.MatchString(u.Host) || !offloadPathRe.MatchString(u.Path) || strings.Contains(u.Path, "/..") {
		return offloadTarget{}, fmt.Errorf("unsafe offload URL %q", raw)
	}
	t := offloadTarget{Host: strings.ToLower(u.Host), Path: u.Path, TLS: u.Scheme == "https",
		ServerName: strings.ToLower(u.Hostname())}
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[t.TLS]
	} else if (t.TLS && port == "443") || (!t.TLS && port == "80") {
		t.Host = strings.ToLower(u.Hostname())
	}
	t.Origin = u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
	return t, nil
}
