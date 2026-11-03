// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package purge

// Hermetic purge contract tests (memoire:D147 slice 1). Fixture goes
// through the real migration chain so the FTS delete-triggers and 0007
// governance triggers are live — purge must coexist with both.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	memdb "memoire/services/api/internal/db"
	"memoire/services/api/internal/drainer"
)

type fixture struct {
	home string
	opts Options
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	dbPath := filepath.Join(home, "memoire.db")
	store, err := memdb.Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	stmts := []string{
		// Project to purge: session, two events, raw-projection + derived
		// observations, review log, a derivation run.
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
		// Non-empty derive_intents rows (memoire:D165 round-2 LOW-NEW-1): a
		// purged project's intent must be deleted; the survivor's must stay.
		`INSERT INTO derive_intents(project, session_id, first_notified_epoch, last_notified_epoch, notify_count)
		 VALUES ('purgeme', 's1', 100, 100, 1)`,
		`INSERT INTO derive_intents(project, session_id, first_notified_epoch, last_notified_epoch, notify_count)
		 VALUES ('keep', 's2', 100, 100, 1)`,
		// Non-empty run manifest (0015): the purged run's manifest rows die
		// with it.
		`INSERT INTO run_input_events(run_id, event_id) VALUES ('run1', 1)`,
		// Survivor project, one observation superseding a purged one.
		`INSERT INTO sessions(session_id, project, client, started_at_epoch) VALUES ('s2', 'keep', 'cc', 100)`,
		`INSERT INTO events(id, idempotency_key, source_event_id, session_id, project, source_adapter, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch)
		 VALUES (3, 'k3', 'k3', 's2', 'keep', 'claude-code', 'note', '/x', '{}', 'keep gamma text', 100, 100)`,
		`INSERT INTO observations(id, project, session_id, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance, supersedes_id)
		 VALUES (12, 'keep', 's2', 'fact', 'keep obs gamma', 100, 100, 'accepted', 'inferred', 11)`,
		// Derived children (#616 slice E, decision 4): the same rows as the
		// Postgres fixture so both engines must cascade identically — obs 11's
		// triage reasoning, usage rows quoting it, and its embedding; obs 12's
		// own children survive, except the usage row whose usage EVENT (2) is
		// purged.
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
		// No embeddings rows: schema/0002 observation_embeddings is an
		// unapplied sketch on SQLite (the vector lane is Postgres-only); the
		// engine probes for the table and reports 0 when it is absent.
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("fixture stmt failed: %v\n%s", err, stmt)
		}
	}

	rawDir := filepath.Join(home, "raw")
	pendingDir := filepath.Join(home, "queue", "pending")
	inflightDir := filepath.Join(home, "queue", "inflight")
	failedDir := filepath.Join(home, "queue", "failed")
	for _, dir := range []string{rawDir, pendingDir, inflightDir, failedDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	envelope := func(project, adapter, sourceID string) string {
		return fmt.Sprintf(`{"project":%q,"source_adapter":%q,"source_event_id":%q,"text":"x"}`, project, adapter, sourceID)
	}
	files := map[string]string{
		filepath.Join(rawDir, "e1.json"):               envelope("purgeme", "claude-code", "k1"),
		filepath.Join(rawDir, "e2.json"):               envelope("purgeme", "claude-code", "k2"),
		filepath.Join(rawDir, "keep.json"):             envelope("keep", "claude-code", "k3"),
		filepath.Join(rawDir, "garbled.json"):          `{not json`,
		filepath.Join(pendingDir, "p1.json"):           envelope("purgeme", "claude-code", "kp"),
		filepath.Join(inflightDir, "i1.json"):          envelope("purgeme", "claude-code", "ki"),
		filepath.Join(failedDir, "f1.json"):            envelope("purgeme", "claude-code", "kf"),
		filepath.Join(failedDir, "f1.json.error.json"): `{"reason":"ingest: boom"}`,
		filepath.Join(failedDir, "fkeep.json"):         envelope("keep", "claude-code", "kk"),
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return fixture{home: home, opts: Options{
		DBPath:       dbPath,
		RawDir:       rawDir,
		PendingDir:   pendingDir,
		InflightDir:  inflightDir,
		FailedDir:    failedDir,
		IntentPath:   filepath.Join(home, "purge-intent.json"),
		ManifestPath: filepath.Join(home, "custody", "MANIFEST.json"),
	}}
}

func countRows(t *testing.T, dbPath, query string) int {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func requireAge(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("age"); err != nil {
		t.Skip("age binary not installed")
	}
}

func TestDryRunPurity(t *testing.T) {
	fx := newFixture(t)
	fx.opts.Selector = Selector{Project: "purgeme"}

	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun {
		t.Fatal("expected dry-run report")
	}
	if report.EventsMatched != 2 || report.ObservationsMatched != 2 {
		t.Fatalf("matched events=%d obs=%d, want 2/2", report.EventsMatched, report.ObservationsMatched)
	}
	if report.RawFilesMatched != 2 || report.PendingFilesMatched != 1 || report.InflightFilesMatched != 1 || report.FailedFilesMatched != 2 {
		t.Fatalf("file matches raw=%d pending=%d inflight=%d failed=%d, want 2/1/1/2 (sidecar rides its item)",
			report.RawFilesMatched, report.PendingFilesMatched, report.InflightFilesMatched, report.FailedFilesMatched)
	}
	if report.EventsDeleted != 0 || report.RawFilesDeleted != 0 {
		t.Fatal("dry-run must delete nothing")
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM events`); got != 3 {
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

func TestApplyProjectPurge(t *testing.T) {
	requireAge(t)
	fx := newFixture(t)
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
	if report.EventsDeleted != 2 || report.ObservationsDeleted != 2 {
		t.Fatalf("deleted events=%d obs=%d, want 2/2", report.EventsDeleted, report.ObservationsDeleted)
	}
	if report.SourceLinksDeleted != 1 || report.ReviewLogDeleted != 1 {
		t.Fatalf("links=%d reviewlog=%d, want 1/1", report.SourceLinksDeleted, report.ReviewLogDeleted)
	}
	if report.SupersedesNulled != 1 {
		t.Fatalf("supersedes_nulled=%d, want 1", report.SupersedesNulled)
	}
	if report.SessionsDeleted != 1 || report.DerivationRunsDeleted != 1 {
		t.Fatalf("sessions=%d runs=%d, want 1/1", report.SessionsDeleted, report.DerivationRunsDeleted)
	}
	if report.IdentityRootsDeleted != 1 || report.IdentityAlertsDeleted != 1 {
		t.Fatalf("identity roots=%d alerts=%d, want 1/1", report.IdentityRootsDeleted, report.IdentityAlertsDeleted)
	}
	// Cascade parity with Postgres (#616 slice E): obs 11's usage row (by
	// observation) + obs 12's row whose usage event 2 is purged (by event) =
	// 2; obs 11's triage reasoning goes; obs 12's survive. Embeddings: the
	// SQLite store has no embeddings table (unapplied 0002 sketch), so the
	// probe reports 0 instead of failing.
	if report.UsageRowsDeleted != 2 || report.TriageSuggestionsDeleted != 1 || report.EmbeddingsDeleted != 0 {
		t.Fatalf("usage=%d triage=%d embeddings=%d, want 2/1/0", report.UsageRowsDeleted, report.TriageSuggestionsDeleted, report.EmbeddingsDeleted)
	}
	if n := countRows(t, fx.opts.DBPath, `SELECT COUNT(*) FROM observation_usage`); n != 1 {
		t.Fatalf("surviving usage rows=%d, want 1 (obs 12 × event 3)", n)
	}
	if n := countRows(t, fx.opts.DBPath, `SELECT COUNT(*) FROM observation_triage_suggestions WHERE observation_id = 12`); n != 1 {
		t.Fatalf("obs 12 triage suggestion must survive, got %d", n)
	}
	if n := countRows(t, fx.opts.DBPath, `SELECT COUNT(*) FROM observation_usage u JOIN observations o ON o.id = u.observation_id WHERE o.text LIKE '%purgeme%'`); n != 0 {
		t.Fatalf("no usage row may still point at purged text, got %d", n)
	}
	if report.DeriveIntentsDeleted != 1 {
		t.Fatalf("derive_intents deleted=%d, want 1 (purged project only)", report.DeriveIntentsDeleted)
	}
	if report.RunManifestRowsDeleted != 1 {
		t.Fatalf("run manifest rows deleted=%d, want 1", report.RunManifestRowsDeleted)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM run_input_events`); got != 0 {
		t.Fatalf("purged run's manifest rows must be gone, got %d", got)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM derive_intents`); got != 1 {
		t.Fatalf("survivor project's intent must remain, got %d rows", got)
	}
	if report.QuarantineMembers != 6 {
		t.Fatalf("quarantine members=%d, want 6", report.QuarantineMembers)
	}
	if !report.Vacuumed {
		t.Fatal("expected vacuum")
	}
	if report.Generation != 1 {
		t.Fatalf("generation=%d, want 1", report.Generation)
	}

	// Survivors intact, purged gone, FTS blind to purged text.
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("surviving events=%d, want 1", got)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM observations`); got != 1 {
		t.Fatalf("surviving observations=%d, want 1", got)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM observations WHERE supersedes_id IS NOT NULL`); got != 0 {
		t.Fatal("supersedes_id not nulled")
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM observations_fts WHERE observations_fts MATCH 'purgeme'`); got != 0 {
		t.Fatalf("FTS still finds purged text: %d hits", got)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM observations_fts WHERE observations_fts MATCH 'gamma'`); got != 1 {
		t.Fatalf("FTS lost survivor text: %d hits", got)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM audit_log WHERE action = 'purge.apply'`); got != 1 {
		t.Fatal("missing audit tombstone")
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

	// Convergence: a second apply is zero-work and does not bump the
	// generation.
	fx.opts.NoQuarantine = true
	fx.opts.QuarantineOut = ""
	second, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.EventsDeleted != 0 || second.ObservationsDeleted != 0 || second.RawFilesDeleted != 0 {
		t.Fatalf("second run not convergent: %+v", second)
	}
	if second.Generation != 1 {
		t.Fatalf("zero-work apply bumped generation to %d", second.Generation)
	}
}

func TestEventIDSelector(t *testing.T) {
	fx := newFixture(t)
	fx.opts.Selector = Selector{EventIDs: []int64{1}}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true

	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	// Event 1 + its raw projection (10) + linked derived (11).
	if report.EventsDeleted != 1 || report.ObservationsDeleted != 2 {
		t.Fatalf("deleted events=%d obs=%d, want 1/2", report.EventsDeleted, report.ObservationsDeleted)
	}
	// Children cascade on the event selector too (slice E, PG parity with
	// TestPGEventIDSelector): obs 11's usage row and triage suggestion go;
	// obs 12's children and the usage row keyed on surviving event 2 stay.
	if report.UsageRowsDeleted != 1 || report.TriageSuggestionsDeleted != 1 {
		t.Fatalf("usage=%d triage=%d, want 1/1", report.UsageRowsDeleted, report.TriageSuggestionsDeleted)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM observation_usage`); got != 2 {
		t.Fatalf("surviving usage rows=%d, want 2 (obs 12 × events 2 and 3)", got)
	}
	if report.SessionsDeleted != 0 || report.DerivationRunsDeleted != 0 {
		t.Fatal("id selector must not touch sessions or derivation runs")
	}
	if report.RawFilesDeleted != 1 {
		t.Fatalf("raw deleted=%d, want 1 (e1 only)", report.RawFilesDeleted)
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "e2.json")); err != nil {
		t.Fatal("e2.json should survive an event-1 purge")
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM events WHERE id = 2`); got != 1 {
		t.Fatal("event 2 should survive")
	}
	// Round-5 falsifier: the EVENT-side manifest-link deletion (the run
	// stays — only the purged event's link goes, so a fresh PG copy can
	// never re-mint id 1 under this manifest).
	if report.RunManifestRowsDeleted != 1 {
		t.Fatalf("manifest links deleted=%d, want 1 (event-side deletion)", report.RunManifestRowsDeleted)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM run_input_events WHERE event_id = 1`); got != 0 {
		t.Fatal("purged event's manifest link must be gone")
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM derivation_runs WHERE id = 'run1'`); got != 1 {
		t.Fatal("the run itself must survive an event-id purge")
	}
}

func TestObservationIDSelectorIsDBOnly(t *testing.T) {
	fx := newFixture(t)
	fx.opts.Selector = Selector{ObservationIDs: []int64{11}}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true

	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservationsDeleted != 1 || report.EventsDeleted != 0 {
		t.Fatalf("deleted obs=%d events=%d, want 1/0", report.ObservationsDeleted, report.EventsDeleted)
	}
	if report.SupersedesNulled != 1 {
		t.Fatalf("supersedes_nulled=%d, want 1", report.SupersedesNulled)
	}
	if report.RawFilesMatched != 0 || report.RawFilesDeleted != 0 {
		t.Fatal("observation purge must not touch files")
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "e1.json")); err != nil {
		t.Fatal("raw files must survive an observation purge")
	}
}

func TestLeftoverIntentRefusesAndCommittedResumeDeletes(t *testing.T) {
	fx := newFixture(t)
	// Committed phase, probe rows absent (id 999 never existed): the DB
	// half landed, resume finishes the file half.
	intent := intentJournal{
		CreatedAtEpoch: 1,
		Phase:          phaseCommitted,
		Selector:       "project",
		Files:          FileMatches{Raw: []string{"e1.json"}},
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}

	fx.opts.Selector = Selector{Project: "purgeme"}
	if _, err := Run(context.Background(), fx.opts); err == nil {
		t.Fatal("expected refusal on leftover intent journal")
	}

	report, err := ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.RawFilesDeleted != 1 {
		t.Fatalf("resume deleted %d raw files, want 1", report.RawFilesDeleted)
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("resume must remove the intent journal")
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "e1.json")); err == nil {
		t.Fatal("resume must delete the listed file")
	}
	// After resume, a normal run works again.
	if _, err := Run(context.Background(), fx.opts); err != nil {
		t.Fatalf("run after resume: %v", err)
	}
}

// Review round-1 finding 1: a pre-commit intent whose probe rows survived
// means the purge transaction rolled back — resume must NOT delete files.
func TestPreCommitIntentWithSurvivingRowsRefusesDeletion(t *testing.T) {
	fx := newFixture(t)
	intent := intentJournal{
		CreatedAtEpoch: 1,
		Phase:          phasePreCommit,
		Selector:       "project",
		Files:          FileMatches{Raw: []string{"e1.json", "e2.json"}},
		ProbeEventIDs:  []int64{1, 2}, // fixture rows, still present
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}

	_, err := ResumeIntent(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("want rolled-back refusal, got: %v", err)
	}
	for _, kept := range []string{"e1.json", "e2.json"} {
		if _, statErr := os.Stat(filepath.Join(fx.opts.RawDir, kept)); statErr != nil {
			t.Fatalf("pre-commit resume must not delete %s", kept)
		}
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("rolled-back intent must be cleared so a fresh apply can run")
	}
	if _, err := os.Stat(fx.opts.ManifestPath); err == nil {
		t.Fatal("rolled-back resume must not bump the generation")
	}
	// Pre-commit with probes GONE is treated as committed: files deleted.
	intent.ProbeEventIDs = []int64{999999}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	report, err := ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.RawFilesDeleted != 2 {
		t.Fatalf("lost-flip resume deleted %d files, want 2", report.RawFilesDeleted)
	}
	// Unknown phase refuses outright.
	if err := writeIntent(fx.opts.IntentPath, intentJournal{Phase: "weird"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeIntent(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "unknown phase") {
		t.Fatalf("want unknown-phase refusal, got: %v", err)
	}
	if err := os.Remove(fx.opts.IntentPath); err != nil {
		t.Fatal(err)
	}
}

// Review round-1 finding 4: a raw envelope that omitted source_event_id
// (direct HTTP producer shape) must still match an event-id purge via the
// drainer's derivation.
func TestEventIDPurgeMatchesDerivedSourceEventID(t *testing.T) {
	fx := newFixture(t)
	envelope := []byte(`{"schema_version":1,"project":"purgeme","session_id":"s9","source_adapter":"http","event_type":"session_end","payload":{},"text":"http-produced"}`)
	normalized, err := drainer.NormalizeEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.SourceEventID == "" || strings.Contains(string(envelope), normalized.SourceEventID) {
		t.Fatalf("precondition: source id %q must be derived, not present on disk", normalized.SourceEventID)
	}
	if err := os.WriteFile(filepath.Join(fx.opts.RawDir, "http.json"), envelope, 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite3", fx.opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO events(id, idempotency_key, source_event_id, session_id, project, source_adapter, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch)
		VALUES (9, 'k9', ?, 's9', 'purgeme', 'http', 'session_end', '/x', '{}', 'http-produced', 100, 100)`, normalized.SourceEventID); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	fx.opts.Selector = Selector{EventIDs: []int64{9}}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.RawFilesDeleted != 1 {
		t.Fatalf("raw deleted=%d, want 1 (derived-id match)", report.RawFilesDeleted)
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "http.json")); err == nil {
		t.Fatal("http.json must be purged via derived source_event_id")
	}
}

// Review round-1 finding 8: a corrupt custody manifest must stop
// destructive runs instead of silently resetting the generation.
func TestCorruptManifestStopsApply(t *testing.T) {
	fx := newFixture(t)
	if err := os.MkdirAll(filepath.Dir(fx.opts.ManifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.opts.ManifestPath, []byte(`{corrupt`), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.opts.Selector = Selector{Project: "purgeme"}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	if _, err := Run(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corrupt-manifest refusal, got: %v", err)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("corrupt-manifest apply must not delete rows, have %d events", got)
	}
}

func TestValidateOptions(t *testing.T) {
	base := Options{}
	if err := ValidateOptions(base); err == nil {
		t.Fatal("expected selector-required error")
	}
	two := Options{Selector: Selector{Project: "x", EventIDs: []int64{1}}}
	if err := ValidateOptions(two); err == nil {
		t.Fatal("expected one-selector error")
	}
	apply := Options{Selector: Selector{Project: "x"}, Apply: true}
	if err := ValidateOptions(apply); err == nil {
		t.Fatal("expected missing age-recipient error")
	}
}

// ageKeypair generates a throwaway age identity, returning the public
// recipient (the identity itself is discarded — the tests never decrypt).
func ageKeypair(t *testing.T) (recipient, identityFile string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "key.txt")
	cmd := exec.Command("age-keygen", "-o", out)
	stderr, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("age-keygen unavailable: %v %s", err, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if pub, ok := strings.CutPrefix(line, "# public key: "); ok {
			return strings.TrimSpace(pub), out
		}
	}
	t.Fatal("no public key line in age-keygen output")
	return "", ""
}

// Round-2 finding N1: a pre-commit intent with NO probe rows cannot prove
// commit — resume must refuse deletion (file-only project matches).
func TestFileOnlyPreCommitIntentRefusesDeletion(t *testing.T) {
	fx := newFixture(t)
	intent := intentJournal{
		CreatedAtEpoch: 1,
		Phase:          phasePreCommit,
		Selector:       "project",
		Files:          FileMatches{Raw: []string{"e1.json"}, Failed: []string{"f1.json"}},
		// No probes: the selector matched only files.
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	_, err := ResumeIntent(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "cannot be proven") {
		t.Fatalf("want unprovable-commit refusal, got: %v", err)
	}
	for _, kept := range []string{
		filepath.Join(fx.opts.RawDir, "e1.json"),
		filepath.Join(fx.opts.FailedDir, "f1.json"),
	} {
		if _, statErr := os.Stat(kept); statErr != nil {
			t.Fatalf("unprovable resume must not delete %s", kept)
		}
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("unprovable intent must be cleared so a fresh apply can run")
	}
	if _, err := os.Stat(fx.opts.ManifestPath); err == nil {
		t.Fatal("unprovable resume must not bump the generation")
	}
}

// Round-2 finding N2: a resumed committed DB-only purge must still bump
// the custody generation before clearing the intent.
func TestCommittedDBOnlyResumeBumpsGeneration(t *testing.T) {
	fx := newFixture(t)
	intent := intentJournal{
		CreatedAtEpoch: 1,
		Phase:          phaseCommitted,
		Selector:       "observation_ids",
		DBDeleted:      true,
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	report, err := ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Generation != 1 {
		t.Fatalf("DB-only resume must bump generation, got %d", report.Generation)
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("resume must remove the intent")
	}
	// Lost-flip variant: pre-commit, probes recorded and gone -> DB work
	// is inferred and the generation still advances.
	if err := writeIntent(fx.opts.IntentPath, intentJournal{
		Phase:         phasePreCommit,
		Selector:      "event_ids",
		ProbeEventIDs: []int64{999999},
	}); err != nil {
		t.Fatal(err)
	}
	report, err = ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Generation != 2 {
		t.Fatalf("lost-flip DB resume must bump generation, got %d", report.Generation)
	}
}

// Round-2 finding N3: a project apply must clean project-scoped rows even
// when no events/observations/files match.
func TestProjectPurgeCleansResidueRows(t *testing.T) {
	fx := newFixture(t)
	conn, err := sql.Open("sqlite3", fx.opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`INSERT INTO sessions(session_id, project, client, started_at_epoch) VALUES ('sr', 'resid', 'cc', 100)`,
		`INSERT INTO derivation_runs(id, project, session_id, provider, data_boundary, derivation_version, input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch)
		 VALUES ('runr', 'resid', 'sr', 'manual', 'local', 'v1', 'hr', 90, 110, 'completed', 100)`,
		`INSERT INTO project_roots(project, root, source, first_seen_epoch, last_seen_epoch)
		 VALUES ('resid', '/home/x/resid', 'event', 100, 100)`,
		// A usage row captured under the project (slice E): project purge
		// deletes it on both engines, and the probe must count it.
		`INSERT INTO observation_usage(observation_id, event_id, session_id, project, verdict, reason, occurred_at_epoch)
		 VALUES (10, 3, 'sr', 'resid', 'used', 'resid reason', 100)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	fx.opts.Selector = Selector{Project: "resid"}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.EventsMatched != 0 || report.ObservationsMatched != 0 {
		t.Fatalf("precondition: resid must have no events/observations, got %d/%d", report.EventsMatched, report.ObservationsMatched)
	}
	if report.SessionsDeleted != 1 || report.DerivationRunsDeleted != 1 || report.IdentityRootsDeleted != 1 || report.UsageRowsDeleted != 1 {
		t.Fatalf("residue cleanup sessions=%d runs=%d roots=%d usage=%d, want 1/1/1/1",
			report.SessionsDeleted, report.DerivationRunsDeleted, report.IdentityRootsDeleted, report.UsageRowsDeleted)
	}
	if report.Generation != 1 {
		t.Fatalf("residue-only purge must bump generation, got %d", report.Generation)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM sessions WHERE project = 'resid'`); got != 0 {
		t.Fatal("resid session must be gone")
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM audit_log WHERE action = 'purge.apply'`); got != 1 {
		t.Fatalf("want exactly 1 purge.apply tombstone, got %d", got)
	}
	// Convergence: the re-apply does zero work, mints no tombstone, keeps
	// the generation.
	second, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionsDeleted != 0 || second.Generation != 1 {
		t.Fatalf("residue re-apply not convergent: sessions=%d gen=%d", second.SessionsDeleted, second.Generation)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM audit_log WHERE action = 'purge.apply'`); got != 1 {
		t.Fatalf("no-op re-apply minted a tombstone: %d", got)
	}
}

// Round-3 finding R3-1: a residue-only project purge has no event/
// observation probes — the project-scoped row count must prove commit vs
// rollback so a lost flip neither loses the generation bump nor deletes on
// a rolled-back transaction.
func TestProjectRowProbeProvesCommitAndRollback(t *testing.T) {
	fx := newFixture(t)
	// Rolled-back shape: probe recorded 4 project rows, rows still there.
	intent := intentJournal{
		Phase:            phasePreCommit,
		Selector:         "project",
		ProbeProject:     "purgeme",
		ProbeProjectRows: 4,
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeIntent(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("surviving project rows must read as rollback, got: %v", err)
	}
	if _, err := os.Stat(fx.opts.ManifestPath); err == nil {
		t.Fatal("rolled-back project resume must not bump the generation")
	}

	// Committed shape: the project rows are gone (residue-only purge whose
	// flip was lost) — resume must infer DB work and bump the generation.
	conn, err := sql.Open("sqlite3", fx.opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DELETE FROM sessions WHERE project = 'purgeme'`,
		`DELETE FROM derivation_runs WHERE project = 'purgeme'`,
		`DELETE FROM project_roots WHERE project = 'purgeme'`,
		`DELETE FROM project_identity_alerts WHERE project = 'purgeme'`,
		`DELETE FROM derive_intents WHERE project = 'purgeme'`,
		// Slice E: the project's usage rows are part of the probed set on
		// both engines, so a committed purge has removed them too.
		`DELETE FROM observation_usage WHERE project = 'purgeme'`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	report, err := ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Generation != 1 {
		t.Fatalf("lost-flip residue purge must bump generation, got %d", report.Generation)
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("resume must remove the intent")
	}
}

// TestUsageOnlyResiduePurgeAdvancesGeneration pins slice E review round-1
// F1: a project whose only remaining rows are usage rows (the orphan
// population the old SQLite purge left behind) is probed, purged, tombstoned
// and bumps the custody generation; the re-apply is convergent. Before the
// fix the SQLite probe ignored usage rows, so the intent recorded no probes
// and a lost committed-intent flip became unprovable.
func TestUsageOnlyResiduePurgeAdvancesGeneration(t *testing.T) {
	fx := newFixture(t)
	conn, err := sql.Open("sqlite3", fx.opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO observation_usage(observation_id, event_id, session_id, project, verdict, reason, occurred_at_epoch)
		 VALUES (10, 2, 'so', 'orphaned', 'used', 'orphaned reason quoting a purged secret', 100)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	fx.opts.Selector = Selector{Project: "orphaned"}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.UsageRowsDeleted != 1 || report.Generation != 1 {
		t.Fatalf("usage-only residue: usage=%d gen=%d, want 1/1", report.UsageRowsDeleted, report.Generation)
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("apply must remove the intent")
	}
	// Lost-flip variant: a pre-commit intent whose only probe is the project
	// row count, rows gone -> DB work is inferred and the generation advances.
	if err := writeIntent(fx.opts.IntentPath, intentJournal{
		Phase:            phasePreCommit,
		Selector:         "project",
		ProbeProject:     "orphaned",
		ProbeProjectRows: 1,
	}); err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Generation != 2 {
		t.Fatalf("lost-flip usage-only resume must bump generation, got %d", resumed.Generation)
	}
	second, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.UsageRowsDeleted != 0 || second.Generation != 2 {
		t.Fatalf("re-apply not convergent: usage=%d gen=%d", second.UsageRowsDeleted, second.Generation)
	}
}

// TestSQLiteEmbeddingsTableCascadesWhenPresent exercises the present-table
// branch of the embeddings probe on SQLite: with the 0002 table created, a
// purged observation's row goes and the survivor's stays.
func TestSQLiteEmbeddingsTableCascadesWhenPresent(t *testing.T) {
	requireAge(t)
	fx := newFixture(t)
	conn, err := sql.Open("sqlite3", fx.opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS observation_embeddings (observation_id INTEGER PRIMARY KEY, model TEXT NOT NULL, dim INTEGER NOT NULL, vector BLOB NOT NULL, created_at_epoch INTEGER NOT NULL)`,
		`INSERT INTO observation_embeddings(observation_id, model, dim, vector, created_at_epoch) VALUES (11, 'm', 2, X'0000', 100)`,
		`INSERT INTO observation_embeddings(observation_id, model, dim, vector, created_at_epoch) VALUES (12, 'm', 2, X'0000', 100)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	pub, _ := ageKeypair(t)
	fx.opts.Selector = Selector{Project: "purgeme"}
	fx.opts.Apply = true
	fx.opts.AgeRecipient = pub
	fx.opts.QuarantineOut = filepath.Join(fx.home, "q.tar.age")
	report, err := Run(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.EmbeddingsDeleted != 1 {
		t.Fatalf("embeddings deleted=%d, want 1 (obs 11)", report.EmbeddingsDeleted)
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM observation_embeddings`); got != 1 {
		t.Fatalf("surviving embeddings=%d, want 1 (obs 12)", got)
	}
}
