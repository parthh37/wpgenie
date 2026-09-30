package cluster

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testNode struct {
	agent *Agent
	addr  string
}

func startAgent(t *testing.T, a *Agent) *testNode {
	t.Helper()
	if a.Log == nil {
		a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.ServeListener(ctx, ln)
	return &testNode{agent: a, addr: ln.Addr().String()}
}

// controlPlane sets up a CA and the control plane's own identity.
func controlPlane(t *testing.T) (*CA, *Client, *Agent) {
	t.Helper()
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ControlIdentity(dir, ca)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ca.PEM(), cert)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Dir: dir}
	if err := a.SetControlIdentity(ca.PEM(), cert); err != nil {
		t.Fatal(err)
	}
	return ca, client, a
}

func pairNode(t *testing.T, ca *CA, n *testNode, id string, num int64) {
	t.Helper()
	code, err := n.agent.PairingCode()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := ParsePairingCode(code)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Pair(context.Background(), ca, n.addr, pc, id, num); err != nil {
		t.Fatalf("pair %s: %v", id, err)
	}
}

func TestPairingAndForwarding(t *testing.T) {
	ca, control, _ := controlPlane(t)

	var gotIdentity Identity
	node := startAgent(t, &Agent{Dir: t.TempDir(),
		Info: func(context.Context) any { return map[string]string{"hello": "world"} },
		API: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotIdentity, _ = IdentityFrom(r.Context())
			w.Write([]byte(`{"ok":true}`))
		}),
	})

	// Before pairing nothing but /pair answers.
	ep := Endpoint{ID: "n1", Address: node.addr}
	if err := control.Call(context.Background(), ep, http.MethodGet, "/cluster/v1/info", nil, nil); err == nil {
		t.Fatal("an unpaired node's self-signed certificate was accepted")
	}

	pairNode(t, ca, node, "n1", 1)
	if id, num, ok := node.agent.Identity(); !ok || id != "n1" || num != 1 {
		t.Fatalf("identity after pairing: %q %d %v", id, num, ok)
	}
	if _, err := os.Stat(filepath.Join(node.agent.Dir, "pairing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the pairing secret survived pairing")
	}
	var info map[string]string
	if err := control.Call(context.Background(), ep, http.MethodGet, "/cluster/v1/info", nil, &info); err != nil || info["hello"] != "world" {
		t.Fatalf("info: %v %v", info, err)
	}

	// Pairing again is refused, even with a fresh code.
	if _, err := node.agent.PairingCode(); err == nil {
		t.Fatal("a paired node printed a new pairing code")
	}

	// Forwarded API requests carry the acting user.
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/sites", nil)
	rp := control.ReverseProxy(ep, Identity{Name: "alice", Role: "operator", Owner: "user:7"})
	rec := &recorder{header: http.Header{}}
	req.Header.Set("Cookie", "wpgenie_session=secret")
	req.Header.Set(HeaderRole, "admin") // a client can't choose its role
	rp.ServeHTTP(rec, req)
	if rec.status != http.StatusOK || gotIdentity != (Identity{Name: "alice", Role: "operator", Owner: "user:7"}) {
		t.Fatalf("forwarded: %d %+v", rec.status, gotIdentity)
	}

	// A restarted agent comes back paired.
	again := &Agent{Dir: node.agent.Dir, Log: node.agent.Log}
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if id, _, ok := again.Identity(); !ok || id != "n1" {
		t.Fatal("pairing didn't persist")
	}
}

func TestPairingRefusals(t *testing.T) {
	ca, _, _ := controlPlane(t)
	node := startAgent(t, &Agent{Dir: t.TempDir()})
	code, err := node.agent.PairingCode()
	if err != nil {
		t.Fatal(err)
	}
	pc, _ := ParsePairingCode(code)

	// Another server's key (the wrong address, or someone in between).
	other := startAgent(t, &Agent{Dir: t.TempDir()})
	if _, _, err := Pair(context.Background(), ca, other.addr, pc, "n1", 1); err == nil ||
		!strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("pairing with a server holding another key: %v", err)
	}
	// The right server, the wrong secret.
	bad := pc
	bad.Secret[0] ^= 1
	if _, _, err := Pair(context.Background(), ca, node.addr, bad, "n1", 1); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if _, _, ok := node.agent.Identity(); ok {
		t.Fatal("paired with a wrong secret")
	}
	if _, err := ParsePairingCode("wpg1-short"); err == nil {
		t.Fatal("malformed code parsed")
	}
}

func TestNodeIdentities(t *testing.T) {
	ca, control, _ := controlPlane(t)
	echo := echoServer(t)
	var peersSaw Peer
	mk := func() *testNode {
		return startAgent(t, &Agent{Dir: t.TempDir(),
			API: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
			Peers: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				peersSaw, _ = PeerFrom(r.Context())
				w.WriteHeader(http.StatusNoContent)
			}),
			Tunnel: func(p Peer, target string) (string, bool) {
				if target == "echo" && (p.Control || p.Node == "n1") {
					return echo, true
				}
				return "", false
			},
		})
	}
	n1, n2 := mk(), mk()
	pairNode(t, ca, n1, "n1", 1)
	pairNode(t, ca, n2, "n2", 2)
	ctx := context.Background()
	e2 := Endpoint{ID: "n2", Address: n2.addr}

	// n1 may use n2's peer API and tunnels, not its control-plane API.
	c1 := n1.agent.Client()
	if err := c1.Call(ctx, e2, http.MethodPost, "/cluster/v1/peer/x", nil, nil); err != nil || peersSaw.Node != "n1" || peersSaw.Control {
		t.Fatalf("peer call: %v %+v", err, peersSaw)
	}
	var se *StatusError
	if err := c1.Call(ctx, e2, http.MethodGet, "/api/v1/sites", nil, nil); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("a node reached another node's panel API: %v", err)
	}
	if err := c1.Call(ctx, e2, http.MethodGet, "/cluster/v1/info", nil, nil); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("a node reached another node's control endpoint: %v", err)
	}
	// Dialling n2 while expecting n1's identity fails: the name is checked.
	if err := control.Call(ctx, Endpoint{ID: "n1", Address: n2.addr}, http.MethodGet, "/cluster/v1/info", nil, nil); err == nil {
		t.Fatal("n2 passed as n1")
	}

	// Tunnels: n1 and the control plane may reach "echo" on n2; n2 may not
	// reach it on n1 (the Tunnel func only allows n1 and the control plane).
	for _, c := range []*Client{c1, control} {
		conn, err := c.DialTunnel(ctx, e2, "echo", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("ping\n")); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || line != "ping\n" {
			t.Fatalf("echo through tunnel: %q %v", line, err)
		}
		conn.Close()
	}
	if _, err := n2.agent.Client().DialTunnel(ctx, Endpoint{ID: "n1", Address: n1.addr}, "echo", nil); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("disallowed tunnel: %v", err)
	}

	// A local forward: connections to it come out at the target.
	fw, err := control.Listen("127.0.0.1:0", e2, "echo", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	for range 3 {
		conn, err := net.Dial("tcp", fw.Addr())
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("hello\n"))
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || line != "hello\n" {
			t.Fatalf("forward: %q %v", line, err)
		}
		conn.Close()
	}

	// Renewal swaps the certificate without re-pairing.
	before := n2.agent.CertNotAfter()
	time.Sleep(1100 * time.Millisecond) // certificates have second precision
	if _, err := control.Renew(ctx, ca, e2); err != nil {
		t.Fatal(err)
	}
	if !n2.agent.CertNotAfter().After(before) {
		t.Fatal("certificate not renewed")
	}
	if err := control.Call(ctx, e2, http.MethodGet, "/cluster/v1/info", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTunnelTargetClosing(t *testing.T) {
	ca, control, _ := controlPlane(t)
	// A target that answers once and closes, like PHP-FPM after a request.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("bye"))
			c.Close()
		}
	}()
	n := startAgent(t, &Agent{Dir: t.TempDir(), Tunnel: func(Peer, string) (string, bool) { return ln.Addr().String(), true }})
	pairNode(t, ca, n, "n1", 1)
	fw, err := control.Listen("127.0.0.1:0", Endpoint{ID: "n1", Address: n.addr}, "x", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	conn, err := net.Dial("tcp", fw.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// We never close our side: the target closing must still end it.
	b, err := io.ReadAll(conn)
	if err != nil || string(b) != "bye" {
		t.Fatalf("read %q, %v", b, err)
	}
}

func TestNodeIDs(t *testing.T) {
	for id, ok := range map[string]bool{"n1": true, "web-2": true, "local": false, "-a": false, "a-": false,
		"A": false, "": false, "a.b": false, strings.Repeat("a", 33): false} {
		if ValidNodeID(id) != ok {
			t.Errorf("ValidNodeID(%q) = %v", id, !ok)
		}
	}
}

func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

type recorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(s int) { r.status = s }

// A certificate the CA issued for a node ID to another key (a removed
// server's, still valid) is refused, on the panel's side and on the nodes'.
func TestPinnedKeys(t *testing.T) {
	ca, control, _ := controlPlane(t)
	n1 := startAgent(t, &Agent{Dir: t.TempDir()})
	code1, _ := n1.agent.PairingCode()
	pc1, _ := ParsePairingCode(code1)
	if _, _, err := Pair(context.Background(), ca, n1.addr, pc1, "n1", 1); err != nil {
		t.Fatal(err)
	}
	pin1 := hex.EncodeToString(pc1.KeyHash[:])
	ctx := context.Background()
	if err := control.Call(ctx, Endpoint{ID: "n1", Address: n1.addr, Pin: pin1}, http.MethodGet, "/cluster/v1/info", nil, nil); err != nil {
		t.Fatalf("pinned key: %v", err)
	}
	// Another server once paired as n1 (removed since): its certificate is
	// genuine, its key isn't the one pinned now.
	old := startAgent(t, &Agent{Dir: t.TempDir()})
	codeOld, _ := old.agent.PairingCode()
	pcOld, _ := ParsePairingCode(codeOld)
	if _, _, err := Pair(ctx, ca, old.addr, pcOld, "n1", 9); err != nil {
		t.Fatal(err)
	}
	if err := control.Call(ctx, Endpoint{ID: "n1", Address: old.addr, Pin: pin1}, http.MethodGet, "/cluster/v1/info", nil, nil); err == nil {
		t.Fatal("the panel accepted the old server's certificate for n1")
	}
	// A node only accepts peers the directory lists with that key.
	n2 := startAgent(t, &Agent{Dir: t.TempDir(),
		Peers:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		PeerPin: func(node string) (string, bool) { return pin1, node == "n1" }})
	code2, _ := n2.agent.PairingCode()
	pc2, _ := ParsePairingCode(code2)
	if _, _, err := Pair(ctx, ca, n2.addr, pc2, "n2", 2); err != nil {
		t.Fatal(err)
	}
	e2 := Endpoint{ID: "n2", Address: n2.addr}
	if err := n1.agent.Client().Call(ctx, e2, http.MethodPost, "/cluster/v1/peer/x", nil, nil); err != nil {
		t.Fatalf("n1 (pinned) to n2: %v", err)
	}
	var se *StatusError
	if err := old.agent.Client().Call(ctx, e2, http.MethodPost, "/cluster/v1/peer/x", nil, nil); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("the old n1 reached n2: %v", err)
	}
	// The panel itself isn't pinned by nodes (only it holds the CA).
	if err := control.Call(ctx, e2, http.MethodGet, "/cluster/v1/info", nil, nil); err != nil {
		t.Fatalf("panel to n2: %v", err)
	}
}

// Log shipping names the panel's and an unpaired server's logs "panel"
// and "unpaired": a new server can't take either ID.
func TestReservedNodeIDs(t *testing.T) {
	c := &Controller{}
	for _, in := range []AddNodeInput{{Name: "Panel"}, {Name: "web", ID: "unpaired"}} {
		in.Address, in.PairingCode = "203.0.113.5", "x"
		if _, err := c.AddNode(context.Background(), in); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if !ValidNodeID("panel") {
		t.Error("an existing node called panel must stay valid")
	}
}
