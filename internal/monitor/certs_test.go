package monitor

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

// tlsServer answers handshakes with the certificate for the requested
// name, like Caddy; unknown names get no certificate (handshake fails).
func tlsServer(t *testing.T, certs map[string]tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if c, ok := certs[h.ServerName]; ok {
			return &c, nil
		}
		return nil, nil
	}})
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
			go func() {
				c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestTLSChecker(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	in30 := time.Now().Add(30 * 24 * time.Hour)
	addr := tlsServer(t, map[string]tls.Certificate{
		"good.test":      ca.leaf(t, in30, "good.test", "www.good.test"),
		"wrong.test":     ca.leaf(t, in30, "other.test"),
		"untrusted.test": other.leaf(t, in30, "untrusted.test"),
		"expired.test":   ca.leaf(t, time.Now().Add(-time.Hour), "expired.test"),
	})
	c := &TLSChecker{Addr: addr, Roots: ca.pool}
	cases := []struct {
		domain  string
		chain   bool
		served  bool
		invalid string
	}{
		{"good.test", true, true, ""},
		{"wrong.test", true, true, "doesn't cover wrong.test"},
		{"untrusted.test", true, true, "not trusted"},
		// An uploaded origin certificate: names and dates only.
		{"untrusted.test", false, true, ""},
		// Expiry is judged by date (its own alert), not as untrusted.
		{"expired.test", true, true, ""},
		{"nocert.test", true, false, ""},
	}
	for _, tc := range cases {
		r := c.Check(context.Background(), CertTarget{Domain: tc.domain, VerifyChain: tc.chain})
		if r.Served != tc.served || !strings.HasPrefix(r.Invalid, tc.invalid) || (tc.invalid == "") != (r.Invalid == "") {
			t.Errorf("%s (chain %v): %+v", tc.domain, tc.chain, r)
		}
		if tc.domain == "good.test" && (r.NotAfter.Sub(in30).Abs() > time.Second || r.Issuer != "Test CA") {
			t.Errorf("good.test: %+v", r)
		}
	}
	// Nothing listening (Caddy down): not served, not judged here.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	if r := (&TLSChecker{Addr: closed}).Check(context.Background(), CertTarget{Domain: "good.test"}); r.Served {
		t.Errorf("closed port: %+v", r)
	}
}

func TestCertFinding(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	set := DefaultSettings()
	day := 24 * time.Hour
	cases := []struct {
		name string
		r    CertResult
		sev  string
		unk  bool
	}{
		{"not served", CertResult{}, "", true},
		{"valid", CertResult{Served: true, NotAfter: now.Add(60 * day)}, "", false},
		{"two weeks", CertResult{Served: true, NotAfter: now.Add(13 * day)}, SevWarning, false},
		{"two days", CertResult{Served: true, NotAfter: now.Add(2 * day)}, SevCritical, false},
		{"expired", CertResult{Served: true, NotAfter: now.Add(-time.Hour)}, SevCritical, false},
		{"wrong name", CertResult{Served: true, NotAfter: now.Add(60 * day), Invalid: "doesn't cover x"}, SevCritical, false},
	}
	for _, c := range cases {
		f := certFinding(now, set, "example.com", c.r)
		if f.severity != c.sev || f.unknown != c.unk || f.key != "certificate:example.com" {
			t.Errorf("%s: %+v", c.name, f)
		}
	}
}

func TestDiskFindings(t *testing.T) {
	set := DefaultSettings()
	const g = 1 << 30
	hosts := []Host{
		{Disks: []Disk{{Path: "/var/lib/wpgenie", Size: 100 * g, Free: 20 * g, Avail: 15 * g}}}, // 80/95 = 84.2%
		{Node: "n2", Disks: []Disk{
			{Path: "/data", Size: 100 * g, Free: 10 * g, Avail: 10 * g}, // 90%
			{Path: "/var/log", Size: 100 * g, Free: 5 * g, Avail: 0},    // 100%: only root's reserve left
			{Path: "/empty", Size: 0, Free: 0, Avail: 0}}},
	}
	want := map[string]string{"disk:/var/lib/wpgenie": "", "disk:n2:/data": SevWarning, "disk:n2:/var/log": SevCritical, "disk:n2:/empty": ""}
	got := diskFindings(set, hosts)
	if len(got) != len(want) {
		t.Fatalf("findings: %+v", got)
	}
	for _, f := range got {
		if sev, ok := want[f.key]; !ok || sev != f.severity {
			t.Errorf("%s: %q (%s)", f.key, f.severity, f.message)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail := parseMeminfo([]byte("MemTotal:        2048000 kB\nMemFree:          100000 kB\nMemAvailable:    1024000 kB\n"))
	if total != 2048000*1024 || avail != 1024000*1024 {
		t.Errorf("%d %d", total, avail)
	}
}
