package site

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/parthh37/wpgenie/internal/backup"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// Backups: restic snapshots of a site's WordPress install and database,
// in a repository shared by all sites that use it (WordPress core,
// themes and plugins deduplicate across sites). One snapshot is one
// backup: /backup/files (the docroot minus caches) and /backup/db
// (database.sql and meta.json). Tags: wpgenie, site=<id>, domain=<primary>
// and the kind: scheduled (the site's retention policy applies), manual
// (kept until deleted) or safety (taken before a restore or a staging push,
// kept 7 days).

// BackupEngine is restic (backup.Restic).
type BackupEngine interface {
	Init(ctx context.Context, repo *store.BackupRepo) error
	Backup(ctx context.Context, repo *store.BackupRepo, in backup.BackupInput) (string, error)
	Snapshots(ctx context.Context, repo *store.BackupRepo, tags ...string) ([]backup.Snapshot, error)
	Dump(ctx context.Context, repo *store.BackupRepo, id, path string, w io.Writer) error
	Apply(ctx context.Context, repo *store.BackupRepo, tags []string, k backup.Keep) error
	Forget(ctx context.Context, repo *store.BackupRepo, ids []string) error
	Prune(ctx context.Context, repo *store.BackupRepo) error
	Check(ctx context.Context, repo *store.BackupRepo) error
	KeyScan(ctx context.Context, host string, port int) (string, error)
}

// Backup kinds (snapshot tags).
const (
	BackupScheduled = "scheduled"
	BackupManual    = "manual"
	BackupSafety    = "safety"
)

// LocalRepoID is the repository every server has: a directory on this
// server. Good against a broken site or a bad update; an off-server
// repository is what survives losing the server.
const LocalRepoID = "local"

var errNoBackups = fmt.Errorf("%w: backups are not available (restic is not configured)", ErrInvalidInput)

// BackupMeta is stored with every backup (/backup/db/meta.json).
type BackupMeta struct {
	SiteID      string    `json:"site_id"`
	Domain      string    `json:"domain"`
	Name        string    `json:"name"`
	TablePrefix string    `json:"table_prefix"`
	Tables      []string  `json:"tables"`
	PHPVersion  string    `json:"php_version"`
	WPGenie     string    `json:"wpgenie"`
	Created     time.Time `json:"created"`
}

// BackupInfo is a backup as the panel shows it.
type BackupInfo struct {
	ID      string    `json:"id"`
	ShortID string    `json:"short_id"`
	RepoID  string    `json:"repo_id"`
	SiteID  string    `json:"site_id"`
	Domain  string    `json:"domain"`
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Size    int64     `json:"size"`  // bytes backed up (before dedup and compression)
	Added   int64     `json:"added"` // new data this backup stored (compressed)
	Files   int       `json:"files"`
}

func infoFrom(repoID string, s backup.Snapshot) BackupInfo {
	b := BackupInfo{ID: s.ID, ShortID: s.ShortID, RepoID: repoID, SiteID: s.Tag("site"), Domain: s.Tag("domain"), Time: s.Time}
	for _, k := range []string{BackupScheduled, BackupManual, BackupSafety} {
		if s.HasTag(k) {
			b.Kind = k
		}
	}
	if s.Summary != nil {
		b.Size, b.Added, b.Files = s.Summary.BytesTotal, s.Summary.DataAddedPacks, s.Summary.FilesTotal
	}
	return b
}

func (s *Service) backupsDir() string { return filepath.Join(s.Cfg.DataDir, "backups") }

// ---- Repositories ----

// RepoInput adds a repository. Kind selects which fields apply.
type RepoInput struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // local | s3 | b2 | sftp
	// local: an absolute directory on this server. sftp: the directory on
	// the server (absolute, or relative to the user's home).
	Path string `json:"path"`
	// s3: endpoint (default AWS), bucket and optional prefix; b2: bucket
	// and prefix.
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	Region   string `json:"region"`
	KeyID    string `json:"key_id"`
	Secret   string `json:"secret"`
	// sftp
	User string `json:"user"`
	Host string `json:"host"`
	Port int    `json:"port"`
	// Password opens an existing repository (e.g. this server's backups on
	// a new server); empty generates one for a new repository.
	Password string `json:"password"`
}

var (
	repoNameRe = regexp.MustCompile(`^[^\x00-\x1f]{1,60}$`)
	bucketRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	b2BucketRe = regexp.MustCompile(`^[A-Za-z0-9-]{6,50}$`)
	prefixPath = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,200}$`)
	endpointRe = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]{1,5})?$`)
)

// localPathOK refuses directories where a repository would be exposed to
// sites or collide with WPGenie's or the system's own files.
func (s *Service) localPathOK(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, ":,\n") || p == "/" {
		return fmt.Errorf("%w: path must be an absolute, clean directory path", ErrInvalidInput)
	}
	forbidden := []string{s.Cfg.SitesDir(), s.Cfg.SitesDir() + ".nosymfollow", s.Cfg.CaddyDataDir,
		filepath.Join(s.Cfg.DataDir, "mariadb"), filepath.Join(s.Cfg.DataDir, "mail"), "/etc", "/proc", "/sys",
		"/dev", "/usr", "/bin", "/sbin", "/lib", "/boot", "/run", "/var/lib/docker", "/root", "/home", "/tmp"}
	for _, f := range forbidden {
		if p == f || strings.HasPrefix(p, f+"/") {
			return fmt.Errorf("%w: backups can't be stored under %s", ErrInvalidInput, f)
		}
	}
	if p == s.Cfg.DataDir {
		return fmt.Errorf("%w: choose a directory of its own", ErrInvalidInput)
	}
	return nil
}

// Repos lists the backup repositories, creating the local one first.
func (s *Service) Repos(ctx context.Context) ([]*store.BackupRepo, error) {
	if s.Backups != nil {
		if _, err := s.ensureLocalRepo(ctx); err != nil {
			s.Log.Warn("creating the local backup repository", "err", err)
		}
	}
	return s.Store.Repos(ctx)
}

var localRepoMu sync.Mutex

// ensureLocalRepo returns the local repository, creating (and
// initialising) it on first use.
func (s *Service) ensureLocalRepo(ctx context.Context) (*store.BackupRepo, error) {
	localRepoMu.Lock()
	defer localRepoMu.Unlock()
	r, err := s.Store.GetRepo(ctx, LocalRepoID)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return r, err
	}
	r = &store.BackupRepo{ID: LocalRepoID, Name: "This server", Kind: backup.KindLocal,
		Location: filepath.Join(s.backupsDir(), "local"), Password: randString(40, passAlphabet)}
	if err := os.MkdirAll(r.Location, 0o700); err != nil {
		return nil, err
	}
	if err := s.Backups.Init(ctx, r); err != nil {
		return nil, err
	}
	if err := s.Store.CreateRepo(ctx, r); err != nil {
		return nil, err
	}
	return s.Store.GetRepo(ctx, LocalRepoID)
}

// AddRepo adds a backup repository and initialises it (or checks that the
// password opens it, for an existing one). An SFTP repository is kept even
// when the server refuses the login: add its public key to the server's
// authorized_keys, then check it.
func (s *Service) AddRepo(ctx context.Context, in RepoInput) (*store.BackupRepo, error) {
	if s.Backups == nil {
		return nil, errNoBackups
	}
	if !repoNameRe.MatchString(in.Name) {
		return nil, fmt.Errorf("%w: name must be 1-60 characters on one line", ErrInvalidInput)
	}
	if strings.ContainsAny(in.Password+in.KeyID+in.Secret+in.Region, "\n\r\x00") {
		return nil, fmt.Errorf("%w: credentials can't contain line breaks", ErrInvalidInput)
	}
	r := &store.BackupRepo{ID: "r" + randString(7, lowerAlnum), Name: in.Name, Kind: in.Kind, Password: in.Password}
	if r.Password == "" {
		r.Password = randString(40, passAlphabet)
	}
	prefix := strings.Trim(in.Prefix, "/")
	if !prefixPath.MatchString(prefix) || strings.Contains(prefix, "..") {
		return nil, fmt.Errorf("%w: invalid prefix", ErrInvalidInput)
	}
	switch in.Kind {
	case backup.KindLocal:
		if err := s.localPathOK(in.Path); err != nil {
			return nil, err
		}
		r.Location = in.Path
		if err := os.MkdirAll(in.Path, 0o700); err != nil {
			return nil, err
		}
	case backup.KindS3:
		endpoint := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(in.Endpoint), "https://"), "http://")
		endpoint = strings.TrimSuffix(endpoint, "/")
		if endpoint == "" {
			endpoint = "s3.amazonaws.com"
		}
		if !endpointRe.MatchString(endpoint) || !bucketRe.MatchString(in.Bucket) || in.KeyID == "" || in.Secret == "" {
			return nil, fmt.Errorf("%w: S3 needs an endpoint, a bucket, a key ID and a secret key", ErrInvalidInput)
		}
		r.Location = "s3:https://" + endpoint + "/" + in.Bucket
		if prefix != "" {
			r.Location += "/" + prefix
		}
		r.Secrets = store.RepoSecrets{AccessKeyID: in.KeyID, SecretAccessKey: in.Secret, Region: in.Region}
	case backup.KindB2:
		if !b2BucketRe.MatchString(in.Bucket) || in.KeyID == "" || in.Secret == "" {
			return nil, fmt.Errorf("%w: B2 needs a bucket, an application key ID and the key", ErrInvalidInput)
		}
		r.Location = "b2:" + in.Bucket + ":" + prefix
		r.Secrets = store.RepoSecrets{AccessKeyID: in.KeyID, SecretAccessKey: in.Secret}
	case backup.KindSFTP:
		if in.Port == 0 {
			in.Port = 22
		}
		path := in.Path
		if path == "" || !prefixPath.MatchString(path) || strings.Contains(path, "..") {
			return nil, fmt.Errorf("%w: SFTP needs a directory on the server", ErrInvalidInput)
		}
		if !backupHostOK(in.Host) {
			return nil, fmt.Errorf("%w: the SFTP server must be another machine", ErrInvalidInput)
		}
		r.Location = backup.SFTPLocation(in.User, in.Host, in.Port, path)
		if _, _, _, _, err := backup.ParseSFTP(r.Location); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		hostKey, err := s.Backups.KeyScan(ctx, in.Host, in.Port)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		priv, pub, err := newSSHKey("wpgenie-backup")
		if err != nil {
			return nil, err
		}
		r.Secrets = store.RepoSecrets{SSHPrivateKey: priv, SSHPublicKey: pub, KnownHosts: hostKey}
	default:
		return nil, fmt.Errorf("%w: kind must be local, s3, b2 or sftp", ErrInvalidInput)
	}
	initErr := s.Backups.Init(ctx, r)
	if initErr != nil && in.Kind != backup.KindSFTP {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, initErr)
	}
	if err := s.Store.CreateRepo(ctx, r); err != nil {
		return nil, err
	}
	if initErr != nil {
		s.Store.RepoChecked(ctx, r.ID, "not connected yet: add the public key to the server's authorized_keys, then check: "+initErr.Error())
	}
	return s.Store.GetRepo(ctx, r.ID)
}

// newSSHKey makes an Ed25519 key pair: the private key in OpenSSH format,
// the public key as an authorized_keys line.
func newSSHKey(comment string) (priv, pub string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, comment)
	if err != nil {
		return "", "", err
	}
	spk, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spk))) + " " + comment, nil
}

// HostKeyFingerprint is the SHA-256 fingerprint of a pinned host key line.
func HostKeyFingerprint(line string) string {
	f := strings.Fields(line)
	if len(f) < 3 {
		return ""
	}
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(f[1] + " " + f[2]))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

func (s *Service) repoLock(id string) *sync.Mutex {
	m, _ := s.repoMu.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// CheckRepo connects to a repository (creating it if it is still empty)
// and verifies its structure.
func (s *Service) CheckRepo(ctx context.Context, id string) (*store.BackupRepo, error) {
	if s.Backups == nil {
		return nil, errNoBackups
	}
	r, err := s.Store.GetRepo(ctx, id)
	if err != nil {
		return nil, err
	}
	mu := s.repoLock(id)
	mu.Lock()
	defer mu.Unlock()
	err = s.Backups.Init(ctx, r)
	if err == nil {
		err = s.Backups.Check(ctx, r)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if err := s.Store.RepoChecked(ctx, id, msg); err != nil {
		return nil, err
	}
	return s.Store.GetRepo(ctx, id)
}

// RemoveRepo forgets a repository; its data stays where it is.
func (s *Service) RemoveRepo(ctx context.Context, id string) error {
	r, err := s.Store.GetRepo(ctx, id)
	if err != nil {
		return err
	}
	if r.SitesUsing > 0 {
		return fmt.Errorf("%w: %d site(s) back up to it; change their backup settings first", ErrConflict, r.SitesUsing)
	}
	if id == LocalRepoID {
		return fmt.Errorf("%w: the local repository can't be removed", ErrInvalidInput)
	}
	return s.Store.DeleteRepo(ctx, id)
}

// RepoPassword is a repository's restic password: what restoring these
// backups anywhere else (another server, restic by hand) needs.
func (s *Service) RepoPassword(ctx context.Context, id string) (string, error) {
	r, err := s.Store.GetRepo(ctx, id)
	if err != nil {
		return "", err
	}
	return r.Password, nil
}

// ---- Policies ----

// PolicyInput sets a site's backup schedule and retention. RepoID "" turns
// scheduled backups off (manual backups go to the local repository).
type PolicyInput struct {
	RepoID        string `json:"repo_id"`
	IntervalHours int    `json:"interval_hours"`
	KeepLast      int    `json:"keep_last"`
	KeepDaily     int    `json:"keep_daily"`
	KeepWeekly    int    `json:"keep_weekly"`
	KeepMonthly   int    `json:"keep_monthly"`
}

var backupIntervals = []int{0, 1, 2, 3, 4, 6, 8, 12, 24, 48, 168}

// SetBackupPolicy changes where and how often a site is backed up, and
// which backups are kept.
func (s *Service) SetBackupPolicy(ctx context.Context, siteID string, in PolicyInput) (*store.BackupPolicy, error) {
	if s.Backups == nil {
		return nil, errNoBackups
	}
	if _, err := s.Store.GetSite(ctx, siteID); err != nil {
		return nil, err
	}
	if in.RepoID == "" {
		err := s.Store.DeleteBackupPolicy(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
		return nil, err
	}
	if !slices.Contains(backupIntervals, in.IntervalHours) {
		return nil, fmt.Errorf("%w: interval_hours must be one of %v", ErrInvalidInput, backupIntervals)
	}
	for _, k := range []int{in.KeepLast, in.KeepDaily, in.KeepWeekly, in.KeepMonthly} {
		if k < 0 || k > 1000 {
			return nil, fmt.Errorf("%w: keep values must be between 0 and 1000", ErrInvalidInput)
		}
	}
	if in.IntervalHours > 0 && in.KeepLast+in.KeepDaily+in.KeepWeekly+in.KeepMonthly == 0 {
		return nil, fmt.Errorf("%w: keep at least one backup (otherwise scheduled backups pile up forever)", ErrInvalidInput)
	}
	if in.RepoID == LocalRepoID {
		if _, err := s.ensureLocalRepo(ctx); err != nil {
			return nil, err
		}
	} else if _, err := s.Store.GetRepo(ctx, in.RepoID); err != nil {
		return nil, fmt.Errorf("%w: unknown repository %q", ErrInvalidInput, in.RepoID)
	}
	if err := s.Store.SetBackupPolicy(ctx, &store.BackupPolicy{SiteID: siteID, RepoID: in.RepoID,
		IntervalHours: in.IntervalHours, KeepLast: in.KeepLast, KeepDaily: in.KeepDaily, KeepWeekly: in.KeepWeekly,
		KeepMonthly: in.KeepMonthly}); err != nil {
		return nil, err
	}
	return s.Store.BackupPolicy(ctx, siteID)
}

// defaultBackupPolicy gives a new live site daily local backups (7 daily,
// 4 weekly, 6 monthly). Best effort: a site without backups still works.
func (s *Service) defaultBackupPolicy(ctx context.Context, st *store.Site) {
	if s.Backups == nil || st.ParentID != "" {
		return
	}
	if _, err := s.Store.BackupPolicy(ctx, st.ID); err == nil {
		return
	}
	if _, err := s.SetBackupPolicy(ctx, st.ID, PolicyInput{RepoID: LocalRepoID, IntervalHours: 24,
		KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}); err != nil {
		s.Log.Warn("setting up default backups", "site", st.ID, "err", err)
	}
}

// siteRepo is the repository a site's manual backups go to: its policy's,
// or the local one.
func (s *Service) siteRepo(ctx context.Context, siteID string) (*store.BackupRepo, error) {
	if p, err := s.Store.BackupPolicy(ctx, siteID); err == nil {
		return s.Store.GetRepo(ctx, p.RepoID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return s.ensureLocalRepo(ctx)
}

// ---- Backing up ----

// StartBackup backs a site up now, as a job.
func (s *Service) StartBackup(ctx context.Context, siteID string) (int64, error) {
	if s.Backups == nil {
		return 0, errNoBackups
	}
	st, err := s.Store.GetSite(ctx, siteID)
	if err != nil {
		return 0, err
	}
	if st.Status != store.StatusActive {
		return 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	return s.submitBackup(ctx, siteID, BackupManual)
}

func (s *Service) submitBackup(ctx context.Context, siteID, kind string) (int64, error) {
	return s.Jobs.Submit(ctx, s.siteJob(siteID, "backup", true), func(ctx context.Context, t *jobs.Task) error {
		st, err := s.Store.GetSite(ctx, siteID) // current, after waiting for the lock
		if err != nil {
			return err
		}
		repo, err := s.siteRepo(ctx, siteID)
		if err != nil {
			return err
		}
		started := time.Now()
		info, err := s.backupLocked(ctx, st, repo, kind, t.Progress)
		if kind == BackupScheduled { // the schedule counts from scheduled backups only
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if rerr := s.Store.BackupAttempted(context.WithoutCancel(ctx), siteID, started, msg); rerr != nil &&
				!errors.Is(rerr, store.ErrNotFound) {
				s.Log.Warn("recording backup", "site", siteID, "err", rerr)
			}
		}
		if err != nil {
			s.event(siteID, "backup", fmt.Sprintf("%s backup failed: %v", kind, err))
			return err
		}
		t.SetResult(info)
		s.event(siteID, "backup", fmt.Sprintf("%s backup %s to %s (%s, %s new)", strings.ToUpper(kind[:1])+kind[1:],
			info.ShortID, repo.Name, humanBytes(info.Size), humanBytes(info.Added)))
		return nil
	})
}

// backupLocked backs a site up to repo. Caller holds the site's
// maintenance lock (the database dump and the files are one moment).
func (s *Service) backupLocked(ctx context.Context, st *store.Site, repo *store.BackupRepo, kind string,
	report Progress) (*BackupInfo, error) {
	report(2, "Connecting to "+repo.Name)
	if err := s.Backups.Init(ctx, repo); err != nil {
		return nil, err
	}
	stage := filepath.Join(s.backupsDir(), "staging", st.ID+"-"+randString(8, lowerAlnum))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)

	report(5, "Dumping the database")
	if err := writeFile(filepath.Join(stage, backup.DumpFile), func(w io.Writer) error {
		return s.Dumper.Dump(ctx, st.DBName, w)
	}); err != nil {
		return nil, fmt.Errorf("dumping the database: %w", err)
	}
	meta := BackupMeta{SiteID: st.ID, Domain: st.PrimaryDomain, Name: st.Name, PHPVersion: st.PHPVersion,
		WPGenie: s.Version, Created: time.Now().UTC()}
	var err error
	// Listed right after the dump, like update snapshots: tables created
	// after it aren't in it, and a restore drops them.
	if meta.Tables, err = s.DB.Tables(ctx, st.DBName); err != nil {
		return nil, err
	}
	if meta.TablePrefix, err = s.tablePrefix(st.ID); err != nil {
		return nil, err
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, backup.MetaFile), mb, 0o600); err != nil {
		return nil, err
	}

	progress := func(pct int) { report(10+pct*85/100, "Backing up files and database") }
	id, err := s.Backups.Backup(ctx, repo, backup.BackupInput{SiteID: st.ID, Files: s.Cfg.SiteRoot(st.ID), DB: stage,
		Tags: []string{kind, "domain=" + st.PrimaryDomain}, Exclude: copyExcludes, Progress: progress})
	if err != nil {
		return nil, err
	}
	report(96, "Applying retention")
	if err := s.applyRetention(ctx, st.ID, repo, kind); err != nil {
		// The backup itself is fine; old ones stay until the next run.
		s.Log.Warn("backup retention", "site", st.ID, "err", err)
	}
	snaps, err := s.Backups.Snapshots(ctx, repo, "site="+st.ID)
	if err != nil {
		return nil, err
	}
	for _, sn := range snaps {
		if sn.ID == id || strings.HasPrefix(sn.ID, id) {
			info := infoFrom(repo.ID, sn)
			return &info, nil
		}
	}
	return &BackupInfo{ID: id, ShortID: id[:min(8, len(id))], RepoID: repo.ID, SiteID: st.ID, Kind: kind, Time: time.Now()}, nil
}

func (s *Service) applyRetention(ctx context.Context, siteID string, repo *store.BackupRepo, kind string) error {
	switch kind {
	case BackupSafety:
		return s.Backups.Apply(ctx, repo, []string{"site=" + siteID, BackupSafety}, backup.Keep{Within: "7d"})
	case BackupScheduled:
		p, err := s.Store.BackupPolicy(ctx, siteID)
		if err != nil || p.RepoID != repo.ID {
			return err
		}
		return s.Backups.Apply(ctx, repo, []string{"site=" + siteID, BackupScheduled},
			backup.Keep{Last: p.KeepLast, Daily: p.KeepDaily, Weekly: p.KeepWeekly, Monthly: p.KeepMonthly})
	}
	return nil // manual backups are kept until deleted
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---- Listing ----

// SiteBackups lists a site's backups in every repository, newest first;
// repositories that can't be read are reported in errs.
func (s *Service) SiteBackups(ctx context.Context, siteID string) (list []BackupInfo, errs map[string]string, err error) {
	if s.Backups == nil {
		return nil, nil, errNoBackups
	}
	repos, err := s.Repos(ctx)
	if err != nil {
		return nil, nil, err
	}
	return s.listBackups(ctx, repos, "site="+siteID)
}

// RepoBackups lists every WPGenie backup in a repository (deleted sites'
// too), newest first.
func (s *Service) RepoBackups(ctx context.Context, repoID string) ([]BackupInfo, error) {
	if s.Backups == nil {
		return nil, errNoBackups
	}
	r, err := s.Store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	list, errs, err := s.listBackups(ctx, []*store.BackupRepo{r}, "wpgenie")
	if err != nil {
		return nil, err
	}
	if msg := errs[repoID]; msg != "" {
		return nil, errors.New(msg)
	}
	return list, nil
}

func (s *Service) listBackups(ctx context.Context, repos []*store.BackupRepo, tag string) ([]BackupInfo, map[string]string, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	list := []BackupInfo{}
	errs := map[string]string{}
	for _, r := range repos {
		wg.Go(func() {
			c, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			snaps, err := s.Backups.Snapshots(c, r, "wpgenie", tag)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[r.ID] = err.Error()
				return
			}
			for _, sn := range snaps {
				list = append(list, infoFrom(r.ID, sn))
			}
		})
	}
	wg.Wait()
	slices.SortFunc(list, func(a, b BackupInfo) int { return b.Time.Compare(a.Time) })
	return list, errs, nil
}

// findBackup resolves a snapshot of a site (siteID "" = any site).
func (s *Service) findBackup(ctx context.Context, repoID, snapID, siteID string) (*store.BackupRepo, *BackupInfo, error) {
	if s.Backups == nil {
		return nil, nil, errNoBackups
	}
	if !backup.ValidID(snapID) {
		return nil, nil, fmt.Errorf("%w: invalid backup ID", ErrInvalidInput)
	}
	r, err := s.Store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, nil, err
	}
	tags := []string{"wpgenie"}
	if siteID != "" {
		tags = append(tags, "site="+siteID)
	}
	snaps, err := s.Backups.Snapshots(ctx, r, tags...)
	if err != nil {
		return nil, nil, err
	}
	for _, sn := range snaps {
		if sn.ID == snapID || (len(snapID) >= 8 && strings.HasPrefix(sn.ID, snapID)) {
			info := infoFrom(r.ID, sn)
			return r, &info, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: no such backup of this site", store.ErrNotFound)
}

// DeleteBackup removes one backup of a site (its data goes with the
// repository's next prune).
func (s *Service) DeleteBackup(ctx context.Context, siteID, repoID, snapID string) error {
	r, b, err := s.findBackup(ctx, repoID, snapID, siteID)
	if err != nil {
		return err
	}
	if err := s.Backups.Forget(ctx, r, []string{b.ID}); err != nil {
		return err
	}
	if siteID != "" {
		s.event(siteID, "backup", "Backup "+b.ShortID+" deleted")
	}
	return nil
}

// DownloadBackup writes a backup as a tar archive to w: backup/files/ (the
// WordPress install) and backup/db/database.sql + meta.json. It returns
// the backup before writing anything, for naming the download.
func (s *Service) DownloadBackup(ctx context.Context, siteID, repoID, snapID string, start func(*BackupInfo) io.Writer) error {
	r, b, err := s.findBackup(ctx, repoID, snapID, siteID)
	if err != nil {
		return err
	}
	return s.Backups.Dump(ctx, r, b.ID, backup.Root, start(b))
}

// ---- Restoring ----

// RestoreInput selects what to restore from a backup.
type RestoreInput struct {
	RepoID   string `json:"repo_id"`
	BackupID string `json:"backup_id"`
	Files    bool   `json:"files"`
	Database bool   `json:"database"`
}

// RestoreResult is recorded with a restore job.
type RestoreResult struct {
	Backup       string `json:"backup"`
	SafetyBackup string `json:"safety_backup,omitempty"`
	Health       Health `json:"health"`
}

// StartRestore puts a site back to one of its backups, as a job. The
// site's current state is backed up first (kind safety, kept 7 days), so
// a restore can itself be undone.
func (s *Service) StartRestore(ctx context.Context, siteID string, in RestoreInput) (int64, error) {
	if !in.Files && !in.Database {
		return 0, fmt.Errorf("%w: choose files, database or both", ErrInvalidInput)
	}
	st, err := s.Store.GetSite(ctx, siteID)
	if err != nil {
		return 0, err
	}
	if st.Status != store.StatusActive {
		return 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	repo, b, err := s.findBackup(ctx, in.RepoID, in.BackupID, siteID)
	if err != nil {
		return 0, err
	}
	return s.Jobs.Submit(ctx, s.siteJob(siteID, "restore", true), func(ctx context.Context, t *jobs.Task) error {
		st, err := s.Store.GetSite(ctx, siteID)
		if err != nil {
			return err
		}
		res := &RestoreResult{Backup: b.ShortID}
		t.SetResult(res)
		safetyRepo, err := s.siteRepo(ctx, siteID)
		if err != nil {
			return err
		}
		safety, err := s.backupLocked(ctx, st, safetyRepo, BackupSafety, func(pct int, step string) {
			t.Progress(pct*40/100, "Backing up the current state first: "+strings.ToLower(step))
		})
		if err != nil {
			return fmt.Errorf("backing up the current state failed, nothing was restored: %w", err)
		}
		res.SafetyBackup = safety.ShortID
		if err := s.restoreLocked(ctx, st, repo, b.ID, in.Files, in.Database, func(pct int, step string) {
			t.Progress(40+pct*55/100, step)
		}); err != nil {
			s.event(siteID, "backup", fmt.Sprintf("Restoring backup %s FAILED: %v. The state before the restore is backup %s.",
				b.ShortID, err, safety.ShortID))
			return fmt.Errorf("%w (the state before the restore is in backup %s)", err, safety.ShortID)
		}
		t.Progress(97, "Checking the site")
		res.Health = s.Prober.Probe(ctx, st.PrimaryDomain)
		what := map[[2]bool]string{{true, true}: "files and database", {true, false}: "files", {false, true}: "database"}[[2]bool{in.Files, in.Database}]
		s.event(siteID, "backup", fmt.Sprintf("Restored %s from backup %s of %s (undo: backup %s)", what, b.ShortID,
			b.Time.Local().Format("2006-01-02 15:04"), safety.ShortID))
		return nil
	})
}

// restoreLocked restores a site's files and/or database from a snapshot.
// Caller holds the site's maintenance lock.
func (s *Service) restoreLocked(ctx context.Context, st *store.Site, repo *store.BackupRepo, snapID string,
	files, database bool, report Progress) error {
	meta, err := s.backupMeta(ctx, repo, snapID)
	if err != nil {
		return err
	}
	if database {
		prefix, err := s.tablePrefix(st.ID)
		if err != nil {
			return err
		}
		if meta.TablePrefix != "" && meta.TablePrefix != prefix {
			return fmt.Errorf("%w: the backup's tables use the prefix %q, this site %q: restore it as a new site instead",
				ErrInvalidInput, meta.TablePrefix, prefix)
		}
	}
	if files {
		report(10, "Restoring files")
		// Entries are backup/files/<path>: strip two components.
		if err := s.replaceInstall(ctx, st.ID, 2, keepNone, func(w io.Writer) error {
			return s.Backups.Dump(ctx, repo, snapID, backup.FilesPath, w)
		}); err != nil {
			return err
		}
	}
	if database {
		report(60, "Restoring the database")
		if err := s.restoreDB(ctx, st, repo, snapID, meta.Tables); err != nil {
			return err
		}
	}
	report(95, "Purging caches")
	if err := s.purgeLocal(ctx, st); err != nil {
		return err
	}
	if err := s.purgeCDNIfOn(ctx, st); err != nil {
		s.Log.Warn("purging the CDN after a restore", "site", st.ID, "err", err)
	}
	return nil
}

// restoreDB loads a backup's dump and drops the tables it didn't have.
func (s *Service) restoreDB(ctx context.Context, st *store.Site, repo *store.BackupRepo, snapID string, tables []string) error {
	err := pipe(func(w io.Writer) error {
		return s.Backups.Dump(ctx, repo, snapID, backup.DBPath+"/"+backup.DumpFile, w)
	}, func(r io.Reader) error { return s.Dumper.Restore(ctx, st.DBName, r) })
	if err != nil {
		return fmt.Errorf("restoring the database: %w", err)
	}
	// Before anything reads the site through WordPress again: cached
	// options would hide the restored ones (and WP-CLI updates matching a
	// cached value are skipped).
	if err := s.flushObjectCache(ctx, st.ID); err != nil {
		return err
	}
	if len(tables) == 0 {
		return nil // an older backup without a table list: keep everything
	}
	now, err := s.DB.Tables(ctx, st.DBName)
	if err != nil {
		return err
	}
	var extra []string
	for _, t := range now {
		if !slices.Contains(tables, t) {
			extra = append(extra, t)
		}
	}
	if len(extra) > 0 {
		return s.DB.DropTables(ctx, st.DBName, extra)
	}
	return nil
}

func (s *Service) backupMeta(ctx context.Context, repo *store.BackupRepo, snapID string) (*BackupMeta, error) {
	var buf strings.Builder
	if err := s.Backups.Dump(ctx, repo, snapID, backup.DBPath+"/"+backup.MetaFile, &limitWriter{w: &buf, n: 1 << 20}); err != nil {
		return nil, fmt.Errorf("reading the backup's description: %w", err)
	}
	var m BackupMeta
	if err := json.Unmarshal([]byte(buf.String()), &m); err != nil {
		return nil, fmt.Errorf("reading the backup's description: %w", err)
	}
	return &m, nil
}

// RestoreAsNewInput creates a new site from a backup (of a deleted site,
// or a copy of a live one on another domain).
type RestoreAsNewInput struct {
	RepoID   string `json:"repo_id"`
	BackupID string `json:"backup_id"`
	Domain   string `json:"domain"`
}

// StartRestoreAsNew creates a site on a new domain from any WPGenie backup
// in a repository, as a job: the backup's files and database, with links
// rewritten from the backup's domain to the new one.
func (s *Service) StartRestoreAsNew(ctx context.Context, in RestoreAsNewInput) (*store.Site, int64, error) {
	domain, err := NormalizeDomain(in.Domain)
	if err != nil {
		return nil, 0, err
	}
	repo, b, err := s.findBackup(ctx, in.RepoID, in.BackupID, "")
	if err != nil {
		return nil, 0, err
	}
	meta, err := s.backupMeta(ctx, repo, b.ID)
	if err != nil {
		return nil, 0, err
	}
	if meta.TablePrefix == "" || meta.Domain == "" {
		return nil, 0, fmt.Errorf("%w: this backup has no description (made by an older version)", ErrInvalidInput)
	}
	// Both end up in generated config and WP-CLI arguments.
	if !regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`).MatchString(meta.TablePrefix) {
		return nil, 0, fmt.Errorf("%w: the backup's table prefix %q is not usable", ErrInvalidInput, meta.TablePrefix)
	}
	if _, err := NormalizeDomain(meta.Domain); err != nil {
		return nil, 0, fmt.Errorf("%w: the backup's domain %q is not valid", ErrInvalidInput, meta.Domain)
	}
	name := meta.Name
	if name == "" || name == meta.Domain {
		name = domain
	}
	if validName(name) != nil {
		name = domain
	}
	st := newSite(domain, name)
	if slices.Contains(s.Cfg.PHPVersions, meta.PHPVersion) {
		st.PHPVersion = meta.PHPVersion
	}
	bld, err := s.reserve(ctx, st)
	if err != nil {
		return nil, 0, err
	}
	out := *bld.st
	id, err := s.Jobs.Submit(ctx, s.siteJob(st.ID, "restore-new", true), func(ctx context.Context, t *jobs.Task) (err error) {
		defer func() {
			if err != nil {
				bld.rollback(err)
			}
		}()
		if err := bld.start(ctx, meta.TablePrefix, "", t.Progress); err != nil {
			return err
		}
		t.Progress(20, "Restoring files")
		if err := s.replaceInstall(ctx, st.ID, 2, keepNone, func(w io.Writer) error {
			return s.Backups.Dump(ctx, repo, b.ID, backup.FilesPath, w)
		}); err != nil {
			return err
		}
		t.Progress(55, "Restoring the database")
		if err := s.restoreDB(ctx, st, repo, b.ID, meta.Tables); err != nil {
			return err
		}
		t.Progress(80, "Rewriting links to "+domain)
		if err := s.searchReplace(ctx, st.ID, meta.Domain, domain); err != nil {
			return err
		}
		if err := bld.finish(ctx, t.Progress); err != nil {
			return err
		}
		t.SetResult(map[string]string{"site_id": st.ID, "url": "https://" + domain, "backup": b.ShortID})
		s.event(st.ID, "backup", fmt.Sprintf("Created from backup %s of %s (%s)", b.ShortID, meta.Domain, meta.SiteID))
		return nil
	})
	if err != nil {
		bld.rollback(err)
		return nil, 0, err
	}
	return &out, id, nil
}

// ---- Schedule and repository upkeep ----

// RunBackups starts scheduled backups when they are due and keeps the
// repositories tidy (weekly prune and check, in the maintenance window).
func (s *Service) RunBackups(ctx context.Context) {
	if s.Backups == nil {
		return
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	pending := map[string]bool{}
	var mu sync.Mutex
	for {
		s.backupTick(ctx, time.Now(), pending, &mu)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// backupDue decides whether a site's scheduled backup should start now.
// Daily and longer schedules run in the maintenance window (before the
// night's automatic updates); shorter ones by interval. A failed attempt
// is retried after an hour, not at every tick.
func backupDue(p *store.BackupPolicy, now time.Time, inWindow bool) bool {
	if p.IntervalHours <= 0 {
		return false
	}
	if !p.LastAttemptAt.IsZero() && p.LastError != "" && now.Sub(p.LastAttemptAt) < time.Hour {
		return false
	}
	since := now.Sub(p.LastBackupAt)
	interval := time.Duration(p.IntervalHours) * time.Hour
	if p.IntervalHours >= 24 {
		// Never backed up: don't wait for the night. Otherwise the window,
		// with slack so a run that finished late still counts as a day.
		if p.LastBackupAt.IsZero() {
			return p.LastAttemptAt.IsZero()
		}
		return (inWindow && since > interval-4*time.Hour) || since > interval+6*time.Hour
	}
	return since >= interval
}

func (s *Service) backupTick(ctx context.Context, now time.Time, pending map[string]bool, mu *sync.Mutex) {
	policies, err := s.Store.BackupPolicies(ctx)
	if err != nil {
		s.Log.Error("backups: listing policies", "err", err)
		return
	}
	window := s.inMaintenanceWindow(now)
	for _, p := range policies {
		mu.Lock()
		busy := pending[p.SiteID]
		mu.Unlock()
		if busy || !backupDue(p, now, window) {
			continue
		}
		st, err := s.Store.GetSite(ctx, p.SiteID)
		if err != nil || st.Status != store.StatusActive {
			continue
		}
		mu.Lock()
		pending[p.SiteID] = true
		mu.Unlock()
		id, err := s.submitBackup(jobs.WithActor(ctx, "scheduler"), p.SiteID, BackupScheduled)
		if err != nil {
			s.Log.Error("backups: starting a scheduled backup", "site", p.SiteID, "err", err)
			mu.Lock()
			delete(pending, p.SiteID)
			mu.Unlock()
			continue
		}
		go func(siteID string) {
			s.Jobs.WaitJob(context.WithoutCancel(ctx), id)
			mu.Lock()
			delete(pending, siteID)
			mu.Unlock()
		}(p.SiteID)
	}
	if window {
		s.repoUpkeep(ctx, now)
		if day := now.Format(time.DateOnly); s.certWarnDay != day {
			s.certWarnDay = day
			s.certExpiryWarnings(ctx, now)
		}
	}
}

// repoUpkeep prunes (deletes data of forgotten backups) and checks each
// repository once a week.
func (s *Service) repoUpkeep(ctx context.Context, now time.Time) {
	repos, err := s.Store.Repos(ctx)
	if err != nil {
		return
	}
	for _, r := range repos {
		if now.Sub(r.PrunedAt) < 6*24*time.Hour {
			continue
		}
		mu := s.repoLock(r.ID)
		if !mu.TryLock() {
			continue
		}
		s.Store.RepoPruned(ctx, r.ID) // before running: a failing prune isn't retried every tick
		_, err := s.Jobs.Submit(jobs.WithActor(ctx, "scheduler"), jobs.Spec{Kind: "repo-upkeep", Heavy: true},
			func(ctx context.Context, t *jobs.Task) error {
				defer mu.Unlock()
				t.Progress(5, "Pruning "+r.Name)
				t.SetResult(map[string]string{"repo_id": r.ID})
				err := s.Backups.Prune(ctx, r)
				if err == nil {
					t.Progress(60, "Checking "+r.Name)
					err = s.Backups.Check(ctx, r)
				}
				msg := ""
				if err != nil {
					msg = err.Error()
				}
				s.Store.RepoChecked(context.WithoutCancel(ctx), r.ID, msg)
				return err
			})
		if err != nil {
			mu.Unlock()
		}
	}
}

// backupHostOK refuses loopback and link-local SFTP hosts: backups on the
// same machine belong in a local repository, and the panel must not be a
// way to probe this server's own ports.
func backupHostOK(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return !(ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast())
	}
	return host != "localhost"
}
