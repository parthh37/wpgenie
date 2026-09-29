package config

import (
	"strings"
	"testing"
)

func TestValidateDatabaseURL(t *testing.T) {
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"", true}, // SQLite
		{"postgres://wpgenie:pw@127.0.0.1:5432/wpgenie", true},
		{"postgresql://wpgenie:pw@db.internal.example/wpgenie?sslmode=verify-full", true},
		{"postgres://wpgenie:pw@db.internal.example/wpgenie", true}, // verify-full implied
		{"postgres://wpgenie:pw@db.internal.example/wpgenie?sslmode=disable", false},
		{"sqlite:///var/lib/wpgenie/wpgenie.db", false},
		{"/var/lib/wpgenie/wpgenie.db", false},
	} {
		cfg := Default()
		cfg.APIToken, cfg.ShieldSecret, cfg.MariaDBDSN = strings.Repeat("a", 32), strings.Repeat("b", 32), "root:x@tcp(db)/"
		cfg.DatabaseURL = c.url
		err := cfg.Validate()
		if (err == nil) != c.ok {
			t.Errorf("database_url %q: %v", c.url, err)
		}
		if err != nil && strings.Contains(err.Error(), ":pw@") {
			t.Errorf("error shows the password: %v", err)
		}
	}
}
