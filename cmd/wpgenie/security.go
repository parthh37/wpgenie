package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/iprep"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// pluginsCmd shows the last plugin analysis, or runs one with --now.
func pluginsCmd(cfg *config.Config, id string, args []string) error {
	fs := flag.NewFlagSet("plugins", flag.ContinueOnError)
	now := fs.Bool("now", false, "analyse now (10-30 seconds)")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var rep *site.PluginReport
	method := "GET"
	if *now {
		method = "POST"
	}
	if err := call(cfg, method, "/sites/"+id+"/plugins", nil, &rep); err != nil {
		return err
	}
	if rep == nil {
		fmt.Println("Not analysed yet: run `wpgenie site plugins " + id + " --now`.")
		return nil
	}
	if *asJSON {
		return printJSON(rep)
	}
	fmt.Printf("Analysed %s", rep.AnalysedAt.Local().Format("2006-01-02 15:04"))
	if p := rep.Profile; p != nil {
		fmt.Printf(": front page %.0f ms, %d queries, %d MB peak memory", p.TotalMS, p.Queries, p.PeakMemoryKB/1024)
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PLUGIN\tVERSION\tSTATUS\tWP.ORG\tFILES\tCOST (MS)\tFINDINGS")
	for _, p := range rep.Plugins {
		cost := "-"
		if p.Perf != nil {
			cost = fmt.Sprintf("%.1f", p.Perf.LoadMS+p.Perf.HookMS)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Slug, p.Version, p.Status, p.Directory, p.Checksums, cost,
			strings.Join(p.Flags, "; "))
	}
	w.Flush()
	for _, s := range rep.ThemeSignatures {
		fmt.Println("Suspicious theme file:", s)
	}
	for _, e := range rep.Errors {
		fmt.Println("Partial analysis:", e)
	}
	return nil
}

// securityListsCmd shows or replaces the server-wide allow / deny lists.
func securityListsCmd(cfg *config.Config, which string, args []string) error {
	var g site.GlobalLists
	if err := call(cfg, "GET", "/security/settings", nil, &g); err != nil {
		return err
	}
	if len(args) > 0 {
		list := []string{}
		if args[0] != "none" {
			for _, v := range strings.Split(strings.Join(args, ","), ",") {
				if v = strings.TrimSpace(v); v != "" {
					list = append(list, v)
				}
			}
		}
		if which == "allow" {
			g.Allow = list
		} else {
			g.Deny = list
		}
		if err := call(cfg, "PUT", "/security/settings", g, &g); err != nil {
			return err
		}
	}
	fmt.Printf("Allowed on every site (never blocked): %v\nBlocked on every site: %v\n", g.Allow, g.Deny)
	return nil
}

func reputationCmd(cfg *config.Config, args []string) error {
	method, path := "GET", "/security/reputation"
	if len(args) > 0 && args[0] == "refresh" {
		method, path = "POST", "/security/reputation/refresh"
	}
	var st struct {
		Lists        []iprep.FeedStatus `json:"lists"`
		Countries    *iprep.GeoStatus   `json:"countries"`
		WAFAvailable bool               `json:"waf_available"`
	}
	if err := call(cfg, method, path, nil, &st); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LIST\tENTRIES\tUPDATED\tERROR")
	for _, l := range st.Lists {
		updated := "never"
		if !l.UpdatedAt.IsZero() {
			updated = l.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", l.Name, l.Entries, updated, l.Error)
	}
	w.Flush()
	if c := st.Countries; c != nil {
		if c.Loaded {
			fmt.Printf("Country database: %s release, %d ranges. %s\n", c.Month, c.Ranges, c.Attribution)
		} else {
			fmt.Println("Country database: not downloaded (it is fetched once a site uses country rules).", c.Error)
		}
	}
	fmt.Println("Request-body WAF available in Caddy:", st.WAFAvailable)
	return nil
}

func userCmd(cfg *config.Config, args []string) error {
	usage := errors.New("usage: wpgenie user ls | add <name> [--role admin|operator|viewer] | role <name> <role> |\n" +
		"  disable <name> | enable <name> | passwd <name> | reset-2fa <name> | rm <name> | require-2fa on|off")
	if len(args) == 0 {
		return usage
	}
	name := ""
	if len(args) > 1 {
		name = url.PathEscape(args[1])
	}
	switch args[0] {
	case "ls":
		var users []store.User
		if err := call(cfg, "GET", "/users", nil, &users); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "USER\tROLE\t2FA\tDISABLED\tLAST SIGN-IN")
		for _, u := range users {
			last := "never"
			if !u.LastLoginAt.IsZero() {
				last = u.LastLoginAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%s\t%v\t%v\t%s\n", u.Username, u.Role, onOff(u.TOTPEnabled), u.Disabled, last)
		}
		return w.Flush()
	case "add":
		if name == "" {
			return usage
		}
		fs := flag.NewFlagSet("add", flag.ContinueOnError)
		role := fs.String("role", "operator", "admin, operator or viewer")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		var out struct {
			Password string `json:"password"`
		}
		if err := call(cfg, "POST", "/users", map[string]string{"username": args[1], "role": *role}, &out); err != nil {
			return err
		}
		fmt.Printf("User %s (%s) created.\n  Password: %s\nSave it now; it is not shown again. They can change it under Account.\n",
			args[1], *role, out.Password)
		return nil
	case "role":
		if len(args) != 3 {
			return usage
		}
		return call(cfg, "PUT", "/users/"+name, map[string]string{"role": args[2]}, nil)
	case "disable", "enable":
		if name == "" {
			return usage
		}
		return call(cfg, "PUT", "/users/"+name, map[string]bool{"disabled": args[0] == "disable"}, nil)
	case "passwd":
		if name == "" {
			return usage
		}
		var out struct{ Password string }
		if err := call(cfg, "POST", "/users/"+name+"/password", nil, &out); err != nil {
			return err
		}
		fmt.Printf("New password for %s: %s\nThey were signed out everywhere.\n", args[1], out.Password)
		return nil
	case "reset-2fa":
		if name == "" {
			return usage
		}
		if err := call(cfg, "DELETE", "/users/"+name+"/totp", nil, nil); err != nil {
			return err
		}
		fmt.Printf("Two-factor authentication is off for %s; they can set it up again under Account.\n", args[1])
		return nil
	case "rm":
		if name == "" {
			return usage
		}
		return call(cfg, "DELETE", "/users/"+name, nil, nil)
	case "require-2fa":
		if len(args) != 2 {
			return usage
		}
		var v onOff
		if err := v.Set(args[1]); err != nil {
			return err
		}
		return call(cfg, "PUT", "/settings/auth", map[string]bool{"require_2fa": bool(v)}, nil)
	}
	return usage
}

func auditCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "entries")
	actor := fs.String("user", "", "only this user (or api-token)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var entries []store.AuditEntry
	q := url.Values{"limit": {fmt.Sprint(*limit)}, "actor": {*actor}}
	if err := call(cfg, "GET", "/audit?"+q.Encode(), nil, &entries); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tUSER\tFROM\tACTION\tSTATUS\tDETAIL")
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", e.Time.Local().Format(time.DateTime), e.Actor, e.IP, e.Action, e.Status, e.Detail)
	}
	return w.Flush()
}
