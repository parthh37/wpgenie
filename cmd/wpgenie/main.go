// Command wpgenie is the WPGenie control plane daemon and its CLI client.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/analytics"
	"github.com/parthh37/wpgenie/internal/api"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/dbprov"
	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
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

	svc := &site.Service{
		Cfg: cfg, Store: st, Runtime: &runtime.Docker{}, DB: db, Log: log,
		// redis_host is also the Valkey container's name (deploy/docker-compose.yml).
		Cache: &runtime.Valkey{Container: cfg.RedisHost},
		Proxy: proxy.NewCaddy(proxy.Config{
			ACMEEmail: cfg.ACMEEmail, AdminURL: cfg.CaddyAdmin, PanelDomain: cfg.PanelDomain,
			PanelUpstream: cfg.ListenAddr, ShieldUpstream: cfg.ListenAddr,
			AccessLog: cfg.AccessLog, CaddyfilePath: cfg.CaddyfilePath,
		}),
	}
	sh := shield.New(shield.Options{Secret: []byte(cfg.ShieldSecret), Sites: svc.ShieldLookup, Logger: log})
	go sh.Run(ctx)

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

	ing := &analytics.Ingester{Path: cfg.AccessLog, Store: st, Secret: []byte(cfg.ShieldSecret), Logger: log}
	go ing.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           (&api.Server{Token: cfg.APIToken, Sites: svc, Store: st, Shield: sh, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Site creation and scaling run synchronously and can take minutes.
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
	return srv.Shutdown(shutdownCtx)
}

// siteCmd is a thin client of the local API, so the CLI and the dashboard
// always go through the same validation and code paths.
func siteCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: wpgenie site ls|create|rm|scale|cache|purge")
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
		if len(args) != 3 {
			return errors.New("usage: wpgenie site create <domain> <admin-email>")
		}
		var out struct {
			Site        store.Site       `json:"site"`
			Credentials site.Credentials `json:"credentials"`
		}
		if err := call(cfg, "POST", "/sites", site.CreateInput{Domain: args[1], AdminEmail: args[2]}, &out); err != nil {
			return err
		}
		c := out.Credentials
		fmt.Printf("Site %s created (%s)\n\n  Admin URL: %s\n  Username:  %s\n  Password:  %s\n\nSave the password now; it is not stored.\n",
			out.Site.ID, c.URL, c.AdminURL, c.Username, c.Password)
		return nil
	case "rm":
		if len(args) != 2 {
			return errors.New("usage: wpgenie site rm <site-id>")
		}
		return call(cfg, "DELETE", "/sites/"+args[1], nil, nil)
	case "scale":
		return scaleCmd(cfg, args[1:])
	case "cache":
		return cacheCmd(cfg, args[1:])
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
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+cfg.ListenAddr+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
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
