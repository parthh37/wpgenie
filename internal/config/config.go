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
	"slices"
	"strings"

	"github.com/parthh37/wpgenie/internal/store"
)

const DefaultPath = "/etc/wpgenie/config.json"

type Config struct {
	// DataDir holds the panel database, site files and backups.
	DataDir string `json:"data_dir"`
	// DatabaseURL chooses the panel database: empty for SQLite at
	// DataDir/wpgenie.db (the default), or a postgres:// URL for a
	// PostgreSQL database several control-plane nodes can share. A server
	// that isn't on this machine must be reached over TLS (see
	// store.PostgresConfig); `wpgenie store migrate-to-postgres` moves the
	// SQLite data over.
	DatabaseURL string `json:"database_url"`
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
	// CaddyImage is Caddy built with the Coraza WAF module (images/caddy);
	// deploy/docker-compose.yml runs it.
	CaddyImage string `json:"caddy_image"`
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

	// PHPVersions are the PHP versions sites may run. PHPImage is the image
	// of the default one (its tag); the others are built from ImagesDir
	// (images/php) as <PHPImage repository>:<version> when a site first
	// switches to them.
	PHPVersions []string `json:"php_versions"`
	// ImagesDir holds the image sources the installer put on the server.
	ImagesDir string `json:"images_dir"`
	// ResticImage runs backups (restic in a container, see internal/backup).
	ResticImage string `json:"restic_image"`
	// RcloneImage copies uploads to object storage (uploads offload, see
	// internal/offload): rclone in a throwaway container per command.
	RcloneImage string `json:"rclone_image"`
	// JobConcurrency is how many heavy jobs (backups, restores, clones)
	// run at the same time.
	JobConcurrency int `json:"job_concurrency"`
	// SFTP server for site files (one container, every user chrooted to a
	// site's directory), published on SFTPPort on all interfaces.
	SFTPImage string `json:"sftp_image"`
	SFTPPort  int    `json:"sftp_port"`
	// Adminer (database admin, on demand) listens on 127.0.0.1:AdminerPort.
	AdminerImage string `json:"adminer_image"`
	AdminerPort  int    `json:"adminer_port"`

	// MariaDBDSN is a root DSN used only to create per-site databases/users.
	MariaDBDSN  string `json:"mariadb_dsn"`
	MariaDBHost string `json:"mariadb_host"` // hostname as seen from site containers
	RedisHost   string `json:"redis_host"`

	APIToken     string `json:"api_token"`
	ShieldSecret string `json:"shield_secret"`

	// Cluster (several servers). ClusterListen is where this server's
	// cluster listener (mutual TLS, port 7443 by default) accepts the
	// panel and other nodes; ClusterDir holds its keys and certificates.
	// ClusterAddress is the address other servers reach this one's
	// listener at (host:port; the panel's is sent to nodes for tunnels).
	ClusterListen  string `json:"cluster_listen"`
	ClusterDir     string `json:"cluster_dir"`
	ClusterAddress string `json:"cluster_address"`
	// PlaceOnControl lets the panel's own server take new sites when
	// placement picks a server (default true).
	PlaceOnControl *bool `json:"place_on_control"`
	// ValkeyAddr is where tunnels from other nodes' replicas of a site that
	// lives here reach Valkey ("": the container's address on the Docker
	// network; Valkey has no password, so it is never published on the
	// host). IngressAddr is Caddy's listener for visitors another server
	// passes on (with their address in a PROXY protocol header) while a
	// moved site's DNS catches up.
	ValkeyAddr  string `json:"valkey_addr"`
	IngressAddr string `json:"ingress_addr"`
	// ValkeyACLDir holds Valkey's ACL file (mounted into its container);
	// ValkeyKey derives every site's cache password (root only). "": no
	// per-site cache users.
	ValkeyACLDir string `json:"valkey_acl_dir"`
	ValkeyKey    string `json:"valkey_key"`
	// LinkImage runs the tunnels a node's replicas of another server's
	// site use to reach that server's database (a static wpgenie binary
	// mounted into it; see site/spread.go).
	LinkImage string `json:"link_image"`
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
		CaddyImage:       "wpgenie/caddy:2",
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
		PHPVersions:      []string{"8.2", "8.3", "8.4"},
		ImagesDir:        "/opt/wpgenie/images",
		ResticImage:      "restic/restic:0.18.1",
		RcloneImage:      "rclone/rclone:1.75.1",
		JobConcurrency:   2,
		SFTPImage:        "wpgenie/sftp:1",
		SFTPPort:         2222,
		AdminerImage:     "wpgenie/adminer:1",
		AdminerPort:      8090,
		RedisHost:        "wpgenie-redis",
		ClusterListen:    ":7443",
		ClusterDir:       "/etc/wpgenie/cluster",
		IngressAddr:      "127.0.0.1:8444",
		LinkImage:        "alpine:3.22",
		ValkeyACLDir:     "/etc/wpgenie/valkey",
		ValkeyKey:        "/etc/wpgenie/valkey.key",
	}
}

// Path is the config file Load reads: path, else $WPGENIE_CONFIG, else
// DefaultPath.
func Path(path string) string {
	if path == "" {
		path = os.Getenv("WPGENIE_CONFIG")
	}
	if path == "" {
		path = DefaultPath
	}
	return path
}

// Load reads path (falling back to $WPGENIE_CONFIG, then DefaultPath) on top
// of the defaults.
func Load(path string) (*Config, error) {
	path = Path(path)
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
	if c.JobConcurrency < 1 || c.SFTPPort < 1 || c.SFTPPort > 65535 || c.AdminerPort < 1 || c.AdminerPort > 65535 {
		errs = append(errs, errors.New("job_concurrency must be >= 1; sftp_port and adminer_port must be valid ports"))
	}
	if !slices.Contains(c.PHPVersions, c.DefaultPHPVersion()) {
		errs = append(errs, fmt.Errorf("php_versions must include the version of php_image (%s)", c.DefaultPHPVersion()))
	}
	if c.MariaDBDSN == "" {
		errs = append(errs, errors.New("mariadb_dsn is required"))
	}
	if c.DatabaseURL != "" {
		// The same parsing and TLS rules the store applies when connecting.
		if _, err := store.PostgresConfig(c.DatabaseURL); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// PlaceSitesOnControl: see PlaceOnControl.
func (c *Config) PlaceSitesOnControl() bool { return c.PlaceOnControl == nil || *c.PlaceOnControl }

// MariaDBLoopback is MariaDB's address from this host (from MariaDBDSN's
// tcp(...) part).
func (c *Config) MariaDBLoopback() string {
	if _, rest, ok := strings.Cut(c.MariaDBDSN, "@tcp("); ok {
		if addr, _, ok := strings.Cut(rest, ")"); ok && addr != "" {
			return addr
		}
	}
	return "127.0.0.1:3306"
}

// IngressAddr: see Config.IngressAddr (Caddy's forwarded-visitor listener).
func (c *Config) IngressListen() string { return c.IngressAddr }

// DefaultPHPVersion is the PHP version of PHPImage (its tag).
func (c *Config) DefaultPHPVersion() string {
	if i := strings.LastIndex(c.PHPImage, ":"); i >= 0 {
		return c.PHPImage[i+1:]
	}
	return "8.3"
}

// PHPImageFor is the image of a PHP version.
func (c *Config) PHPImageFor(version string) string {
	if version == "" || version == c.DefaultPHPVersion() {
		return c.PHPImage
	}
	repo := c.PHPImage
	if i := strings.LastIndex(repo, ":"); i >= 0 {
		repo = repo[:i]
	}
	return repo + ":" + version
}

func (c *Config) SitesDir() string { return filepath.Join(c.DataDir, "sites") }
func (c *Config) DBPath() string   { return filepath.Join(c.DataDir, "wpgenie.db") }

// SiteDir holds a site's wp-config.php (outside the web root) and its
// docroot. The same absolute paths are mounted into Caddy (read-only) and the
// site's PHP container, so SCRIPT_FILENAME is identical everywhere.
func (c *Config) SiteDir(id string) string { return filepath.Join(c.SitesDir(), id) }

// SiteRoot is the document root (the WordPress install) of a site.
func (c *Config) SiteRoot(id string) string { return filepath.Join(c.SiteDir(id), "public") }
