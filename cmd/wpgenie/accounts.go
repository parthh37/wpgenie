package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/store"
)

// Accounts, plans and API tokens: thin clients of the provisioning API.

type accountOut struct {
	store.Account
	Plan       *store.Plan `json:"plan"`
	Suspended  bool        `json:"effectively_suspended"`
	Sites      int         `json:"sites"`
	ParentName string      `json:"parent_name"`
}

func fmtLimit(n int64, unit string) string {
	if n == 0 {
		return "unlimited"
	}
	return strconv.FormatInt(n, 10) + unit
}

func fmtBytes(n int64) string {
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

func accountCmd(cfg *config.Config, args []string) error {
	usage := errors.New("usage: wpgenie account ls | show <id> | create <name> --plan P [--kind customer|reseller]\n" +
		"  [--parent ID] [--email E] [--user NAME] | set <id> [--name N] [--plan P] [--email E] |\n" +
		"  suspend <id> [--reason admin|billing|overage] | unsuspend <id> | terminate <id> [--delete-sites] | rm <id> |\n" +
		"  usage <id> [--measure] | users <id> | user-add <id> <name> | sso <id> [user] | assign <site-id> <id|0>")
	if len(args) == 0 {
		return usage
	}
	id := ""
	if len(args) > 1 {
		id = url.PathEscape(args[1])
	}
	need := func(n int) error {
		if len(args) < n || strings.HasPrefix(args[1], "-") {
			return usage
		}
		return nil
	}
	switch args[0] {
	case "ls":
		var list []accountOut
		if err := call(cfg, "GET", "/accounts", nil, &list); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tKIND\tSTATUS\tPLAN\tSITES\tRESELLER\tWHMCS\tSTRIPE")
		for _, a := range list {
			status := a.Status
			if a.SuspendReason != "" {
				status += " (" + a.SuspendReason + ")"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", a.ID, a.Name, a.Kind, status, a.PlanID, a.Sites,
				a.ParentName, a.WHMCSServiceID, a.StripeCustomerID)
		}
		return w.Flush()
	case "show":
		if err := need(2); err != nil {
			return err
		}
		var a accountOut
		if err := call(cfg, "GET", "/accounts/"+id, nil, &a); err != nil {
			return err
		}
		fmt.Printf("Account %d: %s (%s)\nStatus:  %s %s\nPlan:    %s\nSites:   %d\n", a.ID, a.Name, a.Kind, a.Status,
			a.SuspendReason, a.PlanID, a.Sites)
		if a.ParentName != "" {
			fmt.Printf("Reseller: %s (%d)\n", a.ParentName, a.ParentID)
		}
		if a.Email != "" {
			fmt.Println("Email:  ", a.Email)
		}
		var ev []store.AccountEvent
		if err := call(cfg, "GET", "/accounts/"+id+"/events?limit=10", nil, &ev); err == nil && len(ev) > 0 {
			fmt.Println("Recent:")
			for _, e := range ev {
				fmt.Printf("  %s  [%s] %s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Kind, e.Message)
			}
		}
		return nil
	case "create":
		if err := need(2); err != nil {
			return err
		}
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		plan := fs.String("plan", "", "plan ID")
		kind := fs.String("kind", "customer", "customer or reseller")
		parent := fs.Int64("parent", 0, "reseller account ID")
		email := fs.String("email", "", "contact email")
		user := fs.String("user", "", "first user's username (password generated)")
		if err := fs.Parse(args[2:]); err != nil || *plan == "" {
			return usage
		}
		body := map[string]any{"name": args[1], "plan_id": *plan, "kind": *kind, "parent_id": *parent, "email": *email}
		if *user != "" {
			body["user"] = map[string]string{"username": *user}
		}
		var out struct {
			Account  accountOut `json:"account"`
			Password string     `json:"password"`
		}
		if err := call(cfg, "POST", "/accounts", body, &out); err != nil {
			return err
		}
		fmt.Printf("Account %d (%s) created on plan %s.\n", out.Account.ID, out.Account.Name, out.Account.PlanID)
		if out.Password != "" {
			fmt.Printf("  User %s, password: %s\nSave it now; it is not shown again.\n", *user, out.Password)
		}
		return nil
	case "set":
		if err := need(2); err != nil {
			return err
		}
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		fs.String("name", "", "new name")
		fs.String("plan", "", "new plan ID")
		fs.String("email", "", "new contact email")
		if err := fs.Parse(args[2:]); err != nil {
			return usage
		}
		// Only what was given changes.
		fields := map[string]string{"name": "name", "plan": "plan_id", "email": "email"}
		body := map[string]string{}
		fs.Visit(func(f *flag.Flag) { body[fields[f.Name]] = f.Value.String() })
		if len(body) == 0 {
			return usage
		}
		return call(cfg, "PUT", "/accounts/"+id, body, nil)
	case "suspend":
		if err := need(2); err != nil {
			return err
		}
		fs := flag.NewFlagSet("suspend", flag.ContinueOnError)
		reason := fs.String("reason", billing.ReasonAdmin, "admin, billing or overage")
		if err := fs.Parse(args[2:]); err != nil {
			return usage
		}
		if err := call(cfg, "POST", "/accounts/"+id+"/suspend", map[string]string{"reason": *reason}, nil); err != nil {
			return err
		}
		fmt.Println("Suspended: its sites answer 503 and their PHP is stopped; nothing is deleted.")
		return nil
	case "unsuspend":
		if err := need(2); err != nil {
			return err
		}
		return call(cfg, "POST", "/accounts/"+id+"/unsuspend", nil, nil)
	case "terminate":
		if err := need(2); err != nil {
			return err
		}
		fs := flag.NewFlagSet("terminate", flag.ContinueOnError)
		del := fs.Bool("delete-sites", false, "delete the account's sites too (irreversible)")
		if err := fs.Parse(args[2:]); err != nil {
			return usage
		}
		var out billing.TerminateResult
		if err := call(cfg, "POST", "/accounts/"+id+"/terminate", map[string]any{"confirm": args[1],
			"delete_sites": *del}, &out); err != nil {
			return err
		}
		fmt.Printf("Account %s terminated; %d site(s) deleted.\n", args[1], len(out.Deleted))
		return nil
	case "rm":
		if err := need(2); err != nil {
			return err
		}
		return call(cfg, "DELETE", "/accounts/"+id, nil, nil)
	case "usage":
		if err := need(2); err != nil {
			return err
		}
		fs := flag.NewFlagSet("usage", flag.ContinueOnError)
		measure := fs.Bool("measure", false, "measure disk use now")
		if err := fs.Parse(args[2:]); err != nil {
			return usage
		}
		var u billing.Usage
		method, path := "GET", "/accounts/"+id+"/usage"
		if *measure {
			method, path = "POST", path+"/measure"
		}
		if err := call(cfg, method, path, nil, &u); err != nil {
			return err
		}
		limit := func(n int64) string {
			if n == 0 {
				return "unlimited"
			}
			return fmtBytes(n)
		}
		fmt.Printf("%s, %s:\n  Sites:     %d of %s\n  Disk:      %s of %s\n  Bandwidth: %s of %s\n", u.Name,
			u.MonthStart.Format("January 2006"), u.Sites, fmtLimit(int64(u.MaxSites), ""), fmtBytes(u.DiskBytes),
			limit(u.DiskLimitBytes), fmtBytes(u.BandwidthBytes), limit(u.BandwidthLimitBytes))
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  SITE\tFILES\tDATABASE\tBANDWIDTH\tMEASURED")
		for _, s := range u.PerSite {
			at := "never"
			if !s.MeasuredAt.IsZero() {
				at = s.MeasuredAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", s.SiteID, fmtBytes(s.FilesBytes), fmtBytes(s.DBBytes),
				fmtBytes(s.BandwidthBytes), at)
		}
		return w.Flush()
	case "users":
		if err := need(2); err != nil {
			return err
		}
		var users []store.User
		if err := call(cfg, "GET", "/accounts/"+id+"/users", nil, &users); err != nil {
			return err
		}
		for _, u := range users {
			fmt.Printf("%s\t%s\t2FA %v\tdisabled %v\n", u.Username, u.Role, onOff(u.TOTPEnabled), u.Disabled)
		}
		return nil
	case "user-add":
		if err := need(3); err != nil {
			return err
		}
		var out struct {
			Password string `json:"password"`
		}
		if err := call(cfg, "POST", "/accounts/"+id+"/users", map[string]string{"username": args[2]}, &out); err != nil {
			return err
		}
		fmt.Printf("User %s created.\n  Password: %s\nSave it now; it is not shown again.\n", args[2], out.Password)
		return nil
	case "sso":
		if err := need(2); err != nil {
			return err
		}
		body := map[string]string{}
		if len(args) > 2 {
			body["username"] = args[2]
		}
		var out struct {
			URL       string    `json:"url"`
			Username  string    `json:"username"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := call(cfg, "POST", "/accounts/"+id+"/sso", body, &out); err != nil {
			return err
		}
		fmt.Printf("One-time sign-in link for %s (valid until %s):\n%s\n", out.Username,
			out.ExpiresAt.Local().Format("15:04:05"), out.URL)
		return nil
	case "assign":
		if err := need(3); err != nil {
			return err
		}
		n, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return usage
		}
		return call(cfg, "PUT", "/sites/"+id+"/account", map[string]int64{"account_id": n}, nil)
	}
	return fmt.Errorf("unknown account command %q", args[0])
}

// planFlags defines the flags of plan create/set on p (current values as
// defaults).
func planFlags(fs *flag.FlagSet, p *store.Plan) (features, repos *string) {
	fs.StringVar(&p.Name, "name", p.Name, "display name")
	fs.IntVar(&p.MaxSites, "sites", p.MaxSites, "sites per account, staging copies included (0: unlimited)")
	fs.Int64Var(&p.DiskMB, "disk", p.DiskMB, "disk MB per account, files + databases (0: unlimited)")
	fs.Int64Var(&p.BandwidthGB, "bandwidth", p.BandwidthGB, "bandwidth GB per month (0: unlimited)")
	fs.IntVar(&p.MaxReplicas, "replicas", p.MaxReplicas, "replicas per site (0: server limit)")
	fs.IntVar(&p.MaxMemoryMB, "memory", p.MaxMemoryMB, "memory MB per replica (0: server limit)")
	fs.Float64Var(&p.MaxCPUs, "cpus", p.MaxCPUs, "CPUs per replica (0: server limit)")
	fs.IntVar(&p.MaxDomains, "domains", p.MaxDomains, "domains per site (0: unlimited)")
	fs.Int64Var(&p.BurstMinutes, "burst-minutes", p.BurstMinutes, "burst minutes per account per month (0: unlimited; needs the burst feature)")
	fs.StringVar(&p.Overage, "overage", p.Overage, "past the bandwidth: notify or suspend")
	fs.BoolVar(&p.Resellable, "resellable", p.Resellable, "resellers may assign it")
	f, r := strings.Join(p.Features, ","), strings.Join(p.BackupRepos, ",")
	fs.StringVar(&f, "features", f, "comma-separated: "+strings.Join(billing.Features, ","))
	fs.StringVar(&r, "backup-repos", r, "backup destinations tenants may use (IDs)")
	return &f, &r
}

func splitCSV(v string) []string {
	out := []string{}
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func planCmd(cfg *config.Config, args []string) error {
	usage := errors.New("usage: wpgenie plan ls | show <id> | create <id> [flags] | set <id> [flags] | rm <id>\n" +
		"  flags: --name --sites --disk MB --bandwidth GB --replicas --memory MB --cpus --domains\n" +
		"         --features a,b --backup-repos a,b --overage notify|suspend --resellable")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "ls":
		var list []store.Plan
		if err := call(cfg, "GET", "/plans", nil, &list); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSITES\tDISK\tBANDWIDTH/MO\tREPLICAS\tMEMORY\tCPUS\tFEATURES\tOVERAGE")
		for _, p := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, fmtLimit(int64(p.MaxSites), ""),
				fmtLimit(p.DiskMB, " MB"), fmtLimit(p.BandwidthGB, " GB"), fmtLimit(int64(p.MaxReplicas), ""),
				fmtLimit(int64(p.MaxMemoryMB), " MB"), strconv.FormatFloat(p.MaxCPUs, 'g', -1, 64),
				strings.Join(p.Features, ","), p.Overage)
		}
		return w.Flush()
	case "show", "create", "set", "rm":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return usage
		}
	default:
		return fmt.Errorf("unknown plan command %q", args[0])
	}
	id := url.PathEscape(args[1])
	p := &store.Plan{ID: args[1], Name: args[1], Overage: billing.OverageNotify}
	if args[0] == "show" || args[0] == "set" {
		var list []store.Plan
		if err := call(cfg, "GET", "/plans", nil, &list); err != nil {
			return err
		}
		found := false
		for i := range list {
			if list[i].ID == args[1] {
				p, found = &list[i], true
			}
		}
		if !found {
			return fmt.Errorf("no plan %q", args[1])
		}
	}
	switch args[0] {
	case "show":
		fmt.Printf("Plan %s (%s)\n  Sites %s, disk %s, bandwidth %s/month\n  Per site: %s replicas × %s, %g CPUs, %s domains\n"+
			"  Features: %s\n  Backup destinations: %s\n  Overage: %s, resellable: %v\n", p.ID, p.Name,
			fmtLimit(int64(p.MaxSites), ""), fmtLimit(p.DiskMB, " MB"), fmtLimit(p.BandwidthGB, " GB"),
			fmtLimit(int64(p.MaxReplicas), ""), fmtLimit(int64(p.MaxMemoryMB), " MB"), p.MaxCPUs,
			fmtLimit(int64(p.MaxDomains), ""), strings.Join(p.Features, ", "), strings.Join(p.BackupRepos, ", "),
			p.Overage, p.Resellable)
		return nil
	case "rm":
		return call(cfg, "DELETE", "/plans/"+id, nil, nil)
	}
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	features, repos := planFlags(fs, p)
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() > 0 {
		return usage
	}
	p.Features, p.BackupRepos = splitCSV(*features), splitCSV(*repos)
	body := *p
	body.CreatedAt, body.UpdatedAt = time.Time{}, time.Time{}
	if args[0] == "create" {
		return call(cfg, "POST", "/plans", planBody(body), nil)
	}
	return call(cfg, "PUT", "/plans/"+id, planBody(body), nil)
}

// planBody is a plan without its timestamps (the API refuses unknown and
// read-only fields alike: they are simply not sent).
func planBody(p store.Plan) map[string]any {
	return map[string]any{"id": p.ID, "name": p.Name, "max_sites": p.MaxSites, "disk_mb": p.DiskMB,
		"bandwidth_gb": p.BandwidthGB, "max_replicas": p.MaxReplicas, "max_memory_mb": p.MaxMemoryMB,
		"max_cpus": p.MaxCPUs, "max_domains": p.MaxDomains, "features": p.Features, "backup_repos": p.BackupRepos,
		"overage": p.Overage, "resellable": p.Resellable, "burst_minutes": p.BurstMinutes}
}

func tokenCmd(cfg *config.Config, args []string) error {
	usage := errors.New("usage: wpgenie token ls | create --user NAME [--name N] [--expires-days N] | rm <id>")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "ls":
		var list []store.APIToken
		if err := call(cfg, "GET", "/tokens", nil, &list); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tUSER\tNAME\tTOKEN\tEXPIRES\tLAST USED")
		for _, t := range list {
			exp, used := "never", "never"
			if !t.ExpiresAt.IsZero() {
				exp = t.ExpiresAt.Local().Format("2006-01-02")
			}
			if !t.LastUsedAt.IsZero() {
				used = t.LastUsedAt.Local().Format("2006-01-02 15:04") + " from " + t.LastUsedIP
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Username, t.Name, t.Hint, exp, used)
		}
		return w.Flush()
	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		user := fs.String("user", "", "the user the token acts as")
		name := fs.String("name", "cli", "what the token is for")
		days := fs.Int("expires-days", 0, "expiry in days (0: never)")
		if err := fs.Parse(args[1:]); err != nil || *user == "" {
			return usage
		}
		var out struct {
			Token string `json:"token"`
		}
		if err := call(cfg, "POST", "/users/"+url.PathEscape(*user)+"/tokens",
			map[string]any{"name": *name, "expires_days": *days}, &out); err != nil {
			return err
		}
		fmt.Printf("Token for %s (acts with their role and account):\n%s\nSave it now; it is not shown again.\n", *user, out.Token)
		return nil
	case "rm":
		if len(args) != 2 {
			return usage
		}
		return call(cfg, "DELETE", "/tokens/"+url.PathEscape(args[1]), nil, nil)
	}
	return fmt.Errorf("unknown token command %q", args[0])
}
