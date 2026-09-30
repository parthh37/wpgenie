package logship

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/offload"
	"github.com/parthh37/wpgenie/internal/store"
)

func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeDocker answers the docker commands the service runs, and records
// them.
type fakeDocker struct {
	mu      sync.Mutex
	calls   [][]string
	state   string // the shipper's: "" (none), running, exited
	spec    string
	driver  string // docker info's logging driver
	ps      string // docker ps output
	metrics string // what wget prints
	runErr  error
}

func (d *fakeDocker) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, slices.Clone(args))
	switch args[0] {
	case "info":
		if d.driver == "" {
			return nil, errors.New("docker info: Cannot connect to the Docker daemon")
		}
		return []byte(d.driver + "|/var/lib/docker\n"), nil
	case "inspect":
		if d.state == "" {
			return []byte("Error: No such container: wpgenie-vector"), errors.New("docker inspect: exit status 1")
		}
		return []byte(d.state + "|" + d.spec + "\n"), nil
	case "run":
		if d.runErr != nil {
			return []byte("pull access denied"), d.runErr
		}
		for i, a := range args {
			if a == "--label" && strings.HasPrefix(args[i+1], "wpgenie.spec=") {
				d.spec = strings.TrimPrefix(args[i+1], "wpgenie.spec=")
			}
		}
		d.state = "running"
	case "rm":
		d.state, d.spec = "", ""
	case "ps":
		return []byte(d.ps), nil
	case "exec":
		if d.state != "running" {
			return nil, errors.New("docker exec: container is not running")
		}
		return []byte(d.metrics), nil
	}
	return nil, nil
}

// commands returns the recorded commands' first words (run, inspect…).
func (d *fakeDocker) commands() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.calls {
		out = append(out, c[0])
	}
	return out
}

func (d *fakeDocker) find(verb string) [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out [][]string
	for _, c := range d.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

func (d *fakeDocker) reset() {
	d.mu.Lock()
	d.calls = nil
	d.mu.Unlock()
}

// fakeStorage is a bucket in memory, keyed by full object key.
type fakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	mod     map[string]time.Time
	putErr  error
	delErr  error
	targets []offload.Target
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objects: map[string][]byte{}, mod: map[string]time.Time{}}
}

func (f *fakeStorage) record(t offload.Target) {
	f.targets = append(f.targets, t)
}

func (f *fakeStorage) Put(_ context.Context, t offload.Target, name string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(t)
	if err := t.Validate(); err != nil {
		return err
	}
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[t.Prefix+name] = data
	return nil
}

func (f *fakeStorage) DeleteFile(_ context.Context, t offload.Target, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(t)
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.objects, t.Prefix+name)
	return nil
}

func (f *fakeStorage) List(_ context.Context, t offload.Target) (map[string]offload.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(t)
	out := map[string]offload.Object{}
	for k, v := range f.objects {
		if rel, ok := strings.CutPrefix(k, t.Prefix); ok {
			out[rel] = offload.Object{Size: int64(len(v)), Modified: f.mod[k]}
		}
	}
	return out, nil
}

func (f *fakeStorage) Download(_ context.Context, t offload.Target, paths []string, dir string) (offload.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(t)
	for _, p := range paths {
		b, ok := f.objects[t.Prefix+p]
		if !ok {
			continue // rclone copies what exists
		}
		if err := os.WriteFile(filepath.Join(dir, p), b, 0o600); err != nil {
			return offload.Stats{}, err
		}
	}
	return offload.Stats{Objects: int64(len(paths))}, nil
}

func (f *fakeStorage) Delete(_ context.Context, t offload.Target, paths []string) (offload.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(t)
	for _, p := range paths {
		delete(f.objects, t.Prefix+p)
	}
	return offload.Stats{Deletes: int64(len(paths))}, nil
}

// testDestination is a complete destination.
func testDestination() Destination {
	return Destination{Provider: "minio", Endpoint: "https://minio.example.com:9000", Region: "", Bucket: "logs-bucket",
		Prefix: "wpgenie/", AccessKeyID: "AKIAEXAMPLE123", SecretKey: "s3cr3t-Key/with+chars", PathStyle: true}
}

// newService is a service on a temporary directory with fakes.
func newService(t *testing.T) (*Service, *fakeDocker, *fakeStorage) {
	t.Helper()
	dir := t.TempDir()
	d := &fakeDocker{driver: "json-file"}
	fs := newFakeStorage()
	logs := filepath.Join(dir, "log")
	os.MkdirAll(logs, 0o755)
	s := &Service{Store: newStore(t), Docker: d, Rclone: fs, Log: quietLog(),
		Cfg: Config{Dir: filepath.Join(dir, "logship"), Image: "timberio/vector:0.58.0-alpine",
			AccessLog: filepath.Join(logs, "access.log")},
		Server: func() string { return "panel" }, Panel: true}
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, d, fs
}

// enable turns shipping on with the test destination.
func enable(t *testing.T, s *Service, change func(*Settings)) Settings {
	t.Helper()
	set := DefaultSettings()
	set.Enabled = true
	set.Destination = testDestination()
	if change != nil {
		change(&set)
	}
	saved, err := s.SetSettings(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// readSpool returns every line of a type's spool files (complete and
// being written), in file order.
func readSpool(t *testing.T, dir, typ string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(dir, typ))
	var lines []string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, typ, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for l := range bytes.Lines(b) {
			if !bytes.Contains(l, []byte("wpgenie_spool")) {
				lines = append(lines, strings.TrimSuffix(string(l), "\n"))
			}
		}
	}
	return lines
}

func mustf(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}
