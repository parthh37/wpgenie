package cluster

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Tunnels carry TCP connections between servers inside the cluster's mutual
// TLS: Caddy on a site's home node reaching a PHP-FPM replica on another
// node, replicas there reaching the home node's MariaDB and Valkey, a
// migrated site's old server passing visitors to its new one. Each tunnel
// is one HTTP/2 stream (request body up, response body down), so many
// short FastCGI connections share one TLS connection per pair of servers.
// PHP-FPM, MariaDB and Valkey stay bound to loopback on every server:
// nothing but an authenticated cluster peer can reach them.

const tunnelPath = "/cluster/v1/tunnel"

// Tunnel targets a node serves (the port is appended for "fpm" and
// "ingress" is Caddy's migration listener; see proxy.IngressAddr).
const (
	TargetMariaDB = "mariadb"
	TargetValkey  = "valkey"
	TargetFPM     = "fpm"
	TargetHTTPS   = "https"
	TargetIngress = "ingress"
	TargetHTTP    = "http"
)

// streamConn is a net.Conn over an HTTP/2 request/response pair.
type streamConn struct {
	r      io.ReadCloser // response body: data from the peer
	w      *io.PipeWriter
	cancel context.CancelFunc
	local  net.Addr
	remote net.Addr

	once     sync.Once
	mu       sync.Mutex
	deadline *time.Timer
}

func (c *streamConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *streamConn) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *streamConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		c.r.Close()
		c.cancel()
		c.mu.Lock()
		if c.deadline != nil {
			c.deadline.Stop()
		}
		c.mu.Unlock()
	})
	return nil
}

// CloseWrite ends the upload half (the peer sees EOF) and keeps reading.
func (c *streamConn) CloseWrite() error { return c.w.Close() }

func (c *streamConn) LocalAddr() net.Addr  { return c.local }
func (c *streamConn) RemoteAddr() net.Addr { return c.remote }

// Deadlines close the whole stream when they pass: tunnels are only ever
// piped (io.Copy), where an expired deadline ends the connection anyway.
func (c *streamConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deadline != nil {
		c.deadline.Stop()
		c.deadline = nil
	}
	if !t.IsZero() {
		c.deadline = time.AfterFunc(time.Until(t), func() { c.Close() })
	}
	return nil
}
func (c *streamConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return c.SetDeadline(t) }

type tunnelAddr string

func (a tunnelAddr) Network() string { return "wpgenie-tunnel" }
func (a tunnelAddr) String() string  { return string(a) }

// DialTunnel opens a tunnel to target on node. meta travels as request
// headers (e.g. the client address for the ingress target).
func (c *Client) DialTunnel(ctx context.Context, node Endpoint, target string, meta http.Header) (net.Conn, error) {
	// The stream lives as long as the connection, not the dial context.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, "https://"+node.Address+tunnelPath+"?target="+target, pr)
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range meta {
		req.Header[k] = v
	}
	req.ContentLength = -1
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.transport(node).RoundTrip(req)
		done <- result{resp, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		cancel()
		pw.Close()
		return nil, ctx.Err()
	}
	if res.err != nil {
		cancel()
		pw.Close()
		return nil, res.err
	}
	if res.resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.resp.Body, 512))
		res.resp.Body.Close()
		cancel()
		pw.Close()
		return nil, &StatusError{Code: res.resp.StatusCode, Msg: string(msg)}
	}
	return &streamConn{r: res.resp.Body, w: pw, cancel: cancel,
		local: tunnelAddr("local"), remote: tunnelAddr(node.ID + "/" + target)}, nil
}

// serveTunnel pipes an accepted tunnel to a local TCP address.
func serveTunnel(w http.ResponseWriter, r *http.Request, addr string, log *slog.Logger) {
	var d net.Dialer
	up, err := d.DialContext(r.Context(), "tcp", addr)
	if err != nil {
		http.Error(w, "target unreachable", http.StatusBadGateway)
		return
	}
	defer up.Close()
	// HTTP/1.1 needs this to read the body while writing; HTTP/2 always can.
	_ = http.NewResponseController(w).EnableFullDuplex()
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	go func() {
		io.Copy(up, r.Body)
		if tc, ok := up.(*net.TCPConn); ok {
			tc.CloseWrite() // the peer is done sending; let the target finish
		}
	}()
	// The target closing ends the tunnel, even if the peer never ends its
	// upload: returning closes the request body, which stops the goroutine.
	if _, err := io.Copy(flushWriter{w}, up); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Debug("tunnel ended", "target", addr, "err", err)
	}
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = http.NewResponseController(f.w).Flush()
	}
	return n, err
}

// Forward is a local TCP listener whose connections are tunnelled to a
// target on another server: what Caddy (or a site's PHP) connects to.
type Forward struct {
	ln     net.Listener
	wg     sync.WaitGroup
	closed chan struct{}
}

// Listen starts forwarding connections accepted on addr (e.g.
// 127.0.0.1:19123) to target on node.
func (c *Client) Listen(addr string, node Endpoint, target string, log *slog.Logger) (*Forward, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	f := &Forward{ln: ln, closed: make(chan struct{})}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-f.closed:
					return
				default:
				}
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Warn("tunnel listener", "addr", addr, "err", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer conn.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				remote, err := c.DialTunnel(ctx, node, target, nil)
				cancel()
				if err != nil {
					log.Warn("tunnel", "node", node.ID, "target", target, "err", err)
					return
				}
				defer remote.Close()
				pipe(conn, remote)
			}()
		}
	}()
	return f, nil
}

// Addr is where the forward listens.
func (f *Forward) Addr() string { return f.ln.Addr().String() }

// Close stops accepting; open connections finish on their own.
func (f *Forward) Close() error {
	close(f.closed)
	return f.ln.Close()
}

// pipe connects a local connection to a tunnel. The remote end closing
// ends both (nothing more can come back); the local side closing its write
// half only ends the upload, as TCP would.
func pipe(local, remote net.Conn) {
	go func() {
		io.Copy(remote, local)
		if cw, ok := remote.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	io.Copy(local, remote)
	local.Close()
	remote.Close()
}
