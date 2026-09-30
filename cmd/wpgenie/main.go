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
	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/cdn"
	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/dbprov"
	"github.com/parthh37/wpgenie/internal/iprep"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/monitor"
	"github.com/parthh37/wpgenie/internal/offload"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/sftp"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
	"github.com/parthh37/wpgenie/internal/updater"
	"github.com/parthh37/wpgenie/internal/wplogin"
)

var version = "dev" // set by -ldflags at release time

const usage = `WPGenie — secure, efficient WordPress hosting control panel

Usage:
  wpgenie serve                         run the control plane daemon
  wpgenie site ls                       list sites
  wpgenie site create <domain> <email> [--node ID]
                                        create a WordPress site (on a chosen server)
  wpgenie site rm <site-id>             delete a site (irreversible)
  wpgenie site scale <site-id> [--memory MB] [--cpus N] [--replicas N]
                                        resize a site with no downtime; with no
                                        flags, rolls it onto the current PHP image
  wpgenie site cache <site-id> [--page on|off] [--object on|off] [--mobile on|off]
                                        toggle the page / object cache; --mobile keeps
                                        separate mobile copies of every page
  wpgenie site purge <site-id>          empty the site's caches
  wpgenie site autoscale <site-id> [--on|--off] [--min N] [--max N] [--target PCT]
                          [--target-workers PCT] [--target-ms MS]
                                        scale replicas with CPU use, PHP workers busy
                                        (queue included) and response time (0: off)
  wpgenie site images <site-id> [status|avif,webp|webp|off|convert]
                                        serve uploads as AVIF/WebP (converted copies)
  wpgenie site insights <site-id> [--hours N] [--json] [--clear-errors]
                                        response times, cache hits, slow URLs, PHP errors
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
  wpgenie site cdn <site-id> [status|cloudflare [--edge-html on|off]|bunny <zone-id> <cdn-host>
                          |generic <cdn-host>|off|purge]
                                        Cloudflare (purges, optional edge caching of pages)
                                        or a pull zone for static files; tokens/keys on stdin
  wpgenie site offload <site-id> [status | on --endpoint URL --bucket B --public-url URL [--region R]
                          [--prefix P] [--access-key-id ID] [--local-days N] [--acl public-read|none]
                          | off [--force] | sync | download]
                                        copy uploads to S3-compatible storage, serve missing ones
                                        from its public URL; secret key on stdin
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
  wpgenie site wp <site-id> login [user-id] | users | password <user-id>
                                        one-time link into wp-admin (no WordPress password),
                                        administrators, reset an administrator's password
  wpgenie site optimize <site-id> [ls | recommended | off | key,key,... | cleanup]
                                        WordPress performance tweaks; clean the database now
  wpgenie site analyse <site-id> [--fix <fix>]
                                        security, performance and upkeep report; apply a fix
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
  wpgenie branding [show | set [--name NAME] [--url URL] [--logo FILE|none]]
                                        your brand in WordPress's admin instead of WordPress's
  wpgenie user ls | add <name> [--role admin|operator|viewer] | role <name> <role>
  wpgenie user disable|enable|passwd|reset-2fa|rm <name> | require-2fa on|off
                                        panel accounts (sign in to the dashboard)
  wpgenie audit [--limit N] [--user NAME] who changed what, from where
  wpgenie site move <site-id> <node>    move a site to another server (a job; brief
                                        maintenance page during the final copy)
  wpgenie site move --finish <site-id>  DNS points at the new server: delete the old copy
  wpgenie site spread <site-id> <node,...> | none
                                        also run the site's replicas on other servers
  wpgenie node ls | add <name> <address> <pairing-code> [--public-ip IP] | rm <id> [--force]
  wpgenie node drain|activate|update <id>
                                        servers of a cluster (on the panel)
  wpgenie agent [run | pair-code | status]
                                        run this server as a node of a cluster
  wpgenie alerts [--history N] [--json] | test | metrics-token
                                        firing alerts and history; test the notification
                                        channels; new Prometheus scrape token (shown once)
  wpgenie account ls | show|set|suspend|unsuspend|terminate|rm|usage|users|user-add|sso <id> ...
                          | create <name> --plan P [--kind customer|reseller] [--parent ID] [--user NAME]
                          | assign <site-id> <account-id|0>
                                        customer and reseller accounts (run wpgenie account for flags)
  wpgenie plan ls | show|create|set|rm <id> [flags]
                                        plans: sites, disk, bandwidth, per-site resources, features
  wpgenie token ls | create --user NAME [--name N] [--expires-days N] | rm <id>
                                        per-user API tokens (WHMCS, scripts); shown once
  wpgenie update [check|status]         update WPGenie to the latest signed release
                                        (verified, health-checked, rolled back on failure)
  wpgenie store migrate-to-postgres [<postgres-url> | -]
                                        copy the panel database (SQLite) into an empty
                                        PostgreSQL database; daemon stopped; URL on stdin with -
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
	if args[0] == "link" { // inside a link container: no config file
		if err := linkMain(args[1:]); err != nil {
			fatal(err)
		}
		return
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	switch args[0] {
	case "serve":
		err = serve(cfg, false)
	case "agent":
		err = agentCmd(cfg, args[1:])
	case "node":
		err = nodeCmd(cfg, args[1:])
	case "site":
		err = siteCmd(cfg, args[1:])
	case "update":
		err = updateCmd(cfg, args[1:])
	case "mail":
		err = mailCmd(cfg, args[1:])
	case "security":
		err = securityCmd(cfg, args[1:])
	case "branding":
		err = brandingCmd(cfg, args[1:])
	case "user":
		err = userCmd(cfg, args[1:])
	case "audit":
		err = auditCmd(cfg, args[1:])
	case "jobs":
		err = jobsCmd(cfg, args[1:])
	case "backup":
		err = backupCmd(cfg, args[1:])
	case "alerts":
		err = alertsCmd(cfg, args[1:])
	case "store":
		err = storeCmd(cfg, config.Path(*cfgPath), args[1:])
	case "account":
		err = accountCmd(cfg, args[1:])
	case "plan":
		err = planCmd(cfg, args[1:])
	case "token":
		err = tokenCmd(cfg, args[1:])
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

// serve runs the daemon: the panel, or with node set a node of a cluster
// (`wpgenie agent`): the same data plane for the sites placed on it, run
// by the panel through the cluster listener instead of signed-in users.
func serve(cfg *config.Config, node bool) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.SitesDir(), 0o755); err != nil {
		return err
	}
	st, err := openStore(ctx, cfg)
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
	// PHP response times, from the access log to the autoscaler.
	latency := &analytics.Recent{}
	// One Valkey user per site (object cache isolation); the daemon is its
	// admin user.
	cache := &runtime.Valkey{Container: cfg.RedisHost}
	svc := &site.Service{
		Cfg: cfg, Store: st, Runtime: &runtime.Docker{}, DB: db, Log: log,
		// Container names equal the hostnames sites use (deploy/docker-compose.yml).
		Cache:  cache,
		Dumper: &runtime.MariaDB{Container: cfg.MariaDBHost, Password: rootDSN.Passwd},
		Prober: &site.HTTPProber{Token: healthToken}, HealthToken: healthToken,
		Vulns:     &site.WPVulnerability{UserAgent: "WPGenie/" + version + " (+https://github.com/parthh37/wpgenie)"},
		Directory: &site.WordPressOrg{UserAgent: "WPGenie/" + version + " (+https://github.com/parthh37/wpgenie)"},
		CDN:       &cdn.Cloudflare{},
		CDNRanges: cfRanges,
		Bunny:     &cdn.Bunny{},
		Latency:   latency,
		Jobs:      jobQueue,
		Backups: &backup.Restic{Docker: docker, Image: cfg.ResticImage,
			CacheDir: filepath.Join(cfg.DataDir, "backups", "cache")},
		Images:  docker,
		Offload: &offload.Rclone{Docker: docker, Image: cfg.RcloneImage},
		Version: version,
		Proxy: proxy.NewCaddy(proxy.Config{
			ACMEEmail: cfg.ACMEEmail, AdminURL: cfg.CaddyAdmin, PanelDomain: cfg.PanelDomain,
			PanelUpstream: cfg.ListenAddr, ShieldUpstream: cfg.ListenAddr,
			AccessLog: cfg.AccessLog, CaddyfilePath: cfg.CaddyfilePath,
			CloudflareRanges: cfRanges.Get, IngressListen: cfg.IngressAddr,
		}),
	}

	// The cluster listener (mutual TLS): on a node, what the panel and
	// other nodes connect to; on the panel, what nodes tunnel to (sites
	// moved away from it, replicas of its sites elsewhere), once the first
	// node is added.
	agent := &cluster.Agent{Dir: cfg.ClusterDir, Listen: cfg.ClusterListen, Log: log,
		Tunnel: svc.TunnelTarget, Peers: svc.PeerHandler(),
		Info: func(ctx context.Context) any {
			info := svc.NodeInfo(ctx, version)
			info.CertExpires = agentCertExpiry()
			return info
		}}
	agentCertExpiry = agent.CertNotAfter
	if err := agent.Load(); err != nil {
		return fmt.Errorf("cluster identity: %w", err)
	}
	svc.ClusterClient, svc.ContainerIP = agent.Client, docker.ContainerIP
	// Other nodes must present the key they paired with (the panel's
	// directory has it): a removed server's certificate gets nowhere.
	agent.PeerPin = func(node string) (string, bool) {
		ep, err := svc.Peer(ctx, node)
		return ep.Pin, err == nil
	}
	svc.CacheACL, cache.AdminPassword = cache, svc.CacheAdminPassword
	go func() {
		// Valkey may still be starting (both come up at boot).
		for delay := time.Second; ; delay = min(delay*2, time.Minute) {
			err := svc.UpgradeCacheUsers(ctx)
			if err == nil {
				return
			}
			log.Warn("object cache users, retrying", "err", err, "in", delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()
	if bin, err := os.Executable(); err == nil {
		svc.Links = &runtime.Links{Docker: docker, Image: cfg.PHPImage, Network: cfg.DockerNetwork,
			ClusterDir: cfg.ClusterDir, Binary: bin}
	}
	var ctrl *cluster.Controller
	if node {
		svc.NodeID = func() (string, bool) { id, _, ok := agent.Identity(); return id, ok }
		// Job IDs are numbered per server so the panel can tell whose a job is.
		floor := func(num int64) {
			if err := st.JobIDFloor(ctx, num*cluster.JobStride); err != nil {
				log.Error("cluster: numbering jobs", "err", err)
			}
		}
		if _, num, ok := agent.Identity(); ok {
			floor(num)
		} else {
			// The code (it carries the one-time secret) is printed by the CLI
			// only, never logged.
			if _, err := agent.PairingCode(); err != nil {
				return err
			}
			log.Info("not part of a cluster yet: run `wpgenie agent pair-code` and add this server in the panel",
				"listen", cfg.ClusterListen)
		}
		agent.OnPaired = func(_ string, num int64) { floor(num) }
	} else {
		ctrl = &cluster.Controller{Store: st, Dir: cfg.ClusterDir, Log: log, Agent: agent,
			PlaceOnControl: cfg.PlaceSitesOnControl(),
			LocalInfo:      func(ctx context.Context) cluster.NodeInfo { return svc.NodeInfo(ctx, version) },
			StartAgent: func() {
				go func() {
					if err := agent.Serve(ctx); err != nil {
						log.Error("cluster listener", "err", err)
					}
				}()
			}}
		if cfg.ClusterAddress != "" {
			if err := st.SetSetting(ctx, "cluster_control_address", cfg.ClusterAddress); err != nil {
				return err
			}
		}
		svc.Cluster, svc.DomainTaken = ctrl, st.ClusterDomainTaken
		if err := ctrl.Load(); err != nil {
			return fmt.Errorf("cluster: %w", err)
		}
	}
	if err := svc.LoadCDNRanges(ctx); err != nil {
		log.Warn("using the built-in Cloudflare IP ranges", "err", err)
	}
	// The mail server runs next to the panel; nodes' sites get their
	// credentials from it through the panel.
	var mailSvc *mail.Service
	if !node {
		mailSvc = &mail.Service{
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
	}

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
	// Signing in to sites' wp-admin from the panel (links on their domains).
	wpLoginSvc := &wplogin.Service{Sites: svc, Log: log}
	// Logins that arrived with a site moved here.
	svc.AccessChanged = func(ctx context.Context) {
		if err := sftpSvc.Reconcile(ctx); err != nil {
			log.Error("sftp: logins of a site moved here", "err", err)
		}
	}
	svc.SiteRemoved = func(ctx context.Context, id string) {
		sftpSvc.SiteRemoved(ctx, id)
		adminerSvc.SiteRemoved(ctx, id)
		wpLoginSvc.SiteRemoved(ctx, id)
	}
	// A suspended site's SFTP logins and database sessions end until it is
	// back (the SFTP server leaves suspended sites' logins out).
	svc.SiteSuspended = func(ctx context.Context, id string, suspended bool) {
		if suspended {
			sftpSvc.SiteRemoved(ctx, id)
			adminerSvc.SiteRemoved(ctx, id)
			wpLoginSvc.SiteRemoved(ctx, id)
			return
		}
		if err := sftpSvc.Reconcile(ctx); err != nil {
			log.Error("sftp: restoring an unsuspended site's logins", "site", id, "err", err)
		}
	}

	// Accounts, plans and billing: suspension goes through the site
	// service, usage from the traffic rollups and disk measurements;
	// outgoing webhooks are delivered from their queue.
	// Only on the panel (nodes have no accounts); per-site operations and
	// usage go to the server each site lives on.
	var bill *billing.Service
	if !node {
		ops := site.ClusterOps{S: svc}
		hooks := &billing.Webhooks{Store: st, Log: log}
		go hooks.Run(ctx)
		bill = &billing.Service{Store: st, Sites: ops, Meter: ops, Usage: ops, Hooks: hooks, Stripe: &billing.StripeAPI{},
			InWindow: svc.InMaintenanceWindow, Log: log}
		go bill.RunUsage(ctx)
	}
	panelURL := "http://" + cfg.ListenAddr // through an SSH tunnel
	if cfg.PanelDomain != "" {
		panelURL = "https://" + cfg.PanelDomain
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
	go svc.RunInsights(ctx)
	go svc.RunForwards(ctx)
	go svc.RunSpread(ctx)
	go svc.RunOffload(ctx)

	upd := &updater.Updater{Current: version, Repo: cfg.UpdateRepo, StateDir: filepath.Join(cfg.DataDir, "updates")}
	go upd.Run(ctx, 12*time.Hour)

	ing := &analytics.Ingester{Path: cfg.AccessLog, Store: st, Secret: []byte(cfg.ShieldSecret), Logger: log, Recent: latency}

	// Monitoring: Prometheus metrics (GET /metrics, with its own token) and
	// alerts (uptime, certificates, disk, backups) by e-mail and webhooks.
	mon := &monitor.Service{Store: st, Log: log, Version: version, Shield: sh, Load: svc.CPU,
		Probe: monitor.LocalProbe(svc.Prober), Hosts: monitor.LocalHost(cfg.DataDir, filepath.Dir(cfg.AccessLog))}
	ing.Observer, ing.HealthToken = mon.ObserveTraffic, healthToken
	// The panel watches every server (uptime through each node's Caddy,
	// disks from its health checks, reachability) and sends the alerts; a
	// node only keeps the metrics of its own sites (served to the panel's
	// /metrics?node=).
	if !node {
		(&monitor.Cluster{Ctrl: ctrl, Store: st, LocalProbe: svc.Prober, LocalCerts: &monitor.TLSChecker{},
			LocalHosts: monitor.LocalHost(cfg.DataDir, filepath.Dir(cfg.AccessLog))}).Wire(mon)
		go mon.Run(ctx)
	}
	go ing.Run(ctx)

	apiSrv := &api.Server{Token: cfg.APIToken, Version: version, Sites: svc, Store: st, Shield: sh,
		Updater: upd, Mail: mailSvc, Jobs: jobQueue, SFTP: sftpSvc, Adminer: adminerSvc, WPLogin: wpLoginSvc,
		Lists: lists, Countries: countries, Monitor: mon, Log: log, Cluster: ctrl, Node: node,
		Billing: bill, PanelURL: panelURL}
	apiHandler := apiSrv.Handler()
	if node {
		// Requests the panel forwards arrive over the cluster listener, and
		// so does its scrape of this node's metrics.
		agent.API = apiHandler
		control := http.NewServeMux()
		control.Handle("/cluster/v1/", svc.ClusterHandler())
		control.HandleFunc("GET /cluster/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
			body, err := mon.Collect(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.Write(body)
		})
		agent.Control = control
		go func() {
			if err := agent.Serve(ctx); err != nil {
				log.Error("cluster listener", "err", err)
				stop()
			}
		}()
	} else {
		// The health loop starts once the controller is complete.
		ctrl.Configure = apiSrv.ConfigureNode
		go ctrl.Run(ctx)
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           apiHandler,
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
			"plugins|smtp|cdn|offload|images|insights|events|backup|staging|push|domain|cert|php|sftp|adminer|wp|optimize|analyse")
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
	case "updates", "update", "auto-update", "scan", "plugins", "smtp", "events", "cdn", "images", "insights":
		return siteOpsCmd(cfg, args[0], args[1:])
	case "php":
		return phpCmd(cfg, args[1:])
	case "move":
		return moveCmd(cfg, args[1:])
	case "spread":
		return spreadCmd(cfg, args[1:])
	case "offload":
		return offloadCmd(cfg, args[1:])
	case "backup", "staging", "push", "domain", "cert", "sftp", "adminer", "wp", "optimize", "analyse":
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
		case "wp":
			return wpCmd(cfg, id, rest)
		case "optimize":
			return optimizeCmd(cfg, id, rest)
		case "analyse":
			return analyseCmd(cfg, id, rest)
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
	var page, object, mobile onOff
	st, err := siteFlags(cfg, args, "usage: wpgenie site cache <site-id> [--page on|off] [--object on|off] [--mobile on|off]",
		func(fs *flag.FlagSet, st *store.Site) {
			page, object, mobile = onOff(st.PageCache), onOff(st.ObjectCache), onOff(st.CacheMobile)
			fs.Var(&page, "page", "full-page cache (on|off)")
			fs.Var(&object, "object", "Redis object cache (on|off)")
			fs.Var(&mobile, "mobile", "separate mobile copies of every page, for themes that detect phones themselves (on|off)")
		})
	if err != nil {
		return err
	}
	var out store.Site
	m := bool(mobile)
	in := site.CacheSettings{PageCache: bool(page), ObjectCache: bool(object), Mobile: &m}
	if err := call(cfg, "PUT", "/sites/"+st.ID+"/cache", in, &out); err != nil {
		return err
	}
	fmt.Printf("Site %s: page cache %v (separate mobile copies %v), object cache %v\n", out.ID, onOff(out.PageCache),
		onOff(out.CacheMobile), onOff(out.ObjectCache))
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
