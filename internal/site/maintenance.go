package site

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/store"
)

// Maintenance mode: visitors get a plain "back soon" page (503 with
// Retry-After) while people signed in who can edit posts see the site as
// usual. The maintenance mu-plugin (images/php/maintenance.php, read-only
// in the image) does the work; the wrapper WPGenie writes is the switch and
// carries the message. The wrapper is the state: it moves with the site's
// files (moves between servers, restores), and a staging copy starts the
// way its live site was.

const (
	maintenanceWrapperPath = "wp-content/mu-plugins/wpgenie-maintenance.php"
	// MaxMaintenanceMessage bounds the visitors' message, in characters.
	MaxMaintenanceMessage = 300
)

const maintenanceWrapper = `<?php
/**
 * Plugin Name: WPGenie Maintenance Mode
 * Description: Visitors see a maintenance page; signed-in editors see the site. Managed by WPGenie: turn it off in the panel; this file is rewritten on changes.
 */
define( 'WPGENIE_MAINTENANCE', '%s' );
if ( is_file( '/usr/local/share/wpgenie/maintenance.php' ) ) {
	require_once '/usr/local/share/wpgenie/maintenance.php';
}
`

// maintenanceWrapperFor is the wrapper for a message. The message travels
// base64-encoded, so nothing in it can end the PHP string; the page
// escapes it for HTML.
func maintenanceWrapperFor(msg string) string {
	return fmt.Sprintf(maintenanceWrapper, base64.StdEncoding.EncodeToString([]byte(msg)))
}

var maintenanceDefine = regexp.MustCompile(`define\( 'WPGENIE_MAINTENANCE', '([A-Za-z0-9+/=]*)' \);`)

// parseMaintenanceWrapper reads the message back from a wrapper WPGenie
// wrote. ok is false for anything else (the site may have put a file of
// its own there).
func parseMaintenanceWrapper(b []byte) (msg string, ok bool) {
	if !strings.Contains(string(b), managedMarker) {
		return "", false
	}
	m := maintenanceDefine.FindSubmatch(b)
	if m == nil {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(string(m[1]))
	if err != nil {
		return "", false
	}
	// The site can write the file: never trust what comes back.
	msg = strings.ToValidUTF8(string(raw), "")
	if utf8.RuneCountInString(msg) > MaxMaintenanceMessage {
		msg = string([]rune(msg)[:MaxMaintenanceMessage])
	}
	return msg, true
}

// normalizeMaintenanceMessage trims a message and checks it: at most
// MaxMaintenanceMessage characters, line breaks allowed but no other
// control characters.
func normalizeMaintenanceMessage(msg string) (string, error) {
	msg = strings.TrimSpace(strings.ReplaceAll(msg, "\r\n", "\n"))
	if !utf8.ValidString(msg) {
		return "", fmt.Errorf("%w: the message isn't valid text", ErrInvalidInput)
	}
	if n := utf8.RuneCountInString(msg); n > MaxMaintenanceMessage {
		return "", fmt.Errorf("%w: the message is %d characters; at most %d", ErrInvalidInput, n, MaxMaintenanceMessage)
	}
	if strings.ContainsFunc(msg, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
		return "", fmt.Errorf("%w: the message has control characters", ErrInvalidInput)
	}
	if strings.Count(msg, "\n") > 5 {
		return "", fmt.Errorf("%w: the message has more than 6 lines", ErrInvalidInput)
	}
	return msg, nil
}

// Maintenance is a site's maintenance mode.
type Maintenance struct {
	On bool `json:"on"`
	// Message is what visitors read ("" = the default sentence).
	Message string `json:"message"`
}

// MaintenanceInput turns maintenance mode on (with a message) or off.
type MaintenanceInput struct {
	On      bool   `json:"on"`
	Message string `json:"message"`
}

// MaintenanceMode reads a site's maintenance mode from its wrapper.
func (s *Service) MaintenanceMode(ctx context.Context, id string) (*Maintenance, error) {
	if _, err := s.Store.GetSite(ctx, id); err != nil {
		return nil, err
	}
	return s.readMaintenance(id)
}

func (s *Service) readMaintenance(id string) (*Maintenance, error) {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if errors.Is(err, fs.ErrNotExist) {
		return &Maintenance{}, nil
	} else if err != nil {
		return nil, err
	}
	defer root.Close()
	b, err := readSmallFile(root, maintenanceWrapperPath, 64<<10)
	if err != nil {
		// Missing, or not a file WPGenie wrote (a symlink, a directory, a
		// huge file): maintenance mode is off.
		return &Maintenance{}, nil
	}
	msg, ok := parseMaintenanceWrapper(b)
	return &Maintenance{On: ok, Message: msg}, nil
}

// SetMaintenanceMode turns maintenance mode on or off. The page cache (and the
// CDN's copy) is purged either way: cached pages would go on reaching
// visitors past the maintenance page, or keep showing it afterwards.
func (s *Service) SetMaintenanceMode(ctx context.Context, id string, in MaintenanceInput) (*Maintenance, error) {
	msg := ""
	if in.On {
		var err error
		if msg, err = normalizeMaintenanceMessage(in.Message); err != nil {
			return nil, err
		}
	}
	retire, prev, err := s.setMaintenanceLocked(ctx, id, in.On, msg)
	if err != nil {
		return nil, err
	}
	retire()
	cur := &Maintenance{On: in.On, Message: msg}
	if *prev == *cur {
		return cur, nil
	}
	if err := s.Purge(ctx, id); err != nil {
		s.Log.Warn("purging the cache after changing maintenance mode", "site", id, "err", err)
	}
	switch {
	case !in.On:
		s.event(id, "maintenance", "Maintenance mode off: visitors see the site again")
	case prev.On:
		s.event(id, "maintenance", "Maintenance mode: message changed")
	default:
		s.event(id, "maintenance", "Maintenance mode on: visitors see a maintenance page; signed-in editors see the site")
	}
	return cur, nil
}

func (s *Service) setMaintenanceLocked(ctx context.Context, id string, on bool, msg string) (func(), *Maintenance, error) {
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
	if err := activeSite(st); err != nil {
		return nil, nil, err
	}
	prev, err := s.readMaintenance(id)
	if err != nil {
		return nil, nil, err
	}
	if err := s.writeMaintenanceWrapper(id, on, msg); err != nil {
		return nil, nil, err
	}
	if !on {
		return func() {}, prev, nil
	}
	// Replicas from an image without maintenance.php would ignore the
	// wrapper: roll any that are out of date.
	retire, err := s.reconcile(ctx, st)
	if err != nil {
		return nil, nil, fmt.Errorf("maintenance mode saved, but refreshing the site's PHP containers failed: %w", err)
	}
	return retire, prev, nil
}

func (s *Service) writeMaintenanceWrapper(id string, on bool, msg string) error {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureManaged(root, maintenanceWrapperPath, maintenanceWrapperFor(msg), on)
}

// keepMaintenance runs replace, which replaces the site's files with
// others (a staging push, a backup restore), and leaves maintenance mode as
// the site had it: it's a switch on the site, not something files carry
// over (a copy made during maintenance would otherwise take the live site
// down when pushed, and a restore would switch it on or off).
func (s *Service) keepMaintenance(id string, replace func() error) error {
	m, err := s.readMaintenance(id)
	if err != nil {
		return err
	}
	if err := replace(); err != nil {
		return err
	}
	return s.writeMaintenanceWrapper(id, m.On, m.Message)
}

// rewriteMaintenanceWrapper puts back a root-owned wrapper with the same
// message after something extracted the site's files (a restore or a
// copy brings the file back owned by the site).
func (s *Service) rewriteMaintenanceWrapper(id string) error {
	m, err := s.readMaintenance(id)
	if err != nil || !m.On {
		return err
	}
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(maintenanceWrapperPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return ensureManaged(root, maintenanceWrapperPath, maintenanceWrapperFor(m.Message), true)
}

// readSmallFile reads a regular file through root, refusing anything else
// (the site owns the directory: a FIFO would block, a symlink could point
// at another of its files) and anything over max.
func readSmallFile(root *os.Root, name string, max int64) ([]byte, error) {
	f, fi, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, max)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

func activeSite(st *store.Site) error {
	if st.Status != store.StatusActive {
		return fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	return nil
}
