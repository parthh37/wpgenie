package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Agent is a server's side of the cluster: on a new node it waits to be
// paired, then serves the mutual-TLS listener the control plane (and, for
// tunnels and replicas, other nodes) connect to. The control plane runs one
// too, as node "local", so peers can tunnel to the sites it hosts.
//
// Files in Dir (root only):
//
//	node.key      this server's private key (never leaves it)
//	pairing.json  the one-time pairing secret, until paired
//	node.pem      its certificate, signed by the cluster CA
//	ca.pem        the cluster CA certificate
//	node.json     its node ID and number
type Agent struct {
	Dir    string
	Listen string // e.g. ":7443"
	Log    *slog.Logger

	// API serves panel API requests the control plane forwards, with the
	// acting user's identity in the request context (IdentityFrom).
	API http.Handler
	// Control serves the control plane's internal operations under
	// /cluster/v1/ (site imports, migrations, configuration pushes).
	Control http.Handler
	// Peers serves what other nodes may ask under /cluster/v1/peer/ (guest
	// replicas); handlers authorise by PeerFrom.
	Peers http.Handler
	// Tunnel resolves a tunnel target for a peer to a local address; false
	// refuses it.
	Tunnel func(p Peer, target string) (addr string, ok bool)
	// Info is this server's state for the control plane's health checks.
	Info func(ctx context.Context) any
	// OnPaired runs after pairing succeeded (the daemon sets up its job
	// numbering from num).
	OnPaired func(id string, num int64)
	// PeerPin is the key a node paired with (from the panel's directory):
	// another node's connection is refused unless its key is that one, so a
	// removed server's certificate (still valid) gets nowhere. nil: no check.
	PeerPin func(node string) (string, bool)

	key   *ecdsa.PrivateKey
	self  *tls.Certificate // presented while unpaired
	state atomic.Pointer[agentState]

	pairMu       sync.Mutex
	pairFailures map[string]int // per source address (someone else can't lock pairing)
}

type agentState struct {
	id     string
	num    int64
	cert   *tls.Certificate
	leaf   *x509.Certificate
	caPEM  []byte
	pool   *x509.CertPool
	client *Client
}

type nodeFile struct {
	ID  string `json:"id"`
	Num int64  `json:"num"`
}

type pairingFile struct {
	Secret string `json:"secret"`
}

// Load reads (or creates) the key and any paired identity.
func (a *Agent) Load() error {
	key, err := loadOrCreateKey(filepath.Join(a.Dir, "node.key"))
	if err != nil {
		return err
	}
	a.key = key
	if a.self, err = selfSigned(key); err != nil {
		return err
	}
	var nf nodeFile
	b, err := os.ReadFile(filepath.Join(a.Dir, "node.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil // not paired yet
	} else if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &nf); err != nil {
		return fmt.Errorf("node.json: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(a.Dir, "ca.pem"))
	if err != nil {
		return err
	}
	certPEM, err := os.ReadFile(filepath.Join(a.Dir, "node.pem"))
	if err != nil {
		return err
	}
	return a.adopt(nf.ID, nf.Num, caPEM, certPEM, false)
}

// SetControlIdentity makes this agent the control plane's (node "local"):
// it is paired by definition, with the CA's own certificate.
func (a *Agent) SetControlIdentity(caPEM []byte, cert *tls.Certificate) error {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	client, err := NewClient(caPEM, cert)
	if err != nil {
		return err
	}
	a.state.Store(&agentState{id: LocalNode, cert: cert, leaf: leaf, caPEM: caPEM, pool: pool, client: client})
	return nil
}

// Identity reports the node ID and number once paired.
func (a *Agent) Identity() (id string, num int64, paired bool) {
	st := a.state.Load()
	if st == nil {
		return "", 0, false
	}
	return st.id, st.num, true
}

// Client dials peers with this node's identity (nil until paired).
func (a *Agent) Client() *Client {
	if st := a.state.Load(); st != nil {
		return st.client
	}
	return nil
}

// CertNotAfter is when this node's certificate expires.
func (a *Agent) CertNotAfter() time.Time {
	if st := a.state.Load(); st != nil {
		return st.leaf.NotAfter
	}
	return time.Time{}
}

// PairingCode returns the code to paste into the panel, creating the
// one-time secret on first use.
func (a *Agent) PairingCode() (string, error) {
	if _, _, paired := a.Identity(); paired {
		return "", errors.New("this server is already part of a cluster")
	}
	if a.key == nil {
		if err := a.Load(); err != nil {
			return "", err
		}
	}
	secret, err := a.pairingSecret(true)
	if err != nil {
		return "", err
	}
	var c PairingCode
	if c.KeyHash, err = KeyHash(a.key.Public()); err != nil {
		return "", err
	}
	copy(c.Secret[:], secret)
	return c.String(), nil
}

func (a *Agent) pairingSecret(create bool) ([]byte, error) {
	path := filepath.Join(a.Dir, "pairing.json")
	var pf pairingFile
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &pf); err != nil {
			return nil, err
		}
		return hex.DecodeString(pf.Secret)
	} else if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, err
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(pairingFile{Secret: hex.EncodeToString(secret)})
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return nil, err
	}
	return secret, writeFileAtomic(path, b, 0o600)
}

// adopt switches to a CA-signed identity after checking it: the
// certificate must be for this server's key and name this node.
func (a *Agent) adopt(id string, num int64, caPEM, certPEM []byte, save bool) error {
	if !ValidNodeID(id) {
		return fmt.Errorf("invalid node ID %q", id)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("invalid CA certificate")
	}
	leaf, err := certForKey(certPEM, &a.key.PublicKey)
	if err != nil {
		return err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: NodeDNS(id), Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	cert, err := keyPair(certPEM, a.key)
	if err != nil {
		return err
	}
	client, err := NewClient(caPEM, cert)
	if err != nil {
		return err
	}
	if save {
		nb, _ := json.Marshal(nodeFile{ID: id, Num: num})
		for _, f := range []struct {
			name string
			data []byte
		}{{"ca.pem", caPEM}, {"node.pem", certPEM}, {"node.json", nb}} {
			if err := writeFileAtomic(filepath.Join(a.Dir, f.name), f.data, 0o600); err != nil {
				return err
			}
		}
	}
	if old := a.state.Load(); old != nil && old.client != nil {
		// Renewal: keep connections, present the new certificate.
		old.client.SetCertificate(cert)
		client = old.client
	}
	a.state.Store(&agentState{id: id, num: num, cert: cert, leaf: leaf, caPEM: caPEM, pool: pool, client: client})
	return nil
}

func (a *Agent) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			st := a.state.Load()
			if st == nil {
				// Unpaired: only /cluster/v1/pair answers, and the
				// control plane pins this key through the pairing code.
				return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*a.self},
					NextProtos: []string{"h2", "http/1.1"}}, nil
			}
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{*st.cert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    st.pool,
				NextProtos:   []string{"h2", "http/1.1"},
			}, nil
		},
	}
}

// Serve runs the cluster listener until ctx ends.
func (a *Agent) Serve(ctx context.Context) error {
	if a.key == nil {
		if err := a.Load(); err != nil {
			return err
		}
	}
	ln, err := net.Listen("tcp", a.Listen)
	if err != nil {
		return err
	}
	return a.ServeListener(ctx, ln)
}

// ServeListener serves on an existing listener (tests).
func (a *Agent) ServeListener(ctx context.Context, ln net.Listener) error {
	if a.key == nil {
		if err := a.Load(); err != nil {
			return err
		}
	}
	srv := &http.Server{
		Handler:           a.Handler(),
		TLSConfig:         a.tlsConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       5 * time.Minute,
		ErrorLog:          slog.NewLogLogger(a.Log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	a.Log.Info("cluster listener", "addr", ln.Addr().String())
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type peerKey struct{}

// PeerFrom is the verified peer of a cluster request.
func PeerFrom(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}

type identityKey struct{}

// IdentityFrom is the panel user the control plane forwarded a request
// for. Only set on requests over a verified control-plane connection.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Handler dispatches cluster requests by path and peer.
func (a *Agent) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cluster/v1/pair" && r.Method == http.MethodPost {
			a.pair(w, r)
			return
		}
		peer, ok := PeerOf(r.TLS)
		if !ok {
			jsonError(w, http.StatusForbidden, "a cluster certificate is required")
			return
		}
		if !peer.Control && a.PeerPin != nil {
			pin, known := a.PeerPin(peer.Node)
			if !known || checkPin(*r.TLS, pin) != nil {
				a.Log.Warn("cluster: refused a node that isn't in the directory with that key", "node", peer.Node)
				jsonError(w, http.StatusForbidden, "unknown node")
				return
			}
		}
		ctx := context.WithValue(r.Context(), peerKey{}, peer)
		r = r.WithContext(ctx)
		switch p := r.URL.Path; {
		case p == tunnelPath && r.Method == http.MethodPost:
			a.tunnel(w, r, peer)
		case strings.HasPrefix(p, "/cluster/v1/peer/"):
			if a.Peers == nil {
				http.NotFound(w, r)
				return
			}
			a.Peers.ServeHTTP(w, r)
		case !peer.Control:
			jsonError(w, http.StatusForbidden, "only the control plane may do this")
		case p == "/cluster/v1/info" && r.Method == http.MethodGet:
			var info any
			if a.Info != nil {
				info = a.Info(ctx)
			}
			writeJSON(w, http.StatusOK, info)
		case p == "/cluster/v1/renew" && r.Method == http.MethodPost:
			a.renew(w, r)
		case strings.HasPrefix(p, "/api/"):
			if a.API == nil {
				http.NotFound(w, r)
				return
			}
			id := Identity{Name: r.Header.Get(HeaderActor), Role: r.Header.Get(HeaderRole),
				Owner: r.Header.Get(HeaderOwner), IP: r.Header.Get(HeaderIP)}
			if id.Name == "" || id.Role == "" {
				jsonError(w, http.StatusBadRequest, "missing forwarded identity")
				return
			}
			a.API.ServeHTTP(w, r.WithContext(context.WithValue(ctx, identityKey{}, id)))
		case strings.HasPrefix(p, "/cluster/v1/"):
			if a.Control == nil {
				http.NotFound(w, r)
				return
			}
			a.Control.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (a *Agent) tunnel(w http.ResponseWriter, r *http.Request, peer Peer) {
	target := r.URL.Query().Get("target")
	if a.Tunnel == nil {
		jsonError(w, http.StatusForbidden, "tunnels are not served here")
		return
	}
	addr, ok := a.Tunnel(peer, target)
	if !ok {
		a.Log.Warn("cluster: tunnel refused", "peer", peer.Node, "target", target)
		jsonError(w, http.StatusForbidden, "tunnel target not allowed")
		return
	}
	serveTunnel(w, r, addr, a.Log)
}

type pairRequest struct {
	Secret string `json:"secret"` // hex
	NodeID string `json:"node_id"`
	Num    int64  `json:"num"`
	CA     string `json:"ca"`   // PEM
	Cert   string `json:"cert"` // PEM, for this node's key
}

// pair accepts the control plane's certificate for this node. Only while
// unpaired, only with the one-time secret, and only a handful of wrong
// guesses per start (the secret is 128 bits; this just keeps logs quiet).
func (a *Agent) pair(w http.ResponseWriter, r *http.Request) {
	a.pairMu.Lock()
	defer a.pairMu.Unlock()
	if _, _, paired := a.Identity(); paired {
		jsonError(w, http.StatusConflict, "already paired")
		return
	}
	src, _, _ := net.SplitHostPort(r.RemoteAddr)
	if a.pairFailures == nil {
		a.pairFailures = map[string]int{}
	}
	if a.pairFailures[src] >= 10 {
		jsonError(w, http.StatusTooManyRequests, "too many failed pairing attempts from this address; restart the agent")
		return
	}
	var in pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	secret, err := a.pairingSecret(false)
	got, herr := hex.DecodeString(in.Secret)
	if err != nil || herr != nil || len(secret) == 0 || !secretEqual(got, secret) {
		a.pairFailures[src]++
		a.Log.Warn("cluster: pairing attempt with a wrong secret", "from", r.RemoteAddr)
		jsonError(w, http.StatusForbidden, "wrong pairing code")
		return
	}
	if err := a.adopt(in.NodeID, in.Num, []byte(in.CA), []byte(in.Cert), true); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	os.Remove(filepath.Join(a.Dir, "pairing.json")) // one use
	a.Log.Info("cluster: paired", "node", in.NodeID)
	if a.OnPaired != nil {
		a.OnPaired(in.NodeID, in.Num)
	}
	var info any
	if a.Info != nil {
		info = a.Info(r.Context())
	}
	writeJSON(w, http.StatusOK, info)
}

type renewRequest struct {
	Cert string `json:"cert"`
}

func (a *Agent) renew(w http.ResponseWriter, r *http.Request) {
	var in renewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	st := a.state.Load()
	if err := a.adopt(st.id, st.num, st.caPEM, []byte(in.Cert), true); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
