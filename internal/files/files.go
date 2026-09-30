// Package files is the dashboard's file manager: browsing, editing,
// uploading, downloading and rearranging a site's files (its WordPress
// install, public/) from the panel, for people who'd rather not set up an
// SFTP client.
//
// The daemon runs as root, while the docroot is writable by the site, which
// can plant symlinks anywhere in it. Every operation therefore goes through
// an os.Root opened on the docroot: lookups that would resolve outside it
// fail, whatever links they cross, so no path given here (and no link the
// site made) reaches wp-config.php, another site or the host.
//
// Within the docroot, the file manager does only what the site user (uid
// 82, which PHP and SFTP logins run as) could do, and a little less: it
// adds, renames and removes entries only in directories the site owns,
// changes only files the site owns, and whatever it creates belongs to the
// site. Files WPGenie manages (root-owned drop-ins such as object-cache.php)
// stay read-only here, as they are to PHP.
//
// Files are replaced by writing a new file next to them and renaming it
// over the old one, never by writing into an existing file: PHP and Caddy
// see the old version or the new one, never half of it, and a hard link the
// site planted can't turn a save into a write to whatever it links to.
package files

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/store"
)

var (
	ErrInvalid    = errors.New("invalid input")
	ErrConflict   = errors.New("conflict")
	ErrPermission = errors.New("not allowed")
	ErrTooLarge   = errors.New("too large")
)

// Limits bound what one request may do. Zero values take the defaults.
type Limits struct {
	Edit         int64 // a text file opened in the editor (5 MB)
	Upload       int64 // one uploaded or saved file (1 GB)
	List         int   // entries listed per directory (5000)
	ExtractBytes int64 // what an archive or a copy may write (2 GB)
	ExtractFiles int   // entries an archive or a copy may create (50 000)
}

func (l Limits) withDefaults() Limits {
	def := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&l.Edit, 5<<20)
	def(&l.Upload, 1<<30)
	def(&l.ExtractBytes, 2<<30)
	if l.List <= 0 {
		l.List = 5000
	}
	if l.ExtractFiles <= 0 {
		l.ExtractFiles = 50_000
	}
	return l
}

// SiteUID is the site user (www-data in the PHP images).
const SiteUID = 82

type Service struct {
	// Root is a site's document root (config.SiteRoot).
	Root  func(siteID string) string
	Store *store.Store
	// UID and GID are the site user: who the file manager acts as, and who
	// owns what it creates. Chown is off when the daemon isn't root
	// (development on a workstation).
	UID, GID int
	Chown    bool
	Limits   Limits
}

// New is the file manager for a server: as the site user when the daemon
// runs as root, as the daemon's own user otherwise.
func New(st *store.Store, root func(string) string) *Service {
	s := &Service{Root: root, Store: st, UID: SiteUID, GID: SiteUID, Chown: true}
	if os.Geteuid() != 0 {
		s.UID, s.GID, s.Chown = os.Geteuid(), os.Getegid(), false
	}
	return s
}

func (s *Service) limits() Limits { return s.Limits.withDefaults() }

// Clean turns a path from the dashboard ("/wp-content/themes/", "", "a/../b")
// into a name relative to the docroot ("wp-content/themes", ".", "b"). ".."
// can't climb above the docroot: the path is cleaned as an absolute one.
func Clean(p string) (string, error) {
	if strings.ContainsRune(p, 0) || len(p) > 4096 {
		return "", fmt.Errorf("%w: path", ErrInvalid)
	}
	c := path.Clean("/" + p)
	if c == "/" {
		return ".", nil
	}
	for _, part := range strings.Split(c[1:], "/") {
		if len(part) > 255 {
			return "", fmt.Errorf("%w: a name is longer than 255 bytes", ErrInvalid)
		}
	}
	return c[1:], nil
}

// Display is a name relative to the docroot as the dashboard shows it.
func Display(name string) string {
	if name == "." {
		return "/"
	}
	return "/" + name
}

// entry is a name that must not be the docroot itself (it can't be
// deleted, renamed or overwritten).
func entry(p string) (string, error) {
	name, err := Clean(p)
	if err == nil && name == "." {
		err = fmt.Errorf("%w: that's the site's top folder", ErrInvalid)
	}
	return name, err
}

// open opens a site's docroot. write: the operation changes something,
// refused while the site's logins are off (suspended, or moving between
// servers: the copy mustn't miss a change), as SFTP is.
func (s *Service) open(ctx context.Context, id string, write bool) (*os.Root, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	switch st.Status {
	case store.StatusImporting, store.StatusMoved:
		return nil, fmt.Errorf("%w: the site's files are moving between servers", ErrConflict)
	}
	if write {
		if st.Status != store.StatusActive {
			return nil, fmt.Errorf("%w: the site is %s", ErrConflict, st.Status)
		}
		off, err := s.Store.SuspendedSiteIDs(ctx)
		if err != nil {
			return nil, err
		}
		if off[id] {
			return nil, fmt.Errorf("%w: the site's files are locked while it moves to another server", ErrConflict)
		}
	}
	root, err := os.OpenRoot(s.Root(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: the site has no files on this server", store.ErrNotFound)
	}
	return root, err
}

// owned reports whether the site user owns a file.
func (s *Service) owned(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == s.UID
}

// writableDir is a directory the site user may add entries to and remove
// entries from.
func (s *Service) writableDir(fi fs.FileInfo) bool {
	return fi.IsDir() && s.owned(fi) && fi.Mode().Perm()&0o300 == 0o300
}

// checkDir fails unless dir is a directory the site may change.
func (s *Service) checkDir(root *os.Root, dir string) error {
	fi, err := root.Lstat(dir)
	if err != nil {
		return notFound(err, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s isn't a folder", ErrConflict, Display(dir))
	}
	if !s.writableDir(fi) {
		return fmt.Errorf("%w: the site can't change %s (%s)", ErrPermission, Display(dir), whose(s, fi))
	}
	return nil
}

// checkEntry fails unless the site may rename or remove name: it owns it,
// and the directory it's in.
func (s *Service) checkEntry(root *os.Root, name string) (fs.FileInfo, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, notFound(err, name)
	}
	if !s.owned(fi) {
		return nil, fmt.Errorf("%w: %s is managed by WPGenie, not the site", ErrPermission, Display(name))
	}
	return fi, s.checkDir(root, path.Dir(name))
}

func whose(s *Service, fi fs.FileInfo) string {
	if !s.owned(fi) {
		return "it isn't the site's"
	}
	return "permissions " + modeString(fi.Mode())
}

// notFound makes "no such file" a store.ErrNotFound (404) naming the path
// relative to the docroot. os.Root's errors name the path they were given,
// never the host's.
func notFound(err error, name string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", store.ErrNotFound, Display(name))
	}
	return err
}

func modeString(m fs.FileMode) string { return fmt.Sprintf("%04o", m.Perm()) }

// Entry is a file or directory in a listing.
type Entry struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"` // dir, file, link or other
	Size     int64     `json:"size"`
	Mode     string    `json:"mode"` // "0644"
	Modified time.Time `json:"modified"`
	// Owned: the site owns it, so it may be renamed, deleted, re-moded
	// (and, a file with the owner's write bit, edited).
	Owned  bool   `json:"owned"`
	Target string `json:"target,omitempty"` // a link's target
	// Version: a file just written, for the editor's next save.
	Version string `json:"version,omitempty"`
}

// Listing is a directory's contents: folders first, then by name.
type Listing struct {
	Path string `json:"path"`
	// Writable: the site may add entries here.
	Writable  bool    `json:"writable"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated,omitempty"`
}

func kind(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "dir"
	case m.IsRegular():
		return "file"
	case m&fs.ModeSymlink != 0:
		return "link"
	}
	return "other"
}

func (s *Service) entryOf(root *os.Root, dir string, fi fs.FileInfo) Entry {
	e := Entry{Name: fi.Name(), Type: kind(fi.Mode()), Size: fi.Size(), Mode: modeString(fi.Mode()),
		Modified: fi.ModTime().UTC(), Owned: s.owned(fi)}
	if e.Type == "link" {
		e.Target, _ = root.Readlink(path.Join(dir, fi.Name()))
	}
	if e.Type == "dir" {
		e.Size = 0
	}
	return e
}

// List lists a directory.
func (s *Service) List(ctx context.Context, id, p string) (*Listing, error) {
	dir, err := Clean(p)
	if err != nil {
		return nil, err
	}
	root, err := s.open(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	d, err := root.Open(dir)
	if err != nil {
		return nil, notFound(err, dir)
	}
	defer d.Close()
	fi, err := d.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s isn't a folder", ErrInvalid, Display(dir))
	}
	lim := s.limits().List
	des, err := d.ReadDir(lim + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := &Listing{Path: Display(dir), Writable: s.writableDir(fi), Entries: make([]Entry, 0, len(des))}
	if len(des) > lim {
		des, out.Truncated = des[:lim], true
	}
	for _, de := range des {
		info, err := de.Info() // lstat
		if err != nil {
			continue // removed meanwhile: the site is live
		}
		out.Entries = append(out.Entries, s.entryOf(root, dir, info))
	}
	slices.SortFunc(out.Entries, func(a, b Entry) int {
		if (a.Type == "dir") != (b.Type == "dir") {
			if a.Type == "dir" {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

// Version identifies a file's content as it was read: the editor sends it
// back with a save, which fails if the file changed meanwhile. A string:
// nanoseconds don't survive a JavaScript number.
func Version(fi fs.FileInfo) string {
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return fmt.Sprintf("%x-%x-%x", ino, fi.ModTime().UnixNano(), fi.Size())
}

// Text is a file opened in the editor.
type Text struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Version  string `json:"version"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
	Writable bool   `json:"writable"`
}

// openRegular opens a regular file, not a link to one (os.Root would
// follow a link inside the docroot), and makes sure what was opened is
// what was checked.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, nil, notFound(err, name)
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %s isn't a file", ErrInvalid, Display(name))
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, notFound(err, name)
	}
	got, err := f.Stat()
	if err != nil || !os.SameFile(fi, got) {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s changed while it was opened", ErrConflict, Display(name))
	}
	return f, got, nil
}

// Read opens a text file for the editor. Binary files (and text that
// isn't UTF-8) are refused: a browser's text box would silently replace
// the bytes it can't show, and saving would corrupt the file.
func (s *Service) Read(ctx context.Context, id, p string) (*Text, error) {
	name, err := entry(p)
	if err != nil {
		return nil, err
	}
	root, err := s.open(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, fi, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lim := s.limits().Edit
	if fi.Size() > lim {
		return nil, fmt.Errorf("%w: %s is %d MB; files over %d MB can't be edited here (download it instead)",
			ErrTooLarge, Display(name), fi.Size()>>20, lim>>20)
	}
	data, err := io.ReadAll(io.LimitReader(f, lim+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > lim {
		return nil, fmt.Errorf("%w: %s grew past %d MB while it was read", ErrTooLarge, Display(name), lim>>20)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: %s isn't a UTF-8 text file; download it instead", ErrInvalid, Display(name))
	}
	return &Text{Path: Display(name), Content: string(data), Version: Version(fi), Size: fi.Size(),
		Mode: modeString(fi.Mode()), Writable: s.owned(fi) && fi.Mode().Perm()&0o200 != 0}, nil
}

// WriteOptions says what a write may replace.
type WriteOptions struct {
	// Overwrite: replace an existing file (never a folder or a link).
	Overwrite bool
	// Version: the file must still be as the editor read it ("": no check).
	// Implies Overwrite.
	Version string
}

// Write creates or replaces a file with what r holds (an upload, a save
// from the editor, a new empty file).
func (s *Service) Write(ctx context.Context, id, p string, r io.Reader, o WriteOptions) (*Entry, error) {
	name, err := entry(p)
	if err != nil {
		return nil, err
	}
	root, err := s.open(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := s.checkDir(root, path.Dir(name)); err != nil {
		return nil, err
	}
	mode := fs.FileMode(0o644)
	cur, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if o.Version != "" {
			return nil, fmt.Errorf("%w: %s was deleted since you opened it", ErrConflict, Display(name))
		}
	case err != nil:
		return nil, err
	case !cur.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s is a %s", ErrConflict, Display(name), map[string]string{"dir": "folder", "link": "link"}[kind(cur.Mode())])
	case !o.Overwrite && o.Version == "":
		return nil, fmt.Errorf("%w: %s already exists", ErrConflict, Display(name))
	case o.Version != "" && o.Version != Version(cur):
		return nil, fmt.Errorf("%w: %s changed since you opened it", ErrConflict, Display(name))
	case !s.owned(cur) || cur.Mode().Perm()&0o200 == 0:
		return nil, fmt.Errorf("%w: %s is read-only (%s)", ErrPermission, Display(name), whose(s, cur))
	default:
		mode = cur.Mode().Perm()
	}
	fi, err := s.replace(root, name, r, mode, s.limits().Upload)
	if err != nil {
		return nil, err
	}
	e := s.entryOf(root, path.Dir(name), fi)
	e.Version = Version(fi)
	return &e, nil
}

// replace writes r to a temporary file in name's directory and renames it
// over name (see the package comment). The temporary name is a dotfile,
// which Caddy never serves.
func (s *Service) replace(root *os.Root, name string, r io.Reader, mode fs.FileMode, limit int64) (fs.FileInfo, error) {
	var rnd [8]byte
	rand.Read(rnd[:])
	tmp := path.Join(path.Dir(name), ".wpgenie-upload-"+hex.EncodeToString(rnd[:]))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	done := false
	defer func() {
		if !done {
			f.Close()
			root.Remove(tmp)
		}
	}()
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("%w: files can be at most %d MB", ErrTooLarge, limit>>20)
	}
	if err := s.own(f); err != nil {
		return nil, err
	}
	if err := f.Chmod(mode); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := root.Rename(tmp, name); err != nil {
		root.Remove(tmp)
		done = true
		return nil, err
	}
	done = true
	return root.Lstat(name)
}

// own gives an open file (or directory) to the site user; through the
// handle, so it's this file whatever the path points at by now.
func (s *Service) own(f *os.File) error {
	if !s.Chown {
		return nil
	}
	return f.Chown(s.UID, s.GID)
}

// mkdir creates one directory owned by the site.
func (s *Service) mkdir(root *os.Root, name string) error {
	if err := root.Mkdir(name, 0o755); err != nil {
		return err
	}
	d, err := root.Open(name)
	if err != nil {
		return err
	}
	defer d.Close()
	return s.own(d)
}

// Mkdir creates a directory.
func (s *Service) Mkdir(ctx context.Context, id, p string) error {
	name, err := entry(p)
	if err != nil {
		return err
	}
	root, err := s.open(ctx, id, true)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := s.checkDir(root, path.Dir(name)); err != nil {
		return err
	}
	if err := s.mkdir(root, name); errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s already exists", ErrConflict, Display(name))
	} else if err != nil {
		return err
	}
	return nil
}

// Move renames or moves a file or directory; it never replaces anything.
func (s *Service) Move(ctx context.Context, id, from, to string) error {
	src, err := entry(from)
	if err != nil {
		return err
	}
	dst, err := entry(to)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if strings.HasPrefix(dst, src+"/") {
		return fmt.Errorf("%w: a folder can't be moved into itself", ErrInvalid)
	}
	root, err := s.open(ctx, id, true)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := s.checkEntry(root, src); err != nil {
		return err
	}
	if err := s.checkDir(root, path.Dir(dst)); err != nil {
		return err
	}
	if _, err := root.Lstat(dst); err == nil {
		return fmt.Errorf("%w: %s already exists", ErrConflict, Display(dst))
	}
	return root.Rename(src, dst)
}

// Delete removes a file, a link or a directory with everything in it.
func (s *Service) Delete(ctx context.Context, id, p string) error {
	name, err := entry(p)
	if err != nil {
		return err
	}
	root, err := s.open(ctx, id, true)
	if err != nil {
		return err
	}
	defer root.Close()
	fi, err := s.checkEntry(root, name)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return root.RemoveAll(name)
	}
	return root.Remove(name)
}

// Chmod changes a file's or directory's permission bits (no setuid, setgid
// or sticky bits).
func (s *Service) Chmod(ctx context.Context, id, p string, mode fs.FileMode) error {
	if mode&^0o777 != 0 {
		return fmt.Errorf("%w: permissions are 3 octal digits, like 644", ErrInvalid)
	}
	name, err := Clean(p)
	if err != nil {
		return err
	}
	root, err := s.open(ctx, id, true)
	if err != nil {
		return err
	}
	defer root.Close()
	fi, err := root.Lstat(name)
	if err != nil {
		return notFound(err, name)
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: only files and folders have permissions", ErrInvalid)
	}
	if !s.owned(fi) {
		return fmt.Errorf("%w: %s is managed by WPGenie, not the site", ErrPermission, Display(name))
	}
	if fi.IsDir() && mode&0o700 != 0o700 {
		return fmt.Errorf("%w: the site must keep full access to its own folders (7xx)", ErrInvalid)
	}
	// Through a handle checked to be what was looked at: chmod on a path
	// would follow a link that replaced it meanwhile.
	f, err := root.Open(name)
	if err != nil {
		return notFound(err, name)
	}
	defer f.Close()
	if got, err := f.Stat(); err != nil || !os.SameFile(fi, got) {
		return fmt.Errorf("%w: %s changed meanwhile", ErrConflict, Display(name))
	}
	return f.Chmod(mode)
}

// Download opens a file to send it. The caller closes it.
func (s *Service) Download(ctx context.Context, id, p string) (*os.File, fs.FileInfo, error) {
	name, err := entry(p)
	if err != nil {
		return nil, nil, err
	}
	root, err := s.open(ctx, id, false)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close() // the file stays open
	return openRegular(root, name)
}

// IsDir reports whether p is a directory (downloaded as a zip archive).
func (s *Service) IsDir(ctx context.Context, id, p string) (bool, error) {
	name, err := Clean(p)
	if err != nil {
		return false, err
	}
	root, err := s.open(ctx, id, false)
	if err != nil {
		return false, err
	}
	defer root.Close()
	fi, err := root.Lstat(name)
	if err != nil {
		return false, notFound(err, name)
	}
	return fi.IsDir(), nil
}

// Zip writes a directory, everything in it, as a zip archive. Links aren't
// followed (or included): only what's really in the directory.
func (s *Service) Zip(ctx context.Context, id, p string, w io.Writer) error {
	name, err := Clean(p)
	if err != nil {
		return err
	}
	root, err := s.open(ctx, id, false)
	if err != nil {
		return err
	}
	defer root.Close()
	if fi, err := root.Lstat(name); err != nil {
		return notFound(err, name)
	} else if !fi.IsDir() {
		return fmt.Errorf("%w: %s isn't a folder", ErrInvalid, Display(name))
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer sub.Close()
	zw := zip.NewWriter(w)
	err = fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if p == "." {
				return err
			}
			return nil // gone or unreadable meanwhile: the site is live
		}
		if p == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		switch {
		case d.IsDir():
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			hdr.Name = p + "/"
			_, err = zw.CreateHeader(hdr)
			return err
		case d.Type().IsRegular():
			return zipFile(zw, sub, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

func zipFile(zw *zip.Writer, root *os.Root, name string) error {
	f, fi, err := openRegular(root, name)
	if err != nil {
		return nil // replaced or gone meanwhile: skipped
	}
	defer f.Close()
	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	hdr.Name, hdr.Method = name, zip.Deflate
	out, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, f)
	return err
}

// budget counts what an extraction or a copy has written against the
// limits, so an archive that lies about its sizes (a zip bomb) or a huge
// folder can't fill the disk.
type budget struct {
	bytes int64
	files int
}

func (b *budget) file(lim Limits) error {
	if b.files++; b.files > lim.ExtractFiles {
		return fmt.Errorf("%w: more than %d files", ErrTooLarge, lim.ExtractFiles)
	}
	return nil
}

func (b *budget) left(lim Limits) int64 { return lim.ExtractBytes - b.bytes }

// mkdirs creates dir and its missing parents below base, owned by the
// site. Existing ones must be directories the site can write to.
func (s *Service) mkdirs(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		fi, err := root.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := s.mkdir(root, cur); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("%w: %s exists and isn't a folder", ErrConflict, Display(cur))
		case !s.writableDir(fi):
			return fmt.Errorf("%w: the site can't change %s (%s)", ErrPermission, Display(cur), whose(s, fi))
		}
	}
	return nil
}

// Extracted is what an extraction did.
type Extracted struct {
	Files   int `json:"files"`
	Folders int `json:"folders"`
	// Skipped: links and other special entries, never extracted.
	Skipped int `json:"skipped"`
}

// Extract unpacks a zip archive into a directory (created if missing).
// Without overwrite, it refuses before writing anything if a file in the
// archive already exists.
func (s *Service) Extract(ctx context.Context, id, archive, to string, overwrite bool) (*Extracted, error) {
	name, err := entry(archive)
	if err != nil {
		return nil, err
	}
	dest, err := Clean(to)
	if err != nil {
		return nil, err
	}
	root, err := s.open(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, fi, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		return nil, fmt.Errorf("%w: %s isn't a zip archive", ErrInvalid, Display(name))
	}
	lim := s.limits()

	// Check the whole archive first: nothing is written from one with an
	// unsafe or clashing entry, or one that says it's too big.
	type item struct {
		f    *zip.File
		name string
		dir  bool
	}
	var items []item
	var declared uint64
	out := &Extracted{}
	for _, zf := range zr.File {
		n := strings.TrimSuffix(zf.Name, "/")
		// Relative, clean, forward slashes: no "../", no absolute paths, no
		// Windows drive letters or separators.
		if !fs.ValidPath(n) || n == "." || strings.Contains(n, `\`) || strings.Contains(n, ":") {
			return nil, fmt.Errorf("%w: the archive has an unsafe path: %q", ErrInvalid, zf.Name)
		}
		m := zf.Mode()
		switch {
		case m.IsDir():
			items = append(items, item{zf, path.Join(dest, n), true})
		case m.IsRegular():
			items = append(items, item{zf, path.Join(dest, n), false})
			declared += zf.UncompressedSize64
		default:
			out.Skipped++
			continue
		}
		if len(items) > lim.ExtractFiles {
			return nil, fmt.Errorf("%w: the archive has more than %d entries", ErrTooLarge, lim.ExtractFiles)
		}
	}
	if declared > uint64(lim.ExtractBytes) {
		return nil, fmt.Errorf("%w: the archive holds %d MB; at most %d MB can be extracted at once",
			ErrTooLarge, declared>>20, lim.ExtractBytes>>20)
	}
	var clash []string
	for _, it := range items {
		if it.dir {
			continue
		}
		cur, err := root.Lstat(it.name)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		case err != nil:
			return nil, notFound(err, it.name)
		case !cur.Mode().IsRegular():
			return nil, fmt.Errorf("%w: %s exists and isn't a file", ErrConflict, Display(it.name))
		case !s.owned(cur):
			return nil, fmt.Errorf("%w: %s is managed by WPGenie, not the site", ErrPermission, Display(it.name))
		default:
			clash = append(clash, Display(it.name))
		}
	}
	if len(clash) > 0 && !overwrite {
		more := ""
		if len(clash) > 1 {
			more = fmt.Sprintf(" and %d more", len(clash)-1)
		}
		return nil, fmt.Errorf("%w: %s%s already exist (overwrite to replace them)", ErrConflict, clash[0], more)
	}

	var b budget
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if it.dir {
			if err := s.mkdirs(root, it.name); err != nil {
				return out, err
			}
			out.Folders++
			continue
		}
		if err := s.mkdirs(root, path.Dir(it.name)); err != nil {
			return out, err
		}
		if err := b.file(lim); err != nil {
			return out, err
		}
		mode := fs.FileMode(0o644)
		if it.f.Mode()&0o100 != 0 {
			mode = 0o755
		}
		rc, err := it.f.Open()
		if err != nil {
			return out, fmt.Errorf("%w: %s in the archive: %v", ErrInvalid, it.f.Name, err)
		}
		fi, err := s.replace(root, it.name, rc, mode, b.left(lim))
		rc.Close()
		if errors.Is(err, ErrTooLarge) {
			return out, fmt.Errorf("%w: the archive extracts to more than %d MB", ErrTooLarge, lim.ExtractBytes>>20)
		} else if err != nil {
			return out, err
		}
		b.bytes += fi.Size()
		out.Files++
	}
	return out, nil
}

// Copy copies a file, or a directory and everything in it, to a new name.
// Links aren't copied; nothing existing is replaced.
func (s *Service) Copy(ctx context.Context, id, from, to string) (*Extracted, error) {
	src, err := entry(from)
	if err != nil {
		return nil, err
	}
	dst, err := entry(to)
	if err != nil {
		return nil, err
	}
	if dst == src || strings.HasPrefix(dst, src+"/") {
		return nil, fmt.Errorf("%w: a folder can't be copied into itself", ErrInvalid)
	}
	root, err := s.open(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	fi, err := root.Lstat(src)
	if err != nil {
		return nil, notFound(err, src)
	}
	if err := s.checkDir(root, path.Dir(dst)); err != nil {
		return nil, err
	}
	if _, err := root.Lstat(dst); err == nil {
		return nil, fmt.Errorf("%w: %s already exists", ErrConflict, Display(dst))
	}
	lim := s.limits()
	out := &Extracted{}
	var b budget
	copyFile := func(from, to string) error {
		if err := b.file(lim); err != nil {
			return err
		}
		f, fi, err := openRegular(root, from)
		if err != nil {
			return err
		}
		defer f.Close()
		got, err := s.replace(root, to, f, fi.Mode().Perm()|0o600, b.left(lim))
		if errors.Is(err, ErrTooLarge) {
			return fmt.Errorf("%w: more than %d MB to copy", ErrTooLarge, lim.ExtractBytes>>20)
		} else if err != nil {
			return err
		}
		b.bytes += got.Size()
		out.Files++
		return nil
	}
	switch {
	case fi.Mode().IsRegular():
		return out, copyFile(src, dst)
	case !fi.IsDir():
		return nil, fmt.Errorf("%w: only files and folders can be copied", ErrInvalid)
	}
	sub, err := root.OpenRoot(src)
	if err != nil {
		return nil, err
	}
	defer sub.Close()
	err = fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		target := path.Join(dst, p)
		switch {
		case d.IsDir():
			if err := s.mkdir(root, target); err != nil {
				return err
			}
			out.Folders++
		case d.Type().IsRegular():
			return copyFile(path.Join(src, p), target)
		default:
			out.Skipped++
		}
		return nil
	})
	return out, err
}
