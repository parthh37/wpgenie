package site

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/offload"
	"github.com/parthh37/wpgenie/internal/store"
)

// Uploads offload: a site's wp-content/uploads is copied to S3-compatible
// object storage, and Caddy fetches any upload missing on disk from the
// storage's public URL. That lets local copies go after a while (local_days)
// and, later, lets replicas on other servers serve every upload.
//
//   - Copies (RunOffload): every minute, the files changed since the last
//     copy started (their mtime); nightly, in the maintenance window, every
//     file the bucket lacks or holds an older copy of, found by listing the
//     bucket once. Remote objects are never deleted because a local copy is
//     missing: local copies may have been removed on purpose.
//   - rclone never reads the site's files. The site controls every path under
//     its docroot and could make wp-content/uploads a symlink: bind-mounted,
//     Docker would resolve it on the host (any directory published to a
//     public bucket); inside rclone's container it would reach the keys in
//     rclone's own /proc/self/environ. So the daemon copies the files to
//     upload into a directory of its own first, reading them through os.Root
//     (never outside the uploads directory, never through a symlink, never
//     blocking on a pipe), in batches.
//   - Deletes: an mu-plugin (images/php/offload.php) appends the uploads
//     WordPress deletes to logs/offload-deletes.queue; the daemon reads it
//     like the PHP error log, as untrusted input, and deletes those objects.
//   - The secret key never enters the PHP container, argv or `docker
//     inspect`, and no API returns it.

const (
	offloadWrapperPath = "wp-content/mu-plugins/wpgenie-offload.php"
	offloadUploadsDir  = "wp-content/uploads"
	// In the site directory's logs/, which PHP may write (open_basedir
	// includes the site directory) and Caddy can't read.
	offloadQueuePath = phpLogDir + "/offload-deletes.queue"
	offloadPokePath  = phpLogDir + "/offload.poke"
	offloadScript    = "/usr/local/share/wpgenie/offload.php"

	offloadTick  = 10 * time.Second
	offloadEvery = time.Minute
	// offloadPokeMin: a new upload or delete (PHP touches the poke file) is
	// copied within this much, rather than within a minute; also the most a
	// site can make the daemon start rclone for it.
	offloadPokeMin = 15 * time.Second
	// offloadMargin: an incremental copy also takes files changed a little
	// before the previous one started (a write in progress then).
	offloadMargin     = 2 * time.Minute
	offloadFullEvery  = 20 * time.Hour
	offloadFullMax    = 48 * time.Hour // a full copy even outside the window
	offloadMaxBackoff = time.Hour
	// Copies running at once: full copies (long, nightly) have slots of
	// their own, so a big site's never holds up everyone's new uploads.
	offloadParallel     = 3
	offloadParallelFull = 2
	offloadIncTimeout   = 30 * time.Minute
	// offloadFullTimeout bounds a full copy; a cut one resumes the next time
	// (every uploaded batch stays uploaded).
	offloadFullTimeout = 6 * time.Hour
	// Batches bound the scratch space a copy needs.
	offloadBatchBytes = 256 << 20
	offloadBatchFiles = 2000
	// The delete queue: read at most this much per pass (more is dropped
	// with an event: only a site flooding it gets there), and keep at most
	// offloadPendingMax deletes waiting per site.
	offloadQueueMax    = 16 << 20
	offloadPendingMax  = 500_000
	offloadDeleteBatch = 1000
	maxLocalDays       = 3650
)

// OffloadEngine copies files to object storage (offload.Rclone).
type OffloadEngine interface {
	Upload(ctx context.Context, t offload.Target, dir string) (offload.Stats, error)
	Download(ctx context.Context, t offload.Target, paths []string, dir string) (offload.Stats, error)
	Delete(ctx context.Context, t offload.Target, paths []string) (offload.Stats, error)
	List(ctx context.Context, t offload.Target) (map[string]offload.Object, error)
	Put(ctx context.Context, t offload.Target, name string, data []byte) error
	DeleteFile(ctx context.Context, t offload.Target, name string) error
}

// OffloadInput turns offload on (checked end to end: refused, not stored,
// if it doesn't work), changes it, or turns it off.
type OffloadInput struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"` // https://s3.eu-central-1.amazonaws.com
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	// Prefix is the key prefix (default <site-id>/uploads/).
	Prefix string `json:"prefix"`
	// AccessKeyID and SecretKey: empty keeps the stored ones (the secret
	// must be given again when the endpoint changes).
	AccessKeyID string `json:"access_key_id"`
	SecretKey   string `json:"secret_key"`
	// PublicURL is where the prefix is publicly readable: the bucket's own
	// URL or a CDN in front of it. Uploads missing on disk come from there.
	PublicURL string `json:"public_url"`
	// ACL is "public-read" for services that need it per object, or "".
	ACL string `json:"acl"`
	// LocalDays > 0 removes local copies of uploads older than that, once
	// the bucket is confirmed to hold them. 0 keeps every local copy.
	LocalDays int `json:"local_days"`
	// Force turns offload off although some uploads only exist in the bucket.
	Force bool `json:"force"`
}

// OffloadStatus never includes the secret key; the access key ID is masked.
type OffloadStatus struct {
	Enabled         bool       `json:"enabled"`
	Endpoint        string     `json:"endpoint"`
	Region          string     `json:"region"`
	Bucket          string     `json:"bucket"`
	Prefix          string     `json:"prefix"`
	AccessKeyID     string     `json:"access_key_id"`
	SecretSet       bool       `json:"secret_set"`
	PublicURL       string     `json:"public_url"`
	ACL             string     `json:"acl"`
	LocalDays       int        `json:"local_days"`
	Syncing         bool       `json:"syncing"`
	LastIncremental *time.Time `json:"last_incremental"`
	LastFull        *time.Time `json:"last_full"`
	LastAttempt     *time.Time `json:"last_attempt"`
	LastCleanup     *time.Time `json:"last_cleanup"`
	LastError       string     `json:"last_error"`
	Failures        int        `json:"failures"`
	LastObjects     int64      `json:"last_objects"`
	LastBytes       int64      `json:"last_bytes"`
	TotalObjects    int64      `json:"total_objects"`
	TotalBytes      int64      `json:"total_bytes"`
	TotalDeleted    int64      `json:"total_deleted"`
	// RemovedLocal uploads exist only in the bucket now (local_days).
	RemovedLocal   int64 `json:"removed_local"`
	RemovedBytes   int64 `json:"removed_bytes"`
	PendingDeletes int   `json:"pending_deletes"`
	// ParentPublicURL: a staging site without offload of its own serves
	// uploads missing on disk from its live site's storage (read-only).
	ParentPublicURL string `json:"parent_public_url"`
}

// offloadState is in-memory bookkeeping for the offload loop.
type offloadState struct {
	locks   sync.Map // site ID -> *sync.Mutex: one pass (or change) at a time
	cancels sync.Map // site ID -> context.CancelFunc of the running pass
	lastRun sync.Map // site ID -> time.Time the last pass started
	// sent: site ID -> map[path]fileSig of files recent incremental copies
	// uploaded, so the overlap between two copies isn't uploaded twice.
	// Only touched under the site's lock.
	sent   sync.Map
	failed sync.Map // site ID -> last error message reported as an event
	// http fetches the probe object and checks public copies (nil: default).
	http *http.Client
	// insecure allows http:// endpoints and public URLs on any host: tests
	// against a local MinIO only.
	insecure bool
}

func (s *Service) offloadLock(id string) *sync.Mutex {
	m, _ := s.offload.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (s *Service) offloadClient() *http.Client {
	if s.offload.http != nil {
		return s.offload.http
	}
	return &http.Client{Timeout: 20 * time.Second,
		// Caddy doesn't follow redirects either: the URL must serve the files.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func offloadTarget(o *store.Offload) offload.Target {
	return offload.Target{Endpoint: o.Endpoint, Region: o.Region, Bucket: o.Bucket, Prefix: o.Prefix,
		AccessKeyID: o.AccessKeyID, SecretKey: o.SecretKey, ACL: o.ACL}
}

func (s *Service) offloadWorkDir(id string) string {
	return filepath.Join(s.Cfg.DataDir, "offload", id)
}

// OffloadEnabled reports whether a site's uploads are offloaded (every
// upload is then in the bucket or on its way there). Spreading a site's
// replicas across servers requires it.
func (s *Service) OffloadEnabled(ctx context.Context, siteID string) (bool, error) {
	_, err := s.Store.GetOffload(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ---- settings ----

// safe URL path segments (the public URL's path goes into the Caddyfile).
var offloadPathRe = regexp.MustCompile(`^(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*$`)

// normalizePublicURL accepts https://host[:port][/path] with a real
// hostname (no IP addresses or internal names: Caddy proxies visitors'
// requests there) and a path of safe characters.
func (s *Service) normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	bad := fmt.Errorf("%w: public_url must look like https://media.example.com/uploads or "+
		"https://bucket.s3.amazonaws.com/site/uploads (letters, digits, '.', '_', '~' and '-' in the path; no query)", ErrInvalidInput)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		u.RawPath != "" || !offloadPathRe.MatchString(u.Path) {
		return "", bad
	}
	host := strings.ToLower(u.Hostname())
	if s.offload.insecure {
		if (u.Scheme != "https" && u.Scheme != "http") || host == "" {
			return "", bad
		}
	} else {
		if u.Scheme != "https" {
			return "", fmt.Errorf("%w: public_url must be https:// (the site's uploads are served from there)", ErrInvalidInput)
		}
		if host, err = NormalizeDomain(host); err != nil {
			return "", fmt.Errorf("%w: public_url must use a public hostname, not an IP address or internal name", ErrInvalidInput)
		}
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	return u.Scheme + "://" + host + u.Path, nil
}

// offloadConfig validates settings; cur is the stored configuration (nil
// when offload is off).
func (s *Service) offloadConfig(ctx context.Context, st *store.Site, cur *store.Offload, in OffloadInput) (*store.Offload, error) {
	o := &store.Offload{SiteID: st.ID, LocalDays: in.LocalDays,
		Endpoint: strings.TrimRight(strings.TrimSpace(in.Endpoint), "/"), Region: strings.TrimSpace(in.Region),
		Bucket: strings.TrimSpace(in.Bucket), AccessKeyID: strings.TrimSpace(in.AccessKeyID),
		SecretKey: strings.TrimSpace(in.SecretKey), ACL: strings.TrimSpace(in.ACL)}
	prefix := strings.TrimPrefix(strings.TrimSpace(in.Prefix), "/")
	if prefix == "" {
		prefix = st.ID + "/uploads/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	o.Prefix = prefix
	if o.ACL == "none" || o.ACL == "private" {
		o.ACL = ""
	}
	if o.AccessKeyID == "" && cur != nil {
		o.AccessKeyID = cur.AccessKeyID
	}
	if o.SecretKey == "" {
		// A stored key only goes to the endpoint it was given for.
		if cur == nil || !strings.EqualFold(cur.Endpoint, o.Endpoint) {
			return nil, fmt.Errorf("%w: secret_key is required", ErrInvalidInput)
		}
		o.SecretKey = cur.SecretKey
	}
	if in.LocalDays < 0 || in.LocalDays > maxLocalDays {
		return nil, fmt.Errorf("%w: local_days must be between 0 (keep local copies) and %d", ErrInvalidInput, maxLocalDays)
	}
	if err := offloadTarget(o).Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if !s.offload.insecure && !strings.HasPrefix(strings.ToLower(o.Endpoint), "https://") {
		return nil, fmt.Errorf("%w: endpoint must be https://", ErrInvalidInput)
	}
	var err error
	if o.PublicURL, err = s.normalizePublicURL(in.PublicURL); err != nil {
		return nil, err
	}
	// Each site's objects are its own: a site deletes objects under its
	// prefix (from its delete queue), so no prefix may contain another's.
	others, err := s.Store.ListOffload(ctx)
	if err != nil {
		return nil, err
	}
	host := func(e string) string { u, _ := url.Parse(e); return strings.ToLower(u.Host) }
	for _, x := range others {
		if x.SiteID == st.ID || host(x.Endpoint) != host(o.Endpoint) || x.Bucket != o.Bucket {
			continue
		}
		if strings.HasPrefix(x.Prefix, o.Prefix) || strings.HasPrefix(o.Prefix, x.Prefix) {
			return nil, fmt.Errorf("%w: site %s already offloads to %s/%s: every site needs a prefix of its own (e.g. %s)",
				ErrInvalidInput, x.SiteID, x.Bucket, x.Prefix, st.ID+"/uploads/")
		}
	}
	return o, nil
}

// SetOffload turns uploads offload on (after proving that the key can write
// and delete objects and that they are publicly readable at the public URL),
// changes it, or turns it off. The bucket's objects are never deleted.
func (s *Service) SetOffload(ctx context.Context, id string, in OffloadInput) (*OffloadStatus, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	if s.Offload == nil && in.Enabled {
		return nil, fmt.Errorf("%w: no object storage client configured", ErrInvalidInput)
	}
	// A running copy works with the old settings: stop it and wait.
	lock := s.offloadLock(id)
	for !lock.TryLock() {
		s.stopOffloadPass(id)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer lock.Unlock()
	cur, err := s.Store.GetOffload(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		cur = nil
	} else if err != nil {
		return nil, err
	}

	if !in.Enabled {
		if cur == nil {
			return s.OffloadStatus(ctx, id)
		}
		if cur.RemovedLocal > 0 && !in.Force {
			return nil, fmt.Errorf("%w: %d uploads only exist in the bucket (their local copies were removed after %d days): "+
				"bring them back first (wpgenie site offload %s download), or turn offload off anyway (force) and they are gone from the site",
				ErrConflict, cur.RemovedLocal, cur.LocalDays, id)
		}
		if err := s.Store.DeleteOffload(ctx, id); err != nil {
			return nil, err
		}
		if err := s.writeOffloadWrapper(id, false); err != nil {
			return nil, err
		}
		if err := s.Sync(ctx); err != nil {
			return nil, err
		}
		s.offload.sent.Delete(id)
		s.offload.failed.Delete(id)
		s.event(id, "offload", fmt.Sprintf("Uploads offload turned off. The objects stay in %s/%s: delete them there if they're no longer needed.",
			cur.Bucket, cur.Prefix))
		return s.OffloadStatus(ctx, id)
	}

	next, err := s.offloadConfig(ctx, st, cur, in)
	if err != nil {
		return nil, err
	}
	moved := cur == nil || cur.Endpoint != next.Endpoint || cur.Bucket != next.Bucket || cur.Prefix != next.Prefix
	if cur != nil && moved && cur.RemovedLocal > 0 {
		return nil, fmt.Errorf("%w: %d uploads only exist in %s/%s: bring them back first (wpgenie site offload %s download) "+
			"before moving to another bucket or prefix", ErrConflict, cur.RemovedLocal, cur.Bucket, cur.Prefix, id)
	}
	if moved || cur.AccessKeyID != next.AccessKeyID || cur.SecretKey != next.SecretKey || cur.PublicURL != next.PublicURL ||
		cur.ACL != next.ACL || cur.Region != next.Region {
		if err := s.offloadImageCheck(ctx, id); err != nil {
			return nil, err
		}
		if err := s.probeOffload(ctx, next); err != nil {
			return nil, err
		}
	}
	if err := s.Store.SetOffload(ctx, next, moved); err != nil {
		return nil, err
	}
	if moved {
		s.offload.sent.Delete(id)
		s.offload.failed.Delete(id)
	}
	if err := s.writeOffloadWrapper(id, true); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	switch {
	case moved:
		msg := fmt.Sprintf("Uploads offload on: copied to %s/%s at %s and served from %s when missing on disk",
			next.Bucket, next.Prefix, next.Endpoint, next.PublicURL)
		if next.LocalDays > 0 {
			msg += fmt.Sprintf("; local copies are removed %d days after upload", next.LocalDays)
		}
		s.event(id, "offload", msg)
	case cur.LocalDays != next.LocalDays && next.LocalDays == 0:
		s.event(id, "offload", "Uploads offload: local copies are kept from now on")
	case cur.LocalDays != next.LocalDays:
		s.event(id, "offload", fmt.Sprintf("Uploads offload: local copies are removed %d days after upload", next.LocalDays))
	default:
		s.event(id, "offload", "Uploads offload settings changed")
	}
	return s.OffloadStatus(ctx, id)
}

// offloadImageCheck: deletes only reach the bucket with offload.php in the
// site's PHP image.
func (s *Service) offloadImageCheck(ctx context.Context, id string) error {
	err := s.Runtime.Exec(ctx, id, nil, nil, "test", "-f", offloadScript)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("%w: the site's PHP containers predate uploads offload (uploads deleted in WordPress would stay in the bucket); "+
			"roll them onto the current image first (wpgenie site scale %s)", ErrConflict, id)
	}
	return err
}

// probeOffload writes a test object, fetches it through the public URL and
// deletes it: the three things offload needs.
func (s *Service) probeOffload(ctx context.Context, o *store.Offload) error {
	t := offloadTarget(o)
	name := "wpgenie-probe-" + randString(12, lowerAlnum) + ".txt"
	body := []byte("WPGenie uploads offload check " + randString(24, lowerAlnum) + "\n")
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := s.Offload.Put(c, t, name, body); err != nil {
		return offloadInputErr(c, fmt.Sprintf("writing a test object to %s/%s failed (check the endpoint, region, bucket and "+
			"that the key may write objects there)", o.Bucket, o.Prefix), err)
	}
	fetchErr := s.fetchProbe(c, o, name, body)
	// Deleted whatever the fetch said, on a context of its own.
	dc, dcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer dcancel()
	delErr := s.Offload.DeleteFile(dc, t, name)
	if fetchErr != nil {
		return fetchErr
	}
	if delErr != nil {
		return offloadInputErr(dc, "deleting the test object failed (the key must be allowed to delete objects: "+
			"uploads deleted in WordPress are deleted from the bucket)", delErr)
	}
	return nil
}

func offloadInputErr(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrInvalidInput, what, err)
}

func (s *Service) fetchProbe(ctx context.Context, o *store.Offload, name string, want []byte) error {
	u := o.PublicURL + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	resp, err := s.offloadClient().Do(req)
	if err != nil {
		return fmt.Errorf("%w: the test object couldn't be fetched from %s: %v", ErrInvalidInput, u, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("%w: %s redirects (HTTP %d to %s): public_url must serve the files itself", ErrInvalidInput, u,
			resp.StatusCode, resp.Header.Get("Location"))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: the test object isn't publicly readable at %s (HTTP %d): make the bucket or this prefix publicly "+
			"readable (a bucket policy allowing s3:GetObject, or acl public-read), or correct public_url", ErrInvalidInput, u, resp.StatusCode)
	case !bytes.Equal(got, want):
		return fmt.Errorf("%w: %s doesn't serve the test object written to %s/%s%s: public_url must be the public address of "+
			"that bucket and prefix", ErrInvalidInput, u, o.Bucket, o.Prefix, name)
	}
	return nil
}

const offloadWrapper = `<?php
/**
 * Plugin Name: WPGenie Uploads Offload
 * Description: Tells WPGenie which uploads WordPress deleted, so their copies in object storage go too. Managed by WPGenie: change it in the WPGenie panel; this file is rewritten on changes.
 */
define( 'WPGENIE_OFFLOAD_UPLOADS', '%s' );
define( 'WPGENIE_OFFLOAD_QUEUE', '%s' );
define( 'WPGENIE_OFFLOAD_POKE', '%s' );
if ( is_file( '/usr/local/share/wpgenie/offload.php' ) ) {
	require_once '/usr/local/share/wpgenie/offload.php';
}
`

// writeOffloadWrapper writes (on) or removes the mu-plugin wrapper. The
// paths are the same in the site's containers (mounted at the same place).
func (s *Service) writeOffloadWrapper(id string, on bool) error {
	paths := []any{filepath.Join(s.Cfg.SiteRoot(id), offloadUploadsDir), filepath.Join(s.Cfg.SiteDir(id), offloadQueuePath),
		filepath.Join(s.Cfg.SiteDir(id), offloadPokePath)}
	for _, p := range paths {
		if strings.ContainsAny(p.(string), `'\`+"\n") {
			return fmt.Errorf("unsafe site path %q", p)
		}
	}
	if on {
		if err := ensureLogDir(s.Cfg.SiteDir(id)); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureManaged(root, offloadWrapperPath, fmt.Sprintf(offloadWrapper, paths...), on)
}

// OffloadStatus reports a site's offload (never the secret key).
func (s *Service) OffloadStatus(ctx context.Context, id string) (*OffloadStatus, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &OffloadStatus{}
	o, err := s.Store.GetOffload(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		if st.ParentID != "" {
			if p, err := s.Store.GetOffload(ctx, st.ParentID); err == nil {
				out.ParentPublicURL = p.PublicURL
			}
		}
		return out, nil
	} else if err != nil {
		return nil, err
	}
	at := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}
	out.Enabled = true
	out.Endpoint, out.Region, out.Bucket, out.Prefix = o.Endpoint, o.Region, o.Bucket, o.Prefix
	out.AccessKeyID, out.SecretSet = maskKeyID(o.AccessKeyID), o.SecretKey != ""
	out.PublicURL, out.ACL, out.LocalDays = o.PublicURL, o.ACL, o.LocalDays
	out.LastIncremental, out.LastFull, out.LastAttempt, out.LastCleanup = at(o.IncrementalAt), at(o.FullAt), at(o.AttemptAt), at(o.CleanedAt)
	out.LastError, out.Failures = o.LastError, o.Failures
	out.LastObjects, out.LastBytes, out.TotalObjects, out.TotalBytes = o.LastObjects, o.LastBytes, o.TotalObjects, o.TotalBytes
	out.TotalDeleted, out.RemovedLocal, out.RemovedBytes = o.TotalDeleted, o.RemovedLocal, o.RemovedBytes
	_, out.Syncing = s.offload.cancels.Load(id)
	if out.PendingDeletes, err = s.Store.CountOffloadDeletes(ctx, id); err != nil {
		return nil, err
	}
	return out, nil
}

// maskKeyID shows enough of an access key ID to recognise it.
func maskKeyID(k string) string {
	if len(k) <= 8 {
		return k[:min(2, len(k))] + "…"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// offloadURLs is, per site, the public URL Caddy fetches missing uploads
// from: the site's own, or for a staging site without one, its live site's.
func (s *Service) offloadURLs(ctx context.Context, sites []*store.Site) (map[string]string, error) {
	rows, err := s.Store.ListOffload(ctx)
	if err != nil {
		return nil, err
	}
	own := map[string]string{}
	for _, o := range rows {
		own[o.SiteID] = o.PublicURL
	}
	out := map[string]string{}
	for _, st := range sites {
		if u := own[st.ID]; u != "" {
			out[st.ID] = u
		} else if st.ParentID != "" && own[st.ParentID] != "" {
			out[st.ID] = own[st.ParentID]
		}
	}
	return out, nil
}

// ---- what gets offloaded ----

// offloadPrivateDirs are directories directly under uploads where plugins
// keep files that aren't public (paid downloads, form entries, logs,
// backups), protected by .htaccess rules a public bucket doesn't have.
// They are never offloaded and stay on disk only.
var offloadPrivateDirs = []string{
	"woocommerce_uploads", "wc-logs", "edd", "gravity_forms", "wpforms", "wpcf7_uploads", "wp-staging",
	"updraft", "backwpup-*", "backupwordpress-*", "backup-guard", "backups", "wpvividbackups", "ai1wm-backups",
	"wp-migrate-db", "pb_backupbuddy", "backupbuddy_backups", "wp-security-audit-log", "ithemes-security",
	"solid-security", "sucuri", "wflogs",
}

// offloadSkipRe: files never offloaded: PHP anywhere in the name (Caddy
// never proxies such a path), what Caddy refuses to serve (dumps, logs,
// backups), server configuration, partial files, and the AVIF/WebP copies
// of images (negotiated from disk only; the original is what's offloaded).
var offloadSkipRe = regexp.MustCompile(`(?i)(\.ph(p|tml|ar|t|ps)|\.(sql|sql\.gz|bak|old|orig|swp|log|ini|htaccess|htpasswd|tmp|part)$|` +
	`\.(jpe?g|png)\.(avif|webp)$)`)

// offloadWanted says whether a path under uploads (slash-separated,
// relative) may be offloaded.
func offloadWanted(p string) bool {
	if p == "" || len(p) > 900 || !utf8.ValidString(p) || strings.IndexFunc(p, unicode.IsControl) >= 0 ||
		strings.ContainsRune(p, '\\') || offloadSkipRe.MatchString(p) {
		return false
	}
	for i, seg := range strings.Split(p, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || (i == 0 && offloadPrivateDir(seg) && strings.Contains(p, "/")) {
			return false
		}
	}
	return true
}

func offloadPrivateDir(name string) bool {
	name = strings.ToLower(name)
	for _, pat := range offloadPrivateDirs {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// cleanQueuePath turns a line of the delete queue (written by the site's
// PHP: untrusted) into an object path under the prefix, or "".
func cleanQueuePath(line string) string {
	p := strings.TrimRight(line, "\r")
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || !offloadWanted(p) {
		return ""
	}
	return p
}

// localFile is a file under uploads.
type localFile struct {
	Path    string
	Size    int64
	ModTime time.Time
}

type fileSig struct {
	size int64
	mod  int64
}

func (f localFile) sig() fileSig { return fileSig{f.Size, f.ModTime.UnixNano()} }

// openUploads opens the site's uploads directory. wp-content and uploads
// belong to the site, which could swap either for a symlink at any moment:
// os.Root keeps everything inside the docroot, and the Lstat/SameFile check
// makes sure what was opened is the real uploads directory, not one a
// symlink led to.
func (s *Service) openUploads(id string) (*os.Root, error) {
	doc, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	var lfi fs.FileInfo
	for _, p := range []string{"wp-content", offloadUploadsDir} {
		if lfi, err = doc.Lstat(p); err != nil {
			return nil, err
		}
		if !lfi.IsDir() {
			return nil, fmt.Errorf("%s is not a directory (a symlink?): not offloading it", p)
		}
	}
	up, err := doc.OpenRoot(offloadUploadsDir)
	if err != nil {
		return nil, err
	}
	fi, err := up.Stat(".")
	if err != nil || !os.SameFile(lfi, fi) {
		up.Close()
		return nil, errors.New(offloadUploadsDir + " changed while being opened")
	}
	return up, nil
}

// walkUploads calls fn for every regular file that may be offloaded. It
// never follows a symlink, and never leaves the uploads directory.
func walkUploads(ctx context.Context, up *os.Root, fn func(localFile)) error {
	return fs.WalkDir(up.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return nil // an unreadable directory: what can be read is copied
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != "." && (strings.HasPrefix(d.Name(), ".") || (!strings.Contains(p, "/") && offloadPrivateDir(p))) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !offloadWanted(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // gone meanwhile
		}
		fn(localFile{Path: p, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
}

// openRegular opens a file under uploads for reading: never through a
// symlink, never blocking (a named pipe), only if it's (still) a regular
// file.
func openRegular(up *os.Root, p string) (*os.File, fs.FileInfo, error) {
	f, err := up.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", p)
	}
	return f, fi, nil
}

// stageFile copies an upload into dir (the daemon's own), keeping its mtime
// (rclone stores it with the object). ok is false when the file is gone or
// no longer a regular file.
func stageFile(up *os.Root, p, dir string) (localFile, bool, error) {
	src, fi, err := openRegular(up, p)
	if err != nil {
		return localFile{}, false, nil
	}
	defer src.Close()
	dst := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return localFile{}, false, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return localFile{}, false, err
	}
	// What it was when opened: if it grows meanwhile, its new mtime brings
	// it into the next copy.
	_, err = io.Copy(out, io.LimitReader(src, fi.Size()))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return localFile{}, false, fmt.Errorf("copying %s: %w", p, err)
	}
	if err := os.Chtimes(dst, fi.ModTime(), fi.ModTime()); err != nil {
		return localFile{}, false, err
	}
	return localFile{Path: p, Size: fi.Size(), ModTime: fi.ModTime()}, true, nil
}

// ---- the loop ----

// RunOffload copies offloaded sites' uploads to their buckets until ctx
// ends: new and changed files every minute (within seconds of an upload),
// everything the bucket lacks nightly.
func (s *Service) RunOffload(ctx context.Context) {
	if s.Offload == nil {
		return
	}
	t := time.NewTicker(offloadTick)
	defer t.Stop()
	slots := offloadSlots{inc: make(chan struct{}, offloadParallel), full: make(chan struct{}, offloadParallelFull)}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		s.offloadTick(ctx, time.Now(), slots, &wg)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type offloadSlots struct{ inc, full chan struct{} }

func (s *Service) offloadTick(ctx context.Context, now time.Time, slots offloadSlots, wg *sync.WaitGroup) {
	rows, err := s.Store.ListOffload(ctx)
	if err != nil {
		s.Log.Warn("offload: listing sites", "err", err)
		return
	}
	window := s.inMaintenanceWindow(now)
	for _, o := range rows {
		if ctx.Err() != nil {
			return
		}
		last, _ := s.offload.lastRun.Load(o.SiteID)
		lastRun, _ := last.(time.Time)
		run, full := offloadDue(o, now, lastRun, s.offloadPoked(o.SiteID), window)
		if !run {
			continue
		}
		st, err := s.Store.GetSite(ctx, o.SiteID)
		if err != nil || st.Status != store.StatusActive {
			continue
		}
		sem := slots.inc
		if full {
			sem = slots.full
		}
		select {
		case sem <- struct{}{}:
		default:
			continue // every slot busy: the next tick
		}
		lock := s.offloadLock(o.SiteID)
		if !lock.TryLock() {
			<-sem
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer lock.Unlock()
			s.offloadPass(ctx, st, full)
		}()
	}
}

// offloadBackoff is how long to wait after n failures in a row.
func offloadBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return min(time.Minute<<min(n-1, 10), offloadMaxBackoff)
}

// offloadDue decides whether a site's uploads are copied now, and whether
// fully (compared with the bucket's listing) or only what changed.
func offloadDue(o *store.Offload, now, lastRun, poked time.Time, window bool) (run, full bool) {
	if o.Failures > 0 && now.Sub(o.AttemptAt) < offloadBackoff(o.Failures) {
		return false, false
	}
	if o.FullAt.IsZero() || o.IncrementalAt.IsZero() || (window && now.Sub(o.FullAt) > offloadFullEvery) ||
		now.Sub(o.FullAt) > offloadFullMax {
		return true, true
	}
	since := now.Sub(lastRun)
	return since >= offloadEvery || (poked.After(lastRun) && since >= offloadPokeMin), false
}

// offloadPoked is when PHP last noted an upload or a delete.
func (s *Service) offloadPoked(id string) time.Time {
	root, err := os.OpenRoot(s.Cfg.SiteDir(id))
	if err != nil {
		return time.Time{}
	}
	defer root.Close()
	var t time.Time
	for _, p := range []string{offloadPokePath, offloadQueuePath} {
		if fi, err := root.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	return t
}

// stopOffloadPass cancels a site's running copy, if any.
func (s *Service) stopOffloadPass(id string) {
	if c, ok := s.offload.cancels.Load(id); ok {
		c.(context.CancelFunc)()
	}
}

// offloadCleanupResult is what removing local copies did.
type offloadCleanupResult struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// OffloadRunResult is a sync's outcome, as a job reports it.
type OffloadRunResult struct {
	Full         bool  `json:"full"`
	Objects      int64 `json:"objects"`
	Bytes        int64 `json:"bytes"`
	Deleted      int64 `json:"deleted"`
	RemovedLocal int64 `json:"removed_local"`
}

// offloadPass runs one copy of a site's uploads and records it. Called with
// the site's offload lock held.
func (s *Service) offloadPass(ctx context.Context, st *store.Site, full bool) (*OffloadRunResult, error) {
	o, err := s.Store.GetOffload(ctx, st.ID)
	if err != nil {
		return nil, err // turned off meanwhile
	}
	started := time.Now()
	s.offload.lastRun.Store(st.ID, started)
	timeout := offloadIncTimeout
	if full {
		timeout = offloadFullTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	s.offload.cancels.Store(st.ID, cancel)
	defer func() {
		s.offload.cancels.Delete(st.ID)
		cancel()
	}()
	run, clean, cleanErr, err := s.syncOffload(pctx, st, o, full, started)
	run.Err = err
	rc, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer rcancel()
	if rerr := s.Store.RecordOffloadRun(rc, st.ID, run); rerr != nil {
		s.Log.Warn("offload: recording a sync", "site", st.ID, "err", rerr)
	}
	if clean.Files > 0 {
		if rerr := s.Store.RecordOffloadCleanup(rc, st.ID, time.Now(), clean.Files, clean.Bytes); rerr != nil {
			s.Log.Warn("offload: recording a cleanup", "site", st.ID, "err", rerr)
		}
		s.event(st.ID, "offload", fmt.Sprintf("Removed %d local copies of uploads (%s) older than %d days: the bucket holds them, "+
			"and they're served from %s", clean.Files, fmtMB(clean.Bytes), o.LocalDays, o.PublicURL))
	}
	if cleanErr != nil && ctx.Err() == nil {
		s.Log.Warn("offload: removing local copies", "site", st.ID, "err", cleanErr)
		s.event(st.ID, "offload", "Local copies of uploads were kept: "+cleanErr.Error())
	}
	msg := ""
	if err != nil {
		msg = offload.CleanError(err.Error())
	}
	if prev, _ := s.offload.failed.Swap(st.ID, msg); prev != msg && ctx.Err() == nil {
		if err != nil {
			s.Log.Warn("offload: sync failed", "site", st.ID, "err", err)
			s.event(st.ID, "offload", "Copying uploads to the bucket failed (retried with backoff): "+msg)
		} else if prev != nil && prev != "" {
			s.event(st.ID, "offload", "Copying uploads to the bucket works again")
		}
	}
	if err == nil && full && run.Objects > 0 {
		s.event(st.ID, "offload", fmt.Sprintf("Uploads offload: %d files (%s) copied to the bucket", run.Objects, fmtMB(run.Bytes)))
	}
	return &OffloadRunResult{Full: full, Objects: run.Objects, Bytes: run.Bytes, Deleted: run.Deleted, RemovedLocal: clean.Files}, err
}

// syncOffload: deletes first (so an upload deleted and uploaded again under
// the same name isn't deleted after its new copy went up), then uploads,
// then, after a full copy, local copies older than local_days.
func (s *Service) syncOffload(ctx context.Context, st *store.Site, o *store.Offload, full bool, started time.Time) (
	run store.OffloadRun, clean offloadCleanupResult, cleanErr, err error) {
	run = store.OffloadRun{Full: full, Started: started}
	t := offloadTarget(o)
	var errs []error
	if run.Deleted, err = s.offloadDeletes(ctx, st.ID, t); err != nil {
		errs = append(errs, fmt.Errorf("deleting objects: %w", err))
	}
	var remote map[string]offload.Object
	since := o.IncrementalAt.Add(-offloadMargin)
	if full {
		if remote, err = s.Offload.List(ctx, t); err != nil {
			return run, clean, nil, errors.Join(append(errs, fmt.Errorf("listing the bucket: %w", err))...)
		}
	}
	stats, err := s.offloadUpload(ctx, st.ID, t, full, since, remote)
	run.Objects, run.Bytes = stats.Objects, stats.Bytes
	if err != nil {
		errs = append(errs, err)
	}
	if full && len(errs) == 0 && o.LocalDays > 0 {
		clean, cleanErr = s.offloadCleanup(ctx, st.ID, o, remote, started)
	}
	return run, clean, cleanErr, errors.Join(errs...)
}

// offloadUpload uploads what changed since since (incremental), or what the
// bucket lacks or holds an older copy of (full, remote is its listing).
func (s *Service) offloadUpload(ctx context.Context, id string, t offload.Target, full bool, since time.Time,
	remote map[string]offload.Object) (offload.Stats, error) {
	up, err := s.openUploads(id)
	if errors.Is(err, fs.ErrNotExist) {
		return offload.Stats{}, nil // nothing uploaded yet
	} else if err != nil {
		return offload.Stats{}, err
	}
	defer up.Close()
	sent := s.offloadSent(id, since)
	var files []localFile
	err = walkUploads(ctx, up, func(f localFile) {
		if full {
			// Uploaded after its last change: current (upload time is the
			// server's clock, like --update --use-server-modtime).
			if r, ok := remote[f.Path]; ok && r.Size == f.Size && !f.ModTime.After(r.Modified) {
				return
			}
		} else if f.ModTime.Before(since) || sent[f.Path] == f.sig() {
			return
		}
		files = append(files, f)
	})
	if err != nil {
		return offload.Stats{}, fmt.Errorf("reading the uploads: %w", err)
	}
	return s.uploadBatches(ctx, id, up, t, files, sent)
}

// offloadSent is the site's map of recently uploaded files, without those
// older than since (no incremental copy looks at them any more).
func (s *Service) offloadSent(id string, since time.Time) map[string]fileSig {
	v, _ := s.offload.sent.LoadOrStore(id, map[string]fileSig{})
	m := v.(map[string]fileSig)
	for p, sig := range m {
		if sig.mod < since.UnixNano() {
			delete(m, p)
		}
	}
	return m
}

func (s *Service) uploadBatches(ctx context.Context, id string, up *os.Root, t offload.Target, files []localFile,
	sent map[string]fileSig) (offload.Stats, error) {
	var total offload.Stats
	if len(files) == 0 {
		return total, nil
	}
	work := s.offloadWorkDir(id)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return total, err
	}
	for len(files) > 0 {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		dir, err := os.MkdirTemp(work, "stage-")
		if err != nil {
			return total, err
		}
		var batch []localFile
		var size int64
		for len(files) > 0 && len(batch) < offloadBatchFiles && (len(batch) == 0 || size+files[0].Size <= offloadBatchBytes) {
			f, ok, err := stageFile(up, files[0].Path, dir)
			files = files[1:]
			if err != nil {
				os.RemoveAll(dir)
				return total, fmt.Errorf("staging uploads: %w", err)
			}
			if ok {
				batch, size = append(batch, f), size+f.Size
			}
		}
		if len(batch) > 0 {
			st, err := s.Offload.Upload(ctx, t, dir)
			total.Objects, total.Bytes = total.Objects+st.Objects, total.Bytes+st.Bytes
			if err != nil {
				os.RemoveAll(dir)
				return total, fmt.Errorf("uploading: %w", err)
			}
			for _, f := range batch {
				sent[f.Path] = f.sig()
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return total, err
		}
	}
	return total, nil
}

// ---- deletes ----

// offloadDeletes moves the site's delete queue into the store, then deletes
// the queued objects from the bucket (unless the file is back on disk under
// the same name: then it's uploaded again instead).
func (s *Service) offloadDeletes(ctx context.Context, id string, t offload.Target) (int64, error) {
	paths, dropped, err := s.readOffloadQueue(id)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.Log.Warn("offload: reading the delete queue", "site", id, "err", err)
	}
	if dropped {
		s.event(id, "offload", fmt.Sprintf("The list of deleted uploads was over %d MB: the rest was dropped, so some deleted uploads may "+
			"remain in the bucket", offloadQueueMax>>20))
	}
	if len(paths) > 0 {
		refused, err := s.Store.QueueOffloadDeletes(ctx, id, paths, offloadPendingMax)
		if err != nil {
			return 0, err
		}
		if refused > 0 {
			s.event(id, "offload", fmt.Sprintf("%d deleted uploads weren't queued for deletion from the bucket: over %d are waiting",
				refused, offloadPendingMax))
		}
	}
	up, err := s.openUploads(id)
	if err == nil {
		defer up.Close()
	}
	var total int64
	for {
		pending, err := s.Store.PendingOffloadDeletes(ctx, id, offloadDeleteBatch)
		if err != nil || len(pending) == 0 {
			return total, err
		}
		del := make([]string, 0, len(pending))
		for _, p := range pending {
			if up != nil {
				if fi, err := up.Lstat(p); err == nil && fi.Mode().IsRegular() {
					continue
				}
			}
			del = append(del, p)
		}
		st, err := s.Offload.Delete(ctx, t, del)
		if err != nil {
			return total, err
		}
		total += st.Deletes
		if err := s.Store.DoneOffloadDeletes(ctx, id, pending); err != nil {
			return total, err
		}
		if len(pending) < offloadDeleteBatch {
			return total, nil
		}
	}
}

// readOffloadQueue reads and empties the delete queue PHP appends to. The
// file is the site's: a symlink must not make the daemon read or truncate
// anything else, and a named pipe must not block it (see readPHPLog). Every
// line is untrusted and normalised by cleanQueuePath.
func (s *Service) readOffloadQueue(id string) (paths []string, dropped bool, err error) {
	root, err := os.OpenRoot(s.Cfg.SiteDir(id))
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	lfi, err := root.Lstat(offloadQueuePath)
	if err != nil {
		return nil, false, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, false, errors.New(offloadQueuePath + " is not a regular file")
	}
	if lfi.Size() == 0 {
		return nil, false, nil
	}
	f, err := root.OpenFile(offloadQueuePath, os.O_RDWR|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(lfi, fi) {
		return nil, false, errors.New(offloadQueuePath + " changed while being opened")
	}
	// PHP appends under an exclusive flock: holding it from reading to
	// truncating loses no entry in between. A site holding the lock only
	// delays its own deletes.
	fd := int(f.Fd())
	locked := false
	for range 20 {
		if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			locked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !locked {
		return nil, false, nil // next pass
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	data, err := io.ReadAll(io.LimitReader(f, offloadQueueMax+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > offloadQueueMax {
		dropped = true
		data = data[:bytes.LastIndexByte(data[:offloadQueueMax], '\n')+1]
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if p := cleanQueuePath(line); p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths, dropped, f.Truncate(0)
}

// ---- removing local copies (local_days) ----

// offloadCleanup removes local copies of uploads older than local_days that
// the bucket is confirmed to hold: same size, an MD5 from the listing equal
// to the local file's, uploaded after the file last changed, and the public
// URL serving them. When in doubt the file stays. fullStart is when the
// full copy that produced remote started: nothing newer is touched.
func (s *Service) offloadCleanup(ctx context.Context, id string, o *store.Offload, remote map[string]offload.Object,
	fullStart time.Time) (offloadCleanupResult, error) {
	var res offloadCleanupResult
	cutoff := time.Now().Add(-time.Duration(o.LocalDays) * 24 * time.Hour)
	if cutoff.After(fullStart) {
		cutoff = fullStart
	}
	up, err := s.openUploads(id)
	if errors.Is(err, fs.ErrNotExist) {
		return res, nil
	} else if err != nil {
		return res, err
	}
	defer up.Close()
	var cands []localFile
	err = walkUploads(ctx, up, func(f localFile) {
		r, ok := remote[f.Path]
		if f.ModTime.Before(cutoff) && ok && r.Size == f.Size && r.MD5 != "" && !f.ModTime.After(r.Modified) {
			cands = append(cands, f)
		}
	})
	if err != nil || len(cands) == 0 {
		return res, err
	}
	// Visitors will get them from the public URL: it must answer.
	if err := s.checkPublicCopy(ctx, o.PublicURL, cands[0]); err != nil {
		return res, err
	}
	for _, f := range cands {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if removeIfSame(up, f, remote[f.Path].MD5) {
			res.Files++
			res.Bytes += f.Size
		}
	}
	return res, nil
}

// checkPublicCopy asks the public URL for one file about to lose its local
// copy.
func (s *Service) checkPublicCopy(ctx context.Context, base string, f localFile) error {
	segs := strings.Split(f.Path, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	u := base + "/" + strings.Join(segs, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.offloadClient().Do(req)
	if err != nil {
		return fmt.Errorf("the public URL doesn't answer (%v)", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || (resp.ContentLength >= 0 && resp.ContentLength != f.Size) {
		return fmt.Errorf("the public URL doesn't serve %s (HTTP %d, %d bytes; %d on disk)", u, resp.StatusCode, resp.ContentLength, f.Size)
	}
	return nil
}

// removeIfSame removes an upload if it is still the file that was listed
// and its content matches the bucket's MD5 (an integrity check against the
// listing's ETag, not a security boundary).
func removeIfSame(up *os.Root, f localFile, wantMD5 string) bool {
	src, fi, err := openRegular(up, f.Path)
	if err != nil {
		return false
	}
	defer src.Close()
	if fi.Size() != f.Size || !fi.ModTime().Equal(f.ModTime) {
		return false
	}
	h := md5.New()
	if _, err := io.Copy(h, io.LimitReader(src, f.Size+1)); err != nil || hex.EncodeToString(h.Sum(nil)) != wantMD5 {
		return false
	}
	lfi, err := up.Lstat(f.Path)
	if err != nil || !os.SameFile(lfi, fi) || lfi.Size() != f.Size || !lfi.ModTime().Equal(f.ModTime) {
		return false
	}
	return up.Remove(f.Path) == nil
}

// ---- jobs: sync now, bring local copies back ----

// StartOffloadSync runs a full copy now, as a job.
func (s *Service) StartOffloadSync(ctx context.Context, id string) (int64, error) {
	st, err := s.offloadSite(ctx, id)
	if err != nil {
		return 0, err
	}
	spec := jobs.Spec{SiteID: id, Kind: "offload-sync", Timeout: offloadFullTimeout, Lock: jobs.LockFunc(s.offloadLock(id))}
	return s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) error {
		t.Progress(5, "Comparing the uploads with the bucket")
		res, err := s.offloadPass(ctx, st, true)
		if res != nil {
			t.SetResult(res)
		}
		return err
	})
}

func (s *Service) offloadSite(ctx context.Context, id string) (*store.Site, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	if _, err := s.Store.GetOffload(ctx, id); errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: uploads offload is off for this site", ErrInvalidInput)
	} else if err != nil {
		return nil, err
	}
	if s.Offload == nil {
		return nil, fmt.Errorf("%w: no object storage client configured", ErrInvalidInput)
	}
	return st, nil
}

// StartOffloadDownload copies every offloaded upload missing on disk back
// from the bucket (after local copies were removed, before turning offload
// off or moving it), as a job.
func (s *Service) StartOffloadDownload(ctx context.Context, id string) (int64, error) {
	if _, err := s.offloadSite(ctx, id); err != nil {
		return 0, err
	}
	spec := jobs.Spec{SiteID: id, Kind: "offload-download", Heavy: true, Timeout: 12 * time.Hour,
		Lock: jobs.LockFunc(s.offloadLock(id))}
	return s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) error {
		ctx, cancel := context.WithCancel(ctx)
		s.offload.cancels.Store(id, cancel)
		defer func() {
			s.offload.cancels.Delete(id)
			cancel()
		}()
		res, err := s.offloadDownload(ctx, id, t.Progress)
		if res != nil {
			t.SetResult(res)
		}
		return err
	})
}

func (s *Service) offloadDownload(ctx context.Context, id string, report Progress) (*offloadCleanupResult, error) {
	o, err := s.Store.GetOffload(ctx, id)
	if err != nil {
		return nil, err
	}
	report(2, "Listing the bucket")
	tg := offloadTarget(o)
	remote, err := s.Offload.List(ctx, tg)
	if err != nil {
		return nil, fmt.Errorf("listing the bucket: %w", err)
	}
	if err := s.ensureUploadsDir(id); err != nil {
		return nil, err
	}
	up, err := s.openUploads(id)
	if err != nil {
		return nil, err
	}
	defer up.Close()
	local := map[string]bool{}
	if err := walkUploads(ctx, up, func(f localFile) { local[f.Path] = true }); err != nil {
		return nil, err
	}
	var missing []string
	for p := range remote {
		if !local[p] && cleanQueuePath(p) != "" && !strings.HasPrefix(p, "wpgenie-probe-") {
			missing = append(missing, p)
		}
	}
	slices.Sort(missing)
	work := s.offloadWorkDir(id)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	res := &offloadCleanupResult{}
	for done := 0; done < len(missing); {
		var batch []string
		var size int64
		for done+len(batch) < len(missing) && len(batch) < 500 && (len(batch) == 0 || size+remote[missing[done+len(batch)]].Size <= offloadBatchBytes) {
			p := missing[done+len(batch)]
			batch, size = append(batch, p), size+remote[p].Size
		}
		report(5+90*done/len(missing), fmt.Sprintf("Downloading uploads: %d of %d", done, len(missing)))
		dir, err := os.MkdirTemp(work, "fetch-")
		if err != nil {
			return res, err
		}
		if _, err := s.Offload.Download(ctx, tg, batch, dir); err != nil {
			os.RemoveAll(dir)
			return res, fmt.Errorf("downloading: %w", err)
		}
		for _, p := range batch {
			if n, ok := restoreLocal(up, dir, p); ok {
				res.Files++
				res.Bytes += n
			}
		}
		os.RemoveAll(dir)
		done += len(batch)
	}
	if err := s.Store.ResetOffloadRemoved(ctx, id); err != nil {
		return res, err
	}
	s.event(id, "offload", fmt.Sprintf("Uploads offload: %d uploads (%s) copied back from the bucket to the site", res.Files, fmtMB(res.Bytes)))
	return res, nil
}

// ensureUploadsDir creates wp-content/uploads (owned by the site) if the
// site never had one.
func (s *Service) ensureUploadsDir(id string) error {
	doc, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer doc.Close()
	for _, p := range []string{"wp-content", offloadUploadsDir} {
		if _, err := doc.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			if err := doc.Mkdir(p, 0o755); err != nil {
				return err
			}
			if err := chownToSite(doc, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreLocal moves a downloaded file from dir (the daemon's) into the
// site's uploads, owned by the site, never over an existing file or
// through a symlink.
func restoreLocal(up *os.Root, dir, p string) (int64, bool) {
	src, err := os.Open(filepath.Join(dir, filepath.FromSlash(p)))
	if err != nil {
		return 0, false
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	cur := ""
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "." {
			break
		}
		cur = path.Join(cur, seg)
		lfi, err := up.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if up.Mkdir(cur, 0o755) != nil || chownToSite(up, cur) != nil {
				return 0, false
			}
		case err != nil || !lfi.IsDir():
			return 0, false
		}
	}
	out, err := up.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return 0, false
	}
	n, err := io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		up.Remove(p)
		return 0, false
	}
	chownToSite(up, p)
	up.Chtimes(p, fi.ModTime(), fi.ModTime())
	return n, true
}
