// Package runtime runs site workloads. Runtime is the seam for multi-node
// scaling: today it is local Docker; a future implementation forwards the
// same calls to a wpgenie agent on another server.
package runtime

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

type SiteSpec struct {
	ID    string
	Image string
	// Dir is the site directory (holds wp-config.php, outside the web root)
	// and Docroot the WordPress install inside it. Both are absolute host
	// paths mounted at the same path in the container.
	Dir      string
	Docroot  string
	HostPort int // loopback port Caddy uses to reach PHP-FPM
	Network  string
	MemoryMB int
	CPUs     float64
}

type Runtime interface {
	StartSite(ctx context.Context, spec SiteSpec) error
	RemoveSite(ctx context.Context, id string) error
	// WP runs a WP-CLI command inside the site's container. stdin may be
	// nil; use it (with --prompt) for secrets so they never appear in argv.
	WP(ctx context.Context, id string, stdin io.Reader, args ...string) ([]byte, error)
}

func ContainerName(id string) string { return "wpg-" + id }

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
func runArgs(s SiteSpec) []string {
	if s.MemoryMB == 0 {
		s.MemoryMB = 512
	}
	if s.CPUs == 0 {
		s.CPUs = 1
	}
	return []string{
		"run", "-d",
		"--name", ContainerName(s.ID),
		"--hostname", s.ID,
		"--label", "wpgenie.site=" + s.ID,
		"--restart", "unless-stopped",
		"--network", s.Network,
		// Only loopback: PHP-FPM is never reachable from the internet.
		"-p", "127.0.0.1:" + strconv.Itoa(s.HostPort) + ":9000",
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
		// Resource ceilings so one site can't starve the host.
		"--memory", strconv.Itoa(s.MemoryMB) + "m",
		"--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64),
		"--pids-limit", "256",
		"-v", s.Dir + ":" + s.Dir,
		"-w", s.Docroot,
		"-e", "WPG_ROOT=" + s.Dir, // used by open_basedir in the FPM pool
		s.Image,
		"php-fpm",
	}
}

func (d *Docker) StartSite(ctx context.Context, spec SiteSpec) error {
	_, err := d.run(ctx, nil, runArgs(spec)...)
	return err
}

func (d *Docker) RemoveSite(ctx context.Context, id string) error {
	out, err := d.run(ctx, nil, "rm", "-f", "-v", ContainerName(id))
	if err != nil && strings.Contains(string(out), "No such container") {
		return nil
	}
	return err
}

func (d *Docker) WP(ctx context.Context, id string, stdin io.Reader, args ...string) ([]byte, error) {
	// Plugins and themes are not loaded: a compromised plugin must not be
	// able to hijack maintenance commands run by the panel.
	full := []string{"exec"}
	if stdin != nil {
		full = append(full, "-i")
	}
	full = append(full, ContainerName(id), "wp", "--skip-themes", "--skip-plugins")
	out, err := d.run(ctx, stdin, append(full, args...)...)
	if err != nil && stdin != nil {
		// `wp --prompt` echoes the values it read (i.e. the secret) back to
		// stdout. Never let that reach error messages, logs or API responses.
		return nil, fmt.Errorf("wp %s failed: %s", args[0], errorLines(out))
	}
	return out, err
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
