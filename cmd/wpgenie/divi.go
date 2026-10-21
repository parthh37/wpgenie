package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/site"
)

// The host's Divi (Elegant Themes) license: new sites get Divi installed
// and activated, and every site with Divi gets the license without seeing
// the key. Clients of the local API.

const diviUsage = `usage: wpgenie divi [status]
       wpgenie divi set --username U [--new-sites on|off]   (the API key is read from stdin)
       wpgenie divi new-sites on|off
       wpgenie divi check
       wpgenie divi clear

Create a dedicated API key for this panel in your Elegant Themes account
(Account -> Username & API Key), so it can be revoked on its own. Example:

  wpgenie divi set --username acme < divi-key.txt`

func diviCmd(cfg *config.Config, args []string) error {
	usage := errors.New(diviUsage)
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return usage
		}
		var v site.DiviView
		if err := call(cfg, "GET", "/settings/divi", nil, &v); err != nil {
			return err
		}
		printDivi(v)
		return nil
	}
	switch args[0] {
	case "set":
		fs := flag.NewFlagSet("divi set", flag.ContinueOnError)
		user := fs.String("username", "", "Elegant Themes username")
		newSites := fs.String("new-sites", "", "install Divi on new sites: on or off (default: on when first set)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *user == "" || fs.NArg() != 0 {
			return usage
		}
		key, err := readSecret("Elegant Themes API key (from stdin):")
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("no API key on stdin")
		}
		in := site.DiviInput{Username: user, APIKey: &key}
		if *newSites != "" {
			b, err := diviOnOff(*newSites)
			if err != nil {
				return err
			}
			in.NewSites = &b
		}
		var v site.DiviView
		if err := call(cfg, "PUT", "/settings/divi", in, &v); err != nil {
			return err
		}
		printDivi(v)
		if err := call(cfg, "POST", "/settings/divi/check", nil, nil); err != nil {
			fmt.Fprintln(os.Stderr, "note: the license was saved, but checking it failed:", err)
			return nil
		}
		fmt.Println("Elegant Themes accepted the license.")
		return nil
	case "new-sites":
		if len(args) != 2 {
			return usage
		}
		b, err := diviOnOff(args[1])
		if err != nil {
			return err
		}
		var v site.DiviView
		if err := call(cfg, "PUT", "/settings/divi", site.DiviInput{NewSites: &b}, &v); err != nil {
			return err
		}
		printDivi(v)
		return nil
	case "check":
		if len(args) != 1 {
			return usage
		}
		var out struct {
			Message string `json:"message"`
		}
		if err := call(cfg, "POST", "/settings/divi/check", nil, &out); err != nil {
			return err
		}
		fmt.Println(out.Message)
		return nil
	case "clear":
		if len(args) != 1 {
			return usage
		}
		empty := ""
		if err := call(cfg, "PUT", "/settings/divi", site.DiviInput{Username: &empty, APIKey: &empty}, nil); err != nil {
			return err
		}
		fmt.Println("Divi license removed: sites keep Divi, without updates or premade layouts; new sites don't get it.")
		return nil
	}
	return usage
}

func printDivi(v site.DiviView) {
	if !v.Configured {
		fmt.Println("Divi license: not set up")
		return
	}
	key := "saved"
	if v.KeyHint != "" {
		key = "saved (ends " + v.KeyHint + ")"
	}
	fmt.Printf("Divi license: %s\nAPI key: %s\nInstall on new sites: %v\n", v.Username, key, v.NewSites)
}

func diviOnOff(v string) (bool, error) {
	switch v {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fmt.Errorf("%q: expected on or off", v)
}

// siteDiviCmd installs and activates Divi on an existing site.
func siteDiviCmd(cfg *config.Config, id string) error {
	if _, _, err := startJob(cfg, "POST", "/sites/"+id+"/divi", nil); err != nil {
		return err
	}
	fmt.Println("Divi is active on", id, "with the host's license.")
	return nil
}
