// Package config loads the wpgenie daemon configuration.
//
// The installer writes /etc/wpgenie/config.json with generated secrets; every
// field has a sensible default so a partial file is valid.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const DefaultPath = "/etc/wpgenie/config.json"

type Config struct {
	// DataDir holds the panel database, site files and backups.
	DataDir string `json:"data_dir"`
	// ListenAddr is where the API, UI and shield endpoints listen. Keep it on
	// loopback: Caddy (host network) is the only public entry point.
	ListenAddr string `json:"listen_addr"`
	// PanelDomain, if set, is served by Caddy with automatic TLS.
	PanelDomain string `json:"panel_domain"`
	ACMEEmail   string `json:"acme_email"`

	CaddyAdmin    string `json:"caddy_admin"`
	CaddyfilePath string `json:"caddyfile_path"`
	AccessLog     string `json:"access_log"`

	DockerNetwork string `json:"docker_network"`
	PHPImage      string `json:"php_image"`
	// SitePortBase is the first loopback port handed to a site's PHP-FPM.
	SitePortBase int `json:"site_port_base"`
	// MaxReplicas bounds how many PHP-FPM containers one site may run.
	MaxReplicas int `json:"max_replicas"`
	// DBMaxConnections must match MariaDB's max_connections. One site may use
	// at most half of it, so a traffic spike can't starve every other site.
	DBMaxConnections int `json:"db_max_connections"`
	// CronConcurrency is how many sites run WP-Cron at the same time.
	CronConcurrency int `json:"cron_concurrency"`
	// Mail stack (off until enabled in the panel). CaddyDataDir is Caddy's
	// /data on the host, where the mail server finds its certificate.
	MailImage    string `json:"mail_image"`
	WebmailImage string `json:"webmail_image"`
	WebmailPort  int    `json:"webmail_port"`
	CaddyDataDir string `json:"caddy_data_dir"`

	// UpdateRepo is the GitHub repository (owner/name) WPGenie updates
	// itself from. Releases must be signed with the key built into the binary.
	UpdateRepo string `json:"update_repo"`
	// MaintenanceHour (0-23, server local time) starts the nightly window in
	// which security scans and automatic WordPress updates run.
	MaintenanceHour int `json:"maintenance_hour"`

	// MariaDBDSN is a root DSN used only to create per-site databases/users.
	MariaDBDSN  string `json:"mariadb_dsn"`
	MariaDBHost string `json:"mariadb_host"` // hostname as seen from site containers
	RedisHost   string `json:"redis_host"`

	APIToken     string `json:"api_token"`
	ShieldSecret string `json:"shield_secret"`
}

func Default() *Config {
	return &Config{
		DataDir:          "/var/lib/wpgenie",
		ListenAddr:       "127.0.0.1:8088",
		CaddyAdmin:       "http://127.0.0.1:2019",
		CaddyfilePath:    "/etc/wpgenie/caddy/Caddyfile",
		AccessLog:        "/var/log/wpgenie/access.log",
		DockerNetwork:    "wpgenie",
		PHPImage:         "wpgenie/php:8.3",
		SitePortBase:     19000,
		MaxReplicas:      8,
		DBMaxConnections: 300,
		CronConcurrency:  4,
		MaintenanceHour:  3,
		UpdateRepo:       "parthh37/wpgenie",
		MailImage:        "ghcr.io/docker-mailserver/docker-mailserver:16",
		WebmailImage:     "roundcube/roundcubemail:1.7.x-apache",
		WebmailPort:      8089,
		CaddyDataDir:     "/var/lib/wpgenie/caddy",
		MariaDBHost:      "wpgenie-mariadb",
		RedisHost:        "wpgenie-redis",
	}
}

// Load reads path (falling back to $WPGENIE_CONFIG, then DefaultPath) on top
// of the defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("WPGENIE_CONFIG")
	}
	if path == "" {
		path = DefaultPath
	}
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	var errs []error
	if c.APIToken == "" || len(c.APIToken) < 32 {
		errs = append(errs, errors.New("api_token must be at least 32 characters"))
	}
	if len(c.ShieldSecret) < 32 {
		errs = append(errs, errors.New("shield_secret must be at least 32 characters"))
	}
	if c.MaxReplicas < 1 || c.DBMaxConnections < 20 || c.CronConcurrency < 1 {
		errs = append(errs, errors.New("max_replicas and cron_concurrency must be >= 1, db_max_connections >= 20"))
	}
	if c.MaintenanceHour < 0 || c.MaintenanceHour > 23 {
		errs = append(errs, errors.New("maintenance_hour must be between 0 and 23"))
	}
	if c.MariaDBDSN == "" {
		errs = append(errs, errors.New("mariadb_dsn is required"))
	}
	return errors.Join(errs...)
}

func (c *Config) SitesDir() string { return filepath.Join(c.DataDir, "sites") }
func (c *Config) DBPath() string   { return filepath.Join(c.DataDir, "wpgenie.db") }

// SiteDir holds a site's wp-config.php (outside the web root) and its
// docroot. The same absolute paths are mounted into Caddy (read-only) and the
// site's PHP container, so SCRIPT_FILENAME is identical everywhere.
func (c *Config) SiteDir(id string) string { return filepath.Join(c.SitesDir(), id) }

// SiteRoot is the document root (the WordPress install) of a site.
func (c *Config) SiteRoot(id string) string { return filepath.Join(c.SiteDir(id), "public") }
