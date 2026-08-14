// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package redactderived applies the redact-v1 ruleset to the DERIVED stores
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
// external-content AU trigger fired by the observations UPDATE.
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
// any redact-v1 hit exists there (operator remedy: registry fix +
// project-move re-key, then rerun).
//
// The same key and rules as the raw sweep are used, so markers correlate
// ([REDACTED:<rule>:<hash8>] with the store HMAC key). The key MUST already
// exist — the tool refuses to mint one (a fresh key would break marker
// correlation and a dry-run must stay read-only). Idempotent by
// construction: markers are exempt, so a second pass changes nothing.
//
// DRY-RUN by default; Apply runs scan + updates inside ONE IMMEDIATE
// transaction (consistent snapshot, atomic swap, exact changes count).
// Gating (env double-gate, quiesce) is the cmd wrapper's concern.
package redactderived

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
}

type Report struct {
	Tool         string                 `json:"tool"`
	RulesVersion string                 `json:"rules_version"`
	DBPath       string                 `json:"db_path"`
	Applied      bool                   `json:"applied"`
	Tables       map[string]TableReport `json:"tables"`
	TotalChanged int                    `json:"total_changed"`
	// PreflightHits counts redact-v1 hits in identity-key path columns
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

func preflightScan(ctx context.Context, db *sql.DB, redactor *redact.Redactor) (map[string]int, error) {
	hits := map[string]int{}
	for _, g := range preflightGuards {
		rows, err := db.QueryContext(ctx, "SELECT "+g.col+" FROM "+g.table)
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
			if out, _ := redactor.String(v.String); out != v.String {
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

	db, err := sql.Open("sqlite3", opts.DBPath+"?_busy_timeout=10000")
	if err != nil {
		return Report{}, err
	}
	defer db.Close()
	// The single-connection pool is load-bearing for Apply: BEGIN IMMEDIATE
	// via ExecContext pins the transaction to the pool's only connection, so
	// every later statement runs inside it.
	db.SetMaxOpenConns(1)

	report := Report{
		Tool:         ToolName,
		RulesVersion: redact.RulesVersion,
		DBPath:       opts.DBPath,
		Applied:      opts.Apply,
		Tables:       map[string]TableReport{},
	}

	if opts.Apply {
		if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return Report{}, fmt.Errorf("begin immediate: %w", err)
		}
		defer db.ExecContext(ctx, "ROLLBACK") // no-op after COMMIT
	}

	// Identity-path preflight: inside the apply transaction so the refusal
	// judges the same snapshot the updates would mutate.
	preflight, err := preflightScan(ctx, db, redactor)
	if err != nil {
		return Report{}, err
	}
	report.PreflightHits = preflight
	if opts.Apply && len(preflight) > 0 {
		return Report{}, fmt.Errorf("REFUSING --apply: redact-v1 hits in identity-key path columns %v — these are resolution keys and cannot be rewritten in place; fix via registry + memoire-project-move re-key, then rerun", preflight)
	}

	for _, t := range targets {
		tr, updates, err := scanTable(ctx, db, redactor, t)
		if err != nil {
			return Report{}, fmt.Errorf("scan %s: %w", t.table, err)
		}
		if opts.Apply && len(updates) > 0 {
			changed, err := applyUpdates(ctx, db, t, updates)
			if err != nil {
				return Report{}, fmt.Errorf("update %s: %w", t.table, err)
			}
			if changed != len(updates) {
				return Report{}, fmt.Errorf("update %s: expected %d changed rows, got %d", t.table, len(updates), changed)
			}
		}
		report.Tables[t.table] = tr
		report.TotalChanged += tr.RowsChanged
	}

	if opts.Apply {
		if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
			return Report{}, fmt.Errorf("commit: %w", err)
		}
	}
	return report, nil
}

func scanTable(ctx context.Context, db *sql.DB, redactor *redact.Redactor, t target) (TableReport, []pendingUpdate, error) {
	tr := TableReport{HitsByRule: map[string]int{}}
	cols := t.idCol
	for _, c := range t.columns {
		cols += ", " + c.name
	}
	for _, c := range t.contextCols {
		cols += ", " + c
	}
	rows, err := db.QueryContext(ctx, "SELECT "+cols+" FROM "+t.table)
	if err != nil {
		return tr, nil, err
	}
	defer rows.Close()

	var updates []pendingUpdate
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
			return tr, nil, err
		}
		tr.RowsScanned++

		changed := false
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
			if c.isJSON {
				var o json.RawMessage
				o, hits = redactor.JSON(json.RawMessage(vals[i].String))
				out = string(o)
			} else {
				out, hits = redactor.String(vals[i].String)
			}
			for _, h := range hits {
				tr.HitsByRule[h.Rule] += h.Count
			}
			if out != vals[i].String {
				changed = true
			}
			newVals[i] = out
		}
		if !changed {
			continue
		}
		tr.RowsChanged++

		setCols := make([]string, 0, len(t.columns)+1)
		args := make([]any, 0, len(t.columns)+1)
		for i, c := range t.columns {
			setCols = append(setCols, c.name)
			args = append(args, newVals[i])
		}
		if t.recompute != nil {
			extra, err := t.recompute(newVals, ctxVals)
			if err != nil {
				return tr, nil, err
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
		return tr, nil, err
	}
	return tr, updates, nil
}

func applyUpdates(ctx context.Context, db *sql.DB, t target, updates []pendingUpdate) (int, error) {
	changed := 0
	for _, u := range updates {
		set := ""
		for _, c := range u.setCols {
			if set != "" {
				set += ", "
			}
			set += c + " = ?"
		}
		args := append(append([]any{}, u.args...), u.id)
		res, err := db.ExecContext(ctx, "UPDATE "+t.table+" SET "+set+" WHERE "+t.idCol+" = ?", args...)
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
