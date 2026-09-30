// Package support is the panel's help desk: customers open tickets in
// departments, the provider answers them, and the conversation, its
// attachments and e-mail notifications are kept here.
//
// Three parties meet on a ticket. The customer is any user of the account
// that opened it. The provider is the operator's staff or, for a reseller's
// customer, that reseller (the handler), who can escalate the ticket to
// the operator's staff; staff see and act on every ticket. Providers write
// internal notes to each other, which customers never see: not in the
// conversation, not in list previews, not in e-mail.
//
// Access to a ticket at all (a tenant reaching only their own account's
// tickets, and a reseller their customers') is checked by the API before
// anything here runs; this package decides what each party may do and see
// once they're in.
package support

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/mailer"
	"github.com/parthh37/wpgenie/internal/store"
)

var (
	ErrInvalid   = errors.New("invalid input")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
	// ErrTooLarge: an attachment over the size limit.
	ErrTooLarge = errors.New("too large")
	// ErrDisabled: customers can't open tickets (the settings say so).
	ErrDisabled = errors.New("support tickets are turned off; contact your provider another way")
)

// SettingKey holds the settings (JSON) in the settings table.
const SettingKey = "support"

// Settings are the help desk's.
type Settings struct {
	// Enabled: customers may open tickets (replies to open ones go on).
	Enabled bool `json:"enabled"`
	// NotifyEmails receive new tickets and customers' replies handled by
	// the operator's staff (departments may add their own address).
	NotifyEmails []string `json:"notify_emails"`
	// AutoCloseDays closes tickets answered that long ago without a
	// reply from the customer (0: never).
	AutoCloseDays int `json:"auto_close_days"`
	// Attachments: files per message (0: none), size per file, and the
	// file types allowed (extensions).
	MaxFiles   int      `json:"max_files"`
	MaxFileMB  int      `json:"max_file_mb"`
	Extensions []string `json:"extensions"`
	// ReplyTo is the Reply-To of ticket e-mail ("": the mailer's default).
	ReplyTo string `json:"reply_to"`
}

// DefaultExtensions are the attachment types allowed out of the box:
// screenshots, documents, logs and archives.
var DefaultExtensions = []string{"png", "jpg", "jpeg", "gif", "webp", "pdf", "txt", "log", "csv", "zip"}

func defaults() Settings {
	return Settings{Enabled: true, NotifyEmails: []string{}, AutoCloseDays: 7, MaxFiles: 5, MaxFileMB: 5,
		Extensions: slices.Clone(DefaultExtensions)}
}

// activeTypes are file types a browser (or a desktop, double-clicked) runs
// rather than shows: never allowed, whatever the settings say.
var activeTypes = map[string]bool{"html": true, "htm": true, "xhtml": true, "shtml": true, "svg": true, "svgz": true,
	"xml": true, "xsl": true, "js": true, "mjs": true, "php": true, "phtml": true, "phar": true, "exe": true, "msi": true,
	"bat": true, "cmd": true, "com": true, "scr": true, "ps1": true, "vbs": true, "sh": true, "jar": true, "hta": true,
	"swf": true, "dll": true, "app": true, "lnk": true}

var extRe = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

type Service struct {
	Store *store.Store
	// Mailer sends notifications (nil: none are sent).
	Mailer *mailer.Service
	Log    *slog.Logger
	// Dir holds attachments: Dir/<ticket>/<random name>.
	Dir string
	Now func() time.Time // for tests

	closing sync.Mutex // one auto-close run at a time
	seeding sync.Mutex // the first department is created once
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Settings returns the stored settings, defaults filled in.
func (s *Service) Settings(ctx context.Context) (*Settings, error) {
	out := defaults()
	v, err := s.Store.Setting(ctx, SettingKey)
	if err != nil {
		return nil, err
	}
	if v != "" {
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// SetSettings validates and stores new settings.
func (s *Service) SetSettings(ctx context.Context, in Settings) (*Settings, error) {
	in.ReplyTo = strings.TrimSpace(in.ReplyTo)
	if in.ReplyTo != "" && !mailer.ValidAddress(in.ReplyTo) {
		return nil, fmt.Errorf("%w: reply-to %q is not an e-mail address", ErrInvalid, in.ReplyTo)
	}
	emails := []string{}
	for _, e := range in.NotifyEmails {
		if e = strings.TrimSpace(e); e == "" || slices.Contains(emails, e) {
			continue
		}
		if !mailer.ValidAddress(e) {
			return nil, fmt.Errorf("%w: %q is not an e-mail address", ErrInvalid, e)
		}
		emails = append(emails, e)
	}
	if len(emails) > 20 {
		return nil, fmt.Errorf("%w: at most 20 notification addresses", ErrInvalid)
	}
	in.NotifyEmails = emails
	if in.AutoCloseDays < 0 || in.AutoCloseDays > 365 {
		return nil, fmt.Errorf("%w: auto-close after 0 (never) to 365 days", ErrInvalid)
	}
	if in.MaxFiles < 0 || in.MaxFiles > 10 {
		return nil, fmt.Errorf("%w: 0 to 10 attachments per message", ErrInvalid)
	}
	if in.MaxFileMB < 1 || in.MaxFileMB > 25 {
		return nil, fmt.Errorf("%w: attachments of 1 to 25 MB", ErrInvalid)
	}
	exts := []string{}
	for _, e := range in.Extensions {
		e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e == "" || slices.Contains(exts, e) {
			continue
		}
		if !extRe.MatchString(e) {
			return nil, fmt.Errorf("%w: file type %q (letters and digits, like pdf)", ErrInvalid, e)
		}
		if activeTypes[e] {
			return nil, fmt.Errorf("%w: .%s files can run code when opened; they can't be allowed", ErrInvalid, e)
		}
		exts = append(exts, e)
	}
	if len(exts) > 50 {
		return nil, fmt.Errorf("%w: at most 50 file types", ErrInvalid)
	}
	in.Extensions = exts
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetSetting(ctx, SettingKey, string(b)); err != nil {
		return nil, err
	}
	return &in, nil
}

// ---- Departments ----

// DepartmentInput creates or changes a department.
type DepartmentInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	NotifyEmail string `json:"notify_email"`
	Hidden      bool   `json:"hidden"`
	Sort        int    `json:"sort"`
}

func (in *DepartmentInput) validate() error {
	in.Name, in.Description, in.NotifyEmail = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description), strings.TrimSpace(in.NotifyEmail)
	if in.Name == "" || len(in.Name) > 60 || strings.ContainsAny(in.Name, "\r\n") {
		return fmt.Errorf("%w: a department needs a name (up to 60 characters)", ErrInvalid)
	}
	if len(in.Description) > 300 {
		return fmt.Errorf("%w: description up to 300 characters", ErrInvalid)
	}
	if in.NotifyEmail != "" && !mailer.ValidAddress(in.NotifyEmail) {
		return fmt.Errorf("%w: %q is not an e-mail address", ErrInvalid, in.NotifyEmail)
	}
	if in.Sort < -1000 || in.Sort > 1000 {
		return fmt.Errorf("%w: sort from -1000 to 1000", ErrInvalid)
	}
	return nil
}

// Departments lists departments; customers only see those not hidden.
// A panel without any gets its first one, General.
func (s *Service) Departments(ctx context.Context, withHidden bool) ([]*store.SupportDepartment, error) {
	list, err := s.Store.SupportDepartments(ctx)
	if err == nil && len(list) == 0 {
		list, err = s.firstDepartment(ctx)
	}
	if err != nil || withHidden {
		return list, err
	}
	return slices.DeleteFunc(list, func(d *store.SupportDepartment) bool { return d.Hidden }), nil
}

// firstDepartment creates General, once (not in the migration: a
// migrated database stays empty, as copying one into PostgreSQL needs).
func (s *Service) firstDepartment(ctx context.Context) ([]*store.SupportDepartment, error) {
	s.seeding.Lock()
	defer s.seeding.Unlock()
	list, err := s.Store.SupportDepartments(ctx)
	if err != nil || len(list) > 0 {
		return list, err
	}
	d := &store.SupportDepartment{Name: "General", Description: "Questions about your sites, e-mail and account"}
	if err := s.Store.CreateSupportDepartment(ctx, d); err != nil {
		return nil, err
	}
	return []*store.SupportDepartment{d}, nil
}

func (s *Service) CreateDepartment(ctx context.Context, in DepartmentInput) (*store.SupportDepartment, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	d := &store.SupportDepartment{Name: in.Name, Description: in.Description, NotifyEmail: in.NotifyEmail, Hidden: in.Hidden, Sort: in.Sort}
	if err := s.Store.CreateSupportDepartment(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Service) UpdateDepartment(ctx context.Context, id int64, in DepartmentInput) (*store.SupportDepartment, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	d := &store.SupportDepartment{ID: id, Name: in.Name, Description: in.Description, NotifyEmail: in.NotifyEmail, Hidden: in.Hidden, Sort: in.Sort}
	// Customers need somewhere to write: the last visible department
	// stays visible.
	if in.Hidden {
		if err := s.keepOneVisible(ctx, id); err != nil {
			return nil, err
		}
	}
	if err := s.Store.UpdateSupportDepartment(ctx, d); err != nil {
		return nil, err
	}
	return s.Store.GetSupportDepartment(ctx, id)
}

// DeleteDepartment removes a department without tickets (store.ErrInUse
// otherwise: hide it instead).
func (s *Service) DeleteDepartment(ctx context.Context, id int64) error {
	if err := s.keepOneVisible(ctx, id); err != nil {
		return err
	}
	return s.Store.DeleteSupportDepartment(ctx, id)
}

func (s *Service) keepOneVisible(ctx context.Context, id int64) error {
	visible, err := s.Departments(ctx, false)
	if err != nil {
		return err
	}
	if len(visible) == 1 && visible[0].ID == id {
		return fmt.Errorf("%w: customers need at least one department; add another first", ErrConflict)
	}
	return nil
}

// ---- Canned replies ----

// CannedInput creates or changes a canned reply.
type CannedInput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (in *CannedInput) validate() error {
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if in.Title == "" || len(in.Title) > 100 || strings.ContainsAny(in.Title, "\r\n") {
		return fmt.Errorf("%w: a canned reply needs a title (up to 100 characters)", ErrInvalid)
	}
	if in.Body == "" || len(in.Body) > maxBody {
		return fmt.Errorf("%w: a canned reply needs text (up to %d characters)", ErrInvalid, maxBody)
	}
	return nil
}

func (s *Service) CreateCanned(ctx context.Context, in CannedInput) (*store.CannedReply, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	c := &store.CannedReply{Title: in.Title, Body: in.Body}
	if err := s.Store.CreateCannedReply(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) UpdateCanned(ctx context.Context, id int64, in CannedInput) (*store.CannedReply, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	c := &store.CannedReply{ID: id, Title: in.Title, Body: in.Body}
	if err := s.Store.UpdateCannedReply(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}
