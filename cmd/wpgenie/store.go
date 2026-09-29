package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/store"
)

// openStore opens the panel database config chooses: SQLite in the data
// directory, or PostgreSQL when database_url is set.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	if cfg.DatabaseURL != "" {
		return store.OpenURL(ctx, cfg.DatabaseURL)
	}
	return store.Open(cfg.DBPath())
}

func storeCmd(cfg *config.Config, cfgPath string, args []string) error {
	const usage = "usage: wpgenie store migrate-to-postgres [<postgres-url> | -]"
	if len(args) == 0 || args[0] != "migrate-to-postgres" || len(args) > 2 {
		return errors.New(usage)
	}
	if cfg.DatabaseURL != "" {
		return fmt.Errorf("database_url is already set in %s: the daemon uses PostgreSQL. This command copies the "+
			"SQLite database into an empty PostgreSQL database", cfgPath)
	}
	// Anything the daemon wrote after the copy's snapshot would be lost:
	// it must not be running. It always listens on listen_addr.
	if c, err := net.DialTimeout("tcp", cfg.ListenAddr, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("the daemon is running (%s answers): stop it first (systemctl stop wpgenie)", cfg.ListenAddr)
	}
	// A URL on the command line lands in shell history and is visible in
	// the process list, password included: "-" (or nothing) reads stdin.
	url := ""
	if len(args) == 2 && args[1] != "-" {
		url = args[1]
	} else {
		fmt.Fprint(os.Stderr, "PostgreSQL URL (postgres://user:password@host:5432/db): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading the URL from stdin: %w", err)
		}
		url = strings.TrimSpace(line)
		fmt.Fprintln(os.Stderr)
	}
	if !store.IsPostgresURL(url) {
		return errors.New("the URL must start with postgres:// or postgresql://")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rep, err := store.CopyToPostgres(ctx, cfg.DBPath(), url)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TABLE\tROWS")
	var total int64
	for _, t := range rep.Tables {
		fmt.Fprintf(w, "%s\t%d\n", t.Name, t.Rows)
		total += t.Rows
	}
	w.Flush()
	var line strings.Builder
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false) // keep & in the query string readable
	enc.Encode(url)
	fmt.Printf(`
Copied %d rows in %d tables (schema version %d), counts verified.
Add this line to %s, then start the daemon (systemctl start wpgenie):

  "database_url": %s,

%s is unchanged: to go back, remove database_url (changes made
on PostgreSQL meanwhile stay there).
`, total, len(rep.Tables), rep.Version, cfgPath, strings.TrimSpace(line.String()), cfg.DBPath())
	return nil
}
