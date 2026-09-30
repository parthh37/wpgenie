package logship

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/offload"
)

// The archive: what Vector wrote to the bucket, listed and read back with
// rclone (uploads offload's runner: a throwaway container, the keys on its
// stdin), and trimmed every night.

// Archive is one object of the archive.
type Archive struct {
	Key      string    `json:"key"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// archiveKey is a key of the archive, taken apart:
// <prefix><server>/<type>/YYYY/MM/DD/<name>.
type archiveKey struct {
	Server, Type, Name string
	Day                time.Time
}

// dir is the key's directory under the prefix.
func (k archiveKey) dir() string {
	return k.Server + "/" + k.Type + "/" + k.Day.Format("2006/01/02") + "/"
}

var archiveKeyRe = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,62})/([a-z_]{1,32})/(\d{4}/\d{2}/\d{2})/([A-Za-z0-9][A-Za-z0-9._-]{0,200})$`)

// parseArchiveKey checks that key is an object of the archive under
// prefix: nothing else in the bucket can be read (or deleted) through it.
func parseArchiveKey(prefix, key string) (archiveKey, error) {
	rel, ok := strings.CutPrefix(key, prefix)
	if !ok || len(key) > 1024 {
		return archiveKey{}, invalid("not a log archive")
	}
	m := archiveKeyRe.FindStringSubmatch(rel)
	if m == nil || strings.Contains(m[4], "..") || TypeByName(m[2]) == nil {
		return archiveKey{}, invalid("not a log archive")
	}
	day, err := time.Parse("2006/01/02", m[3])
	if err != nil {
		return archiveKey{}, invalid("not a log archive")
	}
	return archiveKey{Server: m[1], Type: m[2], Name: m[4], Day: day}, nil
}

// errNoDestination: the archive needs a complete destination.
var errNoDestination = invalid("set up where logs go first")

func (s *Service) destination() (Destination, error) {
	d := s.current().Destination
	if d.check() != nil {
		return Destination{}, errNoDestination
	}
	return d, nil
}

// Archives lists a server's objects of one type and day.
func (s *Service) Archives(ctx context.Context, server, typ string, day time.Time) ([]Archive, error) {
	if !serverRe.MatchString(server) {
		return nil, invalid("unknown server")
	}
	if TypeByName(typ) == nil {
		return nil, invalid("unknown log type %q", typ)
	}
	d, err := s.destination()
	if err != nil {
		return nil, err
	}
	k := archiveKey{Server: server, Type: typ, Day: day.UTC()}
	t := d.target()
	t.Prefix += k.dir()
	objs, err := s.Rclone.List(ctx, t)
	if err != nil {
		return nil, storageErr("listing the archive", err)
	}
	out := []Archive{}
	for name, o := range objs {
		if strings.Contains(name, "/") {
			continue
		}
		out = append(out, Archive{Key: t.Prefix + name, Name: name, Size: o.Size, Modified: o.Modified})
	}
	slices.SortFunc(out, func(a, b Archive) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ArchiveReader is an object of the archive being read.
type ArchiveReader struct {
	io.Reader
	// Compressed: zstd, which the panel can't decompress (the bytes are
	// the object's as they are).
	Compressed bool
	Name       string
	f          *os.File
	dir        string
}

func (r *ArchiveReader) Close() error {
	r.f.Close()
	return os.RemoveAll(r.dir)
}

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// OpenArchive fetches an object of the archive (key validated against the
// prefix) and returns its lines: gzip is decompressed; zstd can only be
// downloaded as it is.
func (s *Service) OpenArchive(ctx context.Context, key string) (*ArchiveReader, error) {
	d, err := s.destination()
	if err != nil {
		return nil, err
	}
	k, err := parseArchiveKey(d.Prefix, key)
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(s.Cfg.Dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(tmp, "read-")
	if err != nil {
		return nil, err
	}
	t := d.target()
	t.Prefix += k.dir()
	if _, err := s.Rclone.Download(ctx, t, []string{k.Name}, dir); err != nil {
		os.RemoveAll(dir)
		return nil, storageErr("fetching the archive", err)
	}
	f, err := os.Open(filepath.Join(dir, k.Name))
	if err != nil {
		os.RemoveAll(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: that archive isn't in the bucket (any more)", ErrNotFound)
		}
		return nil, err
	}
	r := &ArchiveReader{f: f, dir: dir, Name: k.Name}
	br := bufio.NewReader(f)
	head, _ := br.Peek(4)
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		gz.Multistream(true)
		r.Reader = gz
		r.Name = strings.TrimSuffix(k.Name, ".gz")
	case slices.Equal(head, zstdMagic):
		r.Reader, r.Compressed = br, true
	default:
		// The service decompressed it (it was stored with a
		// Content-Encoding): plain lines.
		r.Reader = br
		r.Name = strings.TrimSuffix(k.Name, ".gz")
	}
	return r, nil
}

// ErrNotFound is an archive (or thing) that isn't there.
var ErrNotFound = errors.New("not found")

// TestResult is the outcome of a connection test, in plain words.
type TestResult struct {
	OK    bool   `json:"ok"`
	Step  string `json:"step,omitempty"`
	Error string `json:"error,omitempty"`
}

// Test writes a small object to the destination and deletes it: what
// shipping (write) and the archive's retention (delete) need. in is the
// form as it is (not saved yet); its secret may be left out when it goes
// where the stored one does.
func (s *Service) Test(ctx context.Context, in Destination) (TestResult, error) {
	cur := s.current().Destination
	if err := in.merge(cur, true); err != nil {
		return TestResult{}, err
	}
	b := make([]byte, 8)
	rand.Read(b)
	name := "wpgenie-connection-test-" + hex.EncodeToString(b) + ".txt"
	t := in.target()
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := s.Rclone.Put(c, t, name, []byte("WPGenie log shipping: connection test. Safe to delete.\n")); err != nil {
		return TestResult{Step: "write", Error: "Couldn't write a test file to the bucket: " + plainErr(err)}, nil
	}
	if err := s.Rclone.DeleteFile(c, t, name); err != nil {
		return TestResult{Step: "delete", Error: "The test file was written but couldn't be deleted (" + name +
			"): archive retention needs to delete old logs. " + plainErr(err)}, nil
	}
	return TestResult{OK: true}, nil
}

// plainErr is the storage's own words, without rclone's wrapping.
func plainErr(err error) string {
	msg := offload.CleanError(err.Error())
	for _, p := range []string{"rclone rcat: ", "rclone deletefile: ", "rclone lsf: ", "rclone copy: ", "rclone delete: "} {
		msg = strings.TrimPrefix(msg, p)
	}
	return msg
}

func storageErr(what string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %s", ErrStorage, what, plainErr(err))
}

// ErrStorage is the storage refusing or failing (the API answers 502).
var ErrStorage = errors.New("the storage answered with an error")

// retentionBatch bounds one delete command's list.
const retentionBatch = 1000

// trimArchive deletes the archive's objects (every server's) from days
// before the retention period. Only keys of the archive's own shape are
// touched: anything else under the prefix stays.
func (s *Service) trimArchive(ctx context.Context, set Settings) (int, error) {
	if set.ArchiveRetentionDays <= 0 || set.Destination.check() != nil {
		return 0, nil
	}
	t := set.Destination.target()
	objs, err := s.Rclone.List(ctx, t)
	if err != nil {
		return 0, storageErr("listing the archive", err)
	}
	cutoff := s.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -set.ArchiveRetentionDays)
	var old []string
	for rel := range objs {
		if k, err := parseArchiveKey("", rel); err == nil && k.Day.Before(cutoff) {
			old = append(old, rel)
		}
	}
	slices.Sort(old)
	deleted := 0
	for chunk := range slices.Chunk(old, retentionBatch) {
		if _, err := s.Rclone.Delete(ctx, t, chunk); err != nil {
			return deleted, storageErr("deleting old archives", err)
		}
		deleted += len(chunk)
	}
	return deleted, nil
}
