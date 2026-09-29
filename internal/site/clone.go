package site

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
)

// Copying WordPress installs and databases between sites and backups:
// the machinery under restores, staging clones and pushes, and primary
// domain changes.
//
// Files are always written by the site user inside the site's container
// (see restoreScript for why never as root), in two phases: the archive is
// extracted into a temporary directory in the docroot, and only once the
// process producing it has exited cleanly is the live install swapped for
// it. A source that dies half-way can't leave a half-replaced site behind,
// even if tar took the truncated stream for a complete one.

const restoreTmp = ".wpgenie-restore"

// extractScript: $1 docroot, $2 leading path components to strip.
const extractScript = `set -e
cd "$1"
rm -rf ` + restoreTmp + `
mkdir ` + restoreTmp + `
tar -xf - --no-same-owner -C ` + restoreTmp + ` --strip-components="$2"`

// commitScript swaps the extracted install in: $1 docroot, $2 the
// wp-content entries to keep from the current install (space-separated,
// e.g. "uploads cache" to replace only code). Everything else is replaced,
// so files that only exist in the current install go too. .maintenance
// (a push in progress) belongs to the current install either way.
const commitScript = `set -e
cd "$1"
keep=" $2 "
tmp=` + restoreTmp + `
[ -d "$tmp/wp-includes" ] || { echo "the archive is not a WordPress install (no wp-includes)" >&2; rm -rf "$tmp"; exit 1; }
find . -mindepth 1 -maxdepth 1 ! -name wp-content ! -name "$tmp" ! -name .maintenance -exec rm -rf {} +
mkdir -p wp-content
for f in wp-content/* wp-content/.[!.]* wp-content/..?*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  case "$keep" in *" ${f#wp-content/} "*) continue ;; esac
  rm -rf "$f"
done
for f in "$tmp"/* "$tmp"/.[!.]* "$tmp"/..?*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  case "${f##*/}" in wp-content|.maintenance) continue ;; esac
  mv "$f" .
done
if [ -d "$tmp/wp-content" ]; then
  for f in "$tmp"/wp-content/* "$tmp"/wp-content/.[!.]* "$tmp"/wp-content/..?*; do
    [ -e "$f" ] || [ -L "$f" ] || continue
    case "$keep" in *" ${f##*/} "*) continue ;; esac
    mv "$f" wp-content/
  done
fi
rm -rf "$tmp"`

// What an install copy leaves out: caches are rebuilt, .maintenance is
// state of the moment, the restore directory is ours.
var copyExcludes = []string{"wp-content/cache", "wp-content/upgrade", ".maintenance", restoreTmp}

// keepCode keeps the target's uploads (a code-only replace); keepNone
// replaces uploads too. Caches are always kept (and purged afterwards).
var (
	keepCode = []string{"uploads", "cache"}
	keepNone = []string{"cache"}
)

// installTar archives a site's install from inside its container.
func installTar(docroot string, uploads bool) []string {
	args := []string{"tar", "-cf", "-"}
	for _, e := range copyExcludes {
		args = append(args, "--exclude=./"+e)
	}
	if !uploads {
		args = append(args, "--exclude=./wp-content/uploads")
	}
	return append(args, "-C", docroot, ".")
}

// pipe runs produce and consume concurrently, connected by a pipe. The
// producer's failure is reported first: it is the cause when the consumer
// then chokes on a cut stream.
func pipe(produce func(io.Writer) error, consume func(io.Reader) error) error {
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := produce(pw)
		pw.CloseWithError(err)
		errc <- err
	}()
	cerr := consume(pr)
	// A consumer that gave up early must not leave the producer blocked.
	pr.CloseWithError(errors.Join(cerr, errors.New("consumer finished")))
	perr := <-errc
	switch {
	case perr != nil && cerr != nil:
		return fmt.Errorf("%w (then: %v)", perr, cerr)
	case perr != nil:
		return perr
	}
	return cerr
}

// replaceInstall replaces a site's WordPress install with the tar archive
// produce writes, keeping the listed wp-content entries.
func (s *Service) replaceInstall(ctx context.Context, siteID string, strip int, keep []string,
	produce func(io.Writer) error) error {
	root := s.Cfg.SiteRoot(siteID)
	err := pipe(produce, func(r io.Reader) error {
		return s.Runtime.Exec(ctx, siteID, r, nil, "sh", "-c", extractScript, "sh", root, strconv.Itoa(strip))
	})
	if err != nil {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		s.Runtime.Exec(c, siteID, nil, nil, "rm", "-rf", filepath.Join(root, restoreTmp))
		return fmt.Errorf("copying files: %w", err)
	}
	if err := s.Runtime.Exec(ctx, siteID, nil, nil, "sh", "-c", commitScript, "sh", root, strings.Join(keep, " ")); err != nil {
		return fmt.Errorf("replacing files: %w", err)
	}
	// Whatever the archive carried, WPGenie's own wrappers are root-owned
	// and follow the target's settings.
	return s.rewriteManagedFiles(ctx, siteID)
}

// copyInstall copies one site's install over another's.
func (s *Service) copyInstall(ctx context.Context, from, to string, uploads bool) error {
	keep := keepCode
	if uploads {
		keep = keepNone
	}
	return s.replaceInstall(ctx, to, 0, keep, func(w io.Writer) error {
		return s.Runtime.Exec(ctx, from, nil, w, installTar(s.Cfg.SiteRoot(from), uploads)...)
	})
}

// copyDB copies tables (all when empty) from one site database to another,
// replacing them there.
func (s *Service) copyDB(ctx context.Context, fromDB, toDB string, tables ...string) error {
	return pipe(func(w io.Writer) error { return s.Dumper.Dump(ctx, fromDB, w, tables...) },
		func(r io.Reader) error { return s.Dumper.Restore(ctx, toDB, r) })
}

// urlPattern matches links to domain as WordPress stores them: plain
// (//example.com) and JSON-escaped (\/\/example.com, page builders), but
// not a longer hostname (//example.com.au, //example.company).
func urlPattern(domain string) string {
	return `(//|\\/\\/)` + regexp.QuoteMeta(domain) + `(?![A-Za-z0-9.-])`
}

// searchReplaceArgs is WP-CLI's search-replace of links to from with
// links to to, in the given tables (all tables of the database when
// none). Serialised PHP data is handled by WP-CLI; GUIDs are left alone,
// as WordPress requires. With export the result goes to stdout as SQL and
// the database is not changed.
func searchReplaceArgs(from, to string, tables []string, export bool) []string {
	args := []string{"search-replace", urlPattern(from), "${1}" + to}
	args = append(args, tables...)
	args = append(args, "--regex", "--skip-columns=guid", "--no-report")
	if len(tables) == 0 {
		args = append(args, "--all-tables")
	}
	if export {
		args = append(args, "--export")
	}
	return runtime.WPArgs(args...)
}

// searchReplace rewrites a site's links from one domain to another in its
// own database.
func (s *Service) searchReplace(ctx context.Context, siteID, from, to string) error {
	if from == to {
		return nil
	}
	var out bytes.Buffer
	if err := s.Runtime.Exec(ctx, siteID, nil, &out, searchReplaceArgs(from, to, nil, false)...); err != nil {
		return fmt.Errorf("replacing %s with %s in the database: %w", from, to, err)
	}
	return nil
}

var prefixRe = regexp.MustCompile(`(?m)^\$table_prefix\s*=\s*'([A-Za-z0-9_]{1,32})';`)

// tablePrefix reads a site's table prefix from the wp-config.php WPGenie
// generated (root-owned: the site can't have changed it).
func (s *Service) tablePrefix(id string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Cfg.SiteDir(id), "wp-config.php"))
	if err != nil {
		return "", err
	}
	m := prefixRe.FindSubmatch(b)
	if m == nil {
		return "", errors.New("no $table_prefix in wp-config.php")
	}
	return string(m[1]), nil
}
