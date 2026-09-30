package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/wplogin"
)

// WordPress itself: wp-admin sign-in links, administrators' passwords,
// performance tweaks and the site analyser. Clients of the local API.

// wpCmd: wpgenie site wp <site-id> login [user-id] | users | password <user-id>.
func wpCmd(cfg *config.Config, id string, args []string) error {
	usage := errors.New("usage: wpgenie site wp <site-id> login [user-id] | users | password <user-id>")
	base := "/sites/" + id + "/wp-admin"
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "users":
		var users []site.WPUser
		if err := call(cfg, "GET", base+"/users", nil, &users); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tLOGIN\tEMAIL\tNAME")
		for _, u := range users {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", u.ID, u.Login, u.Email, u.Name)
		}
		return w.Flush()
	case "login":
		in := map[string]int{"user_id": 0}
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return usage
			}
			in["user_id"] = n
		}
		var out wplogin.Link
		if err := call(cfg, "POST", base+"/login", in, &out); err != nil {
			return err
		}
		fmt.Printf("Signs in as %s. Open within %s (single use):\n\n  %s\n",
			out.User, time.Until(out.ExpiresAt).Round(time.Second), out.URL)
		return nil
	case "password":
		if len(args) != 2 {
			return usage
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return usage
		}
		var out site.PasswordReset
		if err := call(cfg, "POST", base+"/password", site.PasswordInput{UserID: n}, &out); err != nil {
			return err
		}
		fmt.Printf("New password for %s (shown once; their sessions were ended):\n\n  %s\n", out.User, out.Password)
		return nil
	}
	return usage
}

// optimizeCmd: wpgenie site optimize <site-id> [ls | recommended | off |
// key,key,... | cleanup].
func optimizeCmd(cfg *config.Config, id string, args []string) error {
	path := "/sites/" + id + "/optimize"
	arg := "ls"
	if len(args) > 0 {
		arg = args[0]
	}
	var keys []string
	switch arg {
	case "ls":
		var st struct {
			Optimize []string `json:"optimize"`
		}
		if err := call(cfg, "GET", "/sites/"+id, nil, &st); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ON\tKEY\tTWEAK")
		for _, o := range site.Optimizations {
			on := " "
			for _, k := range st.Optimize {
				if k == o.Key {
					on = "✓"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", on, o.Key, o.Title)
		}
		return w.Flush()
	case "cleanup":
		var r site.CleanupResult
		if err := call(cfg, "POST", path+"/cleanup", nil, &r); err != nil {
			return err
		}
		fmt.Printf("Removed %d expired transients, %d auto-drafts, %d spam comments, %d old revisions.\n",
			r.Transients, r.AutoDrafts, r.Spam, r.Revisions)
		return nil
	case "recommended":
		keys = site.DefaultOptimizations()
	case "off":
		keys = []string{}
	default:
		keys = strings.Split(arg, ",")
	}
	return call(cfg, "PUT", path, site.OptimizeInput{Optimizations: keys}, nil)
}

// analyseCmd: wpgenie site analyse <site-id> [--fix <fix>].
func analyseCmd(cfg *config.Config, id string, args []string) error {
	if len(args) == 2 && args[0] == "--fix" {
		var r site.FixResult
		if err := call(cfg, "POST", "/sites/"+id+"/analysis/fix", map[string]string{"fix": args[1]}, &r); err != nil {
			return err
		}
		fmt.Println(r.Message)
		return nil
	}
	if len(args) != 0 {
		return errors.New("usage: wpgenie site analyse <site-id> [--fix <fix>]")
	}
	var a site.Analysis
	if err := call(cfg, "GET", "/sites/"+id+"/analysis", nil, &a); err != nil {
		return err
	}
	fmt.Printf("Score %d/100 (%s)", a.Score, a.Grade)
	if f := a.Facts; f != nil {
		fmt.Printf(" · WordPress %s on PHP %s · %d active plugins", f.WPVersion, f.PHPVersion, f.PluginsActive)
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "\nSEVERITY\tCATEGORY\tFINDING\tFIX")
	for _, f := range a.Findings {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Severity, f.Category, f.Title, f.Fix)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, e := range a.Errors {
		fmt.Println("note:", e)
	}
	return nil
}

// brandingCmd: wpgenie branding [show | set [--name N] [--url U] [--logo FILE|none]].
func brandingCmd(cfg *config.Config, args []string) error {
	usage := errors.New("usage: wpgenie branding [show | set [--name NAME] [--url URL] [--logo FILE|none]]")
	if len(args) == 0 || args[0] == "show" {
		var b site.BrandingView
		if err := call(cfg, "GET", "/settings/branding", nil, &b); err != nil {
			return err
		}
		fmt.Printf("Name: %s\nLink: %s\nLogo: %v %s\nShown in WordPress: %v\n", b.Name, b.URL, b.HasLogo, b.LogoType, b.Enabled)
		return nil
	}
	if args[0] != "set" {
		return usage
	}
	var cur site.BrandingView
	if err := call(cfg, "GET", "/settings/branding", nil, &cur); err != nil {
		return err
	}
	in := site.BrandingInput{Name: cur.Name, URL: cur.URL}
	rest := args[1:]
	for len(rest) >= 2 {
		switch rest[0] {
		case "--name":
			in.Name = rest[1]
		case "--url":
			in.URL = rest[1]
		case "--logo":
			logo := ""
			if rest[1] != "none" {
				b, err := os.ReadFile(rest[1])
				if err != nil {
					return err
				}
				logo = dataURI(rest[1], b)
			}
			in.Logo = &logo
		default:
			return usage
		}
		rest = rest[2:]
	}
	if len(rest) != 0 {
		return usage
	}
	return call(cfg, "PUT", "/settings/branding", in, nil)
}

// dataURI encodes a logo file for the API; the server checks the bytes
// really are what the type says.
func dataURI(name string, b []byte) string {
	ctype := http.DetectContentType(b)
	if strings.HasSuffix(strings.ToLower(name), ".svg") {
		ctype = "image/svg+xml"
	}
	return "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(b)
}
