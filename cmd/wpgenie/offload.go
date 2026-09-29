package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/site"
)

const offloadUsage = `usage: wpgenie site offload <site-id> [status]
       wpgenie site offload <site-id> on --endpoint URL --bucket NAME --public-url URL [--region R] [--prefix P]
                              [--access-key-id ID] [--local-days N] [--acl public-read|none]
                                  (the secret key is read from stdin; empty keeps the stored one)
       wpgenie site offload <site-id> off [--force]
       wpgenie site offload <site-id> sync | download`

// offloadCmd: uploads offload to S3-compatible storage.
func offloadCmd(cfg *config.Config, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(offloadUsage)
	}
	id := url.PathEscape(args[0])
	sub := "status"
	if len(args) > 1 {
		sub = args[1]
	}
	var st site.OffloadStatus
	switch sub {
	case "status":
		if len(args) > 2 {
			return errors.New(offloadUsage)
		}
		if err := call(cfg, "GET", "/sites/"+id+"/offload", nil, &st); err != nil {
			return err
		}
	case "on":
		var cur site.OffloadStatus
		if err := call(cfg, "GET", "/sites/"+id+"/offload", nil, &cur); err != nil {
			return err
		}
		// Unspecified flags keep the current settings.
		in := site.OffloadInput{Enabled: true, Endpoint: cur.Endpoint, Region: cur.Region, Bucket: cur.Bucket,
			Prefix: cur.Prefix, PublicURL: cur.PublicURL, ACL: cur.ACL, LocalDays: cur.LocalDays}
		fs := flag.NewFlagSet("offload", flag.ContinueOnError)
		fs.StringVar(&in.Endpoint, "endpoint", in.Endpoint, "S3 endpoint URL, e.g. https://s3.eu-central-1.amazonaws.com")
		fs.StringVar(&in.Region, "region", in.Region, "region (if the service needs one)")
		fs.StringVar(&in.Bucket, "bucket", in.Bucket, "bucket")
		fs.StringVar(&in.Prefix, "prefix", in.Prefix, "key prefix (default <site-id>/uploads/)")
		fs.StringVar(&in.AccessKeyID, "access-key-id", "", "access key ID (default: the stored one)")
		fs.StringVar(&in.PublicURL, "public-url", in.PublicURL, "where the prefix is publicly readable (the bucket's URL or a CDN)")
		fs.StringVar(&in.ACL, "acl", in.ACL, "public-read for services that need an ACL per object, or none")
		fs.IntVar(&in.LocalDays, "local-days", in.LocalDays, "remove local copies of uploads older than this many days (0: keep)")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() > 0 {
			return errors.New(offloadUsage)
		}
		secret, err := readSecret("Secret access key for " + in.Endpoint + " (empty keeps the stored one):")
		if err != nil && !cur.SecretSet {
			return err
		}
		in.SecretKey = secret
		fmt.Println("Checking: writing a test object, fetching it through the public URL, deleting it…")
		if err := call(cfg, "PUT", "/sites/"+id+"/offload", in, &st); err != nil {
			return err
		}
	case "off":
		fs := flag.NewFlagSet("offload", flag.ContinueOnError)
		force := fs.Bool("force", false, "turn off although some uploads only exist in the bucket")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() > 0 {
			return errors.New(offloadUsage)
		}
		if err := call(cfg, "PUT", "/sites/"+id+"/offload", site.OffloadInput{Force: *force}, &st); err != nil {
			return err
		}
	case "sync", "download":
		v, _, err := startJob(cfg, "POST", "/sites/"+id+"/offload/"+sub, nil)
		if v != nil && v.Job.Result != "" {
			fmt.Println(v.Job.Result)
		}
		return err
	default:
		return errors.New(offloadUsage)
	}
	return printJSON(st)
}
