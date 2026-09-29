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
	dumpScript = `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mariadb-dump --user=root --single-transaction ` +
		`--quick --routines --triggers --hex-blob --no-tablespaces --default-character-set=utf8mb4 "$1"`
	restoreScript = `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mariadb --user=root --default-character-set=utf8mb4 "$1"`
)

// Dump writes a consistent SQL dump of one site database to w
// (--single-transaction: no locks, the site keeps serving).
func (m *MariaDB) Dump(ctx context.Context, db string, w io.Writer) error {
	if !siteDBRe.MatchString(db) {
		return fmt.Errorf("refusing to dump database %q", db)
	}
	return m.stream(ctx, strings.NewReader(m.Password+"\n"), w,
		"exec", "-i", m.Container, "sh", "-c", dumpScript, "sh", db)
}

// Restore loads an SQL dump made by Dump into db.
func (m *MariaDB) Restore(ctx context.Context, db string, r io.Reader) error {
	if !siteDBRe.MatchString(db) {
		return fmt.Errorf("refusing to restore into database %q", db)
	}
	return m.stream(ctx, io.MultiReader(strings.NewReader(m.Password+"\n"), r), io.Discard,
		"exec", "-i", m.Container, "sh", "-c", restoreScript, "sh", db)
}
