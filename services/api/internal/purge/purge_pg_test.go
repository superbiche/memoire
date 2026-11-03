// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package purge

// Postgres-engine purge contract (Fibery #616 slice B). Runs only with
// MEMOIRE_TEST_PG_URL set — the repo's PG test convention (pgshadow and db
// tests): an admin DSN with CREATEDB, here the local pg-shadow container
// (`make test-pg`, which brings it up via scripts/pg-shadow.sh, image
// docker.io/paradedb/paradedb:18-v0.24.3). Each test creates and drops a
// throwaway database and applies schema-pg through pgshadow.Apply (the
// hub's own deployment step, so the vector lane table exists exactly as on
// the hub). Files ride the SQLite fixture's home (newFixture); only the
// store is swapped to the Postgres DSN.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"memoire/services/api/internal/pgshadow"
)

type pgFixture struct {
	fixture
	pg *sql.DB
}

func newPGFixture(t *testing.T) pgFixture {
	t.Helper()
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres purge leg skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL unparseable: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_purge_" + hex.EncodeToString(suffix[:])
	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	// C collation: the store contract (D0249), same as the hub.
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName + " TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create fixture database (role needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		_ = adminConn.Close()
	})
	u.Path = "/" + dbName
	dsn := u.String()
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	ctx := context.Background()
	if err := pgshadow.Apply(ctx, pg, "../../../../schema-pg"); err != nil {
		t.Fatalf("apply schema-pg: %v", err)
	}

	stmts := []string{
		// Same rows as the SQLite fixture (newFixture)…
		`INSERT INTO sessions(session_id, project, client, started_at_epoch) VALUES ('s1', 'purgeme', 'cc', 100)`,
		`INSERT INTO events(id, idempotency_key, source_event_id, session_id, project, source_adapter, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch)
		 VALUES (1, 'k1', 'k1', 's1', 'purgeme', 'claude-code', 'note', '/x', '{}', 'purgeme secret alpha', 100, 100)`,
		`INSERT INTO events(id, idempotency_key, source_event_id, session_id, project, source_adapter, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch)
		 VALUES (2, 'k2', 'k2', 's1', 'purgeme', 'claude-code', 'note', '/x', '{}', 'purgeme secret beta', 100, 100)`,
		`INSERT INTO observations(id, project, session_id, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance, event_id)
		 VALUES (10, 'purgeme', 's1', 'raw', 'purgeme raw alpha', 100, 100, 'accepted', 'observed', 1)`,
		`INSERT INTO observations(id, project, session_id, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance)
		 VALUES (11, 'purgeme', 's1', 'fact', 'purgeme derived from alpha', 100, 100, 'proposed', 'inferred')`,
		`INSERT INTO observation_source_events(observation_id, event_id, created_at_epoch) VALUES (11, 1, 100)`,
		`INSERT INTO observation_review_log(observation_id, from_state, to_state, actor, occurred_at_epoch)
		 VALUES (11, 'proposed', 'proposed', 'op', 100)`,
		`INSERT INTO derivation_runs(id, project, session_id, provider, data_boundary, derivation_version, input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch)
		 VALUES ('run1', 'purgeme', 's1', 'manual', 'local', 'v1', 'h1', 90, 110, 'completed', 100)`,
		`INSERT INTO project_roots(project, root, source, first_seen_epoch, last_seen_epoch)
		 VALUES ('purgeme', '/home/x/purgeme', 'event', 100, 100)`,
		`INSERT INTO project_identity_alerts(project, root, known_root, source, first_seen_epoch, last_seen_epoch)
		 VALUES ('purgeme', '/other/root', '/home/x/purgeme', 'basename', 100, 100)`,
		`INSERT INTO derive_intents(project, session_id, first_notified_epoch, last_notified_epoch, notify_count)
		 VALUES ('purgeme', 's1', 100, 100, 1)`,
		`INSERT INTO derive_intents(project, session_id, first_notified_epoch, last_notified_epoch, notify_count)
		 VALUES ('keep', 's2', 100, 100, 1)`,
		`INSERT INTO run_input_events(run_id, event_id) VALUES ('run1', 1)`,
		`INSERT INTO sessions(session_id, project, client, started_at_epoch) VALUES ('s2', 'keep', 'cc', 100)`,
		`INSERT INTO events(id, idempotency_key, source_event_id, session_id, project, source_adapter, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch)
		 VALUES (3, 'k3', 'k3', 's2', 'keep', 'claude-code', 'note', '/x', '{}', 'keep gamma text', 100, 100)`,
		`INSERT INTO observations(id, project, session_id, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance, supersedes_id)
		 VALUES (12, 'keep', 's2', 'fact', 'keep obs gamma', 100, 100, 'accepted', 'inferred', 11)`,
		// …plus the Postgres-only children: a triage suggestion and a usage
		// row on the purged derived observation, a usage row whose usage
		// EVENT is purged (observation survives), a survivor usage row, and
		// vector-lane rows for a purged and a surviving observation.
		`INSERT INTO observation_triage_suggestions(observation_id, stage, prompt_version, model, actor, suggested_verdict, reason_text, created_at_epoch)
		 VALUES (11, 1, 'p1', 'm', 'triage', 'accept', 'purgeme reason text', 100)`,
		`INSERT INTO observation_triage_suggestions(observation_id, stage, prompt_version, model, actor, suggested_verdict, created_at_epoch)
		 VALUES (12, 1, 'p1', 'm', 'triage', 'accept', 100)`,
		`INSERT INTO observation_usage(observation_id, event_id, session_id, project, verdict, reason, occurred_at_epoch)
		 VALUES (11, 3, 's2', 'keep', 'used', 'quoted purgeme secret', 100)`,
		`INSERT INTO observation_usage(observation_id, event_id, session_id, project, verdict, occurred_at_epoch)
		 VALUES (12, 2, 's1', 'purgeme', 'used', 100)`,
		`INSERT INTO observation_usage(observation_id, event_id, session_id, project, verdict, occurred_at_epoch)
		 VALUES (12, 3, 's2', 'keep', 'unused', 100)`,
	}
	for _, stmt := range stmts {
		if _, err := pg.Exec(stmt); err != nil {
			t.Fatalf("fixture stmt failed: %v\n%s", err, stmt)
		}
	}
	// schema-pg/0001 requires pgvector, so pgshadow.Apply always provisions
	// the vector lane (cascading FK included) — the explicit-delete path is
	// exercised unconditionally.
	if !pgTableExists(t, pg, "observation_embeddings_pg") {
		t.Fatal("pgshadow.Apply did not provision observation_embeddings_pg")
	}
	for _, id := range []int{11, 12} {
		if _, err := pg.Exec(`INSERT INTO observation_embeddings_pg(observation_id, model, dim, embedding, text_hash, embedded_at_epoch)
			VALUES ($1, 'm', 384, array_fill(0.1::real, ARRAY[384])::vector, 'h', 100)`, id); err != nil {
			t.Fatalf("embedding fixture: %v", err)
		}
	}

	fx := newFixture(t)
	fx.opts.DBPath = dsn
	return pgFixture{fixture: fx, pg: pg}
}

func pgTableExists(t *testing.T, pg *sql.DB, name string) bool {
	t.Helper()
	var ok bool
	if err := pg.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func pgCount(t *testing.T, pg *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestPGDryRunCounts(t *testing.T) {
	fx := newPGFixture(t)
	fx.opts.Selector = Selector{Project: "purgeme"}
	fx.opts.Vacuum = true

	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.Engine != "postgres" {
		t.Fatalf("want postgres dry-run, got dry_run=%v engine=%q", report.DryRun, report.Engine)
	}
	if report.EventsMatched != 2 || report.ObservationsMatched != 2 {
		t.Fatalf("matched events=%d obs=%d, want 2/2", report.EventsMatched, report.ObservationsMatched)
	}
	if report.RawFilesMatched != 2 || report.PendingFilesMatched != 1 || report.InflightFilesMatched != 1 || report.FailedFilesMatched != 2 {
		t.Fatalf("file matches raw=%d pending=%d inflight=%d failed=%d, want 2/1/1/2",
			report.RawFilesMatched, report.PendingFilesMatched, report.InflightFilesMatched, report.FailedFilesMatched)
	}
	if report.DBBytesBefore <= 0 {
		t.Fatalf("db_bytes_before=%d, want pg_database_size", report.DBBytesBefore)
	}
	if got := pgCount(t, fx.pg, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("dry-run mutated events: %d", got)
	}
	for _, path := range []string{fx.opts.IntentPath, fx.opts.ManifestPath} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("dry-run created %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "e1.json")); err != nil {
		t.Fatal("dry-run removed a raw file")
	}
}

func TestPGProjectApplyCascadeQuarantineAndConvergence(t *testing.T) {
	requireAge(t)
	fx := newPGFixture(t)
	pub, _ := ageKeypair(t)
	fx.opts.Selector = Selector{Project: "purgeme"}
	fx.opts.Apply = true
	fx.opts.AgeRecipient = pub
	fx.opts.QuarantineOut = filepath.Join(fx.home, "q.tar.age")
	fx.opts.Vacuum = true

	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		name      string
		got, want int64
	}
	for _, w := range []want{
		{"events", report.EventsDeleted, 2},
		{"observations", report.ObservationsDeleted, 2},
		{"source_links", report.SourceLinksDeleted, 1},
		{"review_log", report.ReviewLogDeleted, 1},
		{"supersedes_nulled", report.SupersedesNulled, 1},
		{"sessions", report.SessionsDeleted, 1},
		{"derivation_runs", report.DerivationRunsDeleted, 1},
		{"identity_roots", report.IdentityRootsDeleted, 1},
		{"identity_alerts", report.IdentityAlertsDeleted, 1},
		{"derive_intents", report.DeriveIntentsDeleted, 1},
		{"run_manifest_rows", report.RunManifestRowsDeleted, 1},
		// obs 11's row (by observation) + the row whose usage event 2 is
		// purged (by event); the survivor row stays.
		{"usage_rows", report.UsageRowsDeleted, 2},
		{"triage_suggestions", report.TriageSuggestionsDeleted, 1},
	} {
		if w.got != w.want {
			t.Errorf("%s deleted=%d, want %d", w.name, w.got, w.want)
		}
	}
	if report.EmbeddingsDeleted != 1 {
		t.Errorf("embeddings deleted=%d, want 1", report.EmbeddingsDeleted)
	}
	if report.Engine != "postgres" || report.Vacuumed || !strings.Contains(report.VacuumSkippedReason, "postgres") {
		t.Fatalf("vacuum must be a reported no-op on postgres: engine=%q vacuumed=%v reason=%q", report.Engine, report.Vacuumed, report.VacuumSkippedReason)
	}
	if report.QuarantineMembers != 6 {
		t.Fatalf("quarantine members=%d, want 6", report.QuarantineMembers)
	}
	if st, err := os.Stat(fx.opts.QuarantineOut); err != nil || st.Size() == 0 {
		t.Fatalf("quarantine archive missing/empty: %v", err)
	}
	if report.Generation != 1 {
		t.Fatalf("generation=%d, want 1", report.Generation)
	}

	for q, n := range map[string]int{
		`SELECT count(*) FROM events`:                                                         1,
		`SELECT count(*) FROM observations`:                                                   1,
		`SELECT count(*) FROM observations WHERE supersedes_id IS NOT NULL`:                   0,
		`SELECT count(*) FROM observations WHERE text_tsv @@ to_tsquery('simple', 'purgeme')`: 0,
		`SELECT count(*) FROM observations WHERE text_tsv @@ to_tsquery('simple', 'gamma')`:   1,
		`SELECT count(*) FROM run_input_events`:                                               0,
		`SELECT count(*) FROM derive_intents`:                                                 1,
		`SELECT count(*) FROM observation_usage`:                                              1,
		`SELECT count(*) FROM observation_triage_suggestions`:                                 1,
		`SELECT count(*) FROM audit_log WHERE action = 'purge.apply'`:                         1,
	} {
		if got := pgCount(t, fx.pg, q); got != n {
			t.Errorf("%s = %d, want %d", q, got, n)
		}
	}
	if got := pgCount(t, fx.pg, `SELECT count(*) FROM observation_embeddings_pg`); got != 1 {
		t.Errorf("surviving embeddings=%d, want 1", got)
	}
	var details string
	if err := fx.pg.QueryRow(`SELECT details_json FROM audit_log WHERE action = 'purge.apply'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var stamp map[string]any
	if err := json.Unmarshal([]byte(details), &stamp); err != nil || stamp["selector"] != "project" {
		t.Fatalf("tombstone details=%q (counts + selector only): %v", details, err)
	}
	if strings.Contains(details, "purgeme") {
		t.Fatalf("tombstone leaks content: %s", details)
	}
	for _, gone := range []string{
		filepath.Join(fx.opts.RawDir, "e1.json"),
		filepath.Join(fx.opts.RawDir, "e2.json"),
		filepath.Join(fx.opts.PendingDir, "p1.json"),
		filepath.Join(fx.opts.InflightDir, "i1.json"),
		filepath.Join(fx.opts.FailedDir, "f1.json"),
		filepath.Join(fx.opts.FailedDir, "f1.json.error.json"),
		fx.opts.IntentPath,
	} {
		if _, err := os.Stat(gone); err == nil {
			t.Fatalf("%s should be gone", gone)
		}
	}
	for _, kept := range []string{
		filepath.Join(fx.opts.RawDir, "keep.json"),
		filepath.Join(fx.opts.RawDir, "garbled.json"),
		filepath.Join(fx.opts.FailedDir, "fkeep.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s should survive: %v", kept, err)
		}
	}

	// Convergence: a second apply is zero-work — no tombstone, no bump.
	fx.opts.NoQuarantine = true
	fx.opts.QuarantineOut = ""
	second, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.dbMutations() != 0 || second.RawFilesDeleted != 0 {
		t.Fatalf("second run not convergent: %+v", second)
	}
	if second.Generation != 1 {
		t.Fatalf("zero-work apply bumped generation to %d", second.Generation)
	}
	if got := pgCount(t, fx.pg, `SELECT count(*) FROM audit_log WHERE action = 'purge.apply'`); got != 1 {
		t.Fatalf("convergent re-apply minted a tombstone: %d", got)
	}
}

func TestPGEventIDSelector(t *testing.T) {
	fx := newPGFixture(t)
	fx.opts.Selector = Selector{EventIDs: []int64{1}}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true

	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	// Event 1, its raw projection (10) and the source-linked derived obs
	// (11) go; sessions and derivation_runs stay (idempotency watermarks).
	if report.EventsDeleted != 1 || report.ObservationsDeleted != 2 {
		t.Fatalf("deleted events=%d obs=%d, want 1/2", report.EventsDeleted, report.ObservationsDeleted)
	}
	if report.SessionsDeleted != 0 || report.DerivationRunsDeleted != 0 {
		t.Fatalf("id selector touched sessions=%d runs=%d", report.SessionsDeleted, report.DerivationRunsDeleted)
	}
	if report.UsageRowsDeleted != 1 || report.TriageSuggestionsDeleted != 1 {
		t.Fatalf("usage=%d triage=%d, want 1/1", report.UsageRowsDeleted, report.TriageSuggestionsDeleted)
	}
	if report.EmbeddingsDeleted != 1 {
		t.Fatalf("embeddings deleted=%d, want 1", report.EmbeddingsDeleted)
	}
	if report.RawFilesDeleted != 1 {
		t.Fatalf("raw files deleted=%d, want 1 (e1.json)", report.RawFilesDeleted)
	}
	if got := pgCount(t, fx.pg, `SELECT count(*) FROM events WHERE id IN (2, 3)`); got != 2 {
		t.Fatalf("unselected events must survive, got %d", got)
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "e2.json")); err != nil {
		t.Fatal("unselected raw file removed")
	}
	if report.Generation != 1 {
		t.Fatalf("generation=%d, want 1", report.Generation)
	}
}

func TestPGLeftoverIntentRefuses(t *testing.T) {
	fx := newPGFixture(t)
	if err := os.WriteFile(fx.opts.IntentPath, []byte(`{"phase":"pre-commit"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.opts.Selector = Selector{Project: "purgeme"}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	if _, err := Run(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "intent journal") {
		t.Fatalf("leftover intent must refuse, got: %v", err)
	}
	if got := pgCount(t, fx.pg, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("refused run mutated events: %d", got)
	}
}

// Rebinding through the resume path: a pre-commit intent whose probe rows
// (ids and the project row count) survive proves rollback — nothing is
// deleted, the intent is cleared.
func TestPGPreCommitResumeProvesRollback(t *testing.T) {
	fx := newPGFixture(t)
	intent := intentJournal{
		Phase:               phasePreCommit,
		Selector:            "project",
		Files:               FileMatches{Raw: []string{"e1.json"}},
		ProbeEventIDs:       []int64{1, 2},
		ProbeObservationIDs: []int64{10, 11},
		ProbeProject:        "purgeme",
		ProbeProjectRows:    6,
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	fx.opts.Apply = true
	_, err := ResumeIntent(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("want rollback refusal, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "e1.json")); err != nil {
		t.Fatal("rollback resume deleted a file")
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("rollback resume must clear the intent")
	}

	// Project-row probe alone (ids absent): still provable on postgres.
	intent.ProbeEventIDs, intent.ProbeObservationIDs = nil, nil
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeIntent(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("project-row probe must prove rollback, got: %v", err)
	}
}

func TestPGObservationIDSelectorIsDBOnly(t *testing.T) {
	fx := newPGFixture(t)
	fx.opts.Selector = Selector{ObservationIDs: []int64{11}}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservationsDeleted != 1 || report.EventsDeleted != 0 || report.RawFilesMatched != 0 {
		t.Fatalf("observation selector: obs=%d events=%d raw=%d", report.ObservationsDeleted, report.EventsDeleted, report.RawFilesMatched)
	}
	if report.SupersedesNulled != 1 || report.UsageRowsDeleted != 1 || report.TriageSuggestionsDeleted != 1 {
		t.Fatalf("children: supersedes=%d usage=%d triage=%d, want 1/1/1", report.SupersedesNulled, report.UsageRowsDeleted, report.TriageSuggestionsDeleted)
	}
	if got := pgCount(t, fx.pg, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("observation selector deleted events: %d left", got)
	}
}
