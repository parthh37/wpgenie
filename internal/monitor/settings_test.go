package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSettingsValidation(t *testing.T) {
	ok := func() Settings {
		s := DefaultSettings()
		s.Email = Email{Enabled: true, Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS, Username: "u", Password: "p",
			From: "WPGenie <alerts@example.com>", To: []string{"ops@example.com"}}
		s.Webhooks = []Webhook{{Name: "Slack", Enabled: true, URL: "https://hooks.slack.com/services/T/B/X"}}
		return s
	}
	cases := []struct {
		name   string
		change func(*Settings)
		bad    bool
	}{
		{"defaults with channels", func(*Settings) {}, false},
		{"email off and empty", func(s *Settings) { s.Email = Email{} }, false},
		{"warn above critical", func(s *Settings) { s.DiskWarnPercent, s.DiskCriticalPercent = 96, 95 }, true},
		{"critical over 100", func(s *Settings) { s.DiskCriticalPercent = 101 }, true},
		{"cert critical >= warn", func(s *Settings) { s.CertCriticalDays = 14 }, true},
		{"down after 0", func(s *Settings) { s.DownAfter = 0 }, true},
		{"renotify never", func(s *Settings) { s.RenotifyHours = 0 }, false},
		{"renotify negative", func(s *Settings) { s.RenotifyHours = -1 }, true},
		{"plain smtp refused", func(s *Settings) { s.Email.TLS = "none" }, true},
		{"implicit tls", func(s *Settings) { s.Email.TLS, s.Email.Port = TLSImplicit, 465 }, false},
		{"smtp host with config syntax", func(s *Settings) { s.Email.Host = "smtp.example.com\r\nX" }, true},
		{"smtp host IP", func(s *Settings) { s.Email.Host = "127.0.0.1" }, false},
		{"bad from", func(s *Settings) { s.Email.From = "not an address" }, true},
		{"no recipients", func(s *Settings) { s.Email.To = nil }, true},
		{"header injection in recipient", func(s *Settings) { s.Email.To = []string{"a@example.com\r\nBcc: x@evil.test"} }, true},
		{"password with newline", func(s *Settings) { s.Email.Password = "a\nb" }, true},
		{"http webhook", func(s *Settings) { s.Webhooks[0].URL = "http://hooks.example.com/x" }, true},
		{"webhook with credentials", func(s *Settings) { s.Webhooks[0].URL = "https://u:p@hooks.example.com/x" }, true},
		{"new webhook without url", func(s *Settings) { s.Webhooks[0].URL = "" }, true},
		{"short secret", func(s *Settings) { s.Webhooks[0].Secret = "short" }, true},
		{"too many webhooks", func(s *Settings) {
			for range maxWebhooks {
				s.Webhooks = append(s.Webhooks, s.Webhooks[0])
			}
		}, true},
	}
	for _, c := range cases {
		in := ok()
		c.change(&in)
		err := in.merge(DefaultSettings(), map[string]string{})
		if c.bad != (err != nil) {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.bad)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v is not ErrInvalid", c.name, err)
		}
	}
}

// Secrets are write-only, and an omitted secret is kept only where it
// would go to the same place.
func TestSettingsSecrets(t *testing.T) {
	ctx := context.Background()
	s := &Service{Store: newStore(t), Log: quietLog()}
	in := DefaultSettings()
	in.Email = Email{Enabled: true, Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS, Username: "u", Password: "hunter2",
		From: "alerts@example.com", To: []string{"ops@example.com"}}
	in.Webhooks = []Webhook{{Name: "chat", Enabled: true, URL: "https://chat.example.com/hooks/secret-path"}}
	saved, gen, err := s.SetSettings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	id := saved.Webhooks[0].ID
	if id == "" || len(gen[id]) != 64 || saved.Webhooks[0].Secret != gen[id] {
		t.Fatalf("generated secret: %q %v", id, gen)
	}

	red := saved.Redacted()
	b, _ := json.Marshal(red)
	for _, secret := range []string{"hunter2", "secret-path", gen[id]} {
		if strings.Contains(string(b), secret) {
			t.Errorf("redacted settings contain %q: %s", secret, b)
		}
	}
	if !red.Email.PasswordSet || !red.Webhooks[0].SecretSet || red.Webhooks[0].URLHint != "https://chat.example.com/…" {
		t.Errorf("redacted flags: %+v", red)
	}

	// The dashboard sends back what it got: secrets stay.
	again, gen2, err := s.SetSettings(ctx, red)
	if err != nil {
		t.Fatal(err)
	}
	if len(gen2) != 0 || again.Email.Password != "hunter2" || again.Webhooks[0].URL != in.Webhooks[0].URL ||
		again.Webhooks[0].Secret != gen[id] || again.Webhooks[0].ID != id {
		t.Errorf("secrets not kept: %+v %v", again, gen2)
	}

	// Another server or user: the stored password isn't sent there.
	moved := red
	moved.Email.Host = "evil.example.net"
	got, _, err := s.SetSettings(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email.Password != "" {
		t.Error("password followed the host change")
	}
	// A new URL gets a new signing secret.
	s.SetSettings(ctx, again)
	moved = again.Redacted()
	moved.Webhooks[0].URL = "https://other.example.org/hook"
	got, gen3, err := s.SetSettings(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhooks[0].Secret == gen[id] || gen3[id] == "" {
		t.Error("signing secret followed the URL change")
	}
	// An unknown ID is a new webhook: it can't borrow another's URL.
	stranger := DefaultSettings()
	stranger.Webhooks = []Webhook{{ID: "nope", Enabled: true}}
	if _, _, err := s.SetSettings(ctx, stranger); !errors.Is(err, ErrInvalid) {
		t.Errorf("webhook without URL: %v", err)
	}
}
