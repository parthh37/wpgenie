package monitor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Data sources of this server. A multi-node control plane replaces the
// Service's func fields with versions that ask each node (see Service).

// LocalProbe probes sites through this server's Caddy (site.HTTPProber:
// loopback, full TLS verification, the shield's health token).
func LocalProbe(p site.Prober) func(context.Context, *store.Site) site.Health {
	return func(ctx context.Context, st *store.Site) site.Health { return p.Probe(ctx, st.PrimaryDomain) }
}

// CertTarget is one served domain whose certificate is checked.
type CertTarget struct {
	Site   *store.Site
	Domain string
	// VerifyChain is false when the site serves its own certificate that
	// doesn't chain to a public root (a Cloudflare origin certificate
	// behind the proxy): only its names and dates can be judged.
	VerifyChain bool
}

// CertResult is what a TLS client connecting to the domain was given.
type CertResult struct {
	// Served is false when the handshake failed: no certificate yet (DNS
	// doesn't point here) or Caddy unreachable. Neither is judged here:
	// the uptime check covers Caddy being down.
	Served   bool      `json:"served"`
	NotAfter time.Time `json:"not_after"`
	Issuer   string    `json:"issuer"`
	// Invalid says why a client would refuse the certificate ("" if it
	// wouldn't): wrong names, untrusted chain.
	Invalid string `json:"invalid,omitempty"`
}

// TLSChecker does what a visitor's browser does: a TLS handshake with
// Caddy (127.0.0.1:443, SNI = the domain), then verifies what came back,
// ACME and uploaded certificates alike.
type TLSChecker struct {
	Addr  string         // default 127.0.0.1:443
	Roots *x509.CertPool // nil: the system roots
	// Dial, if set, reaches Caddy instead of Addr (another node's, through
	// the cluster tunnel).
	Dial func(ctx context.Context) (net.Conn, error)
}

func (c *TLSChecker) Check(ctx context.Context, t CertTarget) CertResult {
	addr := c.Addr
	if addr == "" {
		addr = "127.0.0.1:443"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg := &tls.Config{
		ServerName: t.Domain, MinVersion: tls.VersionTLS12,
		// Verified below, by hand: an invalid certificate must be reported
		// (with why), not just make the handshake fail.
		InsecureSkipVerify: true,
	}
	var conn *tls.Conn
	if c.Dial != nil {
		raw, err := c.Dial(ctx)
		if err != nil {
			return CertResult{}
		}
		conn = tls.Client(raw, cfg)
		if err := conn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return CertResult{}
		}
	} else {
		d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: cfg}
		nc, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return CertResult{}
		}
		conn = nc.(*tls.Conn)
	}
	defer conn.Close()
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		return CertResult{}
	}
	leaf := chain[0]
	res := CertResult{Served: true, NotAfter: leaf.NotAfter.UTC(), Issuer: leaf.Issuer.CommonName}
	if res.Issuer == "" && len(leaf.Issuer.Organization) > 0 {
		res.Issuer = leaf.Issuer.Organization[0]
	}
	if err := leaf.VerifyHostname(t.Domain); err != nil {
		res.Invalid = "doesn't cover " + t.Domain
		return res
	}
	if !t.VerifyChain {
		return res
	}
	inter := x509.NewCertPool()
	for _, ic := range chain[1:] {
		inter.AddCert(ic)
	}
	// Judge the chain at a moment it is within its dates: expiry has its
	// own, earlier warnings.
	at := time.Now()
	if at.After(leaf.NotAfter) {
		at = leaf.NotAfter.Add(-time.Minute)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: t.Domain, Intermediates: inter, Roots: c.Roots, CurrentTime: at}); err != nil {
		res.Invalid = "not trusted: " + err.Error()
	}
	return res
}

// Host is one server's resources.
type Host struct {
	Node string // "" = this server
	// Memory in bytes; 0 when unknown (not Linux).
	MemTotal     uint64
	MemAvailable uint64
	Disks        []Disk
}

// Disk is a filesystem's space, in bytes.
type Disk struct {
	Path  string
	Size  uint64
	Free  uint64 // including the blocks reserved for root
	Avail uint64 // usable by unprivileged processes (what df shows)
}

// UsedPercent is what df reports: used / (used + available), so the
// blocks reserved for root count as full.
func (d Disk) UsedPercent() float64 {
	used := d.Size - d.Free
	if used+d.Avail == 0 {
		return 0
	}
	return float64(used) / float64(used+d.Avail) * 100
}

// LocalHost reports this server's memory and the filesystems holding
// paths (the data directory: sites, database, backups), each filesystem
// once.
func LocalHost(paths ...string) func(context.Context) ([]Host, error) {
	return func(context.Context) ([]Host, error) {
		h := Host{}
		h.MemTotal, h.MemAvailable = meminfo()
		seen := map[uint64]bool{}
		for _, p := range paths {
			d, dev, err := statfs(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue // not created yet (the access log's directory before Caddy's first line)
			}
			if err != nil {
				return nil, fmt.Errorf("statfs %s: %w", p, err)
			}
			if seen[dev] {
				continue
			}
			seen[dev] = true
			h.Disks = append(h.Disks, d)
		}
		return []Host{h}, nil
	}
}

func statfs(path string) (Disk, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, 0, err
	}
	var fi syscall.Stat_t
	if err := syscall.Stat(path, &fi); err != nil {
		return Disk{}, 0, err
	}
	bs := uint64(st.Bsize)
	return Disk{Path: path, Size: uint64(st.Blocks) * bs, Free: uint64(st.Bfree) * bs, Avail: uint64(st.Bavail) * bs},
		uint64(fi.Dev), nil
}

// meminfo reads MemTotal and MemAvailable (bytes) from /proc/meminfo.
func meminfo() (total, avail uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	return parseMeminfo(b)
}

func parseMeminfo(b []byte) (total, avail uint64) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var kb uint64
		if _, err := fmt.Sscanf(sc.Text(), "MemTotal: %d kB", &kb); err == nil {
			total = kb * 1024
		} else if _, err := fmt.Sscanf(sc.Text(), "MemAvailable: %d kB", &kb); err == nil {
			avail = kb * 1024
		}
	}
	return total, avail
}
