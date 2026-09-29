package mail

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/store/storetest"
)

// fakeDocker simulates the docker CLI for the two mail containers and the
// mail server's setup tool (which writes postfix-accounts.cf).
type fakeDocker struct {
	t          *testing.T
	cfgDir     string
	containers map[string]string // name -> spec label
	cmds       [][]string
	stdins     []string
}

func (f *fakeDocker) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, args)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.stdins = append(f.stdins, string(b))
	}
	switch args[0] {
	case "ps":
		var lines []string
		for name, spec := range f.containers {
			lines = append(lines, name+"|"+spec+"|running")
		}
		return []byte(strings.Join(lines, "\n")), nil
	case "rm":
		delete(f.containers, args[2])
		return nil, nil
	case "run":
		if args[1] == "-d" {
			name, spec := args[slices.Index(args, "--name")+1], ""
			for i, a := range args {
				if a == "--label" && strings.HasPrefix(args[i+1], "wpgenie.spec=") {
					spec = strings.TrimPrefix(args[i+1], "wpgenie.spec=")
				}
			}
			f.containers[name] = spec
			return nil, nil
		}
		return f.setup(args[slices.Index(args, "setup")+1:])
	case "exec":
		i := slices.Index(args, "setup")
		return f.setup(args[i+1:])
	}
	return nil, errors.New("unexpected docker " + args[0])
}

func (f *fakeDocker) setup(args []string) ([]byte, error) {
	switch strings.Join(args[:2], " ") {
	case "email add":
		fp, _ := os.OpenFile(filepath.Join(f.cfgDir, "postfix-accounts.cf"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		fp.WriteString(args[2] + "|{SHA512-CRYPT}$6$x\n")
		fp.Close()
	case "config dkim":
		d := args[len(args)-1]
		os.MkdirAll(filepath.Join(f.cfgDir, dkimDir), 0o755)
		os.WriteFile(filepath.Join(f.cfgDir, dkimDir, "rsa-2048-mail-"+d+".public.dns.txt"), []byte("v=DKIM1; k=rsa; p=MIIBKEY\n"), 0o644)
	}
	return nil, nil
}

func newService(t *testing.T) (*Service, *fakeDocker) {
	t.Helper()
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := Config{DataDir: filepath.Join(dir, "mail"), CaddyDataDir: filepath.Join(dir, "caddy"), Network: "wpgenie",
		MailImage: "dms:16", WebmailImage: "roundcube:1.7", WebmailPort: 8089}
	fd := &fakeDocker{t: t, cfgDir: filepath.Join(cfg.DataDir, "config"), containers: map[string]string{}}
	s := &Service{Cfg: cfg, Store: st, Docker: fd, Log: slog.New(slog.DiscardHandler),
		Sync: func(context.Context) error { return nil }}
	if err := s.PrepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, fd
}

func issueCert(t *testing.T, s *Service, host, issuer string) {
	dir := filepath.Join(s.Cfg.CaddyDataDir, "caddy", "certificates", issuer, host)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, host+".crt"), []byte("cert"), 0o600)
	os.WriteFile(filepath.Join(dir, host+".key"), []byte("key"), 0o600)
}

func TestLifecycleWaitsForCertificateAndFirstMailbox(t *testing.T) {
	s, fd := newService(t)
	ctx := context.Background()
	st, err := s.Enable(ctx, "Mail.Example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Hostname != "mail.example.com" || st.Server != "waiting_certificate" || st.Webmail != "running" {
		t.Fatalf("status = %+v", st)
	}
	if host, up := s.Webmail(); host != "mail.example.com" || up != "127.0.0.1:8089" {
		t.Fatalf("Webmail() = %q %q", host, up)
	}

	issueCert(t, s, "mail.example.com", "acme.zerossl.com-v2-dv90") // a fallback issuer works too
	if st := s.Reconcile(ctx); st.Server != "waiting_mailbox" {
		t.Fatalf("with a certificate but no account: %+v", st)
	}
	if _, ok := fd.containers[mailName]; ok {
		t.Fatal("the mail server panics without an account; it must not be started yet")
	}

	if _, err := s.AddDomain(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	_, pw, err := s.CreateMailbox(ctx, MailboxInput{Address: "Jane@Example.com", QuotaMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 20 {
		t.Errorf("generated password %q", pw)
	}
	// The first account was created in a one-off container, the password
	// only on stdin (twice), never in a command line.
	var add []string
	for _, c := range fd.cmds {
		if slices.Contains(c, "add") {
			add = c
		}
		if strings.Contains(strings.Join(c, " "), pw) {
			t.Fatalf("password in argv: %v", c)
		}
	}
	if add[0] != "run" || !slices.Contains(add, "--rm") || !slices.Contains(add, "jane@example.com") {
		t.Fatalf("first account: %v", add)
	}
	if !slices.Contains(fd.stdins, pw+"\n"+pw+"\n") {
		t.Fatalf("stdin = %q", fd.stdins)
	}
	if _, ok := fd.containers[mailName]; !ok || s.Status().Server != "running" {
		t.Fatalf("mail server not started after the first mailbox: %+v", s.Status())
	}
	mailRun := fd.cmds[slices.IndexFunc(fd.cmds, func(c []string) bool { return slices.Contains(c, mailName) && c[0] == "run" })]
	joined := strings.Join(mailRun, " ")
	for _, want := range []string{"--network-alias mail.example.com", "SSL_CERT_PATH=/srv/tls/mail.example.com.crt",
		"acme.zerossl.com-v2-dv90/mail.example.com:/srv/tls:ro", "ENABLE_RSPAMD=1", "SPOOF_PROTECTION=1", "-p 587:587", "wpgenie-mail-state:/var/mail-state"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mail server args missing %q:\n%s", want, joined)
		}
	}
	// DKIM needs the running server, and then appears in the records.
	info, _ := s.Domain(ctx, "example.com", false)
	if dk := info.Records[2]; dk.Value != "v=DKIM1; k=rsa; p=MIIBKEY" || dk.Name != "mail._domainkey.example.com" {
		t.Fatalf("dkim record = %+v", dk)
	}

	// Second account: in the running server.
	runs := len(fd.containers)
	if _, _, err := s.CreateMailbox(ctx, MailboxInput{Address: "bob@example.com", Password: "a-long-enough-pw"}); err != nil {
		t.Fatal(err)
	}
	last := fd.cmds[slices.IndexFunc(fd.cmds, func(c []string) bool { return slices.Contains(c, "bob@example.com") })]
	if last[0] != "exec" || len(fd.containers) != runs {
		t.Fatalf("second account: %v", last)
	}

	// Nothing changed: nothing is recreated. A relay change recreates the server.
	before := len(fd.cmds)
	s.Reconcile(ctx)
	for _, c := range fd.cmds[before:] {
		if c[0] == "run" || c[0] == "rm" {
			t.Fatalf("steady state recreated a container: %v", c)
		}
	}
	oldSpec := fd.containers[mailName]
	if _, err := s.SetRelay(ctx, &Relay{Host: "smtp.relay.example", User: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if fd.containers[mailName] == oldSpec {
		t.Fatal("relay change did not recreate the mail server")
	}
	if st := s.Status(); st.Relay == nil || st.Relay.Password != "" || st.Relay.Port != 587 {
		t.Fatalf("status must never expose the relay password: %+v", st.Relay)
	}
	for _, c := range fd.cmds {
		if strings.Contains(strings.Join(c, " "), "RELAY_PASSWORD") || slices.Contains(c, "p") {
			t.Fatalf("relay password on a docker command line: %v", c)
		}
	}
	creds, _ := os.ReadFile(filepath.Join(fd.cfgDir, "postfix-sasl-password.cf"))
	fi, _ := os.Stat(filepath.Join(fd.cfgDir, "postfix-sasl-password.cf"))
	if string(creds) != "[smtp.relay.example]:587 u:p\n" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("relay credentials file %q %v", creds, fi.Mode())
	}
	// A new host must not inherit the stored password.
	if _, err := s.SetRelay(ctx, &Relay{Host: "evil.example", User: "u"}); err != nil {
		t.Fatal(err)
	}
	if s.current().Relay.Password != "" {
		t.Fatal("stored relay password carried over to a different host")
	}
	rc, _ := os.ReadFile(filepath.Join(s.Cfg.DataDir, "webmail/config/wpgenie.php"))
	if !strings.Contains(string(rc), "$config['des_key'] = '"+s.current().DESKey+"';") {
		t.Fatalf("webmail config: %s", rc)
	}
	for _, c := range fd.cmds {
		if strings.Contains(strings.Join(c, " "), s.current().DESKey) {
			t.Fatalf("webmail session key on a docker command line: %v", c)
		}
	}

	if _, err := s.Disable(ctx, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("disable with a site sending through it: %v", err)
	}
	if _, err := s.Disable(ctx, 0); err != nil || len(fd.containers) != 0 {
		t.Fatalf("disable: %v, containers %v", err, fd.containers)
	}
}

func TestEnableRevertsWhenTheProxyRejectsIt(t *testing.T) {
	s, _ := newService(t)
	s.Sync = func(context.Context) error { return errors.New("caddy rejected config") }
	if _, err := s.Enable(context.Background(), "mail.example.com", 0); err == nil {
		t.Fatal("expected the proxy error")
	}
	if host, _ := s.Webmail(); host != "" || s.current().Enabled {
		t.Fatal("mail left half-enabled after the proxy refused it")
	}
	s.Load(context.Background())
	if s.current().Enabled {
		t.Fatal("stored settings not reverted")
	}
}

func TestMailboxValidation(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	if _, _, err := s.CreateMailbox(ctx, MailboxInput{Address: "a@example.com"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("mail off: %v", err)
	}
	s.Enable(ctx, "mail.example.com", 0)
	s.AddDomain(ctx, "example.com")
	for _, in := range []MailboxInput{
		{Address: "no-at-sign"},
		{Address: "a@not a domain"},
		{Address: ".dot@example.com"},
		{Address: "a@other.com"}, // not a mail domain
		{Address: "a@example.com", Password: "short"},
		{Address: "a@example.com", Password: " leading-space-pw"},
		{Address: "a@example.com", Password: "line\nbreak-password"},
		{Address: "a@example.com", QuotaMB: -1},
	} {
		if _, _, err := s.CreateMailbox(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateMailbox(%+v) = %v, want invalid", in, err)
		}
	}
	if _, _, err := s.CreateMailbox(ctx, MailboxInput{Address: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateMailbox(ctx, MailboxInput{Address: "a@example.com"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.AddAlias(ctx, "a@example.com", "x@gmail.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("alias shadowing a mailbox: %v", err)
	}
	if _, err := s.AddAlias(ctx, "info@example.com", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDomain(ctx, "example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting a domain with mailboxes: %v", err)
	}
}

func TestSenderMailboxIsManaged(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	s.Enable(ctx, "mail.example.com", 0)
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := s.Store.CreateSite(ctx, &store.Site{ID: id, Name: id, PrimaryDomain: id + ".test", PHPVersion: "8.3",
			FPMPort: map[string]int{"s1": 19000, "s2": 19001, "s3": 19002}[id], DBName: "wp_" + id, Status: store.StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	// shop.test's mail is hosted elsewhere: it must not become local here.
	addr, pw, err := s.EnsureSender(ctx, "s1", "shop.test")
	if err != nil || addr != "s1@mail.example.com" || pw == "" {
		t.Fatalf("EnsureSender = %q %q %v", addr, pw, err)
	}
	if ok, _ := s.Store.MailDomainExists(ctx, "shop.test"); ok {
		t.Fatal("the site's domain was claimed: mail to its real mailboxes elsewhere would bounce")
	}
	// A domain whose mail already lives here sends as wordpress@ it.
	s.AddDomain(ctx, "blog.test")
	if addr, _, err := s.EnsureSender(ctx, "s3", "blog.test"); err != nil || addr != "wordpress@blog.test" {
		t.Fatalf("sender on a local mail domain = %q %v", addr, err)
	}

	if err := s.DeleteMailbox(ctx, addr); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting a site's sender by hand: %v", err)
	}
	if addr2, pw2, err := s.EnsureSender(ctx, "s1", "shop.test"); err != nil || addr2 != addr || pw2 == pw {
		t.Fatalf("re-enabling must reuse the mailbox with a new password: %q %v", addr2, err)
	}
	if err := s.RemoveSender(ctx, "s1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.GetMailbox(ctx, addr); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("sender mailbox not removed")
	}

	// A site deleted while the mail server was unreachable leaves its sender
	// behind: it must then be deletable by hand.
	a2, _, err := s.EnsureSender(ctx, "s2", "s2.test")
	if err != nil {
		t.Fatal(err)
	}
	s.Store.DeleteSite(ctx, "s2")
	if err := s.DeleteMailbox(ctx, a2); err != nil {
		t.Fatalf("orphaned sender: %v", err)
	}
}

func TestDeleteAliasKeepsRowWhenServerFails(t *testing.T) {
	s, fd := newService(t)
	ctx := context.Background()
	s.Enable(ctx, "mail.example.com", 0)
	s.AddDomain(ctx, "example.com")
	if _, err := s.AddAlias(ctx, "info@example.com", "x@gmail.com"); err != nil {
		t.Fatal(err)
	}
	s.Docker = failingDocker{fd, "alias"}
	if err := s.DeleteAlias(ctx, "info@example.com", "x@gmail.com"); err == nil {
		t.Fatal("expected the mail server error")
	}
	if as, _ := s.Aliases(ctx); len(as) != 1 {
		t.Fatal("alias vanished from the panel while still active on the mail server")
	}
	s.Docker = fd
	if err := s.DeleteAlias(ctx, "info@example.com", "x@gmail.com"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

type failingDocker struct {
	*fakeDocker
	match string
}

func (f failingDocker) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if slices.Contains(args, f.match) {
		return []byte("ERROR boom"), errors.New("exit status 1")
	}
	return f.fakeDocker.Run(ctx, stdin, args...)
}

func TestHostnameCannotChangeUnderSMTPSites(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	s.Enable(ctx, "mail.example.com", 0)
	if _, err := s.Enable(ctx, "mx.example.com", 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("hostname change with SMTP sites: %v", err)
	}
	if _, err := s.Enable(ctx, "mail.example.com", 2); err != nil {
		t.Fatalf("re-enabling with the same hostname: %v", err)
	}
}

type fakeResolver struct {
	mx  map[string][]*net.MX
	txt map[string][]string
}

func (f fakeResolver) LookupMX(_ context.Context, n string) ([]*net.MX, error) {
	if v, ok := f.mx[n]; ok {
		return v, nil
	}
	return nil, errors.New("nxdomain")
}
func (f fakeResolver) LookupTXT(_ context.Context, n string) ([]string, error) {
	if v, ok := f.txt[n]; ok {
		return v, nil
	}
	return nil, errors.New("nxdomain")
}

func TestCheckDNS(t *testing.T) {
	s, fd := newService(t)
	ctx := context.Background()
	s.Enable(ctx, "mail.example.com", 0)
	s.AddDomain(ctx, "example.com")
	fd.setup([]string{"config", "dkim", "domain", "example.com"})
	recs := s.records("example.com")
	s.checkDNS(ctx, fakeResolver{
		mx: map[string][]*net.MX{"example.com": {{Host: "MAIL.example.com.", Pref: 10}}},
		txt: map[string][]string{
			"example.com":                 {"google-site-verification=x", "v=spf1 mx ~all"},
			"mail._domainkey.example.com": {"v=DKIM1; k=rsa; p=OTHERKEY"},
		},
	}, recs)
	got := map[string]string{}
	for _, r := range recs {
		got[r.Purpose] = r.Status
	}
	want := map[string]string{"mx": "ok", "spf": "ok", "dkim": "mismatch", "dmarc": "missing"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s, want %s (all: %v)", k, got[k], v, got)
		}
	}
}

func TestSetupErrorNeverEchoesInput(t *testing.T) {
	out := []byte("Enter Password: hunter2-long-secret\n\x1b[1;31mERROR\x1b[0m addmailuser: 'a@b.c' already exists\n")
	got := setupError(out, errors.New("exit status 1"))
	if strings.Contains(got, "hunter2") || got != "ERROR addmailuser: 'a@b.c' already exists" {
		t.Fatalf("setupError = %q", got)
	}
}
