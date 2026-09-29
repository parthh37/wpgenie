// Package runtime runs site workloads. Runtime is the seam for multi-node
// scaling: today it is local Docker; a future implementation forwards the
// same calls to a wpgenie agent on another server.
package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SiteSpec describes one PHP-FPM replica of a site. Every replica of a site
// shares the same spec; they differ only by the loopback port they publish.
type SiteSpec struct {
	ID    string
	Image string
	// ImageID pins the exact image build, so rebuilding the image (a PHP or
	// WordPress upgrade) changes Hash and rolls the replicas.
	ImageID string
	// Dir is the site directory (holds wp-config.php, outside the web root)
	// and Docroot the WordPress install inside it. Both are absolute host
	// paths mounted at the same path in the container.
	Dir         string
	Docroot     string
	Domain      string // primary domain, exposed to cron runs as HTTP_HOST
	Network     string
	MemoryMB    int
	CPUs        float64
	MaxChildren int // PHP-FPM pm.max_children
	// PHPEnv are per-site PHP settings (WPG_* variables the image's FPM
	// pool reads, KEY=VALUE); empty means the image defaults.
	PHPEnv []string
}

// Hash identifies everything that requires recreating a container when it
// changes. Replicas labelled with the current hash are kept as they are.
func (s SiteSpec) Hash() string {
	b := fmt.Appendf(nil, "%s|%s|%s|%s|%s|%d|%g|%d",
		s.ImageID, s.Image, s.Dir, s.Docroot, s.Network, s.MemoryMB, s.CPUs, s.MaxChildren)
	// Only when set: sites without PHP settings keep the hash they had
	// before the settings existed, so upgrading doesn't roll them.
	if len(s.PHPEnv) > 0 {
		b = fmt.Appendf(b, "|%s", strings.Join(s.PHPEnv, ","))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// Replica is a running (or crashed) PHP-FPM container of a site.
type Replica struct {
	Name     string
	Port     int    // 0 for containers created before replicas existed
	SpecHash string // "" for containers created before replicas existed
	Running  bool
}

// ErrNoJail means a container predates the cron jail (old image), so cron
// must not run PHP in it; WordPress's own visitor-triggered cron still does.
var ErrNoJail = errors.New("container image has no cron jail; recreate the site's replicas")

type Runtime interface {
	// ImageID resolves an image reference to its content ID.
	ImageID(ctx context.Context, image string) (string, error)
	// StartReplica starts one PHP-FPM container publishing 127.0.0.1:port.
	StartReplica(ctx context.Context, spec SiteSpec, port int) error
	// Ready reports whether the replica's PHP-FPM is accepting connections.
	Ready(ctx context.Context, name string) (bool, error)
	// ActiveConnections counts requests the replica's PHP-FPM is serving.
	ActiveConnections(ctx context.Context, name string) (int, error)
	// StopReplica stops and removes the container. It does not wait for
	// requests: callers drain first (ActiveConnections reaching 0).
	StopReplica(ctx context.Context, name string) error
	// Replicas lists a site's containers, or every site's when siteID is "".
	Replicas(ctx context.Context, siteID string) ([]Replica, error)
	// RemoveSite force-removes every container of a site.
	RemoveSite(ctx context.Context, id string) error
	// WP runs a WP-CLI command inside a running replica. stdin may be nil;
	// use it (with --prompt) for secrets so they never appear in argv.
	WP(ctx context.Context, id string, stdin io.Reader, args ...string) ([]byte, error)
	// Exec runs a command as the site user in a running replica, streaming
	// stdin (may be nil) in and stdout out. Stderr ends up in the error. Use
	// WPArgs to run WP-CLI this way (e.g. for JSON output that warnings on
	// stderr must not corrupt).
	Exec(ctx context.Context, id string, stdin io.Reader, stdout io.Writer, args ...string) error
	// RunCron runs WordPress's due cron events in a running replica, under
	// the same PHP jail as web requests.
	RunCron(ctx context.Context, spec SiteSpec) ([]byte, error)
	// CPUUsage samples every running site replica's CPU use, keyed by
	// container name, in percent of one core (200 = two cores busy).
	CPUUsage(ctx context.Context) (map[string]float64, error)
}

func ContainerName(id string, port int) string { return "wpg-" + id + "-" + strconv.Itoa(port) }

// Docker drives the docker CLI. The CLI (rather than the Go SDK) keeps the
// binary small and makes every action reproducible by hand when debugging.
type Docker struct {
	Bin string // defaults to "docker"
}

func (d *Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

// Run runs any docker command and returns its combined output. For
// components managed outside the site Runtime (the mail stack).
func (d *Docker) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return d.run(ctx, stdin, args...)
}

// Stream runs any docker command with stdout going to w; stderr (bounded)
// goes into the error. For components managed outside the site Runtime.
func (d *Docker) Stream(ctx context.Context, stdin io.Reader, w io.Writer, args ...string) error {
	return d.stream(ctx, stdin, w, args...)
}

// stream runs docker with stdout going to w; stderr (bounded) goes into
// the error. For large outputs (snapshots) that must not sit in memory.
func (d *Docker) stream(ctx context.Context, stdin io.Reader, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stdin = stdin
	cmd.Stdout = w
	stderr := &capped{max: 8 << 10}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// capped keeps the first max bytes written to it.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (d *Docker) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// runArgs builds the `docker run` invocation. Each flag is a layer of
// defence if WordPress or a plugin gets compromised.
func runArgs(s SiteSpec, port int) []string {
	if s.MemoryMB == 0 {
		s.MemoryMB = 512
	}
	if s.CPUs == 0 {
		s.CPUs = 1
	}
	if s.MaxChildren == 0 {
		s.MaxChildren = FPMMaxChildren(s.MemoryMB)
	}
	args := []string{
		"run", "-d",
		"--name", ContainerName(s.ID, port),
		"--hostname", s.ID,
		"--label", "wpgenie.site=" + s.ID,
		"--label", "wpgenie.port=" + strconv.Itoa(port),
		"--label", "wpgenie.spec=" + s.Hash(),
		"--restart", "unless-stopped",
		"--network", s.Network,
		// Only loopback: PHP-FPM is never reachable from the internet.
		"-p", "127.0.0.1:" + strconv.Itoa(port) + ":9000",
		// Unprivileged www-data (uid 82 on Alpine) with no capabilities and
		// no way to regain any via setuid binaries.
		"--user", "82:82",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		// Immutable container filesystem: malware can only land in the site's
		// own docroot (which the scanner watches) or in tmpfs.
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=256m",
		// The upstream image declares VOLUME /var/www/html, which would give
		// every container an anonymous *writable* volume outside the jail
		// (and leak volumes on delete). Shadow it with a read-only tmpfs.
		"--tmpfs", "/var/www/html:ro,noexec,nosuid,size=64k",
		// Resource ceilings so one site can't starve the host. Every replica
		// gets the full allowance: scaling out multiplies capacity.
		"--memory", strconv.Itoa(s.MemoryMB) + "m",
		"--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64),
		"--pids-limit", "256",
		// docker stop sends the image's SIGQUIT (graceful); allow the FPM
		// process_control_timeout to elapse before SIGKILL.
		"--stop-timeout", "35",
		"-v", s.Dir + ":" + s.Dir,
		"-w", s.Docroot,
		"-e", "WPG_ROOT=" + s.Dir, // used by open_basedir in the FPM pool
		"-e", "WPG_MAX_CHILDREN=" + strconv.Itoa(s.MaxChildren),
	}
	for _, e := range s.PHPEnv {
		args = append(args, "-e", e)
	}
	return append(args, s.Image, "php-fpm")
}

// ImageExists reports whether an image is present locally.
func (d *Docker) ImageExists(ctx context.Context, image string) bool {
	_, err := d.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
	return err == nil
}

// Build builds an image from a directory; buildArgs are KEY=VALUE.
func (d *Docker) Build(ctx context.Context, tag, dir string, buildArgs ...string) error {
	args := []string{"build", "-q", "-t", tag}
	for _, a := range buildArgs {
		args = append(args, "--build-arg", a)
	}
	_, err := d.run(ctx, nil, append(args, dir)...)
	return err
}

func (d *Docker) ImageID(ctx context.Context, image string) (string, error) {
	out, err := d.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
	return strings.TrimSpace(string(out)), err
}

func (d *Docker) StartReplica(ctx context.Context, spec SiteSpec, port int) error {
	_, err := d.run(ctx, nil, runArgs(spec, port)...)
	return err
}

// Ready checks the container's own socket table for a listener on :9000.
// Probing the published port from the host would not work: docker-proxy
// accepts connections before PHP-FPM is listening.
func (d *Docker) Ready(ctx context.Context, name string) (bool, error) {
	listening, _, err := d.fpmSockets(ctx, name)
	return listening, err
}

// ActiveConnections uses the same socket table: once the proxy stops
// sending a replica traffic, its connection count falls to 0 as the last
// requests finish, which is the moment it is safe to stop.
func (d *Docker) ActiveConnections(ctx context.Context, name string) (int, error) {
	_, active, err := d.fpmSockets(ctx, name)
	return active, err
}

func (d *Docker) fpmSockets(ctx context.Context, name string) (bool, int, error) {
	// tcp6 is absent on hosts booted with ipv6.disable=1; still succeed
	// (docker exec itself fails if the container isn't running).
	out, err := d.run(ctx, nil, "exec", name, "sh", "-c", "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null; true")
	if err != nil {
		return false, 0, err
	}
	listening, active := socketStates(out, 9000)
	return listening, active, nil
}

// socketStates parses /proc/net/tcp{,6} for sockets on local port: whether
// one is listening (0A), and how many connections a request may still be in
// flight on (01 ESTABLISHED; 08 CLOSE_WAIT = the client hung up, PHP hasn't).
func socketStates(procNet []byte, port int) (listening bool, active int) {
	suffix := fmt.Sprintf(":%04X", port)
	sc := bufio.NewScanner(bytes.NewReader(procNet))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) <= 3 || !strings.HasSuffix(f[1], suffix) {
			continue
		}
		switch f[3] {
		case "0A":
			listening = true
		case "01", "08":
			active++
		}
	}
	return listening, active
}

func (d *Docker) StopReplica(ctx context.Context, name string) error {
	if _, err := d.run(ctx, nil, "stop", name); err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}
	out, err := d.run(ctx, nil, "rm", "-f", "-v", name)
	if err != nil && strings.Contains(string(out), "No such container") {
		return nil
	}
	return err
}

func (d *Docker) Replicas(ctx context.Context, siteID string) ([]Replica, error) {
	filter := "label=wpgenie.site"
	if siteID != "" {
		filter += "=" + siteID
	}
	out, err := d.run(ctx, nil, "ps", "-a", "--filter", filter,
		"--format", `{{.Names}}|{{.Label "wpgenie.port"}}|{{.Label "wpgenie.spec"}}|{{.State}}`)
	if err != nil {
		return nil, err
	}
	return parseReplicas(out), nil
}

func parseReplicas(out []byte) []Replica {
	var rs []Replica
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 4 || f[0] == "" {
			continue
		}
		port, _ := strconv.Atoi(f[1])
		rs = append(rs, Replica{Name: f[0], Port: port, SpecHash: f[2], Running: f[3] == "running"})
	}
	return rs
}

// running returns the name of one running replica of the site.
func (d *Docker) running(ctx context.Context, id string) (string, error) {
	rs, err := d.Replicas(ctx, id)
	if err != nil {
		return "", err
	}
	for _, r := range rs {
		if r.Running {
			return r.Name, nil
		}
	}
	return "", fmt.Errorf("site %s has no running PHP container", id)
}

func (d *Docker) RemoveSite(ctx context.Context, id string) error {
	rs, err := d.Replicas(ctx, id)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		out, err := d.run(ctx, nil, "rm", "-f", "-v", r.Name)
		if err != nil && !strings.Contains(string(out), "No such container") {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *Docker) WP(ctx context.Context, id string, stdin io.Reader, args ...string) ([]byte, error) {
	name, err := d.running(ctx, id)
	if err != nil {
		return nil, err
	}
	full := []string{"exec"}
	if stdin != nil {
		full = append(full, "-i")
	}
	full = append(full, name)
	out, err := d.run(ctx, stdin, append(full, WPArgs(args...)...)...)
	if err != nil && stdin != nil {
		// `wp --prompt` echoes the values it read (i.e. the secret) back to
		// stdout. Never let that reach error messages, logs or API responses.
		return nil, fmt.Errorf("wp %s failed: %s", args[0], errorLines(out))
	}
	return out, err
}

// WPArgs is the command line for WP-CLI as the panel runs it. Plugins and
// themes are not loaded: a compromised plugin must not be able to hijack
// maintenance commands run by the panel.
func WPArgs(args ...string) []string {
	return append([]string{"wp", "--skip-themes", "--skip-plugins"}, args...)
}

func (d *Docker) Exec(ctx context.Context, id string, stdin io.Reader, stdout io.Writer, args ...string) error {
	name, err := d.running(ctx, id)
	if err != nil {
		return err
	}
	full := []string{"exec"}
	if stdin != nil {
		full = append(full, "-i")
	}
	full = append(full, name)
	if stdout == nil {
		stdout = io.Discard
	}
	return d.stream(ctx, stdin, stdout, append(full, args...)...)
}

const (
	jailIni     = "/usr/local/etc/php/jail.d/zz-jail.ini"
	noJailExit  = 99
	cronTimeout = "300"
)

// cronArgs runs wp-cron.php with plain PHP rather than WP-CLI: cron executes
// plugin code, so it must get the web jail (open_basedir, no exec()), which
// PHP_INI_SCAN_DIR adds on top of the normal ini files. `timeout` runs inside
// the container because killing the docker client would not stop PHP.
// The CLI copies env vars into $_SERVER, so plugins see a normal host.
func cronArgs(name string, s SiteSpec) []string {
	return []string{
		"exec",
		"-e", "PHP_INI_SCAN_DIR=:" + jailIni[:strings.LastIndex(jailIni, "/")],
		"-e", "HTTP_HOST=" + s.Domain, "-e", "SERVER_NAME=" + s.Domain, "-e", "HTTPS=on",
		name, "sh", "-c", `[ -f "$1" ] || exit ` + strconv.Itoa(noJailExit) + `; exec timeout "$2" php "$3"`,
		"sh", jailIni, cronTimeout, s.Docroot + "/wp-cron.php",
	}
}

func (d *Docker) RunCron(ctx context.Context, s SiteSpec) ([]byte, error) {
	name, err := d.running(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	out, err := d.run(ctx, nil, cronArgs(name, s)...)
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.ExitCode() == noJailExit {
		return nil, ErrNoJail
	}
	return out, err
}

// CPUUsage asks the daemon for one stats sample of every running container
// (it measures over about a second) and keeps the site replicas. Listing
// all containers rather than naming them means a replica stopping between
// the listing and the sample can't fail the whole call.
func (d *Docker) CPUUsage(ctx context.Context) (map[string]float64, error) {
	out, err := d.run(ctx, nil, "stats", "--no-stream", "--format", "{{.Name}}|{{.CPUPerc}}")
	if err != nil {
		return nil, err
	}
	return parseCPUStats(out), nil
}

func parseCPUStats(out []byte) map[string]float64 {
	usage := map[string]float64{}
	for _, line := range strings.Split(string(out), "\n") {
		name, perc, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || !strings.HasPrefix(name, "wpg-") {
			continue
		}
		// "--" while a container is starting: no sample yet, not zero.
		v, err := strconv.ParseFloat(strings.TrimSuffix(perc, "%"), 64)
		if err != nil {
			continue
		}
		usage[name] = v
	}
	return usage
}

// errorLines keeps only WP-CLI's "Error:" lines from combined output.
func errorLines(out []byte) string {
	var keep []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "Error:") {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		return "unknown error (output withheld: may contain secrets)"
	}
	return strings.Join(keep, "; ")
}

// EnsureBuilt builds an image from the sources in dir unless the image
// already carries their hash (label wpgenie.src), and returns its ID. For
// images the daemon builds on first use (SFTP, Adminer): an upgrade that
// changes their sources rebuilds them the next time they are needed.
func (d *Docker) EnsureBuilt(ctx context.Context, tag, dir string) (string, error) {
	hash, err := dirHash(dir)
	if err != nil {
		return "", fmt.Errorf("image sources for %s: %w", tag, err)
	}
	out, err := d.run(ctx, nil, "image", "inspect", "--format", `{{.Id}}|{{index .Config.Labels "wpgenie.src"}}`, tag)
	if id, label, ok := strings.Cut(strings.TrimSpace(string(out)), "|"); err == nil && ok && label == hash {
		return id, nil
	}
	if _, err := d.run(ctx, nil, "build", "-q", "--label", "wpgenie.src="+hash, "-t", tag, dir); err != nil {
		return "", err
	}
	return d.ImageID(ctx, tag)
}

// dirHash hashes the regular files of a directory tree (names and content).
func dirHash(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)[:8]), err
}
