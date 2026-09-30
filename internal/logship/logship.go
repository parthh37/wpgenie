// Package logship ships WPGenie's logs to S3-compatible object storage,
// so they're kept (and searchable elsewhere) without filling the server's
// disk.
//
// The shipper is Vector (vector.dev, MPL-2.0) in a container the daemon
// manages, wpgenie-vector: it tails the logs other programs write (Caddy's
// access log, the mail server's), and ships the spool, where the daemon
// writes the logs it produces or rescues: the PHP error logs and the WAF
// audit log it truncates after reading them, the shield's security events
// (kept only in memory otherwise), its own log, rows of its tables (audit
// log, jobs, accounts' activity, e-mail), and its containers' output.
// Objects land at <prefix><server>/<type>/YYYY/MM/DD/HH-<uuid>.log.gz,
// JSON lines. Every server of a cluster ships its own logs, with the
// panel's settings.
//
// The container is hardened (see container.go): root without any
// capability, a read-only root filesystem, everything mounted read-only
// but its data directory (checkpoints, disk buffer) and the spool (it
// deletes what it shipped), a memory limit, metrics on its own loopback
// only. Its configuration (DataDir/logship/vector.yaml, 0600) is
// generated; the keys are in a credentials file (0600, mounted read-only),
// never in argv or its environment. It's recreated only when either
// changes.
package logship

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	// ctail: containers' logs being read (the Run loop's only).
	ctail containerTail
	// nodeSync sends the settings to the cluster's servers that lack them
	// (the panel; see SetNodeSync); nodeVersions: node ID -> the version
	// of the settings it was last sent.
	nodeSync     atomic.Pointer[func(context.Context) error]
	nodeVersions sync.Map
}

// SetNodeSync makes the loop call sync every minute: it sends the
// settings to the servers of a cluster that don't have them yet (so a
// failed push is retried).
func (s *Service) SetNodeSync(sync func(context.Context) error) { s.nodeSync.Store(&sync) }

// NodeVersion is the version of the settings a server was last sent ("":
// none since the panel started).
func (s *Service) NodeVersion(node string) string {
	v, _ := s.nodeVersions.Load(node)
	str, _ := v.(string)
	return str
}

// SetNodeVersion records that a server got a version of the settings.
func (s *Service) SetNodeVersion(node, version string) { s.nodeVersions.Store(node, version) }

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
	// The archive's trimming: every day before TrimmedBefore was trimmed
	// at TrimDest (endpoint|bucket|prefix), for these servers.
	TrimmedBefore string   `json:"trimmed_before,omitempty"`
	TrimDest      string   `json:"trim_dest,omitempty"`
	TrimServers   []string `json:"trim_servers,omitempty"`
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
	for _, d := range []string{s.Cfg.Dir, filepath.Join(s.Cfg.Dir, "data"), filepath.Join(s.Cfg.Dir, "tables"),
		filepath.Join(s.Cfg.Dir, "secrets")} {
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
	// Archives being read when the daemon stopped.
	tmp := filepath.Join(s.Cfg.Dir, "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		return err
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
		if err := errors.Join(s.export(ctx), s.tailContainers(ctx)); err != nil && ctx.Err() == nil {
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
			if f := s.nodeSync.Load(); f != nil {
				if err := (*f)(ctx); err != nil && ctx.Err() == nil {
					s.Log.Warn("logship: sending the settings to other servers", "err", err)
				}
			}
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

	// A registry that stalls a pull (or a Docker that hangs) mustn't
	// freeze the loop: the next pass tries again.
	c, cancel := context.WithTimeout(ctx, ensureTimeout)
	err := s.ensure(c, set)
	cancel()
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
