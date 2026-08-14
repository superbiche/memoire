// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package pgshadow is the slice-5 groundwork harness (memoire:D157,
// docs/SLICE5-GROUNDWORK-DESIGN.md): it applies the schema-pg/ DDL to a
// Postgres shadow, populates it from the live SQLite store (COPY lane) or
// from raw/ custody (REPLAY lane), and produces the parity report that is
// the standing-shadow rehearsal evidence (D152).
//
// Both engines are driven through database/sql (pgx stdlib driver for
// Postgres) so every parity projection runs IDENTICAL scan/format code.
// Projections are ID-FREE: numeric row ids differ between engines (live
// SQLite has purge gaps; identities re-count), so rows are keyed by their
// natural identities (idempotency_key, session_id, joins through them).
// created_at_epoch on observations is excluded from replay-lane hashes
// (stamped at ingest time, differs by construction).
package pgshadow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"memoire/services/api/internal/authctx"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/drainer"
)

// Apply runs every schema-pg/*.sql file in name order against the shadow.
// Files carry their own BEGIN/COMMIT (same convention as schema/).
func Apply(ctx context.Context, pg *sql.DB, schemaDir string) error {
	entries, err := filepath.Glob(filepath.Join(schemaDir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(entries)
	if len(entries) == 0 {
		return fmt.Errorf("no *.sql files in %s", schemaDir)
	}
	for _, path := range entries {
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := pg.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(path), err)
		}
	}
	// BM25 index provisioning (D160): Apply IS the controlled deployment
	// step for the Postgres store, so the pg_search index is created here —
	// never at store Open (round-1 finding 1: app-path DDL made the
	// optional upgrade a blocking startup dependency). No-op without the
	// extension; loud on failure like every schema statement above.
	if err := db.ProvisionPGSearchIndex(ctx, pg); err != nil {
		return fmt.Errorf("apply bm25 index: %w", err)
	}
	// Vector lane provisioning (D218): same deployment-step posture. The
	// table starts empty so the HNSW index builds incrementally — this can
	// never block on existing data. No-op without the pgvector extension.
	if err := db.ProvisionVectorSchema(ctx, pg); err != nil {
		return fmt.Errorf("apply vector lane: %w", err)
	}
	return nil
}

// tableSpec drives the COPY lane. Cols are the shared column lists (the
// Postgres generated text_tsv is deliberately absent). identityCol names
// the identity sequence to advance after an explicit-id bulk load.
type tableSpec struct {
	name        string
	cols        []string
	identityCol string
}

var copyTables = []tableSpec{
	{"sessions", []string{"session_id", "project", "client", "started_at_epoch", "ended_at_epoch", "principal_id", "meta_json"}, ""},
	{"events", []string{"id", "idempotency_key", "session_id", "content_session_id", "project", "source_adapter", "source_event_id", "event_type", "cwd", "payload_json", "text", "occurred_at_epoch", "received_at_epoch", "deleted_at_epoch", "principal_id", "credential_id", "request_id", "meta_json"}, "id"},
	{"observations", []string{"id", "event_id", "project", "session_id", "kind", "text", "source_adapter", "event_type", "occurred_at_epoch", "created_at_epoch", "deleted_at_epoch", "principal_id", "supersedes_id", "ttl_review_epoch", "confidence", "meta_json", "review_state", "derivation_run_id", "content_hash", "provenance", "can_instruct", "can_evidence", "needs_confirmation", "noise_candidate"}, "id"},
	{"credentials", []string{"id", "token_hash", "prefix", "label", "scopes_json", "revoked_at_epoch", "created_at_epoch", "last_used_at_epoch"}, ""},
	{"audit_log", []string{"id", "principal_id", "credential_id", "request_id", "action", "resource_type", "resource_id", "details_json", "created_at_epoch"}, "id"},
	{"derivation_runs", []string{"id", "project", "session_id", "provider", "data_boundary", "derivation_version", "prompt_version", "input_hash", "window_start_epoch", "window_end_epoch", "status", "created_at_epoch", "completed_at_epoch", "meta_json", "model", "endpoint_id", "created_at_ns", "snapshot_max_event_id"}, ""},
	{"observation_source_events", []string{"observation_id", "event_id", "created_at_epoch"}, ""},
	{"observation_review_log", []string{"id", "observation_id", "from_state", "to_state", "actor", "reason_text", "occurred_at_epoch", "action"}, "id"},
	{"project_roots", []string{"project", "root", "source", "first_seen_epoch", "last_seen_epoch"}, ""},
	{"project_identity_alerts", []string{"project", "root", "known_root", "source", "first_seen_epoch", "last_seen_epoch", "event_count"}, ""},
	{"derive_intents", []string{"project", "session_id", "first_notified_epoch", "last_notified_epoch", "notify_count", "completed_at_epoch", "completed_run_id", "latency_seconds"}, ""},
	{"run_input_events", []string{"run_id", "event_id"}, ""},
}

const copyBatch = 500

// Copy bulk-copies every table from the SQLite store into the shadow,
// preserving row ids (identities are BY DEFAULT), then advances the
// identity sequences. The shadow must be freshly applied and empty.
func Copy(ctx context.Context, pg *sql.DB, lite *sql.DB) (map[string]int64, error) {
	counts := map[string]int64{}
	for _, spec := range copyTables {
		n, err := copyTable(ctx, pg, lite, spec)
		if err != nil {
			return counts, fmt.Errorf("copy %s: %w", spec.name, err)
		}
		counts[spec.name] = n
		// Advance the identity even for a ZERO-row copy (memoire:D165
		// round-4 MED-NEW-1): a store whose events were all hard-purged
		// still carries the sqlite_sequence high-water, and a fresh PG
		// sequence left at 1 would re-mint purged ids — which an orphan
		// manifest row could then falsely cover.
		if spec.identityCol != "" {
			// High-water comes from sqlite_sequence, not MAX(id): SQLite
			// AUTOINCREMENT never reuses ids, so after a hard purge the next
			// id sits ABOVE the surviving max — the shadow must not re-mint
			// purged ids (round-1 finding 6).
			var seqHW sql.NullInt64
			_ = lite.QueryRowContext(ctx,
				`SELECT seq FROM sqlite_sequence WHERE name = ?`, spec.name).Scan(&seqHW)
			adv := fmt.Sprintf(
				`SELECT setval(pg_get_serial_sequence('%s','%s'), GREATEST($1::bigint, (SELECT COALESCE(MAX(%s),1) FROM %s)))`,
				spec.name, spec.identityCol, spec.identityCol, spec.name)
			if _, err := pg.ExecContext(ctx, adv, seqHW.Int64); err != nil {
				return counts, fmt.Errorf("advance %s identity: %w", spec.name, err)
			}
		}
	}
	return counts, nil
}

func copyTable(ctx context.Context, pg *sql.DB, lite *sql.DB, spec tableSpec) (int64, error) {
	rows, err := lite.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(spec.cols, ", "), spec.name))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total int64
	batch := make([][]any, 0, copyBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var sb strings.Builder
		args := make([]any, 0, len(batch)*len(spec.cols))
		fmt.Fprintf(&sb, "INSERT INTO %s(%s) VALUES ", spec.name, strings.Join(spec.cols, ", "))
		for i, row := range batch {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(")
			for j := range row {
				if j > 0 {
					sb.WriteString(", ")
				}
				fmt.Fprintf(&sb, "$%d", len(args)+1)
				args = append(args, row[j])
			}
			sb.WriteString(")")
		}
		if _, err := pg.ExecContext(ctx, sb.String(), args...); err != nil {
			return err
		}
		total += int64(len(batch))
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		vals := make([]any, len(spec.cols))
		ptrs := make([]any, len(spec.cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return total, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		batch = append(batch, vals)
		if len(batch) >= copyBatch {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return total, err
	}
	return total, flush()
}

type ReplayStats struct {
	Files      int
	Skipped    int
	Ingested   int
	Duplicates int
	Failed     int
}

// preloadSet loads a single-TEXT-column query into a membership set (the
// delta-replay skip predicates).
func preloadSet(ctx context.Context, pg *sql.DB, query string) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	rows, err := pg.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		set[v] = struct{}{}
	}
	return set, rows.Err()
}

// Replay drives raw/ custody through the hub-ingest semantics into the
// shadow: NormalizeEnvelope for identity, IngestText for the search-visible
// text, ON CONFLICT (idempotency_key) DO NOTHING for dedup, and the same
// raw-observation projection Ingest writes. Idempotent by construction —
// the standing-shadow delta iteration is just running it again.
//
// delta preloads the shadow's idempotency keys and skips files whose key
// is already present, without any per-file DB round-trip. This is the
// standing-shadow/cutover tail mechanism: a full pass pays ~4 round-trips
// per duplicate; delta pays one SELECT total, then local parse+hash per
// file. Skip is semantically equivalent to the duplicate path ONLY when the
// duplicate path would have written nothing: it re-runs the SESSION insert
// (from the raw file's own session_id) before hitting the event conflict,
// and the idempotency key ignores session_id whenever source_event_id is
// set — so a tail file can repeat an existing key under a NEW session
// (round-2 finding 1). The skip predicate therefore checks BOTH sides:
// the key must be present in events AND the raw file's own session_id must
// be present in sessions; otherwise the file takes the full path, whose
// session insert + event-conflict dedup reproduce drain semantics exactly.
// The duplicate path writes neither observations nor audit rows, so
// nothing else differs.
func Replay(ctx context.Context, pg *sql.DB, rawDir string, delta bool) (ReplayStats, error) {
	var stats ReplayStats
	var present, knownSessions map[string]struct{}
	if delta {
		var err error
		present, err = preloadSet(ctx, pg, `SELECT idempotency_key FROM events`)
		if err != nil {
			return stats, fmt.Errorf("replay delta preload: %w", err)
		}
		knownSessions, err = preloadSet(ctx, pg, `SELECT session_id FROM sessions`)
		if err != nil {
			return stats, fmt.Errorf("replay delta preload: %w", err)
		}
	}
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return stats, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.Files++
		path := filepath.Join(rawDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			stats.Failed++
			continue
		}
		event, err := drainer.NormalizeEnvelope(body)
		if err != nil {
			stats.Failed++
			continue
		}
		occurred, err := event.OccurredEpoch()
		if err != nil {
			stats.Failed++
			continue
		}
		received := event.ReceivedAtEpoch
		if received == 0 {
			// Envelope predates the received_at stamp: the raw file's mtime is
			// the drainer's copy time — the faithful approximation. A
			// replay-time now() would shift the event's received_at by the
			// full corpus age (found live: 1 event in 196,720; smoke envelope
			// from before the stamp existed).
			if info, statErr := os.Stat(path); statErr == nil {
				received = info.ModTime().Unix()
			} else {
				received = time.Now().Unix()
			}
		}
		payload := event.Payload
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}
		meta := event.Metadata
		if len(meta) == 0 {
			meta = []byte(`{}`)
		}
		text := db.IngestText(event, payload)
		key := event.IdempotencyKey(payload)
		if delta {
			_, keyKnown := present[key]
			_, sessionKnown := knownSessions[event.SessionID]
			if keyKnown && sessionKnown {
				stats.Skipped++
				continue
			}
		}
		principal := event.PrincipalID
		if principal == "" {
			principal = authctx.PrincipalLocal
		}

		tx, err := pg.BeginTx(ctx, nil)
		if err != nil {
			return stats, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions(session_id, project, client, started_at_epoch, principal_id, meta_json)
			 VALUES ($1,$2,$3,$4,$5,'{}') ON CONFLICT (session_id) DO NOTHING`,
			event.SessionID, event.Project, event.SourceAdapter, occurred, principal); err != nil {
			_ = tx.Rollback()
			return stats, fmt.Errorf("replay %s sessions: %w", name, err)
		}
		var id int64
		err = tx.QueryRowContext(ctx,
			`INSERT INTO events(
				idempotency_key, session_id, content_session_id, project, source_adapter, source_event_id,
				event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch,
				principal_id, credential_id, request_id, meta_json
			 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULL,$14,$15)
			 ON CONFLICT (idempotency_key) DO NOTHING
			 RETURNING id`,
			key, event.SessionID, nullIfEmpty(event.ContentSessionID), event.Project, event.SourceAdapter,
			nullIfEmpty(event.SourceEventID), event.EventType, nullIfEmpty(event.CWD), string(payload),
			nullIfEmpty(text), occurred, received, principal, nullIfEmpty(event.RequestID), string(meta)).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			stats.Duplicates++
			if err := tx.Commit(); err != nil {
				return stats, err
			}
			continue
		case err != nil:
			_ = tx.Rollback()
			return stats, fmt.Errorf("replay %s events: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO observations(
				event_id, project, session_id, kind, text, source_adapter, event_type,
				occurred_at_epoch, created_at_epoch, principal_id, meta_json, provenance
			 ) VALUES ($1,$2,$3,'raw',$4,$5,$6,$7,$8,$9,'{}','observed')
			 ON CONFLICT DO NOTHING`,
			id, event.Project, event.SessionID, coalesce(text, ""), event.SourceAdapter, event.EventType,
			occurred, time.Now().Unix(), principal); err != nil {
			_ = tx.Rollback()
			return stats, fmt.Errorf("replay %s observations: %w", name, err)
		}
		// Drain-equivalence for the tail (round-1 finding 4): Store.Ingest also
		// audits the ingest and maintains project identity state; a cutover
		// tail replayed without them would leave the hub missing both.
		// resource_id is TEXT; format the id in Go — pgx (correctly) refuses
		// an implicit int64->text parameter encode on PG 18, and a server-side
		// $n::text cast doesn't change the parameter's wire type.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audit_log(principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch)
			 VALUES ($1, NULL, $2, 'event.ingest', 'event', $3, '{}', $4)`,
			principal, nullIfEmpty(event.RequestID), strconv.FormatInt(id, 10), received); err != nil {
			_ = tx.Rollback()
			return stats, fmt.Errorf("replay %s audit: %w", name, err)
		}
		if err := recordProjectRootPG(ctx, tx, event.Project, payload, received); err != nil {
			_ = tx.Rollback()
			return stats, fmt.Errorf("replay %s identity: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return stats, err
		}
		stats.Ingested++
	}
	return stats, nil
}

// recordProjectRootPG is the Postgres port of store.recordProjectRootTx
// (liaison:D239 identity tracking) with identical rules: refresh known roots,
// learn first/registry roots silently, alert on new roots from other
// sources. Advisory — malformed payloads never block replay.
func recordProjectRootPG(ctx context.Context, tx *sql.Tx, project string, payload []byte, now int64) error {
	var ident struct {
		Root   string `json:"memoire_project_root"`
		Source string `json:"memoire_project_source"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &ident) != nil {
		return nil
	}
	root := strings.TrimSpace(ident.Root)
	if project == "" || root == "" {
		return nil
	}
	source := strings.TrimSpace(ident.Source)
	if source == "" {
		source = "unknown"
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE project_roots SET last_seen_epoch = $1 WHERE project = $2 AND root = $3`, now, project, root)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var knownRoot string
	// root tie-break under COLLATE "C" = SQLite BINARY order (round-2 N2):
	// same-epoch ties must pick the same known_root on both engines.
	err = tx.QueryRowContext(ctx,
		`SELECT root FROM project_roots WHERE project = $1 ORDER BY first_seen_epoch ASC, root COLLATE "C" ASC LIMIT 1`,
		project).Scan(&knownRoot)
	if err == sql.ErrNoRows || (err == nil && source == "registry") {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO project_roots(project, root, source, first_seen_epoch, last_seen_epoch)
			 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (project, root) DO NOTHING`,
			project, root, source, now, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM project_identity_alerts WHERE project = $1 AND root = $2`, project, root)
		return err
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO project_identity_alerts(project, root, known_root, source, first_seen_epoch, last_seen_epoch, event_count)
		 VALUES ($1,$2,$3,$4,$5,$6,1)
		 ON CONFLICT (project, root) DO UPDATE SET
		   last_seen_epoch = EXCLUDED.last_seen_epoch,
		   event_count = project_identity_alerts.event_count + 1`,
		project, root, knownRoot, source, now, now)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func coalesce(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Copy-lane checks are FULL-COLUMN direct projections INCLUDING ids: the
// copy preserves row ids, so nothing is projected away and orphan rows
// (dangling event_id etc.) are covered by construction (round-1 findings
// 1+2). Replay-lane checks are ID-FREE natural-key joins (fresh identities
// differ from the live store's purge-gapped ids) restricted to the
// replay-covered surfaces, with created_at/meta excluded where the replay
// stamps them; an explicit orphan probe covers what the joins cannot see.
type parityCheck struct {
	name    string
	sqliteQ string
	pgQ     string // empty = same as sqliteQ
}

var copyChecks = []parityCheck{
	// Every NULLABLE column is projected as an (IS NULL, COALESCE) pair:
	// formatVal can distinguish NULL from empty, but a COALESCE sentinel
	// erases the distinction in SQL first — NULL confidence must not hash
	// equal to a real sentinel value (round-2 finding N1). SQLite yields
	// 0/1 for IS NULL, Postgres yields boolean; formatVal maps both to
	// "0"/"1".
	{name: "events", sqliteQ: `SELECT id, idempotency_key, session_id,
		content_session_id IS NULL, COALESCE(content_session_id,''), project,
		source_adapter, source_event_id IS NULL, COALESCE(source_event_id,''), event_type,
		cwd IS NULL, COALESCE(cwd,''), payload_json, text IS NULL, COALESCE(text,''),
		occurred_at_epoch, received_at_epoch, deleted_at_epoch IS NULL, COALESCE(deleted_at_epoch,0),
		principal_id IS NULL, COALESCE(principal_id,''), credential_id IS NULL, COALESCE(credential_id,''),
		request_id IS NULL, COALESCE(request_id,''), meta_json IS NULL, COALESCE(meta_json,'')
		FROM events`},
	{name: "observations", sqliteQ: `SELECT id, event_id IS NULL, COALESCE(event_id,0), project,
		session_id IS NULL, COALESCE(session_id,''), kind, text,
		source_adapter IS NULL, COALESCE(source_adapter,''), event_type IS NULL, COALESCE(event_type,''),
		occurred_at_epoch, created_at_epoch, deleted_at_epoch IS NULL, COALESCE(deleted_at_epoch,0),
		principal_id IS NULL, COALESCE(principal_id,''), supersedes_id IS NULL, COALESCE(supersedes_id,0),
		ttl_review_epoch IS NULL, COALESCE(ttl_review_epoch,0), confidence IS NULL, COALESCE(confidence,0),
		meta_json IS NULL, COALESCE(meta_json,''), review_state,
		derivation_run_id IS NULL, COALESCE(derivation_run_id,''), content_hash IS NULL, COALESCE(content_hash,''),
		provenance IS NULL, COALESCE(provenance,''),
		can_instruct, can_evidence, needs_confirmation, noise_candidate
		FROM observations`},
	{name: "sessions", sqliteQ: `SELECT session_id, project, client, started_at_epoch,
		ended_at_epoch IS NULL, COALESCE(ended_at_epoch,0),
		principal_id IS NULL, COALESCE(principal_id,''), meta_json IS NULL, COALESCE(meta_json,'')
		FROM sessions`},
	{name: "credentials", sqliteQ: `SELECT id, token_hash, prefix, label IS NULL, COALESCE(label,''),
		scopes_json, revoked_at_epoch IS NULL, COALESCE(revoked_at_epoch,0), created_at_epoch,
		last_used_at_epoch IS NULL, COALESCE(last_used_at_epoch,0) FROM credentials`},
	{name: "audit_log", sqliteQ: `SELECT id, principal_id IS NULL, COALESCE(principal_id,''),
		credential_id IS NULL, COALESCE(credential_id,''), request_id IS NULL, COALESCE(request_id,''),
		action, resource_type IS NULL, COALESCE(resource_type,''), resource_id IS NULL, COALESCE(resource_id,''),
		details_json IS NULL, COALESCE(details_json,''), created_at_epoch FROM audit_log`},
	{name: "derivation_runs", sqliteQ: `SELECT id, project, session_id IS NULL, COALESCE(session_id,''),
		provider, data_boundary, derivation_version, prompt_version IS NULL, COALESCE(prompt_version,''),
		input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch,
		completed_at_epoch IS NULL, COALESCE(completed_at_epoch,0), meta_json IS NULL, COALESCE(meta_json,''),
		model IS NULL, COALESCE(model,''), endpoint_id IS NULL, COALESCE(endpoint_id,''),
		created_at_ns IS NULL, COALESCE(created_at_ns,0),
		snapshot_max_event_id IS NULL, COALESCE(snapshot_max_event_id,0) FROM derivation_runs`},
	{name: "observation_source_events", sqliteQ: `SELECT observation_id, event_id, created_at_epoch
		FROM observation_source_events`},
	{name: "observation_review_log", sqliteQ: `SELECT id, observation_id, from_state, to_state, actor,
		reason_text IS NULL, COALESCE(reason_text,''), occurred_at_epoch,
		action IS NULL, COALESCE(action,'') FROM observation_review_log`},
	{name: "project_roots", sqliteQ: `SELECT project, root, source, first_seen_epoch, last_seen_epoch
		FROM project_roots`},
	{name: "project_identity_alerts", sqliteQ: `SELECT project, root, known_root, source, first_seen_epoch,
		last_seen_epoch, event_count FROM project_identity_alerts`},
	{name: "derive_intents", sqliteQ: `SELECT project, session_id, first_notified_epoch, last_notified_epoch,
		notify_count, completed_at_epoch IS NULL, COALESCE(completed_at_epoch,0),
		completed_run_id IS NULL, COALESCE(completed_run_id,''),
		latency_seconds IS NULL, COALESCE(latency_seconds,0) FROM derive_intents`},
	{name: "run_input_events", sqliteQ: `SELECT run_id, event_id FROM run_input_events`},
}

var replayChecks = []parityCheck{
	{name: "events (id-free)", sqliteQ: `SELECT idempotency_key, project, session_id,
		content_session_id IS NULL, COALESCE(content_session_id,''), source_adapter,
		source_event_id IS NULL, COALESCE(source_event_id,''), event_type,
		cwd IS NULL, COALESCE(cwd,''), payload_json, text IS NULL, COALESCE(text,''),
		occurred_at_epoch, received_at_epoch, principal_id IS NULL, COALESCE(principal_id,''),
		request_id IS NULL, COALESCE(request_id,''), meta_json IS NULL, COALESCE(meta_json,'')
		FROM events`},
	{name: "raw observations (id-free)", sqliteQ: `SELECT e.idempotency_key, o.kind, o.project,
		o.session_id IS NULL, COALESCE(o.session_id,''), o.text,
		o.source_adapter IS NULL, COALESCE(o.source_adapter,''),
		o.event_type IS NULL, COALESCE(o.event_type,''),
		o.occurred_at_epoch, o.review_state, o.provenance IS NULL, COALESCE(o.provenance,''),
		o.principal_id IS NULL, COALESCE(o.principal_id,''),
		o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate
		FROM observations o JOIN events e ON e.id = o.event_id`},
	{name: "sessions", sqliteQ: `SELECT session_id, project, client, started_at_epoch,
		principal_id IS NULL, COALESCE(principal_id,''), meta_json IS NULL, COALESCE(meta_json,'')
		FROM sessions`},
}

// orphanProbes catch rows the replay-lane joins cannot see (finding 2):
// counted on both engines and reported; any nonzero side is surfaced.
var orphanProbes = []parityCheck{
	{name: "orphan event-linked observations", sqliteQ: `SELECT count(*) FROM observations o
		WHERE o.event_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM events e WHERE e.id = o.event_id)`},
	{name: "orphan source links", sqliteQ: `SELECT count(*) FROM observation_source_events l
		WHERE NOT EXISTS (SELECT 1 FROM observations o WHERE o.id = l.observation_id)
		   OR NOT EXISTS (SELECT 1 FROM events e WHERE e.id = l.event_id)`},
	{name: "orphan review-log rows", sqliteQ: `SELECT count(*) FROM observation_review_log l
		WHERE NOT EXISTS (SELECT 1 FROM observations o WHERE o.id = l.observation_id)`},
	// Events whose session row is missing: no FK declares the invariant, and
	// the delta-replay skip set assumes it (round-1 finding 1) — surface any
	// violation on both engines.
	{name: "events without sessions", sqliteQ: `SELECT count(*) FROM events e
		WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.session_id = e.session_id)`},
}

// hashRows runs the projection and folds every row into an
// ORDER-INDEPENDENT multiset hash: each row is sha256'd individually and
// the digests are summed mod 2^256. SQLite's BINARY collation and
// Postgres's locale collation order TEXT differently (paths, JSON
// punctuation), so an ordered running hash would diff on sort order alone;
// a digest sum is blind to order while duplicates still accumulate
// (unlike XOR, where identical pairs cancel).
func hashRows(ctx context.Context, conn *sql.DB, query string) (int64, string, error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, "", err
	}
	var sum [sha256.Size]byte
	var count int64
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return count, "", err
		}
		h := sha256.New()
		for i, v := range vals {
			if i > 0 {
				io.WriteString(h, "\x1f")
			}
			io.WriteString(h, formatVal(v))
		}
		addDigest(&sum, h.Sum(nil))
		count++
	}
	return count, hex.EncodeToString(sum[:]), rows.Err()
}

// addDigest adds d into sum as a big-endian integer mod 2^256.
func addDigest(sum *[sha256.Size]byte, d []byte) {
	carry := 0
	for i := sha256.Size - 1; i >= 0; i-- {
		v := int(sum[i]) + int(d[i]) + carry
		sum[i] = byte(v & 0xff)
		carry = v >> 8
	}
}

func formatVal(v any) string {
	switch t := v.(type) {
	case nil:
		return "\x00"
	case []byte:
		return string(t)
	case string:
		return t
	case int64:
		return fmt.Sprintf("%d", t)
	case float64:
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case time.Time:
		return fmt.Sprintf("%d", t.Unix())
	default:
		return fmt.Sprintf("%v", t)
	}
}

type DiffOptions struct {
	Project    string   // timeline/search filter; empty = auto-pick busiest
	Probes     []string // search probe queries
	ReplayLane bool     // restrict hash checks to replay-covered surfaces
}

var defaultProbes = []string{
	"purge generation custody",
	"review queue latency",
	"redaction contract violation",
	"forwarder custody ack",
	"clock skew alert",
}

// Diff writes the parity report and returns overall pass/fail. Search
// ranking deltas are reported against the journaled thresholds
// (overlap@10 >= 7, top1 in other engine's top3) but graded per the
// design doc: counts/hashes and timeline are hard PASS/FAIL, search is
// PASS/WARN (deltas expected, measured not solved).
func Diff(ctx context.Context, pg *sql.DB, lite *sql.DB, opts DiffOptions, out io.Writer) (bool, error) {
	pass := true
	fmt.Fprintf(out, "# pgshadow parity report — %s\n\n", time.Now().UTC().Format(time.RFC3339))

	project := opts.Project
	if project == "" {
		_ = lite.QueryRowContext(ctx,
			`SELECT project FROM events GROUP BY project ORDER BY count(*) DESC LIMIT 1`).Scan(&project)
	}
	fmt.Fprintf(out, "Probe project: `%s`; lane: %s\n\n", project, laneName(opts.ReplayLane))

	checks := copyChecks
	if opts.ReplayLane {
		checks = replayChecks
	}
	fmt.Fprintf(out, "## Table parity (count + multiset content hash)\n\n")
	fmt.Fprintf(out, "| check | sqlite rows | pg rows | hashes | verdict |\n|---|---|---|---|---|\n")
	for _, c := range checks {
		pgQ := c.pgQ
		if pgQ == "" {
			pgQ = c.sqliteQ
		}
		ln, lh, err := hashRows(ctx, lite, c.sqliteQ)
		if err != nil {
			return false, fmt.Errorf("sqlite %s: %w", c.name, err)
		}
		pn, ph, err := hashRows(ctx, pg, pgQ)
		if err != nil {
			return false, fmt.Errorf("pg %s: %w", c.name, err)
		}
		verdict := "PASS"
		if ln != pn || lh != ph {
			verdict = "FAIL"
			pass = false
		}
		fmt.Fprintf(out, "| %s | %d | %d | %s / %s | %s |\n", c.name, ln, pn, lh[:12], ph[:12], verdict)
	}

	fmt.Fprintf(out, "\n## Orphan probes (rows invisible to join-based checks)\n\n")
	if opts.ReplayLane {
		fmt.Fprintf(out, "(informational in replay lane: a fresh replay cannot reproduce live orphans)\n")
	}
	for _, c := range orphanProbes {
		lr, err := querySet(ctx, lite, c.sqliteQ)
		if err != nil {
			return false, fmt.Errorf("sqlite %s: %w", c.name, err)
		}
		pr, err := querySet(ctx, pg, c.sqliteQ)
		if err != nil {
			return false, fmt.Errorf("pg %s: %w", c.name, err)
		}
		same := len(lr) == 1 && len(pr) == 1 && lr[0] == pr[0]
		if !same && !opts.ReplayLane {
			pass = false
		}
		fmt.Fprintf(out, "- %s: sqlite=%s pg=%s %s\n", c.name, first(lr), first(pr),
			map[bool]string{true: "PASS", false: "FAIL"}[same])
	}

	fmt.Fprintf(out, "\n## Timeline parity (inject read path; exact row-set equality)\n\n")
	tlQ := `SELECT project, kind, text, occurred_at_epoch FROM observations
		WHERE deleted_at_epoch IS NULL AND review_state = 'accepted' AND noise_candidate = 0
		AND project = $1 ORDER BY occurred_at_epoch DESC, id DESC LIMIT 50`
	liteRows, err := querySet(ctx, lite, strings.ReplaceAll(tlQ, "$1", "?"), project)
	if err != nil {
		return false, fmt.Errorf("sqlite timeline: %w", err)
	}
	pgRows, err := querySet(ctx, pg, tlQ, project)
	if err != nil {
		return false, fmt.Errorf("pg timeline: %w", err)
	}
	if setsEqual(liteRows, pgRows) {
		fmt.Fprintf(out, "PASS — %d rows, identical sets.\n", len(liteRows))
	} else {
		pass = false
		fmt.Fprintf(out, "FAIL — sqlite %d rows vs pg %d rows; first sqlite-only: %.120s; first pg-only: %.120s\n",
			len(liteRows), len(pgRows), firstOnly(liteRows, pgRows), firstOnly(pgRows, liteRows))
	}

	fmt.Fprintf(out, "\n## Search ranking parity (bm25 vs ts_rank_cd — deltas measured, thresholds: overlap@10 >= 7, top1 in top3)\n\n")
	probes := opts.Probes
	if len(probes) == 0 {
		probes = defaultProbes
	}
	fmt.Fprintf(out, "| probe | overlap@10 | top1-in-top3 | verdict |\n|---|---|---|---|\n")
	for _, probe := range probes {
		liteTop, err := querySet(ctx, lite,
			`SELECT o.text FROM observations_fts f JOIN observations o ON o.id = f.rowid
			 WHERE observations_fts MATCH ? AND o.deleted_at_epoch IS NULL
			   AND o.review_state = 'accepted' AND o.noise_candidate = 0
			 ORDER BY rank LIMIT 10`, db.SanitizeFTS(probe))
		if err != nil {
			return false, fmt.Errorf("sqlite search %q: %w", probe, err)
		}
		// SanitizeFTS joins quoted tokens with OR (store.go): the PG probe
		// must use the SAME candidate-set semantics or the comparison mixes
		// matching with ranking (round-1 finding 3). tsquery syntax: token
		// quotes become single quotes, OR becomes |.
		pgQuery := strings.ReplaceAll(strings.ReplaceAll(db.SanitizeFTS(probe), `"`, `'`), " OR ", " | ")
		pgTop, err := querySet(ctx, pg,
			`SELECT o.text FROM observations o
			 WHERE o.text_tsv @@ to_tsquery('simple', $1) AND o.deleted_at_epoch IS NULL
			   AND o.review_state = 'accepted' AND o.noise_candidate = 0
			 ORDER BY ts_rank_cd(o.text_tsv, to_tsquery('simple', $1)) DESC, o.id DESC
			 LIMIT 10`, pgQuery)
		if err != nil {
			return false, fmt.Errorf("pg search %q: %w", probe, err)
		}
		overlap := overlapCount(liteTop, pgTop)
		top1 := len(liteTop) == 0 || indexOf(pgTop, liteTop[0]) >= 0 && indexOf(pgTop, liteTop[0]) < 3
		verdict := "PASS"
		if (len(liteTop) >= 10 && overlap < 7) || !top1 {
			verdict = "WARN"
		}
		fmt.Fprintf(out, "| %.40s | %d/%d | %v | %s |\n", probe, overlap, max(len(liteTop), len(pgTop)), top1, verdict)
	}

	fmt.Fprintf(out, "\n## Postgres EXPLAIN spot checks (informational)\n\n")
	for name, q := range map[string]string{
		"idx_events_future_skew": `EXPLAIN SELECT count(*) FROM events WHERE occurred_at_epoch > received_at_epoch AND occurred_at_epoch > received_at_epoch + 60`,
		"idx_events_redaction_violation": `EXPLAIN SELECT count(*) FROM events WHERE (meta_json::jsonb -> 'redaction_contract_violation') IN ('true'::jsonb, '1'::jsonb)`,
	} {
		plan, err := querySet(ctx, pg, q)
		if err != nil {
			return false, err
		}
		uses := strings.Contains(strings.Join(plan, "\n"), name)
		fmt.Fprintf(out, "- %s used: %v (small tables may legitimately seq-scan)\n", name, uses)
	}

	fmt.Fprintf(out, "\n## Overall: %s\n", map[bool]string{true: "PASS", false: "FAIL"}[pass])
	return pass, nil
}

func first(list []string) string {
	if len(list) == 0 {
		return "(none)"
	}
	return list[0]
}

func laneName(replay bool) string {
	if replay {
		return "replay (events/raw surfaces only)"
	}
	return "copy (all tables)"
}

func querySet(ctx context.Context, conn *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = formatVal(v)
		}
		out = append(out, strings.Join(parts, "\x1f"))
	}
	return out, rows.Err()
}

func setsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

func firstOnly(a, b []string) string {
	seen := map[string]bool{}
	for _, s := range b {
		seen[s] = true
	}
	for _, s := range a {
		if !seen[s] {
			return s
		}
	}
	return ""
}

func overlapCount(a, b []string) int {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	n := 0
	for _, s := range b {
		if seen[s] {
			n++
		}
	}
	return n
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
