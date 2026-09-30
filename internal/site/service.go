// Package site orchestrates the lifecycle of a WordPress site across the
// panel store, MariaDB, the container runtime and the reverse proxy.
package site

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/domain"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/store"
)

var (
	ErrDomainTaken   = errors.New("domain is already attached to a site")
	ErrInvalidDomain = domain.ErrInvalid
	ErrInvalidInput  = errors.New("invalid input")
)

type DBProvisioner interface {
	CreateSiteDB(ctx context.Context, name, user, password string) error
	DropSiteDB(ctx context.Context, name, user string) error
	SetConnectionLimit(ctx context.Context, user string, n int) error
	Tables(ctx context.Context, db string) ([]string, error)
	DropTables(ctx context.Context, db string, tables []string) error
	// Temporary accounts with rights on one database (see dbprov).
	CreateTempUser(ctx context.Context, db, user, password string) error
	DropTempUser(ctx context.Context, user string) error
}

// ObjectCache clears keys from the shared object cache. Flushing must never
// run site code (see runtime.Valkey).
type ObjectCache interface {
	FlushPrefix(ctx context.Context, prefix string) error
}

type ProxyApplier interface {
	Apply(ctx context.Context, sites []proxy.Site) error
}

type Service struct {
	Cfg     *config.Config
	Store   *store.Store
	Runtime runtime.Runtime
	DB      DBProvisioner
	Dumper  DBDumper
	Proxy   ProxyApplier
	Cache   ObjectCache
	Prober  Prober
	Vulns   VulnDB
	// Directory is wordpress.org's plugin directory (plugin analysis).
	Directory PluginDirectory
	Mailer    Mailer
	// Webmail returns the webmail host and upstream to publish, or "".
	Webmail func() (host, upstream string)
	// CDN purges Cloudflare's cache; CDNRanges are its edge networks; DNS
	// (nil: system resolver) checks whether domains go through it.
	CDN       CDNProvider
	CDNRanges *cdn.Ranges
	DNS       Resolver
	// interfaceAddrs lists this server's addresses (nil: its network
	// interfaces'); tests replace it.
	interfaceAddrs func() ([]net.Addr, error)
	// Bunny is bunny.net's API (pull zones).
	Bunny PullZoneAPI
	// Offload copies uploads to object storage (offload.Rclone); nil: off.
	Offload OffloadEngine
	// Latency reports sites' recent PHP response times (the analytics
	// ingester), for autoscaling on them; nil: CPU and workers only.
	Latency LatencySource
	Log     *slog.Logger
	// Jobs runs long operations (create, backups, restores, clones) in the
	// background; Backups is restic (see internal/backup); Images builds
	// the PHP image of a version the first time a site switches to it.
	Jobs    *jobs.Queue
	Backups BackupEngine
	Images  ImageBuilder
	// Version is WPGenie's version, recorded in backups.
	Version string
	// SiteRemoved is told about deleted sites (SFTP logins, phpMyAdmin
	// sessions).
	SiteRemoved func(ctx context.Context, id string)
	// AccessChanged is told when SFTP logins changed other than through the
	// SFTP service (a site moved here with its logins).
	AccessChanged func(ctx context.Context)

	// Cluster is the control plane's registry of other servers (nil on a
	// node, and on the panel until one is added). ClusterClient dials other
	// servers as this one (nil until it is part of a cluster). DomainTaken,
	// if set, also checks domains used on other servers.
	Cluster       *cluster.Controller
	ClusterClient func() *cluster.Client
	DomainTaken   func(ctx context.Context, domain, exceptSite string) (bool, error)
	// NodeID is this server's node ID once paired (nil on the panel:
	// "local"). Links runs the containers through which this node's
	// replicas of other nodes' sites reach their database.
	NodeID func() (string, bool)
	// HealthToken is this daemon's shield health token (the panel probes a
	// node's sites with it). ContainerIP finds a container's address on the
	// Docker network (Valkey, for tunnels).
	HealthToken string
	ContainerIP func(ctx context.Context, name string) (string, error)
	Links       LinkManager
	// CacheACL reloads Valkey's users (nil: no per-site cache users).
	CacheACL ValkeyACL
	cacheKey cacheKeyState
	// remoteMu: site ID -> *sync.Mutex (one spread sync at a time);
	// remoteErr: site ID -> the last one's error.
	remoteMu, remoteErr sync.Map
	tun                 tunnels
	// SiteSuspended is told when a site is suspended or back (SFTP logins
	// and phpMyAdmin sessions end while it is suspended).
	SiteSuspended func(ctx context.Context, id string, suspended bool)

	// opsMu serialises everything that allocates ports or starts/stops
	// containers (create, scale, cache changes): port allocation is only
	// race-free while the allocated ports are recorded under the same lock.
	opsMu    sync.Mutex
	syncMu   sync.Mutex // see Sync
	shieldSt atomic.Pointer[map[string]shield.SiteSettings]
	cpu      sync.Map // site ID -> CPUReading, written by the autoscaler loop
	// maint holds one mutex per site (ID -> *sync.Mutex) so updates and
	// scans of a site never overlap; different sites run independently.
	maint sync.Map
	// scaleFailNoted: site ID -> time of the last autoscale failure logged.
	scaleFailNoted sync.Map
	// lastMaint: site ID -> time the maintenance loop last ran for it.
	lastMaint sync.Map
	inflight  sync.WaitGroup // running WordPress updates
	cdn       cdnState
	offload   offloadState
	brand     brandState
	// builds: PHP version -> *sync.Mutex, so one image builds at a time.
	builds sync.Map
	// repoMu serialises repository maintenance (prune, check) per repo.
	repoMu sync.Map
	// phpLogLocks: site ID -> *sync.Mutex, one error log read at a time.
	phpLogLocks sync.Map
	// certWarnDay: the day expiring certificates were last reported (the
	// backup scheduler's goroutine only).
	certWarnDay string
}

type CreateInput struct {
	Domain     string `json:"domain"`
	Name       string `json:"name"`
	AdminEmail string `json:"admin_email"`
	AdminUser  string `json:"admin_user"`
}

type Credentials struct {
	URL      string `json:"url"`
	AdminURL string `json:"admin_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

var (
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	userRe  = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,60}$`)
)

func NormalizeDomain(d string) (string, error) { return domain.Normalize(d) }

// Progress reports how far a long operation is (see jobs.Task.Progress).
type Progress func(pct int, step string)

func noProgress(int, string) {}

// siteBuild provisions a new site step by step. Every completed step
// registers an undo; if a later step fails, rollback runs them in reverse,
// so a failed create (or clone, or restore into a new site) leaves nothing
// behind and the domain can be retried.
type siteBuild struct {
	s              *Service
	st             *store.Site
	dbUser, dbPass string
	undo           []func()
}

func bgCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Minute)
}

func (b *siteBuild) rollback(cause error) {
	b.s.Log.Error("site provisioning failed, rolling back", "site", b.st.ID, "err", cause)
	// Fresh contexts: the job's own context may be what failed.
	for i := len(b.undo) - 1; i >= 0; i-- {
		b.undo[i]()
	}
	b.undo = nil
}

// newSite is a site record with WPGenie's defaults for new sites.
func newSite(domain, name string) *store.Site {
	return &store.Site{
		Name: name, PrimaryDomain: domain, PHPVersion: "8.3",
		ShieldMode: string(shield.ModeStandard), BlockAIBots: true, WAF: true,
		Reputation: ReputationChallenge, CountryMode: CountryOff, CountryAction: ReputationBlock, BodyWAF: BodyWAFBlock,
		MemoryMB: defaultMemoryMB, CPUs: defaultCPUs, Replicas: 1,
		PageCache: true, ObjectCache: true, Optimize: DefaultOptimizations(),
	}
}

// validName: the name ends up in generated config comments, so no line
// breaks or other control characters.
func validName(name string) error {
	if utf8.RuneCountInString(name) > 100 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: name must be up to 100 characters on one line", ErrInvalidInput)
	}
	return nil
}

// reserve records a new site (status provisioning) with its primary domain
// and first port. opsMu makes the domain check and the port allocation
// atomic with the insert.
func (s *Service) reserve(ctx context.Context, st *store.Site) (*siteBuild, error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if err := s.domainFree(ctx, st.PrimaryDomain); err != nil {
		return nil, err
	}
	ports, err := s.Store.AllocatePorts(ctx, s.Cfg.SitePortBase, 1)
	if err != nil {
		return nil, err
	}
	id := "s" + randString(7, lowerAlnum)
	st.ID, st.FPMPort, st.Upstreams = id, ports[0], []int{ports[0]}
	st.DBName, st.Status = "wp_"+id, store.StatusProvisioning
	if err := s.Store.CreateSite(ctx, st); err != nil {
		return nil, err
	}
	st.Domains = []string{st.PrimaryDomain}
	b := &siteBuild{s: s, st: st, dbUser: "u_" + id, dbPass: randString(32, passAlphabet)}
	b.undo = append(b.undo, func() { c, cancel := bgCtx(); defer cancel(); s.Store.DeleteSite(c, id) })
	return b, nil
}

// DomainFree checks a domain can be attached to a site on another server:
// the panel and mail hostnames, this server's sites and the registry.
func (s *Service) DomainFree(ctx context.Context, name string) error {
	domain, err := NormalizeDomain(name)
	if err != nil {
		return err
	}
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	return s.domainFree(ctx, domain)
}

// domainFree checks a domain can be attached to a site. Caller holds opsMu.
func (s *Service) domainFree(ctx context.Context, domain string) error {
	return s.domainFreeExcept(ctx, domain, "")
}

// domainFreeExcept is domainFree for a site that may already hold the
// domain on another server (moving here).
func (s *Service) domainFreeExcept(ctx context.Context, domain, except string) error {
	if s.Webmail != nil {
		if host, _ := s.Webmail(); host == domain {
			return fmt.Errorf("%w: it is the mail server's hostname", ErrDomainTaken)
		}
	}
	if domain == s.Cfg.PanelDomain {
		return fmt.Errorf("%w: it is the panel's hostname", ErrDomainTaken)
	}
	if taken, err := s.Store.DomainExists(ctx, domain); err != nil {
		return err
	} else if taken {
		return ErrDomainTaken
	}
	if s.DomainTaken != nil {
		if taken, err := s.DomainTaken(ctx, domain, except); err != nil {
			return err
		} else if taken {
			return fmt.Errorf("%w (by a site on another server)", ErrDomainTaken)
		}
	}
	return nil
}

// start creates the site's database and directory (wp-config.php with the
// given table prefix, random when "") and runs its first replica.
func (b *siteBuild) start(ctx context.Context, prefix, environment string, report Progress) error {
	s, st := b.s, b.st
	report(5, "Creating the database")
	if err := s.DB.CreateSiteDB(ctx, st.DBName, b.dbUser, b.dbPass); err != nil {
		return err
	}
	b.undo = append(b.undo, func() { c, cancel := bgCtx(); defer cancel(); s.DB.DropSiteDB(c, st.DBName, b.dbUser) })

	dir, docroot := s.Cfg.SiteDir(st.ID), s.Cfg.SiteRoot(st.ID)
	if err := s.prepareFiles(st.ID, dir, docroot, b.dbUser, b.dbPass, prefix, environment); err != nil {
		return err
	}
	b.undo = append(b.undo, func() { os.RemoveAll(dir) })
	// Its cache user before WordPress first connects (without it the site
	// just runs uncached: never a reason to fail).
	if err := s.SyncCacheUsers(ctx); err != nil {
		s.Log.Warn("object cache users", "site", st.ID, "err", err)
	}

	report(10, "Starting PHP")
	spec, err := s.specFor(ctx, st)
	if err != nil {
		return err
	}
	// Registered before starting: a failed `docker run` can still leave a
	// created container behind.
	b.undo = append(b.undo, func() { c, cancel := bgCtx(); defer cancel(); s.Runtime.RemoveSite(c, st.ID) })
	if err := s.Runtime.StartReplica(ctx, spec, st.FPMPort); err != nil {
		return err
	}
	// The image entrypoint copies WordPress core into the empty docroot on
	// first start and only then execs PHP-FPM, so "FPM is listening" means
	// the copy is complete. (Waiting for one file to appear raced the copy.)
	return s.waitReady(ctx, []string{runtime.ContainerName(st.ID, st.FPMPort)}, 90*time.Second)
}

// finish writes WPGenie's wrappers, limits the site's database connections
// and puts it live.
func (b *siteBuild) finish(ctx context.Context, report Progress) error {
	s, st := b.s, b.st
	report(90, "Going live")
	if err := s.rewriteManagedFiles(ctx, st.ID); err != nil {
		return err
	}
	// Copied or restored databases were loaded under WordPress's feet.
	if err := s.flushObjectCache(ctx, st.ID); err != nil {
		return err
	}
	if err := s.DB.SetConnectionLimit(ctx, b.dbUser, dbConnLimit(st.Replicas, runtime.FPMMaxChildren(st.MemoryMB))); err != nil {
		return err
	}
	if err := s.Store.SetSiteStatus(ctx, st.ID, store.StatusActive); err != nil {
		return err
	}
	if err := s.Sync(ctx); err != nil {
		return err
	}
	st.Status = store.StatusActive
	b.undo = nil
	s.defaultBackupPolicy(ctx, st)
	return nil
}

// prepareCreate validates a create request and reserves the site.
func (s *Service) prepareCreate(ctx context.Context, in *CreateInput) (*siteBuild, error) {
	domain, err := NormalizeDomain(in.Domain)
	if err != nil {
		return nil, err
	}
	if !emailRe.MatchString(in.AdminEmail) {
		return nil, fmt.Errorf("%w: admin_email", ErrInvalidInput)
	}
	if in.AdminUser == "" {
		in.AdminUser = "wpg_" + randString(6, lowerAlnum) // never the guessable "admin"
	}
	if !userRe.MatchString(in.AdminUser) {
		return nil, fmt.Errorf("%w: admin_user", ErrInvalidInput)
	}
	if in.Name == "" {
		in.Name = domain
	}
	if err := validName(in.Name); err != nil {
		return nil, err
	}
	return s.reserve(ctx, newSite(domain, in.Name))
}

// install provisions a reserved site with a fresh WordPress.
func (s *Service) install(ctx context.Context, b *siteBuild, in CreateInput, report Progress) (_ *Credentials, err error) {
	defer func() {
		if err != nil {
			b.rollback(err)
		}
	}()
	st := b.st
	if err := b.start(ctx, "", "", report); err != nil {
		return nil, err
	}
	report(70, "Installing WordPress")
	creds := &Credentials{
		URL: "https://" + st.PrimaryDomain, AdminURL: "https://" + st.PrimaryDomain + "/wp-admin/",
		Username: in.AdminUser, Password: randString(20, passAlphabet),
	}
	if _, err := s.Runtime.WP(ctx, st.ID, strings.NewReader(creds.Password+"\n"),
		"core", "install", "--url="+creds.URL, "--title="+in.Name,
		"--admin_user="+in.AdminUser, "--admin_email="+in.AdminEmail,
		"--prompt=admin_password", "--skip-email"); err != nil {
		return nil, err
	}
	if err := b.finish(ctx, report); err != nil {
		return nil, err
	}
	return creds, nil
}

// Create provisions a site end to end and returns its admin credentials
// (never stored). StartCreate does the same as a job.
func (s *Service) Create(ctx context.Context, in CreateInput) (*store.Site, *Credentials, error) {
	b, err := s.prepareCreate(ctx, &in)
	if err != nil {
		return nil, nil, err
	}
	creds, err := s.install(ctx, b, in, noProgress)
	if err != nil {
		return nil, nil, err
	}
	return b.st, creds, nil
}

// StartCreate validates the request and reserves the domain right away
// (so "domain taken" is an immediate error), then provisions the site as a
// job. The admin credentials are the job's secret, for whoever started it.
func (s *Service) StartCreate(ctx context.Context, in CreateInput) (*store.Site, int64, error) {
	b, err := s.prepareCreate(ctx, &in)
	if err != nil {
		return nil, 0, err
	}
	st := *b.st
	owner := jobs.OwnerFrom(ctx)
	id, err := s.Jobs.Submit(ctx, s.siteJob(st.ID, "create", false), func(ctx context.Context, t *jobs.Task) error {
		creds, err := s.install(ctx, b, in, t.Progress)
		if err != nil {
			return err
		}
		t.SetSecret(creds, owner)
		t.SetResult(map[string]string{"site_id": st.ID, "url": creds.URL})
		return nil
	})
	if err != nil {
		b.rollback(err)
		return nil, 0, err
	}
	return &st, id, nil
}

// siteJob is a job that holds the site's maintenance lock while it runs,
// so it never overlaps an update, a scan or another job on the same site.
func (s *Service) siteJob(siteID, kind string, heavy bool) jobs.Spec {
	return jobs.Spec{SiteID: siteID, Kind: kind, Heavy: heavy, Lock: jobs.LockFunc(s.maintLock(siteID))}
}

// prepareFiles lays out the site directory:
//
//	<dir>/            root:82  0751   others (Caddy) may traverse, not list
//	<dir>/wp-config.php root:82 0640  readable, NOT writable, by PHP
//	<dir>/public/     82:82    0755   WordPress install (docroot)
//	<dir>/logs/       82:82    0750   PHP's error log (see insights.go)
//
// Keeping wp-config.php outside the docroot and read-only to PHP means a
// compromised plugin can neither leak it over HTTP nor rewrite it. Caddy runs
// as its own user outside group 82, so it reaches public/ but can never read
// wp-config.php, even through a symlink a site plants in its docroot.
func (s *Service) prepareFiles(id, dir, docroot, dbUser, dbPass, prefix, environment string) error {
	if prefix == "" {
		prefix = "wp_" + randString(4, lowerAlnum) + "_"
	}
	cacheUser, cachePass, err := s.cacheCredentials(id)
	if err != nil {
		return err
	}
	cfg, err := renderWPConfig(wpConfigData{
		SiteID: id, DBName: "wp_" + id, DBUser: dbUser, DBPassword: dbPass,
		DBHost: s.Cfg.MariaDBHost, RedisHost: s.Cfg.RedisHost, RedisUser: cacheUser, RedisPassword: cachePass,
		TablePrefix: prefix, Environment: environment,
	})
	if err != nil {
		return err
	}
	return layoutSite(dir, docroot, cfg)
}

// layoutSite creates a site directory (see prepareFiles) with the given
// wp-config.php: a new site's, or on another node the copy of a spread
// site's (same salts, so a login works on every replica).
func layoutSite(dir, docroot string, cfg []byte) error {
	if err := os.MkdirAll(docroot, 0o755); err != nil {
		return err
	}
	if err := ensureLogDir(dir); err != nil {
		return err
	}
	cfgPath := filepath.Join(dir, "wp-config.php")
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, cfg, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return nil // development on a workstation; ownership is best effort
	}
	const wwwData = 82
	for _, c := range []struct {
		path     string
		uid, gid int
		mode     os.FileMode
	}{
		{dir, 0, wwwData, 0o751},
		{cfgPath, 0, wwwData, 0o640},
		{docroot, wwwData, wwwData, 0o755},
	} {
		if err := os.Chown(c.path, c.uid, c.gid); err != nil {
			return err
		}
		if err := os.Chmod(c.path, c.mode); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a site. Traffic is cut first (proxy), then the workload,
// database and files. It keeps going on errors so a half-broken site can
// always be cleaned up, and reports everything that failed.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.deleteLocal(ctx, id, true)
}

// deleteLocal deletes a site from this server. removeSender is false for
// the old copy of a site that moved to another server: its mail sender
// (on the panel's mail server) moved with it.
func (s *Service) deleteLocal(ctx context.Context, id string, removeSender bool) error {
	// Never delete under a running update or scan: it would keep writing
	// snapshots and files for a site that no longer exists.
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return fmt.Errorf("%w: an update or scan is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	// Nor under a reconcile, whose `docker run -v` would recreate the
	// site directory we are about to remove.
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if staging, err := s.Store.StagingOf(ctx, id); err != nil {
		return err
	} else if len(staging) > 0 {
		return fmt.Errorf("%w: delete its staging site (%s) first", ErrConflict, strings.Join(staging, ", "))
	}
	if err := s.Store.DeleteSite(ctx, id); err != nil {
		return err
	}
	if len(st.SpreadNodes) > 0 || len(st.RemoteUpstreams) > 0 {
		// Other servers: not under opsMu (bounded calls, in the background).
		go s.removeGuests(context.WithoutCancel(ctx), st)
	}
	var errs []error
	// SFTP logins and database sessions go with the site (the store
	// dropped their records; the services still have to hear about it).
	if s.SiteRemoved != nil {
		s.SiteRemoved(ctx, id)
	}
	if s.Mailer != nil && removeSender {
		// Unconditionally: a half-finished SetSMTP can leave the sender
		// mailbox behind with smtp still off. A no-op if there is none.
		errs = append(errs, s.Mailer.RemoveSender(ctx, id, st.PrimaryDomain))
	}
	errs = append(errs, s.Sync(ctx))
	errs = append(errs, s.Runtime.RemoveSite(ctx, id))
	errs = append(errs, s.DB.DropSiteDB(ctx, st.DBName, "u_"+id))
	errs = append(errs, os.RemoveAll(s.Cfg.SiteDir(id)))
	// Snapshots hold full database dumps: a deleted site's data must go too.
	// (Backups in restic repositories stay: restoring a deleted site is
	// what they are for. Delete them from the backups view.)
	errs = append(errs, os.RemoveAll(s.snapshotRoot(id)))
	errs = append(errs, os.RemoveAll(s.certDir(id)))
	errs = append(errs, s.Store.DeleteSiteForward(ctx, id))
	// Offloaded uploads stay in the bucket (the operator's); the copy
	// running for the site, if any, stops with its scratch files.
	s.stopOffloadPass(id)
	errs = append(errs, os.RemoveAll(s.offloadWorkDir(id)))
	if err := s.SyncCacheUsers(context.WithoutCancel(ctx)); err != nil {
		s.Log.Warn("object cache users", "site", id, "err", err)
	}
	return errors.Join(errs...)
}

// Sync pushes the current set of active sites to the proxy and refreshes
// the shield's in-memory settings snapshot.
//
// syncMu covers reading the sites and applying them: otherwise a Sync that
// read the store before a reconcile switched upstreams could apply after
// it, pointing Caddy back at replicas that are about to be stopped.
func (s *Service) Sync(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		return err
	}
	certs, err := s.Store.SiteCerts(ctx)
	if err != nil {
		return err
	}
	cdns, err := s.Store.ListCDN(ctx)
	if err != nil {
		return err
	}
	cdnOf := map[string]*store.CDN{}
	for _, c := range cdns {
		cdnOf[c.SiteID] = c
	}
	offloadOf, err := s.offloadURLs(ctx, sites)
	if err != nil {
		return err
	}
	var ps []proxy.Site
	settings := make(map[string]shield.SiteSettings, len(sites))
	// A site that moved here is also served to visitors its old server
	// passes on while DNS catches up (that site only).
	ingress := s.ingress(ctx)
	for _, st := range sites {
		if st.Status == store.StatusSuspended {
			ps = append(ps, suspendedProxySite(st, certs[st.ID] != nil))
			continue
		}
		if st.Status != store.StatusActive {
			continue
		}
		mode := shield.Mode(st.ShieldMode)
		settings[st.ID] = shieldSettings(st)
		// The primary domain first: redirects go to Domains[0].
		domains := append([]string{st.PrimaryDomain}, slices.DeleteFunc(slices.Clone(st.Domains),
			func(d string) bool { return d == st.PrimaryDomain })...)
		ps = append(ps, proxy.Site{
			ID: st.ID, Name: st.Name, Domains: domains, Redirects: st.RedirectDomains, Root: s.Cfg.SiteRoot(st.ID),
			Upstreams: upstreamAddrs(st.Upstreams), ShieldEnabled: mode != shield.ModeOff, BlockXMLRPC: !st.XMLRPC,
			PageCache: st.PageCache, BodyWAF: proxy.WAFMode(st.BodyWAF),
			CustomCert: certs[st.ID] != nil, Staging: st.ParentID != "",
			Images: st.ImageFormats, Forwarded: ingress[st.ID].From != "", Offload: offloadOf[st.ID],
		})
		if len(st.RemoteUpstreams) > 0 {
			// Spread: every replica serves page views; writes stay home.
			p := &ps[len(ps)-1]
			p.HomeUpstreams = p.Upstreams
			p.Upstreams = slices.Clone(p.Upstreams)
			for _, u := range st.RemoteUpstreams {
				p.Upstreams = append(p.Upstreams, "127.0.0.1:"+strconv.Itoa(u.Port))
			}
		}
		if c := cdnOf[st.ID]; c != nil {
			ps[len(ps)-1].AssetCDN = c.AssetHost != ""
			ps[len(ps)-1].EdgeHTML = c.Provider == cdnCloudflare && c.EdgeHTML
		}
	}
	// Sites that moved away: their domains are passed on to the new server.
	fwds, err := s.Store.SiteForwards(ctx)
	if err != nil {
		return err
	}
	for _, f := range fwds {
		ps = append(ps, proxy.Site{ID: f.SiteID, Name: "moved to " + f.NodeID, Domains: f.Domains,
			Forward: "127.0.0.1:" + strconv.Itoa(f.Port), ForwardHTTP: "127.0.0.1:" + strconv.Itoa(f.HTTPPort)})
	}
	if s.Webmail != nil {
		if host, up := s.Webmail(); host != "" {
			settings["webmail"] = shield.SiteSettings{ID: "webmail", Mode: shield.ModeStandard, Inspect: true, Webmail: true}
			ps = append(ps, proxy.Site{ID: "webmail", Name: "Webmail (Roundcube)", Domains: []string{host},
				Proxy: up, ShieldEnabled: true})
		}
	}
	// Publish shield settings before the proxy starts routing to new sites.
	s.shieldSt.Store(&settings)
	return s.Proxy.Apply(ctx, ps)
}

// ShieldLookup serves shield settings from an atomic snapshot: the hot path
// (every dynamic request) never touches SQLite or takes a lock.
func (s *Service) ShieldLookup(id string) (shield.SiteSettings, bool) {
	m := s.shieldSt.Load()
	if m == nil {
		return shield.SiteSettings{}, false
	}
	st, ok := (*m)[id]
	return st, ok
}
