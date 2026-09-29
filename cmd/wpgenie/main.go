// Command wpgenie is the WPGenie control plane daemon and its CLI client.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/parthh37/wpgenie/internal/adminer"
	"github.com/parthh37/wpgenie/internal/analytics"
	"github.com/parthh37/wpgenie/internal/api"
	"github.com/parthh37/wpgenie/internal/backup"
	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/dbprov"
	"github.com/parthh37/wpgenie/internal/iprep"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/sftp"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/updater"
)

var version = "dev" // set by -ldflags at release time

const usage = `WPGenie — secure, efficient WordPress hosting control panel

Usage:
  wpgenie serve                         run the control plane daemon
  wpgenie site ls                       list sites
  wpgenie site create <domain> <email>  create a WordPress site
  wpgenie site rm <site-id>             delete a site (irreversible)
  wpgenie site scale <site-id> [--memory MB] [--cpus N] [--replicas N]
                                        resize a site with no downtime; with no
                                        flags, rolls it onto the current PHP image
  wpgenie site cache <site-id> [--page on|off] [--object on|off]
                                        toggle the page / object cache
  wpgenie site purge <site-id>          empty the site's caches
  wpgenie site autoscale <site-id> [--on|--off] [--min N] [--max N] [--target PCT]
                                        scale replicas with CPU use
  wpgenie site shield <site-id> [--mode off|standard|under_attack] [--waf on|off]
                          [--body-waf off|detect|block] [--xmlrpc on|off]
                          [--admin-allow IP/CIDR,...] [--trusted IP/CIDR,...] [--deny IP/CIDR,...]
                          [--reputation off|challenge|block] [--country-mode off|block|allow]
                          [--countries CC,...] [--country-action block|challenge]
                          [--rate N] [--burst N] [--login-rate N] [--difficulty BITS]
  wpgenie site updates <site-id>        list WordPress core/plugin/theme updates
  wpgenie site update <site-id> [--all] [--core] [--plugins a,b] [--themes c]
                                        snapshot, update, health-check, roll back on failure
  wpgenie site auto-update <site-id> off|security|all
  wpgenie site scan <site-id>           security scan (vulnerabilities, file integrity)
  wpgenie site plugins <site-id> [--now] [--json]
                                        plugin analysis: wordpress.org status, abandoned,
                                        modified/nulled files, cost per plugin
  wpgenie site smtp <site-id> on|off    send WordPress mail through the mail server
  wpgenie site cdn <site-id> [status|cloudflare|off|purge]
                                        Cloudflare cache purging; "cloudflare" reads
                                        the API token from stdin
  wpgenie site events <site-id>         activity log (autoscaling, updates, scans)
  wpgenie site backup <site-id> [now|ls|restore <repo> <backup> [--files-only|--db-only]
                          |download <repo> <backup> [file]|rm <repo> <backup>|policy [flags]]
                                        restic backups: files + database, deduplicated, encrypted
  wpgenie site staging <site-id> [domain]
                                        clone into a staging site (default staging.<domain>)
  wpgenie site push <staging-id> [--files code|all] [--db [--tables t1,t2]]
                                        push staging to its live site (backed up first)
  wpgenie site domain <site-id> add <domain> [--redirect] | rm <domain>
                          | redirect <domain> on|off | primary <domain>
                                        aliases, www <-> apex redirects, primary domain
  wpgenie site cert <site-id> [show | set <cert.pem> <key.pem> | rm]
                                        the site's own TLS certificate
  wpgenie site php <site-id> [--version 8.2|8.3|8.4] [--memory-limit MB] [--upload-max MB]
                          [--max-execution-time S] [--max-input-vars N]
  wpgenie site sftp <site-id> [ls | add [--suffix NAME] [--password] [--key FILE] | rm <login>
                          | passwd <login> | nopasswd <login> | keys <login> <file>]
  wpgenie site adminer <site-id>        one-time link to Adminer on the site's database
  wpgenie jobs [<job-id>] [--site ID] [--active]
                                        long operations (create, backups, restores, clones)
  wpgenie backup repos | repo add local|s3|b2|sftp ... | repo check|rm|password <repo>
  wpgenie backup ls <repo> | restore-new <repo> <backup> <domain>
                                        backup destinations; restore any backup as a new site
  wpgenie mail enable <hostname> | disable | status
  wpgenie mail domain add|rm|dns <domain>
  wpgenie mail box add <address> [--quota MB] | passwd <address> | rm <address> | ls
  wpgenie mail alias add|rm <alias> <target>
  wpgenie security bans | unban <ip> | ban <ip> [--hours N]
  wpgenie security allow|deny [IP/CIDR,... | none]
                                        server-wide lists (every site)
  wpgenie security reputation [refresh] IP blocklists and country database status
  wpgenie user ls | add <name> [--role admin|operator|viewer] | role <name> <role>
  wpgenie user disable|enable|passwd|reset-2fa|rm <name> | require-2fa on|off
                                        panel accounts (sign in to the dashboard)
  wpgenie audit [--limit N] [--user NAME] who changed what, from where
  wpgenie update [check|status]         update WPGenie to the latest signed release
                                        (verified, health-checked, rolled back on failure)
  wpgenie version

Flags:
  -config path   config file (default /etc/wpgenie/config.json, or $WPGENIE_CONFIG)
`

func main() {
	fs := flag.NewFlagSet("wpgenie", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	if args[0] == "version" {
		fmt.Println("wpgenie", version)
		return
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	switch args[0] {
	case "serve":
		err = serve(cfg)
	case "site":
		err = siteCmd(cfg, args[1:])
	case "update":
		err = updateCmd(cfg, args[1:])
	case "mail":
		err = mailCmd(cfg, args[1:])
	case "security":
		err = securityCmd(cfg, args[1:])
	case "user":
		err = userCmd(cfg, args[1:])
	case "audit":
		err = auditCmd(cfg, args[1:])
	case "jobs":
		err = jobsCmd(cfg, args[1:])
	case "backup":
		err = backupCmd(cfg, args[1:])
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func serve(cfg *config.Config) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.SitesDir(), 0o755); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	db, err := dbprov.Open(cfg.MariaDBDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	rootDSN, err := mysql.ParseDSN(cfg.MariaDBDSN)
	if err != nil {
		return err
	}
	// Proves the daemon's own health checks to the shield (per process).
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	healthToken := hex.EncodeToString(tok)
	if n, err := st.FailInterruptedUpdates(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("marked updates interrupted by the last shutdown as failed", "count", n)
	}
	if n, err := st.FailInterruptedJobs(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("marked jobs interrupted by the last shutdown as failed", "count", n)
	}
	jobQueue := &jobs.Queue{Store: st, Log: log, Heavy: cfg.JobConcurrency}
	// restic's cache (and the database dumps being backed up) are root-only.
	for _, d := range []string{filepath.Join(cfg.DataDir, "backups", "cache"), filepath.Join(cfg.DataDir, "backups", "staging")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	docker := &runtime.Docker{}
	cfRanges := cdn.NewRanges()
	svc := &site.Service{
		Cfg: cfg, Store: st, Runtime: &runtime.Docker{}, DB: db, Log: log,
		// Container names equal the hostnames sites use (deploy/docker-compose.yml).
		Cache:     &runtime.Valkey{Container: cfg.RedisHost},
		Dumper:    &runtime.MariaDB{Container: cfg.MariaDBHost, Password: rootDSN.Passwd},
		Prober:    &site.HTTPProber{Token: healthToken},
		Vulns:     &site.WPVulnerability{UserAgent: "WPGenie/" + version + " (+https://github.com/parthh37/wpgenie)"},
		Directory: &site.WordPressOrg{UserAgent: "WPGenie/" + version + " (+https://github.com/parthh37/wpgenie)"},
		CDN:       &cdn.Cloudflare{},
		CDNRanges: cfRanges,
		Jobs:      jobQueue,
		Backups: &backup.Restic{Docker: docker, Image: cfg.ResticImage,
			CacheDir: filepath.Join(cfg.DataDir, "backups", "cache")},
		Images:  docker,
		Version: version,
		Proxy: proxy.NewCaddy(proxy.Config{
			ACMEEmail: cfg.ACMEEmail, AdminURL: cfg.CaddyAdmin, PanelDomain: cfg.PanelDomain,
			PanelUpstream: cfg.ListenAddr, ShieldUpstream: cfg.ListenAddr,
			AccessLog: cfg.AccessLog, CaddyfilePath: cfg.CaddyfilePath,
			CloudflareRanges: cfRanges.Get,
		}),
	}
	if err := svc.LoadCDNRanges(ctx); err != nil {
		log.Warn("using the built-in Cloudflare IP ranges", "err", err)
	}
	mailSvc := &mail.Service{
		Cfg: mail.Config{DataDir: filepath.Join(cfg.DataDir, "mail"), CaddyDataDir: cfg.CaddyDataDir,
			Network: cfg.DockerNetwork, MailImage: cfg.MailImage, WebmailImage: cfg.WebmailImage, WebmailPort: cfg.WebmailPort},
		Store: st, Docker: &runtime.Docker{}, Sync: svc.Sync, Log: log,
	}
	if err := mailSvc.PrepareDirs(); err != nil {
		return err
	}
	if err := mailSvc.Load(ctx); err != nil {
		return err
	}
	svc.Mailer, svc.Webmail = mailSvc, mailSvc.Webmail
	go mailSvc.Run(ctx)

	// SFTP (one chrooted OpenSSH server for every site) and Adminer (on
	// demand, on sites' own domains).
	sftpSvc := &sftp.Service{Store: st, Docker: docker, Log: log, Cfg: sftp.Config{
		DataDir: filepath.Join(cfg.DataDir, "sftp"), SitesDir: cfg.SitesDir(), Image: cfg.SFTPImage,
		ImageDir: filepath.Join(cfg.ImagesDir, "sftp"), Port: cfg.SFTPPort}}
	go func() {
		if err := sftpSvc.Reconcile(ctx); err != nil {
			log.Error("sftp: starting the server", "err", err)
		}
	}()
	adminerSvc := &adminer.Service{Store: st, Docker: docker, Accounts: db, Log: log, Cfg: adminer.Config{
		Image: cfg.AdminerImage, ImageDir: filepath.Join(cfg.ImagesDir, "adminer"), Port: cfg.AdminerPort,
		Network: cfg.DockerNetwork, DBHost: cfg.MariaDBHost}}
	go adminerSvc.Run(ctx)
	svc.SiteRemoved = func(ctx context.Context, id string) {
		sftpSvc.SiteRemoved(ctx, id)
		adminerSvc.SiteRemoved(ctx, id)
	}

	// IP reputation: blocklists (saved, so a restart without network keeps
	// them) and the country database, downloaded once a site uses it.
	repDir := filepath.Join(cfg.DataDir, "iprep")
	lists := &iprep.Lists{Dir: repDir, Feeds: iprep.DefaultFeeds, Log: log}
	lists.Load()
	countries := &iprep.Countries{Dir: repDir, Log: log, Needed: svc.CountryRulesInUse}
	if err := countries.Load(); err != nil {
		log.Warn("country database unreadable; it will be downloaded again", "err", err)
	}
	go lists.Run(ctx, 6*time.Hour)
	go countries.Run(ctx)

	sh := shield.New(shield.Options{Secret: []byte(cfg.ShieldSecret), Sites: svc.ShieldLookup, Logger: log,
		HealthToken: healthToken, Reputation: &iprep.Reputation{Lists: lists, Countries: countries}})
	if g, err := svc.GlobalLists(ctx); err != nil {
		return err
	} else {
		sh.SetGlobal(g.Shield())
	}
	go sh.Run(ctx)

	// Request-body WAF matches (Coraza in Caddy) join the security log.
	wafLog := &proxy.WAFLog{Path: filepath.Join(filepath.Dir(cfg.AccessLog), "waf.log"), Log: log,
		Handle: func(events []proxy.WAFEvent) {
			idx, err := st.DomainIndex(ctx)
			if err != nil {
				log.Warn("WAF events: domain index", "err", err)
				return
			}
			for _, e := range events {
				siteID, ok := idx[e.Host]
				if !ok {
					continue
				}
				verdict := "detect"
				if e.Blocked {
					verdict = "block"
				}
				sh.Record(shield.Event{Time: e.Time, Site: siteID, IP: e.IP, Verdict: verdict, Reason: e.Reason(), Path: e.Path})
			}
		}}
	go wafLog.Run(ctx)

	// Caddy may still be starting (both come up at boot); retry the first sync.
	go func() {
		for delay := time.Second; ; delay = min(delay*2, 30*time.Second) {
			if err := svc.Sync(ctx); err == nil {
				log.Info("proxy synced")
				return
			} else {
				log.Warn("initial proxy sync failed, retrying", "err", err, "in", delay)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()

	go svc.RunCron(ctx)
	go svc.RunAutoscaler(ctx)
	go svc.RunMaintenance(ctx)
	go svc.RunCDN(ctx)
	go svc.RunCDNRanges(ctx)
	go svc.RunBackups(ctx)

	upd := &updater.Updater{Current: version, Repo: cfg.UpdateRepo, StateDir: filepath.Join(cfg.DataDir, "updates")}
	go upd.Run(ctx, 12*time.Hour)

	ing := &analytics.Ingester{Path: cfg.AccessLog, Store: st, Secret: []byte(cfg.ShieldSecret), Logger: log}
	go ing.Run(ctx)

	srv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: (&api.Server{Token: cfg.APIToken, Version: version, Sites: svc, Store: st, Shield: sh,
			Updater: upd, Mail: mailSvc, Jobs: jobQueue, SFTP: sftpSvc, Adminer: adminerSvc,
			Lists: lists, Countries: countries, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Scaling runs synchronously and can take minutes (long operations
		// are jobs; backup downloads extend their own deadline).
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("wpgenie listening", "addr", cfg.ListenAddr, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	// Within systemd's default 90 s stop timeout.
	if !svc.WaitUpdates(time.Minute) {
		log.Warn("stopping with a WordPress update still running; it will be marked interrupted")
	}
	if !jobQueue.Wait(20 * time.Second) {
		log.Warn("stopping with jobs still running; they will be marked interrupted")
	}
	return err
}

// siteCmd is a thin client of the local API, so the CLI and the dashboard
// always go through the same validation and code paths.
func siteCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: wpgenie site ls|create|rm|scale|cache|purge|autoscale|shield|updates|update|auto-update|scan|" +
			"plugins|smtp|cdn|events|backup|staging|push|domain|cert|php|sftp|adminer")
	}
	switch args[0] {
	case "ls":
		var sites []store.Site
		if err := call(cfg, "GET", "/sites", nil, &sites); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tDOMAIN\tSTATUS\tSHIELD\tAI-BLOCK\tREPLICAS\tMEMORY\tCPUS\tPAGE-CACHE\tOBJ-CACHE")
		for _, s := range sites {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%d\t%dM\t%g\t%v\t%v\n", s.ID, s.PrimaryDomain, s.Status,
				s.ShieldMode, s.BlockAIBots, s.Replicas, s.MemoryMB, s.CPUs, s.PageCache, s.ObjectCache)
		}
		return w.Flush()
	case "create":
		return createSiteCmd(cfg, args[1:])
	case "rm":
		if len(args) != 2 {
			return errors.New("usage: wpgenie site rm <site-id>")
		}
		return call(cfg, "DELETE", "/sites/"+args[1], nil, nil)
	case "scale":
		return scaleCmd(cfg, args[1:])
	case "cache":
		return cacheCmd(cfg, args[1:])
	case "autoscale":
		return autoscaleCmd(cfg, args[1:])
	case "shield":
		return shieldCmd(cfg, args[1:])
	case "updates", "update", "auto-update", "scan", "plugins", "smtp", "events", "cdn":
		return siteOpsCmd(cfg, args[0], args[1:])
	case "php":
		return phpCmd(cfg, args[1:])
	case "backup", "staging", "push", "domain", "cert", "sftp", "adminer":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("usage: wpgenie site %s <site-id> ...", args[0])
		}
		id, rest := args[1], args[2:]
		switch args[0] {
		case "backup":
			return siteBackupCmd(cfg, id, rest)
		case "staging":
			return stagingCmd(cfg, id, rest)
		case "push":
			return pushCmd(cfg, id, rest)
		case "domain":
			return domainCmd(cfg, id, rest)
		case "cert":
			return certCmd(cfg, id, rest)
		case "sftp":
			return sftpCmd(cfg, id, rest)
		default:
			return adminerCmd(cfg, id)
		}
	case "purge":
		if len(args) != 2 {
			return errors.New("usage: wpgenie site purge <site-id>")
		}
		if err := call(cfg, "POST", "/sites/"+args[1]+"/cache/purge", nil, nil); err != nil {
			return err
		}
		fmt.Println("Caches purged.")
		return nil
	}
	return fmt.Errorf("unknown site command %q", args[0])
}

// siteFlags parses "<site-id> [flags]", starting from the site's current
// settings so unspecified flags keep their values.
func siteFlags(cfg *config.Config, args []string, usage string, define func(*flag.FlagSet, *store.Site)) (*store.Site, error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return nil, errors.New(usage)
	}
	var st store.Site
	if err := call(cfg, "GET", "/sites/"+args[0], nil, &st); err != nil {
		return nil, err
	}
	fs := flag.NewFlagSet("site", flag.ContinueOnError)
	define(fs, &st)
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return nil, errors.New(usage)
	}
	return &st, nil
}

func scaleCmd(cfg *config.Config, args []string) error {
	st, err := siteFlags(cfg, args, "usage: wpgenie site scale <site-id> [--memory MB] [--cpus N] [--replicas N]",
		func(fs *flag.FlagSet, st *store.Site) {
			fs.IntVar(&st.MemoryMB, "memory", st.MemoryMB, "memory per replica, in MB")
			fs.Float64Var(&st.CPUs, "cpus", st.CPUs, "CPU cores per replica")
			fs.IntVar(&st.Replicas, "replicas", st.Replicas, "number of PHP-FPM containers")
		})
	if err != nil {
		return err
	}
	var out store.Site
	in := site.Resources{MemoryMB: st.MemoryMB, CPUs: st.CPUs, Replicas: st.Replicas}
	if err := call(cfg, "PUT", "/sites/"+st.ID+"/resources", in, &out); err != nil {
		return err
	}
	fmt.Printf("Site %s: %d replica(s) × %d MB / %g CPU, serving on ports %v\n",
		out.ID, out.Replicas, out.MemoryMB, out.CPUs, out.Upstreams)
	return nil
}

func cacheCmd(cfg *config.Config, args []string) error {
	var page, object onOff
	st, err := siteFlags(cfg, args, "usage: wpgenie site cache <site-id> [--page on|off] [--object on|off]",
		func(fs *flag.FlagSet, st *store.Site) {
			page, object = onOff(st.PageCache), onOff(st.ObjectCache)
			fs.Var(&page, "page", "full-page cache (on|off)")
			fs.Var(&object, "object", "Redis object cache (on|off)")
		})
	if err != nil {
		return err
	}
	var out store.Site
	in := site.CacheSettings{PageCache: bool(page), ObjectCache: bool(object)}
	if err := call(cfg, "PUT", "/sites/"+st.ID+"/cache", in, &out); err != nil {
		return err
	}
	fmt.Printf("Site %s: page cache %v, object cache %v\n", out.ID, onOff(out.PageCache), onOff(out.ObjectCache))
	return nil
}

type onOff bool

func (v onOff) String() string {
	if v {
		return "on"
	}
	return "off"
}

func (v *onOff) Set(s string) error {
	switch s {
	case "on", "true", "1":
		*v = true
	case "off", "false", "0":
		*v = false
	default:
		return errors.New("want on or off")
	}
	return nil
}

func call(cfg *config.Config, method, path string, body, out any) error {
	return callCtx(context.Background(), cfg, 5*time.Minute, method, path, body, out)
}

func callCtx(ctx context.Context, cfg *config.Config, timeout time.Duration, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+cfg.ListenAddr+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("is the daemon running? %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func updateCmd(cfg *config.Config, args []string) error {
	sub := "now"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "now":
		var out struct{ Version string }
		if err := call(cfg, "POST", "/system/update", nil, &out); err != nil {
			return err
		}
		fmt.Printf("Installing %s. WPGenie restarts when it is ready; follow with `wpgenie update status`.\n", out.Version)
		return nil
	case "check", "status":
		var in updater.Info
		method, path := "GET", "/system"
		if sub == "check" {
			method, path = "POST", "/system/update/check"
		}
		if err := call(cfg, method, path, nil, &in); err != nil {
			return err
		}
		fmt.Printf("Running:  %s\n", in.Current)
		switch {
		case in.CheckErr != "":
			fmt.Printf("Latest:   unknown (%s)\n", in.CheckErr)
		case in.Latest != nil:
			fmt.Printf("Latest:   %s (%s)\n", in.Latest.Version, in.Latest.URL)
		}
		if in.Available {
			fmt.Println("An update is available: run `wpgenie update`.")
		}
		if !in.Signed {
			fmt.Println("This build has no release signing key: self-update is disabled.")
		}
		if st := in.Status; st != nil {
			fmt.Printf("Last update: %s → %s: %s (%s)\n", st.From, st.To, st.Phase, st.Message)
		}
		return nil
	case "apply":
		if len(args) != 2 {
			return errors.New("usage: wpgenie update apply <staged-dir> (run by systemd, not by hand)")
		}
		return applyUpdate(cfg, args[1])
	}
	return fmt.Errorf("unknown update command %q", sub)
}

// applyUpdate is the self-update applier, started by the daemon as a
// transient systemd unit. It is a copy of the old binary, so version is
// the version being replaced.
func applyUpdate(cfg *config.Config, staged string) error {
	paths := updater.DefaultPaths()
	paths.StateDir = filepath.Join(cfg.DataDir, "updates")
	a := &updater.Applier{
		Staged: staged, From: version, Paths: paths, PHPImage: cfg.PHPImage, CaddyImage: cfg.CaddyImage,
		Run: updater.ExecRun, Log: os.Stderr,
		Version: func(ctx context.Context) (string, error) {
			var out struct{ Version string }
			err := callCtx(ctx, cfg, 5*time.Second, "GET", "/system/version", nil, &out)
			return out.Version, err
		},
		RollSites: func(ctx context.Context) error {
			return callCtx(ctx, cfg, 30*time.Second, "POST", "/system/roll-sites", nil, nil)
		},
	}
	return a.Apply(context.Background())
}
