package logship

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/offload"
)

// The shipper's container, wpgenie-vector. It runs as root with no
// capabilities at all: not even DAC_READ_SEARCH, which with Docker's
// default seccomp profile allows open_by_handle_at, and so reading any
// file of the host through a bind mount. Everything it reads is therefore
// readable by uid 0 without privileges: the daemon's own files are root's
// (0600/0700), and the logs other users write (Caddy's access log, the
// mail server's) are made group-readable and their groups added to the
// container (see logGroups).

// ensureTimeout bounds one attempt at starting the shipper (the image's
// pull included).
const ensureTimeout = 10 * time.Minute

// containerInfo is the shipper's container as Docker reports it.
type containerInfo struct {
	State    string // running | restarting | exited | … ("": there's none)
	Spec     string
	ExitCode int
	Restarts int
	Error    string
}

// stuck: the container exists with the current spec but doesn't run.
// Docker's restart policy (with its backoff) handles crashes: recreating
// it every minute would only hide them.
func (c containerInfo) stuck() bool {
	return c.State == "restarting" || c.State == "exited" || c.State == "dead" || c.State == "paused"
}

func (s *Service) ensure(ctx context.Context, set Settings) error {
	if !set.Enabled || set.Destination.check() != nil {
		return s.stopShipper(ctx)
	}
	changed, err := s.writeTables(ctx)
	if err != nil {
		return err
	}
	cfg, err := vectorConfig(vectorInput{Settings: set, Server: s.server(), Tailed: s.tailed(set),
		AccessLog: filepath.Base(s.Cfg.AccessLog)})
	if err != nil {
		return err
	}
	creds, err := credentialsFile(set.Destination)
	if err != nil {
		return err
	}
	groups, err := s.logGroups(set)
	if err != nil {
		return err
	}
	args, err := s.runArgs(set, groups)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bytes.Join([][]byte{cfg, creds, []byte(strings.Join(args, "\x00"))}, []byte{0}))
	spec := hex.EncodeToString(sum[:8])
	// An earlier version passed the keys in an env file: gone.
	os.Remove(filepath.Join(s.Cfg.Dir, "vector.env"))

	c, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if c.Spec == spec && c.State == "running" {
		if changed {
			// The tables are reloaded on SIGHUP (the files' times changed).
			if _, err := s.Docker.Run(ctx, nil, "kill", "--signal", "HUP", Container); err != nil {
				return fmt.Errorf("reloading the shipper's tables: %w", err)
			}
		}
		s.setSpec(spec)
		return nil
	}
	if c.Spec == spec && c.stuck() {
		s.setSpec(spec)
		return nil // see stuck; the status reports it
	}
	if err := writeFile(filepath.Join(s.Cfg.Dir, "vector.yaml"), cfg); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.Cfg.Dir, "secrets", "credentials"), creds); err != nil {
		return err
	}
	if c.State != "" {
		if err := s.removeShipper(ctx); err != nil {
			return err
		}
	}
	run := append([]string{"run", "-d", "--name", Container, "--label", "wpgenie.spec=" + spec}, args...)
	if out, err := s.Docker.Run(ctx, nil, run...); err != nil {
		// A failed run can leave a created container (on a context of its
		// own: this one may be what ran out).
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		s.Docker.Run(rc, nil, "rm", "-f", Container)
		cancel()
		return fmt.Errorf("starting the shipper: %w", errors.Join(err, errors.New(offload.CleanError(string(out)))))
	}
	s.Log.Info("logship: shipper started", "image", s.Cfg.Image, "server", s.server())
	s.setSpec(spec)
	return nil
}

func (s *Service) setSpec(spec string) {
	s.mu.Lock()
	s.st.spec = spec
	s.mu.Unlock()
}

// inspect is the shipper's container (State "" when there's none).
func (s *Service) inspect(ctx context.Context) (containerInfo, error) {
	out, err := s.Docker.Run(ctx, nil, "inspect", "--type", "container", "--format",
		`{{.State.Status}}|{{index .Config.Labels "wpgenie.spec"}}|{{.State.ExitCode}}|{{.RestartCount}}|{{.State.Error}}`, Container)
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)+err.Error()), "no such") {
			return containerInfo{}, nil
		}
		return containerInfo{}, err
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), "|", 5)
	for len(f) < 5 {
		f = append(f, "")
	}
	c := containerInfo{State: f[0], Spec: f[1], Error: f[4]}
	c.ExitCode, _ = strconv.Atoi(f[2])
	c.Restarts, _ = strconv.Atoi(f[3])
	return c, nil
}

// stopShipper stops the container if there is one (shipping turned off).
func (s *Service) stopShipper(ctx context.Context) error {
	c, err := s.inspect(ctx)
	if err != nil || c.State == "" {
		return err
	}
	s.setSpec("")
	s.Log.Info("logship: shipper stopped")
	return s.removeShipper(ctx)
}

// removeShipper stops Vector gracefully (it flushes its disk buffer and
// checkpoints) and removes its container.
func (s *Service) removeShipper(ctx context.Context) error {
	s.Docker.Run(ctx, nil, "stop", "--time", "30", Container)
	out, err := s.Docker.Run(ctx, nil, "rm", "-f", Container)
	if err != nil && !strings.Contains(string(out)+err.Error(), "No such container") {
		return err
	}
	return nil
}

// runArgs are the container's settings (see the package comment); groups
// are added to its process (see logGroups).
func (s *Service) runArgs(set Settings, groups []int) ([]string, error) {
	d := s.Cfg.Dir
	args := []string{"--label", "wpgenie.logship=1", "--restart", "unless-stopped",
		// The internet (the bucket), not the sites' network with the databases.
		"--network", "bridge",
		"--user", "0:0", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
		"--memory", "384m", "--pids-limit", "256", "--stop-timeout", "30",
	}
	for _, g := range groups {
		args = append(args, "--group-add", strconv.Itoa(g))
	}
	mounts := [][3]string{
		{filepath.Join(d, "vector.yaml"), ctrConfigFile, "ro"},
		{filepath.Join(d, "tables"), ctrTables, "ro"},
		{filepath.Join(d, "secrets"), ctrSecrets, "ro"},
		{filepath.Join(d, "data"), ctrData, ""},
		{filepath.Join(d, "spool"), ctrSpool, ""},
	}
	for _, typ := range s.tailed(set) {
		switch typ {
		case TypeAccess:
			mounts = append(mounts, [3]string{filepath.Dir(s.Cfg.AccessLog), ctrCaddy, "ro"})
		case TypeMail:
			mounts = append(mounts, [3]string{s.Cfg.MailLogDir, ctrMail, "ro"})
		}
	}
	for _, m := range mounts {
		if !filepath.IsAbs(m[0]) || strings.ContainsAny(m[0], ":,\n") {
			return nil, fmt.Errorf("unsafe mount path %q", m[0])
		}
		v := m[0] + ":" + m[1]
		if m[2] != "" {
			v += ":" + m[2]
		}
		args = append(args, "-v", v)
	}
	return append(args, s.Cfg.Image, "--config", ctrConfigFile), nil
}

// logGroups makes the logs Vector tails readable to a process with uid 0
// and no capabilities, and returns the groups it needs: a file (or its
// directory) owned by root is readable as its owner; otherwise through
// its group, which the daemon (root) makes readable (Caddy creates its
// access log 0600, the mail server's logs are its syslog's). Only the
// files it ships: the WAF audit log next to the access log stays 0600.
func (s *Service) logGroups(set Settings) ([]int, error) {
	var groups []int
	add := func(path string, dir bool) error {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid == 0 {
			return nil
		}
		need := os.FileMode(0o040) // group read
		if dir {
			need = 0o050 // group read and search
		}
		if fi.Mode().Perm()&need != need {
			if err := os.Chmod(path, fi.Mode().Perm()|need); err != nil {
				return fmt.Errorf("letting the shipper read %s: %w", path, err)
			}
		}
		if g := int(st.Gid); g != 0 && !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
		return nil
	}
	for _, typ := range s.tailed(set) {
		var dir string
		var files []string
		switch typ {
		case TypeAccess:
			dir, files = filepath.Dir(s.Cfg.AccessLog), []string{s.Cfg.AccessLog}
		case TypeMail:
			dir = s.Cfg.MailLogDir
			files, _ = filepath.Glob(filepath.Join(dir, "*.log"))
		}
		if err := add(dir, true); err != nil {
			return nil, err
		}
		for _, f := range files {
			if err := add(f, false); err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(groups)
	return groups, nil
}

// credentialsFile is the AWS credentials file Vector's S3 sink reads (a
// file, not the environment: docker inspect shows a container's
// environment to anyone who may use Docker). The AWS parser only takes a
// '#' or ';' preceded by whitespace for a comment: with none around '=',
// a key holding them is read whole.
func credentialsFile(d Destination) ([]byte, error) {
	for _, v := range []string{d.AccessKeyID, d.SecretKey} {
		if strings.IndexFunc(v, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
			return nil, errors.New("credentials may only hold printable characters without spaces")
		}
	}
	return []byte("[" + credentialsProfile + "]\naws_access_key_id=" + d.AccessKeyID +
		"\naws_secret_access_key=" + d.SecretKey + "\n"), nil
}

// writeFile replaces a file (0600) unless it already holds b.
func writeFile(path string, b []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- Enrichment table ----

// writeTables writes the host -> site table, reporting whether it
// changed.
func (s *Service) writeTables(ctx context.Context) (bool, error) {
	idx, err := s.Store.DomainIndex(ctx)
	if err != nil {
		return false, err
	}
	s.sites.Store(&idx)
	hosts := make([][]string, 0, len(idx))
	for h, id := range idx {
		hosts = append(hosts, []string{h, id})
	}
	return s.writeTable("sites.csv", []string{"host", "site"}, hosts)
}

// writeTable writes a CSV table with a header, rows sorted (so an equal
// table is an equal file). Vector needs the file to exist even empty.
func (s *Service) writeTable(name string, header []string, rows [][]string) (bool, error) {
	slices.SortFunc(rows, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.Write(header)
	w.WriteAll(rows)
	if err := w.Error(); err != nil {
		return false, err
	}
	path := filepath.Join(s.Cfg.Dir, "tables", name)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b.Bytes()) {
		return false, nil
	}
	return true, writeFile(path, b.Bytes())
}
