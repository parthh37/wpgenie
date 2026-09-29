package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/monitor"
)

// alertsCmd shows firing alerts and history, tests the notification
// channels and creates the Prometheus scrape token. Thresholds and
// channels are set in the dashboard (Monitoring) or with
// PUT /api/v1/monitoring/settings.
func alertsCmd(cfg *config.Config, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "test":
			var res []monitor.ChannelResult
			if err := call(cfg, "POST", "/monitoring/test", nil, &res); err != nil {
				return err
			}
			if len(res) == 0 {
				fmt.Println("No notification channels are configured (dashboard: Monitoring).")
				return nil
			}
			failed := 0
			for _, r := range res {
				if r.OK {
					fmt.Printf("%s: sent\n", r.Channel)
				} else {
					failed++
					fmt.Printf("%s: FAILED: %s\n", r.Channel, r.Error)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d channel(s) failed", failed)
			}
			return nil
		case "metrics-token":
			var out struct{ Token string }
			if err := call(cfg, "POST", "/monitoring/metrics-token", nil, &out); err != nil {
				return err
			}
			fmt.Printf("Metrics token (shown only now; any previous token stops working):\n\n  %s\n\n", out.Token)
			fmt.Printf("Prometheus: scrape http://%s/metrics with\n  authorization:\n    type: Bearer\n    credentials: <token>\n",
				cfg.ListenAddr)
			return nil
		}
	}
	fs := flag.NewFlagSet("alerts", flag.ContinueOnError)
	history := fs.Int("history", 20, "history lines to show")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || *history < 0 || *history > 1000 {
		return errors.New("usage: wpgenie alerts [--history N] [--json] | test | metrics-token")
	}
	var o monitor.Overview
	if err := call(cfg, "GET", fmt.Sprintf("/monitoring/alerts?limit=%d", max(*history, 1)), nil, &o); err != nil {
		return err
	}
	if *history == 0 {
		o.History = nil
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(o)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if len(o.Active) == 0 {
		fmt.Fprintf(w, "No alerts firing (%d targets watched", o.Watched)
		if !o.EvaluatedAt.IsZero() {
			fmt.Fprintf(w, ", last checked %s", o.EvaluatedAt.Local().Format(time.DateTime))
		}
		fmt.Fprintln(w, ").")
	} else {
		fmt.Fprintln(w, "SEVERITY\tSINCE\tKIND\tTARGET\tMESSAGE")
		for _, a := range o.Active {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Severity, a.Since.Local().Format(time.DateTime), a.Kind, a.Target, a.Message)
		}
	}
	if len(o.History) > 0 {
		fmt.Fprintln(w, "\nTIME\tSTATE\tSEVERITY\tTARGET\tMESSAGE")
		for i := len(o.History) - 1; i >= 0; i-- {
			e := o.History[i]
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format(time.DateTime), e.State, e.Severity, e.Target, e.Message)
		}
	}
	return w.Flush()
}
