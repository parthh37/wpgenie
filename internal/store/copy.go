package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

// TableCopy is one table copied by CopyToPostgres.
type TableCopy struct {
	Name string
	Rows int64
}

// CopyReport describes a finished CopyToPostgres.
type CopyReport struct {
	Version int // schema version of both databases
	Tables  []TableCopy
}

// CopyToPostgres copies the SQLite panel database at sqlitePath into the
// empty PostgreSQL database named by databaseURL: it migrates PostgreSQL
// to the SQLite database's schema version, copies every table (ids
// included) in one transaction, moves identity sequences past the copied
// ids (and past ids SQLite's AUTOINCREMENT already handed out), and checks
// the row counts before committing. The daemon starting on PostgreSQL
// then runs any newer migrations itself.
//
// The SQLite file is opened read-only and left as it was: switching back
// is removing database_url. The daemon must be stopped (the caller
// checks): the copy reads one consistent snapshot, and anything written
// to SQLite after it would stay behind.
func CopyToPostgres(ctx context.Context, sqlitePath, databaseURL string) (*CopyReport, error) {
	if _, err := os.Stat(sqlitePath); err != nil {
		return nil, err
	}
	src, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer src.Close()
	conn, err := src.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// One read transaction: every table from the same snapshot.
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)

	var version int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(v), 0) FROM schema_version`).Scan(&version); err != nil {
		return nil, fmt.Errorf("%s is not a WPGenie panel database: %w", sqlitePath, err)
	}
	if version < 1 || version > len(migrations) {
		return nil, fmt.Errorf("the SQLite database is at schema version %d; this WPGenie knows 1-%d", version, len(migrations))
	}
	tables, err := sqliteTables(ctx, conn)
	if err != nil {
		return nil, err
	}

	pg, err := openPostgres(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer pg.Close()
	if v, err := schemaVersion(ctx, pg); err != nil {
		return nil, err
	} else if v > version {
		return nil, fmt.Errorf("the PostgreSQL database is at schema version %d, newer than the SQLite database (%d): "+
			"start the daemon once on SQLite to upgrade it, then copy", v, version)
	}
	if err := migratePostgres(ctx, pg, version); err != nil {
		return nil, err
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Held until commit: a daemon started on this database meanwhile waits
	// before migrating it.
	if _, err := tx.ExecContext(ctx, migrationLock); err != nil {
		return nil, err
	}
	for _, t := range tables {
		types, err := pgColumns(ctx, tx, t.name)
		if err != nil {
			return nil, err
		}
		for _, c := range t.cols {
			if _, ok := types[c]; !ok {
				return nil, fmt.Errorf("PostgreSQL table %s has no column %s: schemas differ", t.name, c)
			}
		}
		if len(types) != len(t.cols) {
			return nil, fmt.Errorf("PostgreSQL table %s has %d columns, SQLite %d: schemas differ", t.name, len(types), len(t.cols))
		}
		t.types = types
		var full bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+quoteIdent(t.name)+`)`).Scan(&full); err != nil {
			return nil, err
		}
		if full {
			return nil, fmt.Errorf("the PostgreSQL database already has data (table %s): copy into an empty database", t.name)
		}
	}

	report := &CopyReport{Version: version}
	for _, t := range tables {
		n, err := copyTable(ctx, conn, tx, t)
		if err != nil {
			return nil, fmt.Errorf("copying %s: %w", t.name, err)
		}
		var srcN, dstN int64
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(t.name)).Scan(&srcN); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(t.name)).Scan(&dstN); err != nil {
			return nil, err
		}
		if n != srcN || dstN != srcN {
			return nil, fmt.Errorf("table %s: %d rows in SQLite, %d copied, %d in PostgreSQL", t.name, srcN, n, dstN)
		}
		report.Tables = append(report.Tables, TableCopy{t.name, n})
	}
	floors, err := sqliteSequences(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := resetIdentities(ctx, tx, floors); err != nil {
		return nil, err
	}
	return report, tx.Commit()
}

type copyTableSpec struct {
	name  string
	cols  []string
	deps  []string          // tables its foreign keys reference
	types map[string]string // PostgreSQL column -> data type
}

// sqliteTables lists the panel's tables, those referenced by foreign keys
// before those referencing them.
func sqliteTables(ctx context.Context, conn *sql.Conn) ([]*copyTableSpec, error) {
	var names []string
	rows, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name <> 'schema_version' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	specs := map[string]*copyTableSpec{}
	for _, n := range names {
		t := &copyTableSpec{name: n}
		if t.cols, err = queryStrings(ctx, conn, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, n); err != nil {
			return nil, err
		}
		if t.deps, err = queryStrings(ctx, conn, `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`, n); err != nil {
			return nil, err
		}
		specs[n] = t
	}
	// Topological order; a cycle (none today) falls back to name order and
	// the foreign keys report it.
	var out []*copyTableSpec
	done, visiting := map[string]bool{}, map[string]bool{}
	var visit func(n string)
	visit = func(n string) {
		t, ok := specs[n]
		if !ok || done[n] || visiting[n] {
			return
		}
		visiting[n] = true
		for _, d := range t.deps {
			if d != n {
				visit(d)
			}
		}
		done[n] = true
		out = append(out, t)
	}
	for _, n := range names {
		visit(n)
	}
	return out, nil
}

func queryStrings(ctx context.Context, conn *sql.Conn, q string, args ...any) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// sqliteSequences reads AUTOINCREMENT high-water marks: ids SQLite has
// handed out (to rows since deleted, too) and must not be handed out again.
func sqliteSequences(ctx context.Context, conn *sql.Conn) (map[string]int64, error) {
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'sqlite_sequence'`).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, err
		}
		out[name] = seq
	}
	return out, rows.Err()
}

func pgColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var c, t string
		if err := rows.Scan(&c, &t); err != nil {
			return nil, err
		}
		out[c] = t
	}
	if len(out) == 0 && rows.Err() == nil {
		return nil, fmt.Errorf("PostgreSQL has no table %s: schemas differ", table)
	}
	return out, rows.Err()
}

// copyTable copies a table in multi-row INSERTs (ids included), returning
// the number of rows.
func copyTable(ctx context.Context, src *sql.Conn, dst *sql.Tx, t *copyTableSpec) (int64, error) {
	quoted := make([]string, len(t.cols))
	for i, c := range t.cols {
		quoted[i] = quoteIdent(c)
	}
	rows, err := src.QueryContext(ctx, `SELECT `+strings.Join(quoted, ", ")+` FROM `+quoteIdent(t.name))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	// PostgreSQL takes at most 65535 parameters per statement.
	perBatch := max(1, min(500, 60000/len(t.cols)))
	var batch []any
	var n int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var b strings.Builder
		b.WriteString(`INSERT INTO ` + quoteIdent(t.name) + ` (` + strings.Join(quoted, ", ") + `) VALUES `)
		for r := 0; r < len(batch)/len(t.cols); r++ {
			if r > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('(')
			for c := range t.cols {
				if c > 0 {
					b.WriteString(", ")
				}
				b.WriteString("$" + strconv.Itoa(r*len(t.cols)+c+1))
			}
			b.WriteByte(')')
		}
		if _, err := dst.ExecContext(ctx, b.String(), batch...); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	vals := make([]any, len(t.cols))
	ptrs := make([]any, len(t.cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		for i, c := range t.cols {
			v, err := pgValue(vals[i], t.types[c])
			if err != nil {
				return 0, fmt.Errorf("row %d, column %s: %w", n+1, c, err)
			}
			batch = append(batch, v)
		}
		n++
		if len(batch) >= perBatch*len(t.cols) {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, flush()
}

// pgValue converts a value SQLite returned (its storage class may differ
// from the declared type: SQLite doesn't enforce types) to the PostgreSQL
// column's type, refusing anything lossy.
func pgValue(v any, pgType string) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch pgType {
	case "bigint":
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			if x == math.Trunc(x) && math.Abs(x) < 1<<63 {
				return int64(x), nil
			}
		case string:
			return strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		case []byte:
			return strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64)
		}
	case "double precision":
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case string:
			return strconv.ParseFloat(strings.TrimSpace(x), 64)
		}
	case "text":
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return strconv.FormatFloat(x, 'g', -1, 64), nil
		}
	case "bytea":
		switch x := v.(type) {
		case []byte:
			return slices.Clone(x), nil
		case string:
			return []byte(x), nil
		}
	default:
		return nil, fmt.Errorf("unexpected PostgreSQL type %s", pgType)
	}
	// The value itself stays out of the message: it may be a secret.
	return nil, fmt.Errorf("a %T value doesn't fit a %s column", v, pgType)
}
