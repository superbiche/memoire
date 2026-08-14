// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package derive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"memoire/services/api/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

func TestRuleR001TypedMemoireMarkers(t *testing.T) {
	dbPath, store := openTestStore(t)
	defer store.Close()

	positiveID := ingestPromptEvent(t, store, "session-r001", "prompt-r001-positive", 100, ""+
		"Memoire decision: Keep additive migrations only\n"+
		"Memoire open_question: Should review logs stay operator-only?\n")
	ingestPromptEvent(t, store, "session-r001", "prompt-r001-negative", 101, ""+
		"Remember: this should not persist\n"+
		"Question: this should not persist either\n"+
		"```go\n"+
		"Memoire decision: fenced false positive\n"+
		"```\n"+
		"// Memoire fact: pasted code comment\n")

	first := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r001"})
	if first.Run.Duplicate {
		t.Fatal("first rules run should not be duplicate")
	}
	if first.SourceEventCount != 2 {
		t.Fatalf("expected 2 source events, got %d", first.SourceEventCount)
	}
	if first.CandidateCount != 2 || len(first.Applied) != 2 {
		t.Fatalf("expected 2 applied candidates, got candidates=%d applied=%d", first.CandidateCount, len(first.Applied))
	}

	decision := queryDerivedObservation(t, dbPath, "session-r001", "decision", "Keep additive migrations only")
	if decision.ReviewState != "accepted" || decision.Confidence != 1.0 {
		t.Fatalf("unexpected decision row: %#v", decision)
	}
	if decision.DerivationRunID != first.Run.ID {
		t.Fatalf("expected decision derivation run %q, got %q", first.Run.ID, decision.DerivationRunID)
	}
	assertProvenance(t, decision.MetaJSON, "R001", "1", []int64{positiveID})
	if decision.ContentHash != mustContentHash(t, "memoire-test", "decision", "Keep additive migrations only") {
		t.Fatalf("unexpected decision content hash: %s", decision.ContentHash)
	}

	openQuestion := queryDerivedObservation(t, dbPath, "session-r001", "open_question", "Should review logs stay operator-only?")
	if openQuestion.ReviewState != "proposed" || openQuestion.Confidence != 1.0 {
		t.Fatalf("unexpected open question row: %#v", openQuestion)
	}

	second := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r001"})
	if !second.FullyCovered {
		t.Fatalf("second rules run must skip fully_covered (manifest coverage), got %+v", second.Run)
	}
	if count := countDerivedObservations(t, dbPath, "session-r001"); count != 2 {
		t.Fatalf("expected replay to keep 2 derived observations, got %d", count)
	}

	// Manifest-less LEGACY run (pre-0015 rows): with the manifest gone the
	// coverage exclusion cannot fire, and the identical-input_hash dedupe
	// must still short-circuit — that reuse is also the self-heal that
	// re-attaches the missing manifest (OutstandingDeriveWork round 4).
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("DELETE FROM run_input_events WHERE run_id = ?", first.Run.ID); err != nil {
		t.Fatalf("strip manifest: %v", err)
	}
	conn.Close()
	legacy := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r001"})
	if legacy.FullyCovered || !legacy.Run.Duplicate || legacy.Run.ID != first.Run.ID {
		t.Fatalf("manifest-less rerun must dedup via input hash (self-heal path), got %+v", legacy)
	}
	if count := countDerivedObservations(t, dbPath, "session-r001"); count != 2 {
		t.Fatalf("legacy dedup must not re-mint observations, got %d", count)
	}
	third := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r001"})
	if !third.FullyCovered {
		t.Fatalf("self-heal must re-attach the manifest (fully_covered again), got %+v", third.Run)
	}
}

func TestRuleR001GotchaMarker(t *testing.T) {
	// gotcha is the seventh sealed kind (pre-run v1 amendment). The deterministic
	// "Memoire gotcha:" operator marker is an exact typed marker like the others
	// and defaults to accepted review state.
	dbPath, store := openTestStore(t)
	defer store.Close()

	id := ingestPromptEvent(t, store, "session-gotcha", "prompt-gotcha", 700,
		"Memoire gotcha: DisallowUnknownFields silently failed the whole parse on one stray field\n")

	first := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-gotcha"})
	if first.CandidateCount != 1 || len(first.Applied) != 1 {
		t.Fatalf("expected 1 gotcha candidate, got candidates=%d applied=%d", first.CandidateCount, len(first.Applied))
	}
	row := queryDerivedObservation(t, dbPath, "session-gotcha",
		"gotcha", "DisallowUnknownFields silently failed the whole parse on one stray field")
	if row.ReviewState != "accepted" || row.Confidence != 1.0 {
		t.Fatalf("unexpected gotcha row: %#v", row)
	}
	assertProvenance(t, row.MetaJSON, "R001", "1", []int64{id})
}

func TestRuleR002ExplicitTaskMarkers(t *testing.T) {
	dbPath, store := openTestStore(t)
	defer store.Close()

	ingestPromptEvent(t, store, "session-r002", "prompt-r002-positive", 200, ""+
		"TODO: Ship rules provider\n"+
		"Next: Add operator derive command\n"+
		"Follow up: Document the replay path\n")
	ingestPromptEvent(t, store, "session-r002", "prompt-r002-negative", 201, ""+
		"Implement the rules provider soon.\n"+
		"```txt\n"+
		"TODO: fenced false positive\n"+
		"```\n"+
		"# TODO: pasted shell comment\n")

	first := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r002"})
	if first.CandidateCount != 3 || len(first.Applied) != 3 {
		t.Fatalf("expected 3 applied task candidates, got candidates=%d applied=%d", first.CandidateCount, len(first.Applied))
	}

	for _, text := range []string{
		"Ship rules provider",
		"Add operator derive command",
		"Document the replay path",
	} {
		row := queryDerivedObservation(t, dbPath, "session-r002", "task", text)
		if row.ReviewState != "accepted" || row.Confidence != 1.0 {
			t.Fatalf("unexpected task row for %q: %#v", text, row)
		}
		if row.DerivationRunID != first.Run.ID {
			t.Fatalf("expected task derivation run %q, got %q", first.Run.ID, row.DerivationRunID)
		}
		assertProvenance(t, row.MetaJSON, "R002", "1", nil)
	}

	second := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r002"})
	if !second.FullyCovered {
		t.Fatalf("second rules run must skip fully_covered (manifest coverage), got %+v", second.Run)
	}
	if count := countDerivedObservations(t, dbPath, "session-r002"); count != 3 {
		t.Fatalf("expected replay to keep 3 derived observations, got %d", count)
	}
}

func TestRuleR003OpenQuestionMarkers(t *testing.T) {
	dbPath, store := openTestStore(t)
	defer store.Close()

	ingestPromptEvent(t, store, "session-r003", "prompt-r003-positive", 300, ""+
		"Open question: How should review logs be exposed?\n"+
		"Unresolved: whether cross-session windows need a reason field\n")
	ingestPromptEvent(t, store, "session-r003", "prompt-r003-negative", 301, ""+
		"Question: should stay transient\n"+
		"```md\n"+
		"Open question: fenced false positive\n"+
		"```\n")

	first := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r003"})
	if first.CandidateCount != 2 || len(first.Applied) != 2 {
		t.Fatalf("expected 2 applied open questions, got candidates=%d applied=%d", first.CandidateCount, len(first.Applied))
	}

	for _, text := range []string{
		"How should review logs be exposed?",
		"whether cross-session windows need a reason field",
	} {
		row := queryDerivedObservation(t, dbPath, "session-r003", "open_question", text)
		if row.ReviewState != "proposed" || row.Confidence != 1.0 {
			t.Fatalf("unexpected open question row for %q: %#v", text, row)
		}
		if row.DerivationRunID != first.Run.ID {
			t.Fatalf("expected open question derivation run %q, got %q", first.Run.ID, row.DerivationRunID)
		}
		assertProvenance(t, row.MetaJSON, "R003", "1", nil)
	}

	second := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r003"})
	if !second.FullyCovered {
		t.Fatalf("second rules run must skip fully_covered (manifest coverage), got %+v", second.Run)
	}
	if count := countDerivedObservations(t, dbPath, "session-r003"); count != 2 {
		t.Fatalf("expected replay to keep 2 derived observations, got %d", count)
	}
}

func TestRuleR004ResumePrompt(t *testing.T) {
	dbPath, store := openTestStore(t)
	defer store.Close()

	resumeText := "" +
		"Resume Memoire in `/home/user/projects/memoire`: keep the rules slice narrow.\n" +
		"Then add the operator command.\n"
	ingestPromptEvent(t, store, "session-r004", "prompt-r004-positive", 400, resumeText)
	ingestPromptEvent(t, store, "session-r004", "prompt-r004-negative", 401, ""+
		"resume Memoire in `/home/user/projects/memoire`: lower-case should not match\n")
	ingestPromptEvent(t, store, "session-r004", "prompt-r004-skip", 402, ""+
		"Resume Memoire in `/home/user/projects/memoire`: should skip R004 when explicit task exists.\n"+
		"TODO: Prefer the explicit task marker from this prompt\n")

	first := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r004"})
	if first.CandidateCount != 2 || len(first.Applied) != 2 {
		t.Fatalf("expected 2 applied candidates, got candidates=%d applied=%d", first.CandidateCount, len(first.Applied))
	}

	resumeRow := queryDerivedObservation(t, dbPath, "session-r004", "task", "Resume Memoire in `/home/user/projects/memoire`: keep the rules slice narrow.\nThen add the operator command.")
	if resumeRow.ReviewState != "proposed" || resumeRow.Confidence != 0.75 {
		t.Fatalf("unexpected resume row: %#v", resumeRow)
	}
	if resumeRow.DerivationRunID != first.Run.ID {
		t.Fatalf("expected resume derivation run %q, got %q", first.Run.ID, resumeRow.DerivationRunID)
	}
	assertProvenance(t, resumeRow.MetaJSON, "R004", "1", nil)

	explicitTask := queryDerivedObservation(t, dbPath, "session-r004", "task", "Prefer the explicit task marker from this prompt")
	if explicitTask.ReviewState != "accepted" || explicitTask.Confidence != 1.0 {
		t.Fatalf("unexpected explicit task row: %#v", explicitTask)
	}

	if count := countDerivedObservations(t, dbPath, "session-r004"); count != 2 {
		t.Fatalf("expected only the resume task and explicit task, got %d rows", count)
	}

	second := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r004"})
	if !second.FullyCovered {
		t.Fatalf("second rules run must skip fully_covered (manifest coverage), got %+v", second.Run)
	}
}

func TestCanonicalHashesAreDeterministic(t *testing.T) {
	first, err := CanonicalContentHash("memoire-test", "task", "  Resume\tMemoire\nin  /tmp  ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalContentHash("memoire-test", "task", "Resume Memoire in /tmp")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("expected normalized content hashes to match, got %q and %q", first, second)
	}

	events := []db.EventRecord{
		{
			ID:              2,
			IdempotencyKey:  "b",
			SourceAdapter:   "claude-code",
			SourceEventID:   "evt-b",
			EventType:       "claude_PostToolUse",
			OccurredAtEpoch: 11,
			Payload:         json.RawMessage(`{"b":2,"a":1}`),
		},
		{
			ID:              1,
			IdempotencyKey:  "a",
			SourceAdapter:   "claude-code",
			SourceEventID:   "evt-a",
			EventType:       "claude_UserPromptSubmit",
			OccurredAtEpoch: 10,
			Payload:         json.RawMessage(`{"prompt":"hello","z":2,"a":1}`),
		},
	}
	hashOne, err := CanonicalInputHash(events)
	if err != nil {
		t.Fatal(err)
	}
	hashTwo, err := CanonicalInputHash([]db.EventRecord{events[1], events[0]})
	if err != nil {
		t.Fatal(err)
	}
	if hashOne != hashTwo {
		t.Fatalf("expected input hashes to be order-independent, got %q and %q", hashOne, hashTwo)
	}
}

type storedObservation struct {
	ID              int64
	ReviewState     string
	Confidence      float64
	ContentHash     string
	DerivationRunID string
	MetaJSON        string
}

func openTestStore(t *testing.T) (string, *db.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memoire.db")
	store, err := db.Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	return dbPath, store
}

func ingestPromptEvent(t *testing.T, store *db.Store, sessionID, sourceEventID string, occurredAt int64, prompt string) int64 {
	t.Helper()
	res, err := store.Ingest(context.Background(), db.EventInput{
		Project:         "memoire-test",
		SessionID:       sessionID,
		SourceAdapter:   "claude-code",
		SourceEventID:   sourceEventID,
		EventType:       "claude_UserPromptSubmit",
		OccurredAtEpoch: occurredAt,
		Payload:         json.RawMessage(`{"prompt":` + mustJSONString(t, prompt) + `}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func runRulesForSession(t *testing.T, store *db.Store, window Window) Result {
	t.Helper()
	result, err := RunRules(context.Background(), store, window)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func queryDerivedObservation(t *testing.T, dbPath, sessionID, kind, text string) storedObservation {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var row storedObservation
	if err := conn.QueryRowContext(context.Background(), `SELECT id, review_state, confidence, content_hash, derivation_run_id, meta_json
		FROM observations
		WHERE session_id = ? AND kind = ? AND text = ?`, sessionID, kind, text).Scan(
		&row.ID, &row.ReviewState, &row.Confidence, &row.ContentHash, &row.DerivationRunID, &row.MetaJSON,
	); err != nil {
		t.Fatal(err)
	}
	return row
}

func countDerivedObservations(t *testing.T, dbPath, sessionID string) int {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var count int
	if err := conn.QueryRowContext(context.Background(), `SELECT count(*)
		FROM observations
		WHERE session_id = ? AND kind <> 'raw'`, sessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertProvenance(t *testing.T, metaJSON, wantRuleID, wantRuleVersion string, wantSourceIDs []int64) {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatal(err)
	}

	rule, ok := meta["rule"].(map[string]any)
	if !ok || rule["id"] != wantRuleID || rule["version"] != wantRuleVersion {
		t.Fatalf("unexpected rule provenance: %#v", meta["rule"])
	}
	if meta["confidence_source"] != ConfidenceSourceRuleFixed {
		t.Fatalf("unexpected confidence_source: %#v", meta["confidence_source"])
	}

	window, ok := meta["source_window"].(map[string]any)
	if !ok || window["project"] != "memoire-test" {
		t.Fatalf("unexpected source_window: %#v", meta["source_window"])
	}

	derivation, ok := meta["derivation"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected derivation metadata: %#v", meta["derivation"])
	}
	if derivation["version"] != DerivationVersionV1 ||
		derivation["provider"] != ProviderRules ||
		derivation["data_boundary"] != DataBoundaryLocal ||
		derivation["prompt_version"] != "" ||
		derivation["confidence_source"] != ConfidenceSourceRuleFixed {
		t.Fatalf("unexpected derivation metadata: %#v", derivation)
	}
	if _, ok := derivation["input_hash"].(string); !ok {
		t.Fatalf("expected derivation input_hash in %#v", derivation)
	}

	sourceEventIDs, ok := meta["source_event_ids"].([]any)
	if !ok || len(sourceEventIDs) != 1 {
		t.Fatalf("unexpected source_event_ids: %#v", meta["source_event_ids"])
	}
	if len(wantSourceIDs) == 1 {
		if got := int64(sourceEventIDs[0].(float64)); got != wantSourceIDs[0] {
			t.Fatalf("unexpected source event id %d", got)
		}
	}
}

func mustContentHash(t *testing.T, project, kind, text string) string {
	t.Helper()
	hash, err := CanonicalContentHash(project, kind, text)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRuleR005LiaisonEvidenceIngest(t *testing.T) {
	dbPath, store := openTestStore(t)
	defer store.Close()

	envelope := `{"liaison_evidence_observation":{"artifact_ref":"tmp/evidence/echo.txt",` +
		`"evidence_type":"artifact-content","judge_verdict":"pass","supersedes":[],` +
		`"task_ref":"r005-echo","task_shape":{"operation_kind":"design","oracle":"oracle-none",` +
		`"risk_tier":"reversible-local"},"timestamp":"2026-07-18T00:00:00+00:00","ttl_review":""},` +
		`"schema_version":1}`
	positiveID := ingestLiaisonEvidenceEvent(t, store, "session-r005", "liaison-evidence-echo:pos1", 100, envelope)
	// Malformed envelopes yield NO candidate (skip, not error): the Liaison
	// evidence store is authoritative (liaison:D218).
	ingestLiaisonEvidenceEvent(t, store, "session-r005", "liaison-evidence-echo:bad1", 101, "not json at all")
	ingestLiaisonEvidenceEvent(t, store, "session-r005", "liaison-evidence-echo:bad2", 102, `{"schema_version":1}`)
	// Wrong event_type under the liaison-evidence adapter: gated out by R005,
	// and the claude marker path requires source_adapter=claude-code.
	if _, err := store.Ingest(context.Background(), db.EventInput{
		Project:         "memoire-test",
		SessionID:       "session-r005",
		SourceAdapter:   "liaison-evidence",
		SourceEventID:   "liaison-evidence-echo:bad3",
		EventType:       "claude_UserPromptSubmit",
		OccurredAtEpoch: 103,
		Payload:         json.RawMessage(`{"prompt":"Memoire fact: should not derive"}`),
		Text:            envelope,
	}); err != nil {
		t.Fatal(err)
	}

	result := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r005"})
	if result.SourceEventCount != 4 {
		t.Fatalf("expected 4 source events in window, got %d", result.SourceEventCount)
	}
	if result.CandidateCount != 1 || len(result.Applied) != 1 {
		t.Fatalf("expected exactly 1 applied candidate, got candidates=%d applied=%d", result.CandidateCount, len(result.Applied))
	}

	row := queryDerivedObservation(t, dbPath, "session-r005", "fact", envelope)
	if row.ReviewState != "accepted" || row.Confidence != 1.0 {
		t.Fatalf("unexpected R005 row: %#v", row)
	}
	assertProvenance(t, row.MetaJSON, "R005", "1", []int64{positiveID})
	if row.ContentHash != mustContentHash(t, "memoire-test", "fact", envelope) {
		t.Fatalf("unexpected R005 content hash: %s", row.ContentHash)
	}

	second := runRulesForSession(t, store, Window{Project: "memoire-test", SessionID: "session-r005"})
	if !second.FullyCovered {
		t.Fatalf("second rules run must skip fully_covered (manifest coverage), got %+v", second.Run)
	}
	if count := countDerivedObservations(t, dbPath, "session-r005"); count != 1 {
		t.Fatalf("expected replay to keep 1 derived observation, got %d", count)
	}
}

func ingestLiaisonEvidenceEvent(t *testing.T, store *db.Store, sessionID, sourceEventID string, occurredAt int64, text string) int64 {
	t.Helper()
	res, err := store.Ingest(context.Background(), db.EventInput{
		Project:         "memoire-test",
		SessionID:       sessionID,
		SourceAdapter:   "liaison-evidence",
		SourceEventID:   sourceEventID,
		EventType:       "liaison_evidence",
		OccurredAtEpoch: occurredAt,
		Payload:         json.RawMessage(`{}`),
		Text:            text,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

// faultStore wraps the real store and fails the Nth InsertDerivedObservation
// once — the round-6 MED-NEW-1 falsifier scaffold.
type faultStore struct {
	*db.Store
	failOnInsert int
	inserts      int
}

func (f *faultStore) InsertDerivedObservation(ctx context.Context, in db.DerivedObservationInput) (db.DerivedObservationResult, error) {
	f.inserts++
	if f.inserts == f.failOnInsert {
		return db.DerivedObservationResult{}, errors.New("injected insert fault")
	}
	return f.Store.InsertDerivedObservation(ctx, in)
}

// A failed attempt that committed a prefix of its candidates must NOT
// duplicate them on the retry (memoire:D165 round-6 MED-NEW-1): the retry
// skips already-landed candidates by content hash and the final counts are
// value-exact.
func TestRunRulesFailedRetryDoesNotDuplicateObservations(t *testing.T) {
	dbPath, store := openTestStore(t)
	defer store.Close()

	ingestPromptEvent(t, store, "session-retry", "prompt-retry-1", 100,
		"Memoire decision: first durable decision\n")
	ingestPromptEvent(t, store, "session-retry", "prompt-retry-2", 101,
		"Memoire fact: second durable fact\n")

	window := Window{Project: "memoire-test", SessionID: "session-retry"}
	// Attempt 1: candidate 2's insert fails after candidate 1 committed.
	fs := &faultStore{Store: store, failOnInsert: 2}
	if _, err := RunRules(context.Background(), fs, window); err == nil {
		t.Fatal("attempt 1 must fail on the injected fault")
	}
	if got := countObservations(t, dbPath, "session-retry"); got != 1 {
		t.Fatalf("attempt 1 must have committed exactly the prefix, got %d", got)
	}
	if st := runStatus(t, dbPath, "session-retry"); st != "failed" {
		t.Fatalf("attempt 1 must leave a retryable failed run, got %q", st)
	}

	// Attempt 2: same window, real store — must complete with EXACTLY the
	// two candidates, replaying (skipping) the committed one.
	second, err := RunRules(context.Background(), store, window)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.Run.Duplicate {
		t.Fatalf("retry must claim the failed run in place, got duplicate: %+v", second.Run)
	}
	if second.ReplayedCount != 1 || len(second.Applied) != 1 {
		t.Fatalf("retry must skip the committed prefix (replayed=1 applied=1), got replayed=%d applied=%d",
			second.ReplayedCount, len(second.Applied))
	}
	if got := countObservations(t, dbPath, "session-retry"); got != 2 {
		t.Fatalf("post-retry observation count must be exact, got %d", got)
	}
	if st := runStatus(t, dbPath, "session-retry"); st != "completed" {
		t.Fatalf("retry must terminate the run, got %q", st)
	}
}

// ensureStore wraps the real store but forces EnsureDerivationRun to report
// a RUNNING duplicate — the round-6 LOW-NEW-1 falsifier.
type runningDupStore struct {
	*db.Store
}

func (r *runningDupStore) EnsureDerivationRun(ctx context.Context, in db.DerivationRunInput) (db.DerivationRunResult, error) {
	return db.DerivationRunResult{ID: "busy-run", Duplicate: true, Status: "running"}, nil
}

func (r *runningDupStore) InsertDerivedObservation(ctx context.Context, in db.DerivedObservationInput) (db.DerivedObservationResult, error) {
	panic("a non-terminal duplicate must perform no work")
}

func TestRunRulesRunningDuplicateIsDeferredNotSuccess(t *testing.T) {
	_, store := openTestStore(t)
	defer store.Close()
	ingestPromptEvent(t, store, "session-busy", "prompt-busy-1", 100,
		"Memoire decision: busy fixture\n")
	res, err := RunRules(context.Background(), &runningDupStore{Store: store}, Window{Project: "memoire-test", SessionID: "session-busy"})
	if err != nil {
		t.Fatalf("running duplicate must not error: %v", err)
	}
	if !res.Run.Duplicate || res.Run.Status != "running" {
		t.Fatalf("result must expose the non-terminal duplicate status, got %+v", res.Run)
	}
	if len(res.Applied) != 0 || res.CandidateCount != 0 {
		t.Fatalf("a deferred duplicate must report no work, got %+v", res)
	}
}

func countObservations(t *testing.T, dbPath, sessionID string) int {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM observations WHERE session_id = ? AND derivation_run_id IS NOT NULL AND deleted_at_epoch IS NULL`, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func runStatus(t *testing.T, dbPath, sessionID string) string {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var st string
	if err := conn.QueryRow(`SELECT status FROM derivation_runs WHERE session_id = ? ORDER BY created_at_epoch DESC LIMIT 1`, sessionID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Round-7 falsifier: same-content candidates from DISTINCT source events
// are separate rows by contract; the replay guard must consume the
// committed MULTISET, not collapse it to a boolean set — a retry after
// committing one of two identical-hash candidates must still insert the
// second one's provenance.
func TestRunRulesFailedRetryPreservesSameContentTwin(t *testing.T) {
	dbPath, store := openTestStore(t)
	defer store.Close()

	ingestPromptEvent(t, store, "session-twin", "prompt-twin-1", 100,
		"Memoire fact: identical durable text\n")
	ingestPromptEvent(t, store, "session-twin", "prompt-twin-2", 101,
		"Memoire fact: identical durable text\n")

	window := Window{Project: "memoire-test", SessionID: "session-twin"}
	fs := &faultStore{Store: store, failOnInsert: 2}
	if _, err := RunRules(context.Background(), fs, window); err == nil {
		t.Fatal("attempt 1 must fail on the injected fault")
	}
	if got := countObservations(t, dbPath, "session-twin"); got != 1 {
		t.Fatalf("attempt 1 must have committed exactly one twin, got %d", got)
	}

	second, err := RunRules(context.Background(), store, window)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.ReplayedCount != 1 || len(second.Applied) != 1 {
		t.Fatalf("retry must consume ONE committed occurrence and insert the twin (replayed=1 applied=1), got replayed=%d applied=%d",
			second.ReplayedCount, len(second.Applied))
	}
	if got := countObservations(t, dbPath, "session-twin"); got != 2 {
		t.Fatalf("both same-content twins must exist post-retry, got %d", got)
	}
	if st := runStatus(t, dbPath, "session-twin"); st != "completed" {
		t.Fatalf("retry must terminate the run, got %q", st)
	}
}
