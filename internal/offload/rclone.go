// Package offload drives rclone: sites' uploads copied to S3-compatible
// object storage, so they can be served from there (and, later, by replicas
// on several servers).
//
// rclone runs in a throwaway container per command, like restic (see
// internal/backup). It never sees a site's files: the daemon copies the
// files to upload into a directory of its own first (see site/offload.go),
// because a site controls every path under its docroot and could turn
// wp-content/uploads into a symlink. Mounted into Docker, that symlink would
// be resolved on the host (any directory published to a public bucket);
// inside the container, it would lead rclone to its own /proc/self/environ,
// where the keys are. The keys go in on stdin (a KEY=VALUE block the
// entrypoint exports), never in argv or the environment `docker inspect`
// shows.
package offload

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Docker runs docker commands (runtime.Docker).
type Docker interface {
	Stream(ctx context.Context, stdin io.Reader, w io.Writer, args ...string) error
}

// Rclone runs rclone commands against a Target.
type Rclone struct {
	Docker Docker
	Image  string // e.g. rclone/rclone:1.75.1
	// Network is the Docker network rclone's containers join ("" is
	// "bridge": the internet, not the sites' network with the databases).
	Network string
}

// Target is where a site's uploads go: a bucket and key prefix on an
// S3-compatible service. SecretKey is a secret.
type Target struct {
	Endpoint    string // https://s3.example.com
	Region      string
	Bucket      string
	Prefix      string // "sabc1234/uploads/" ("" for the bucket's root)
	AccessKeyID string
	SecretKey   string
	// ACL is a canned ACL set on every object ("public-read"), or "" for
	// none: the bucket's policy makes objects readable (and buckets that
	// enforce object ownership refuse ACLs).
	ACL string
}

// ACLs rclone may set on uploaded objects ("" sets none).
var ACLs = []string{"", "public-read"}

// CacheControl is sent with every object: uploads are renamed rather than
// overwritten, like the static files Caddy serves (30 days).
const CacheControl = "public, max-age=2592000"

// wrapper reads KEY=VALUE lines from stdin up to an empty line into the
// environment and runs rclone; whatever follows on stdin is rclone's
// (rcat's content, a --files-from-raw list). With $1 = "log", rclone's log
// (JSON lines) goes to stdout, where the daemon reads the transfer
// statistics; otherwise stdout is rclone's output (lsf) and errors stay on
// stderr. Values never contain line breaks (checked in env).
const wrapper = `set -e
while IFS= read -r l && [ -n "$l" ]; do export "$l"; done
if [ "$1" = log ]; then shift; exec rclone "$@" 2>&1; fi
shift
exec rclone "$@"`

// Stats is what a command moved, from rclone's final statistics.
type Stats struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
	Deletes int64 `json:"deletes"`
	Errors  int64 `json:"errors"`
}

// Object is a file in the bucket. MD5 is empty when the service didn't
// give one in the listing (multipart uploads: their ETag isn't an MD5).
type Object struct {
	Size     int64
	MD5      string
	Modified time.Time // when it was uploaded (the server's clock)
}

var (
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	regionRe = regexp.MustCompile(`^[a-z0-9-]{0,40}$`)
	// S3's bucket naming rules (MinIO, R2 and B2 use the same).
	bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	// A key prefix: path segments of safe characters, ending in "/".
	prefixRe = regexp.MustCompile(`^([A-Za-z0-9_-][A-Za-z0-9._-]*/)*$`)
	// Access keys are identifiers (AWS: AKIA…, R2: 32 hex, B2: 25 chars).
	keyIDRe = regexp.MustCompile(`^[A-Za-z0-9+/=._-]{3,128}$`)
)

// Validate checks a target's fields; the messages name the field.
func (t Target) Validate() error {
	u, err := url.Parse(t.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || !hostRe.MatchString(u.Host) ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must be the service's URL, like https://s3.eu-central-1.amazonaws.com (no path)")
	}
	switch {
	case !regionRe.MatchString(t.Region):
		return errors.New("region may only hold lowercase letters, digits and dashes")
	case !bucketRe.MatchString(t.Bucket) || strings.Contains(t.Bucket, ".."):
		return errors.New("bucket must be a valid bucket name (3-63 lowercase letters, digits, dots and dashes)")
	case !prefixRe.MatchString(t.Prefix) || len(t.Prefix) > 512:
		return errors.New("prefix must be path segments of letters, digits, '.', '_' and '-', ending in '/' (e.g. site1/uploads/)")
	case !keyIDRe.MatchString(t.AccessKeyID):
		return errors.New("access_key_id doesn't look like an access key ID")
	case t.SecretKey == "" || len(t.SecretKey) > 256 || strings.IndexFunc(t.SecretKey, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0:
		return errors.New("secret_key must be printable characters without spaces")
	}
	for _, a := range ACLs {
		if t.ACL == a {
			return nil
		}
	}
	return fmt.Errorf("acl must be empty or public-read")
}

// provider is rclone's S3 provider for the endpoint: it decides quirks like
// path-style addressing (AWS wants virtual-hosted buckets; MinIO and most
// self-hosted services path-style).
func (t Target) provider() string {
	u, _ := url.Parse(t.Endpoint)
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.HasSuffix(host, ".amazonaws.com"):
		return "AWS"
	case strings.HasSuffix(host, ".r2.cloudflarestorage.com"):
		return "Cloudflare"
	case strings.HasSuffix(host, ".digitaloceanspaces.com"):
		return "DigitalOcean"
	case strings.HasSuffix(host, ".wasabisys.com"):
		return "Wasabi"
	}
	return "Other"
}

// remote is the rclone path of name under the target's prefix: an
// on-the-fly remote configured only by flags and the environment.
func (t Target) remote(name string) string {
	return ":s3:" + t.Bucket + "/" + strings.TrimSuffix(t.Prefix+name, "/")
}

func (t Target) env() ([]string, error) {
	env := []string{"RCLONE_S3_ACCESS_KEY_ID=" + t.AccessKeyID, "RCLONE_S3_SECRET_ACCESS_KEY=" + t.SecretKey}
	for _, e := range env {
		if strings.ContainsAny(e, "\n\r\x00") {
			return nil, errors.New("credentials contain a line break")
		}
	}
	return env, nil
}

func (t Target) flags() []string {
	f := []string{"--config", "/dev/null", "--s3-provider", t.provider(), "--s3-endpoint", t.Endpoint,
		// The key may only be allowed to write objects: never try to create
		// the bucket.
		"--s3-no-check-bucket",
		"--use-json-log", "--retries", "3", "--low-level-retries", "5",
		"--contimeout", "30s", "--timeout", "5m"}
	if t.Region != "" {
		f = append(f, "--s3-region", t.Region)
	}
	if t.ACL != "" {
		f = append(f, "--s3-acl", t.ACL)
	}
	return f
}

// Mount is a host directory made visible to rclone: always one the daemon
// created (never a path a site controls).
type Mount struct {
	Host, Container string
	ReadOnly        bool
}

func (r *Rclone) dockerArgs(name string, mounts []Mount, logs bool, args []string) []string {
	network := r.Network
	if network == "" {
		network = "bridge"
	}
	out := []string{"run", "--rm", "-i", "--name", name, "--label", "wpgenie.rclone=1",
		// rclone reads and writes only the daemon's own directories: no
		// capabilities at all, and a read-only root filesystem.
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m", "-e", "HOME=/tmp",
		"--memory", "512m", "--pids-limit", "256", "--network", network}
	for _, m := range mounts {
		v := m.Host + ":" + m.Container
		if m.ReadOnly {
			v += ":ro"
		}
		out = append(out, "-v", v)
	}
	mode := "out"
	if logs {
		mode = "log"
	}
	out = append(out, "--entrypoint", "sh", r.Image, "-c", wrapper, "sh", mode)
	return append(out, args...)
}

// Error is a failed rclone command.
type Error struct {
	Code int // rclone's exit code, -1 if it didn't run
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// run runs one rclone command: the secret block, then extra on stdin. With
// logs, stdout is rclone's JSON log (statistics and errors are parsed from
// it); otherwise stdout goes to out.
func (r *Rclone) run(ctx context.Context, t Target, mounts []Mount, extra io.Reader, out io.Writer, logs bool, args ...string) (Stats, error) {
	if err := t.Validate(); err != nil {
		return Stats{}, err
	}
	env, err := t.env()
	if err != nil {
		return Stats{}, err
	}
	for _, m := range mounts {
		if strings.ContainsAny(m.Host, ":,\n") || !strings.HasPrefix(m.Host, "/") {
			return Stats{}, fmt.Errorf("unsafe mount path %q", m.Host)
		}
	}
	b := make([]byte, 6)
	rand.Read(b)
	name := "wpgenie-rclone-" + hex.EncodeToString(b)
	var stdin io.Reader = strings.NewReader(strings.Join(env, "\n") + "\n\n")
	if extra != nil {
		stdin = io.MultiReader(stdin, extra)
	}
	lg := &logParser{}
	if logs {
		out = lg
	} else if out == nil {
		out = io.Discard
	}
	args = append(args, t.flags()...)
	err = r.Docker.Stream(ctx, stdin, out, r.dockerArgs(name, mounts, logs, args)...)
	lg.flush()
	if err == nil {
		return lg.stats, nil
	}
	if ctx.Err() != nil {
		// Killing the docker client doesn't stop the container.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r.Docker.Stream(c, nil, io.Discard, "rm", "-f", name)
		return lg.stats, fmt.Errorf("rclone %s: %w", args[0], ctx.Err())
	}
	code := -1
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		code = ec.ExitCode()
	}
	msg := lg.message()
	if msg == "" {
		msg = CleanError(err.Error())
	}
	return lg.stats, &Error{code, fmt.Errorf("rclone %s: %s", args[0], msg)}
}

// Upload copies everything in dir (a directory the daemon filled) to the
// target, keeping the paths. The daemon already chose what needs
// uploading, so the bucket isn't listed and nothing is checked there first.
func (r *Rclone) Upload(ctx context.Context, t Target, dir string) (Stats, error) {
	return r.run(ctx, t, []Mount{{dir, "/src", true}}, nil, nil, true,
		"copy", "/src", t.remote(""), "--no-check-dest", "--no-traverse",
		"--header-upload", "Cache-Control: "+CacheControl,
		"--transfers", "4", "--stats", "24h", "--stats-log-level", "NOTICE", "--stats-one-line")
}

// Download copies the listed objects (paths relative to the prefix) into
// dir, a directory the daemon created and moves them out of.
func (r *Rclone) Download(ctx context.Context, t Target, paths []string, dir string) (Stats, error) {
	if err := checkPaths(paths); err != nil {
		return Stats{}, err
	}
	return r.run(ctx, t, []Mount{{dir, "/dst", false}}, strings.NewReader(strings.Join(paths, "\n")+"\n"), nil, true,
		"copy", t.remote(""), "/dst", "--files-from-raw", "-", "--no-traverse",
		"--transfers", "4", "--stats", "24h", "--stats-log-level", "NOTICE", "--stats-one-line")
}

// Delete removes the listed objects (paths relative to the prefix); objects
// that don't exist are no error.
func (r *Rclone) Delete(ctx context.Context, t Target, paths []string) (Stats, error) {
	if err := checkPaths(paths); err != nil {
		return Stats{}, err
	}
	if len(paths) == 0 {
		return Stats{}, nil
	}
	return r.run(ctx, t, nil, strings.NewReader(strings.Join(paths, "\n")+"\n"), nil, true,
		"delete", t.remote(""), "--files-from-raw", "-", "--no-traverse",
		"--stats", "24h", "--stats-log-level", "NOTICE", "--stats-one-line")
}

// Put writes one small object (the probe) with the given content.
func (r *Rclone) Put(ctx context.Context, t Target, name string, data []byte) error {
	if err := checkPaths([]string{name}); err != nil {
		return err
	}
	_, err := r.run(ctx, t, nil, bytes.NewReader(data), nil, true, "rcat", t.remote(name))
	return err
}

// DeleteFile removes one object.
func (r *Rclone) DeleteFile(ctx context.Context, t Target, name string) error {
	if err := checkPaths([]string{name}); err != nil {
		return err
	}
	_, err := r.run(ctx, t, nil, nil, nil, true, "deletefile", t.remote(name))
	return err
}

// List returns every object under the prefix (one listing: no request per
// object), by path relative to the prefix.
func (r *Rclone) List(ctx context.Context, t Target) (map[string]Object, error) {
	pr, pw := io.Pipe()
	type result struct {
		objs map[string]Object
		err  error
	}
	done := make(chan result, 1)
	go func() {
		objs, err := parseListing(pr)
		pr.CloseWithError(err) // unblock rclone if parsing gave up
		done <- result{objs, err}
	}()
	_, err := r.run(ctx, t, nil, nil, pw, false,
		"lsf", t.remote(""), "-R", "--files-only", "--csv", "--format", "psth", "--hash", "MD5",
		"--use-server-modtime", "--time-format", time.RFC3339Nano)
	pw.CloseWithError(err)
	res := <-done
	if err != nil {
		return nil, err
	}
	return res.objs, res.err
}

// maxListing bounds a listing held in memory (about 200 bytes an object).
const maxListing = 5_000_000

func parseListing(rd io.Reader) (map[string]Object, error) {
	cr := csv.NewReader(bufio.NewReaderSize(rd, 64<<10))
	cr.FieldsPerRecord = 4
	cr.ReuseRecord = true
	out := map[string]Object{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the bucket listing: %w", err)
		}
		size, err := strconv.ParseInt(rec[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("reading the bucket listing: size %q", rec[1])
		}
		mod, _ := time.Parse(time.RFC3339Nano, rec[2])
		md5 := strings.ToLower(rec[3])
		if len(md5) != 32 {
			md5 = ""
		}
		out[strings.Clone(rec[0])] = Object{Size: size, MD5: md5, Modified: mod}
		if len(out) > maxListing {
			return nil, errors.New("the bucket prefix holds too many objects to compare")
		}
	}
}

// checkPaths refuses paths rclone would take for anything but a file
// under the prefix. Callers normalise them first (site.cleanQueuePath);
// this is the last line.
func checkPaths(paths []string) error {
	for _, p := range paths {
		if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\n\r\x00\\") ||
			p == ".." || strings.HasPrefix(p, "../") || strings.HasSuffix(p, "/..") || strings.Contains(p, "/../") {
			return fmt.Errorf("unsafe object path %q", p)
		}
	}
	return nil
}

// logParser reads rclone's JSON log: the last statistics, and error
// messages (a few, deduplicated) for the error report.
type logParser struct {
	buf   []byte
	stats Stats
	errs  []string
}

func (l *logParser) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.line(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 1<<20 { // a runaway line: drop it
		l.buf = nil
	}
	return len(p), nil
}

func (l *logParser) flush() {
	if len(l.buf) > 0 {
		l.line(l.buf)
		l.buf = nil
	}
}

func (l *logParser) line(b []byte) {
	var m struct {
		Level  string `json:"level"`
		Msg    string `json:"msg"`
		Object string `json:"object"`
		Stats  *struct {
			Bytes     int64 `json:"bytes"`
			Transfers int64 `json:"transfers"`
			Deletes   int64 `json:"deletes"`
			Errors    int64 `json:"errors"`
		} `json:"stats"`
	}
	if json.Unmarshal(bytes.TrimSpace(b), &m) != nil {
		if s := strings.TrimSpace(string(b)); s != "" && len(l.errs) < 5 {
			l.errs = append(l.errs, s) // not JSON: rclone died before logging
		}
		return
	}
	if m.Stats != nil {
		l.stats = Stats{Objects: m.Stats.Transfers, Bytes: m.Stats.Bytes, Deletes: m.Stats.Deletes, Errors: m.Stats.Errors}
		return
	}
	if m.Level != "error" && m.Level != "critical" && !strings.HasPrefix(m.Msg, "Failed to ") {
		return
	}
	msg := shortError(m.Msg)
	if m.Object != "" {
		msg = m.Object + ": " + msg
	}
	for _, e := range l.errs {
		if e == msg {
			return
		}
	}
	if len(l.errs) < 5 {
		l.errs = append(l.errs, msg)
	}
}

func (l *logParser) message() string {
	return CleanError(strings.Join(l.errs, "; "))
}

// apiErr is the useful part of an AWS SDK error: "api error Code: text".
var apiErr = regexp.MustCompile(`(?:StatusCode: (\d+)[^;]*?)?api error ([A-Za-z]+): (.*)$`)

// shortError turns the SDK's "operation error S3: PutObject, https response
// error StatusCode: 403, RequestID: …, api error AccessDenied: Access
// Denied." into "403 AccessDenied: Access Denied.".
func shortError(s string) string {
	if m := apiErr.FindStringSubmatch(s); m != nil {
		out := m[2] + ": " + m[3]
		if m[1] != "" {
			out = m[1] + " " + out
		}
		return out
	}
	return s
}

// CleanError bounds an error message for the panel.
func CleanError(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 1000 {
		s = s[:1000] + "…"
	}
	return s
}
