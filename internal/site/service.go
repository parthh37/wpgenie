// Package site orchestrates the lifecycle of a WordPress site across the
// panel store, MariaDB, the container runtime and the reverse proxy.
package site

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/domain"
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
	Mailer  Mailer
	// Webmail returns the webmail host and upstream to publish, or "".
	Webmail func() (host, upstream string)
	// CDN purges Cloudflare's cache; CDNRanges are its edge networks; DNS
	// (nil: system resolver) checks whether domains go through it.
	CDN       CDNProvider
	CDNRanges *cdn.Ranges
	DNS       Resolver
	Log       *slog.Logger

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

// Create provisions a site end to end. Every completed step registers an
// undo; if a later step fails, the undos run in reverse so a failed create
// leaves nothing behind and the domain can be retried.
func (s *Service) Create(ctx context.Context, in CreateInput) (_ *store.Site, _ *Credentials, err error) {
	domain, err := NormalizeDomain(in.Domain)
	if err != nil {
		return nil, nil, err
	}
	if !emailRe.MatchString(in.AdminEmail) {
		return nil, nil, fmt.Errorf("%w: admin_email", ErrInvalidInput)
	}
	if in.AdminUser == "" {
		in.AdminUser = "wpg_" + randString(6, lowerAlnum) // never the guessable "admin"
	}
	if !userRe.MatchString(in.AdminUser) {
		return nil, nil, fmt.Errorf("%w: admin_user", ErrInvalidInput)
	}
	if in.Name == "" {
		in.Name = domain
	}

	s.opsMu.Lock()
	defer s.opsMu.Unlock()

	if s.Webmail != nil {
		if host, _ := s.Webmail(); host == domain {
			return nil, nil, fmt.Errorf("%w: it is the mail server's hostname", ErrDomainTaken)
		}
	}
	if taken, err := s.Store.DomainExists(ctx, domain); err != nil {
		return nil, nil, err
	} else if taken {
		return nil, nil, ErrDomainTaken
	}
	ports, err := s.Store.AllocatePorts(ctx, s.Cfg.SitePortBase, 1)
	if err != nil {
		return nil, nil, err
	}
	port := ports[0]

	id := "s" + randString(7, lowerAlnum)
	st := &store.Site{
		ID: id, Name: in.Name, PrimaryDomain: domain, PHPVersion: "8.3", FPMPort: port,
		DBName: "wp_" + id, Status: store.StatusProvisioning,
		ShieldMode: string(shield.ModeStandard), BlockAIBots: true, WAF: true,
		MemoryMB: defaultMemoryMB, CPUs: defaultCPUs, Replicas: 1,
		PageCache: true, ObjectCache: true, Upstreams: []int{port},
	}
	dbUser, dbPass := "u_"+id, randString(32, passAlphabet)
	dir, docroot := s.Cfg.SiteDir(id), s.Cfg.SiteRoot(id)

	var undo []func()
	defer func() {
		if err == nil {
			return
		}
		s.Log.Error("site create failed, rolling back", "site", id, "err", err)
		// Use a fresh context: the request context may be what failed.
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()
	bg := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), time.Minute)
	}

	if err = s.Store.CreateSite(ctx, st); err != nil {
		return nil, nil, err
	}
	undo = append(undo, func() { c, cancel := bg(); defer cancel(); s.Store.DeleteSite(c, id) })

	if err = s.DB.CreateSiteDB(ctx, st.DBName, dbUser, dbPass); err != nil {
		return nil, nil, err
	}
	undo = append(undo, func() { c, cancel := bg(); defer cancel(); s.DB.DropSiteDB(c, st.DBName, dbUser) })

	if err = s.prepareFiles(id, dir, docroot, dbUser, dbPass); err != nil {
		return nil, nil, err
	}
	undo = append(undo, func() { os.RemoveAll(dir) })

	spec, err := s.specFor(ctx, st)
	if err != nil {
		return nil, nil, err
	}
	// Registered before starting: a failed `docker run` can still leave a
	// created container behind.
	undo = append(undo, func() { c, cancel := bg(); defer cancel(); s.Runtime.RemoveSite(c, id) })
	if err = s.Runtime.StartReplica(ctx, spec, port); err != nil {
		return nil, nil, err
	}

	// The image entrypoint copies WordPress core into the empty docroot on
	// first start and only then execs PHP-FPM, so "FPM is listening" means
	// the copy is complete. (Waiting for one file to appear raced the copy.)
	if err = s.waitReady(ctx, []string{runtime.ContainerName(id, port)}, 90*time.Second); err != nil {
		return nil, nil, err
	}

	creds := &Credentials{
		URL: "https://" + domain, AdminURL: "https://" + domain + "/wp-admin/",
		Username: in.AdminUser, Password: randString(20, passAlphabet),
	}
	_, err = s.Runtime.WP(ctx, id, strings.NewReader(creds.Password+"\n"),
		"core", "install", "--url="+creds.URL, "--title="+in.Name,
		"--admin_user="+in.AdminUser, "--admin_email="+in.AdminEmail,
		"--prompt=admin_password", "--skip-email")
	if err != nil {
		return nil, nil, err
	}

	if err = s.writeCacheFiles(id, CacheSettings{PageCache: st.PageCache, ObjectCache: st.ObjectCache}); err != nil {
		return nil, nil, err
	}
	if err = s.DB.SetConnectionLimit(ctx, dbUser, dbConnLimit(st.Replicas, spec.MaxChildren)); err != nil {
		return nil, nil, err
	}
	if err = s.Store.SetSiteStatus(ctx, id, store.StatusActive); err != nil {
		return nil, nil, err
	}
	if err = s.Sync(ctx); err != nil {
		return nil, nil, err
	}
	st.Status = store.StatusActive
	st.Domains = []string{domain}
	return st, creds, nil
}

// prepareFiles lays out the site directory:
//
//	<dir>/            root:82  0751   others (Caddy) may traverse, not list
//	<dir>/wp-config.php root:82 0640  readable, NOT writable, by PHP
//	<dir>/public/     82:82    0755   WordPress install (docroot)
//
// Keeping wp-config.php outside the docroot and read-only to PHP means a
// compromised plugin can neither leak it over HTTP nor rewrite it. Caddy runs
// as its own user outside group 82, so it reaches public/ but can never read
// wp-config.php, even through a symlink a site plants in its docroot.
func (s *Service) prepareFiles(id, dir, docroot, dbUser, dbPass string) error {
	if err := os.MkdirAll(docroot, 0o755); err != nil {
		return err
	}
	cfg, err := renderWPConfig(wpConfigData{
		SiteID: id, DBName: "wp_" + id, DBUser: dbUser, DBPassword: dbPass,
		DBHost: s.Cfg.MariaDBHost, RedisHost: s.Cfg.RedisHost,
		TablePrefix: "wp_" + randString(4, lowerAlnum) + "_",
	})
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(dir, "wp-config.php")
	if err := os.WriteFile(cfgPath, cfg, 0o640); err != nil {
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
	if err := s.Store.DeleteSite(ctx, id); err != nil {
		return err
	}
	var errs []error
	if s.Mailer != nil {
		// Unconditionally: a half-finished SetSMTP can leave the sender
		// mailbox behind with smtp still off. A no-op if there is none.
		errs = append(errs, s.Mailer.RemoveSender(ctx, id, st.PrimaryDomain))
	}
	errs = append(errs, s.Sync(ctx))
	errs = append(errs, s.Runtime.RemoveSite(ctx, id))
	errs = append(errs, s.DB.DropSiteDB(ctx, st.DBName, "u_"+id))
	errs = append(errs, os.RemoveAll(s.Cfg.SiteDir(id)))
	// Snapshots hold full database dumps: a deleted site's data must go too.
	errs = append(errs, os.RemoveAll(s.snapshotRoot(id)))
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
	var ps []proxy.Site
	settings := make(map[string]shield.SiteSettings, len(sites))
	for _, st := range sites {
		if st.Status != store.StatusActive {
			continue
		}
		mode := shield.Mode(st.ShieldMode)
		settings[st.ID] = shield.SiteSettings{ID: st.ID, Mode: mode, BlockAIBots: st.BlockAIBots, Inspect: st.WAF,
			AdminAllow: mustPrefixes(st.AdminAllow), Trusted: mustPrefixes(st.TrustedIPs)}
		ps = append(ps, proxy.Site{
			ID: st.ID, Name: st.Name, Domains: st.Domains, Root: s.Cfg.SiteRoot(st.ID),
			Upstreams: upstreamAddrs(st.Upstreams), ShieldEnabled: mode != shield.ModeOff, BlockXMLRPC: true,
			PageCache: st.PageCache,
		})
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
