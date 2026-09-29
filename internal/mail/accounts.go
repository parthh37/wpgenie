package mail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/parthh37/wpgenie/internal/domain"
	"github.com/parthh37/wpgenie/internal/store"
)

// DNSRecord is a record a mail domain needs, with what DNS currently says.
type DNSRecord struct {
	Purpose string `json:"purpose"` // mx, spf, dkim, dmarc
	Type    string `json:"type"`
	Name    string `json:"name"`
	Value   string `json:"value"`
	// Status: ok, missing, mismatch, pending (DKIM before the server ran),
	// or unknown (not checked).
	Status string `json:"status"`
	Found  string `json:"found,omitempty"`
}

type DomainInfo struct {
	Domain    string      `json:"domain"`
	Records   []DNSRecord `json:"records"`
	Mailboxes int         `json:"mailboxes"`
}

// Resolver is the DNS lookup subset CheckDNS uses (net.DefaultResolver).
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

var localRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._+-]{0,62}[a-z0-9])?$`)

func (s *Service) requireEnabled() error {
	if !s.current().Enabled {
		return ErrDisabled
	}
	return nil
}

// AddDomain accepts mail for a domain. Its DKIM key is generated as soon as
// the mail server runs; DNS records to publish are in Domain().
func (s *Service) AddDomain(ctx context.Context, d string) (*DomainInfo, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	d, err := domain.Normalize(d)
	if err != nil {
		return nil, fmt.Errorf("%w: domain", ErrInvalid)
	}
	s.mu.Lock()
	err = s.Store.AddMailDomain(ctx, d)
	s.mu.Unlock()
	if errors.Is(err, store.ErrExists) {
		return nil, fmt.Errorf("%w: %s is already a mail domain", ErrConflict, d)
	}
	if err != nil {
		return nil, err
	}
	s.Reconcile(ctx) // generates the DKIM key if the server is running
	return s.Domain(ctx, d, false)
}

// DeleteDomain stops accepting mail for a domain with no mailboxes or
// aliases left.
func (s *Service) DeleteDomain(ctx context.Context, d string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Store.DeleteMailDomain(ctx, d); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Service) Domains(ctx context.Context) ([]DomainInfo, error) {
	ds, err := s.Store.MailDomains(ctx)
	if err != nil {
		return nil, err
	}
	out := []DomainInfo{}
	for _, d := range ds {
		info, err := s.Domain(ctx, d.Domain, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *info)
	}
	return out, nil
}

// Domain returns a domain's required DNS records; with check, each is
// compared against live DNS.
func (s *Service) Domain(ctx context.Context, d string, check bool) (*DomainInfo, error) {
	ok, err := s.Store.MailDomainExists(ctx, d)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, store.ErrNotFound
	}
	boxes, err := s.Store.Mailboxes(ctx, d)
	if err != nil {
		return nil, err
	}
	info := &DomainInfo{Domain: d, Mailboxes: len(boxes), Records: s.records(d)}
	if check {
		s.checkDNS(ctx, net.DefaultResolver, info.Records)
	}
	return info, nil
}

func (s *Service) records(d string) []DNSRecord {
	host := s.current().Hostname
	spf := "v=spf1 mx -all"
	if r := s.current().Relay; r != nil {
		// Mail leaves through the relay; its SPF include is provider-specific.
		spf = "v=spf1 mx include:<your relay provider's SPF domain> -all"
	}
	dkim := DNSRecord{Purpose: "dkim", Type: "TXT", Name: selector + "._domainkey." + d, Status: "unknown"}
	if v, err := s.dkimRecord(d); err == nil {
		dkim.Value = v
	} else {
		dkim.Status, dkim.Value = "pending", "(generated when the mail server first runs)"
	}
	return []DNSRecord{
		{Purpose: "mx", Type: "MX", Name: d, Value: "10 " + host, Status: "unknown"},
		{Purpose: "spf", Type: "TXT", Name: d, Value: spf, Status: "unknown"},
		dkim,
		{Purpose: "dmarc", Type: "TXT", Name: "_dmarc." + d, Value: "v=DMARC1; p=quarantine; adkim=r; aspf=r", Status: "unknown"},
	}
}

func (s *Service) dkimPath(d string) string {
	return filepath.Join(s.configDir(), dkimDir, "rsa-2048-"+selector+"-"+d+".public.dns.txt")
}

func (s *Service) dkimRecord(d string) (string, error) {
	b, err := os.ReadFile(s.dkimPath(d))
	if err != nil {
		return "", err
	}
	v := strings.Join(strings.Fields(string(b)), " ")
	if !strings.HasPrefix(v, "v=DKIM1") {
		return "", fmt.Errorf("unexpected DKIM record format in %s", s.dkimPath(d))
	}
	return v, nil
}

// ensureDKIM generates keys for domains that have none. Rspamd's key tool
// only works inside the running server (it reads the server's runtime
// settings), and it reloads Rspamd itself: no restart. Caller holds mu.
func (s *Service) ensureDKIM(ctx context.Context) error {
	ds, err := s.Store.MailDomains(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range ds {
		if _, err := os.Stat(s.dkimPath(d.Domain)); err == nil {
			continue
		}
		if _, err := s.Docker.Run(ctx, nil, "exec", mailName, "setup", "config", "dkim",
			"keytype", "rsa", "keysize", "2048", "selector", selector, "domain", d.Domain); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Domain, err))
			continue
		}
		s.Log.Info("DKIM key generated", "domain", d.Domain)
	}
	return errors.Join(errs...)
}

func (s *Service) checkDNS(ctx context.Context, r Resolver, recs []DNSRecord) {
	for i := range recs {
		rec := &recs[i]
		if rec.Status == "pending" {
			continue
		}
		switch rec.Type {
		case "MX":
			mxs, err := r.LookupMX(ctx, rec.Name)
			if err != nil || len(mxs) == 0 {
				rec.Status = "missing"
				continue
			}
			want := strings.Fields(rec.Value)[1]
			var found []string
			rec.Status = "mismatch"
			for _, mx := range mxs {
				h := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
				found = append(found, h)
				if h == want {
					rec.Status = "ok"
				}
			}
			rec.Found = strings.Join(found, ", ")
		case "TXT":
			txts, err := r.LookupTXT(ctx, rec.Name)
			if err != nil {
				rec.Status = "missing"
				continue
			}
			prefix := strings.SplitN(rec.Value, ";", 2)[0]
			prefix = strings.Fields(prefix)[0] // v=spf1 / v=DKIM1 / v=DMARC1
			rec.Status = "missing"
			for _, t := range txts {
				if !strings.HasPrefix(t, prefix) {
					continue
				}
				rec.Found = t
				rec.Status = "ok"
				switch rec.Purpose {
				case "dkim": // the key must be exactly ours
					if normTXT(t) != normTXT(rec.Value) {
						rec.Status = "mismatch"
					}
				case "spf": // any SPF that lets the MX send is fine
					if !strings.Contains(t, " mx") && !strings.Contains(t, s.current().Hostname) {
						rec.Status = "mismatch"
					}
				}
				break
			}
		}
	}
}

func normTXT(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// MailboxInput creates a mailbox. An empty password generates one.
type MailboxInput struct {
	Address  string `json:"address"`
	Password string `json:"password"`
	QuotaMB  int    `json:"quota_mb"`
}

func validatePassword(p string) error {
	if len(p) < 12 || len(p) > 128 {
		return fmt.Errorf("%w: password must be 12 to 128 characters", ErrInvalid)
	}
	// docker-mailserver reads it with `read`, which strips surrounding
	// whitespace, and it travels as one line.
	if strings.TrimSpace(p) != p || strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("%w: password must not start or end with spaces or contain control characters", ErrInvalid)
	}
	return nil
}

func splitAddress(a string) (local, d string, err error) {
	a = strings.ToLower(strings.TrimSpace(a))
	local, d, ok := strings.Cut(a, "@")
	if !ok || !localRe.MatchString(local) {
		return "", "", fmt.Errorf("%w: address %q", ErrInvalid, a)
	}
	if d, err = domain.Normalize(d); err != nil {
		return "", "", fmt.Errorf("%w: address %q", ErrInvalid, a)
	}
	return local, d, nil
}

// CreateMailbox creates a mailbox and returns its password (shown once,
// never stored by the panel).
func (s *Service) CreateMailbox(ctx context.Context, in MailboxInput) (*store.Mailbox, string, error) {
	return s.createMailbox(ctx, in, "")
}

func (s *Service) createMailbox(ctx context.Context, in MailboxInput, siteID string) (*store.Mailbox, string, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, "", err
	}
	local, d, err := splitAddress(in.Address)
	if err != nil {
		return nil, "", err
	}
	addr := local + "@" + d
	if in.Password == "" {
		in.Password = randString(20)
	}
	if err := validatePassword(in.Password); err != nil {
		return nil, "", err
	}
	if in.QuotaMB < 0 || in.QuotaMB > 1<<20 {
		return nil, "", fmt.Errorf("%w: quota_mb", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok, err := s.Store.MailDomainExists(ctx, d); err != nil {
		return nil, "", err
	} else if !ok {
		return nil, "", fmt.Errorf("%w: %s is not a mail domain; add it first", ErrInvalid, d)
	}
	aliases, err := s.Store.MailAliases(ctx)
	if err != nil {
		return nil, "", err
	}
	if slices.ContainsFunc(aliases, func(a store.MailAlias) bool { return a.Alias == addr }) {
		return nil, "", fmt.Errorf("%w: %s is an alias", ErrConflict, addr)
	}
	m := store.Mailbox{Address: addr, Domain: d, QuotaMB: in.QuotaMB, SiteID: siteID}
	if err := s.Store.AddMailbox(ctx, m); errors.Is(err, store.ErrExists) {
		return nil, "", fmt.Errorf("%w: %s already exists", ErrConflict, addr)
	} else if err != nil {
		return nil, "", err
	}
	// The password goes in on stdin, twice (the tool asks to confirm it).
	pw := strings.NewReader(in.Password + "\n" + in.Password + "\n")
	if err := s.setup(ctx, pw, "email", "add", addr); err != nil {
		s.Store.DeleteMailbox(context.WithoutCancel(ctx), addr)
		return nil, "", err
	}
	if in.QuotaMB > 0 {
		if err := s.setup(ctx, nil, "quota", "set", addr, strconv.Itoa(in.QuotaMB)+"M"); err != nil {
			s.Log.Warn("setting mailbox quota", "address", addr, "err", err)
		}
	}
	// The first mailbox is what lets the server start.
	st := s.reconcileLocked(ctx)
	s.status.Store(&st)
	return &m, in.Password, nil
}

// SetPassword sets a new password (generated if empty) and returns it.
func (s *Service) SetPassword(ctx context.Context, address, password string) (string, error) {
	if err := s.requireEnabled(); err != nil {
		return "", err
	}
	if password == "" {
		password = randString(20)
	}
	if err := validatePassword(password); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Store.GetMailbox(ctx, address); err != nil {
		return "", err
	}
	pw := strings.NewReader(password + "\n" + password + "\n")
	if err := s.setup(ctx, pw, "email", "update", address); err != nil {
		return "", err
	}
	return password, nil
}

func (s *Service) SetQuota(ctx context.Context, address string, quotaMB int) error {
	if quotaMB < 0 || quotaMB > 1<<20 {
		return fmt.Errorf("%w: quota_mb", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Store.GetMailbox(ctx, address); err != nil {
		return err
	}
	var err error
	if quotaMB == 0 {
		err = s.setup(ctx, nil, "quota", "del", address)
	} else {
		err = s.setup(ctx, nil, "quota", "set", address, strconv.Itoa(quotaMB)+"M")
	}
	if err != nil {
		return err
	}
	return s.Store.SetMailboxQuota(ctx, address, quotaMB)
}

// DeleteMailbox deletes a mailbox and all its mail. Mailboxes that are a
// site's WordPress sender are removed by turning that off on the site.
func (s *Service) DeleteMailbox(ctx context.Context, address string) error {
	m, err := s.Store.GetMailbox(ctx, address)
	if err != nil {
		return err
	}
	if m.SiteID != "" {
		// Allowed once the site is gone (a deletion that couldn't reach the
		// mail server would otherwise strand the mailbox forever).
		if _, err := s.Store.GetSite(ctx, m.SiteID); err == nil {
			return fmt.Errorf("%w: %s is site %s's WordPress sender; turn off WordPress mail on the site instead",
				ErrConflict, address, m.SiteID)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return s.deleteMailbox(ctx, address)
}

func (s *Service) deleteMailbox(ctx context.Context, address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setup(ctx, nil, "email", "del", "-y", address); err != nil {
		return err
	}
	return s.Store.DeleteMailbox(ctx, address)
}

func (s *Service) Mailboxes(ctx context.Context) ([]store.Mailbox, error) {
	return s.Store.Mailboxes(ctx, "")
}

// AddAlias forwards mail for alias (on a mail domain) to target (any
// address, local or not).
func (s *Service) AddAlias(ctx context.Context, alias, target string) (*store.MailAlias, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	local, d, err := splitAddress(alias)
	if err != nil {
		return nil, err
	}
	tl, td, err := splitAddress(target)
	if err != nil {
		return nil, err
	}
	a := store.MailAlias{Alias: local + "@" + d, Target: tl + "@" + td, Domain: d}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok, err := s.Store.MailDomainExists(ctx, d); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: %s is not a mail domain", ErrInvalid, d)
	}
	if _, err := s.Store.GetMailbox(ctx, a.Alias); err == nil {
		return nil, fmt.Errorf("%w: %s is a mailbox", ErrConflict, a.Alias)
	}
	if err := s.Store.AddMailAlias(ctx, a); errors.Is(err, store.ErrExists) {
		return nil, fmt.Errorf("%w: alias exists", ErrConflict)
	} else if err != nil {
		return nil, err
	}
	if err := s.setup(ctx, nil, "alias", "add", a.Alias, a.Target); err != nil {
		s.Store.DeleteMailAlias(context.WithoutCancel(ctx), a.Alias, a.Target)
		return nil, err
	}
	return &a, nil
}

func (s *Service) DeleteAlias(ctx context.Context, alias, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	aliases, err := s.Store.MailAliases(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(aliases, store.MailAlias{Alias: alias, Target: target, Domain: aliasDomain(alias)}) {
		return store.ErrNotFound
	}
	// Mail server first: if that fails the alias still shows (and still
	// works), so deleting it can be retried.
	if err := s.setup(ctx, nil, "alias", "del", alias, target); err != nil {
		return err
	}
	return s.Store.DeleteMailAlias(ctx, alias, target)
}

func aliasDomain(a string) string {
	_, d, _ := strings.Cut(a, "@")
	return d
}

func (s *Service) Aliases(ctx context.Context) ([]store.MailAlias, error) {
	return s.Store.MailAliases(ctx)
}

// WordPress senders: a site that sends mail through this server gets a
// managed mailbox. If the site's domain already receives mail here, that is
// wordpress@<domain>. Otherwise the site sends as <site-id>@<mail hostname>:
// registering its domain here would make this server treat every address
// on it as local, so mail to admin@<domain> hosted elsewhere (Google, a
// hosting provider) would bounce as "user unknown".

func (s *Service) senderAddress(ctx context.Context, siteID, d string) (string, string, error) {
	ok, err := s.Store.MailDomainExists(ctx, d)
	if err != nil {
		return "", "", err
	}
	if ok {
		return "wordpress@" + d, d, nil
	}
	host := s.current().Hostname
	return siteID + "@" + host, host, nil
}

// EnsureSender creates (or re-keys) a site's sender mailbox and returns its
// address and password.
func (s *Service) EnsureSender(ctx context.Context, siteID, d string) (string, string, error) {
	if err := s.requireEnabled(); err != nil {
		return "", "", err
	}
	if existing, err := s.senderOf(ctx, siteID); err != nil {
		return "", "", err
	} else if existing != nil {
		// The panel never stores passwords: issue a new one.
		pw, err := s.SetPassword(ctx, existing.Address, "")
		return existing.Address, pw, err
	}
	addr, mailDomain, err := s.senderAddress(ctx, siteID, d)
	if err != nil {
		return "", "", err
	}
	if ok, err := s.Store.MailDomainExists(ctx, mailDomain); err != nil {
		return "", "", err
	} else if !ok {
		// Only ever the mail hostname itself, which this server owns.
		if _, err := s.AddDomain(ctx, mailDomain); err != nil && !errors.Is(err, ErrConflict) {
			return "", "", err
		}
	}
	if _, err := s.Store.GetMailbox(ctx, addr); err == nil {
		return "", "", fmt.Errorf("%w: %s already exists as a regular mailbox", ErrConflict, addr)
	}
	_, pw, err := s.createMailbox(ctx, MailboxInput{Address: addr, QuotaMB: 256}, siteID)
	return addr, pw, err
}

func (s *Service) senderOf(ctx context.Context, siteID string) (*store.Mailbox, error) {
	boxes, err := s.Store.Mailboxes(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, m := range boxes {
		if m.SiteID == siteID {
			return &m, nil
		}
	}
	return nil, nil
}

// RemoveSender deletes a site's sender mailbox, if it has one.
func (s *Service) RemoveSender(ctx context.Context, siteID, _ string) error {
	m, err := s.senderOf(ctx, siteID)
	if err != nil || m == nil {
		return err
	}
	return s.deleteMailbox(ctx, m.Address)
}
