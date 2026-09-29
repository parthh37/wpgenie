package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"strings"
	"unicode"

	"github.com/parthh37/wpgenie/internal/domain"
)

// ErrInvalid marks settings the API should refuse with 400.
var ErrInvalid = errors.New("invalid monitoring settings")

// settingKey holds Settings (JSON, secrets included) in the store's
// settings table, which is readable by root only.
const settingKey = "monitoring"

// Settings are the alert thresholds and notification channels. Secrets
// (the SMTP password, webhook URLs and signing secrets) are write-only:
// Redacted() is what the API returns.
type Settings struct {
	// Filesystem use (percent of the space usable by the daemon).
	DiskWarnPercent     float64 `json:"disk_warn_percent"`
	DiskCriticalPercent float64 `json:"disk_critical_percent"`
	// Days before a served certificate expires.
	CertWarnDays     int `json:"cert_warn_days"`
	CertCriticalDays int `json:"cert_critical_days"`
	// DownAfter consecutive failed probes (one a minute) make a site down.
	DownAfter int `json:"down_after"`
	// RenotifyHours repeats a firing alert's notification; 0: never.
	RenotifyHours int `json:"renotify_hours"`

	Email    Email     `json:"email"`
	Webhooks []Webhook `json:"webhooks"`
}

// Email sends notifications through an SMTP server that offers TLS: the
// password is never sent in clear, so plain SMTP isn't an option.
type Email struct {
	Enabled  bool     `json:"enabled"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	TLS      string   `json:"tls"` // starttls (submission, 587) | tls (implicit, 465)
	Username string   `json:"username"`
	Password string   `json:"password,omitempty"` // write-only
	From     string   `json:"from"`
	To       []string `json:"to"`
	// PasswordSet is output only: whether a password is stored.
	PasswordSet bool `json:"password_set"`
}

// Webhook posts JSON to a URL. Chat webhook URLs (Slack, Discord,
// Mattermost) are credentials themselves, so the URL is write-only like
// the signing secret: the API shows URLHint (scheme and host) instead.
type Webhook struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`    // write-only; "" on update keeps the stored one
	Secret  string `json:"secret,omitempty"` // write-only; HMAC-SHA256 key for X-WPGenie-Signature
	// Output only.
	URLHint   string `json:"url_hint,omitempty"`
	SecretSet bool   `json:"secret_set"`
}

const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"

	maxWebhooks   = 10
	maxRecipients = 20
)

// DefaultSettings: warn at 85% disk and 14 days of certificate left,
// critical at 95% and 3 days; a site is down after three failed probes in a
// row; firing alerts are repeated every four hours.
func DefaultSettings() Settings {
	return Settings{DiskWarnPercent: 85, DiskCriticalPercent: 95, CertWarnDays: 14, CertCriticalDays: 3,
		DownAfter: 3, RenotifyHours: 4, Email: Email{Port: 587, TLS: TLSStartTLS}, Webhooks: []Webhook{}}
}

// Settings returns the stored settings, secrets included (never hand them
// to API clients: see Redacted).
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	v, err := s.Store.Setting(ctx, settingKey)
	if err != nil || v == "" {
		return DefaultSettings(), err
	}
	set := DefaultSettings()
	if err := json.Unmarshal([]byte(v), &set); err != nil {
		return DefaultSettings(), err
	}
	if set.Webhooks == nil {
		set.Webhooks = []Webhook{}
	}
	return set, nil
}

// Redacted is the settings without their secrets, for the API.
func (set Settings) Redacted() Settings {
	out := set
	out.Email.PasswordSet = set.Email.Password != ""
	out.Email.Password = ""
	out.Webhooks = make([]Webhook, len(set.Webhooks))
	for i, w := range set.Webhooks {
		w.URLHint = urlHint(w.URL)
		w.SecretSet = w.Secret != ""
		w.URL, w.Secret = "", ""
		out.Webhooks[i] = w
	}
	return out
}

func urlHint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// SetSettings validates and stores new settings. Omitted secrets keep the
// stored ones, but only where they would go to the same place: otherwise
// anyone with API access could send the stored SMTP password to a server of
// their choosing (the rule the mail relay follows). Webhooks without a
// secret get a generated one, returned once in generated (by webhook ID).
func (s *Service) SetSettings(ctx context.Context, in Settings) (Settings, map[string]string, error) {
	old, err := s.Settings(ctx)
	if err != nil {
		return Settings{}, nil, err
	}
	generated := map[string]string{}
	if err := in.merge(old, generated); err != nil {
		return Settings{}, nil, err
	}
	b, err := json.Marshal(in)
	if err != nil {
		return Settings{}, nil, err
	}
	if err := s.Store.SetSetting(ctx, settingKey, string(b)); err != nil {
		return Settings{}, nil, err
	}
	return in, generated, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// merge validates in and fills in the secrets kept from old.
func (in *Settings) merge(old Settings, generated map[string]string) error {
	switch {
	case in.DiskWarnPercent <= 0 || in.DiskWarnPercent >= 100:
		return invalid("disk_warn_percent must be between 0 and 100")
	case in.DiskCriticalPercent <= in.DiskWarnPercent || in.DiskCriticalPercent > 100:
		return invalid("disk_critical_percent must be above disk_warn_percent and at most 100")
	case in.CertWarnDays < 1 || in.CertWarnDays > 90:
		return invalid("cert_warn_days must be between 1 and 90")
	case in.CertCriticalDays < 0 || in.CertCriticalDays >= in.CertWarnDays:
		return invalid("cert_critical_days must be at least 0 and below cert_warn_days")
	case in.DownAfter < 1 || in.DownAfter > 60:
		return invalid("down_after must be between 1 and 60 failed checks")
	case in.RenotifyHours < 0 || in.RenotifyHours > 168:
		return invalid("renotify_hours must be between 0 (never) and 168")
	}
	if err := in.Email.merge(old.Email); err != nil {
		return err
	}
	if len(in.Webhooks) > maxWebhooks {
		return invalid("at most %d webhooks", maxWebhooks)
	}
	if in.Webhooks == nil {
		in.Webhooks = []Webhook{}
	}
	stored := map[string]Webhook{}
	for _, w := range old.Webhooks {
		stored[w.ID] = w
	}
	seen := map[string]bool{}
	for i := range in.Webhooks {
		w := &in.Webhooks[i]
		w.URLHint, w.SecretSet = "", false
		prev, known := stored[w.ID]
		if w.ID == "" || !known {
			w.ID = randomHex(6)
		}
		if seen[w.ID] {
			return invalid("webhook %q is listed twice", w.ID)
		}
		seen[w.ID] = true
		w.Name = strings.TrimSpace(w.Name)
		if len(w.Name) > 64 || strings.ContainsFunc(w.Name, unicode.IsControl) {
			return invalid("webhook names are at most 64 characters")
		}
		switch {
		case w.URL == "" && known:
			w.URL = prev.URL
		case w.URL == "":
			return invalid("webhook %q needs a URL", w.Name)
		default:
			if err := checkWebhookURL(w.URL); err != nil {
				return err
			}
		}
		if w.Secret != "" {
			if len(w.Secret) < 16 || len(w.Secret) > 256 || strings.ContainsFunc(w.Secret, unicode.IsControl) {
				return invalid("webhook secrets are 16 to 256 characters")
			}
			continue
		}
		if known && prev.URL == w.URL && prev.Secret != "" {
			w.Secret = prev.Secret
			continue
		}
		w.Secret = randomHex(32)
		generated[w.ID] = w.Secret
	}
	return nil
}

func (e *Email) merge(old Email) error {
	e.PasswordSet = false
	e.Host = strings.TrimSpace(strings.ToLower(e.Host))
	if !e.Enabled && e.Host == "" {
		*e = Email{Port: 587, TLS: TLSStartTLS}
		return nil
	}
	if ip := net.ParseIP(e.Host); ip == nil {
		h, err := domain.Normalize(e.Host)
		if err != nil {
			return invalid("email: SMTP host")
		}
		e.Host = h
	}
	if e.Port == 0 {
		e.Port = 587
	}
	if e.Port < 1 || e.Port > 65535 {
		return invalid("email: port")
	}
	if e.TLS != TLSStartTLS && e.TLS != TLSImplicit {
		return invalid("email: tls must be %q or %q (credentials and alerts are never sent in clear)", TLSStartTLS, TLSImplicit)
	}
	if len(e.Username) > 256 || len(e.Password) > 256 ||
		strings.ContainsFunc(e.Username+e.Password, unicode.IsControl) {
		return invalid("email: username or password")
	}
	if e.Password == "" && old.Password != "" && old.Host == e.Host && old.Port == e.Port && old.Username == e.Username {
		e.Password = old.Password
	}
	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return invalid("email: from address")
	}
	e.From = from.Address
	if len(e.To) == 0 || len(e.To) > maxRecipients {
		return invalid("email: 1 to %d recipients", maxRecipients)
	}
	for i, t := range e.To {
		a, err := mail.ParseAddress(strings.TrimSpace(t))
		if err != nil {
			return invalid("email: recipient %q", t)
		}
		e.To[i] = a.Address
	}
	return nil
}

// checkWebhookURL accepts https URLs only: alerts name sites and the
// request is signed, and a chat webhook's URL is its credential. Where it
// may connect is checked again when dialling (see notify.go).
func checkWebhookURL(raw string) error {
	if len(raw) > 2048 {
		return invalid("webhook URL too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return invalid("webhook URLs must be https://host/... without credentials")
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
