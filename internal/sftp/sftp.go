// Package sftp gives sites SFTP access to their files: one OpenSSH server
// (container wpgenie-sftp, built from images/sftp) for every site, where
// each login is chrooted to its site's directory and gets the SFTP
// subsystem only (no shell, no tunnels).
//
// The jail is the site directory (/var/lib/wpgenie/sites/<id>, root-owned,
// as sshd requires): the login starts in public/ and can read, but not
// change, wp-config.php (root:82 0640), exactly like the site's PHP. Logins
// write as uid 82, the site user, so WordPress can manage what they upload.
// Accounts live in the panel's database; the daemon renders them into
// passwd/group/shadow and authorized_keys files that the container installs
// with each change. Passwords are stored only as SHA-512 crypt hashes and
// shown once.
package sftp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"

	"github.com/parthh37/wpgenie/internal/store"
)

var (
	ErrInvalid  = errors.New("invalid input")
	ErrConflict = errors.New("conflict")
)

// Docker runs docker (runtime.Docker).
type Docker interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	EnsureBuilt(ctx context.Context, tag, dir string) (string, error)
}

type Config struct {
	DataDir  string // /var/lib/wpgenie/sftp: config/ (accounts) and hostkeys/
	SitesDir string // mounted at the same path in the container
	Image    string
	ImageDir string // images/sftp
	Port     int
}

type Service struct {
	Cfg    Config
	Store  *store.Store
	Docker Docker
	Log    *slog.Logger
	mu     sync.Mutex
}

const (
	container = "wpgenie-sftp"
	siteUID   = 82 // www-data, the site user
	sftpGID   = 2000
)

var (
	suffixRe = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	siteIDRe = regexp.MustCompile(`^s[a-z0-9]{1,15}$`)
)

// Username is the login for a site: the site ID, or <site>-<suffix> for
// more than one login (a developer, an agency). Never a system account.
func Username(siteID, suffix string) (string, error) {
	if !siteIDRe.MatchString(siteID) {
		return "", fmt.Errorf("%w: site ID", ErrInvalid)
	}
	if suffix == "" {
		return siteID, nil
	}
	if !suffixRe.MatchString(suffix) {
		return "", fmt.Errorf("%w: the login name suffix must be 1-16 lowercase letters or digits", ErrInvalid)
	}
	return siteID + "-" + suffix, nil
}

// ValidLogin reports whether username is a login name Username could have
// made for siteID (a login arriving from elsewhere, a moved site's, is
// checked like one made here: it becomes a file name and a passwd entry).
func ValidLogin(siteID, username string) bool {
	if !siteIDRe.MatchString(siteID) {
		return false
	}
	if username == siteID {
		return true
	}
	suffix, ok := strings.CutPrefix(username, siteID+"-")
	return ok && suffixRe.MatchString(suffix)
}

var cryptRe = regexp.MustCompile(`^\$6\$(rounds=[0-9]{4,9}\$)?[./0-9A-Za-z]{1,16}\$[./0-9A-Za-z]{86}$`)

// ValidPasswordHash reports whether h is "" (keys only) or a SHA-512 crypt
// hash, the only kind this server stores.
func ValidPasswordHash(h string) bool { return h == "" || cryptRe.MatchString(h) }

// NormalizeKeys validates authorized_keys lines: public keys only, no
// options (command=, from=, … would change what a login may do), RSA of
// at least 2048 bits. Blank lines and comments are dropped.
func NormalizeKeys(lines []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range lines {
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				return nil, fmt.Errorf("%w: not an SSH public key: %.40q", ErrInvalid, line)
			}
			if len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
				return nil, fmt.Errorf("%w: keys can't carry options (%s)", ErrInvalid, strings.Join(options, ","))
			}
			if ck, ok := key.(ssh.CryptoPublicKey); ok {
				if r, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok && r.N.BitLen() < 2048 {
					return nil, fmt.Errorf("%w: RSA keys must be at least 2048 bits", ErrInvalid)
				}
			}
			norm := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if seen[norm] {
				continue
			}
			seen[norm] = true
			comment = strings.Map(func(r rune) rune {
				if unicode.IsPrint(r) && r != ' ' {
					return r
				}
				return -1
			}, comment)
			if comment != "" {
				norm += " " + comment[:min(len(comment), 64)]
			}
			out = append(out, norm)
		}
	}
	if len(out) > 20 {
		return nil, fmt.Errorf("%w: at most 20 keys per login", ErrInvalid)
	}
	return out, nil
}

// UserInput adds a login.
type UserInput struct {
	Suffix     string   `json:"suffix"`
	Password   bool     `json:"password"` // generate a password (shown once)
	PublicKeys []string `json:"public_keys"`
	// Who adds it (store.SFTPUser.AddedBy, AddedByName): set by the API.
	AddedBy     string `json:"-"`
	AddedByName string `json:"-"`
}

func (s *Service) Users(ctx context.Context, siteID string) ([]*store.SFTPUser, error) {
	return s.Store.SFTPUsers(ctx, siteID)
}

// Add creates a login for a site; it returns the generated password, if
// one was asked for.
func (s *Service) Add(ctx context.Context, siteID string, in UserInput) (*store.SFTPUser, string, error) {
	name, err := Username(siteID, in.Suffix)
	if err != nil {
		return nil, "", err
	}
	keys, err := NormalizeKeys(in.PublicKeys)
	if err != nil {
		return nil, "", err
	}
	if !in.Password && len(keys) == 0 {
		return nil, "", fmt.Errorf("%w: give the login a password, a public key or both", ErrInvalid)
	}
	st, err := s.Store.GetSite(ctx, siteID)
	if err != nil {
		return nil, "", err
	}
	if st.Status != store.StatusActive {
		return nil, "", fmt.Errorf("%w: site is %s", ErrInvalid, st.Status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Store.GetSFTPUser(ctx, name); err == nil {
		return nil, "", fmt.Errorf("%w: login %s exists", ErrConflict, name)
	}
	u := &store.SFTPUser{Username: name, SiteID: siteID, PublicKeys: keys, AddedBy: in.AddedBy, AddedByName: in.AddedByName}
	var pw string
	if in.Password {
		pw = newPassword()
		u.Password = HashPassword(pw)
	}
	if err := s.Store.CreateSFTPUser(ctx, u); err != nil {
		return nil, "", err
	}
	if err := s.reconcileLocked(ctx, nil); err != nil {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		s.Store.DeleteSFTPUser(c, name)
		return nil, "", errors.Join(err, s.reconcileLocked(c, []string{name}))
	}
	u, err = s.Store.GetSFTPUser(ctx, name)
	return u, pw, err
}

func (s *Service) user(ctx context.Context, siteID, name string) (*store.SFTPUser, error) {
	u, err := s.Store.GetSFTPUser(ctx, name)
	if err != nil {
		return nil, err
	}
	if u.SiteID != siteID {
		return nil, store.ErrNotFound
	}
	return u, nil
}

// SetKeys replaces a login's public keys.
func (s *Service) SetKeys(ctx context.Context, siteID, name string, keys []string) (*store.SFTPUser, error) {
	keys, err := NormalizeKeys(keys)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.user(ctx, siteID, name)
	if err != nil {
		return nil, err
	}
	if u.Password == "" && len(keys) == 0 {
		return nil, fmt.Errorf("%w: the login has no password: keep at least one key (or delete it)", ErrInvalid)
	}
	if err := s.Store.SetSFTPCredentials(ctx, name, u.Password, keys); err != nil {
		return nil, err
	}
	if err := s.reconcileLocked(ctx, nil); err != nil {
		return nil, err
	}
	return s.Store.GetSFTPUser(ctx, name)
}

// SetPassword gives a login a new generated password (returned once), or
// removes its password (keys only) when on is false.
func (s *Service) SetPassword(ctx context.Context, siteID, name string, on bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.user(ctx, siteID, name)
	if err != nil {
		return "", err
	}
	var pw, hash string
	if on {
		pw = newPassword()
		hash = HashPassword(pw)
	} else if len(u.PublicKeys) == 0 {
		return "", fmt.Errorf("%w: the login has no keys: keep the password (or delete it)", ErrInvalid)
	}
	if err := s.Store.SetSFTPCredentials(ctx, name, hash, u.PublicKeys); err != nil {
		return "", err
	}
	// A new password also ends sessions opened with the old one.
	return pw, s.reconcileLocked(ctx, []string{name})
}

// Delete removes a login and ends its open sessions.
func (s *Service) Delete(ctx context.Context, siteID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.user(ctx, siteID, name); err != nil {
		return err
	}
	if err := s.Store.DeleteSFTPUser(ctx, name); err != nil {
		return err
	}
	return s.reconcileLocked(ctx, []string{name})
}

// DeleteAddedBy removes the logins of a site that addedBy added (and ends
// their sessions): their access to the site ended. It returns how many.
func (s *Service) DeleteAddedBy(ctx context.Context, siteID, addedBy string) (int, error) {
	if addedBy == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.Store.SFTPUsers(ctx, siteID)
	if err != nil {
		return 0, err
	}
	var gone []string
	for _, u := range users {
		if u.AddedBy != addedBy {
			continue
		}
		if err := s.Store.DeleteSFTPUser(ctx, u.Username); err != nil && !errors.Is(err, store.ErrNotFound) {
			return len(gone), err
		}
		gone = append(gone, u.Username)
	}
	if len(gone) == 0 {
		return 0, nil
	}
	return len(gone), s.reconcileLocked(ctx, gone)
}

// SiteRemoved drops a deleted site's logins (the store already did) from
// the server and ends their sessions.
func (s *Service) SiteRemoved(ctx context.Context, siteID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := s.writtenUsers(siteID)
	if len(gone) == 0 {
		return
	}
	if err := s.reconcileLocked(ctx, gone); err != nil {
		s.Log.Error("sftp: removing a deleted site's logins", "site", siteID, "err", err)
	}
}

// Reconcile renders the accounts and makes the server match (run at
// startup): running while any login exists, otherwise stopped.
func (s *Service) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileLocked(ctx, nil)
}

func (s *Service) configDir() string  { return filepath.Join(s.Cfg.DataDir, "config") }
func (s *Service) hostKeyDir() string { return filepath.Join(s.Cfg.DataDir, "hostkeys") }

// writtenUsers lists the logins in the rendered passwd (of one site, or
// all when siteID is "").
func (s *Service) writtenUsers(siteID string) []string {
	b, err := os.ReadFile(filepath.Join(s.configDir(), "passwd"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		name, _, ok := strings.Cut(line, ":")
		if ok && (siteID == "" || name == siteID || strings.HasPrefix(name, siteID+"-")) {
			out = append(out, name)
		}
	}
	return out
}

// render produces the account files for users.
func (s *Service) render(users []*store.SFTPUser) (passwd, group, shadow string) {
	var p, sh strings.Builder
	var names []string
	for _, u := range users {
		home := filepath.Join(s.Cfg.SitesDir, u.SiteID)
		fmt.Fprintf(&p, "%s:x:%d:%d:WPGenie site %s:%s:/sbin/nologin\n", u.Username, siteUID, siteUID, u.SiteID, home)
		hash := u.Password
		if hash == "" {
			hash = "*" // no password; "!" would lock the account for keys too
		}
		fmt.Fprintf(&sh, "%s:%s:19000:0:99999:7:::\n", u.Username, hash)
		names = append(names, u.Username)
	}
	return p.String(), fmt.Sprintf("sftp:x:%d:%s\n", sftpGID, strings.Join(names, ",")), sh.String()
}

// reconcileLocked writes the accounts, (re)starts or stops the server and
// ends the sessions of kick (removed or re-keyed logins).
func (s *Service) reconcileLocked(ctx context.Context, kick []string) error {
	users, err := s.Store.SFTPUsers(ctx, "")
	if err != nil {
		return err
	}
	// Logins of suspended sites (their account is suspended) are left out
	// until the site is back; the records stay.
	suspended, err := s.Store.SuspendedSiteIDs(ctx)
	if err != nil {
		return err
	}
	users = slices.DeleteFunc(users, func(u *store.SFTPUser) bool { return suspended[u.SiteID] })
	for _, u := range users {
		// Each login becomes a file name and a passwd entry: only names this
		// server would have made.
		if !ValidLogin(u.SiteID, u.Username) || strings.ContainsAny(u.Username+u.Password, ":\n") {
			return fmt.Errorf("refusing to render SFTP login %q", u.Username)
		}
	}
	if err := s.writeConfig(users); err != nil {
		return err
	}
	have, err := s.containerSpec(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		if have != "" {
			_, err := s.Docker.Run(ctx, nil, "rm", "-f", container)
			return err
		}
		return nil
	}
	imageID, err := s.Docker.EnsureBuilt(ctx, s.Cfg.Image, s.Cfg.ImageDir)
	if err != nil {
		return fmt.Errorf("building the SFTP image: %w", err)
	}
	args := s.runArgs()
	spec := specHash(append(args, imageID))
	if have == spec {
		if _, err := s.Docker.Run(ctx, nil, "exec", container, "/usr/local/bin/wpg-sftp-reload"); err != nil {
			return err
		}
		for _, name := range kick {
			// Session processes are titled "sshd-session: <user>…".
			s.Docker.Run(ctx, nil, "exec", container, "pkill", "-f", "^sshd-session: "+regexp.QuoteMeta(name)+"[ @]")
		}
		return nil
	}
	if have != "" {
		if _, err := s.Docker.Run(ctx, nil, "rm", "-f", container); err != nil {
			return err
		}
	}
	run := append([]string{"run", "-d", "--name", container, "--label", "wpgenie.sftp=1", "--label", "wpgenie.spec=" + spec}, args...)
	if _, err := s.Docker.Run(ctx, nil, append(run, s.Cfg.Image)...); err != nil {
		return fmt.Errorf("starting the SFTP server: %w", err)
	}
	s.Log.Info("sftp server started", "port", s.Cfg.Port, "logins", len(users))
	return nil
}

// runArgs are the container's settings. sshd needs root to chroot and
// switch to the login's user, and nothing more.
func (s *Service) runArgs() []string {
	return []string{
		"--restart", "unless-stopped",
		"-p", strconv.Itoa(s.Cfg.Port) + ":2222",
		"--cap-drop", "ALL",
		"--cap-add", "SYS_CHROOT", "--cap-add", "SETUID", "--cap-add", "SETGID",
		"--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "KILL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "512", "--memory", "256m",
		"-v", s.Cfg.SitesDir + ":" + s.Cfg.SitesDir,
		"-v", s.configDir() + ":/config:ro",
		"-v", s.hostKeyDir() + ":/hostkeys",
	}
}

func specHash(args []string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// containerSpec is the running server's spec label, "" if there is none
// (a stopped one counts as none: it is recreated).
func (s *Service) containerSpec(ctx context.Context) (string, error) {
	out, err := s.Docker.Run(ctx, nil, "ps", "-a", "--filter", "label=wpgenie.sftp",
		"--format", `{{.Names}}|{{.Label "wpgenie.spec"}}|{{.State}}`)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) == 3 && f[0] == container {
			if f[2] != "running" {
				return "stopped", nil
			}
			return f[1], nil
		}
	}
	return "", nil
}

// writeConfig renders the account files (root-owned; shadow root-only).
func (s *Service) writeConfig(users []*store.SFTPUser) error {
	dir := s.configDir()
	keys := filepath.Join(dir, "keys")
	for _, d := range []string{dir, keys, s.hostKeyDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		// sshd refuses authorized_keys in group/world-writable directories.
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	os.Chmod(s.hostKeyDir(), 0o700)
	passwd, group, shadow := s.render(users)
	for _, f := range []struct {
		name, data string
		mode       os.FileMode
	}{{"passwd", passwd, 0o644}, {"group", group, 0o644}, {"shadow", shadow, 0o600}} {
		if err := writeAtomic(filepath.Join(dir, f.name), []byte(f.data), f.mode); err != nil {
			return err
		}
	}
	want := map[string]bool{}
	for _, u := range users {
		want[u.Username] = true
		data := ""
		if len(u.PublicKeys) > 0 {
			data = strings.Join(u.PublicKeys, "\n") + "\n"
		}
		if err := writeAtomic(filepath.Join(keys, u.Username), []byte(data), 0o644); err != nil {
			return err
		}
	}
	entries, _ := os.ReadDir(keys)
	for _, e := range entries {
		if !want[e.Name()] {
			os.Remove(filepath.Join(keys, e.Name()))
		}
	}
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Info is how to connect.
type Info struct {
	Running bool     `json:"running"`
	Port    int      `json:"port"`
	HostKey []string `json:"host_keys"` // SHA-256 fingerprints to compare on first connect
}

func (s *Service) Info(ctx context.Context) Info {
	in := Info{Port: s.Cfg.Port, HostKey: []string{}}
	if spec, err := s.containerSpec(ctx); err == nil && spec != "" && spec != "stopped" {
		in.Running = true
	}
	for _, t := range []string{"ed25519", "rsa"} {
		b, err := os.ReadFile(filepath.Join(s.hostKeyDir(), "ssh_host_"+t+"_key.pub"))
		if err != nil {
			continue
		}
		if k, _, _, _, err := ssh.ParseAuthorizedKey(b); err == nil {
			in.HostKey = append(in.HostKey, k.Type()+" "+ssh.FingerprintSHA256(k))
		}
	}
	return in
}

const pwAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

func newPassword() string {
	out := make([]byte, 0, 24)
	b := make([]byte, 1)
	for len(out) < cap(out) {
		rand.Read(b)
		// Rejection sampling: no bias towards the first letters.
		if int(b[0]) < 256-256%len(pwAlphabet) {
			out = append(out, pwAlphabet[int(b[0])%len(pwAlphabet)])
		}
	}
	return string(out)
}
