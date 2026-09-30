package logship

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/parthh37/wpgenie/internal/offload"
)

// ErrInvalid marks settings (or requests) the API refuses with 400.
var ErrInvalid = errors.New("invalid")

// settingKey holds Settings (JSON, the secret included) in the store's
// settings table, which is readable by root only.
const settingKey = "logship"

// Settings are what ships where, and how much stays on this server. The
// secret key is write-only: Redacted() is what the API returns.
type Settings struct {
	Enabled     bool            `json:"enabled"`
	Destination Destination     `json:"destination"`
	Types       map[string]bool `json:"types"`
	// Compression of the objects: gzip (readable everywhere, and in the
	// panel's archive browser) or zstd (smaller).
	Compression string `json:"compression"`
	// An object is written when a batch reaches BatchMaxMB (before
	// compression) or is BatchMaxSeconds old.
	BatchMaxMB      int `json:"batch_max_mb"`
	BatchMaxSeconds int `json:"batch_max_seconds"`
	// SpoolCapMB bounds the logs waiting on this server while the
	// destination can't be reached: the oldest are dropped past it.
	SpoolCapMB int `json:"spool_cap_mb"`
	// ArchiveRetentionDays: objects older than this are deleted from the
	// bucket every night (0: kept forever).
	ArchiveRetentionDays int `json:"archive_retention_days"`
	// LocalAccessLogs is how many rotated access log files (100 MB each)
	// Caddy keeps on this server while shipping is on.
	LocalAccessLogs int `json:"local_access_logs"`
	// ContainerLogMB caps each WPGenie container's own log on this server
	// while shipping is on (two files of this size).
	ContainerLogMB int `json:"container_log_mb"`
}

// Destination is an S3-compatible bucket. SecretKey is write-only.
type Destination struct {
	Provider    string `json:"provider"`
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region"`
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix"`
	AccessKeyID string `json:"access_key_id"`
	SecretKey   string `json:"secret_key,omitempty"`
	// PathStyle puts the bucket in the URL's path (MinIO, most
	// self-hosted services) rather than its host name (AWS).
	PathStyle bool `json:"path_style"`
	// SecretSet is output only: whether a secret key is stored.
	SecretSet bool `json:"secret_key_set"`
}

// Providers are the destination presets the panel offers (the endpoint
// and region hints are the panel's; the daemon treats them all alike).
var Providers = []string{"aws", "r2", "b2", "wasabi", "spaces", "minio", "custom"}

// Compressions of the shipped objects.
var Compressions = []string{"gzip", "zstd"}

// Limits of the numeric settings.
const (
	minBatchMB, maxBatchMB           = 1, 100
	minBatchSeconds, maxBatchSeconds = 30, 3600
	minSpoolMB, maxSpoolMB           = 64, 100 << 10
	maxRetentionDays                 = 3650
	minAccessLogs, maxAccessLogs     = 1, 10
	minContainerMB, maxContainerMB   = 1, 1024
	// containerLogFiles is how many files a container's log rotates over.
	containerLogFiles = 2
)

// DefaultSettings: off; when turned on, everything but containers' own
// output ships, gzip, batches of 10 MB or 5 minutes, 1 GB of logs may wait
// here, archives are kept 90 days, and this server keeps 2 old access logs
// and 10 MB per container log.
func DefaultSettings() Settings {
	set := Settings{Destination: Destination{Provider: "aws", Prefix: "logs/"}, Types: map[string]bool{},
		Compression: "gzip", BatchMaxMB: 10, BatchMaxSeconds: 300, SpoolCapMB: 1024, ArchiveRetentionDays: 90,
		LocalAccessLogs: 2, ContainerLogMB: 10}
	for _, t := range Types {
		set.Types[t.Name] = t.Default
	}
	return set
}

// Settings returns the stored settings, the secret included (never hand
// them to API clients: see Redacted).
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	v, err := s.Store.Setting(ctx, settingKey)
	if err != nil || v == "" {
		return DefaultSettings(), err
	}
	set := DefaultSettings()
	if err := json.Unmarshal([]byte(v), &set); err != nil {
		return DefaultSettings(), err
	}
	// Types added by a later version start at their default.
	for _, t := range Types {
		if _, ok := set.Types[t.Name]; !ok {
			set.Types[t.Name] = t.Default
		}
	}
	return set, nil
}

// Redacted is the settings without the secret, for the API.
func (set Settings) Redacted() Settings {
	out := set
	out.Types = maps.Clone(set.Types)
	out.Destination.SecretSet = set.Destination.SecretKey != ""
	out.Destination.SecretKey = ""
	return out
}

// SetSettings validates and stores new settings, then applies them (in the
// background: starting the shipper may pull its image). An omitted secret
// keeps the stored one, but only where it would go to the same place:
// otherwise anyone with API access could send the stored key to a server
// of their choosing.
func (s *Service) SetSettings(ctx context.Context, in Settings) (Settings, error) {
	old, err := s.Settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	if err := in.merge(old); err != nil {
		return Settings{}, err
	}
	b, err := json.Marshal(in)
	if err != nil {
		return Settings{}, err
	}
	if err := s.Store.SetSetting(ctx, settingKey, string(b)); err != nil {
		return Settings{}, err
	}
	s.setCurrent(in)
	s.Kick()
	return in, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// merge validates in, fills in what was left out and the secret kept from
// old.
func (in *Settings) merge(old Settings) error {
	if in.Types == nil {
		in.Types = map[string]bool{}
	}
	for name := range in.Types {
		if TypeByName(name) == nil {
			return invalid("unknown log type %q", name)
		}
	}
	for _, t := range Types {
		if _, ok := in.Types[t.Name]; !ok {
			in.Types[t.Name] = old.Types[t.Name]
		}
	}
	if in.Compression == "" {
		in.Compression = "gzip"
	}
	switch {
	case !slices.Contains(Compressions, in.Compression):
		return invalid("compression must be gzip or zstd")
	case in.BatchMaxMB < minBatchMB || in.BatchMaxMB > maxBatchMB:
		return invalid("batch size must be between %d and %d MB", minBatchMB, maxBatchMB)
	case in.BatchMaxSeconds < minBatchSeconds || in.BatchMaxSeconds > maxBatchSeconds:
		return invalid("batch time must be between %d seconds and %d minutes", minBatchSeconds, maxBatchSeconds/60)
	case in.SpoolCapMB < minSpoolMB || in.SpoolCapMB > maxSpoolMB:
		return invalid("the space for logs waiting on this server must be between %d MB and %d GB", minSpoolMB, maxSpoolMB>>10)
	case in.ArchiveRetentionDays < 0 || in.ArchiveRetentionDays > maxRetentionDays:
		return invalid("archives are kept between 1 and %d days (0 keeps them forever)", maxRetentionDays)
	case in.LocalAccessLogs < minAccessLogs || in.LocalAccessLogs > maxAccessLogs:
		return invalid("keep between %d and %d old access log files on the server", minAccessLogs, maxAccessLogs)
	case in.ContainerLogMB < minContainerMB || in.ContainerLogMB > maxContainerMB:
		return invalid("container logs are capped between %d and %d MB", minContainerMB, maxContainerMB)
	}
	return in.Destination.merge(old.Destination, in.Enabled)
}

// merge normalises and validates a destination; complete is required when
// shipping is on (a draft may be saved while it's off).
func (d *Destination) merge(old Destination, complete bool) error {
	d.SecretSet = false
	d.Provider = strings.TrimSpace(d.Provider)
	if d.Provider == "" {
		d.Provider = "custom"
	}
	if !slices.Contains(Providers, d.Provider) {
		return invalid("unknown storage provider %q", d.Provider)
	}
	d.Endpoint = strings.TrimRight(strings.TrimSpace(d.Endpoint), "/")
	d.Region = strings.TrimSpace(d.Region)
	d.Bucket = strings.TrimSpace(d.Bucket)
	d.AccessKeyID = strings.TrimSpace(d.AccessKeyID)
	d.Prefix = strings.TrimSpace(d.Prefix)
	if d.Prefix != "" && !strings.HasSuffix(d.Prefix, "/") {
		d.Prefix += "/"
	}
	if d.SecretKey == "" && old.SecretKey != "" && old.Endpoint == d.Endpoint && old.Bucket == d.Bucket &&
		old.AccessKeyID == d.AccessKeyID {
		d.SecretKey = old.SecretKey
	}
	if !complete && d.Endpoint == "" && d.Bucket == "" && d.AccessKeyID == "" && d.SecretKey == "" {
		return nil // nothing entered yet
	}
	return d.check()
}

// check validates a complete destination (the same rules as uploads
// offload's, which runs the same rclone against it).
func (d Destination) check() error {
	if d.Endpoint == "" || d.Bucket == "" || d.AccessKeyID == "" {
		return invalid("the destination needs an endpoint, a bucket and an access key")
	}
	if d.SecretKey == "" {
		return invalid("enter the secret key (it's needed again when the endpoint, bucket or access key changes)")
	}
	if err := d.target().Validate(); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// target is the destination for rclone (uploads offload's runner), at the
// root of the logs' prefix.
func (d Destination) target() offload.Target {
	return offload.Target{Endpoint: d.Endpoint, Region: d.Region, Bucket: d.Bucket, Prefix: d.Prefix,
		AccessKeyID: d.AccessKeyID, SecretKey: d.SecretKey}
}

// region is the region to sign requests for: S3-compatible services that
// have none accept us-east-1, R2 wants "auto".
func (d Destination) region() string {
	switch {
	case d.Region != "":
		return d.Region
	case d.Provider == "r2":
		return "auto"
	}
	return "us-east-1"
}
