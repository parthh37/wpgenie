// Package logship ships WPGenie's logs to S3-compatible object storage,
// so they're kept (and searchable elsewhere) without filling the server's
// disk.
//
// The shipper is Vector (vector.dev, MPL-2.0) in a container the daemon
// manages, wpgenie-vector: it tails the logs other programs write (Caddy's
// access log, the mail server's, containers' own output), and ships the
// spool, where the daemon writes the logs it produces or rescues: the PHP
// error logs and the WAF audit log it truncates after reading them, the
// shield's security events (kept only in memory otherwise), its own log,
// and rows of its tables (audit log, jobs, accounts' activity, e-mail).
// Objects land at <prefix><server>/<type>/YYYY/MM/DD/HH-<uuid>.log.gz,
// JSON lines. Every server of a cluster ships its own logs, with the
// panel's settings.
//
// The container is hardened: no capabilities but DAC_READ_SEARCH (to read
// logs other users own), a read-only root filesystem, everything mounted
// read-only but its data directory (checkpoints, disk buffer) and the
// spool (it deletes what it shipped), a memory limit, metrics on its own
// loopback only. Its configuration (DataDir/logship/vector.yaml, 0600) is
// generated; the credentials go in through an env file (0600), never in
// argv. It's recreated only when either changes.
package logship

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parthh37/wpgenie/internal/offload"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// Docker runs docker commands (runtime.Docker).
type Docker interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
}

// Storage reaches the bucket (offload.Rclone).
type Storage interface {
	Put(ctx context.Context, t offload.Target, name string, data []byte) error
	DeleteFile(ctx context.Context, t offload.Target, name string) error
	List(ctx context.Context, t offload.Target) (map[string]offload.Object, error)
	Download(ctx context.Context, t offload.Target, paths []string, dir string) (offload.Stats, error)
	Delete(ctx context.Context, t offload.Target, paths []string) (offload.Stats, error)
}

// Config is where things are on this server.
type Config struct {
	Dir   string // DataDir/logship: configuration, spool, Vector's data
	Image string // timberio/vector:<version>-alpine
	// AccessLog is Caddy's access log; MailLogDir the mail server's logs
	// ("": no mail server here).
	AccessLog  string
	MailLogDir string
}

// Container is the shipper's container name.
const Container = "wpgenie-vector"

// Service runs log shipping on one server.
type Service struct {
	Store  *store.Store
	Docker Docker
	Rclone Storage
	Log    *slog.Logger
	Cfg    Config
	// Server is this server's name in object keys: "panel", or a node's ID.
	Server func() string
	// Panel: this server trims the archive (every server's objects).
	Panel bool
	// Resync re-renders Caddy's configuration (how many access logs it
	// keeps changed; see AccessLogKeep).
	Resync func(ctx context.Context) error
	Now    func() time.Time

	spool *Spool
	kick  chan struct{}
	cur   atomic.Pointer[Settings]
	sites atomic.Pointer[map[string]string] // host -> site ID

	mu sync.Mutex // st
	st runState
	// pollMu: one metrics poll at a time (the loop's and a status
	// request's), or an older reading would look like a counter reset.
	pollMu sync.Mutex
}

// runState is what the loop learnt, for the status.
type runState struct {
	dockerChecked bool
	jsonFile      bool   // Docker's default logging driver is json-file
	dockerRoot    string // /var/lib/docker
	keep          int    // the access logs Caddy was last told to keep
	spec          string // the running container's
	applyErr      string
	appliedAt     time.Time

	metrics     *vectorMetrics
	metricsAt   time.Time
	metricsErr  string
	prevRecv    map[string][2]float64
	prevSent    float64
	prevErrors  float64
	lastUpload  time.Time
	lastErrorAt time.Time
	exportErr   string
}

// persisted is what outlives a restart (settings key logship_state).
type persisted struct {
	LastUpload  time.Time `json:"last_upload,omitzero"`
	RetentionAt time.Time `json:"retention_at,omitzero"`
	Deleted     int       `json:"retention_deleted"`
	RetErr      string    `json:"retention_error,omitempty"`
}

const stateKey = "logship_state"

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ServerName is this server's name in the archive's keys.
func (s *Service) ServerName() string { return s.server() }

func (s *Service) server() string {
	if s.Server != nil {
		if n := s.Server(); serverRe.MatchString(n) {
			return n
		}
	}
	return "panel"
}

// Load reads the settings and prepares the spool. Call it before wiring
// the taps (they write to the spool) and Run.
func (s *Service) Load(ctx context.Context) error {
	for _, d := range []string{s.Cfg.Dir, filepath.Join(s.Cfg.Dir, "data"), filepath.Join(s.Cfg.Dir, "tables")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if s.spool == nil {
		s.spool = NewSpool(filepath.Join(s.Cfg.Dir, "spool"), s.Log)
		s.spool.Now = s.Now
		if err := s.spool.Open(); err != nil {
			return err
		}
	}
	s.kick = make(chan struct{}, 1)
	set, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	s.setCurrent(set)
	s.st.keep = s.AccessLogKeep() // what the first proxy sync renders
	s.st.lastUpload = s.loadPersisted(ctx).LastUpload
	if idx, err := s.Store.DomainIndex(ctx); err == nil {
		s.sites.Store(&idx)
	}
	return nil
}

// Spool is where the daemon's own logs go (see Tee).
func (s *Service) Spool() *Spool { return s.spool }

func (s *Service) current() Settings {
	if p := s.cur.Load(); p != nil {
		return *p
	}
	return DefaultSettings()
}

// setCurrent makes set the settings in force: what the spool takes.
func (s *Service) setCurrent(set Settings) {
	s.cur.Store(&set)
	if s.spool == nil {
		return
	}
	if !set.Enabled {
		s.spool.Configure(nil, int64(set.SpoolCapMB)<<20)
		return
	}
	types := map[string]bool{}
	for _, t := range Types {
		types[t.Name] = t.Collect != collectTailed && set.Types[t.Name]
	}
	s.spool.Configure(types, int64(set.SpoolCapMB)<<20)
}

// Kick applies the settings now (rather than at the next pass).
func (s *Service) Kick() {
	if s.kick == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// AccessLogKeep is how many rotated access logs Caddy keeps: fewer while
// they're shipped (0: Caddy's default).
func (s *Service) AccessLogKeep() int {
	set := s.current()
	if !set.Enabled || !set.Types[TypeAccess] {
		return 0
	}
	return set.LocalAccessLogs
}

// siteOf is the site a host name belongs to ("" when none).
func (s *Service) siteOf(host string) string {
	if p := s.sites.Load(); p != nil {
		return (*p)[strings.ToLower(host)]
	}
	return ""
}

// Run ships until ctx ends: the spool's writer, the exporters every 15
// seconds; the shipper's container, its tables and metrics every minute;
// the archive's retention every night (on the panel).
func (s *Service) Run(ctx context.Context) {
	go s.spool.Run(ctx)
	s.apply(ctx)
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	var lastMinute, lastDay time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
			s.apply(ctx)
			lastMinute = s.now()
		case <-t.C:
		}
		if err := s.export(ctx); err != nil && ctx.Err() == nil {
			s.setExportErr(err)
			s.Log.Warn("logship: exporting", "err", err)
		} else {
			s.setExportErr(nil)
		}
		now := s.now()
		if now.Sub(lastMinute) >= time.Minute {
			lastMinute = now
			s.apply(ctx)
			s.pollMetrics(ctx, 0)
			s.recordVolumes(ctx)
		}
		if now.Sub(lastDay) >= time.Hour {
			lastDay = now
			s.daily(ctx)
		}
	}
}

func (s *Service) setExportErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.exportErr = ""
	if err != nil {
		s.st.exportErr = err.Error()
	}
}

// daily trims the archive (panel) and the volume history, at most once a
// day (checked hourly; the last run survives restarts).
func (s *Service) daily(ctx context.Context) {
	p := s.loadPersisted(ctx)
	if s.now().Sub(p.RetentionAt) < 23*time.Hour {
		return
	}
	if err := s.Store.PruneLogVolumes(ctx, s.now().AddDate(0, 0, -60)); err != nil {
		s.Log.Warn("logship: pruning volumes", "err", err)
	}
	set := s.current()
	if !s.Panel || !set.Enabled {
		return
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	n, err := s.trimArchive(c, set)
	p = s.loadPersisted(ctx)
	p.RetentionAt, p.Deleted, p.RetErr = s.now(), n, ""
	if err != nil {
		p.RetErr = err.Error()
		s.Log.Warn("logship: deleting old archives", "err", err, "deleted", n)
	} else if n > 0 {
		s.Log.Info("logship: deleted old archives", "objects", n, "days", set.ArchiveRetentionDays)
	}
	s.savePersisted(ctx, p)
}

func (s *Service) loadPersisted(ctx context.Context) persisted {
	var p persisted
	if v, err := s.Store.Setting(ctx, stateKey); err == nil && v != "" {
		json.Unmarshal([]byte(v), &p)
	}
	return p
}

func (s *Service) savePersisted(ctx context.Context, p persisted) {
	b, _ := json.Marshal(p)
	if err := s.Store.SetSetting(ctx, stateKey, string(b)); err != nil {
		s.Log.Warn("logship: saving state", "err", err)
	}
}

// ---- The shipper's container ----

// checkDocker learns Docker's default logging driver and root directory
// (once it answers).
func (s *Service) checkDocker(ctx context.Context) {
	s.mu.Lock()
	done := s.st.dockerChecked
	s.mu.Unlock()
	if done {
		return
	}
	out, err := s.Docker.Run(ctx, nil, "info", "--format", "{{.LoggingDriver}}|{{.DockerRootDir}}")
	if err != nil {
		return
	}
	driver, root, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.dockerChecked = true
	s.st.jsonFile = driver == "json-file"
	if filepath.IsAbs(root) && !strings.ContainsAny(root, ":,\n") {
		s.st.dockerRoot = filepath.Clean(root)
	}
}

// Available reports whether a type can be collected on this server: the
// mail server's logs exist only where it runs, containers' own output
// only with Docker's json-file logs.
func (s *Service) Available(typ string) bool {
	switch typ {
	case TypeAccess:
		return s.Cfg.AccessLog != ""
	case TypeMail:
		if s.Cfg.MailLogDir == "" {
			return false
		}
		fi, err := os.Stat(s.Cfg.MailLogDir)
		return err == nil && fi.IsDir()
	case TypeContainers:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.st.jsonFile && s.st.dockerRoot != ""
	}
	return TypeByName(typ) != nil
}

// tailed are the tailed types that ship from this server.
func (s *Service) tailed(set Settings) []string {
	var out []string
	for _, t := range Types {
		if t.Collect == collectTailed && set.Types[t.Name] && s.Available(t.Name) {
			out = append(out, t.Name)
		}
	}
	return out
}

// apply brings the server in line with the settings: local retention,
// the tables, the shipper's configuration and container.
func (s *Service) apply(ctx context.Context) {
	set := s.current()
	s.checkDocker(ctx)
	s.mu.Lock()
	jsonFile := s.st.jsonFile
	keepWas := s.st.keep
	s.mu.Unlock()

	// Local retention: capped container logs and fewer old access logs
	// while they ship.
	if set.Enabled && jsonFile {
		runtime.SetContainerLogLimit(set.ContainerLogMB, containerLogFiles)
	} else {
		runtime.SetContainerLogLimit(0, 0)
	}
	if keep := s.AccessLogKeep(); keep != keepWas && s.Resync != nil {
		if err := s.Resync(ctx); err != nil {
			s.Log.Warn("logship: applying the access logs kept", "err", err)
		} else {
			s.mu.Lock()
			s.st.keep = keep
			s.mu.Unlock()
		}
	}

	err := s.ensure(ctx, set)
	s.mu.Lock()
	s.st.appliedAt, s.st.applyErr = s.now(), ""
	if err != nil {
		s.st.applyErr = err.Error()
	}
	s.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		s.Log.Warn("logship: starting the shipper", "err", err)
	}
}

func (s *Service) ensure(ctx context.Context, set Settings) error {
	if !set.Enabled || set.Destination.check() != nil {
		return s.stopShipper(ctx)
	}
	changed, err := s.writeTables(ctx, set)
	if err != nil {
		return err
	}
	cfg, err := vectorConfig(vectorInput{Settings: set, Server: s.server(), Tailed: s.tailed(set),
		AccessLog: filepath.Base(s.Cfg.AccessLog)})
	if err != nil {
		return err
	}
	env, err := envFile(set.Destination)
	if err != nil {
		return err
	}
	args, err := s.runArgs(set)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bytes.Join([][]byte{cfg, env, []byte(strings.Join(args, "\x00"))}, []byte{0}))
	spec := hex.EncodeToString(sum[:8])

	state, have, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if have == spec && state == "running" {
		if changed {
			// The tables are reloaded on SIGHUP (the files' times changed).
			if _, err := s.Docker.Run(ctx, nil, "kill", "--signal", "HUP", Container); err != nil {
				return fmt.Errorf("reloading the shipper's tables: %w", err)
			}
		}
		s.setSpec(spec)
		return nil
	}
	if err := writeFile(filepath.Join(s.Cfg.Dir, "vector.yaml"), cfg); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.Cfg.Dir, "vector.env"), env); err != nil {
		return err
	}
	if state != "" {
		if err := s.removeShipper(ctx); err != nil {
			return err
		}
	}
	run := append([]string{"run", "-d", "--name", Container, "--label", "wpgenie.spec=" + spec}, args...)
	if out, err := s.Docker.Run(ctx, nil, run...); err != nil {
		s.Docker.Run(ctx, nil, "rm", "-f", Container) // a failed run can leave a created container
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

// inspect is the container's state ("" when there's none) and spec label.
func (s *Service) inspect(ctx context.Context) (state, spec string, err error) {
	out, err := s.Docker.Run(ctx, nil, "inspect", "--type", "container", "--format",
		`{{.State.Status}}|{{index .Config.Labels "wpgenie.spec"}}`, Container)
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)+err.Error()), "no such") {
			return "", "", nil
		}
		return "", "", err
	}
	state, spec, _ = strings.Cut(strings.TrimSpace(string(out)), "|")
	return state, spec, nil
}

// stopShipper stops the container if there is one (shipping turned off).
func (s *Service) stopShipper(ctx context.Context) error {
	state, _, err := s.inspect(ctx)
	if err != nil || state == "" {
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

// runArgs are the container's settings (see the package comment).
func (s *Service) runArgs(set Settings) ([]string, error) {
	d := s.Cfg.Dir
	args := []string{"--label", "wpgenie.logship=1", "--restart", "unless-stopped",
		// The internet (the bucket), not the sites' network with the databases.
		"--network", "bridge",
		"--cap-drop", "ALL", "--cap-add", "DAC_READ_SEARCH", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
		"--memory", "384m", "--pids-limit", "256", "--stop-timeout", "30",
		"--env-file", filepath.Join(d, "vector.env"),
	}
	mounts := [][3]string{
		{filepath.Join(d, "vector.yaml"), ctrConfigFile, "ro"},
		{filepath.Join(d, "tables"), ctrTables, "ro"},
		{filepath.Join(d, "data"), ctrData, ""},
		{filepath.Join(d, "spool"), ctrSpool, ""},
	}
	for _, typ := range s.tailed(set) {
		switch typ {
		case TypeAccess:
			mounts = append(mounts, [3]string{filepath.Dir(s.Cfg.AccessLog), ctrCaddy, "ro"})
		case TypeMail:
			mounts = append(mounts, [3]string{s.Cfg.MailLogDir, ctrMail, "ro"})
		case TypeContainers:
			s.mu.Lock()
			root := s.st.dockerRoot
			s.mu.Unlock()
			mounts = append(mounts, [3]string{filepath.Join(root, "containers"), ctrContainers, "ro"})
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

// envFile holds the credentials: Vector's S3 sink takes them from the
// environment (AWS's default chain), so they're never in its
// configuration or anyone's argv.
func envFile(d Destination) ([]byte, error) {
	lines := []string{"AWS_ACCESS_KEY_ID=" + d.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + d.SecretKey,
		// Never ask a cloud's instance metadata for other credentials.
		"AWS_EC2_METADATA_DISABLED=true"}
	for _, l := range lines {
		if strings.ContainsAny(l, "\n\r\x00") {
			return nil, errors.New("credentials contain a line break")
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
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

// ---- Enrichment tables ----

var containerNameRe = regexp.MustCompile(`^(wpg-|wpgenie-)[A-Za-z0-9_.-]{1,120}$`)

// writeTables writes the host -> site table (and the container ID -> name
// table when containers ship), reporting whether either changed.
func (s *Service) writeTables(ctx context.Context, set Settings) (bool, error) {
	idx, err := s.Store.DomainIndex(ctx)
	if err != nil {
		return false, err
	}
	s.sites.Store(&idx)
	hosts := make([][]string, 0, len(idx))
	for h, id := range idx {
		hosts = append(hosts, []string{h, id})
	}
	changed, err := s.writeTable("sites.csv", []string{"host", "site"}, hosts)
	if err != nil || !slices.Contains(s.tailed(set), TypeContainers) {
		return changed, err
	}
	out, err := s.Docker.Run(ctx, nil, "ps", "-a", "--no-trunc", "--format", "{{.ID}} {{.Names}}")
	if err != nil {
		return changed, fmt.Errorf("listing containers: %w", err)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && len(id) == 64 && containerNameRe.MatchString(name) {
			rows = append(rows, []string{id, name})
		}
	}
	c2, err := s.writeTable("containers.csv", []string{"id", "name"}, rows)
	return changed || c2, err
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

// ---- Metrics and volumes ----

// pollMetrics reads Vector's metrics (from inside its container: they're
// only on its loopback) and notes uploads and errors, unless they were
// read less than fresh ago.
func (s *Service) pollMetrics(ctx context.Context, fresh time.Duration) {
	if !s.current().Enabled {
		return
	}
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	s.mu.Lock()
	recent := !s.st.metricsAt.IsZero() && s.now().Sub(s.st.metricsAt) < fresh
	s.mu.Unlock()
	if recent {
		return
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := s.Docker.Run(c, nil, "exec", Container, "wget", "-q", "-T", "10", "-O", "-", "http://"+metricsAddr+"/metrics")
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.metricsAt = now
	if err != nil {
		s.st.metricsErr = offload.CleanError(err.Error())
		return
	}
	m := parseVectorMetrics(out)
	s.st.metricsErr = ""
	// Counters that grew (or restarted with Vector, and grew since) mean
	// uploads (errors) since the last poll; the first poll only learns them.
	grew := func(cur, prev float64) bool { return s.st.metrics != nil && (cur > prev || (cur > 0 && cur < prev)) }
	if grew(m.Sent, s.st.prevSent) || (m.Sent > 0 && s.st.lastUpload.IsZero()) {
		s.st.lastUpload = now
	}
	if grew(m.Errors, s.st.prevErrors) {
		s.st.lastErrorAt = now
	}
	// Tailed types' volumes: what Vector read since the last poll (its
	// counters restart with it).
	for typ, cur := range m.Received {
		prev := s.st.prevRecv[typ]
		ev, by := cur[0]-prev[0], cur[1]-prev[1]
		if cur[0] < prev[0] || cur[1] < prev[1] {
			ev, by = cur[0], cur[1]
		}
		if (ev > 0 || by > 0) && s.st.prevRecv != nil {
			if err := s.Store.AddLogVolume(ctx, store.LogVolume{Day: now, Kind: typ, Events: int64(ev), Bytes: int64(by)}); err != nil {
				s.Log.Warn("logship: recording volumes", "err", err)
			}
		}
	}
	s.st.prevRecv, s.st.prevSent, s.st.prevErrors, s.st.metrics = m.Received, m.Sent, m.Errors, &m
	if !s.st.lastUpload.IsZero() {
		p := s.loadPersisted(ctx)
		if !p.LastUpload.Equal(s.st.lastUpload) {
			p.LastUpload = s.st.lastUpload
			s.savePersisted(ctx, p)
		}
	}
}

// recordVolumes adds what the spool wrote (and dropped) to today's
// volumes.
func (s *Service) recordVolumes(ctx context.Context) {
	now := s.now()
	for typ, c := range s.spool.Take() {
		if err := s.Store.AddLogVolume(ctx, store.LogVolume{Day: now, Kind: typ, Events: c.Events, Bytes: c.Bytes,
			Dropped: c.Dropped}); err != nil {
			s.Log.Warn("logship: recording volumes", "err", err)
		}
	}
}
