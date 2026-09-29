package store

// The dialect layer. Store queries are written once, in the SQL both
// SQLite and PostgreSQL understand, with `?` placeholders. DB and Tx wrap
// database/sql and, per statement (cached):
//
//   - reject constructs that only one of the two runs (INSERT OR REPLACE,
//     rowid, strftime, LastInsertId, ...), on BOTH backends: the default
//     SQLite test run then fails on code that would break on Postgres;
//   - on PostgreSQL, rebind `?` to `$n` (never inside string literals,
//     quoted identifiers or comments), apply a few mechanical rewrites
//     (IFNULL, scalar MAX/MIN, INSERT OR IGNORE, LIKE, TRUE/FALSE, CAST
//     types, bare column names in ON CONFLICT DO UPDATE), and turn bool
//     arguments into the 0/1 integers the columns hold;
//   - on SQLite, drop a trailing row-locking clause (FOR UPDATE / FOR
//     SHARE [SKIP LOCKED|NOWAIT]): its single connection already runs one
//     transaction at a time, which is the lock.
//
// Everything else reaches the driver unchanged, so SQLite runs exactly the
// statements it ran before this layer existed.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the panel database: a *sql.DB plus the dialect it speaks.
type DB struct {
	sql      *sql.DB
	postgres bool

	cache  sync.Map // query -> rewritten
	cacheN atomic.Int64
}

type rewritten struct {
	q   string
	err error
}

// maxCachedQueries bounds the rewrite cache. Queries are constants in the
// code (a few hundred variants), so this is only a backstop against code
// that builds SQL from data.
const maxCachedQueries = 4096

func (db *DB) prepare(q string) (string, error) {
	if v, ok := db.cache.Load(q); ok {
		r := v.(rewritten)
		return r.q, r.err
	}
	out, err := rewrite(q, db.postgres)
	if err != nil {
		err = fmt.Errorf("store: non-portable SQL: %w\n\tin: %s", err, compact(q))
	}
	if db.cacheN.Load() < maxCachedQueries {
		if _, loaded := db.cache.LoadOrStore(q, rewritten{out, err}); !loaded {
			db.cacheN.Add(1)
		}
	}
	return out, err
}

func (db *DB) args(args []any) []any {
	if !db.postgres {
		return args
	}
	return pgArgs(args)
}

func (db *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	q, err := db.prepare(q)
	if err != nil {
		return nil, err
	}
	res, err := db.sql.ExecContext(ctx, q, db.args(args)...)
	if err != nil {
		return nil, err
	}
	return result{res}, nil
}

func (db *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	q, err := db.prepare(q)
	if err != nil {
		return nil, err
	}
	return db.sql.QueryContext(ctx, q, db.args(args)...)
}

func (db *DB) QueryRowContext(ctx context.Context, q string, args ...any) *Row {
	q, err := db.prepare(q)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{row: db.sql.QueryRowContext(ctx, q, db.args(args)...)}
}

func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := db.sql.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, db: db}, nil
}

// inTx runs fn in a transaction and commits it if fn returns nil. On
// PostgreSQL a transaction that lost a serialization conflict or was
// picked as a deadlock victim is retried (a few times, with jitter): fn
// must therefore only have effects through tx, and read what it needs
// through tx. SQLite never reports either.
func (db *DB) inTx(ctx context.Context, fn func(tx *Tx) error) error {
	for attempt := 1; ; attempt++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err = fn(tx); err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err == nil || attempt == 5 || !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt*10+rand.IntN(20)) * time.Millisecond):
		}
	}
}

// Tx is a transaction on DB, with the same statement handling.
type Tx struct {
	tx *sql.Tx
	db *DB
}

func (tx *Tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	q, err := tx.db.prepare(q)
	if err != nil {
		return nil, err
	}
	res, err := tx.tx.ExecContext(ctx, q, tx.db.args(args)...)
	if err != nil {
		return nil, err
	}
	return result{res}, nil
}

func (tx *Tx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	q, err := tx.db.prepare(q)
	if err != nil {
		return nil, err
	}
	return tx.tx.QueryContext(ctx, q, tx.db.args(args)...)
}

func (tx *Tx) QueryRowContext(ctx context.Context, q string, args ...any) *Row {
	q, err := tx.db.prepare(q)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{row: tx.tx.QueryRowContext(ctx, q, tx.db.args(args)...)}
}

func (tx *Tx) Commit() error   { return tx.tx.Commit() }
func (tx *Tx) Rollback() error { return tx.tx.Rollback() }

// lockTable blocks every other writer of table until the transaction
// ends (readers go on), for decisions that depend on the absence of rows,
// which no row lock can cover ("create the first user only if there is
// none"). SQLite needs nothing: its one connection runs one transaction
// at a time.
func (tx *Tx) lockTable(ctx context.Context, table string) error {
	if !tx.db.postgres {
		return nil
	}
	// SHARE ROW EXCLUSIVE conflicts with itself and with every write, not
	// with plain SELECTs. table is a constant from the store's code.
	_, err := tx.tx.ExecContext(ctx, `LOCK TABLE `+quoteIdent(table)+` IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

// Row is sql.Row, able to carry a rewrite error to Scan.
type Row struct {
	row *sql.Row
	err error
}

func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

func (r *Row) Err() error {
	if r.err != nil {
		return r.err
	}
	return r.row.Err()
}

// errLastInsertID: PostgreSQL has no last insert ID. Use
// `INSERT ... RETURNING id` with QueryRowContext, on both backends.
var errLastInsertID = errors.New("store: LastInsertId is not portable; use INSERT ... RETURNING id")

// result hides LastInsertId on both backends, so code relying on it fails
// in the SQLite tests too rather than only in production on Postgres.
type result struct{ sql.Result }

func (result) LastInsertId() (int64, error) { return 0, errLastInsertID }

// isUnique reports a UNIQUE or PRIMARY KEY violation.
func isUnique(err error) bool {
	if err == nil {
		return false
	}
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pg.Code == "23505"
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// retryable: serialization failure or deadlock (PostgreSQL); the whole
// transaction may simply be run again.
func retryable(err error) bool {
	pg, ok := errors.AsType[*pgconn.PgError](err)
	return ok && (pg.Code == "40001" || pg.Code == "40P01")
}

// pgArgs converts arguments to what PostgreSQL columns hold. Booleans are
// stored as 0/1 in BIGINT columns (as in SQLite, whose driver does this
// conversion itself); named types (SiteStatus) become their underlying
// type. The slice is copied only when something changes.
func pgArgs(args []any) []any {
	var out []any
	for i, a := range args {
		if v, changed := pgArg(a); changed {
			if out == nil {
				out = append([]any(nil), args...)
			}
			out[i] = v
		}
	}
	if out == nil {
		return args
	}
	return out
}

func pgArg(a any) (any, bool) {
	switch v := a.(type) {
	case nil, string, []byte, int, int64, float64, time.Time:
		return a, false
	case bool:
		if v {
			return int64(1), true
		}
		return int64(0), true
	case driver.Valuer:
		return a, false
	}
	rv := reflect.ValueOf(a)
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return int64(1), true
		}
		return int64(0), true
	case reflect.String:
		return rv.String(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if u := rv.Uint(); u <= math.MaxInt64 {
			return int64(u), true
		}
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return a, false
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func compact(q string) string { return strings.Join(strings.Fields(q), " ") }

// --- tokenizer ---

type tokKind uint8

const (
	tkSpace  tokKind = iota // whitespace and comments
	tkWord                  // identifier or keyword
	tkQuoted                // "identifier"
	tkString                // 'literal'
	tkNumber                //
	tkParam                 // ?
	tkPunct                 // operators, parentheses, commas
	tkOther                 // `ident`, [ident], ?NNN, :name, @name, $name
)

type token struct {
	kind tokKind
	text string
}

func (t token) is(word string) bool { return t.kind == tkWord && strings.EqualFold(t.text, word) }

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentChar(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' || c == '$' }

// tokenize splits SQL into tokens; concatenating their texts gives q back.
func tokenize(q string) ([]token, error) {
	var out []token
	for i := 0; i < len(q); {
		c, start := q[i], i
		kind := tkPunct
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			for i < len(q) && strings.IndexByte(" \t\n\r\f", q[i]) >= 0 {
				i++
			}
			kind = tkSpace
		case c == '-' && strings.HasPrefix(q[i:], "--"):
			if j := strings.IndexByte(q[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(q)
			}
			kind = tkSpace
		case c == '/' && strings.HasPrefix(q[i:], "/*"):
			j := strings.Index(q[i+2:], "*/")
			if j < 0 {
				return nil, errors.New("unterminated comment")
			}
			i += j + 4
			kind = tkSpace
		case c == '\'' || c == '"' || c == '`':
			i++
			for {
				j := strings.IndexByte(q[i:], c)
				if j < 0 {
					return nil, fmt.Errorf("unterminated %c quote", c)
				}
				i += j + 1
				if i < len(q) && q[i] == c { // doubled: escaped quote
					i++
					continue
				}
				break
			}
			kind = map[byte]tokKind{'\'': tkString, '"': tkQuoted, '`': tkOther}[c]
		case c == '[':
			j := strings.IndexByte(q[i:], ']')
			if j < 0 {
				return nil, errors.New("unterminated [")
			}
			i += j + 1
			kind = tkOther
		case c == '?':
			i++
			kind = tkParam
			for i < len(q) && q[i] >= '0' && q[i] <= '9' {
				i++
				kind = tkOther
			}
		case (c == ':' || c == '@' || c == '$') && i+1 < len(q) && (isIdentStart(q[i+1]) || q[i+1] >= '0' && q[i+1] <= '9'):
			i++
			for i < len(q) && isIdentChar(q[i]) {
				i++
			}
			kind = tkOther
		case isIdentStart(c):
			for i < len(q) && isIdentChar(q[i]) {
				i++
			}
			kind = tkWord
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9':
			for i < len(q) && (isIdentChar(q[i]) || q[i] == '.' ||
				(q[i] == '+' || q[i] == '-') && (q[i-1] == 'e' || q[i-1] == 'E')) {
				i++
			}
			kind = tkNumber
		default:
			i++
			for _, op := range []string{"->>", "||", "<=", ">=", "<>", "!=", "==", "::", "<<", ">>", "->"} {
				if strings.HasPrefix(q[start:], op) {
					i = start + len(op)
					break
				}
			}
		}
		out = append(out, token{kind, q[start:i]})
	}
	return out, nil
}

// significant returns the indexes of the non-space tokens.
func significant(toks []token) []int {
	var idx []int
	for i, t := range toks {
		if t.kind != tkSpace {
			idx = append(idx, i)
		}
	}
	return idx
}

// sqliteOnlyFuncs are functions SQLite has and PostgreSQL lacks or runs
// differently. IFNULL and 2+-argument MAX/MIN are rewritten instead.
var sqliteOnlyFuncs = map[string]string{
	"group_concat":      "aggregate in Go",
	"strftime":          "store unix seconds (INTEGER) and compute times in Go",
	"datetime":          "store unix seconds (INTEGER) and compute times in Go",
	"date":              "store unix seconds (INTEGER) and compute times in Go",
	"time":              "store unix seconds (INTEGER) and compute times in Go",
	"julianday":         "store unix seconds (INTEGER) and compute times in Go",
	"unixepoch":         "pass time.Now().Unix() as an argument",
	"iif":               "use CASE WHEN ... THEN ... ELSE ... END",
	"instr":             "compare in Go, or use LIKE",
	"printf":            "format in Go",
	"format":            "format in Go",
	"random":            "generate random values in Go",
	"randomblob":        "generate random values in Go",
	"zeroblob":          "build the value in Go",
	"hex":               "encode in Go",
	"unhex":             "decode in Go",
	"char":              "build the string in Go",
	"typeof":            "columns have fixed types; no need",
	"total":             "use COALESCE(SUM(x), 0)",
	"likely":            "drop the planner hint",
	"unlikely":          "drop the planner hint",
	"likelihood":        "drop the planner hint",
	"glob":              "use LIKE",
	"json_extract":      "decode JSON in Go",
	"json_each":         "decode JSON in Go",
	"json_tree":         "decode JSON in Go",
	"json_set":          "encode JSON in Go",
	"json_insert":       "encode JSON in Go",
	"json_replace":      "encode JSON in Go",
	"json_remove":       "encode JSON in Go",
	"json_patch":        "encode JSON in Go",
	"json_valid":        "validate JSON in Go",
	"json_group_array":  "aggregate in Go",
	"json_group_object": "aggregate in Go",
	"last_insert_rowid": "use INSERT ... RETURNING id",
	"changes":           "use RowsAffected",
	"total_changes":     "use RowsAffected",
}

// lint rejects SQL that would not run the same way on both backends.
func lint(toks []token, sig []int) error {
	at := func(k int) token {
		if k < 0 || k >= len(sig) {
			return token{}
		}
		return toks[sig[k]]
	}
	for k, i := range sig {
		t := toks[i]
		switch t.kind {
		case tkOther:
			switch t.text[0] {
			case '`', '[':
				return fmt.Errorf("%s: quote identifiers with double quotes", t.text)
			default:
				return fmt.Errorf("%s: use ? placeholders", t.text)
			}
		case tkPunct:
			switch t.text {
			case "==":
				return errors.New("==: use =")
			case ";":
				if k != len(sig)-1 {
					return errors.New("one statement per call")
				}
			}
		case tkString:
			if p := at(k - 1); p.kind == tkWord && strings.EqualFold(p.text, "x") && sig[k-1] == i-1 {
				return errors.New("X'...' blob literals: pass []byte as an argument")
			}
		case tkWord:
			w := strings.ToLower(t.text)
			next := at(k + 1)
			if next.kind == tkPunct && next.text == "(" && at(k-1).text != "." {
				if hint, bad := sqliteOnlyFuncs[w]; bad {
					return fmt.Errorf("%s(): SQLite-only function; %s", w, hint)
				}
			}
			switch {
			case w == "rowid" || w == "oid" || w == "_rowid_":
				return fmt.Errorf("%s: PostgreSQL has no rowid; use the primary key", t.text)
			case strings.HasPrefix(w, "sqlite_"):
				return fmt.Errorf("%s: SQLite internals", t.text)
			case k == 0 && (w == "replace" || w == "pragma" || w == "vacuum" || w == "attach" || w == "detach" ||
				w == "reindex" || w == "analyze"):
				return fmt.Errorf("%s: SQLite-only statement", strings.ToUpper(w))
			case (w == "insert" || w == "update") && at(k+1).is("or") && !at(k+2).is("ignore"):
				return fmt.Errorf("%s OR %s: use INSERT ... ON CONFLICT (...) DO UPDATE / DO NOTHING",
					strings.ToUpper(w), strings.ToUpper(at(k+2).text))
			case w == "update" && at(k+1).is("or"):
				return errors.New("UPDATE OR IGNORE: SQLite-only")
			case w == "glob" || w == "regexp" || w == "match":
				return fmt.Errorf("%s: SQLite-only operator; use LIKE", strings.ToUpper(w))
			case w == "limit" && at(k+1).text == "-":
				return errors.New("LIMIT -1: leave LIMIT out")
			case w == "indexed" && at(k-1).is("not") || w == "indexed" && at(k+1).is("by"):
				return errors.New("INDEXED BY: SQLite-only")
			case w == "conflict" && at(k-1).is("on") && at(k+1).is("do") && at(k+2).is("update"):
				return errors.New("ON CONFLICT DO UPDATE needs a conflict target: ON CONFLICT (col, ...)")
			}
		}
	}
	return nil
}

// lockClause returns the index (into sig) where a trailing row-locking
// clause starts, or -1: FOR UPDATE|SHARE [OF t, ...] [NOWAIT|SKIP LOCKED].
func lockClause(toks []token, sig []int) int {
	n := len(sig)
	if n > 0 && toks[sig[n-1]].text == ";" {
		n--
	}
	for k := n - 2; k >= 0; k-- {
		if toks[sig[k]].is("for") && (toks[sig[k+1]].is("update") || toks[sig[k+1]].is("share")) {
			return k
		}
		t := toks[sig[k+1]]
		if t.kind != tkWord && t.kind != tkQuoted && t.text != "," && t.text != "." {
			return -1
		}
	}
	return -1
}

// rewrite returns q as the given dialect should run it.
func rewrite(q string, postgres bool) (string, error) {
	toks, err := tokenize(q)
	if err != nil {
		return "", err
	}
	sig := significant(toks)
	if err := lint(toks, sig); err != nil {
		return "", err
	}
	lock := lockClause(toks, sig)
	if lock >= 0 && !toks[sig[0]].is("select") {
		return "", errors.New("FOR UPDATE/SHARE belongs to a SELECT")
	}
	if !postgres {
		if lock < 0 {
			return q, nil
		}
		var b strings.Builder
		for _, t := range toks[:sig[lock]] {
			b.WriteString(t.text)
		}
		return strings.TrimRight(b.String(), " \t\r\n"), nil
	}

	at := func(k int) token {
		if k < 0 || k >= len(sig) {
			return token{}
		}
		return toks[sig[k]]
	}
	repl := map[int]string{} // token index -> replacement text
	// INSERT OR IGNORE -> INSERT ... ON CONFLICT DO NOTHING (before any
	// RETURNING).
	if at(0).is("insert") && at(1).is("or") && at(2).is("ignore") {
		repl[sig[1]], repl[sig[2]] = "", ""
		depth, pos := 0, len(toks)
		if n := len(sig); toks[sig[n-1]].text == ";" {
			pos = sig[n-1]
		}
		for k := 3; k < len(sig); k++ {
			t := at(k)
			switch {
			case t.text == "(":
				depth++
			case t.text == ")":
				depth--
			case depth == 0 && t.is("conflict") && at(k-1).is("on"):
				return "", errors.New("INSERT OR IGNORE with ON CONFLICT")
			case depth == 0 && t.is("returning"):
				pos = sig[k]
				k = len(sig)
			}
		}
		if pos == len(toks) {
			toks = append(toks, token{tkSpace, ""})
		}
		repl[pos] = " ON CONFLICT DO NOTHING " + toks[pos].text
	}
	qualifyUpsert(toks, sig, repl)
	n := 0
	for k, i := range sig {
		t := toks[i]
		switch t.kind {
		case tkParam:
			n++
			repl[i] = "$" + strconv.Itoa(n)
		case tkWord:
			w := strings.ToLower(t.text)
			call := at(k+1).text == "(" && at(k-1).text != "."
			switch {
			case w == "ifnull" && call:
				repl[i] = "COALESCE"
			case (w == "max" || w == "min") && call && topLevelCommas(toks, sig, k+1) > 0:
				// SQLite's scalar max(a, b)/min(a, b).
				repl[i] = map[string]string{"max": "GREATEST", "min": "LEAST"}[w]
			case w == "like":
				// SQLite's LIKE ignores (ASCII) case; PostgreSQL's doesn't.
				repl[i] = "ILIKE"
			case w == "true" || w == "false":
				// Booleans are 0/1 integers in both schemas.
				repl[i] = map[string]string{"true": "1", "false": "0"}[w]
			case at(k-1).is("as") && at(k+1).text == ")":
				// CAST(x AS INTEGER): SQLite integers are 64-bit.
				switch w {
				case "integer", "int":
					repl[i] = "BIGINT"
				case "real":
					repl[i] = "DOUBLE PRECISION"
				case "blob":
					repl[i] = "BYTEA"
				}
			}
		}
	}
	if len(repl) == 0 {
		return q, nil
	}
	var b strings.Builder
	for i, t := range toks {
		if r, ok := repl[i]; ok {
			b.WriteString(r)
		} else {
			b.WriteString(t.text)
		}
	}
	return b.String(), nil
}

// exprKeywords are the words in an upsert's DO UPDATE clause that are not
// column names.
var exprKeywords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`and or not null is in between like ilike escape case when then else end distinct
		from true false as collate exists any all some array isnull notnull default where current_timestamp
		current_date current_time localtime localtimestamp`) {
		exprKeywords[w] = true
	}
}

// qualifyUpsert qualifies the column references of an INSERT's ON
// CONFLICT ... DO UPDATE SET clause with the target table: SQLite reads a
// bare name there as the existing row's column, PostgreSQL finds it
// ambiguous with excluded's. Assignment targets stay bare, as both
// require. A clause with a subquery is left alone (and fails loudly on
// PostgreSQL if it needs qualifying: qualify it by hand).
func qualifyUpsert(toks []token, sig []int, repl map[int]string) {
	at := func(k int) token {
		if k < 0 || k >= len(sig) {
			return token{}
		}
		return toks[sig[k]]
	}
	if !at(0).is("insert") || !at(1).is("into") {
		return
	}
	k := 2
	if at(k+1).text == "." { // schema.table
		k += 2
	}
	qual := at(k).text
	if at(k + 1).is("as") {
		qual = at(k + 2).text
	}
	start, depth := -1, 0
	for j := k; j < len(sig) && start < 0; j++ {
		switch t := at(j); {
		case t.text == "(":
			depth++
		case t.text == ")":
			depth--
		case depth == 0 && t.is("do") && at(j+1).is("update") && at(j+2).is("set"):
			start = j + 3
		}
	}
	if start < 0 {
		return
	}
	for j := start; j < len(sig); j++ {
		if at(j).is("select") {
			return
		}
	}
	depth = 0
	for j := start; j < len(sig); j++ {
		t := at(j)
		switch {
		case t.text == "(":
			depth++
			continue
		case t.text == ")":
			depth--
			continue
		case depth == 0 && t.is("returning"):
			return
		case t.kind != tkWord && t.kind != tkQuoted:
			continue
		case t.kind == tkWord && exprKeywords[strings.ToLower(t.text)]:
			continue
		}
		prev, next := at(j-1), at(j+1)
		target := depth == 0 && (j == start || prev.text == ",") && next.text == "="
		if target || prev.text == "." || next.text == "." || next.text == "(" || prev.is("as") || prev.is("collate") {
			continue
		}
		repl[sig[j]] = qual + "." + t.text
	}
}

// topLevelCommas counts the commas directly inside the parenthesis at
// sig[open].
func topLevelCommas(toks []token, sig []int, open int) int {
	depth, n := 0, 0
	for k := open; k < len(sig); k++ {
		switch toks[sig[k]].text {
		case "(":
			depth++
		case ")":
			if depth--; depth == 0 {
				return n
			}
		case ",":
			if depth == 1 {
				n++
			}
		}
	}
	return n
}
