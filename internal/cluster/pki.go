// Package cluster connects WPGenie servers: the control plane (the panel)
// and nodes running `wpgenie agent`. Every node is a complete WPGenie data
// plane for the sites placed on it (Caddy, PHP, MariaDB, Valkey, its own
// daemon loops); the control plane keeps the registry of which site lives
// where and forwards site operations to the right node.
//
// All traffic between servers is HTTP/2 over mutual TLS with a private
// certificate authority that only the control plane holds. A node's
// identity is a DNS name in its certificate (<id>.nodes.wpgenie), so
// verifying the chain against a per-node ServerName is also what checks
// that a connection reached the node it was meant for.
package cluster

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// LocalNode is the control plane's own node: sites that live on the
	// panel's server, as on a single-server install.
	LocalNode = "local"

	controlDNS    = "control.wpgenie"
	nodeDNSSuffix = ".nodes.wpgenie"

	caValidity   = 20 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	// RenewBefore: certificates are reissued this long before they expire
	// (the control plane checks every node on each health pass).
	RenewBefore = 60 * 24 * time.Hour
)

// NodeDNS is the name in a node's certificate, and the TLS server name
// used to reach it.
func NodeDNS(id string) string { return id + nodeDNSSuffix }

// Peer is the verified identity on the other end of a connection.
type Peer struct {
	// Node is the peer's node ID ("local" for the control plane).
	Node string
	// Control is set only for the control plane's certificate.
	Control bool
}

// PeerOf reads the identity of a verified client certificate. It trusts
// only a chain the TLS handshake verified against the cluster CA.
func PeerOf(cs *tls.ConnectionState) (Peer, bool) {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return Peer{}, false
	}
	var p Peer
	for _, name := range cs.VerifiedChains[0][0].DNSNames {
		switch {
		case name == controlDNS:
			p.Control = true
		case strings.HasSuffix(name, nodeDNSSuffix):
			p.Node = strings.TrimSuffix(name, nodeDNSSuffix)
		}
	}
	return p, p.Node != ""
}

// ValidNodeID: node IDs end up in DNS names, container labels and paths.
// ReservedNodeID: IDs a new server can't take. Log shipping names the
// panel's objects "panel" and an unpaired server's "unpaired" (see
// internal/logship); a node called either would mix its logs with them.
// Checked when adding a server only: an existing one keeps its ID.
func ReservedNodeID(id string) bool { return id == "panel" || id == "unpaired" }

func ValidNodeID(id string) bool {
	if len(id) < 1 || len(id) > 32 || id == LocalNode {
		return false
	}
	for i, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(id)-1:
		default:
			return false
		}
	}
	return true
}

// CA is the cluster's certificate authority. Its key never leaves the
// control plane: nodes only ever receive the certificate.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

// LoadOrCreateCA reads ca.pem and ca.key from dir, creating them (and dir,
// root-only) the first time a node is added.
func LoadOrCreateCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	if _, err := os.Stat(certPath); err == nil {
		return LoadCA(dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "WPGenie cluster CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	// Key first: a certificate without its key would be loaded next time.
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return LoadCA(dir)
}

// LoadCA reads an existing CA; fs.ErrNotExist if the cluster has none yet.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, err
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("ca.pem: %w", err)
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca.key: %w", err)
	}
	if !cert.IsCA || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("ca.pem and ca.key don't belong together")
	}
	return &CA{cert: cert, key: key, pem: certPEM}, nil
}

// PEM is the CA certificate, what nodes trust.
func (c *CA) PEM() []byte { return c.pem }

// Pool trusts only this CA.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// Issue signs a certificate for pub: a node's (<node>.nodes.wpgenie), or the
// control plane's (also control.wpgenie). Every certificate is usable both
// as a server and as a client: nodes dial each other for tunnels.
func (c *CA) Issue(pub crypto.PublicKey, node string, control bool) (certPEM []byte, notAfter time.Time, err error) {
	names := []string{NodeDNS(node)}
	if control {
		names = append(names, controlDNS)
	}
	notAfter = time.Now().Add(leafValidity).Truncate(time.Second)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: names[len(names)-1]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.key)
	if err != nil {
		return nil, time.Time{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), notAfter, nil
}

// ControlIdentity loads the control plane's key and certificate from dir,
// creating the key once and (re)issuing the certificate when it is missing
// or close to expiry.
func ControlIdentity(dir string, ca *CA) (*tls.Certificate, error) {
	keyPath, certPath := filepath.Join(dir, "control.key"), filepath.Join(dir, "control.pem")
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(certPath); err == nil {
		if cert, err := parseCert(b); err == nil && time.Until(cert.NotAfter) > RenewBefore &&
			key.PublicKey.Equal(cert.PublicKey) {
			return keyPair(b, key)
		}
	}
	certPEM, _, err := ca.Issue(key.Public(), LocalNode, true)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return keyPair(certPEM, key)
}

// ---- Pairing codes ----

// A pairing code is what `wpgenie agent pair-code` prints on a new node and
// the administrator pastes into the panel: the SHA-256 of the node's public
// key (so the control plane knows it reached that node and no one in the
// middle) and a one-time secret (so the node knows the control plane is the
// one the administrator trusts). Neither side needs the other's network to
// be trusted.
const codePrefix = "wpg1-"

type PairingCode struct {
	KeyHash [32]byte
	Secret  [16]byte
}

func (c PairingCode) String() string {
	return codePrefix + base64.RawURLEncoding.EncodeToString(append(c.KeyHash[:], c.Secret[:]...))
}

func ParsePairingCode(s string) (PairingCode, error) {
	var c PairingCode
	s = strings.TrimSpace(s)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, codePrefix))
	if !strings.HasPrefix(s, codePrefix) || err != nil || len(raw) != 48 {
		return c, errors.New("not a WPGenie pairing code (run `wpgenie agent pair-code` on the new server)")
	}
	copy(c.KeyHash[:], raw[:32])
	copy(c.Secret[:], raw[32:])
	return c, nil
}

// KeyHash is the SHA-256 of a public key's SubjectPublicKeyInfo.
func KeyHash(pub crypto.PublicKey) ([32]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(der), nil
}

func secretEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// ---- helpers ----

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return n
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA key")
	}
	return ek, nil
}

func parseCert(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate PEM block")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		return parseKey(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return key, writeFileAtomic(path, b, 0o600)
}

func keyPair(certPEM []byte, key *ecdsa.PrivateKey) (*tls.Certificate, error) {
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// selfSigned is the certificate an unpaired node presents: the control
// plane pins its key through the pairing code, not the certificate.
func selfSigned(key *ecdsa.PrivateKey) (*tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "unpaired WPGenie node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return keyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key)
}

// writeFileAtomic replaces path in one rename, so a crash never leaves a
// half-written key or certificate.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// certForKey parses a PEM certificate and checks it is for pub.
func certForKey(certPEM []byte, pub *ecdsa.PublicKey) (*x509.Certificate, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, err
	}
	cpub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !cpub.Equal(pub) {
		return nil, errors.New("the certificate is for another key")
	}
	return cert, nil
}
