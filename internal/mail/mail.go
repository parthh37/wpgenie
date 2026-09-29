// Package mail runs WPGenie's optional mail stack: docker-mailserver
// (Postfix, Dovecot, Rspamd) for SMTP/IMAP and Roundcube for webmail. Like
// site replicas, both containers are created by the daemon, not compose,
// so mail can be switched on from the panel; a spec hash label says when a
// container must be recreated.
//
// TLS: Caddy serves webmail on the mail hostname and so obtains its
// certificate; the mail server reads that certificate (read-only mount) and
// reloads Postfix/Dovecot itself when Caddy renews it.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/parthh37/wpgenie/internal/domain"
	"github.com/parthh37/wpgenie/internal/store"
)

var (
	ErrDisabled = errors.New("mail is not enabled")
	ErrInvalid  = errors.New("invalid input")
	ErrConflict = errors.New("conflict")
)

// Docker runs the docker CLI (runtime.Docker).
type Docker interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
}

type Config struct {
	DataDir      string // mail data, e.g. /var/lib/wpgenie/mail
	CaddyDataDir string // Caddy's /data on the host, e.g. /var/lib/wpgenie/caddy
	Network      string
	MailImage    string
	WebmailImage string
	WebmailPort  int // Roundcube on 127.0.0.1, proxied by Caddy
}

// Relay sends outbound mail through a provider (SES, Postmark, Mailgun, …):
// needed where the cloud blocks outbound port 25, and better for
// deliverability from a fresh IP.
type Relay struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
}

// Settings are stored in the panel database (settings key "mail").
type Settings struct {
	Enabled  bool   `json:"enabled"`
	Hostname string `json:"hostname"`
	Relay    *Relay `json:"relay,omitempty"`
	DESKey   string `json:"des_key"` // Roundcube session encryption key
}

// Status is the mail stack as the last reconcile found it.
type Status struct {
	Enabled    bool      `json:"enabled"`
	Hostname   string    `json:"hostname,omitempty"`
	Server     string    `json:"server"`  // running | waiting_certificate | waiting_mailbox | starting | stopped | error
	Webmail    string    `json:"webmail"` // running | stopped | error
	Detail     string    `json:"detail,omitempty"`
	Relay      *Relay    `json:"relay,omitempty"` // password removed
	Checked    time.Time `json:"checked"`
	WebmailURL string    `json:"webmail_url,omitempty"`
}

const (
	settingsKey = "mail"
	mailName    = "wpgenie-mail"
	webmailName = "wpgenie-webmail"
	dkimDir     = "rspamd/dkim"
	selector    = "mail"
)

type Service struct {
	Cfg    Config
	Store  *store.Store
	Docker Docker
	// Sync re-renders the proxy config (the webmail host appears or goes).
	Sync func(context.Context) error
	Log  *slog.Logger

	mu       sync.Mutex // serialises every mail operation
	settings atomic.Pointer[Settings]
	status   atomic.Pointer[Status]
}

func (s *Service) configDir() string { return filepath.Join(s.Cfg.DataDir, "config") }

// Load reads the settings; call once at startup.
func (s *Service) Load(ctx context.Context) error {
	v, err := s.Store.Setting(ctx, settingsKey)
	if err != nil {
		return err
	}
	st := &Settings{}
	if v != "" {
		if err := json.Unmarshal([]byte(v), st); err != nil {
			return fmt.Errorf("mail settings: %w", err)
		}
	}
	s.settings.Store(st)
	return nil
}

func (s *Service) current() Settings {
	if p := s.settings.Load(); p != nil {
		return *p
	}
	return Settings{}
}

func (s *Service) save(ctx context.Context, st Settings) error {
	b, _ := json.Marshal(st)
	if err := s.Store.SetSetting(ctx, settingsKey, string(b)); err != nil {
		return err
	}
	s.settings.Store(&st)
	return nil
}

// Webmail is what the proxy serves for webmail: host and upstream, or ""
// when mail is off.
func (s *Service) Webmail() (host, upstream string) {
	st := s.current()
	if !st.Enabled {
		return "", ""
	}
	return st.Hostname, "127.0.0.1:" + strconv.Itoa(s.Cfg.WebmailPort)
}

// SMTPHost is where sites submit mail (the mail server's network alias),
// or "" when mail is off.
func (s *Service) SMTPHost() string {
	if st := s.current(); st.Enabled {
		return st.Hostname
	}
	return ""
}

// Status returns the latest reconcile result.
func (s *Service) Status() Status {
	if p := s.status.Load(); p != nil {
		return *p
	}
	st := s.current()
	return Status{Enabled: st.Enabled, Hostname: st.Hostname, Server: "stopped", Webmail: "stopped"}
}

// Enable turns mail on with hostname as the mail server's name: the MX
// target of every mail domain, the name mail clients connect to, and the
// webmail address. Its DNS A record must point at this server.
//
// sitesUsingSMTP counts sites whose WordPress mail goes through the server:
// their credentials name the current hostname, so it can't change under them.
func (s *Service) Enable(ctx context.Context, hostname string, sitesUsingSMTP int) (Status, error) {
	host, err := domain.Normalize(hostname)
	if err != nil {
		return Status{}, fmt.Errorf("%w: hostname: %v", ErrInvalid, err)
	}
	if cur := s.current(); cur.Hostname != "" && cur.Hostname != host && sitesUsingSMTP > 0 {
		return Status{}, fmt.Errorf("%w: %d site(s) send WordPress mail through %s; turn that off before changing the hostname",
			ErrConflict, sitesUsingSMTP, cur.Hostname)
	}
	s.mu.Lock()
	prev := s.current()
	st := prev
	if st.DESKey == "" {
		st.DESKey = randString(24)
	}
	st.Enabled, st.Hostname = true, host
	err = s.save(ctx, st)
	s.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	// The proxy must know the host first: that's what gets the certificate.
	if err := s.Sync(ctx); err != nil {
		s.mu.Lock()
		if rerr := s.save(context.WithoutCancel(ctx), prev); rerr != nil {
			err = errors.Join(err, rerr)
		}
		s.mu.Unlock()
		return Status{}, err
	}
	return s.Reconcile(ctx), nil
}

// Disable stops the mail containers. Mailboxes and mail are kept on disk,
// so enabling again brings everything back.
func (s *Service) Disable(ctx context.Context, sitesUsingSMTP int) (Status, error) {
	if sitesUsingSMTP > 0 {
		return Status{}, fmt.Errorf("%w: %d site(s) send WordPress mail through this server; turn that off first",
			ErrConflict, sitesUsingSMTP)
	}
	s.mu.Lock()
	st := s.current()
	st.Enabled = false
	err := s.save(ctx, st)
	s.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	if err := s.Sync(ctx); err != nil {
		return Status{}, err
	}
	return s.Reconcile(ctx), nil
}

// SetRelay configures (or with nil, removes) the outbound relay.
func (s *Service) SetRelay(ctx context.Context, r *Relay) (Status, error) {
	if r != nil {
		host, err := domain.Normalize(r.Host)
		if err != nil {
			return Status{}, fmt.Errorf("%w: relay host", ErrInvalid)
		}
		r.Host = host
		if r.Port == 0 {
			r.Port = 587
		}
		// Postfix's password map is "key user:password" per line.
		if r.Port < 1 || r.Port > 65535 || strings.ContainsFunc(r.User+r.Password, unicode.IsSpace) ||
			strings.ContainsFunc(r.User+r.Password, unicode.IsControl) || strings.Contains(r.User, ":") ||
			len(r.User) > 256 || len(r.Password) > 256 {
			return Status{}, fmt.Errorf("%w: relay port, or user/password with spaces", ErrInvalid)
		}
		if r.Password == "" {
			// Unchanged: the panel never shows it. Only for the same server
			// and user, or anyone with API access could send the stored
			// password to a host of their choosing.
			if old := s.current().Relay; old != nil && old.User == r.User && old.Host == r.Host && old.Port == r.Port {
				r.Password = old.Password
			}
		}
	}
	s.mu.Lock()
	st := s.current()
	st.Relay = r
	err := s.save(ctx, st)
	s.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	return s.Reconcile(ctx), nil
}

// Run reconciles periodically: the mail server starts as soon as Caddy has
// obtained its certificate, and crashed containers are noticed.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Reconcile makes the containers match the settings and records the result.
func (s *Service) Reconcile(ctx context.Context) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.reconcileLocked(ctx)
	st.Checked = time.Now().UTC()
	s.status.Store(&st)
	return st
}

func (s *Service) reconcileLocked(ctx context.Context) Status {
	set := s.current()
	out := Status{Enabled: set.Enabled, Hostname: set.Hostname, Server: "stopped", Webmail: "stopped"}
	if set.Relay != nil {
		r := *set.Relay
		r.Password = ""
		out.Relay = &r
	}
	running, err := s.containers(ctx)
	if err != nil {
		out.Server, out.Detail = "error", err.Error()
		return out
	}
	if !set.Enabled {
		for _, name := range []string{mailName, webmailName} {
			if _, ok := running[name]; ok {
				if err := s.remove(ctx, name); err != nil {
					out.Detail = err.Error()
				}
			}
		}
		return out
	}
	out.WebmailURL = "https://" + set.Hostname + "/"
	if err := errors.Join(s.writeWebmailConfig(set), s.writeRelayCreds(set),
		writeSecret(filepath.Join(s.configDir(), "fail2ban-jail.cf"), fail2banJail, 0)); err != nil {
		out.Server, out.Webmail, out.Detail = "error", "error", err.Error()
		return out
	}

	// Webmail doesn't need the certificate (Caddy terminates TLS).
	out.Webmail = "running"
	if err := s.ensure(ctx, webmailName, s.webmailArgs(set), running); err != nil {
		out.Webmail, out.Detail = "error", err.Error()
	}

	cert, key, ok := s.findCert(set.Hostname)
	if !ok {
		out.Server = "waiting_certificate"
		out.Detail = "waiting for Caddy to obtain a TLS certificate for " + set.Hostname +
			" (point its DNS A/AAAA record at this server)"
		return out
	}
	if n, err := s.accountCount(); err != nil || n == 0 {
		// docker-mailserver refuses to start without an account.
		out.Server, out.Detail = "waiting_mailbox", "create the first mailbox to start the mail server"
		return out
	}
	out.Server = "running"
	if err := s.ensure(ctx, mailName, s.mailArgs(set, cert, key), running); err != nil {
		out.Server, out.Detail = "error", err.Error()
		return out
	}
	if err := s.ensureDKIM(ctx); err != nil {
		out.Detail = "DKIM: " + err.Error()
	}
	return out
}

type container struct {
	spec    string
	running bool
}

func (s *Service) containers(ctx context.Context) (map[string]container, error) {
	out, err := s.Docker.Run(ctx, nil, "ps", "-a", "--filter", "label=wpgenie.mail",
		"--format", `{{.Names}}|{{.Label "wpgenie.spec"}}|{{.State}}`)
	if err != nil {
		return nil, err
	}
	m := map[string]container{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) == 3 {
			m[f[0]] = container{spec: f[1], running: f[2] == "running"}
		}
	}
	return m, nil
}

// ensure (re)creates a container unless one with the same spec runs.
func (s *Service) ensure(ctx context.Context, name string, args []string, have map[string]container) error {
	spec := specHash(args)
	if c, ok := have[name]; ok {
		if c.spec == spec && c.running {
			return nil
		}
		if err := s.remove(ctx, name); err != nil {
			return err
		}
	}
	full := append([]string{"run", "-d", "--name", name, "--label", "wpgenie.mail=" + name,
		"--label", "wpgenie.spec=" + spec}, args...)
	if _, err := s.Docker.Run(ctx, nil, full...); err != nil {
		s.remove(ctx, name) // a failed run can leave a created container
		return err
	}
	s.Log.Info("mail container started", "container", name)
	return nil
}

func (s *Service) remove(ctx context.Context, name string) error {
	out, err := s.Docker.Run(ctx, nil, "rm", "-f", name)
	if err != nil && !strings.Contains(string(out)+err.Error(), "No such container") {
		return err
	}
	return nil
}

func specHash(args []string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// serverEnv configures docker-mailserver: Rspamd handles spam, DKIM
// signing and DMARC/SPF checks (the older OpenDKIM/Amavis pieces are off);
// ClamAV (1 GB+ of RAM) is off. SPOOF_PROTECTION stops one mailbox owner
// from sending as another address on the server. Fail2ban bans IMAP/SMTP
// password guessing (see fail2banJail).
func serverEnv() []string {
	return []string{
		"ENABLE_RSPAMD=1", "ENABLE_OPENDKIM=0", "ENABLE_OPENDMARC=0", "ENABLE_POLICYD_SPF=0",
		"ENABLE_AMAVIS=0", "ENABLE_CLAMAV=0", "ENABLE_FAIL2BAN=1", "ENABLE_QUOTAS=1",
		"SPOOF_PROTECTION=1", "PERMIT_DOCKER=none",
	}
}

func (s *Service) mailArgs(set Settings, cert, key string) []string {
	d := s.Cfg.DataDir
	args := []string{
		"--hostname", set.Hostname,
		"--network", s.Cfg.Network,
		// Sites and Roundcube connect by the public name, so the
		// certificate matches without leaving the Docker network.
		"--network-alias", set.Hostname,
		"--restart", "unless-stopped", "--stop-timeout", "60",
		// Fail2ban's nftables rules, inside the container's own network namespace.
		"--cap-add", "NET_ADMIN",
		"-p", "25:25", "-p", "465:465", "-p", "587:587", "-p", "993:993",
		"-v", filepath.Join(d, "data") + ":/var/mail",
		// Postfix's queue and its UNIX sockets live in the state directory: a
		// Docker volume works on every host filesystem (a bind mount fails on
		// Docker Desktop's virtiofs with "chmod socket: Invalid argument").
		"-v", "wpgenie-mail-state:/var/mail-state",
		"-v", filepath.Join(d, "logs") + ":/var/log/mail",
		"-v", s.configDir() + ":/tmp/docker-mailserver",
		// Only this host's certificate directory, not every site's keys.
		// A directory mount (not files) survives renewals replacing them.
		"-v", filepath.Dir(cert) + ":/srv/tls:ro",
		"-e", "SSL_TYPE=manual",
		"-e", "SSL_CERT_PATH=/srv/tls/" + filepath.Base(cert),
		"-e", "SSL_KEY_PATH=/srv/tls/" + filepath.Base(key),
	}
	for _, e := range serverEnv() {
		args = append(args, "-e", e)
	}
	if r := set.Relay; r != nil {
		// The credentials are in postfix-sasl-password.cf (writeRelayCreds),
		// not the environment, which `ps` and `docker inspect` would show.
		args = append(args, "-e", "DEFAULT_RELAY_HOST="+relayKey(r))
	}
	return append(args, s.Cfg.MailImage)
}

func relayKey(r *Relay) string { return "[" + r.Host + "]:" + strconv.Itoa(r.Port) }

// fail2banJail overrides docker-mailserver's defaults (6 failures in a
// week bans for a week: one phone with a stale password locks its owner
// out). Docker's private ranges are ignored: Roundcube and the sites log in
// from there, and the shield already rate-limits the webmail login.
const fail2banJail = `# Managed by WPGenie.
[DEFAULT]
ignoreip = 127.0.0.1/8 ::1 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
findtime = 10m
maxretry = 10
bantime = 1h
bantime.increment = true
bantime.maxtime = 1w
`

// writeRelayCreds maintains docker-mailserver's relay password map (root
// only; the server watches it and reloads Postfix on changes).
func (s *Service) writeRelayCreds(set Settings) error {
	path := filepath.Join(s.configDir(), "postfix-sasl-password.cf")
	if r := set.Relay; r != nil && r.User != "" {
		return writeSecret(path, relayKey(r)+" "+r.User+":"+r.Password+"\n", 0)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeSecret writes a 0600 (or 0640 with group gid) file atomically.
func writeSecret(path, content string, gid int) error {
	tmp := path + ".tmp"
	mode := os.FileMode(0o600)
	if gid != 0 {
		mode = 0o640
	}
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	if gid != 0 && os.Geteuid() == 0 {
		if err := os.Chown(tmp, 0, gid); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, path)
}

// roundcubeConfig is included after the image's own config. The session
// key lives here (readable by Roundcube only), not in the container's
// environment. X-Forwarded-For is deliberately not trusted: site containers
// share Roundcube's Docker network and could forge it.
const roundcubeConfig = `<?php
// Managed by WPGenie: rewritten when mail settings change.
$config['use_https'] = true; // TLS is terminated by Caddy
$config['product_name'] = 'Webmail';
$config['des_key'] = '%s';
`

// roundcubeUID is www-data in the Roundcube image.
const roundcubeUID = 33

func (s *Service) writeWebmailConfig(set Settings) error {
	return writeSecret(filepath.Join(s.Cfg.DataDir, "webmail/config/wpgenie.php"),
		fmt.Sprintf(roundcubeConfig, set.DESKey), roundcubeUID)
}

func (s *Service) webmailArgs(set Settings) []string {
	d := filepath.Join(s.Cfg.DataDir, "webmail")
	host := set.Hostname
	return []string{
		"--network", s.Cfg.Network,
		"--restart", "unless-stopped",
		"-p", "127.0.0.1:" + strconv.Itoa(s.Cfg.WebmailPort) + ":80",
		"-v", filepath.Join(d, "db") + ":/var/roundcube/db",
		"-v", filepath.Join(d, "config") + ":/var/roundcube/config:ro",
		"-e", "ROUNDCUBEMAIL_DEFAULT_HOST=ssl://" + host, "-e", "ROUNDCUBEMAIL_DEFAULT_PORT=993",
		"-e", "ROUNDCUBEMAIL_SMTP_SERVER=tls://" + host, "-e", "ROUNDCUBEMAIL_SMTP_PORT=587",
		"-e", "ROUNDCUBEMAIL_DB_TYPE=sqlite",
		"-e", "ROUNDCUBEMAIL_UPLOAD_MAX_FILESIZE=25M",
		// The config template is in the spec so changing it recreates the
		// container (the key isn't: it never changes, and must not leak).
		"--label", "wpgenie.config=" + specHash([]string{roundcubeConfig}),
		s.Cfg.WebmailImage,
	}
}

// PrepareDirs creates the data directories and Roundcube's config file.
func (s *Service) PrepareDirs() error {
	for _, d := range []string{"data", "logs", "config", "webmail/db", "webmail/config"} {
		if err := os.MkdirAll(filepath.Join(s.Cfg.DataDir, d), 0o755); err != nil {
			return err
		}
	}
	if err := os.Chmod(s.Cfg.DataDir, 0o711); err != nil {
		return err
	}
	// Roundcube (www-data inside its container) writes its SQLite database
	// and reads its config; nobody else on the host reads either.
	if os.Geteuid() == 0 {
		if err := os.Chown(filepath.Join(s.Cfg.DataDir, "webmail/db"), roundcubeUID, roundcubeUID); err != nil {
			return err
		}
		if err := os.Chown(filepath.Join(s.Cfg.DataDir, "webmail/config"), 0, roundcubeUID); err != nil {
			return err
		}
	}
	if err := os.Chmod(filepath.Join(s.Cfg.DataDir, "webmail/config"), 0o750); err != nil {
		return err
	}
	return os.Chmod(s.configDir(), 0o700)
}

// findCert locates the certificate Caddy obtained for host, whichever ACME
// issuer it came from (Let's Encrypt preferred).
func (s *Service) findCert(host string) (cert, key string, ok bool) {
	matches, _ := filepath.Glob(filepath.Join(s.Cfg.CaddyDataDir, "caddy", "certificates", "*", host, host+".crt"))
	for _, m := range matches {
		k := strings.TrimSuffix(m, ".crt") + ".key"
		if _, err := os.Stat(k); err != nil {
			continue
		}
		if cert == "" || strings.Contains(m, "letsencrypt") {
			cert, key, ok = m, k, true
		}
	}
	return cert, key, ok
}

func (s *Service) accountCount() (int, error) {
	b, err := os.ReadFile(filepath.Join(s.configDir(), "postfix-accounts.cf"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return strings.Count(string(b), "|{"), err
}

// setup runs docker-mailserver's `setup` tool: inside the running server,
// or in a throwaway container on the same config while the server can't
// run yet (it won't start without an account, so the first one is always
// created this way). Returns the output for error messages.
func (s *Service) setup(ctx context.Context, stdin io.Reader, args ...string) error {
	running, err := s.containers(ctx)
	if err != nil {
		return err
	}
	var full []string
	if c, ok := running[mailName]; ok && c.running {
		full = []string{"exec"}
		if stdin != nil {
			full = append(full, "-i")
		}
		full = append(full, mailName, "setup")
	} else {
		full = []string{"run", "--rm", "-i", "-v", s.configDir() + ":/tmp/docker-mailserver"}
		for _, e := range serverEnv() {
			full = append(full, "-e", e)
		}
		full = append(full, s.Cfg.MailImage, "setup")
	}
	out, err := s.Docker.Run(ctx, stdin, append(full, args...)...)
	if err != nil {
		return fmt.Errorf("mail server: %s", setupError(out, err))
	}
	return nil
}

// setupError keeps docker-mailserver's ERROR lines (never the input, which
// may be a password) for messages.
func setupError(out []byte, err error) string {
	var keep []string
	for _, l := range strings.Split(string(out), "\n") {
		if i := strings.Index(l, "ERROR"); i >= 0 {
			keep = append(keep, strings.TrimSpace(stripANSI(l[i:])))
		}
	}
	if len(keep) == 0 {
		return err.Error()
	}
	return strings.Join(keep, "; ")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

const passAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

func randString(n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(passAlphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = passAlphabet[v.Int64()]
	}
	return string(out)
}
