package store

import (
	"errors"
	"strings"
	"testing"
)

func TestRewritePostgres(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`SELECT a FROM t WHERE b = ? AND c = ?`, `SELECT a FROM t WHERE b = $1 AND c = $2`},
		// Not inside literals, quoted identifiers or comments.
		{`SELECT '?', "a?b" FROM t -- why?
		 WHERE x = ? /* ? */`, `SELECT '?', "a?b" FROM t -- why?
		 WHERE x = $1 /* ? */`},
		{`SELECT 'it''s ?' || ? FROM t`, `SELECT 'it''s ?' || $1 FROM t`},
		{`SELECT IFNULL(a, 0), ifnull(b, '') FROM t`, `SELECT COALESCE(a, 0), COALESCE(b, '') FROM t`},
		// Scalar max/min; the aggregates stay.
		{`SELECT MAX(a, b), min(c, (d), e), MAX(f), COUNT(*) FROM t`,
			`SELECT GREATEST(a, b), LEAST(c, (d), e), MAX(f), COUNT(*) FROM t`},
		{`INSERT OR IGNORE INTO t (a) VALUES (?)`, `INSERT   INTO t (a) VALUES ($1) ON CONFLICT DO NOTHING `},
		{`INSERT OR IGNORE INTO t (a) VALUES (?) RETURNING id`,
			`INSERT   INTO t (a) VALUES ($1)  ON CONFLICT DO NOTHING RETURNING id`},
		{`SELECT a FROM t WHERE b LIKE ? AND c NOT LIKE 'x%'`, `SELECT a FROM t WHERE b ILIKE $1 AND c NOT ILIKE 'x%'`},
		{`UPDATE t SET on_ = TRUE WHERE off_ = false`, `UPDATE t SET on_ = 1 WHERE off_ = 0`},
		{`SELECT CAST(a AS INTEGER), CAST(b AS REAL), CAST(c AS TEXT) FROM t`,
			`SELECT CAST(a AS BIGINT), CAST(b AS DOUBLE PRECISION), CAST(c AS TEXT) FROM t`},
		{`SELECT a FROM t WHERE id = ? FOR UPDATE`, `SELECT a FROM t WHERE id = $1 FOR UPDATE`},
		// Upserts: bare column references in DO UPDATE become the table's.
		{`INSERT INTO c (k, n) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET n = n + excluded.n, m = MAX(m, excluded.m)`,
			`INSERT INTO c (k, n) VALUES ($1, $2) ON CONFLICT (k) DO UPDATE SET n = c.n + excluded.n, m = GREATEST(c.m, excluded.m)`},
		{`INSERT INTO p (a) VALUES (?) ON CONFLICT (a) DO UPDATE SET
			x = CASE WHEN repo = excluded.repo THEN x ELSE 0 END, "offset" = excluded."offset"
			WHERE p.y IS NOT NULL RETURNING id`,
			`INSERT INTO p (a) VALUES ($1) ON CONFLICT (a) DO UPDATE SET
			x = CASE WHEN p.repo = excluded.repo THEN p.x ELSE 0 END, "offset" = excluded."offset"
			WHERE p.y IS NOT NULL RETURNING id`},
		{`INSERT INTO t AS o (a) VALUES (?) ON CONFLICT (a) DO UPDATE SET n = n + 1`,
			`INSERT INTO t AS o (a) VALUES ($1) ON CONFLICT (a) DO UPDATE SET n = o.n + 1`},
		{`INSERT INTO t (a) VALUES (?) ON CONFLICT (a) DO NOTHING`, `INSERT INTO t (a) VALUES ($1) ON CONFLICT (a) DO NOTHING`},
	} {
		got, err := rewrite(c.in, true)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// SQLite runs what it always ran; only a row-locking clause goes (its
// single connection is the lock).
func TestRewriteSQLite(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`SELECT a FROM t WHERE b = ? AND MAX(a, b) > 0 AND c LIKE ?`, `SELECT a FROM t WHERE b = ? AND MAX(a, b) > 0 AND c LIKE ?`},
		{`SELECT a FROM t WHERE id = ? FOR UPDATE`, `SELECT a FROM t WHERE id = ?`},
		{"SELECT a FROM t WHERE id = ?\n\t\tFOR UPDATE SKIP LOCKED", `SELECT a FROM t WHERE id = ?`},
		{`SELECT a FROM t FOR SHARE OF t NOWAIT`, `SELECT a FROM t`},
		{`SELECT a FROM t WHERE kind = 'for update'`, `SELECT a FROM t WHERE kind = 'for update'`},
	} {
		got, err := rewrite(c.in, false)
		if err != nil || got != c.want {
			t.Errorf("rewrite(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// Constructs only one backend runs are refused on both, so the SQLite
// tests catch them.
func TestRewriteRejectsNonPortableSQL(t *testing.T) {
	for _, q := range []string{
		`INSERT OR REPLACE INTO t (a) VALUES (?)`,
		`REPLACE INTO t (a) VALUES (?)`,
		`UPDATE OR IGNORE t SET a = 1`,
		`DELETE FROM t WHERE rowid IN (SELECT rowid FROM t LIMIT 5)`,
		`SELECT GROUP_CONCAT(a, ',') FROM t`,
		`SELECT strftime('%s', 'now')`,
		`SELECT datetime('now')`,
		`SELECT a FROM t WHERE b GLOB 'x*'`,
		`SELECT a FROM t LIMIT -1 OFFSET 5`,
		`SELECT a FROM t WHERE b == 1`,
		"SELECT `a` FROM t",
		`SELECT [a] FROM t`,
		`SELECT a FROM t WHERE b = ?1`,
		`SELECT a FROM t WHERE b = :b`,
		`SELECT a FROM t WHERE b = $1`,
		`SELECT name FROM sqlite_master`,
		`PRAGMA foreign_keys = ON`,
		`VACUUM`,
		`INSERT INTO t (a) VALUES (1); INSERT INTO t (a) VALUES (2)`,
		`INSERT INTO t (a) VALUES (?) ON CONFLICT DO UPDATE SET a = 1`,
		`SELECT X'00ff'`,
		`SELECT a FROM t WHERE b = 'unterminated`,
	} {
		for _, pg := range []bool{false, true} {
			if _, err := rewrite(q, pg); err == nil {
				t.Errorf("rewrite(%q, postgres=%v) accepted it", q, pg)
			}
		}
	}
	// Words that merely look alike are fine.
	for _, q := range []string{
		`SELECT id, time, kind FROM site_events WHERE site_id = ?`,
		`INSERT INTO audit_log (time, actor) VALUES (?, ?)`,
		`SELECT a FROM t WHERE kind = 'rowid' AND "date" = ?`,
		`SELECT a FROM t;`,
	} {
		for _, pg := range []bool{false, true} {
			if _, err := rewrite(q, pg); err != nil {
				t.Errorf("rewrite(%q, postgres=%v): %v", q, pg, err)
			}
		}
	}
}

func TestPGArgs(t *testing.T) {
	type status string
	in := []any{true, false, status("active"), uint32(7), int8(-1), float32(0.5), "s", nil, []byte("b")}
	out := pgArgs(in)
	want := []any{int64(1), int64(0), "active", int64(7), int64(-1), float64(0.5), "s", nil, []byte("b")}
	for i := range want {
		if w, ok := want[i].([]byte); ok {
			if string(out[i].([]byte)) != string(w) {
				t.Errorf("arg %d = %#v", i, out[i])
			}
			continue
		}
		if out[i] != want[i] {
			t.Errorf("arg %d = %#v, want %#v", i, out[i], want[i])
		}
	}
	if in[0] != true {
		t.Error("pgArgs modified its input")
	}
	plain := []any{"a", int64(1)}
	if got := pgArgs(plain); &got[0] != &plain[0] {
		t.Error("pgArgs copied arguments it didn't change")
	}
}

func TestLastInsertIdRefused(t *testing.T) {
	s := openSQLite(t)
	res, err := s.db.ExecContext(t.Context(), `INSERT INTO settings (key, value) VALUES ('k', 'v')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.LastInsertId(); !errors.Is(err, errLastInsertID) {
		t.Errorf("LastInsertId: %v", err)
	}
	if n, err := res.RowsAffected(); n != 1 || err != nil {
		t.Errorf("RowsAffected = %d, %v", n, err)
	}
	// The rewrite error reaches the caller, with the query.
	err = s.db.QueryRowContext(t.Context(), `SELECT rowid FROM settings`).Scan(new(int))
	if err == nil || !strings.Contains(err.Error(), "SELECT rowid FROM settings") {
		t.Errorf("err = %v", err)
	}
}
