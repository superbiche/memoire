// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Engine selects the SQL dialect the store speaks. The seam is deliberately
// store-internal (memoire:D157): SQLite stays a first-class backend
// (local-first single-user deployments), Postgres/CNPG serves hub scale.
// Queries in this package are written once in the shared subset where the
// engines agree, and branched per engine ONLY where they cannot
// (docs/SLICE5-GROUNDWORK-DESIGN.md translation table is the contract).
type Engine int

const (
	EngineSQLite Engine = iota
	EnginePostgres
)

func (e Engine) String() string {
	if e == EnginePostgres {
		return "postgres"
	}
	return "sqlite"
}

// IsPostgresDSN reports whether a store path selects the Postgres engine.
// Anything else is a SQLite file path (the historical contract of
// MEMOIRE_DB).
func IsPostgresDSN(path string) bool {
	return strings.HasPrefix(path, "postgres://") || strings.HasPrefix(path, "postgresql://")
}

// rebind converts this package's canonical '?' placeholders to Postgres
// $1..$n. Placeholders inside single-quoted SQL string literals are left
// alone (none of this package's queries has one — the scan is
// belt-and-suspenders; a doubled '' escape toggles the flag twice, net
// unchanged).
func rebind(eng Engine, query string) string {
	if eng != EnginePostgres || !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 0
	inStr := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			inStr = !inStr
			b.WriteByte(c)
		case c == '?' && !inStr:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// insertIgnore returns the per-engine spelling of "insert, silently skip
// unique-key duplicates" as a (prefix, suffix) pair around a plain INSERT:
// SQLite keeps its historical INSERT OR IGNORE byte-for-byte (retrocompat —
// OR IGNORE also swallows non-unique constraint violations, and the SQLite
// path must not change behavior under the seam); Postgres appends
// ON CONFLICT DO NOTHING.
func insertIgnore(eng Engine) (prefix, suffix string) {
	if eng == EnginePostgres {
		return "", " ON CONFLICT DO NOTHING"
	}
	return "OR IGNORE ", ""
}

// dbc wraps a *sql.DB with the engine tag and rebinds placeholders on the
// way through. It exposes ONLY the surface this package uses, so a new call
// pattern cannot silently bypass the rebind (no embedding on purpose).
type dbc struct {
	conn *sql.DB
	eng  Engine
}

func (c dbc) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(ctx, rebind(c.eng, query), args...)
}

func (c dbc) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.conn.QueryContext(ctx, rebind(c.eng, query), args...)
}

func (c dbc) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.conn.QueryRowContext(ctx, rebind(c.eng, query), args...)
}

func (c dbc) Exec(query string, args ...any) (sql.Result, error) {
	return c.conn.Exec(rebind(c.eng, query), args...)
}

func (c dbc) Query(query string, args ...any) (*sql.Rows, error) {
	return c.conn.Query(rebind(c.eng, query), args...)
}

func (c dbc) QueryRow(query string, args ...any) *sql.Row {
	return c.conn.QueryRow(rebind(c.eng, query), args...)
}

func (c dbc) BeginTx(ctx context.Context, opts *sql.TxOptions) (dbtx, error) {
	tx, err := c.conn.BeginTx(ctx, opts)
	if err != nil {
		return dbtx{}, err
	}
	return dbtx{tx: tx, eng: c.eng}, nil
}

func (c dbc) Ping() error   { return c.conn.Ping() }
func (c dbc) Close() error  { return c.conn.Close() }
func (c dbc) SetMaxOpenConns(n int) { c.conn.SetMaxOpenConns(n) }

// dbtx is the transaction twin of dbc.
type dbtx struct {
	tx  *sql.Tx
	eng Engine
}

func (t dbtx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, rebind(t.eng, query), args...)
}

func (t dbtx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, rebind(t.eng, query), args...)
}

func (t dbtx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, rebind(t.eng, query), args...)
}

func (t dbtx) Commit() error   { return t.tx.Commit() }
func (t dbtx) Rollback() error { return t.tx.Rollback() }

// The tsvector fallback consumes SanitizeFTS output VERBATIM via
// websearch_to_tsquery (see SearchWithFilter): the double quotes are
// websearch phrase syntax, which neutralizes operator-shaped tokens
// (a literal OR or -rf stays a term), and the bare OR joiners are the
// disjunction. No transform function exists for that path on purpose
// (round-2 N5 / round-3 R3-2: the transform WAS the bug).

// ftsToMatchText converts SanitizeFTS output into pg_search |||
// match-disjunction input: bare tokens, space-separated. ||| tokenizes the
// input itself (no query syntax — a literal OR token is just a term), so
// the FTS5 quoting and the OR JOINERS both go; quote removal is safe
// because SanitizeFTS tokens cannot contain quotes (tokenRE), and joiner
// removal is anchored on the quoted boundaries `" OR "` produced by
// SanitizeFTS itself, so a bare OR token (rendered `"OR"`) survives as a
// term (round-1 finding 4).
func ftsToMatchText(match string) string {
	return strings.ReplaceAll(strings.ReplaceAll(match, `" OR "`, `" "`), `"`, ``)
}
