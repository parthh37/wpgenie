package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/store"
)

// agentCertExpiry reports when this server's cluster certificate expires
// (health checks show it in the panel).
var agentCertExpiry = func() time.Time { return time.Time{} }

// agentCmd: `wpgenie agent` runs this server as a node of a cluster;
// `wpgenie agent pair-code` prints the code to add it in the panel.
func agentCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 || args[0] == "run" {
		return serve(cfg, true)
	}
	a := &cluster.Agent{Dir: cfg.ClusterDir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.Load(); err != nil {
		return err
	}
	switch args[0] {
	case "pair-code":
		code, err := a.PairingCode()
		if err != nil {
			return err
		}
		fmt.Println(code)
		fmt.Fprintf(os.Stderr, "\nIn the panel, add this server under Servers with this code and its address "+
			"(this server's IP, port %s). The code works once.\n", strings.TrimPrefix(cfg.ClusterListen, ":"))
		return nil
	case "status":
		if id, num, ok := a.Identity(); ok {
			fmt.Printf("Node %s (number %d) of a cluster; certificate valid until %s\n", id, num,
				a.CertNotAfter().Local().Format(time.DateOnly))
		} else {
			fmt.Println("Not part of a cluster yet: run `wpgenie agent pair-code`.")
		}
		return nil
	}
	return errors.New("usage: wpgenie agent [run | pair-code | status]")
}

// linkMain runs inside a link container (see runtime.Links): tunnels the
// home server's MariaDB and Valkey to this node's replicas of its sites.
// It has no config file, only the node's cluster identity.
func linkMain(args []string) error {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	dir := fs.String("dir", "/cluster", "cluster identity directory")
	node := fs.String("node", "", "the sites' home node")
	address := fs.String("address", "", "its cluster listener, host:port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	a := &cluster.Agent{Dir: *dir, Log: log}
	if err := a.Load(); err != nil {
		return err
	}
	client := a.Client()
	if client == nil {
		return errors.New("this node is not paired")
	}
	home := cluster.Endpoint{ID: *node, Address: *address}
	for _, m := range []struct{ listen, target string }{
		{":3306", cluster.TargetMariaDB}, {":6379", cluster.TargetValkey},
	} {
		f, err := client.Listen(m.listen, home, m.target, log)
		if err != nil {
			return err
		}
		defer f.Close()
	}
	log.Info("link up", "home", *node)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}

type nodeRow struct {
	*store.Node
	Local bool `json:"local"`
	Sites int  `json:"sites"`
	Up    bool `json:"up"`
}

// nodeCmd manages the servers of a cluster (on the panel).
func nodeCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	switch args[0] {
	case "ls":
		var nodes []nodeRow
		if err := call(cfg, "GET", "/nodes", nil, &nodes); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tADDRESS\tSTATUS\tSITES\tMEMORY\tDISK FREE\tVERSION")
		for _, n := range nodes {
			var info cluster.NodeInfo
			json.Unmarshal(n.Info, &info)
			status := n.Status
			if !n.Up {
				status = "unreachable"
			}
			addr := n.Address
			if n.Local {
				addr = "(panel)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d/%d MB\t%.0f GB\t%s\n", n.ID, n.Name, addr, status, n.Sites,
				info.CommittedMB, info.MemTotalMB, info.DiskFreeGB, info.Version)
		}
		return w.Flush()
	case "add":
		fs := flag.NewFlagSet("node add", flag.ContinueOnError)
		publicIP := fs.String("public-ip", "", "where sites' DNS should point (default: the address's IP)")
		id := fs.String("id", "", "node ID (default: from the name)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 3 {
			return errors.New("usage: wpgenie node add <name> <address[:7443]> <pairing-code> [--public-ip IP] [--id ID]")
		}
		in := cluster.AddNodeInput{ID: *id, Name: fs.Arg(0), Address: fs.Arg(1), PairingCode: fs.Arg(2), PublicIP: *publicIP}
		var n store.Node
		if err := call(cfg, "POST", "/nodes", in, &n); err != nil {
			return err
		}
		fmt.Printf("Added node %s (%s).\n", n.ID, n.Address)
		return nil
	case "rm":
		fs := flag.NewFlagSet("node rm", flag.ContinueOnError)
		force := fs.Bool("force", false, "forget it even though sites live on it")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return errors.New("usage: wpgenie node rm <id> [--force]")
		}
		q := ""
		if *force {
			q = "?force=1"
		}
		return call(cfg, "DELETE", "/nodes/"+fs.Arg(0)+q, nil, nil)
	case "drain", "update":
		if len(args) != 2 {
			return fmt.Errorf("usage: wpgenie node %s <id>", args[0])
		}
		var out map[string]any
		if err := call(cfg, "POST", "/nodes/"+args[1]+"/"+args[0], nil, &out); err != nil {
			return err
		}
		if id, ok := out["job_id"]; ok {
			fmt.Printf("Moving every site off %s: job %v (wpgenie jobs %v)\n", args[1], id, id)
		} else {
			fmt.Println("Update started on", args[1])
		}
		return nil
	case "activate":
		if len(args) != 2 {
			return errors.New("usage: wpgenie node activate <id>")
		}
		return call(cfg, "PUT", "/nodes/"+args[1], cluster.UpdateNodeInput{Status: store.NodeActive}, nil)
	}
	return errors.New("usage: wpgenie node ls | add | rm | drain | activate | update")
}

// moveCmd moves a site to another server; `--finish` deletes the old copy
// once DNS points at the new one.
func moveCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("site move", flag.ContinueOnError)
	finish := fs.Bool("finish", false, "the move is done (DNS updated): delete the old copy now")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *finish && fs.NArg() == 1:
		return call(cfg, "POST", "/sites/"+fs.Arg(0)+"/move/finish", nil, nil)
	case fs.NArg() == 2:
		var out map[string]int64
		if err := call(cfg, "POST", "/sites/"+fs.Arg(0)+"/migrate", map[string]string{"node": fs.Arg(1)}, &out); err != nil {
			return err
		}
		fmt.Printf("Moving: job %d (wpgenie jobs %d). The site shows a maintenance page only during the final copy.\n",
			out["job_id"], out["job_id"])
		return nil
	}
	return errors.New("usage: wpgenie site move <site-id> <node> | --finish <site-id>")
}

// spreadCmd runs some of a site's replicas on other nodes.
func spreadCmd(cfg *config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: wpgenie site spread <site-id> <node>[,<node>...] | none")
	}
	nodes := []string{}
	if args[1] != "none" {
		for _, n := range strings.Split(strings.Join(args[1:], ","), ",") {
			if n = strings.TrimSpace(n); n != "" {
				nodes = append(nodes, n)
			}
		}
	}
	var st store.Site
	if err := call(cfg, "PUT", "/sites/"+args[0]+"/spread", map[string][]string{"nodes": nodes}, &st); err != nil {
		return err
	}
	fmt.Printf("%s: %d replicas, spread over %v\n", st.ID, st.Replicas, st.SpreadNodes)
	return nil
}
