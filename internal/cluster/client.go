package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Endpoint is where a node's cluster listener answers.
type Endpoint struct {
	ID      string
	Address string // host:port
	// Pin is the SHA-256 of the node's public key, fixed at pairing (hex;
	// "" for the panel's own identity). A certificate the CA once issued for
	// this ID to another key (a removed server's, still valid) is refused.
	Pin string `json:"pin,omitempty"`
}

// StatusError is a non-2xx answer from a node.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	msg := strings.TrimSpace(e.Msg)
	var j struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &j) == nil && j.Error != "" {
		msg = j.Error
	}
	return fmt.Sprintf("node answered %d: %s", e.Code, msg)
}

// Client dials other servers of the cluster with this server's identity.
// One transport per node: each has its own TLS server name (the node's
// identity) and its own pool of HTTP/2 connections.
type Client struct {
	cert atomic.Pointer[tls.Certificate]
	pool *x509.CertPool

	mu         sync.Mutex
	transports map[string]*http.Transport // node ID + address
}

// NewClient trusts only the cluster CA.
func NewClient(caPEM []byte, cert *tls.Certificate) (*Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no CA certificate")
	}
	c := &Client{pool: pool, transports: map[string]*http.Transport{}}
	c.cert.Store(cert)
	return c, nil
}

// SetCertificate swaps this server's certificate (after renewal); new
// connections use it.
func (c *Client) SetCertificate(cert *tls.Certificate) { c.cert.Store(cert) }

func (c *Client) transport(n Endpoint) *http.Transport {
	key := n.ID + "|" + n.Address + "|" + n.Pin
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.transports[key]; ok {
		return t
	}
	t := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    c.pool,
			// The node's identity: the chain must end in its name.
			ServerName: NodeDNS(n.ID),
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return c.cert.Load(), nil
			},
			VerifyConnection: func(cs tls.ConnectionState) error { return checkPin(cs, n.Pin) },
		},
		ForceAttemptHTTP2:   true,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		// Synchronous operations (a scale) answer within minutes; a node
		// that never answers mustn't hold a request forever.
		ResponseHeaderTimeout: 15 * time.Minute,
		IdleConnTimeout:       5 * time.Minute,
		MaxIdleConnsPerHost:   4,
	}
	c.transports[key] = t
	return t
}

// checkPin refuses a peer whose key isn't the pinned one ("" pins nothing).
func checkPin(cs tls.ConnectionState, pin string) error {
	if pin == "" {
		return nil
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	h, err := KeyHash(cs.PeerCertificates[0].PublicKey)
	if err != nil {
		return err
	}
	if !secretEqual([]byte(hex.EncodeToString(h[:])), []byte(pin)) {
		return errors.New("the server's key isn't the one paired under this node ID")
	}
	return nil
}

// Forget drops a node's connections (removed, or its address changed).
func (c *Client) Forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, t := range c.transports {
		if strings.HasPrefix(k, id+"|") {
			t.CloseIdleConnections()
			delete(c.transports, k)
		}
	}
}

// Do sends a request to a node.
func (c *Client) Do(n Endpoint, req *http.Request) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host = "https", n.Address
	return c.transport(n).RoundTrip(req)
}

// Call sends a JSON request (body may be nil) and decodes a JSON answer
// into out (may be nil). Non-2xx answers are *StatusError.
func (c *Client) Call(ctx context.Context, n Endpoint, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+n.Address+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(n, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &StatusError{Code: resp.StatusCode, Msg: string(msg)}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// Identity is who the control plane acts for when it forwards a request:
// the node applies the same role checks and binds job secrets to owner.
type Identity struct {
	Name  string
	Role  string
	Owner string
	// IP is the client's address as the control plane saw it (audit log).
	IP string
}

// Forwarded identity headers. Only the control plane's certificate may
// set them; nodes ignore them on every other connection.
const (
	HeaderActor = "X-Wpgenie-Actor"
	HeaderRole  = "X-Wpgenie-Role"
	HeaderOwner = "X-Wpgenie-Owner"
	HeaderIP    = "X-Wpgenie-Client-Ip"
)

// ReverseProxy forwards a panel API request to a node as id. Credentials
// the browser or CLI sent to the control plane (session cookie, API token)
// never leave it.
func (c *Client) ReverseProxy(n Endpoint, id Identity) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "https", Host: n.Address}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = n.Address
			h := pr.Out.Header
			h.Del("Cookie")
			h.Del("Authorization")
			h.Del(HeaderActor)
			h.Del(HeaderRole)
			h.Del(HeaderOwner)
			h.Del(HeaderIP)
			h.Set(HeaderActor, id.Name)
			h.Set(HeaderIP, id.IP)
			h.Set(HeaderRole, id.Role)
			h.Set(HeaderOwner, id.Owner)
		},
		Transport:     c.transport(n),
		FlushInterval: -1, // downloads and job output stream through
		// The answer is served on the panel's own origin: whatever a node
		// (compromised, say) sends must not become a page there. JSON stays
		// JSON; anything else is a download, sandboxed, never sniffed, and
		// no node sets cookies on the panel.
		ModifyResponse: func(resp *http.Response) error {
			h := resp.Header
			h.Del("Set-Cookie")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
			if !strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "application/json") {
				h.Set("Content-Type", "application/octet-stream")
				if !strings.HasPrefix(strings.ToLower(h.Get("Content-Disposition")), "attachment") {
					h.Set("Content-Disposition", "attachment")
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": "node " + n.ID + " unreachable: " + err.Error()})
		},
	}
}
