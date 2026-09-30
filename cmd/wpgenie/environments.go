package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// CLI for Phase 2: jobs, backups, staging, domains, certificates, PHP,
// SFTP and phpMyAdmin. Like the rest of the CLI, a client of the local API.

type jobView struct {
	Job    store.Job      `json:"job"`
	Secret map[string]any `json:"secret"`
}

// waitJob follows a job until it ends, printing its steps.
func waitJob(cfg *config.Config, id int64) (*jobView, error) {
	last := ""
	for {
		var v jobView
		if err := call(cfg, "GET", "/jobs/"+strconv.FormatInt(id, 10), nil, &v); err != nil {
			return nil, err
		}
		if line := fmt.Sprintf("%3d%%  %s", v.Job.Progress, v.Job.Step); v.Job.Step != "" && line != last {
			fmt.Fprintln(os.Stderr, line)
			last = line
		}
		switch v.Job.Status {
		case store.JobSucceeded:
			return &v, nil
		case store.JobFailed:
			return &v, fmt.Errorf("job %d (%s) failed: %s", id, v.Job.Kind, v.Job.Error)
		}
		time.Sleep(2 * time.Second)
	}
}

// startJob calls an endpoint that answers 202 with a job ID and follows it.
func startJob(cfg *config.Config, method, path string, body any) (*jobView, map[string]any, error) {
	var out map[string]any
	if err := call(cfg, method, path, body, &out); err != nil {
		return nil, nil, err
	}
	id, _ := out["job_id"].(float64)
	fmt.Fprintf(os.Stderr, "Job %d started.\n", int64(id))
	v, err := waitJob(cfg, int64(id))
	return v, out, err
}

func jobsCmd(cfg *config.Config, args []string) error {
	if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return errors.New("usage: wpgenie jobs [<job-id>] [--site ID] [--active]")
		}
		v, err := waitJob(cfg, id)
		if v != nil {
			printJSON(v.Job)
		}
		return err
	}
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	siteID := fs.String("site", "", "only this site's jobs")
	active := fs.Bool("active", false, "only queued and running jobs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{"site": {*siteID}}
	if *active {
		q.Set("active", "1")
	}
	var list []store.Job
	if err := call(cfg, "GET", "/jobs?"+q.Encode(), nil, &list); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSITE\tKIND\tSTATUS\tPROGRESS\tBY\tSTARTED\tSTEP / ERROR")
	for _, j := range list {
		detail := j.Step
		if j.Error != "" {
			detail = j.Error
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d%%\t%s\t%s\t%s\n", j.ID, j.SiteID, j.Kind, j.Status, j.Progress, j.Actor,
			j.CreatedAt.Local().Format("01-02 15:04"), truncate(detail, 80))
	}
	return w.Flush()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func createSiteCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("site create", flag.ContinueOnError)
	node := fs.String("node", "", "server to create it on (default: the one with the most room; \"local\": the panel's)")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: wpgenie site create <domain> <admin-email> [--node ID]")
	}
	v, out, err := startJob(cfg, "POST", "/sites", struct {
		site.CreateInput
		Node string `json:"node,omitempty"`
	}{site.CreateInput{Domain: fs.Arg(0), AdminEmail: fs.Arg(1)}, *node})
	if err != nil {
		return err
	}
	c := v.Secret
	st, _ := out["site"].(map[string]any)
	fmt.Printf("Site %v created (%v)\n\n  Admin URL: %v\n  Username:  %v\n  Password:  %v\n\nSave the password now; it is not stored.\n",
		st["id"], c["url"], c["admin_url"], c["username"], c["password"])
	call(cfg, "DELETE", fmt.Sprintf("/jobs/%d/secret", v.Job.ID), nil, nil)
	return nil
}

// reorderFlags moves flags ahead of positional arguments, so they may come
// last ("site create a.com me@a.com --node web-2") as the usage shows.
func reorderFlags(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, args[i])
	}
	return append(flags, rest...)
}

// ---- Backups ----

func siteBackupCmd(cfg *config.Config, id string, args []string) error {
	usage := errors.New(`usage: wpgenie site backup <site-id> [now | ls | restore <repo> <backup> [--files-only|--db-only]
       | download <repo> <backup> [file] | rm <repo> <backup>
       | policy [--repo ID|none] [--every HOURS] [--keep-last N] [--keep-daily N] [--keep-weekly N] [--keep-monthly N]]`)
	sub := "now"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	base := "/sites/" + id + "/backups"
	switch sub {
	case "now":
		v, _, err := startJob(cfg, "POST", base, nil)
		if err == nil {
			fmt.Println("Backed up:", v.Job.Result)
		}
		return err
	case "ls":
		var out struct {
			Policy  *store.BackupPolicy `json:"policy"`
			Backups []site.BackupInfo   `json:"backups"`
			Errors  map[string]string   `json:"errors"`
		}
		if err := call(cfg, "GET", base, nil, &out); err != nil {
			return err
		}
		if p := out.Policy; p != nil {
			fmt.Printf("Policy: every %dh to %s, keep last %d / daily %d / weekly %d / monthly %d; last backup %s %s\n\n",
				p.IntervalHours, p.RepoID, p.KeepLast, p.KeepDaily, p.KeepWeekly, p.KeepMonthly,
				fmtTime(p.LastBackupAt), p.LastError)
		} else {
			fmt.Print("No scheduled backups (manual backups go to the local repository).\n\n")
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "BACKUP\tREPO\tTIME\tKIND\tSIZE\tNEW DATA")
		for _, b := range out.Backups {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", b.ShortID, b.RepoID, fmtTime(b.Time), b.Kind, human(b.Size), human(b.Added))
		}
		for r, e := range out.Errors {
			fmt.Fprintf(w, "(repository %s unreadable: %s)\n", r, e)
		}
		return w.Flush()
	case "restore":
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		filesOnly := fs.Bool("files-only", false, "restore files only")
		dbOnly := fs.Bool("db-only", false, "restore the database only")
		if len(args) < 2 || fs.Parse(args[2:]) != nil || (*filesOnly && *dbOnly) {
			return usage
		}
		in := site.RestoreInput{RepoID: args[0], BackupID: args[1], Files: !*dbOnly, Database: !*filesOnly}
		v, _, err := startJob(cfg, "POST", base+"/restore", in)
		if err == nil {
			fmt.Println("Restored:", v.Job.Result)
		}
		return err
	case "download":
		if len(args) < 2 || len(args) > 3 {
			return usage
		}
		w := io.Writer(os.Stdout)
		if len(args) == 3 {
			f, err := os.OpenFile(args[2], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		return download(cfg, base+"/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1])+"/download", w)
	case "rm":
		if len(args) != 2 {
			return usage
		}
		return call(cfg, "DELETE", base+"/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1]), nil, nil)
	case "policy":
		var cur struct {
			Policy *store.BackupPolicy `json:"policy"`
		}
		if err := call(cfg, "GET", base, nil, &cur); err != nil {
			return err
		}
		in := site.PolicyInput{RepoID: site.LocalRepoID, IntervalHours: 24, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
		if p := cur.Policy; p != nil {
			in = site.PolicyInput{RepoID: p.RepoID, IntervalHours: p.IntervalHours, KeepLast: p.KeepLast,
				KeepDaily: p.KeepDaily, KeepWeekly: p.KeepWeekly, KeepMonthly: p.KeepMonthly}
		}
		fs := flag.NewFlagSet("policy", flag.ContinueOnError)
		fs.StringVar(&in.RepoID, "repo", in.RepoID, `repository ("none": no scheduled backups)`)
		fs.IntVar(&in.IntervalHours, "every", in.IntervalHours, "hours between backups (0: manual only)")
		fs.IntVar(&in.KeepLast, "keep-last", in.KeepLast, "keep the newest N")
		fs.IntVar(&in.KeepDaily, "keep-daily", in.KeepDaily, "keep one a day for N days")
		fs.IntVar(&in.KeepWeekly, "keep-weekly", in.KeepWeekly, "keep one a week for N weeks")
		fs.IntVar(&in.KeepMonthly, "keep-monthly", in.KeepMonthly, "keep one a month for N months")
		if err := fs.Parse(args); err != nil {
			return usage
		}
		if in.RepoID == "none" {
			in = site.PolicyInput{}
		}
		var out *store.BackupPolicy
		if err := call(cfg, "PUT", base+"/policy", in, &out); err != nil {
			return err
		}
		if out == nil {
			fmt.Println("Scheduled backups off.")
			return nil
		}
		return printJSON(out)
	}
	return usage
}

func download(cfg *config.Config, path string, w io.Writer) error {
	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://"+cfg.ListenAddr+"/api/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("is the daemon running? %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	n, err := io.Copy(w, resp.Body)
	if err == nil {
		fmt.Fprintf(os.Stderr, "%s downloaded.\n", human(n))
	}
	return err
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func human(n int64) string {
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

func backupCmd(cfg *config.Config, args []string) error {
	usage := errors.New(`usage: wpgenie backup repos
       wpgenie backup repo add local <name> <path>
       wpgenie backup repo add s3 <name> --bucket B [--endpoint host] [--prefix P] [--region R] --key-id ID  (secret key on stdin)
       wpgenie backup repo add b2 <name> --bucket B [--prefix P] --key-id ID  (application key on stdin)
       wpgenie backup repo add sftp <name> --host H [--port 22] --user U --path P
       wpgenie backup repo check|rm|password <repo>
       wpgenie backup ls <repo>                      every backup in it, deleted sites' too
       wpgenie backup restore-new <repo> <backup> <domain>
       Secrets are read from stdin, never the command line: for s3/b2 the secret key, then (with
       --existing) the repository password on the next line; for local/sftp with --existing, the password.`)
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "repos":
		var repos []map[string]any
		if err := call(cfg, "GET", "/backups/repos", nil, &repos); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tKIND\tLOCATION\tSITES\tCHECK")
		for _, r := range repos {
			check := "ok"
			if e, _ := r["check_error"].(string); e != "" {
				check = e
			} else if r["checked_at"] == nil {
				check = "-"
			}
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%s\n", r["id"], r["name"], r["kind"], r["location"], r["sites_using"], truncate(check, 60))
		}
		return w.Flush()
	case "repo":
		if len(args) < 3 {
			return usage
		}
		switch args[1] {
		case "add":
			return addRepoCmd(cfg, args[2:], usage)
		case "check":
			var out map[string]any
			if err := call(cfg, "POST", "/backups/repos/"+url.PathEscape(args[2])+"/check", nil, &out); err != nil {
				return err
			}
			if e, _ := out["check_error"].(string); e != "" {
				return errors.New(e)
			}
			fmt.Println("Repository OK.")
			return nil
		case "rm":
			return call(cfg, "DELETE", "/backups/repos/"+url.PathEscape(args[2]), nil, nil)
		case "password":
			var out map[string]string
			if err := call(cfg, "POST", "/backups/repos/"+url.PathEscape(args[2])+"/password", nil, &out); err != nil {
				return err
			}
			fmt.Println(out["password"])
			return nil
		}
	case "ls":
		if len(args) != 2 {
			return usage
		}
		var list []site.BackupInfo
		if err := call(cfg, "GET", "/backups/repos/"+url.PathEscape(args[1])+"/backups", nil, &list); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "BACKUP\tSITE\tDOMAIN\tTIME\tKIND\tSIZE")
		for _, b := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", b.ShortID, b.SiteID, b.Domain, fmtTime(b.Time), b.Kind, human(b.Size))
		}
		return w.Flush()
	case "restore-new":
		if len(args) != 4 {
			return usage
		}
		v, out, err := startJob(cfg, "POST", "/backups/restore-new", site.RestoreAsNewInput{RepoID: args[1], BackupID: args[2], Domain: args[3]})
		if err != nil {
			return err
		}
		st, _ := out["site"].(map[string]any)
		fmt.Printf("Site %v created from backup %s: %s\n", st["id"], args[2], v.Job.Result)
		return nil
	}
	return usage
}

func addRepoCmd(cfg *config.Config, args []string, usage error) error {
	if len(args) < 2 {
		return usage
	}
	in := site.RepoInput{Kind: args[0], Name: args[1]}
	fs := flag.NewFlagSet("repo", flag.ContinueOnError)
	fs.StringVar(&in.Bucket, "bucket", "", "bucket")
	fs.StringVar(&in.Endpoint, "endpoint", "", "S3 endpoint (default AWS)")
	fs.StringVar(&in.Prefix, "prefix", "", "path inside the bucket")
	fs.StringVar(&in.Region, "region", "", "S3 region")
	fs.StringVar(&in.KeyID, "key-id", "", "access key ID")
	fs.StringVar(&in.Host, "host", "", "SFTP host")
	fs.IntVar(&in.Port, "port", 22, "SFTP port")
	fs.StringVar(&in.User, "user", "", "SFTP user")
	fs.StringVar(&in.Path, "path", "", "directory")
	existing := fs.Bool("existing", false, "attach an existing repository (its password on stdin)")
	rest := args[2:]
	if in.Kind == "local" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		in.Path, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return usage
	}
	var lines []string
	if in.Kind == "s3" || in.Kind == "b2" || *existing {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
		if err != nil {
			return err
		}
		lines = strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	}
	if in.Kind == "s3" || in.Kind == "b2" {
		in.Secret, lines = strings.TrimSpace(lines[0]), lines[1:]
	}
	if *existing {
		if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
			return errors.New("--existing: give the repository password on stdin")
		}
		in.Password = strings.TrimSpace(lines[0])
	}
	var out struct {
		Repo     map[string]any `json:"repo"`
		Password string         `json:"password"`
	}
	if err := call(cfg, "POST", "/backups/repos", in, &out); err != nil {
		return err
	}
	fmt.Printf("Repository %v added.\n", out.Repo["id"])
	if out.Password != "" {
		fmt.Printf("\nRepository password (store it somewhere safe, off this server: without it these\nbackups can't be restored if the server is lost):\n\n  %s\n", out.Password)
	}
	if pk, _ := out.Repo["public_key"].(string); pk != "" {
		fmt.Printf("\nAdd this key to ~/.ssh/authorized_keys of %s on %s, then run `wpgenie backup repo check %v`:\n\n  %s\n\nServer key pinned: %v\n",
			in.User, in.Host, out.Repo["id"], pk, out.Repo["host_key_fingerprint"])
	}
	if e, _ := out.Repo["check_error"].(string); e != "" {
		fmt.Println("\nNot connected yet:", e)
	}
	return nil
}

// ---- Staging, domains, certificates, PHP ----

func stagingCmd(cfg *config.Config, id string, args []string) error {
	in := site.StagingInput{}
	if len(args) == 1 {
		in.Domain = args[0]
	} else if len(args) > 1 {
		return errors.New("usage: wpgenie site staging <site-id> [domain]  (default staging.<domain>)")
	}
	v, out, err := startJob(cfg, "POST", "/sites/"+id+"/staging", in)
	if err != nil {
		return err
	}
	st, _ := out["site"].(map[string]any)
	fmt.Printf("Staging site %v ready: %s\n", st["id"], v.Job.Result)
	return nil
}

func pushCmd(cfg *config.Config, id string, args []string) error {
	var in site.PushInput
	var tables listFlag
	tables.v = &in.Tables
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.StringVar(&in.Files, "files", "", "code (everything but uploads) or all")
	fs.BoolVar(&in.Database, "db", false, "push the database")
	fs.Var(&tables, "tables", "only these tables (comma-separated)")
	if err := fs.Parse(args); err != nil || (in.Files == "" && !in.Database) {
		return errors.New("usage: wpgenie site push <staging-id> [--files code|all] [--db [--tables t1,t2]]")
	}
	if len(in.Tables) > 0 {
		in.Database = true
	}
	v, _, err := startJob(cfg, "POST", "/sites/"+id+"/push", in)
	if err == nil {
		fmt.Println("Pushed to the live site:", v.Job.Result)
	}
	return err
}

func domainCmd(cfg *config.Config, id string, args []string) error {
	usage := errors.New("usage: wpgenie site domain <site-id> add <domain> [--redirect] | rm <domain> | redirect <domain> on|off | primary <domain>")
	if len(args) < 2 {
		return usage
	}
	base := "/sites/" + id
	var out store.Site
	var err error
	switch args[0] {
	case "add":
		redirect := len(args) == 3 && args[2] == "--redirect"
		if len(args) > 3 || (len(args) == 3 && !redirect) {
			return usage
		}
		err = call(cfg, "POST", base+"/domains", map[string]any{"domain": args[1], "redirect": redirect}, &out)
	case "rm":
		err = call(cfg, "DELETE", base+"/domains/"+url.PathEscape(args[1]), nil, &out)
	case "redirect":
		var on onOff
		if len(args) != 3 || on.Set(args[2]) != nil {
			return usage
		}
		err = call(cfg, "PUT", base+"/domains/"+url.PathEscape(args[1]), map[string]bool{"redirect": bool(on)}, &out)
	case "primary":
		_, _, err = startJob(cfg, "PUT", base+"/primary-domain", map[string]string{"domain": args[1]})
		if err != nil {
			return err
		}
		err = call(cfg, "GET", base, nil, &out)
	default:
		return usage
	}
	if err != nil {
		return err
	}
	fmt.Printf("Primary: %s\nServed:  %s\nRedirect to primary: %s\n", out.PrimaryDomain, strings.Join(out.Domains, ", "),
		strings.Join(out.RedirectDomains, ", "))
	return nil
}

func certCmd(cfg *config.Config, id string, args []string) error {
	usage := errors.New("usage: wpgenie site cert <site-id> [show | set <cert.pem> <key.pem> | rm]")
	base := "/sites/" + id + "/certificate"
	if len(args) == 0 || args[0] == "show" {
		var c *store.SiteCert
		if err := call(cfg, "GET", base, nil, &c); err != nil {
			return err
		}
		if c == nil {
			fmt.Println("Automatic certificates (Let's Encrypt).")
			return nil
		}
		return printJSON(c)
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			return usage
		}
		cert, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		key, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		var c store.SiteCert
		if err := call(cfg, "PUT", base, site.CertInput{Certificate: string(cert), Key: string(key)}, &c); err != nil {
			return err
		}
		fmt.Printf("Certificate for %s installed, expires %s.\n", strings.Join(c.Names, ", "), fmtTime(c.NotAfter))
		if !c.Trusted {
			fmt.Println("Note: browsers don't trust it directly (fine behind Cloudflare with an origin certificate).")
		}
		return nil
	case "rm":
		return call(cfg, "DELETE", base, nil, nil)
	}
	return usage
}

func phpCmd(cfg *config.Config, args []string) error {
	var in site.PHPInput
	st, err := siteFlags(cfg, args, "usage: wpgenie site php <site-id> [--version 8.2|8.3|8.4] [--memory-limit MB] "+
		"[--upload-max MB] [--max-execution-time S] [--max-input-vars N]  (0 = default)",
		func(fs *flag.FlagSet, st *store.Site) {
			in = site.PHPInput{Version: st.PHPVersion, Settings: st.PHP}
			fs.StringVar(&in.Version, "version", in.Version, "PHP version")
			fs.IntVar(&in.Settings.MemoryLimitMB, "memory-limit", in.Settings.MemoryLimitMB, "memory_limit in MB")
			fs.IntVar(&in.Settings.UploadMaxMB, "upload-max", in.Settings.UploadMaxMB, "largest upload in MB")
			fs.IntVar(&in.Settings.MaxExecutionTime, "max-execution-time", in.Settings.MaxExecutionTime, "seconds")
			fs.IntVar(&in.Settings.MaxInputVars, "max-input-vars", in.Settings.MaxInputVars, "max_input_vars")
		})
	if err != nil {
		return err
	}
	if len(args) == 1 {
		fmt.Printf("Site %s: PHP %s, settings %+v\n", st.ID, st.PHPVersion, st.PHP)
		return nil
	}
	_, _, err = startJob(cfg, "PUT", "/sites/"+st.ID+"/php", in)
	if err == nil {
		fmt.Printf("Site %s runs PHP %s.\n", st.ID, in.Version)
	}
	return err
}

// ---- SFTP and phpMyAdmin ----

func sftpCmd(cfg *config.Config, id string, args []string) error {
	usage := errors.New(`usage: wpgenie site sftp <site-id> [ls | add [--suffix NAME] [--password] [--key FILE] | rm <login>
       | passwd <login> | nopasswd <login> | keys <login> <authorized_keys-file>]`)
	base := "/sites/" + id + "/sftp"
	if len(args) == 0 || args[0] == "ls" {
		var out struct {
			Users  []store.SFTPUser `json:"users"`
			Server struct {
				Running bool     `json:"running"`
				Port    int      `json:"port"`
				HostKey []string `json:"host_keys"`
			} `json:"server"`
			Host string `json:"host"`
		}
		if err := call(cfg, "GET", base, nil, &out); err != nil {
			return err
		}
		fmt.Printf("Connect: sftp -P %d <login>@%s   (server running: %v)\n", out.Server.Port, out.Host, out.Server.Running)
		for _, k := range out.Server.HostKey {
			fmt.Println("Server key:", k)
		}
		fmt.Println()
		for _, u := range out.Users {
			fmt.Printf("%s\tpassword: %v\tkeys: %d\n", u.Username, u.HasPass, len(u.PublicKeys))
		}
		return nil
	}
	readKeys := func(path string) ([]string, error) {
		b, err := os.ReadFile(path)
		return []string{string(b)}, err
	}
	switch args[0] {
	case "add":
		var in struct {
			Suffix     string   `json:"suffix"`
			Password   bool     `json:"password"`
			PublicKeys []string `json:"public_keys"`
		}
		fs := flag.NewFlagSet("sftp", flag.ContinueOnError)
		fs.StringVar(&in.Suffix, "suffix", "", "login name <site>-<suffix> (default: the site ID)")
		fs.BoolVar(&in.Password, "password", false, "generate a password")
		key := fs.String("key", "", "file with public keys (authorized_keys format)")
		if err := fs.Parse(args[1:]); err != nil {
			return usage
		}
		if *key != "" {
			k, err := readKeys(*key)
			if err != nil {
				return err
			}
			in.PublicKeys = k
		}
		var out struct {
			User     store.SFTPUser `json:"user"`
			Password string         `json:"password"`
		}
		if err := call(cfg, "POST", base, in, &out); err != nil {
			return err
		}
		fmt.Printf("SFTP login %s created.\n", out.User.Username)
		if out.Password != "" {
			fmt.Printf("Password (shown once): %s\n", out.Password)
		}
		return nil
	case "rm":
		if len(args) != 2 {
			return usage
		}
		return call(cfg, "DELETE", base+"/"+url.PathEscape(args[1]), nil, nil)
	case "passwd", "nopasswd":
		if len(args) != 2 {
			return usage
		}
		var out map[string]string
		if err := call(cfg, "PUT", base+"/"+url.PathEscape(args[1])+"/password", map[string]bool{"enabled": args[0] == "passwd"}, &out); err != nil {
			return err
		}
		if out["password"] != "" {
			fmt.Printf("New password (shown once): %s\n", out["password"])
		} else {
			fmt.Println("Password removed: keys only.")
		}
		return nil
	case "keys":
		if len(args) != 3 {
			return usage
		}
		k, err := readKeys(args[2])
		if err != nil {
			return err
		}
		return call(cfg, "PUT", base+"/"+url.PathEscape(args[1])+"/keys", map[string][]string{"public_keys": k}, nil)
	}
	return usage
}

func phpMyAdminCmd(cfg *config.Config, id string) error {
	var out struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := call(cfg, "POST", "/sites/"+id+"/phpmyadmin", nil, &out); err != nil {
		return err
	}
	fmt.Printf("Open within %s (single use):\n\n  %s\n\nThe session ends after 15 minutes idle or an hour.\n",
		time.Until(out.ExpiresAt).Round(time.Second), out.URL)
	return nil
}
