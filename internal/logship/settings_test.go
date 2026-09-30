package logship

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
		s.Enabled = true
		s.Destination = testDestination()
		return s
	}
	cases := []struct {
		name   string
		change func(*Settings)
		bad    bool
	}{
		{"defaults with a destination", func(*Settings) {}, false},
		{"off with nothing entered", func(s *Settings) { s.Enabled, s.Destination = false, Destination{} }, false},
		{"off with a half-entered destination", func(s *Settings) { s.Enabled, s.Destination.SecretKey = false, "" }, true},
		{"on without a destination", func(s *Settings) { s.Destination = Destination{Provider: "aws"} }, true},
		{"unknown provider", func(s *Settings) { s.Destination.Provider = "dropbox" }, true},
		{"endpoint with a path", func(s *Settings) { s.Destination.Endpoint = "https://s3.example.com/bucket" }, true},
		{"endpoint not http", func(s *Settings) { s.Destination.Endpoint = "ftp://s3.example.com" }, true},
		{"endpoint with credentials", func(s *Settings) { s.Destination.Endpoint = "https://u:p@s3.example.com" }, true},
		{"bucket with a slash", func(s *Settings) { s.Destination.Bucket = "a/b" }, true},
		{"prefix escaping", func(s *Settings) { s.Destination.Prefix = "../x/" }, true},
		{"prefix with a template", func(s *Settings) { s.Destination.Prefix = "{{ x }}/" }, true},
		{"prefix without its slash", func(s *Settings) { s.Destination.Prefix = "logs" }, false},
		{"empty prefix", func(s *Settings) { s.Destination.Prefix = "" }, false},
		{"secret with a newline", func(s *Settings) { s.Destination.SecretKey = "a\nb" }, true},
		{"key id with a newline", func(s *Settings) { s.Destination.AccessKeyID = "AKIA\nX=1" }, true},
		{"region with spaces", func(s *Settings) { s.Destination.Region = "us east" }, true},
		{"unknown type", func(s *Settings) { s.Types["kernel"] = true }, true},
		{"zstd", func(s *Settings) { s.Compression = "zstd" }, false},
		{"brotli", func(s *Settings) { s.Compression = "br" }, true},
		{"batch too big", func(s *Settings) { s.BatchMaxMB = 500 }, true},
		{"batch too quick", func(s *Settings) { s.BatchMaxSeconds = 5 }, true},
		{"spool too small", func(s *Settings) { s.SpoolCapMB = 1 }, true},
		{"archive kept forever", func(s *Settings) { s.ArchiveRetentionDays = 0 }, false},
		{"negative retention", func(s *Settings) { s.ArchiveRetentionDays = -1 }, true},
		{"no access logs kept", func(s *Settings) { s.LocalAccessLogs = 0 }, true},
		{"container logs uncapped", func(s *Settings) { s.ContainerLogMB = 0 }, true},
	}
	for _, c := range cases {
		in := ok()
		c.change(&in)
		err := in.merge(DefaultSettings())
		if c.bad != (err != nil) {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.bad)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v is not ErrInvalid", c.name, err)
		}
	}
	// Normalised: the prefix ends in a slash, types left out keep theirs.
	in := ok()
	in.Destination.Prefix = "logs"
	delete(in.Types, TypeMail)
	if err := in.merge(DefaultSettings()); err != nil || in.Destination.Prefix != "logs/" || !in.Types[TypeMail] {
		t.Errorf("normalised: %v %+v", err, in)
	}
}

// The secret is write-only, and kept only where it would go to the same
// place (endpoint, bucket and access key unchanged).
func TestSettingsSecret(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t)
	saved := enable(t, s, nil)
	if saved.Destination.SecretKey == "" {
		t.Fatal("secret not stored")
	}
	red := saved.Redacted()
	b, _ := json.Marshal(red)
	if strings.Contains(string(b), testDestination().SecretKey) || !red.Destination.SecretSet ||
		!strings.Contains(string(b), `"secret_key_set":true`) {
		t.Errorf("redacted settings: %s", b)
	}
	if saved.Destination.SecretKey == "" || saved.Types[TypeAccess] != true {
		t.Error("Redacted changed the settings it was called on")
	}

	// Same place, no secret: kept.
	in := red
	in.BatchMaxSeconds = 120
	if _, err := s.SetSettings(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Settings(ctx)
	if got.Destination.SecretKey != testDestination().SecretKey || got.BatchMaxSeconds != 120 {
		t.Errorf("secret not kept: %+v", got.Destination)
	}
	// Another endpoint, bucket or key: the secret must be given again.
	for _, change := range []func(*Destination){
		func(d *Destination) { d.Endpoint = "https://evil.example.net" },
		func(d *Destination) { d.Bucket = "other-bucket" },
		func(d *Destination) { d.AccessKeyID = "AKIAOTHER" },
	} {
		in := red
		change(&in.Destination)
		if _, err := s.SetSettings(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: secret reused: %v", in.Destination, err)
		}
	}
	// The set flag in input is ignored (output only).
	in = red
	in.Destination.SecretSet = true
	in.Destination.Endpoint = "https://other.example.com"
	if _, err := s.SetSettings(ctx, in); err == nil {
		t.Error("secret_key_set in input stood for a secret")
	}
}

// Settings stored by an older version get the types added since at their
// default.
func TestSettingsNewTypes(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t)
	s.Store.SetSetting(ctx, settingKey, `{"enabled":false,"types":{"access":false},"compression":"gzip"}`)
	set, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if set.Types[TypeAccess] || !set.Types[TypeAudit] || set.Types[TypeContainers] || set.SpoolCapMB != 1024 {
		t.Errorf("%+v", set)
	}
}
