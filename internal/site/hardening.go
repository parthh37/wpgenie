package site

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// WordPress hardening: what security plugins do inside WordPress that the
// shield in front of it can't (it never sees who is signed in, or what a
// plugin does with the database). The hardening mu-plugin
// (images/php/hardening.php, read-only in the image) applies the ones a
// site has on; the wrapper WPGenie writes names them. Site code can't
// change or switch it off: the wrapper is root-owned.

const hardeningWrapperPath = "wp-content/mu-plugins/wpgenie-hardening.php"

// Hardening keys (store.Site.Harden).
const (
	HardenUserEnum        = "user_enum"
	HardenLoginErrors     = "login_errors"
	HardenAppPasswords    = "app_passwords"
	HardenAdminLock       = "admin_lock"
	HardenFileMods        = "file_mods"
	HardenShortSessions   = "short_sessions"
	HardenStrongPasswords = "strong_passwords"
	HardenPingbacks       = "pingbacks"
)

// HardeningOption describes one hardening setting for the panel.
type HardeningOption struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Default: new sites get it, and "apply recommended" turns it on.
	Default bool `json:"default"`
}

// HardeningOptions is every setting, in the order the panel lists them.
// The two off by default change how people work in wp-admin.
var HardeningOptions = []HardeningOption{
	{HardenUserEnum, "Hide usernames",
		"Visitors can't look up sign-in names through ?author= links, the REST API, author sitemaps or embeds, so bots have to guess the username as well as the password.", true},
	{HardenLoginErrors, "Vague sign-in errors",
		"A failed sign-in only says the details were wrong, never whether the username exists. Password-reset requests don't give it away either.", true},
	{HardenAppPasswords, "No application passwords",
		"Turns off WordPress application passwords, which let apps sign in without your password. Turn this off if an app or service connects to the site with one.", true},
	{HardenAdminLock, "Lock administrator accounts",
		"New administrators (and roles that can manage users, settings or plugins) can only be added from this panel. If a plugin or someone in wp-admin tries, it's refused and written to the PHP error log.", false},
	{HardenFileMods, "No plugin installs from wp-admin",
		"Plugins and themes can't be installed, updated or deleted from wp-admin, so a stolen login can't upload a backdoor. Updates from this panel still work; turn this off for a moment to install a plugin.", false},
	{HardenShortSessions, "Shorter administrator sign-ins",
		`Administrators are signed out after 12 hours (3 days with "Remember me") instead of 14 days.`, true},
	{HardenStrongPasswords, "Strong passwords",
		"People who can write or edit content must choose passwords of at least 12 characters that mix letters with numbers or symbols, aren't common, and don't contain their username or the site's name.", true},
	{HardenPingbacks, "No pingbacks or trackbacks",
		"Other sites can't notify yours of links to it (mostly spam, and a way to use your site against others), and yours doesn't notify them.", true},
}

// DefaultHardening is the hardening new sites start with.
func DefaultHardening() []string {
	var out []string
	for _, o := range HardeningOptions {
		if o.Default {
			out = append(out, o.Key)
		}
	}
	return out
}

// normalizeHardening validates keys and puts them in catalogue order,
// without duplicates.
func normalizeHardening(in []string) ([]string, error) {
	for _, k := range in {
		if !slices.ContainsFunc(HardeningOptions, func(o HardeningOption) bool { return o.Key == k }) {
			return nil, fmt.Errorf("%w: unknown hardening setting %q", ErrInvalidInput, k)
		}
	}
	out := []string{}
	for _, o := range HardeningOptions {
		if slices.Contains(in, o.Key) {
			out = append(out, o.Key)
		}
	}
	return out, nil
}

const hardeningWrapper = `<?php
/**
 * Plugin Name: WPGenie Hardening
 * Description: WordPress hardening chosen in the WPGenie panel. Managed by WPGenie: change it in the panel; this file is rewritten on changes.
 */
define( 'WPGENIE_HARDEN', '%s' );
if ( is_file( '/usr/local/share/wpgenie/hardening.php' ) ) {
	require_once '/usr/local/share/wpgenie/hardening.php';
}
`

// hardeningWrapperFor is the wrapper naming keys (validated: only [a-z_]
// and commas reach the PHP string).
func hardeningWrapperFor(keys []string) string {
	return fmt.Sprintf(hardeningWrapper, strings.Join(keys, ","))
}

type HardeningInput struct {
	Hardening []string `json:"hardening"`
}

// SetHardening chooses a site's WordPress hardening. The page cache is
// purged: cached pages still carry the pingback link it removes.
func (s *Service) SetHardening(ctx context.Context, id string, in HardeningInput) (*store.Site, error) {
	keys, err := normalizeHardening(in.Hardening)
	if err != nil {
		return nil, err
	}
	retire, prev, err := s.setHardeningLocked(ctx, id, keys)
	if err != nil {
		return nil, err
	}
	retire()
	if !slices.Equal(prev, keys) {
		if err := s.Purge(ctx, id); err != nil {
			s.Log.Warn("purging the cache after changing hardening", "site", id, "err", err)
		}
		if len(keys) == 0 {
			s.event(id, "security", "WordPress hardening turned off")
		} else {
			s.event(id, "security", "WordPress hardening: "+strings.Join(keys, ", "))
		}
	}
	return s.Store.GetSite(ctx, id)
}

func (s *Service) setHardeningLocked(ctx context.Context, id string, keys []string) (func(), []string, error) {
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, nil, fmt.Errorf("%w: an update, scan or job is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if st.Status != store.StatusActive {
		return nil, nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	prev := st.Harden
	if err := s.writeHardeningWrapper(id, keys); err != nil {
		return nil, nil, err
	}
	if err := s.Store.SetHarden(ctx, id, keys); err != nil {
		return nil, nil, err
	}
	// Replicas from an image without hardening.php would ignore the
	// wrapper: roll any that are out of date.
	st.Harden = keys
	retire, err := s.reconcile(ctx, st)
	if err != nil {
		return nil, nil, fmt.Errorf("hardening saved, but refreshing the site's PHP containers failed: %w", err)
	}
	return retire, prev, nil
}

func (s *Service) writeHardeningWrapper(id string, keys []string) error {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureManaged(root, hardeningWrapperPath, hardeningWrapperFor(keys), len(keys) > 0)
}

// ---- Sign everyone out ----

// wpConfigEnd ends WPGenie's part of wp-config.php; below it is the site's.
const wpConfigEnd = "// --- end WPGenie ---"

var saltLineRe = regexp.MustCompile(`(?m)^define\( '([A-Z_]+)', '[^'\\]*' \);$`)

// rotateSalts gives every salt in WPGenie's part of a wp-config.php a new
// random value and leaves every other byte as it was. It refuses a file
// whose salts aren't where WPGenie writes them, rather than guess.
func rotateSalts(cfg []byte) ([]byte, error) {
	end := bytes.Index(cfg, []byte(wpConfigEnd))
	if end < 0 {
		return nil, errors.New("wp-config.php has no WPGenie section")
	}
	head, tail := cfg[:end], cfg[end:]
	seen := map[string]int{}
	out := saltLineRe.ReplaceAllFunc(head, func(line []byte) []byte {
		name := string(saltLineRe.FindSubmatch(line)[1])
		if !slices.Contains(saltNames, name) {
			return line
		}
		seen[name]++
		return fmt.Appendf(nil, "define( '%s', '%s' );", name, randString(64, saltAlphabet))
	})
	for _, n := range saltNames {
		if seen[n] != 1 {
			return nil, fmt.Errorf("wp-config.php: %s is defined %d times in WPGenie's section", n, seen[n])
		}
	}
	return append(out, tail...), nil
}

// endSessionsPHP forgets every user's sessions (the new salts already
// make their cookies invalid; this also empties "Log out everywhere" lists).
const endSessionsPHP = `if ( class_exists( 'WP_Session_Tokens' ) ) { WP_Session_Tokens::destroy_all_for_all_users(); }`

// SignOutEveryone ends every WordPress session on a site: its salts are
// replaced, so every sign-in cookie, password-reset link and nonce stops
// working at once. Everyone signs in again; nothing else changes.
func (s *Service) SignOutEveryone(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if st.Status != store.StatusActive {
		return fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	// Not the maintenance lock: this is what someone does when they think
	// the site is being broken into, and must not wait for a scan.
	if err := s.rotateSiteSalts(id); err != nil {
		return fmt.Errorf("new security keys: %w", err)
	}
	if err := s.Runtime.Exec(ctx, id, nil, nil, runtime.WPArgs("eval", endSessionsPHP)...); err != nil {
		s.Log.Warn("forgetting WordPress sessions after new salts", "site", id, "err", err)
	}
	// A spread site's other servers have a copy of wp-config.php: bring
	// them the new one. In the background, like after a scale: until then
	// a cookie still works there, never for longer than the copy takes.
	if len(st.SpreadNodes) > 0 || len(st.RemoteUpstreams) > 0 {
		if spec, err := s.specFor(ctx, st); err != nil {
			s.Log.Error("spread: new security keys for other servers", "site", id, "err", err)
		} else {
			go s.syncRemote(context.WithoutCancel(ctx), st, spec)
		}
	}
	s.event(id, "security", "Everyone was signed out of WordPress (new security keys)")
	return nil
}

func (s *Service) rotateSiteSalts(id string) error {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	path := filepath.Join(s.Cfg.SiteDir(id), "wp-config.php")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := rotateSalts(b)
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	// WPGenie's file (root:82 0640, see prepareFiles): replaced whole, so
	// PHP never reads half of it.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, fi.Mode().Perm()); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(tmp, 0, wwwData); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, path)
}
