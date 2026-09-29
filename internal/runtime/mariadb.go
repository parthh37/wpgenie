package runtime

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// MariaDB dumps and restores site databases with the client tools inside
// the MariaDB container: site images carry no MySQL client, and the root
// password travels on stdin, never in a command line or environment that
// `ps` or `docker inspect` would show.
type MariaDB struct {
	Docker
	Container string
	Password  string
}

var siteDBRe = regexp.MustCompile(`^wp_[a-z0-9]{1,29}$`)

// The first stdin line is the password: `read` consumes exactly that line
// (the shell reads a pipe byte by byte), leaving the rest for the client.
const (
	dumpScript = `IFS= read -r MYSQL_PWD; export MYSQL_PWD; db=$1; shift; exec mariadb-dump --user=root ` +
		`--single-transaction --quick --routines --triggers --hex-blob --no-tablespaces --default-character-set=utf8mb4 "$db" "$@"`
	// --sandbox: no client-side commands (\! shell, source, pager) whatever
	// the SQL contains. The second form logs in as a given user: $2.
	restoreScript = `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mariadb --sandbox --user=root ` +
		`--default-character-set=utf8mb4 "$1"`
	restoreAsScript = `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mariadb --sandbox --user="$2" ` +
		`--default-character-set=utf8mb4 "$1"`
)

var accountRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// tableRe: table names come from plugins; only characters that can't be
// taken for options or break out of an identifier.
var tableRe = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)

// Dump writes a consistent SQL dump of one site database (only the given
// tables, when any) to w (--single-transaction: no locks, the site keeps
// serving). Each table is dropped and recreated when the dump is loaded.
func (m *MariaDB) Dump(ctx context.Context, db string, w io.Writer, tables ...string) error {
	if !siteDBRe.MatchString(db) {
		return fmt.Errorf("refusing to dump database %q", db)
	}
	args := []string{"exec", "-i", m.Container, "sh", "-c", dumpScript, "sh", db}
	for _, t := range tables {
		// Table names start after the database name; none can look like an
		// option (tableRe has no "-").
		if !tableRe.MatchString(t) {
			return fmt.Errorf("refusing to dump table %q", t)
		}
		args = append(args, t)
	}
	return m.stream(ctx, strings.NewReader(m.Password+"\n"), w, args...)
}

// Restore loads an SQL dump made by Dump into db.
func (m *MariaDB) Restore(ctx context.Context, db string, r io.Reader) error {
	if !siteDBRe.MatchString(db) {
		return fmt.Errorf("refusing to restore into database %q", db)
	}
	return m.stream(ctx, io.MultiReader(strings.NewReader(m.Password+"\n"), r), io.Discard,
		"exec", "-i", m.Container, "sh", "-c", restoreScript, "sh", db)
}

// RestoreAs loads SQL into db logged in as user (password on stdin), not
// root: for SQL a site produced (a staging push), which may then do no
// more than that account may, i.e. touch that one database.
func (m *MariaDB) RestoreAs(ctx context.Context, db, user, password string, r io.Reader) error {
	if !siteDBRe.MatchString(db) || !accountRe.MatchString(user) || strings.ContainsAny(password, "\n\r") {
		return fmt.Errorf("refusing to restore into database %q as %q", db, user)
	}
	return m.stream(ctx, io.MultiReader(strings.NewReader(password+"\n"), r), io.Discard,
		"exec", "-i", m.Container, "sh", "-c", restoreAsScript, "sh", db, user)
}
