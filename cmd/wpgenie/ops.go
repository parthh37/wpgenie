package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

func autoscaleCmd(cfg *config.Config, args []string) error {
	var on, off bool
	st, err := siteFlags(cfg, args, "usage: wpgenie site autoscale <site-id> [--on|--off] [--min N] [--max N] [--target PCT]",
		func(fs *flag.FlagSet, st *store.Site) {
			fs.BoolVar(&on, "on", false, "turn autoscaling on")
			fs.BoolVar(&off, "off", false, "turn autoscaling off")
			fs.IntVar(&st.MinReplicas, "min", st.MinReplicas, "fewest replicas")
			fs.IntVar(&st.MaxReplicas, "max", st.MaxReplicas, "most replicas")
			fs.IntVar(&st.TargetCPU, "target", st.TargetCPU, "target CPU use per replica, percent")
		})
	if err != nil {
		return err
	}
	enabled := (st.Autoscale || on) && !off
	var out store.Site
	in := site.AutoscaleSettings{Enabled: enabled, MinReplicas: st.MinReplicas, MaxReplicas: st.MaxReplicas, TargetCPU: st.TargetCPU}
	if err := call(cfg, "PUT", "/sites/"+st.ID+"/autoscale", in, &out); err != nil {
		return err
	}
	if out.Autoscale {
		fmt.Printf("Site %s autoscales between %d and %d replicas at %d%% CPU (now %d).\n",
			out.ID, out.MinReplicas, out.MaxReplicas, out.TargetCPU, out.Replicas)
	} else {
		fmt.Printf("Site %s: autoscaling off, %d replica(s).\n", out.ID, out.Replicas)
	}
	return nil
}

type listFlag struct {
	v   *[]string
	set bool
}

func (l *listFlag) String() string { return "" }
func (l *listFlag) Set(s string) error {
	l.set = true
	*l.v = []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l.v = append(*l.v, p)
		}
	}
	return nil
}

func shieldCmd(cfg *config.Config, args []string) error {
	var waf onOff
	var admin, trusted listFlag
	var mode string
	st, err := siteFlags(cfg, args, "usage: wpgenie site shield <site-id> [--mode off|standard|under_attack] [--waf on|off] "+
		"[--admin-allow IP/CIDR,...] [--trusted IP/CIDR,...]  (an empty list clears it)",
		func(fs *flag.FlagSet, st *store.Site) {
			mode, waf = st.ShieldMode, onOff(st.WAF)
			admin.v, trusted.v = &st.AdminAllow, &st.TrustedIPs
			fs.StringVar(&mode, "mode", mode, "protection level")
			fs.Var(&waf, "waf", "request inspection (on|off)")
			fs.Var(&admin, "admin-allow", "only these networks reach wp-admin / wp-login.php")
			fs.Var(&trusted, "trusted", "networks that bypass the shield")
		})
	if err != nil {
		return err
	}
	w := bool(waf)
	in := site.ShieldInput{Mode: shield.Mode(mode), BlockAIBots: st.BlockAIBots, WAF: &w,
		AdminAllow: &st.AdminAllow, TrustedIPs: &st.TrustedIPs}
	var out store.Site
	if err := call(cfg, "PUT", "/sites/"+st.ID+"/shield", in, &out); err != nil {
		return err
	}
	fmt.Printf("Site %s: shield %s, WAF %v, admin allowlist %v, trusted %v\n",
		out.ID, out.ShieldMode, onOff(out.WAF), out.AdminAllow, out.TrustedIPs)
	return nil
}

func siteOpsCmd(cfg *config.Config, op string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: wpgenie site %s <site-id> ...", op)
	}
	id := url.PathEscape(args[0])
	switch op {
	case "updates":
		var inv site.Inventory
		if err := call(cfg, "GET", "/sites/"+id+"/updates", nil, &inv); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "TYPE\tNAME\tVERSION\tUPDATE")
		for _, c := range append(append([]site.Component{inv.Core}, inv.Plugins...), inv.Themes...) {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Type, c.Slug, c.Version, c.UpdateVersion)
		}
		return w.Flush()
	case "update":
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		var req site.UpdateRequest
		var plugins, themes listFlag
		plugins.v, themes.v = &req.Plugins, &req.Themes
		fs.BoolVar(&req.All, "all", false, "everything with an update")
		fs.BoolVar(&req.Core, "core", false, "WordPress core")
		fs.Var(&plugins, "plugins", "plugin slugs, comma-separated")
		fs.Var(&themes, "themes", "theme slugs, comma-separated")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var out struct {
			RunID int64 `json:"run_id"`
		}
		if err := call(cfg, "POST", "/sites/"+id+"/updates", req, &out); err != nil {
			return err
		}
		fmt.Printf("Update %d started: a snapshot is taken first and restored if the site breaks.\n"+
			"Follow it with `wpgenie site events %s`.\n", out.RunID, args[0])
		return nil
	case "auto-update":
		if len(args) != 2 {
			return errors.New("usage: wpgenie site auto-update <site-id> off|security|all")
		}
		return call(cfg, "PUT", "/sites/"+id+"/auto-update", map[string]string{"policy": args[1]}, nil)
	case "scan":
		var rep site.ScanReport
		if err := call(cfg, "POST", "/sites/"+id+"/scan", nil, &rep); err != nil {
			return err
		}
		return printJSON(rep)
	case "smtp":
		if len(args) != 2 {
			return errors.New("usage: wpgenie site smtp <site-id> on|off")
		}
		var v onOff
		if err := v.Set(args[1]); err != nil {
			return err
		}
		return call(cfg, "PUT", "/sites/"+id+"/smtp", map[string]bool{"enabled": bool(v)}, nil)
	case "events":
		var ev []store.Event
		if err := call(cfg, "GET", "/sites/"+id+"/events?limit=50", nil, &ev); err != nil {
			return err
		}
		for i := len(ev) - 1; i >= 0; i-- {
			fmt.Printf("%s  %-9s %s\n", ev[i].Time.Local().Format("2006-01-02 15:04"), ev[i].Kind, ev[i].Message)
		}
		return nil
	case "cdn":
		return cdnCmd(cfg, id, args[1:])
	}
	return nil
}

func cdnCmd(cfg *config.Config, id string, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	var st site.CDNStatus
	switch sub {
	case "status":
		if err := call(cfg, "GET", "/sites/"+id+"/cdn", nil, &st); err != nil {
			return err
		}
	case "cloudflare":
		// On stdin, never argv: command lines are visible to every user in ps.
		fmt.Fprintln(os.Stderr, "Cloudflare API token (Zone: Read, Cache Purge: Purge, optionally Zone Settings: Read):")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no token on stdin")
		}
		in := site.CDNInput{Provider: "cloudflare", APIToken: strings.TrimSpace(line)}
		if err := call(cfg, "PUT", "/sites/"+id+"/cdn", in, &st); err != nil {
			return err
		}
	case "off":
		if err := call(cfg, "PUT", "/sites/"+id+"/cdn", site.CDNInput{}, &st); err != nil {
			return err
		}
	case "purge":
		if err := call(cfg, "POST", "/sites/"+id+"/cdn/purge", nil, nil); err != nil {
			return err
		}
		fmt.Println("CDN cache purged.")
		return nil
	default:
		return errors.New("usage: wpgenie site cdn <site-id> [status|cloudflare|off|purge]")
	}
	return printJSON(st)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func mailCmd(cfg *config.Config, args []string) error {
	usage := errors.New("usage: wpgenie mail enable <hostname>|disable|status|domain|box|alias ...")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "status":
		var st mail.Status
		if err := call(cfg, "GET", "/mail", nil, &st); err != nil {
			return err
		}
		return printJSON(st)
	case "enable", "disable":
		in := map[string]any{"enabled": args[0] == "enable"}
		if args[0] == "enable" {
			if len(args) != 2 {
				return errors.New("usage: wpgenie mail enable <hostname>   (e.g. mail.example.com; its A record must point here)")
			}
			in["hostname"] = args[1]
		}
		var st mail.Status
		if err := call(cfg, "PUT", "/mail", in, &st); err != nil {
			return err
		}
		return printJSON(st)
	case "domain":
		if len(args) != 3 {
			return errors.New("usage: wpgenie mail domain add|rm|dns <domain>")
		}
		d := url.PathEscape(args[2])
		switch args[1] {
		case "add":
			var info mail.DomainInfo
			if err := call(cfg, "POST", "/mail/domains", map[string]string{"domain": args[2]}, &info); err != nil {
				return err
			}
			return printRecords(info)
		case "rm":
			return call(cfg, "DELETE", "/mail/domains/"+d, nil, nil)
		case "dns":
			var info mail.DomainInfo
			if err := call(cfg, "GET", "/mail/domains/"+d+"?check=1", nil, &info); err != nil {
				return err
			}
			return printRecords(info)
		}
	case "box":
		if len(args) < 2 {
			return errors.New("usage: wpgenie mail box ls | add <address> [--quota MB] | passwd <address> | rm <address>")
		}
		switch args[1] {
		case "ls":
			var boxes []store.Mailbox
			if err := call(cfg, "GET", "/mail/mailboxes", nil, &boxes); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ADDRESS\tQUOTA\tSITE")
			for _, b := range boxes {
				q := "unlimited"
				if b.QuotaMB > 0 {
					q = fmt.Sprintf("%d MB", b.QuotaMB)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", b.Address, q, b.SiteID)
			}
			return w.Flush()
		case "add":
			if len(args) < 3 {
				return errors.New("usage: wpgenie mail box add <address> [--quota MB]")
			}
			fs := flag.NewFlagSet("box", flag.ContinueOnError)
			quota := fs.Int("quota", 0, "quota in MB (0 = unlimited)")
			if err := fs.Parse(args[3:]); err != nil {
				return err
			}
			var out struct{ Password string }
			if err := call(cfg, "POST", "/mail/mailboxes", mail.MailboxInput{Address: args[2], QuotaMB: *quota}, &out); err != nil {
				return err
			}
			fmt.Printf("Mailbox %s created.\n  Password: %s\nSave it now; it is not stored.\n", args[2], out.Password)
			return nil
		case "passwd":
			if len(args) != 3 {
				return errors.New("usage: wpgenie mail box passwd <address>")
			}
			var out struct{ Password string }
			if err := call(cfg, "PUT", "/mail/mailboxes/"+url.PathEscape(args[2])+"/password", map[string]string{}, &out); err != nil {
				return err
			}
			fmt.Printf("New password for %s: %s\n", args[2], out.Password)
			return nil
		case "rm":
			if len(args) != 3 {
				return errors.New("usage: wpgenie mail box rm <address>   (deletes all its mail)")
			}
			return call(cfg, "DELETE", "/mail/mailboxes/"+url.PathEscape(args[2]), nil, nil)
		}
	case "alias":
		if len(args) != 4 {
			return errors.New("usage: wpgenie mail alias add|rm <alias> <target>")
		}
		if args[1] == "add" {
			return call(cfg, "POST", "/mail/aliases", map[string]string{"alias": args[2], "target": args[3]}, nil)
		}
		q := url.Values{"alias": {args[2]}, "target": {args[3]}}
		return call(cfg, "DELETE", "/mail/aliases?"+q.Encode(), nil, nil)
	}
	return usage
}

func printRecords(info mail.DomainInfo) error {
	fmt.Printf("DNS records for %s:\n\n", info.Domain)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tTYPE\tNAME\tVALUE")
	for _, r := range info.Records {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Status, r.Type, r.Name, r.Value)
	}
	return w.Flush()
}

func securityCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: wpgenie security bans | ban <ip> [--hours N] | unban <ip>")
	}
	switch args[0] {
	case "bans":
		var bans []shield.Ban
		if err := call(cfg, "GET", "/security/bans", nil, &bans); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ADDRESS\tUNTIL\tREASON")
		for _, b := range bans {
			fmt.Fprintf(w, "%s\t%s\t%s\n", b.Addr, b.Until.Local().Format("2006-01-02 15:04"), b.Reason)
		}
		return w.Flush()
	case "ban":
		if len(args) < 2 {
			return errors.New("usage: wpgenie security ban <ip> [--hours N]")
		}
		fs := flag.NewFlagSet("ban", flag.ContinueOnError)
		hours := fs.Int("hours", 24, "ban duration")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		return call(cfg, "POST", "/security/bans", map[string]any{"addr": args[1], "hours": *hours}, nil)
	case "unban":
		if len(args) != 2 {
			return errors.New("usage: wpgenie security unban <ip>")
		}
		return call(cfg, "DELETE", "/security/bans?addr="+url.QueryEscape(args[1]), nil, nil)
	}
	return fmt.Errorf("unknown security command %q", args[0])
}
