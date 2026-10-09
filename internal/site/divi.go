package site

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// Divi: the host's Elegant Themes license. An administrator saves the
// account's username and API key once; new sites get the Divi theme
// installed and activated (unless skipped for one site), and every site
// with Divi gets the license applied without it ever being stored in the
// site's database or shown in Divi's settings:
//
//   - the username and key go into a credentials file next to
//     wp-config.php (root:82 0640: outside the docroot, read-only to PHP),
//     like the SMTP credentials;
//   - a root-owned mu-plugin wrapper loads images/php/divi.php from the PHP
//     image, which hands Divi the account through its option's pre_option
//     filter, refuses to write the option, and masks the key on Divi's
//     Theme Options screen.
//
// The key never goes into process arguments (the theme is downloaded by
// this daemon, then installed from a local file), job output, events,
// errors or API responses. Anyone who can run their own PHP on a site can
// still read it, which is why the panel recommends a dedicated API key.

const (
	diviSettingKey  = "divi_license"
	diviWrapperPath = "wp-content/mu-plugins/wpgenie-divi.php"
	// diviCredsFile lives next to wp-config.php (see smtpCredsFile).
	diviCredsFile = "wpgenie-divi.php"
	// diviThemeDir is where Divi lives in a site (its slug, as Elegant
	// Themes ships it).
	diviThemeDir  = "wp-content/themes/Divi"
	diviThemeSlug = "Divi"
	// DefaultDiviAPI is Elegant Themes' download endpoint.
	DefaultDiviAPI = "https://www.elegantthemes.com/api/api_downloads.php"
	// maxDiviZip bounds the download (Divi is ~30 MB).
	maxDiviZip      = 150 << 20
	diviDownloadMax = 10 * time.Minute
	diviInstallMax  = 15 * time.Minute
)

// ErrDiviRefused: Elegant Themes didn't give the theme for the account.
var ErrDiviRefused = errors.New("Elegant Themes refused the username or API key (or the account has no active Divi license)")

var (
	// The values go into PHP single-quoted strings: nothing that could end
	// one. Elegant Themes' API keys are letters and digits; usernames are
	// account names or e-mail addresses.
	diviUserRe = regexp.MustCompile(`^[A-Za-z0-9@._+-]{1,100}$`)
	diviKeyRe  = regexp.MustCompile(`^[A-Za-z0-9]{8,128}$`)
	zipMagic   = []byte("PK\x03\x04")
)

// DiviLicense is the server-wide license (stored as JSON in settings).
type DiviLicense struct {
	Username string `json:"username"`
	APIKey   string `json:"api_key"`
	// NewSites: new sites get Divi unless their creation says otherwise.
	NewSites bool `json:"new_sites"`
}

// Configured: there is a license to use.
func (l *DiviLicense) Configured() bool { return l != nil && l.Username != "" && l.APIKey != "" }

// DiviView is the license as the API shows it: never the key.
type DiviView struct {
	Configured bool `json:"configured"`
	NewSites   bool `json:"new_sites"`
	// For administrators only (see Public).
	Username string `json:"username,omitempty"`
	KeySet   bool   `json:"key_set"`
	KeyHint  string `json:"key_hint,omitempty"`
}

// View is the administrator's view: the username and the key's last four
// characters.
func (l *DiviLicense) View() DiviView {
	v := DiviView{Configured: l.Configured(), NewSites: l.NewSites, Username: l.Username, KeySet: l.APIKey != ""}
	if len(l.APIKey) >= 12 {
		v.KeyHint = "…" + l.APIKey[len(l.APIKey)-4:]
	}
	return v
}

// Public is what everyone else sees: whether new sites get Divi.
func (v DiviView) Public() DiviView { return DiviView{Configured: v.Configured, NewSites: v.NewSites} }

// DiviInput changes the license. Absent fields keep what is saved; an
// empty username or key removes the license.
type DiviInput struct {
	Username *string `json:"username,omitempty"`
	APIKey   *string `json:"api_key,omitempty"`
	NewSites *bool   `json:"new_sites,omitempty"`
}

// Input is the whole license as a change (the panel pushes it to nodes,
// which install Divi on their own sites).
func (l *DiviLicense) Input() DiviInput {
	user, key, ns := l.Username, l.APIKey, l.NewSites
	return DiviInput{Username: &user, APIKey: &key, NewSites: &ns}
}

type diviState struct {
	mu     sync.Mutex
	cur    *DiviLicense
	loaded bool
}

// Divi returns the server-wide license (empty when none is set).
func (s *Service) Divi(ctx context.Context) (*DiviLicense, error) {
	s.divi.mu.Lock()
	defer s.divi.mu.Unlock()
	if s.divi.loaded {
		c := *s.divi.cur
		return &c, nil
	}
	v, err := s.Store.Setting(ctx, diviSettingKey)
	if err != nil {
		return nil, err
	}
	l := &DiviLicense{}
	if v != "" {
		if err := json.Unmarshal([]byte(v), l); err != nil {
			// Never the stored value: it holds the key.
			return nil, errors.New("the stored Divi license is unreadable")
		}
	}
	s.divi.cur, s.divi.loaded = l, true
	c := *l
	return &c, nil
}

func validDiviUser(u string) error {
	if !diviUserRe.MatchString(u) {
		return fmt.Errorf("%w: the Elegant Themes username is up to 100 letters, digits and @ . _ + -", ErrInvalidInput)
	}
	return nil
}

func validDiviKey(k string) error {
	if !diviKeyRe.MatchString(k) {
		// Never echo it back.
		return fmt.Errorf("%w: an Elegant Themes API key is 8 to 128 letters and digits (copy it from your account's Username & API Key page)", ErrInvalidInput)
	}
	return nil
}

// SetDivi changes the license and applies it to every site with Divi on
// this server (removed everywhere when the license is).
func (s *Service) SetDivi(ctx context.Context, in DiviInput) (*DiviLicense, error) {
	cur, err := s.Divi(ctx)
	if err != nil {
		return nil, err
	}
	next := *cur
	if in.Username != nil {
		next.Username = strings.TrimSpace(*in.Username)
	}
	if in.APIKey != nil {
		next.APIKey = strings.TrimSpace(*in.APIKey)
	}
	if in.NewSites != nil {
		next.NewSites = *in.NewSites
	}
	switch {
	case (in.Username != nil && next.Username == "") || (in.APIKey != nil && next.APIKey == ""):
		// Removing the license.
		next.Username, next.APIKey = "", ""
	case next.Username == "" && next.APIKey == "":
		// Only the new-sites switch, without a license yet.
	default:
		if next.Username == "" {
			return nil, fmt.Errorf("%w: enter the Elegant Themes username", ErrInvalidInput)
		}
		if next.APIKey == "" {
			return nil, fmt.Errorf("%w: enter the Elegant Themes API key", ErrInvalidInput)
		}
		if err := validDiviUser(next.Username); err != nil {
			return nil, err
		}
		if err := validDiviKey(next.APIKey); err != nil {
			return nil, err
		}
		// A license saved for the first time installs Divi on new sites
		// unless told otherwise.
		if !cur.Configured() && in.NewSites == nil {
			next.NewSites = true
		}
	}
	b, err := json.Marshal(&next)
	if err != nil {
		return nil, err
	}
	s.divi.mu.Lock()
	if err := s.Store.SetSetting(ctx, diviSettingKey, string(b)); err != nil {
		s.divi.mu.Unlock()
		return nil, err
	}
	stored := next
	s.divi.cur, s.divi.loaded = &stored, true
	s.divi.mu.Unlock()
	if cur.Username != next.Username || cur.APIKey != next.APIKey {
		s.applyDivi(ctx)
	}
	return &next, nil
}

// applyDivi writes (or removes) the license files of every site with Divi
// on this server, and the credentials of the spread sites whose replicas
// run here. A site whose files can't be written keeps its old ones
// (logged); the next change, update or restore writes them again.
func (s *Service) applyDivi(ctx context.Context) {
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		s.Log.Warn("divi: listing sites", "err", err)
		return
	}
	for _, st := range sites {
		// Not a site still being provisioned: its install writes them.
		if st.Status != store.StatusActive && st.Status != store.StatusSuspended {
			continue
		}
		if err := s.writeDiviFiles(ctx, st.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Log.Warn("divi: writing a site's license files", "site", st.ID, "err", err)
		}
	}
	guests, err := s.Store.GuestReplicas(ctx, "")
	if err != nil {
		s.Log.Warn("divi: listing guest replicas", "err", err)
		return
	}
	done := map[string]bool{}
	for _, g := range guests {
		if done[g.SiteID] {
			continue
		}
		done[g.SiteID] = true
		if err := s.writeDiviCreds(ctx, g.SiteID); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Log.Warn("divi: writing a guest site's license", "site", g.SiteID, "err", err)
		}
	}
}

// siteHasDivi: the site has the Divi theme (only those get the license).
func (s *Service) siteHasDivi(id string) bool {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return false
	}
	defer root.Close()
	fi, err := root.Stat(diviThemeDir + "/style.css")
	return err == nil && fi.Mode().IsRegular()
}

const diviWrapper = `<?php
/**
 * Plugin Name: WPGenie Divi License
 * Description: Applies your host's Divi license (updates and premade layouts) without storing it in WordPress. Managed by WPGenie: change it in the WPGenie panel; this file is rewritten on changes.
 */
if ( is_file( '/usr/local/share/wpgenie/divi.php' ) ) {
	require_once '/usr/local/share/wpgenie/divi.php';
}
`

// diviCreds renders the credentials file. Both values are checked against
// the strict charsets (they go into PHP single-quoted strings).
func diviCreds(user, key string) ([]byte, error) {
	if !diviUserRe.MatchString(user) || !diviKeyRe.MatchString(key) {
		return nil, errors.New("unsafe Divi license value")
	}
	return fmt.Appendf(nil, `<?php
// Generated by WPGenie: the Elegant Themes account of the host, for Divi
// updates and premade layouts. Read by /usr/local/share/wpgenie/divi.php.
define( 'WPGENIE_ET_USER', '%s' );
define( 'WPGENIE_ET_KEY', '%s' );
`, user, key), nil
}

// writeDiviFiles makes a site's license files match the license: the
// credentials and the wrapper when there is one and the site has Divi,
// neither otherwise.
func (s *Service) writeDiviFiles(ctx context.Context, id string) error {
	if err := s.writeDiviCreds(ctx, id); err != nil {
		return err
	}
	l, err := s.Divi(ctx)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureManaged(root, diviWrapperPath, diviWrapper, l.Configured() && s.siteHasDivi(id))
}

// writeDiviCreds writes or removes the site's credentials file only (a
// spread site's replicas here get the wrapper with the home's files).
func (s *Service) writeDiviCreds(ctx context.Context, id string) error {
	l, err := s.Divi(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(s.Cfg.SiteDir(id), diviCredsFile)
	if !l.Configured() || !s.siteHasDivi(id) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	creds, err := diviCreds(l.Username, l.APIKey)
	if err != nil {
		return err
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, creds) {
		return nil
	}
	return s.writeSiteFile(id, diviCredsFile, creds)
}

// diviHTTP is the client the theme is downloaded with.
func (s *Service) diviHTTP() *http.Client {
	if s.DiviClient != nil {
		return s.DiviClient
	}
	return &http.Client{Timeout: diviDownloadMax}
}

// diviURL is the download address. It carries the key: it never goes into
// logs, errors or anything shown.
func (s *Service) diviURL(l *DiviLicense) string {
	base := s.DiviAPI
	if base == "" {
		base = DefaultDiviAPI
	}
	q := url.Values{}
	q.Set("api_update", "1")
	q.Set("theme", "Divi")
	q.Set("username", l.Username)
	q.Set("api_key", l.APIKey)
	return base + "?" + q.Encode()
}

// keyless strips the request (and its URL, with the key) from an HTTP
// client error, and the key from anything else, just in case.
func keyless(err error, key string) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if key != "" && strings.Contains(err.Error(), key) {
		return errors.New(strings.ReplaceAll(err.Error(), key, "[key]"))
	}
	return err
}

// openDivi starts downloading Divi and checks the answer is a zip file:
// the body is returned with its first bytes already read back in front.
func (s *Service) openDivi(ctx context.Context, l *DiviLicense) (io.ReadCloser, error) {
	if !l.Configured() {
		return nil, fmt.Errorf("%w: no Divi license is set up (System → Divi)", ErrInvalidInput)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.diviURL(l), nil)
	if err != nil {
		return nil, keyless(err, l.APIKey)
	}
	req.Header.Set("User-Agent", "WPGenie")
	resp, err := s.diviHTTP().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching Elegant Themes: %w", keyless(err, l.APIKey))
	}
	fail := func(err error) (io.ReadCloser, error) {
		resp.Body.Close()
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fail(ErrDiviRefused)
	case resp.StatusCode != http.StatusOK:
		return fail(fmt.Errorf("Elegant Themes answered %s; try again later", resp.Status))
	case resp.ContentLength > maxDiviZip:
		return fail(fmt.Errorf("Elegant Themes sent %d MB, more than the %d MB a theme may be", resp.ContentLength>>20, maxDiviZip>>20))
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(resp.Body, head)
	head = head[:n]
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fail(fmt.Errorf("downloading Divi: %w", keyless(err, l.APIKey)))
	}
	if !bytes.HasPrefix(head, zipMagic) {
		// Wrong credentials get a short message instead of the theme.
		if msg := plainMessage(head, l.APIKey); msg != "" {
			return fail(fmt.Errorf("%w: %q", ErrDiviRefused, msg))
		}
		return fail(ErrDiviRefused)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}, nil
}

// plainMessage is a short plain-text answer worth showing (no markup, no
// key), or "".
func plainMessage(b []byte, key string) string {
	m := strings.TrimSpace(string(b))
	if m == "" || len(m) > 200 || !utf8.ValidString(m) || strings.ContainsAny(m, "<>") ||
		strings.ContainsFunc(m, func(r rune) bool { return unicode.IsControl(r) }) {
		return ""
	}
	if key != "" && strings.Contains(m, key) {
		return ""
	}
	return m
}

// CheckDivi checks Elegant Themes gives the saved account the theme: it
// starts the download and stops once the answer is a zip file.
func (s *Service) CheckDivi(ctx context.Context) error {
	l, err := s.Divi(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	body, err := s.openDivi(ctx, l)
	if err != nil {
		return err
	}
	return body.Close()
}

// downloadDivi downloads the theme into a new file in the site's directory
// (outside the docroot, root:82 0640: the site's PHP can read it, not
// change it) and returns its path, which is also its path inside the
// site's containers. The caller removes it.
func (s *Service) downloadDivi(ctx context.Context, id string, l *DiviLicense) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, diviDownloadMax)
	defer cancel()
	body, err := s.openDivi(ctx, l)
	if err != nil {
		return "", err
	}
	defer body.Close()
	path := filepath.Join(s.Cfg.SiteDir(id), ".wpgenie-divi-"+randString(12, lowerAlnum)+".zip")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("saving the Divi download: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(path)
		}
	}()
	if os.Geteuid() == 0 {
		if err := f.Chown(0, wwwData); err != nil {
			return "", err
		}
	}
	n, err := io.Copy(f, io.LimitReader(body, maxDiviZip+1))
	if err != nil {
		return "", fmt.Errorf("downloading Divi: %w", keyless(err, l.APIKey))
	}
	if n > maxDiviZip {
		return "", fmt.Errorf("Elegant Themes sent more than the %d MB a theme may be", maxDiviZip>>20)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := checkDiviZip(path); err != nil {
		os.Remove(path)
		return "", err
	}
	ok = true
	return path, nil
}

// checkDiviZip: the download is whole and is the Divi theme.
func checkDiviZip(path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("the Divi download is incomplete or damaged (%v); try again", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "Divi/style.css" {
			return nil
		}
	}
	return errors.New("the Elegant Themes download isn't the Divi theme")
}

// installDivi downloads Divi and installs and activates it on a site (or
// activates it, when it's there already), then applies the license. The
// download never outlives the call.
func (s *Service) installDivi(ctx context.Context, id string, report Progress) (*Theme, error) {
	l, err := s.Divi(ctx)
	if err != nil {
		return nil, err
	}
	if !l.Configured() {
		return nil, fmt.Errorf("%w: no Divi license is set up (System → Divi)", ErrInvalidInput)
	}
	ctx, cancel := context.WithTimeout(ctx, diviInstallMax)
	defer cancel()
	if s.siteHasDivi(id) {
		report(60, "Activating Divi")
		if _, err := s.Runtime.WP(ctx, id, nil, "theme", "activate", diviThemeSlug); err != nil {
			return nil, fmt.Errorf("activating Divi: %w", err)
		}
	} else {
		report(30, "Downloading Divi from Elegant Themes")
		zipPath, err := s.downloadDivi(ctx, id, l)
		if err != nil {
			return nil, err
		}
		defer os.Remove(zipPath)
		report(60, "Installing Divi")
		if _, err := s.Runtime.WP(ctx, id, nil, "theme", "install", zipPath, "--activate"); err != nil {
			return nil, fmt.Errorf("installing Divi: %w", err)
		}
		// WordPress may have unpacked it somewhere else, or not at all.
		if !s.siteHasDivi(id) {
			return nil, errors.New("Divi was not installed (WordPress didn't unpack it into wp-content/themes/Divi)")
		}
	}
	if err := s.writeDiviFiles(ctx, id); err != nil {
		return nil, fmt.Errorf("applying the Divi license: %w", err)
	}
	t := &Theme{Slug: diviThemeSlug, Title: "Divi", Status: "active"}
	if list, err := s.themes(ctx, id); err == nil {
		if i := slices.IndexFunc(list, func(th Theme) bool { return th.Slug == diviThemeSlug }); i >= 0 {
			t = &list[i]
		}
	}
	return t, nil
}

// wantDivi decides whether a new site gets Divi: as its creation says, or
// the license's default for new sites.
func (s *Service) wantDivi(ctx context.Context, choice *bool) bool {
	if choice != nil {
		return *choice
	}
	l, err := s.Divi(ctx)
	return err == nil && l.Configured() && l.NewSites
}

// DiviDefault is whether a new site gets Divi when its creation doesn't
// say (the panel decides it for sites it creates on other servers).
func (s *Service) DiviDefault(ctx context.Context) bool { return s.wantDivi(ctx, nil) }

// installDiviOnNew installs Divi on a site being created. Never fatal: a
// site without Divi is still a site; the reason goes to its activity.
func (s *Service) installDiviOnNew(ctx context.Context, id string, report Progress) {
	scaled := func(pct int, step string) { report(75+pct/7, step) }
	t, err := s.installDivi(ctx, id, scaled)
	if err != nil {
		s.Log.Warn("divi: installing on a new site", "site", id, "err", err)
		s.event(id, "tools", "Divi couldn't be installed: "+err.Error()+". Install it later from the site's Tools.")
		return
	}
	s.event(id, "tools", fmt.Sprintf("Divi %s installed and activated, with the host's license", t.Version))
}

// StartDiviInstall installs and activates Divi on an existing site as a
// job (activates it when it's installed already) and applies the license.
func (s *Service) StartDiviInstall(ctx context.Context, id string) (int64, error) {
	if err := s.requireActive(ctx, id); err != nil {
		return 0, err
	}
	l, err := s.Divi(ctx)
	if err != nil {
		return 0, err
	}
	if !l.Configured() {
		return 0, fmt.Errorf("%w: no Divi license is set up (System → Divi)", ErrInvalidInput)
	}
	spec := s.siteJob(id, "divi-install", false)
	spec.Timeout = diviInstallMax + time.Minute
	return s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) error {
		if err := s.requireActive(ctx, id); err != nil {
			return err
		}
		had := s.siteHasDivi(id)
		th, err := s.installDivi(ctx, id, t.Progress)
		if err != nil {
			return err
		}
		if err := s.Purge(ctx, id); err != nil {
			s.Log.Warn("purging the cache after activating Divi", "site", id, "err", err)
		}
		t.SetResult(th)
		msg := fmt.Sprintf("Divi %s installed and activated, with the host's license", th.Version)
		if had {
			msg = fmt.Sprintf("Divi %s activated, with the host's license", th.Version)
		}
		s.event(id, "tools", msg)
		return nil
	})
}
