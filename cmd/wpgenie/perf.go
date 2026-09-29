package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// CLI for Phase 3: image optimisation, performance insights and the CDN
// providers.

func imagesCmd(cfg *config.Config, args []string) error {
	const usage = "usage: wpgenie site images <site-id> [status | avif,webp | webp | avif | off | convert]"
	if len(args) < 1 || len(args) > 2 {
		return errors.New(usage)
	}
	id := url.PathEscape(args[0])
	sub := "status"
	if len(args) == 2 {
		sub = args[1]
	}
	switch sub {
	case "status":
		var st store.Site
		if err := call(cfg, "GET", "/sites/"+id, nil, &st); err != nil {
			return err
		}
		if len(st.ImageFormats) == 0 {
			fmt.Println("Image optimisation off.")
		} else {
			fmt.Printf("Uploads are served as %s to browsers that accept them.\n", strings.ToUpper(strings.Join(st.ImageFormats, ", ")))
		}
		return nil
	case "convert":
		v, _, err := startJob(cfg, "POST", "/sites/"+id+"/images/convert", nil)
		if v != nil && v.Job.Result != "" {
			fmt.Println(v.Job.Result)
		}
		return err
	}
	formats := []string{}
	if sub != "off" {
		formats = strings.Split(sub, ",")
	}
	var out struct {
		Site  store.Site `json:"site"`
		JobID int64      `json:"job_id"`
	}
	if err := call(cfg, "PUT", "/sites/"+id+"/images", site.ImagesInput{Formats: formats}, &out); err != nil {
		return err
	}
	if out.JobID == 0 {
		fmt.Println("Nothing changed.")
		return nil
	}
	fmt.Fprintf(os.Stderr, "Job %d started.\n", out.JobID)
	v, err := waitJob(cfg, out.JobID)
	if v != nil && v.Job.Result != "" {
		fmt.Println(v.Job.Result)
	}
	return err
}

func insightsCmd(cfg *config.Config, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: wpgenie site insights <site-id> [--hours N] [--json] [--clear-errors]")
	}
	id := url.PathEscape(args[0])
	fs := flag.NewFlagSet("insights", flag.ContinueOnError)
	hours := fs.Int("hours", 24, "period, in hours (up to 2160)")
	asJSON := fs.Bool("json", false, "print the raw report")
	clear := fs.Bool("clear-errors", false, "forget the site's PHP errors (after fixing them)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *clear {
		if err := call(cfg, "DELETE", "/sites/"+id+"/insights/errors", nil, nil); err != nil {
			return err
		}
		fmt.Println("PHP errors cleared.")
		return nil
	}
	var in site.Insights
	if err := call(cfg, "GET", fmt.Sprintf("/sites/%s/insights?hours=%d", id, *hours), nil, &in); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(in)
	}
	p := in.Perf.Totals
	fmt.Printf("Last %d h: %d PHP responses, average %.0f ms, median %.0f ms, 95%% within %.0f ms, 99%% within %.0f ms; %d took over %d ms\n",
		*hours, p.PHPRequests, in.Perf.AvgMS, in.Perf.P50MS, in.Perf.P95MS, in.Perf.P99MS, p.Slow, store.SlowMS)
	if pages := p.CacheHits + p.CacheMisses; pages > 0 {
		fmt.Printf("Page cache: %d hits, %d misses (%.0f%% served without PHP)\n", p.CacheHits, p.CacheMisses, 100*float64(p.CacheHits)/float64(pages))
	}
	if l := in.Live; l != nil {
		fmt.Printf("Now: CPU %.0f%%", l.Percent)
		if l.Workers != nil {
			fmt.Printf(", PHP workers %.0f%% busy, %d queued", *l.Workers, l.Queued)
		}
		if l.P95MS != nil {
			fmt.Printf(", 95%% of the last minute's responses within %.0f ms", *l.P95MS)
		}
		fmt.Printf(" (%d replica(s))\n", l.Replicas)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if len(in.Slow) > 0 {
		fmt.Fprintln(w, "\nSLOW URL\tCOUNT\tAVG MS\tMAX MS\tSTATUS")
		for _, s := range in.Slow {
			fmt.Fprintf(w, "%s %s\t%d\t%d\t%d\t%d\n", s.Method, s.Path, s.Count, s.AvgMS, s.MaxMS, s.LastStatus)
		}
	}
	if len(in.Errors) > 0 {
		fmt.Fprintln(w, "\nPHP ERROR\tSOURCE\tWHERE\tCOUNT\tLAST SEEN")
		for _, e := range in.Errors {
			msg := e.Message
			if len(msg) > 90 {
				msg = msg[:90] + "…"
			}
			fmt.Fprintf(w, "%s: %s\t%s\t%s:%d\t%d\t%s\n", e.Level, msg, e.Source, e.File, e.Line, e.Count, e.LastSeen.Local().Format("2006-01-02 15:04"))
		}
	}
	return w.Flush()
}

// readSecret reads a token or key from stdin: command lines are visible to
// every user in ps.
func readSecret(prompt string) (string, error) {
	fmt.Fprintln(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("nothing on stdin")
	}
	return strings.TrimSpace(line), nil
}
