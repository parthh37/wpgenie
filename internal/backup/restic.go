// Package backup drives restic: deduplicated, encrypted backups to a local
// directory, S3-compatible storage, Backblaze B2 or an SFTP server.
//
// restic runs in a throwaway container per command. The container only
// sees what one operation needs (a site's files read-only, its database
// dump, the repository when it is local), runs without network for local
// repositories, and gets its secrets (repository password, cloud keys, SSH
// key) on stdin: never in argv or the environment `docker inspect` shows.
package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Docker runs docker commands (runtime.Docker).
type Docker interface {
	Stream(ctx context.Context, stdin io.Reader, w io.Writer, args ...string) error
}

type Restic struct {
	Docker Docker
	Image  string // e.g. restic/restic:0.18.1
	// CacheDir is restic's local cache on the host (root only): it keeps
	// remote repositories fast without downloading their index every time.
	CacheDir string
}

// Host is the restic hostname of every snapshot, so a site's snapshots
// stay one series whatever the server is called, and after moving servers.
const Host = "wpgenie"

// Repository kinds.
const (
	KindLocal = "local"
	KindS3    = "s3"
	KindB2    = "b2"
	KindSFTP  = "sftp"
)

// Where the parts of a backup are inside every snapshot.
const (
	Root      = "/backup"
	FilesPath = "/backup/files" // the WordPress install (the site's docroot)
	DBPath    = "/backup/db"    // database.sql and meta.json
	DumpFile  = "database.sql"
	MetaFile  = "meta.json"
)

// Exit codes restic documents.
const (
	exitIncomplete    = 3
	exitNoRepository  = 10
	exitLocked        = 11
	exitWrongPassword = 12
)

// wrapper reads KEY=VALUE lines from stdin up to an empty line into the
// environment, turns the SSH key (base64) into files on a tmpfs, and runs
// restic. Values never contain newlines (checked in targetFor).
const wrapper = `set -e
while IFS= read -r l && [ -n "$l" ]; do export "$l"; done
if [ -n "${WPG_SSH_KEY:-}" ]; then
  umask 077
  printf %s "$WPG_SSH_KEY" | base64 -d > /run/wpg/id
  printf %s "$WPG_KNOWN_HOSTS" | base64 -d > /run/wpg/known_hosts
fi
unset WPG_SSH_KEY WPG_KNOWN_HOSTS
exec restic "$@"`

// Mount is a host directory made visible to restic.
type Mount struct {
	Host, Container string
	ReadOnly        bool
}

type target struct {
	repo    string
	network string
	mounts  []Mount
	env     []string
	opts    []string
}

var (
	sftpUserRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	hostRe     = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

// targetFor translates a repository into restic's arguments.
func targetFor(r *store.BackupRepo) (*target, error) {
	if r.Password == "" {
		return nil, errors.New("repository has no password")
	}
	t := &target{repo: r.Location, network: "bridge",
		env: []string{"RESTIC_PASSWORD=" + r.Password, "RESTIC_PROGRESS_FPS=0.5"}}
	sec := r.Secrets
	switch r.Kind {
	case KindLocal:
		t.repo, t.network = "/repo", "none"
		t.mounts = []Mount{{Host: r.Location, Container: "/repo"}}
	case KindS3:
		t.env = append(t.env, "AWS_ACCESS_KEY_ID="+sec.AccessKeyID, "AWS_SECRET_ACCESS_KEY="+sec.SecretAccessKey)
		if sec.Region != "" {
			t.env = append(t.env, "AWS_DEFAULT_REGION="+sec.Region)
		}
	case KindB2:
		t.env = append(t.env, "B2_ACCOUNT_ID="+sec.AccessKeyID, "B2_ACCOUNT_KEY="+sec.SecretAccessKey)
	case KindSFTP:
		user, host, port, _, err := ParseSFTP(r.Location)
		if err != nil {
			return nil, err
		}
		if sec.SSHPrivateKey == "" || sec.KnownHosts == "" {
			return nil, errors.New("SFTP repository has no SSH key or pinned host key")
		}
		t.env = append(t.env,
			"WPG_SSH_KEY="+base64.StdEncoding.EncodeToString([]byte(sec.SSHPrivateKey)),
			"WPG_KNOWN_HOSTS="+base64.StdEncoding.EncodeToString([]byte(sec.KnownHosts)))
		// The host key is pinned when the repository is added: a changed key
		// (someone in the middle) fails the backup instead of sending it.
		t.opts = []string{"-o", fmt.Sprintf("sftp.command=ssh -i /run/wpg/id -o UserKnownHostsFile=/run/wpg/known_hosts "+
			"-o StrictHostKeyChecking=yes -o BatchMode=yes -o ServerAliveInterval=60 -p %d %s@%s -s sftp", port, user, host)}
	default:
		return nil, fmt.Errorf("unknown repository kind %q", r.Kind)
	}
	for _, e := range t.env {
		if strings.ContainsAny(e, "\n\r\x00") {
			return nil, errors.New("repository credentials contain a line break")
		}
	}
	return t, nil
}

// ParseSFTP splits an SFTP repository location, sftp://user@host:port//path.
func ParseSFTP(loc string) (user, host string, port int, path string, err error) {
	u, err := url.Parse(loc)
	if err != nil || u.Scheme != "sftp" || u.User == nil {
		return "", "", 0, "", errors.New("SFTP location must look like sftp://user@host:port//path")
	}
	user, host, path = u.User.Username(), u.Hostname(), u.Path
	port = 22
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			return "", "", 0, "", errors.New("invalid SFTP port")
		}
	}
	if !sftpUserRe.MatchString(user) || (!hostRe.MatchString(host) && net.ParseIP(host) == nil) || path == "" {
		return "", "", 0, "", errors.New("invalid SFTP user, host or path")
	}
	return user, host, port, path, nil
}

// SFTPLocation builds the location ParseSFTP reads. path is absolute on
// the server, or relative to the user's home.
func SFTPLocation(user, host string, port int, path string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "sftp://" + user + "@" + host + ":" + strconv.Itoa(port) + "/" + path
}

// dockerArgs is the `docker run` for one restic command.
func (r *Restic) dockerArgs(name string, t *target, mounts []Mount, args []string) []string {
	out := []string{"run", "--rm", "-i", "--name", name, "--label", "wpgenie.restic=1",
		// Reading a site's files is all restic needs root for.
		"--cap-drop", "ALL", "--cap-add", "DAC_READ_SEARCH", "--security-opt", "no-new-privileges",
		"--network", t.network,
		"--tmpfs", "/run/wpg:rw,noexec,nosuid,size=1m",
		"-v", r.CacheDir + ":/cache"}
	for _, m := range append(append([]Mount(nil), t.mounts...), mounts...) {
		v := m.Host + ":" + m.Container
		if m.ReadOnly {
			v += ":ro"
		}
		out = append(out, "-v", v)
	}
	out = append(out, "--entrypoint", "sh", r.Image, "-c", wrapper, "sh",
		"--repo", t.repo, "--cache-dir", "/cache")
	out = append(out, t.opts...)
	return append(out, args...)
}

// Error is a failed restic command.
type Error struct {
	Code int // restic's exit code, -1 if it didn't run
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// ErrWrongPassword means the repository exists but the password (or
// another installation's password) doesn't open it.
var ErrWrongPassword = errors.New("the repository exists but this password doesn't open it")

func (r *Restic) run(ctx context.Context, repo *store.BackupRepo, mounts []Mount, stdout io.Writer, args ...string) error {
	t, err := targetFor(repo)
	if err != nil {
		return err
	}
	for _, m := range append(t.mounts, mounts...) {
		if strings.ContainsAny(m.Host, ":,\n") || !strings.HasPrefix(m.Host, "/") {
			return fmt.Errorf("unsafe mount path %q", m.Host)
		}
	}
	b := make([]byte, 6)
	rand.Read(b)
	name := "wpgenie-restic-" + hex.EncodeToString(b)
	stdin := strings.NewReader(strings.Join(t.env, "\n") + "\n\n")
	if stdout == nil {
		stdout = io.Discard
	}
	err = r.Docker.Stream(ctx, stdin, stdout, r.dockerArgs(name, t, mounts, args)...)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		// Killing the docker client doesn't stop the container.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r.Docker.Stream(c, nil, io.Discard, "rm", "-f", name)
		return fmt.Errorf("restic %s: %w", args[0], ctx.Err())
	}
	code := -1
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	msg := cleanError(err.Error())
	switch code {
	case exitWrongPassword:
		return &Error{code, ErrWrongPassword}
	case exitLocked:
		msg = "the repository is locked by another operation (a prune or check); try again later: " + msg
	}
	return &Error{code, fmt.Errorf("restic %s: %s", args[0], msg)}
}

// cleanError trims docker's wrapping and restic's JSON noise down to the
// messages worth showing.
func cleanError(s string) string {
	var msgs []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		var m struct {
			MessageType string `json:"message_type"`
			Message     string `json:"message"`
			Error       struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &m) == nil {
			if m.Error.Message != "" {
				msgs = append(msgs, m.Error.Message)
			} else if m.Message != "" {
				msgs = append(msgs, m.Message)
			}
			continue
		}
		if line != "" {
			msgs = append(msgs, line)
		}
	}
	out := strings.Join(msgs, "; ")
	if len(out) > 1500 {
		out = out[:1500] + "…"
	}
	return out
}

// Init makes sure the repository exists and opens with its password,
// creating it if there is nothing at the location yet.
func (r *Restic) Init(ctx context.Context, repo *store.BackupRepo) error {
	err := r.run(ctx, repo, nil, nil, "cat", "config")
	if err == nil {
		return nil
	}
	if re := (*Error)(nil); !errors.As(err, &re) || re.Code != exitNoRepository {
		return err
	}
	return r.run(ctx, repo, nil, nil, "init")
}

// Snapshot is a restic snapshot as WPGenie reads it.
type Snapshot struct {
	ID      string    `json:"id"`
	ShortID string    `json:"short_id"`
	Time    time.Time `json:"time"`
	Tags    []string  `json:"tags"`
	Paths   []string  `json:"paths"`
	Summary *struct {
		FilesNew       int   `json:"files_new"`
		FilesTotal     int   `json:"total_files_processed"`
		BytesTotal     int64 `json:"total_bytes_processed"`
		DataAdded      int64 `json:"data_added"`
		DataAddedPacks int64 `json:"data_added_packed"`
	} `json:"summary,omitempty"`
}

// Tag returns the value of a key=value tag, or "".
func (s Snapshot) Tag(key string) string {
	for _, t := range s.Tags {
		if v, ok := strings.CutPrefix(t, key+"="); ok {
			return v
		}
	}
	return ""
}

// HasTag reports whether the snapshot carries tag.
func (s Snapshot) HasTag(tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// BackupInput is one site backup.
type BackupInput struct {
	SiteID string
	Files  string   // host directory of the WordPress install
	DB     string   // host directory holding database.sql and meta.json
	Tags   []string // besides wpgenie and site=<id>
	// Exclude are paths relative to the WordPress install.
	Exclude  []string
	Progress func(pct int)
}

// Backup snapshots a site and returns the snapshot ID.
func (r *Restic) Backup(ctx context.Context, repo *store.BackupRepo, in BackupInput) (string, error) {
	args := []string{"backup", "--json", "--host", Host, "--tag", "wpgenie", "--tag", "site=" + in.SiteID,
		"--retry-lock", "30m"}
	for _, t := range in.Tags {
		args = append(args, "--tag", t)
	}
	for _, e := range in.Exclude {
		args = append(args, "--exclude", FilesPath+"/"+strings.TrimPrefix(e, "/"))
	}
	args = append(args, Root)
	var snap string
	lines := &lineWriter{fn: func(line []byte) {
		var m struct {
			Type        string  `json:"message_type"`
			PercentDone float64 `json:"percent_done"`
			SnapshotID  string  `json:"snapshot_id"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		switch m.Type {
		case "status":
			if in.Progress != nil {
				in.Progress(int(m.PercentDone * 100))
			}
		case "summary":
			snap = m.SnapshotID
		}
	}}
	err := r.run(ctx, repo, []Mount{{in.Files, FilesPath, true}, {in.DB, DBPath, true}}, lines, args...)
	lines.Flush()
	if re := (*Error)(nil); errors.As(err, &re) && re.Code == exitIncomplete {
		// The snapshot exists but lacks files restic couldn't read: don't
		// pass it off as a backup.
		if snap != "" {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer cancel()
			r.Forget(c, repo, []string{snap})
		}
		return "", fmt.Errorf("some files could not be read, so the backup was discarded: %w", err)
	}
	if err != nil {
		return "", err
	}
	if snap == "" {
		return "", errors.New("restic reported no snapshot")
	}
	return snap, nil
}

// Snapshots lists snapshots carrying all of tags, oldest first.
func (r *Restic) Snapshots(ctx context.Context, repo *store.BackupRepo, tags ...string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json", "--no-lock"}
	if len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	var out bytes.Buffer
	if err := r.run(ctx, repo, nil, &out, args...); err != nil {
		return nil, err
	}
	snaps := []Snapshot{}
	if err := json.Unmarshal(out.Bytes(), &snaps); err != nil {
		return nil, fmt.Errorf("restic snapshots: unexpected output: %w", err)
	}
	return snaps, nil
}

var snapIDRe = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// ValidID reports whether id looks like a restic snapshot ID.
func ValidID(id string) bool { return snapIDRe.MatchString(id) }

// Dump writes a file from a snapshot as it is, or a directory as a tar
// archive (entries named by their full path without the leading slash,
// e.g. backup/files/index.php), to w.
func (r *Restic) Dump(ctx context.Context, repo *store.BackupRepo, id, path string, w io.Writer) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid snapshot ID %q", id)
	}
	return r.run(ctx, repo, nil, w, "dump", "--no-lock", "--archive", "tar", id, path)
}

// Keep is a retention policy (restic forget --keep-*); zero rules are off.
type Keep struct {
	Last, Daily, Weekly, Monthly int
	Within                       string // e.g. "7d"
}

func (k Keep) args() []string {
	var out []string
	for _, r := range []struct {
		flag string
		n    int
	}{{"--keep-last", k.Last}, {"--keep-daily", k.Daily}, {"--keep-weekly", k.Weekly}, {"--keep-monthly", k.Monthly}} {
		if r.n > 0 {
			out = append(out, r.flag, strconv.Itoa(r.n))
		}
	}
	if k.Within != "" {
		out = append(out, "--keep-within", k.Within)
	}
	return out
}

// Apply removes the snapshots carrying all of tags that the policy
// doesn't keep (without pruning their data: see Prune). A policy with no
// rules keeps everything.
func (r *Restic) Apply(ctx context.Context, repo *store.BackupRepo, tags []string, k Keep) error {
	keep := k.args()
	if len(keep) == 0 {
		return nil
	}
	args := append([]string{"forget", "--retry-lock", "30m", "--tag", strings.Join(tags, ","), "--group-by", "host"}, keep...)
	return r.run(ctx, repo, nil, nil, args...)
}

// Forget removes snapshots by ID (their data goes with the next prune).
func (r *Restic) Forget(ctx context.Context, repo *store.BackupRepo, ids []string) error {
	for _, id := range ids {
		if !ValidID(id) {
			return fmt.Errorf("invalid snapshot ID %q", id)
		}
	}
	return r.run(ctx, repo, nil, nil, append([]string{"forget", "--retry-lock", "30m"}, ids...)...)
}

// Prune deletes data no snapshot references any more. It locks the
// repository exclusively; backups wait for it (--retry-lock).
func (r *Restic) Prune(ctx context.Context, repo *store.BackupRepo) error {
	// Locks left by a crashed run would block every backup.
	if err := r.run(ctx, repo, nil, nil, "unlock"); err != nil {
		return err
	}
	return r.run(ctx, repo, nil, nil, "prune", "--retry-lock", "30m", "--max-unused", "10%")
}

// Check verifies the repository's structure (not every byte of data).
func (r *Restic) Check(ctx context.Context, repo *store.BackupRepo) error {
	return r.run(ctx, repo, nil, nil, "check", "--retry-lock", "30m")
}

// KeyScan fetches an SFTP server's host key, to pin it.
func (r *Restic) KeyScan(ctx context.Context, host string, port int) (string, error) {
	if !hostRe.MatchString(host) && net.ParseIP(host) == nil {
		return "", errors.New("invalid host")
	}
	var out bytes.Buffer
	err := r.Docker.Stream(ctx, nil, &out, "run", "--rm", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--entrypoint", "ssh-keyscan", r.Image, "-T", "10", "-p", strconv.Itoa(port), "-t", "ed25519,ecdsa,rsa", host)
	if err != nil {
		return "", fmt.Errorf("scanning the SFTP server's host key: %s", cleanError(err.Error()))
	}
	return pickHostKey(out.String())
}

// pickHostKey chooses the strongest key ssh-keyscan found.
func pickHostKey(scan string) (string, error) {
	best, rank := "", 0
	for _, line := range strings.Split(scan, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || strings.HasPrefix(line, "#") {
			continue
		}
		r := map[string]int{"ssh-ed25519": 3, "ecdsa-sha2-nistp256": 2, "ecdsa-sha2-nistp384": 2,
			"ecdsa-sha2-nistp521": 2, "ssh-rsa": 1}[f[1]]
		if r > rank {
			best, rank = strings.Join(f, " "), r
		}
	}
	if best == "" {
		return "", errors.New("the SFTP server offered no host key (is SSH running on that port?)")
	}
	return best, nil
}

// lineWriter calls fn for every complete line written to it.
type lineWriter struct {
	buf bytes.Buffer
	fn  func([]byte)
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		i := bytes.IndexByte(l.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := l.buf.Next(i + 1)
		l.fn(bytes.TrimSpace(line))
	}
	if l.buf.Len() > 1<<20 { // a runaway line: drop it
		l.buf.Reset()
	}
	return len(p), nil
}

func (l *lineWriter) Flush() {
	if l.buf.Len() > 0 {
		sc := bufio.NewScanner(&l.buf)
		for sc.Scan() {
			l.fn(sc.Bytes())
		}
	}
}
