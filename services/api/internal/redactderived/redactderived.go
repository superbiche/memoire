// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package redactderived applies the current redaction ruleset (redact.RulesVersion) to the DERIVED stores
// (memoire:D144 residual gap; liaison:D97 decision 1's "then rebuild derived"
// step, realized as targeted in-place redaction instead of a full rebuild —
// no raw-file linkage column exists, and a rebuild would destroy review
// history that is not reproducible from raw).
//
// Scope: every text-bearing column an ingested envelope or a derivation can
// reach — events(text, payload_json, meta_json, cwd), observations(text,
// meta_json), derivation_runs(meta_json — failed runs retain raw model
// responses), sessions(meta_json), observation_review_log(reason_text),
// audit_log(details_json). observations_fts stays consistent through the
// external-content AU trigger fired by the observations UPDATE; on Postgres
// the generated observations.text_tsv column follows the rewritten text
// automatically.
//
// Hash consistency: observations.content_hash is the dedupe/overlap key over
// (project, kind, normalized text) — when this tool changes an observation's
// text it recomputes the stored hash with the same derive.CanonicalContentHash,
// so overlap detection keeps working against redacted rows. KNOWN CAVEAT
// (MUST be operator-ratified before live apply): derivation_runs.input_hash
// is computed from event payload bytes at derive time and is NOT recomputed
// here. An operator-triggered re-derive of a pre-redaction window therefore
// computes a different input_hash, mints a NEW run row (0006 tuple differs),
// and re-inserts its candidates — and the accepted-overlap check only
// DOWNGRADES colliding candidates to proposed, it does not suppress
// insertion (store.go InsertDerivedObservation), so such a re-derive can
// create proposed duplicate observation rows needing review-queue triage.
// The auto-sweep derives only recent windows; historical re-derives are
// operator-initiated.
//
// Identity-path columns (project_roots.root, project_identity_alerts.root/
// known_root) are NOT rewritten: they are primary-key components project
// resolution matches on — redacting one would corrupt identity, not clean
// it. Instead a preflight scans them and the tool REFUSES to apply while
// any redaction hit exists there (operator remedy: registry fix + the re-key
// procedure at ~/.agents/shared/memoire-rekey.md, then rerun).
//
// The same key and rules as the raw sweep are used, so markers correlate
// ([REDACTED:<rule>:<hash8>] with the store HMAC key). The key MUST already
// exist — the tool refuses to mint one (a fresh key would break marker
// correlation and a dry-run must stay read-only). Idempotent by
// construction: markers are exempt, so a second pass changes nothing.
//
// Two engines (memoire:D157 seam; redact-v5 S2a): Options.DBPath is a
// SQLite file path or a postgres:// DSN (db.IsPostgresDSN). On Postgres the
// store is opened raw through pgx — NEVER db.Open: no migrations, no DDL
// against the hub — and runs inside ONE REPEATABLE READ transaction:
// apply is all-or-nothing (a failure on any table leaves the store exactly
// as before), dry-run is REPEATABLE READ READ ONLY (a consistent snapshot
// it can never write). SQLite keeps its single IMMEDIATE transaction on
// apply, unchanged. The Report names a Postgres store by its password-
// redacted DSN; the DSN password never reaches an error, stdout or report
// file.
//
// Vector lane (Postgres only): observation_embeddings_pg holds an embedding
// and a sha256 of the observation's PRE-redaction text, so apply DELETES
// the embedding row of every observation whose text it changed, in the same
// transaction — the embed worker re-embeds the redacted text. TableReport.
// EmbeddingsInvalidated counts the affected rows, in dry-run too (would-be
// count); SQLite has no vector lane and always reports 0.
//
// Gating (env double-gate, quiesce) is the cmd wrapper's concern.
package redactderived

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"memoire/services/api/internal/db"
	"memoire/services/api/internal/derive"
	"memoire/services/api/internal/redact"
)

const ToolName = "memoire-redact-derived"

type Options struct {
	DBPath           string
	RedactionKeyPath string
	Apply            bool
}

type TableReport struct {
	RowsScanned      int            `json:"rows_scanned"`
	RowsChanged      int            `json:"rows_changed"`
	HashesRecomputed int            `json:"hashes_recomputed,omitempty"`
	HitsByRule       map[string]int `json:"hits_by_rule"`
	// EmbeddingsInvalidated (Postgres only): observation_embeddings_pg rows
	// of the observations whose text this run changed — counted in dry-run
	// (would-be count), deleted only on apply, in the same transaction.
	// SQLite has no vector lane: always 0.
	EmbeddingsInvalidated int `json:"embeddings_invalidated,omitempty"`
}

type Report struct {
	Tool         string                 `json:"tool"`
	RulesVersion string                 `json:"rules_version"`
	DBPath       string                 `json:"db_path"`
	Applied      bool                   `json:"applied"`
	Tables       map[string]TableReport `json:"tables"`
	TotalChanged int                    `json:"total_changed"`
	// PreflightHits counts redaction hits (current ruleset) in identity-key path columns
	// ("table.col" keys). Any nonzero entry blocks apply.
	PreflightHits map[string]int `json:"preflight_hits,omitempty"`
}

// column describes one redaction target column and how to redact it.
type column struct {
	name   string
	isJSON bool // JSON() for *_json columns, String() for plain text
}

type target struct {
	table   string
	idCol   string
	columns []column
	// contextCols are selected read-only alongside the redaction columns and
	// fed to recompute for changed rows.
	contextCols []string
	// recompute derives extra SET columns for a changed row (e.g. a content
	// hash that must track the new text). newVals is aligned with columns;
	// ctx with contextCols. Returned map keys are column names.
	recompute func(newVals []any, ctx []sql.NullString) (map[string]any, error)
}

// recomputeObservationHash keeps observations.content_hash consistent with
// redacted text: the hash is the dedupe/overlap key over (project, kind,
// normalized text), computed with the exact function derives use. Rows with
// a NULL/empty stored hash (pre-0004 vintage) stay untouched.
func recomputeObservationHash(newVals []any, ctx []sql.NullString) (map[string]any, error) {
	// ctx: project, kind, content_hash — see the observations target below.
	if len(ctx) != 3 || !ctx[2].Valid || ctx[2].String == "" {
		return nil, nil
	}
	text, _ := newVals[0].(string) // columns[0] = text (NOT NULL)
	hash, err := derive.CanonicalContentHash(ctx[0].String, ctx[1].String, text)
	if err != nil {
		return nil, fmt.Errorf("recompute content_hash: %w", err)
	}
	if hash == ctx[2].String {
		return nil, nil
	}
	return map[string]any{"content_hash": hash}, nil
}

// targets enumerates every derived text surface BY NAME (the slice-A lesson:
// enumeration by category grows holes). Adding a text-bearing column to the
// schema means enrolling it here; the contract test pins this list against
// the live schema so drift fails loudly.
var targets = []target{
	{table: "events", idCol: "id", columns: []column{
		{name: "text", isJSON: false},
		{name: "payload_json", isJSON: true},
		{name: "meta_json", isJSON: true},
		{name: "cwd", isJSON: false},
	}},
	{table: "observations", idCol: "id",
		columns: []column{
			{name: "text", isJSON: false},
			{name: "meta_json", isJSON: true},
		},
		contextCols: []string{"project", "kind", "content_hash"},
		recompute:   recomputeObservationHash,
	},
	{table: "derivation_runs", idCol: "id", columns: []column{
		{name: "meta_json", isJSON: true},
	}},
	{table: "sessions", idCol: "session_id", columns: []column{
		{name: "meta_json", isJSON: true},
	}},
	{table: "observation_review_log", idCol: "id", columns: []column{
		{name: "reason_text", isJSON: false},
	}},
	// Triage suggestions (0017): the judge writes free-text reasons ABOUT
	// observation content, so the column is the same class as the review
	// log's reason_text and is enrolled rather than allowlisted. The genre
	// slug is a closed/short vocabulary the lane emits, not quoted content.
	{table: "observation_triage_suggestions", idCol: "id", columns: []column{
		{name: "reason_text", isJSON: false},
	}},
	// Attribution at Stop (0019): the consuming agent's reason line restates
	// the claim an observation carried, so it quotes content and is enrolled
	// like the triage reason. Verdict is the closed measured vocabulary.
	{table: "observation_usage", idCol: "id", columns: []column{
		{name: "reason", isJSON: false},
	}},
	{table: "audit_log", idCol: "id", columns: []column{
		{name: "details_json", isJSON: true},
	}},
}

// Targets exposes the enumeration for the schema-drift contract test.
func Targets() map[string][]string {
	out := make(map[string][]string, len(targets))
	for _, t := range targets {
		cols := make([]string, 0, len(t.columns))
		for _, c := range t.columns {
			cols = append(cols, c.name)
		}
		out[t.table] = cols
	}
	return out
}

type pendingUpdate struct {
	id      any
	setCols []string
	args    []any // aligned with setCols
}

// preflightGuards are identity-key path columns that cannot be rewritten in
// place (see the package comment). The tool scans them with the same rules
// and refuses to apply while any hit exists; dry-run reports the counts.
var preflightGuards = []struct{ table, col string }{
	{"project_roots", "root"},
	{"project_identity_alerts", "root"},
	{"project_identity_alerts", "known_root"},
}

// PreflightGuards exposes the guarded columns for the schema contract test.
func PreflightGuards() [][2]string {
	out := make([][2]string, 0, len(preflightGuards))
	for _, g := range preflightGuards {
		out = append(out, [2]string{g.table, g.col})
	}
	return out
}

// executor is the statement surface one Run drives: on Postgres the
// REPEATABLE READ transaction (apply) or the REPEATABLE READ READ ONLY
// transaction (dry-run); on SQLite the single-connection *sql.DB, with an
// explicit BEGIN IMMEDIATE open on apply (both *sql.DB and *sql.Tx satisfy
// it).
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// store is the redact-derived handle: a single-connection pool tagged with
// its engine. Every statement that carries '?' placeholders goes through q,
// which is the identity on SQLite (byte-for-byte the single-engine
// statements) and binds them to $n on Postgres (db.Rebind).
type store struct {
	*sql.DB
	eng db.Engine
}

func (s *store) pg() bool { return s.eng == db.EnginePostgres }

func (s *store) q(query string) string { return db.Rebind(s.eng, query) }

func engineOf(path string) db.Engine {
	if db.IsPostgresDSN(path) {
		return db.EnginePostgres
	}
	return db.EngineSQLite
}

// openStore opens the engine for DBPath. Postgres: raw pgx handle, never
// db.Open — no migrations, no migrate lock, no provisioning, no DDL against
// the hub; single connection; Ping. SQLite: the single-connection pool the
// IMMEDIATE transaction rides on, unchanged.
func openStore(ctx context.Context, path string) (*store, error) {
	if engineOf(path) == db.EnginePostgres {
		dbh, err := sql.Open("pgx", path)
		if err != nil {
			// The driver's config error can echo the DSN itself (it carries
			// the hub's CNPG password) — keep it out of the message.
			return nil, scrubDSN(path, err)
		}
		dbh.SetMaxOpenConns(1)
		dbh.SetMaxIdleConns(1)
		if err := dbh.PingContext(ctx); err != nil {
			_ = dbh.Close()
			return nil, scrubDSN(path, fmt.Errorf("connect postgres store: %w", err))
		}
		return &store{DB: dbh, eng: db.EnginePostgres}, nil
	}
	dbh, err := sql.Open("sqlite3", path+"?_busy_timeout=10000")
	if err != nil {
		return nil, err
	}
	// The single-connection pool is load-bearing for Apply: BEGIN IMMEDIATE
	// via ExecContext pins the transaction to the pool's only connection, so
	// every later statement runs inside it.
	dbh.SetMaxOpenConns(1)
	return &store{DB: dbh, eng: db.EngineSQLite}, nil
}

// scrubDSN returns a connection error whose message cannot carry the DSN
// password: the driver can echo the DSN into config errors, and the hub
// DSN comes from the memoire-pg-app secret.
func scrubDSN(dsn string, err error) error {
	msg := err.Error()
	if u, perr := url.Parse(dsn); perr == nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "[REDACTED]")
		}
	}
	return errors.New(msg)
}

// redactedDSN is the Report's name for a Postgres store: the DSN with its
// password redacted (url.URL.Redacted) — the hub DSN comes from a secret
// and the report lands in logs and files.
func redactedDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "postgres://[redacted]"
	}
	return u.Redacted()
}

// embeddingBatch bounds one IN-list statement: a hub re-sweep can change
// tens of thousands of observations, and the protocol caps bind parameters
// per statement (65535).
const embeddingBatch = 5000

// invalidateEmbeddings counts (dry-run) or deletes (apply) the
// observation_embeddings_pg rows of the changed observations, in batches.
// It is a no-op (0) when the vector lane table does not exist — its vector
// and text_hash were computed from the PRE-redaction text, so a rewritten
// observation must lose its row in the SAME transaction; the embed worker
// re-embeds the redacted text.
func (s *store) invalidateEmbeddings(ctx context.Context, ex executor, apply bool, obsIDs []any) (int, error) {
	// The probe rides the SAME executor as every other statement: on
	// Postgres the pool's only connection is pinned by the open transaction,
	// so a pool-level query here would deadlock on itself.
	var hasTable bool
	if err := ex.QueryRowContext(ctx, `SELECT to_regclass('public.observation_embeddings_pg') IS NOT NULL`).Scan(&hasTable); err != nil {
		return 0, fmt.Errorf("probe observation_embeddings_pg: %w", err)
	}
	if !hasTable {
		return 0, nil
	}
	total := 0
	for start := 0; start < len(obsIDs); start += embeddingBatch {
		ids := obsIDs[start:min(start+embeddingBatch, len(obsIDs))]
		var sb strings.Builder
		sb.WriteString("observation_id IN (")
		for i := range ids {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("?")
		}
		sb.WriteString(")")
		if apply {
			res, err := ex.ExecContext(ctx, s.q("DELETE FROM observation_embeddings_pg WHERE "+sb.String()), ids...)
			if err != nil {
				return total, fmt.Errorf("delete observation_embeddings_pg: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return total, err
			}
			total += int(n)
		} else {
			var n int64
			if err := ex.QueryRowContext(ctx, s.q("SELECT count(*) FROM observation_embeddings_pg WHERE "+sb.String()), ids...).Scan(&n); err != nil {
				return total, fmt.Errorf("count observation_embeddings_pg: %w", err)
			}
			total += int(n)
		}
	}
	return total, nil
}

func preflightScan(ctx context.Context, ex executor, redactor *redact.Redactor) (map[string]int, error) {
	hits := map[string]int{}
	for _, g := range preflightGuards {
		rows, err := ex.QueryContext(ctx, "SELECT "+g.col+" FROM "+g.table)
		if err != nil {
			return nil, fmt.Errorf("preflight %s.%s: %w", g.table, g.col, err)
		}
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			if !v.Valid || v.String == "" {
				continue
			}
			out, _, err := redactor.String(v.String)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("preflight %s.%s: %w", g.table, g.col, err)
			}
			if out != v.String {
				hits[g.table+"."+g.col]++
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.DBPath == "" {
		return Report{}, errors.New("missing db path")
	}
	if opts.RedactionKeyPath == "" {
		return Report{}, errors.New("missing redaction key path")
	}
	// The key must pre-exist: minting a new one here would break marker
	// correlation with the write path and the raw sweep, and would make a
	// dry-run write to disk (review finding 3).
	if _, err := os.Stat(opts.RedactionKeyPath); err != nil {
		return Report{}, fmt.Errorf("redaction key not found: %s (refusing to mint one — markers must correlate with the store's write path)", opts.RedactionKeyPath)
	}
	key, err := redact.LoadOrCreateKey(opts.RedactionKeyPath)
	if err != nil {
		return Report{}, fmt.Errorf("load redaction key: %w", err)
	}
	redactor := redact.New(key)

	c, err := openStore(ctx, opts.DBPath)
	if err != nil {
		return Report{}, err
	}
	defer c.Close()

	report := Report{
		Tool:         ToolName,
		RulesVersion: redact.RulesVersion,
		DBPath:       opts.DBPath,
		Applied:      opts.Apply,
		Tables:       map[string]TableReport{},
	}
	if c.pg() {
		// The hub DSN carries the CNPG password: never in the report.
		report.DBPath = redactedDSN(opts.DBPath)
	}

	// One Run, one transaction (the single-connection pool makes both spellings
	// ride the same underlying handle):
	//   Postgres: REPEATABLE READ, covering preflight + every scan + every
	//     update — apply is all-or-nothing, and the dry-run's READ ONLY
	//     snapshot can never be written.
	//   SQLite: BEGIN IMMEDIATE on apply (unchanged); the dry-run stays
	//     transaction-free as before.
	var ex executor
	var pgTx *sql.Tx
	if c.pg() {
		var err error
		pgTx, err = c.BeginTx(ctx, &sql.TxOptions{
			Isolation: sql.LevelRepeatableRead,
			ReadOnly:  !opts.Apply,
		})
		if err != nil {
			return Report{}, fmt.Errorf("begin repeatable-read transaction: %w", err)
		}
		defer pgTx.Rollback() // no-op after COMMIT
		ex = pgTx
	} else if opts.Apply {
		if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return Report{}, fmt.Errorf("begin immediate: %w", err)
		}
		defer c.ExecContext(ctx, "ROLLBACK") // no-op after COMMIT
		ex = c
	} else {
		ex = c
	}

	// Identity-path preflight: inside the transaction so the refusal
	// judges the same snapshot the updates would mutate.
	preflight, err := preflightScan(ctx, ex, redactor)
	if err != nil {
		return Report{}, err
	}
	report.PreflightHits = preflight
	if opts.Apply && len(preflight) > 0 {
		return Report{}, fmt.Errorf("REFUSING --apply: %s hits in identity-key path columns %v — these are resolution keys and cannot be rewritten in place; fix via registry + the operator-run re-key procedure (see the shared manual ~/.agents/shared/memoire-rekey.md; the SQLite-era scripts/memoire-project-move.sh does not apply to the Postgres hub), then rerun", redact.RulesVersion, preflight)
	}

	for _, t := range targets {
		tr, updates, textChangedIDs, err := scanTable(ctx, ex, redactor, t)
		if err != nil {
			return Report{}, fmt.Errorf("scan %s: %w", t.table, err)
		}
		if opts.Apply && len(updates) > 0 {
			changed, err := applyUpdates(ctx, ex, c, t, updates)
			if err != nil {
				return Report{}, fmt.Errorf("update %s: %w", t.table, err)
			}
			if changed != len(updates) {
				return Report{}, fmt.Errorf("update %s: expected %d changed rows, got %d", t.table, len(updates), changed)
			}
		}
		// Vector-lane invalidation (Postgres only), in the same transaction:
		// a rewritten observation's embedding and text_hash were computed
		// from its pre-redaction text, so the row must go; the embed worker
		// re-embeds the redacted text. Only a TEXT change invalidates (a
		// meta_json-only change leaves the vector valid). Counted in dry-run.
		if t.table == "observations" && c.pg() && len(textChangedIDs) > 0 {
			n, err := c.invalidateEmbeddings(ctx, ex, opts.Apply, textChangedIDs)
			if err != nil {
				return Report{}, fmt.Errorf("embeddings invalidation: %w", err)
			}
			tr.EmbeddingsInvalidated = n
		}
		report.Tables[t.table] = tr
		report.TotalChanged += tr.RowsChanged
	}

	if c.pg() {
		// Commit the REPEATABLE READ transaction (the dry-run's READ ONLY
		// commit writes nothing).
		if err := pgTx.Commit(); err != nil {
			return Report{}, fmt.Errorf("commit: %w", err)
		}
	} else if opts.Apply {
		if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
			return Report{}, fmt.Errorf("commit: %w", err)
		}
	}
	return report, nil
}

// scanTable returns the report, the pending updates, and — for rows whose
// FIRST column (the text column on text-bearing targets) the run changes —
// their ids (the vector-lane invalidation input; empty on other targets).
func scanTable(ctx context.Context, ex executor, redactor *redact.Redactor, t target) (TableReport, []pendingUpdate, []any, error) {
	tr := TableReport{HitsByRule: map[string]int{}}
	cols := t.idCol
	for _, c := range t.columns {
		cols += ", " + c.name
	}
	for _, c := range t.contextCols {
		cols += ", " + c
	}
	rows, err := ex.QueryContext(ctx, "SELECT "+cols+" FROM "+t.table)
	if err != nil {
		return tr, nil, nil, err
	}
	defer rows.Close()

	var updates []pendingUpdate
	var textChangedIDs []any
	for rows.Next() {
		var id any
		vals := make([]sql.NullString, len(t.columns))
		ctxVals := make([]sql.NullString, len(t.contextCols))
		dest := make([]any, 0, len(t.columns)+len(t.contextCols)+1)
		dest = append(dest, &id)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		for i := range ctxVals {
			dest = append(dest, &ctxVals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return tr, nil, nil, err
		}
		tr.RowsScanned++

		changed := false
		firstChanged := false
		newVals := make([]any, len(t.columns))
		for i, c := range t.columns {
			if !vals[i].Valid || vals[i].String == "" {
				if vals[i].Valid {
					newVals[i] = vals[i].String
				} else {
					newVals[i] = nil
				}
				continue
			}
			var out string
			var hits []redact.Hit
			var rerr error
			if c.isJSON {
				var o json.RawMessage
				o, hits, rerr = redactor.JSON(json.RawMessage(vals[i].String))
				out = string(o)
			} else {
				out, hits, rerr = redactor.String(vals[i].String)
			}
			if rerr != nil {
				return tr, nil, nil, fmt.Errorf("%s.%s row %v: %w", t.table, c.name, id, rerr)
			}
			for _, h := range hits {
				tr.HitsByRule[h.Rule] += h.Count
			}
			if out != vals[i].String {
				changed = true
				if i == 0 {
					firstChanged = true
				}
			}
			newVals[i] = out
		}
		if !changed {
			continue
		}
		tr.RowsChanged++
		if firstChanged {
			textChangedIDs = append(textChangedIDs, id)
		}

		setCols := make([]string, 0, len(t.columns)+1)
		args := make([]any, 0, len(t.columns)+1)
		for i, c := range t.columns {
			setCols = append(setCols, c.name)
			args = append(args, newVals[i])
		}
		if t.recompute != nil {
			extra, err := t.recompute(newVals, ctxVals)
			if err != nil {
				return tr, nil, nil, err
			}
			for k, v := range extra {
				setCols = append(setCols, k)
				args = append(args, v)
				tr.HashesRecomputed++
			}
		}
		updates = append(updates, pendingUpdate{id: id, setCols: setCols, args: args})
	}
	if err := rows.Err(); err != nil {
		return tr, nil, nil, err
	}
	return tr, updates, textChangedIDs, nil
}

func applyUpdates(ctx context.Context, ex executor, c *store, t target, updates []pendingUpdate) (int, error) {
	changed := 0
	for _, u := range updates {
		set := ""
		for _, col := range u.setCols {
			if set != "" {
				set += ", "
			}
			set += col + " = ?"
		}
		args := append(append([]any{}, u.args...), u.id)
		res, err := ex.ExecContext(ctx, c.q("UPDATE "+t.table+" SET "+set+" WHERE "+t.idCol+" = ?"), args...)
		if err != nil {
			return changed, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return changed, err
		}
		changed += int(n)
	}
	return changed, nil
}
