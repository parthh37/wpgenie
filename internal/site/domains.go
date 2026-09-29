package site

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// Domains: a site serves its primary domain and any aliases, and answers
// on redirect domains (www <-> apex, old names) with a permanent redirect
// to the primary one. Changing the primary domain rewrites the site's
// links in its database. A site may bring its own certificate (e.g. a
// Cloudflare origin certificate); otherwise Caddy gets them from Let's
// Encrypt.

// AddDomain attaches a domain to a site, served or redirecting.
func (s *Service) AddDomain(ctx context.Context, id, name string, redirect bool) (*store.Site, error) {
	domain, err := NormalizeDomain(name)
	if err != nil {
		return nil, err
	}
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	if err := s.domainFree(ctx, domain); err != nil {
		return nil, err
	}
	if !redirect {
		if err := s.certCoversNew(ctx, id, domain); err != nil {
			return nil, err
		}
	}
	if err := s.Store.AddDomain(ctx, id, domain, redirect); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		c, cancel := bgCtx()
		defer cancel()
		s.Store.RemoveDomain(c, id, domain)
		return nil, errors.Join(err, s.Sync(c))
	}
	what := "serves"
	if redirect {
		what = "redirects to " + st.PrimaryDomain
	}
	s.event(id, "domain", fmt.Sprintf("Added %s (%s)", domain, what))
	return s.Store.GetSite(ctx, id)
}

// RemoveDomain detaches a domain other than the primary one.
func (s *Service) RemoveDomain(ctx context.Context, id, domain string) (*store.Site, error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if domain == st.PrimaryDomain {
		return nil, fmt.Errorf("%w: the primary domain can't be removed; make another domain primary first", ErrInvalidInput)
	}
	redirect := slices.Contains(st.RedirectDomains, domain)
	if err := s.Store.RemoveDomain(ctx, id, domain); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		c, cancel := bgCtx()
		defer cancel()
		s.Store.AddDomain(c, id, domain, redirect)
		return nil, errors.Join(err, s.Sync(c))
	}
	s.event(id, "domain", "Removed "+domain)
	return s.Store.GetSite(ctx, id)
}

// SetDomainRedirect switches an alias between serving the site and
// redirecting to the primary domain.
func (s *Service) SetDomainRedirect(ctx context.Context, id, domain string, redirect bool) (*store.Site, error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if domain == st.PrimaryDomain {
		return nil, fmt.Errorf("%w: the primary domain is always served", ErrInvalidInput)
	}
	if !redirect {
		if err := s.certCoversNew(ctx, id, domain); err != nil {
			return nil, err
		}
	}
	if err := s.Store.SetDomainRedirect(ctx, id, domain, redirect); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		c, cancel := bgCtx()
		defer cancel()
		s.Store.SetDomainRedirect(c, id, domain, !redirect)
		return nil, errors.Join(err, s.Sync(c))
	}
	return s.Store.GetSite(ctx, id)
}

// StartPrimaryDomain makes an attached domain the site's primary one, as a
// job: links in the database are rewritten to it (WordPress's home and
// siteurl among them) and the old primary domain redirects to it. This is
// also how a site moves between www and the bare domain.
func (s *Service) StartPrimaryDomain(ctx context.Context, id, name string) (int64, error) {
	domain, err := NormalizeDomain(name)
	if err != nil {
		return 0, err
	}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return 0, err
	}
	if st.Status != store.StatusActive {
		return 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	if domain == st.PrimaryDomain {
		return 0, fmt.Errorf("%w: %s is already the primary domain", ErrInvalidInput, domain)
	}
	if !slices.Contains(st.Domains, domain) && !slices.Contains(st.RedirectDomains, domain) {
		return 0, fmt.Errorf("%w: add %s to the site first", ErrInvalidInput, domain)
	}
	if err := s.certCoversNew(ctx, id, domain); err != nil {
		return 0, err
	}
	return s.Jobs.Submit(ctx, s.siteJob(id, "primary-domain", false), func(ctx context.Context, t *jobs.Task) error {
		st, err := s.Store.GetSite(ctx, id)
		if err != nil {
			return err
		}
		old := st.PrimaryDomain
		t.Progress(10, "Rewriting links from "+old+" to "+domain)
		if err := s.searchReplace(ctx, id, old, domain); err != nil {
			return err
		}
		t.Progress(70, "Switching the proxy")
		s.opsMu.Lock()
		err = s.Store.SetPrimaryDomain(ctx, id, domain)
		if err == nil {
			err = s.Sync(ctx)
		}
		s.opsMu.Unlock()
		if err != nil {
			// Put the links back: the proxy still serves the old domain.
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
			defer cancel()
			s.opsMu.Lock()
			s.Store.SetPrimaryDomain(c, id, old)
			s.Sync(c)
			s.opsMu.Unlock()
			return errors.Join(err, s.searchReplace(c, id, domain, old))
		}
		t.Progress(90, "Purging caches")
		st.PrimaryDomain = domain
		if err := s.purgeLocal(ctx, st); err != nil {
			s.Log.Warn("purging after a domain change", "site", id, "err", err)
		}
		if err := s.purgeCDNIfOn(ctx, st); err != nil {
			s.Log.Warn("purging the CDN after a domain change", "site", id, "err", err)
		}
		s.event(id, "domain", fmt.Sprintf("Primary domain %s → %s (%s now redirects)", old, domain, old))
		return nil
	})
}

// ---- Custom certificates ----

// CertInput is a certificate chain and its private key, PEM.
type CertInput struct {
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
}

// certDir holds a site's certificate on the host; Caddy sees Caddy's
// config directory (the Caddyfile's) at /etc/caddy.
func (s *Service) certDir(id string) string {
	return filepath.Join(filepath.Dir(s.Cfg.CaddyfilePath), "certs", id)
}

// parseCert checks a certificate chain and key: they must match, be valid
// now and cover every domain the site serves.
func parseCert(in CertInput, domains []string, now time.Time) (*store.SiteCert, error) {
	pair, err := tls.X509KeyPair([]byte(in.Certificate), []byte(in.Key))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	switch k := pair.PrivateKey.(type) {
	case *rsa.PrivateKey:
		if k.N.BitLen() < 2048 {
			return nil, fmt.Errorf("%w: RSA keys must be at least 2048 bits", ErrInvalidInput)
		}
	case *ecdsa.PrivateKey, ed25519.PrivateKey:
	default:
		return nil, fmt.Errorf("%w: unsupported key type", ErrInvalidInput)
	}
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("%w: the certificate is valid from %s to %s", ErrInvalidInput,
			leaf.NotBefore.Format(time.DateOnly), leaf.NotAfter.Format(time.DateOnly))
	}
	if err := certCovers(leaf, domains); err != nil {
		return nil, err
	}
	// Informational: a Cloudflare origin certificate is fine behind
	// Cloudflare but not trusted by browsers directly.
	inter := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		if c, err := x509.ParseCertificate(der); err == nil {
			inter.AddCert(c)
		}
	}
	_, verr := leaf.Verify(x509.VerifyOptions{Intermediates: inter, CurrentTime: now})
	names := leaf.DNSNames
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = []string{leaf.Subject.CommonName}
	}
	return &store.SiteCert{Names: names, Issuer: leaf.Issuer.String(), NotAfter: leaf.NotAfter.UTC(), Trusted: verr == nil}, nil
}

func certCovers(leaf *x509.Certificate, domains []string) error {
	var missing []string
	for _, d := range domains {
		if leaf.VerifyHostname(d) != nil {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: the certificate doesn't cover %s (a site's certificate must cover every domain it serves)",
			ErrInvalidInput, strings.Join(missing, ", "))
	}
	return nil
}

// certCoversNew checks that a site's own certificate, if it has one, also
// covers a domain about to be served.
func (s *Service) certCoversNew(ctx context.Context, id, domain string) error {
	if _, err := s.Store.SiteCert(ctx, id); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(s.certDir(id), "cert.pem"))
	if err != nil {
		return err
	}
	leaf, err := firstCert(b)
	if err != nil {
		return err
	}
	return certCovers(leaf, []string{domain})
}

// firstCert parses the leaf (first) certificate of a PEM chain.
func firstCert(pemBytes []byte) (*x509.Certificate, error) {
	for {
		var block *pem.Block
		block, pemBytes = pem.Decode(pemBytes)
		if block == nil {
			return nil, errors.New("no certificate in cert.pem")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// caddyGroup is the group Caddy runs as: the owner of its data directory
// (install.sh chowns it). -1 when unknown (development).
func (s *Service) caddyGroup() int {
	fi, err := os.Stat(s.Cfg.CaddyDataDir)
	if err != nil {
		return -1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Gid)
	}
	return -1
}

// SetCert installs a site's own certificate. Caddy serves it for the
// site's domains instead of obtaining one (it doesn't renew it: upload the
// next one before this one expires).
func (s *Service) SetCert(ctx context.Context, id string, in CertInput) (*store.SiteCert, error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := parseCert(in, st.Domains, time.Now())
	if err != nil {
		return nil, err
	}
	c.SiteID = id
	dir := s.certDir(id)
	prevCert, _ := os.ReadFile(filepath.Join(dir, "cert.pem"))
	prevKey, _ := os.ReadFile(filepath.Join(dir, "key.pem"))
	prevMeta, _ := s.Store.SiteCert(ctx, id)
	if err := s.writeCertFiles(dir, []byte(in.Certificate), []byte(in.Key)); err != nil {
		return nil, err
	}
	if err := s.Store.SetSiteCert(ctx, c); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		// Caddy refused it: put back what was there.
		cctx, cancel := bgCtx()
		defer cancel()
		if prevMeta != nil {
			s.writeCertFiles(dir, prevCert, prevKey)
			s.Store.SetSiteCert(cctx, prevMeta)
		} else {
			os.RemoveAll(dir)
			s.Store.DeleteSiteCert(cctx, id)
		}
		return nil, errors.Join(err, s.Sync(cctx))
	}
	s.event(id, "domain", fmt.Sprintf("Own certificate installed (%s, expires %s)", strings.Join(c.Names, ", "),
		c.NotAfter.Format(time.DateOnly)))
	return s.Store.SiteCert(ctx, id)
}

func (s *Service) writeCertFiles(dir string, cert, key []byte) error {
	gid := s.caddyGroup()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	root := os.Geteuid() == 0 && gid >= 0
	if root {
		if err := os.Chown(dir, 0, gid); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		name string
		data []byte
		mode fs.FileMode
	}{{"cert.pem", cert, 0o644}, {"key.pem", key, 0o640}} {
		tmp := filepath.Join(dir, "."+f.name+".tmp")
		if err := os.WriteFile(tmp, f.data, 0o600); err != nil {
			return err
		}
		if root {
			if err := os.Chown(tmp, 0, gid); err != nil {
				return err
			}
		}
		if err := os.Chmod(tmp, f.mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	return nil
}

// RemoveCert goes back to automatic (Let's Encrypt) certificates.
func (s *Service) RemoveCert(ctx context.Context, id string) error {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if err := s.Store.DeleteSiteCert(ctx, id); err != nil {
		return err
	}
	if err := s.Sync(ctx); err != nil {
		return err
	}
	s.event(id, "domain", "Own certificate removed: Caddy obtains certificates automatically again")
	return os.RemoveAll(s.certDir(id))
}

// certExpiryWarnings logs an event, once a day, for uploaded certificates
// expiring within two weeks: nothing renews them.
func (s *Service) certExpiryWarnings(ctx context.Context, now time.Time) {
	certs, err := s.Store.SiteCerts(ctx)
	if err != nil {
		return
	}
	for id, c := range certs {
		if left := c.NotAfter.Sub(now); left < 14*24*time.Hour {
			s.event(id, "domain", fmt.Sprintf("The site's own certificate expires %s: upload a new one",
				c.NotAfter.Format(time.DateOnly)))
		}
	}
}
