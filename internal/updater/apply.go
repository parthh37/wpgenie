package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Status is the progress of the last self-update, shared between the
// daemon and the applier through StateDir/status.json.
type Status struct {
	Phase   string    `json:"phase"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

const (
	PhaseStaged     = "staged"
	PhaseApplying   = "applying"
	PhaseRestarting = "restarting"
	PhaseDone       = "done"
	PhaseRolledBack = "rolled_back"
	PhaseFailed     = "failed"
)

// Active reports an update still in flight. A status stuck in a running
// phase for longer than any update takes means the applier died.
func (s *Status) Active() bool {
	switch s.Phase {
	case PhaseStaged, PhaseApplying, PhaseRestarting:
		return time.Since(s.At) < 45*time.Minute
	}
	return false
}

func WriteStatus(dir string, s *Status) error {
	s.At = time.Now().UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "status.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "status.json"))
}

func ReadStatus(dir string) (*Status, error) {
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		return nil, err
	}
	var s Status
	return &s, json.Unmarshal(b, &s)
}

// Paths are where an installation lives (see deploy/install.sh).
type Paths struct {
	Bin      string // /usr/local/bin/wpgenie
	Share    string // /opt/wpgenie: deploy/ and images/
	Unit     string // /etc/systemd/system/wpgenie.service
	EnvFile  string // /etc/wpgenie/infra.env
	StateDir string // /var/lib/wpgenie/updates
}

func DefaultPaths() Paths {
	return Paths{
		Bin: "/usr/local/bin/wpgenie", Share: "/opt/wpgenie", Unit: "/etc/systemd/system/wpgenie.service",
		EnvFile: "/etc/wpgenie/infra.env", StateDir: "/var/lib/wpgenie/updates",
	}
}

// Applier installs a staged release. It runs outside the daemon (as a
// transient systemd unit) because it restarts the daemon, and because the
// daemon's sandbox can't write /usr/local/bin or /opt.
type Applier struct {
	Staged   string // verified release: wpgenie, deploy/, images/, VERSION
	From     string // version being replaced
	Paths    Paths
	PHPImage string // tag to rebuild, e.g. wpgenie/php:8.3
	// CaddyImage is the Caddy build with the Coraza WAF (wpgenie/caddy:2),
	// rebuilt when the release has images/caddy.
	CaddyImage string
	// Run executes a command and returns its combined output.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Version asks the running daemon for its version.
	Version func(ctx context.Context) (string, error)
	// RollSites asks the new daemon to roll every site onto the new PHP
	// image (zero-downtime, like a scale). Optional.
	RollSites     func(ctx context.Context) error
	HealthTimeout time.Duration // default 2 minutes
	Log           io.Writer
}

func (a *Applier) logf(format string, args ...any) {
	if a.Log != nil {
		fmt.Fprintf(a.Log, format+"\n", args...)
	}
}

func (a *Applier) status(phase, to, msg string) {
	if err := WriteStatus(a.Paths.StateDir, &Status{Phase: phase, From: a.From, To: to, Message: msg}); err != nil {
		a.logf("writing status: %v", err)
	}
	a.logf("[%s] %s", phase, msg)
}

func (a *Applier) run(ctx context.Context, name string, args ...string) error {
	out, err := a.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(string(out), 5))
	}
	return nil
}

// Apply installs the release, restarts WPGenie and checks the new version
// answers; if anything fails after the first change, everything is put
// back as it was.
func (a *Applier) Apply(ctx context.Context) error {
	vb, err := os.ReadFile(filepath.Join(a.Staged, "VERSION"))
	if err != nil {
		return err
	}
	to := strings.TrimSpace(string(vb))

	// 1. Build the new images first: the slowest and likeliest step to
	// fail, and nothing live changes until they are built. The old image
	// IDs are kept so a rollback can re-tag them.
	oldImages := map[string]string{}
	for _, img := range a.images() {
		a.status(PhaseApplying, to, "building the "+img.name+" image")
		if out, err := a.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", img.tag); err == nil {
			oldImages[img.tag] = strings.TrimSpace(string(out))
		}
		args := append([]string{"build", "-q", "-t", img.tag}, img.args...)
		if err := a.run(ctx, "docker", append(args, filepath.Join(a.Staged, "images", img.dir))...); err != nil {
			// Put back what an earlier build in this loop replaced.
			for tag, id := range oldImages {
				if tag != img.tag && id != "" {
					a.run(ctx, "docker", "tag", id, tag)
				}
			}
			a.status(PhaseFailed, to, img.name+" image build failed, nothing was changed: "+err.Error())
			return err
		}
	}

	// 2. Swap files, keeping the previous ones next to them.
	a.status(PhaseApplying, to, "installing")
	if err := a.install(); err != nil {
		return a.rollback(ctx, to, oldImages, fmt.Errorf("installing files: %w", err))
	}
	if err := a.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return a.rollback(ctx, to, oldImages, err)
	}
	// 3. Shared services (Caddy, MariaDB, Valkey): compose only recreates
	// what the new compose file changed.
	if err := a.compose(ctx); err != nil {
		return a.rollback(ctx, to, oldImages, err)
	}
	// 4. Restart and wait for the new version to answer.
	a.status(PhaseRestarting, to, "restarting WPGenie")
	if err := a.run(ctx, "systemctl", "restart", "wpgenie"); err != nil {
		return a.rollback(ctx, to, oldImages, err)
	}
	if err := a.waitVersion(ctx, to); err != nil {
		return a.rollback(ctx, to, oldImages, fmt.Errorf("the new version did not come up: %w", err))
	}
	msg := "updated from " + a.From + " to " + to
	if a.RollSites != nil {
		if err := a.RollSites(ctx); err != nil {
			msg += "; rolling sites onto the new PHP image failed (" + err.Error() + "): run `wpgenie site scale <id>` per site"
		} else {
			msg += "; sites are being rolled onto the new PHP image"
		}
	}
	a.status(PhaseDone, to, msg)
	os.RemoveAll(a.Paths.Share + ".prev")
	return nil
}

type imageBuild struct {
	name, tag, dir string
	args           []string
}

// images are the images a release builds: PHP, and Caddy with Coraza
// (releases before body inspection have no images/caddy).
func (a *Applier) images() []imageBuild {
	php := imageBuild{name: "PHP", tag: a.PHPImage, dir: "php"}
	if _, tag, ok := strings.Cut(a.PHPImage, ":"); ok {
		php.args = []string{"--build-arg", "PHP_VERSION=" + tag}
	}
	out := []imageBuild{php}
	if _, err := os.Stat(filepath.Join(a.Staged, "images", "caddy", "Dockerfile")); err == nil && a.CaddyImage != "" {
		out = append(out, imageBuild{name: "Caddy", tag: a.CaddyImage, dir: "caddy"})
	}
	return out
}

func (a *Applier) install() error {
	p := a.Paths
	// Copies left by an earlier update are from an older version: a
	// rollback must never find those if taking fresh copies fails below.
	for _, f := range []string{p.Bin + ".prev", p.Unit + ".prev"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// Binary: keep a copy, then atomic rename over the old one.
	if err := copyFile(p.Bin, p.Bin+".prev", 0o755); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(a.Staged, "wpgenie"), p.Bin+".new", 0o755); err != nil {
		return err
	}
	if err := os.Rename(p.Bin+".new", p.Bin); err != nil {
		return err
	}
	// Sources (compose file, image build context): whole-directory swap.
	os.RemoveAll(p.Share + ".prev")
	if err := os.Rename(p.Share, p.Share+".prev"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, d := range []string{"deploy", "images"} {
		if err := copyDir(filepath.Join(a.Staged, d), filepath.Join(p.Share, d)); err != nil {
			return err
		}
	}
	for _, f := range []string{"LICENSE", "README.md", "VERSION"} {
		if _, err := os.Stat(filepath.Join(a.Staged, f)); err == nil {
			if err := copyFile(filepath.Join(a.Staged, f), filepath.Join(p.Share, f), 0o644); err != nil {
				return err
			}
		}
	}
	if err := copyFile(p.Unit, p.Unit+".prev", 0o644); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return copyFile(filepath.Join(a.Staged, "deploy", "wpgenie.service"), p.Unit, 0o644)
}

func (a *Applier) compose(ctx context.Context) error {
	return a.run(ctx, "docker", "compose", "-f", filepath.Join(a.Paths.Share, "deploy", "docker-compose.yml"),
		"--env-file", a.Paths.EnvFile, "up", "-d", "--quiet-pull")
}

func (a *Applier) waitVersion(ctx context.Context, want string) error {
	timeout := a.HealthTimeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		v, err := a.Version(ctx)
		if err == nil && v == want {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("running version is %q", v)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// rollback restores the previous binary, sources, unit and image tags
// and restarts the previous version.
func (a *Applier) rollback(ctx context.Context, to string, oldImages map[string]string, cause error) error {
	a.status(PhaseApplying, to, "rolling back: "+cause.Error())
	p := a.Paths
	var errs []error
	if _, err := os.Stat(p.Bin + ".prev"); err == nil {
		if err := copyFile(p.Bin+".prev", p.Bin+".new", 0o755); err == nil {
			errs = append(errs, os.Rename(p.Bin+".new", p.Bin))
		} else {
			errs = append(errs, err)
		}
	}
	if _, err := os.Stat(p.Share + ".prev"); err == nil {
		errs = append(errs, os.RemoveAll(p.Share), os.Rename(p.Share+".prev", p.Share))
	}
	if _, err := os.Stat(p.Unit + ".prev"); err == nil {
		errs = append(errs, copyFile(p.Unit+".prev", p.Unit, 0o644))
	}
	errs = append(errs, a.run(ctx, "systemctl", "daemon-reload"))
	for tag, id := range oldImages {
		if id != "" {
			errs = append(errs, a.run(ctx, "docker", "tag", id, tag))
		}
	}
	errs = append(errs, a.compose(ctx))
	errs = append(errs, a.run(ctx, "systemctl", "restart", "wpgenie"))
	if err := errors.Join(errs...); err != nil {
		a.status(PhaseFailed, to, fmt.Sprintf("update failed (%v) and so did the rollback (%v): WPGenie needs attention; "+
			"the previous binary is %s.prev", cause, err, p.Bin))
		return errors.Join(cause, err)
	}
	if err := a.waitVersion(ctx, a.From); err != nil {
		a.status(PhaseFailed, to, fmt.Sprintf("update failed (%v); rolled back, but %s is not answering: %v", cause, a.From, err))
		return errors.Join(cause, err)
	}
	a.status(PhaseRolledBack, to, fmt.Sprintf("update to %s failed and was rolled back; still running %s. Cause: %v", to, a.From, cause))
	return cause
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return copyFile(p, target, info.Mode().Perm())
		}
		return fmt.Errorf("unexpected file type: %s", p)
	})
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// ExecRun is the real Applier.Run.
func ExecRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// LaunchSystemd starts the applier as a transient systemd unit: outside
// the daemon's cgroup (restarting the daemon must not kill it) and outside
// its sandbox. The applier is a copy of the running, trusted binary; the
// staged release's binary only runs once installed.
func LaunchSystemd(ctx context.Context, staged string) error {
	if os.Getenv("INVOCATION_ID") == "" {
		return ErrNotService
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	applier := filepath.Join(staged, "applier")
	if err := copyFile(self, applier, 0o700); err != nil {
		return err
	}
	// RuntimeMaxSec bounds a simple unit's whole run (TimeoutStartSec would not).
	args := []string{"--unit=wpgenie-update", "--collect", "--quiet", "--property=RuntimeMaxSec=45min"}
	if c := os.Getenv("WPGENIE_CONFIG"); c != "" {
		args = append(args, "--setenv=WPGENIE_CONFIG="+c)
	}
	args = append(args, applier, "update", "apply", staged)
	out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already") {
			return ErrBusy
		}
		return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
