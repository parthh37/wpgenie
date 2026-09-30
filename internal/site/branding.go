package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/store"
)

// Branding: WordPress's admin shows the host's brand instead of
// WordPress's. One server-wide setting (name, link, logo); every site gets
// a root-owned mu-plugin wrapper naming it, which loads
// images/php/branding.php from the image: the login page's logo and link,
// the admin bar's logo menu, the admin footer and page titles, and no
// WordPress news widget.
//
// The logo itself is served by the daemon on each site's own domain
// (BrandLogoPath, through Caddy's /_wpgenie/ route), versioned by its hash,
// so it is cached for good and a new logo shows at once. It never goes
// into sites' directories, where site code could replace it.

const (
	brandWrapperPath = "wp-content/mu-plugins/wpgenie-brand.php"
	brandSettingKey  = "branding"
	// BrandLogoPath is where sites (and Caddy's /_wpgenie/ route) find the
	// logo.
	BrandLogoPath = "/_wpgenie/brand/logo"
	maxLogoBytes  = 256 << 10
	maxBrandName  = 80
)

// logoTypes are the logo formats accepted. SVG is served under a sandbox
// CSP (see ServeBrandLogo): opened directly, its scripts can't run.
var logoTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/svg+xml": true,
}

// Branding is the server-wide brand (stored as JSON in settings).
type Branding struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	LogoType string `json:"logo_type,omitempty"`
	Logo     []byte `json:"logo,omitempty"`
}

// Enabled: sites show the brand (and no WordPress logo).
func (b *Branding) Enabled() bool { return b != nil && (b.Name != "" || len(b.Logo) > 0) }

// LogoVersion identifies the logo's content ("" without one).
func (b *Branding) LogoVersion() string {
	if b == nil || len(b.Logo) == 0 {
		return ""
	}
	sum := sha256.Sum256(b.Logo)
	return hex.EncodeToString(sum[:6])
}

// BrandingView is the brand as the API shows it (the logo is fetched on
// its own).
type BrandingView struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Enabled     bool   `json:"enabled"`
	HasLogo     bool   `json:"has_logo"`
	LogoType    string `json:"logo_type,omitempty"`
	LogoVersion string `json:"logo_version,omitempty"`
}

func (b *Branding) View() BrandingView {
	return BrandingView{Name: b.Name, URL: b.URL, Enabled: b.Enabled(), HasLogo: len(b.Logo) > 0,
		LogoType: b.LogoType, LogoVersion: b.LogoVersion()}
}

// BrandingInput changes the brand. Logo is a data: URI
// (data:image/png;base64,…); "" removes the logo, absent keeps it.
type BrandingInput struct {
	Name string  `json:"name"`
	URL  string  `json:"url"`
	Logo *string `json:"logo,omitempty"`
}

// Input is the whole brand as a change (the panel pushes it to nodes).
func (b *Branding) Input() BrandingInput {
	logo := ""
	if len(b.Logo) > 0 {
		logo = "data:" + b.LogoType + ";base64," + base64.StdEncoding.EncodeToString(b.Logo)
	}
	return BrandingInput{Name: b.Name, URL: b.URL, Logo: &logo}
}

type brandState struct {
	mu     sync.Mutex
	cur    *Branding
	loaded bool
}

func validBrandName(name string) error {
	if utf8.RuneCountInString(name) > maxBrandName {
		return fmt.Errorf("%w: brand name longer than %d characters", ErrInvalidInput, maxBrandName)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%w: brand name has control characters", ErrInvalidInput)
	}
	return nil
}

func validBrandURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(v) > 500 {
		return fmt.Errorf("%w: brand link must be an http(s) URL", ErrInvalidInput)
	}
	return nil
}

// parseLogo decodes and checks a data: URI logo.
func parseLogo(uri string) (ctype string, data []byte, err error) {
	meta, payload, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ",")
	if !ok || !strings.HasPrefix(uri, "data:") || !strings.HasSuffix(meta, ";base64") {
		return "", nil, fmt.Errorf("%w: logo must be a base64 data: URI", ErrInvalidInput)
	}
	ctype = strings.ToLower(strings.TrimSuffix(meta, ";base64"))
	if !logoTypes[ctype] {
		return "", nil, fmt.Errorf("%w: logo must be PNG, JPEG, GIF, WebP or SVG", ErrInvalidInput)
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > maxLogoBytes+3 {
		return "", nil, fmt.Errorf("%w: logo larger than %d KB", ErrInvalidInput, maxLogoBytes>>10)
	}
	data, err = base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) == 0 {
		return "", nil, fmt.Errorf("%w: logo isn't valid base64", ErrInvalidInput)
	}
	if len(data) > maxLogoBytes {
		return "", nil, fmt.Errorf("%w: logo larger than %d KB", ErrInvalidInput, maxLogoBytes>>10)
	}
	// The bytes must be what the type says: a file sniffed as something
	// else never goes out under an image type.
	if ctype == "image/svg+xml" {
		if !utf8.Valid(data) || !bytes.Contains(bytes.ToLower(data), []byte("<svg")) {
			return "", nil, fmt.Errorf("%w: logo isn't an SVG image", ErrInvalidInput)
		}
	} else if got := http.DetectContentType(data); got != ctype {
		return "", nil, fmt.Errorf("%w: logo is %s, not %s", ErrInvalidInput, got, ctype)
	}
	return ctype, data, nil
}

// Branding returns the server-wide brand (empty when none is set).
func (s *Service) Branding(ctx context.Context) (*Branding, error) {
	s.brand.mu.Lock()
	defer s.brand.mu.Unlock()
	if s.brand.loaded {
		return s.brand.cur, nil
	}
	v, err := s.Store.Setting(ctx, brandSettingKey)
	if err != nil {
		return nil, err
	}
	b := &Branding{}
	if v != "" {
		if err := json.Unmarshal([]byte(v), b); err != nil {
			return nil, fmt.Errorf("stored branding: %w", err)
		}
	}
	s.brand.cur, s.brand.loaded = b, true
	return b, nil
}

// SetBranding changes the brand and rewrites every site's wrapper. A site
// whose wrapper can't be written keeps its old one (logged); the next
// change, update or restore writes it again.
func (s *Service) SetBranding(ctx context.Context, in BrandingInput) (*Branding, error) {
	name, link := strings.TrimSpace(in.Name), strings.TrimSpace(in.URL)
	if err := validBrandName(name); err != nil {
		return nil, err
	}
	if err := validBrandURL(link); err != nil {
		return nil, err
	}
	cur, err := s.Branding(ctx)
	if err != nil {
		return nil, err
	}
	next := &Branding{Name: name, URL: link, LogoType: cur.LogoType, Logo: cur.Logo}
	if in.Logo != nil {
		next.LogoType, next.Logo = "", nil
		if *in.Logo != "" {
			if next.LogoType, next.Logo, err = parseLogo(*in.Logo); err != nil {
				return nil, err
			}
		}
	}
	b, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	s.brand.mu.Lock()
	if err := s.Store.SetSetting(ctx, brandSettingKey, string(b)); err != nil {
		s.brand.mu.Unlock()
		return nil, err
	}
	s.brand.cur, s.brand.loaded = next, true
	s.brand.mu.Unlock()
	s.applyBranding(ctx)
	return next, nil
}

// applyBranding rewrites the brand wrapper of every site on this server.
func (s *Service) applyBranding(ctx context.Context) {
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		s.Log.Warn("branding: listing sites", "err", err)
		return
	}
	for _, st := range sites {
		// Not a site still being provisioned: the WordPress image only
		// copies core into an empty docroot (its finish writes the wrapper).
		if st.Status != store.StatusActive && st.Status != store.StatusSuspended {
			continue
		}
		if err := s.writeBrandWrapper(ctx, st.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Log.Warn("branding: writing a site's wrapper", "site", st.ID, "err", err)
		}
	}
}

const brandWrapper = `<?php
/**
 * Plugin Name: WPGenie Branding
 * Description: Shows your host's brand in WordPress's admin. Managed by WPGenie: change it in the WPGenie panel; this file is rewritten on changes.
 */
define( 'WPGENIE_BRAND_NAME', base64_decode( '%s' ) );
define( 'WPGENIE_BRAND_URL', base64_decode( '%s' ) );
define( 'WPGENIE_BRAND_LOGO', '%s' );
if ( is_file( '/usr/local/share/wpgenie/branding.php' ) ) {
	require_once '/usr/local/share/wpgenie/branding.php';
}
`

// brandWrapperFor renders the wrapper. Name and link are base64 (whatever
// they contain can't end the PHP string); the logo path is ours, with a
// hex version.
func brandWrapperFor(b *Branding) string {
	logo := ""
	if v := b.LogoVersion(); v != "" {
		logo = BrandLogoPath + "?v=" + v
	}
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf(brandWrapper, enc([]byte(b.Name)), enc([]byte(b.URL)), logo)
}

func (s *Service) writeBrandWrapper(ctx context.Context, id string) error {
	b, err := s.Branding(ctx)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureManaged(root, brandWrapperPath, brandWrapperFor(b), b.Enabled())
}

// ServeBrandLogo serves the brand's logo (BrandLogoPath on sites' domains,
// and the panel's preview).
func (s *Service) ServeBrandLogo(w http.ResponseWriter, r *http.Request) {
	b, err := s.Branding(r.Context())
	if err != nil || len(b.Logo) == 0 {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", b.LogoType)
	h.Set("X-Content-Type-Options", "nosniff")
	// An SVG opened on its own is a document: no scripts, no requests.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
	if r.URL.Query().Get("v") == b.LogoVersion() {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=300")
	}
	h.Set("ETag", `"`+b.LogoVersion()+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b.Logo))
}
